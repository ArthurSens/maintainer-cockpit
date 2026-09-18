package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

func TestRepositoryProgressCollectedAt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
	}}); err != nil {
		t.Fatal(err)
	}
	if collectedAt, err := store.RepositoryProgressCollectedAt(ctx, "acme/widgets"); err != nil {
		t.Fatal(err)
	} else if !collectedAt.IsZero() {
		t.Fatalf("initial collection time = %v, want zero", collectedAt)
	}
	want := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	if err := store.RecordProgressSnapshot(ctx, "acme/widgets", nil, nil, want); err != nil {
		t.Fatal(err)
	}
	if got, err := store.RepositoryProgressCollectedAt(ctx, "acme/widgets"); err != nil {
		t.Fatal(err)
	} else if !got.Equal(want) {
		t.Errorf("collection time = %v, want %v", got, want)
	}
}

func TestProgressHistoryIsPrunedAfterThreeMonths(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
	}}); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	event := ProgressEvent{
		ID: "old-review", ActorID: 42, ActivityType: QualifyingActivityReview,
		Repository: "acme/widgets", Number: 1, Title: "Old PR",
		PullRequestURL: "https://github.com/acme/widgets/pull/1",
		ActivityURL:    "https://github.com/acme/widgets/pull/1#review",
		OccurredAt:     old, CollectionIDs: []string{"acme"},
	}
	if err := store.RecordProgressEvents(ctx, "acme/widgets", []ProgressEvent{event}, old); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordProgressEvents(
		ctx, "acme/widgets", nil, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatal(err)
	}
	progress, err := store.GetDailyProgress(ctx, 42, "acme", old)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Count != 0 {
		t.Errorf("old progress count = %d, want pruned", progress.Count)
	}
}

func TestResolvedReviewThreadCountsOnlyOnObservedTransition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
	}}); err != nil {
		t.Fatal(err)
	}
	thread := ReviewThreadState{
		ID: "thread-1", Repository: "acme/widgets", Number: 7, Title: "Discuss",
		PullRequestURL: "https://github.com/acme/widgets/pull/7",
		ActivityURL:    "https://github.com/acme/widgets/pull/7#discussion_r1",
		CollectionIDs:  []string{"acme"},
	}
	first := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	if err := store.RecordProgressSnapshot(
		ctx, "acme/widgets", nil, []ReviewThreadState{thread}, first,
	); err != nil {
		t.Fatal(err)
	}
	thread.Resolved = true
	thread.ResolvedBy = 42
	resolvedAt := first.Add(time.Hour)
	if err := store.RecordProgressSnapshot(
		ctx, "acme/widgets", nil, []ReviewThreadState{thread}, resolvedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordProgressSnapshot(
		ctx, "acme/widgets", nil, []ReviewThreadState{thread}, resolvedAt.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	progress, err := store.GetDailyProgress(ctx, 42, "acme", resolvedAt)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Count != 1 || len(progress.PullRequests) != 1 ||
		len(progress.PullRequests[0].Activities) != 1 ||
		!progress.PullRequests[0].Activities[0].OccurredAt.Equal(resolvedAt) {
		t.Errorf("resolved thread progress = %+v, want one activity at transition collection", progress)
	}
}
