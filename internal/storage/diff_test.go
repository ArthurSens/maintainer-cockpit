package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

func TestSnapshotPersistsHeadAndQueuesDiffBeforeAnalysis(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	store := diffTestStore(t)
	pr := PullRequest{
		Repository: "acme/widgets", Number: 7, Title: "Add evidence",
		Author: "octocat", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/7", HeadSHA: "head-7",
	}
	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []PullRequest{pr}, now); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimPullRequestDiff(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("ClaimPullRequestDiff() = %+v, %v", job, err)
	}
	if job.Repository != pr.Repository || job.Number != pr.Number || job.HeadSHA != pr.HeadSHA {
		t.Fatalf("diff job = %+v", job)
	}
	if analysis, err := store.ClaimAnalysis(ctx, now); err != nil || analysis != nil {
		t.Fatalf("analysis before diff = %+v, %v, want nil", analysis, err)
	}
}

func TestCompleteDiffCachesCurrentHeadAndEnqueuesAllCollectionAnalyses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	store := diffTestStore(t)
	pr := PullRequest{
		Repository: "acme/widgets", Number: 7, Title: "Add evidence",
		Author: "octocat", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/7", HeadSHA: "head-7",
	}
	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []PullRequest{pr}, now); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimPullRequestDiff(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("claim diff = %+v, %v", job, err)
	}
	patch := "@@ -1 +1 @@\n-old\n+new"
	evidence := PullRequestDiffEvidence{
		Completeness: "partial", OriginalFiles: 102, SentFiles: 1, OmittedFiles: 101,
		OriginalBytes: 9000, SentBytes: len(patch), Truncated: true,
		Sources: []PullRequestDiffSource{{
			Path: "main.go", Patch: patch, OriginalBytes: len(patch),
			SentBytes: len(patch), Truncated: false,
		}},
	}
	if err := store.CompletePullRequestDiff(ctx, job, evidence, now, nil); err != nil {
		t.Fatal(err)
	}
	analysisJob, err := store.ClaimAnalysis(ctx, now)
	if err != nil || analysisJob == nil {
		t.Fatalf("analysis after diff = %+v, %v", analysisJob, err)
	}
	request, err := store.BuildAnalysisRequest(ctx, *analysisJob)
	if err != nil {
		t.Fatal(err)
	}
	if request.DiffEvidence.Completeness != "partial" ||
		request.DiffEvidence.OmittedFiles != 101 || !request.DiffEvidence.Truncated {
		t.Fatalf("diff metadata = %+v", request.DiffEvidence)
	}
	found := false
	for _, source := range request.Sources {
		if source.ID == "diff:main.go" && source.Kind == "diff" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sources = %+v, want stable diff source", request.Sources)
	}

	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []PullRequest{pr}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if next, err := store.ClaimPullRequestDiff(ctx, now.Add(time.Hour)); err != nil || next != nil {
		t.Fatalf("cached diff claim = %+v, %v, want nil", next, err)
	}
}

func TestFailedDiffIsUnavailableAnalyzableAndRetryableOnRefresh(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	store := diffTestStore(t)
	pr := PullRequest{
		Repository: "acme/widgets", Number: 7, Title: "Add evidence",
		Author: "octocat", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/7", HeadSHA: "head-7",
	}
	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []PullRequest{pr}, now); err != nil {
		t.Fatal(err)
	}
	job, _ := store.ClaimPullRequestDiff(ctx, now)
	if err := store.CompletePullRequestDiff(
		ctx, job, PullRequestDiffEvidence{}, now, context.DeadlineExceeded,
	); err != nil {
		t.Fatal(err)
	}
	analysisJob, err := store.ClaimAnalysis(ctx, now)
	if err != nil || analysisJob == nil {
		t.Fatalf("analysis after failed diff = %+v, %v", analysisJob, err)
	}
	request, err := store.BuildAnalysisRequest(ctx, *analysisJob)
	if err != nil {
		t.Fatal(err)
	}
	if request.DiffEvidence.Completeness != "unavailable" {
		t.Fatalf("completeness = %q", request.DiffEvidence.Completeness)
	}
	if err := store.CompleteAnalysisJob(ctx, analysisJob.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []PullRequest{pr}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if retry, err := store.ClaimPullRequestDiff(ctx, now.Add(time.Hour)); err != nil || retry == nil {
		t.Fatalf("retry diff = %+v, %v", retry, err)
	}
}

func TestRunningOldHeadAnalysisCompletionQueuesCurrentHead(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	store := diffTestStore(t)
	pr := PullRequest{
		Repository: "acme/widgets", Number: 7, Title: "Add evidence",
		Author: "octocat", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/7", HeadSHA: "head-a",
	}
	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []PullRequest{pr}, now); err != nil {
		t.Fatal(err)
	}
	completeTestDiff(ctx, t, store, now)
	oldJob, err := store.ClaimAnalysis(ctx, now)
	if err != nil || oldJob == nil || oldJob.HeadSHA != "head-a" {
		t.Fatalf("old analysis = %+v, %v", oldJob, err)
	}

	pr.HeadSHA = "head-b"
	pr.UpdatedAt = now.Add(time.Minute)
	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []PullRequest{pr}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	completeTestDiff(ctx, t, store, now.Add(time.Minute))
	if current, err := store.ClaimAnalysis(ctx, now.Add(time.Minute)); err != nil || current != nil {
		t.Fatalf("analysis while old head runs = %+v, %v, want nil", current, err)
	}
	if err := store.CompleteAnalysisJob(ctx, oldJob.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	current, err := store.ClaimAnalysis(ctx, now.Add(2*time.Minute))
	if err != nil || current == nil || current.HeadSHA != "head-b" {
		t.Fatalf("replacement analysis = %+v, %v, want head-b", current, err)
	}
}

func TestBuildAndPersistenceRejectSupersededAnalysisHead(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	store := diffTestStore(t)
	pr := PullRequest{
		Repository: "acme/widgets", Number: 7, Title: "Add evidence",
		Author: "octocat", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/7", HeadSHA: "head-a",
	}
	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []PullRequest{pr}, now); err != nil {
		t.Fatal(err)
	}
	completeTestDiff(ctx, t, store, now)
	job, err := store.ClaimAnalysis(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("analysis = %+v, %v", job, err)
	}
	request, err := store.BuildAnalysisRequest(ctx, *job)
	if err != nil {
		t.Fatal(err)
	}

	pr.HeadSHA = "head-b"
	pr.UpdatedAt = now.Add(time.Minute)
	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []PullRequest{pr}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BuildAnalysisRequest(ctx, *job); !errors.Is(err, ErrAnalysisSuperseded) {
		t.Fatalf("BuildAnalysisRequest(old head) error = %v", err)
	}
	err = store.RecordAnalysisFailure(
		ctx, *job, request, "model", "", nil, nil, now,
		"failed", "provider unavailable",
	)
	if !errors.Is(err, ErrAnalysisSuperseded) {
		t.Fatalf("RecordAnalysisFailure(old head) error = %v", err)
	}
}

func completeTestDiff(
	ctx context.Context, t *testing.T, store *Store, now time.Time,
) {
	t.Helper()
	job, err := store.ClaimPullRequestDiff(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("diff job = %+v, %v", job, err)
	}
	if err := store.CompletePullRequestDiff(ctx, job, PullRequestDiffEvidence{
		Completeness: "complete",
	}, now, nil); err != nil {
		t.Fatal(err)
	}
}

func diffTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(context.Background(), []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
		ModelProvider: "model",
	}}); err != nil {
		t.Fatal(err)
	}
	return store
}
