package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/correlation"
)

func TestCorrelationJobsAreScheduledCoalescedAndClaimed(t *testing.T) {
	t.Parallel()

	store := newCorrelationStore(t)
	now := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)
	enqueued, coalesced, err := store.EnqueueCollectionCorrelation(
		t.Context(), "acme", false, now,
	)
	if err != nil || enqueued != 1 || coalesced != 0 {
		t.Fatalf("initial enqueue = %d, coalesced = %d, error = %v", enqueued, coalesced, err)
	}
	enqueued, coalesced, err = store.EnqueueCollectionCorrelation(
		t.Context(), "acme", true, now,
	)
	if err != nil {
		t.Fatalf("EnqueueCollectionCorrelation() error = %v", err)
	}
	if enqueued != 0 || coalesced != 1 {
		t.Errorf("enqueue = %d, coalesced = %d, want 0, 1", enqueued, coalesced)
	}

	job, err := store.ClaimCorrelation(t.Context(), now)
	if err != nil {
		t.Fatalf("ClaimCorrelation() error = %v", err)
	}
	if job == nil || job.CollectionID != "acme" || !job.Forced ||
		job.PrimaryProvider != "local" {
		t.Errorf("ClaimCorrelation() = %+v", job)
	}
}

func TestCorrelationWaitsForActiveRepositoryRefresh(t *testing.T) {
	t.Parallel()

	store := newCorrelationStore(t)
	now := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnqueueRefresh(t.Context(), "acme/widgets", now); err != nil {
		t.Fatal(err)
	}
	refreshJob, err := store.ClaimRefresh(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartRefreshGeneration(t.Context(), refreshJob, RefreshGenerationDiscovery{
		Members: map[int][]string{1: {"acme"}}, DiscoveredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueCollectionCorrelation(t.Context(), "acme", false, now); err != nil {
		t.Fatal(err)
	}
	if job, err := store.ClaimCorrelation(t.Context(), now); err != nil {
		t.Fatal(err)
	} else if job != nil {
		t.Fatalf("correlation claimed during refresh: %+v", job)
	}
	if err := store.CompleteRefreshJob(t.Context(), refreshJob.ID, now); err != nil {
		t.Fatal(err)
	}
	if job, err := store.ClaimCorrelation(t.Context(), now); err != nil {
		t.Fatal(err)
	} else if job == nil {
		t.Fatal("correlation remained blocked after refresh completion")
	}
}

func TestCorrelationSuccessAtomicallyReplacesGroupsAndFeedsLaterAnalysis(t *testing.T) {
	t.Parallel()

	store := newCorrelationStore(t)
	now := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)
	for number, title := range map[int]string{1: "Server support", 2: "Client types"} {
		if err := store.UpsertPullRequest(t.Context(), "acme", PullRequest{
			Repository: "acme/widgets", Number: number, Title: title,
			UpdatedAt: now, URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.EnqueueCollectionCorrelation(t.Context(), "acme", false, now); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimCorrelation(t.Context(), now)
	if err != nil || job == nil {
		t.Fatalf("ClaimCorrelation() = %+v, %v", job, err)
	}
	requests, err := store.BuildCorrelationRequests(t.Context(), *job)
	if err != nil {
		t.Fatalf("BuildCorrelationRequests() error = %v", err)
	}
	if len(requests) != 1 || len(requests[0].PullRequests) != 2 {
		t.Fatalf("requests = %+v", requests)
	}
	members, evidence := correlation.KnownSources(requests[0])
	response := []byte(`{"groups":[{
		"name":"Native histogram support",
		"description":"Coordinates server and client support.",
		"confidence":"high",
		"sourceIDs":["PR_acme_widgets_1","PR_acme_widgets_2"],
		"members":["PR_acme_widgets_1","PR_acme_widgets_2"],
		"edges":[{
			"from":"PR_acme_widgets_1","to":"PR_acme_widgets_2",
			"type":"stacked_on_top_of","confidence":"high",
			"reason":"Server support builds on client types.",
			"sourceIDs":["PR_acme_widgets_1","PR_acme_widgets_2"]
		}]
	}]}`)
	result, err := correlation.ValidateResponse(response, members, evidence)
	if err != nil {
		t.Fatalf("ValidateResponse() error = %v", err)
	}
	if err := store.RecordCorrelationSuccess(
		t.Context(), *job, requests, result.Groups, "local", "", "qwen3",
		[]byte("request"), response, now,
	); err != nil {
		t.Fatalf("RecordCorrelationSuccess() error = %v", err)
	}

	groups, err := store.ListCorrelationGroups(t.Context(), "acme")
	if err != nil {
		t.Fatalf("ListCorrelationGroups() error = %v", err)
	}
	if len(groups) != 1 || groups[0].Name != "Native histogram support" ||
		groups[0].OpenMemberCount != 2 || len(groups[0].Edges) != 1 {
		t.Fatalf("groups = %+v", groups)
	}

	analysisRequest, err := store.BuildAnalysisRequest(t.Context(), AnalysisJob{
		CollectionID: "acme", Repository: "acme/widgets", Number: 1,
	})
	if err != nil {
		t.Fatalf("BuildAnalysisRequest() error = %v", err)
	}
	found := false
	for _, source := range analysisRequest.Sources {
		if source.Kind == "correlation_group" &&
			source.Text == correlation.TextGraph(groups[0].Group) {
			found = true
		}
	}
	if !found {
		t.Errorf("analysis sources = %+v, want correlation_group", analysisRequest.Sources)
	}
}

func TestCorrelationFailurePreservesLastSuccessfulGroups(t *testing.T) {
	t.Parallel()

	store := newCorrelationStore(t)
	now := time.Now().UTC()
	if err := store.ReplaceCorrelationGroups(t.Context(), "acme", []correlation.Group{{
		ID: "group-one", Name: "Existing feature", Description: "Last successful result.",
		Confidence: "high",
		Members: []correlation.Member{
			{SourceID: "PR_acme_widgets_1", Repository: "acme/widgets", Number: 1, State: "open"},
			{SourceID: "PR_acme_widgets_2", Repository: "acme/widgets", Number: 2, State: "open"},
		},
	}}, CorrelationProvenance{
		Provider: "local", Model: "qwen3", SchemaVersion: correlation.SchemaVersion,
		PromptVersion: correlation.PromptVersion, CorrelatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCorrelationFailure(
		t.Context(), CorrelationJob{CollectionID: "acme"}, "local", "qwen3",
		nil, nil,
		now.Add(time.Hour), "Model provider unavailable.",
	); err != nil {
		t.Fatal(err)
	}
	groups, err := store.ListCorrelationGroups(t.Context(), "acme")
	if err != nil || len(groups) != 1 || groups[0].Name != "Existing feature" {
		t.Errorf("groups after failure = %+v, %v", groups, err)
	}
	providers, err := store.ModelProviderStatuses(t.Context())
	if err != nil || len(providers) != 1 || providers[0].Name != "local" ||
		providers[0].State != "degraded" {
		t.Errorf("provider statuses = %+v, %v", providers, err)
	}
	failures, err := store.RecentFailures(t.Context(), 20)
	if err != nil || len(failures) != 1 || failures[0].Component != "correlation" ||
		failures[0].Subject != "acme" {
		t.Errorf("recent failures = %+v, %v", failures, err)
	}
}

func newCorrelationStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "correlation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	interval := config.FeatureCorrelation{Schedule: "0 4 * * *"}
	if err := store.SyncCollectionsAt(context.Background(), []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
		ModelProvider: "local", FeatureCorrelation: &interval,
	}}, time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	return store
}
