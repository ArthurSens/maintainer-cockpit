package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

func TestRemovedCollectionCanBeRecoveredForSevenDays(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	collection := config.Collection{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
	}
	if err := store.SyncCollectionsAt(ctx, []config.Collection{collection}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPullRequest(ctx, "acme", PullRequest{
		Repository: "acme/widgets", Number: 7, Title: "Preserve me",
		UpdatedAt: time.Now().UTC(), URL: "https://github.com/acme/widgets/pull/7",
	}); err != nil {
		t.Fatal(err)
	}
	removedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := store.SyncCollectionsAt(ctx, nil, removedAt); err != nil {
		t.Fatal(err)
	}
	if collections, err := store.ListCollections(ctx); err != nil || len(collections) != 0 {
		t.Fatalf("ListCollections() after removal = %+v, %v", collections, err)
	}
	if err := store.SyncCollectionsAt(ctx, []config.Collection{collection}, removedAt.Add(6*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if pullRequests, err := store.ListPullRequests(ctx, "acme"); err != nil || len(pullRequests) != 1 {
		t.Fatalf("recovered pull requests = %+v, %v", pullRequests, err)
	}
}

func TestRetentionExpiresOperationalDataAndRemovedCollections(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	collection := config.Collection{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
	}
	if err := store.SyncCollectionsAt(ctx, []config.Collection{collection}, now.Add(-10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO retained_model_payloads (id, request_json, response_json, created_at)
		VALUES
			('expired', 'sensitive request', 'sensitive response', ?),
			('current', '{}', '{}', ?)
	`, formatTime(now.AddDate(0, -3, -1)), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncCollectionsAt(ctx, nil, now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	result, err := store.RunRetention(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelPayloads != 1 || result.Collections != 1 {
		t.Errorf("RunRetention() = %+v, want one payload and one collection removed", result)
	}
	var payloads int
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM retained_model_payloads",
	).Scan(&payloads); err != nil {
		t.Fatal(err)
	}
	if payloads != 1 {
		t.Errorf("retained payloads = %d, want 1", payloads)
	}
}

func TestAuditRejectsSensitiveOrUnknownDetails(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.RecordAudit(ctx, "reload_succeeded", nil, "configuration_changed", now); err != nil {
		t.Fatalf("RecordAudit(valid) error = %v", err)
	}
	for _, test := range []struct {
		event, detail string
	}{
		{event: "unknown_event"},
		{event: "reload_failed", detail: "token=secret"},
		{event: "reload_failed", detail: "raw prompt content"},
	} {
		if err := store.RecordAudit(ctx, test.event, nil, test.detail, now); err == nil {
			t.Errorf("RecordAudit(%q, %q) error = nil", test.event, test.detail)
		}
	}
}

func TestRefreshPreservesCurrentAnalysisAndArchivesClosedAnalysis(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
	}}); err != nil {
		t.Fatal(err)
	}
	pr := PullRequest{
		Repository: "acme/widgets", Number: 7, Title: "Analyzed",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/7", ReviewState: "none",
	}
	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []PullRequest{pr}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO analysis_results (
			collection_id, repository, number, status, input_revision,
			provider, fallback_from, model, analyzed_at, payload_id,
			result_json, quality_level, error, schema_version, prompt_version
		) VALUES ('acme', 'acme/widgets', 7, 'complete', 'revision',
			'provider', '', 'model', ?, 'payload', '{}',
			'no_concerns', '', 'analysis.v2', 'analysis-prompt.v2')
	`, formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRepositorySnapshot(
		ctx, pr.Repository, []PullRequest{pr}, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	var current int
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM analysis_results",
	).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != 1 {
		t.Fatalf("current analyses after unchanged refresh = %d, want 1", current)
	}
	if err := store.ReplaceRepositorySnapshot(
		ctx, pr.Repository, nil, now.Add(2*time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	var archived int
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM retained_analysis_results",
	).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 1 {
		t.Errorf("archived analyses after close = %d, want 1", archived)
	}
}
