package storage

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

func TestRefreshGenerationStagesBatchesDurably(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnqueueRefresh(ctx, "acme/widgets", now); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimRefresh(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	discovery := RefreshGenerationDiscovery{
		Members: map[int][]string{
			1: {"acme"},
			2: {"acme"},
			3: {"acme"},
		},
		Limits:        []ContextLimit{{Scope: "discovery_search", Reason: "provider_limit", Collected: 3, Total: 4}},
		DiscoveredAt:  now,
		ProgressSince: now.Add(-time.Hour),
	}
	if err := store.StartRefreshGeneration(ctx, job, discovery); err != nil {
		t.Fatal(err)
	}
	batch, err := store.LoadRefreshGenerationBatch(ctx, job.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 || batch[0].Number != 1 || batch[1].Number != 2 {
		t.Fatalf("first batch = %+v", batch)
	}
	first := testPullRequest(1, "First")
	second := testPullRequest(2, "Second")
	for _, pullRequest := range []*PullRequest{&first, &second} {
		pullRequest.Repository = "acme/widgets"
		pullRequest.CollectionIDs = []string{"acme"}
		pullRequest.URL = "https://github.com/acme/widgets/pull/" + strconv.Itoa(pullRequest.Number)
	}
	if err := store.PublishRefreshGenerationBatch(
		ctx, job.ID,
		[]StagedRefreshPullRequest{
			{Number: 1, Payload: []byte(`{"number":1}`), FingerprintVersion: "v1", Fingerprint: "one", CacheOutcome: RefreshCacheMiss},
			{Number: 2, Payload: []byte(`{"number":2}`), FingerprintVersion: "v1", Fingerprint: "two", CacheOutcome: RefreshCacheHit},
		},
		[]PullRequest{first, second}, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.RequeueRefreshJob(ctx, job.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	job, err = reopened.ClaimRefresh(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	batch, err = reopened.LoadRefreshGenerationBatch(ctx, job.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 || batch[0].Number != 3 {
		t.Fatalf("resumed batch = %+v, want only PR 3", batch)
	}
	generation, err := reopened.LoadRefreshGeneration(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if generation == nil || generation.Completed != 2 || generation.Pending != 1 ||
		generation.CacheHits != 1 || generation.CacheMisses != 1 ||
		len(generation.Limits) != 1 || !generation.ProgressSince.Equal(discovery.ProgressSince) {
		t.Fatalf("generation = %+v", generation)
	}
}

func TestRefreshGenerationPublishesBatchWithoutRemovingPriorSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const repository = "acme/widgets"
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{repository},
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	published := testPullRequest(1, "Published")
	published.Repository = repository
	published.URL = "https://github.com/acme/widgets/pull/1"
	if err := store.ReplaceRepositorySnapshot(
		ctx, repository, []PullRequest{published}, now.Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueRefresh(ctx, repository, now); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimRefresh(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartRefreshGeneration(ctx, job, RefreshGenerationDiscovery{
		Members: map[int][]string{2: {"acme"}}, DiscoveredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	next := testPullRequest(2, "New batch")
	next.Repository = repository
	next.URL = "https://github.com/acme/widgets/pull/2"
	next.CollectionIDs = []string{"acme"}
	if err := store.PublishRefreshGenerationBatch(
		ctx, job.ID,
		[]StagedRefreshPullRequest{{
			Number: 2, Payload: []byte(`{"number":2}`),
			FingerprintVersion: "v1", Fingerprint: "two", CacheOutcome: RefreshCacheMiss,
		}},
		[]PullRequest{next}, now,
	); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListPullRequests(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 1 || got[1].Number != 2 {
		t.Fatalf("published batch = %+v, want prior and newly hydrated PR", got)
	}
	if diff, err := store.ClaimPullRequestDiff(ctx, now); err != nil {
		t.Fatal(err)
	} else if diff != nil {
		t.Fatalf("batch publication released downstream work: %+v", diff)
	}
}

func TestScheduledRefreshRequestsInPlaceRediscovery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const repository = "acme/widgets"
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{repository},
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnqueueRefresh(ctx, repository, now); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimRefresh(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartRefreshGeneration(ctx, job, RefreshGenerationDiscovery{
		Members: map[int][]string{1: {"acme"}}, DiscoveredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RequeueRefreshJob(ctx, job.ID, now); err != nil {
		t.Fatal(err)
	}
	nextSchedule := now.Add(3 * time.Hour)
	if err := store.EnqueueAllRefreshes(ctx, nextSchedule); err != nil {
		t.Fatal(err)
	}
	generation, err := store.LoadRefreshGeneration(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if generation == nil || !generation.RediscoveryRequested {
		t.Fatalf("generation = %+v, want scheduled rediscovery request", generation)
	}
	if err := store.ReconcileRefreshGenerationDiscovery(ctx, job.ID, RefreshGenerationDiscovery{
		Members: map[int][]string{
			1: {"acme"},
			2: {"acme"},
		},
		DiscoveredAt: nextSchedule,
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := store.LoadRefreshGenerationBatch(ctx, job.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 || batch[0].Number != 1 || batch[1].Number != 2 {
		t.Fatalf("reconciled batch = %+v", batch)
	}
}
