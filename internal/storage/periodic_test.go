package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

func TestScheduleInitializationIsDurableAndIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schedule.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	first, err := store.InitializeSchedule(ctx, "github", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first initialization = false, want true")
	}
	first, err = store.InitializeSchedule(ctx, "github", "", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first {
		t.Fatal("second initialization = true, want false")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	first, err = reopened.InitializeSchedule(ctx, "github", "", now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first {
		t.Fatal("reopened initialization = true, want durable false")
	}
}

func TestEnqueueCollectionEvidenceUsesLatestStoredAuthorsAndCoalesces(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	const repository = "acme/widgets"
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{repository},
		Contribution: &config.Contribution{Schedule: "0 3 * * 1"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRepositorySnapshot(ctx, repository, []PullRequest{{
		Repository: repository, Number: 1, Title: "Latest author",
		Author: "octocat", AuthorID: 42, CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/1",
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueCollectionEvidence(ctx, "acme", now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueCollectionEvidence(ctx, "acme", now); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimContributionEvidence(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("ClaimContributionEvidence() = %+v, %v", job, err)
	}
	if job.AuthorID != 42 || job.Repository != repository {
		t.Errorf("job = %+v, want latest stored author and repository", job)
	}
	if extra, err := store.ClaimContributionEvidence(ctx, now); err != nil || extra != nil {
		t.Fatalf("coalesced extra job = %+v, %v", extra, err)
	}
	ancillary, err := store.ClaimAncillaryAuthorEvidence(ctx, now)
	if err != nil || ancillary == nil || ancillary.AuthorID != 42 {
		t.Fatalf("ClaimAncillaryAuthorEvidence() = %+v, %v", ancillary, err)
	}
}

func TestDeleteScheduleStateMakesReenabledTargetBootstrap(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	if _, err := store.InitializeSchedule(ctx, "correlation", "acme", now); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteScheduleState(ctx, "correlation", "acme"); err != nil {
		t.Fatal(err)
	}
	first, err := store.InitializeSchedule(ctx, "correlation", "acme", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("reenabled initialization = false, want bootstrap")
	}
}

func TestEnqueueAllRefreshesCoalescesRepositories(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/one", "acme/two"},
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	if err := store.EnqueueAllRefreshes(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueAllRefreshes(ctx, now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if job, err := store.ClaimRefresh(ctx, now); err != nil || job == nil {
			t.Fatalf("ClaimRefresh() = %+v, %v", job, err)
		}
	}
	if job, err := store.ClaimRefresh(ctx, now); err != nil || job != nil {
		t.Fatalf("coalesced extra refresh = %+v, %v", job, err)
	}
}

func TestCancelDisabledContributionEvidenceLeavesRunningWork(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	const repository = "acme/widgets"
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{repository},
		Contribution: &config.Contribution{Schedule: "0 3 * * 1"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueAuthorEvidenceAfterSnapshot(
		ctx, repository, []EvidenceAuthor{{ID: 42, Login: "octocat"}}, now,
	); err != nil {
		t.Fatal(err)
	}
	running, err := store.ClaimContributionEvidence(ctx, now)
	if err != nil || running == nil {
		t.Fatalf("running job = %+v, %v", running, err)
	}
	if err := store.CancelDisabledContributionEvidence(ctx, nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var runningCount, queuedCount int
	if err := store.db.QueryRow(`
		SELECT COUNT(*) FROM contribution_evidence_jobs WHERE status = 'running'
	`).Scan(&runningCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`
		SELECT COUNT(*) FROM contribution_evidence_jobs WHERE status = 'queued'
	`).Scan(&queuedCount); err != nil {
		t.Fatal(err)
	}
	if runningCount != 1 || queuedCount != 0 {
		t.Fatalf("running = %d, queued = %d, want 1 and 0", runningCount, queuedCount)
	}
}
