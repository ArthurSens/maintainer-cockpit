package evidenceworker

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/contribution"
	"github.com/ArthurSens/maintainer-cockpit/internal/githubapp"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestRunOnceCompletesRepositoryAndAncillaryEvidence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := evidenceStore(t, now, []storage.EvidenceAuthor{{
		ID: 42, Login: "octocat", Association: "CONTRIBUTOR",
	}})
	worker := New(
		store,
		repositoryCollectorFunc(func(
			context.Context, int64, string, string, string,
		) (githubapp.ContributionResult, error) {
			return githubapp.ContributionResult{
				Evidence: githubapp.RepositoryContributionEvidence{
					History: contribution.RepositoryHistory{
						Association: "MEMBER", Merged: 3, ClosedUnmerged: 1, Open: 1,
					},
				},
			}, nil
		}),
		ancillaryCollectorFunc(func(
			context.Context, int64, string,
		) (githubapp.AncillaryAuthorEvidence, error) {
			return githubapp.AncillaryAuthorEvidence{
				AccountCreatedAt: now.AddDate(-1, 0, 0),
				RecentActivity: []contribution.RepositoryActivity{{
					Repository: "other/tools", OccurredAt: now.Add(-time.Hour),
				}},
			}, nil
		}),
		Options{Concurrency: 2, Now: func() time.Time { return now }},
	)
	if err := worker.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	author, err := store.GetAuthorContext(ctx, "mixed", "octocat", now)
	if err != nil {
		t.Fatal(err)
	}
	if author.Contribution.Completeness != contribution.CompletenessComplete ||
		author.Contribution.Counts.Merged != 3 ||
		author.Contribution.Association != "MEMBER" {
		t.Errorf("contribution = %+v", author.Contribution)
	}
}

func TestRepositoryFailurePreservesPriorEvidenceAndCoreFreshness(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	authors := []storage.EvidenceAuthor{{
		ID: 42, Login: "octocat", Association: "CONTRIBUTOR",
	}}
	store := evidenceStore(t, now, authors)
	repositoryJob, err := store.ClaimContributionEvidence(ctx, now)
	if err != nil || repositoryJob == nil {
		t.Fatalf("ClaimContributionEvidence() = %+v, %v", repositoryJob, err)
	}
	if err := store.CompleteContributionEvidence(
		ctx, repositoryJob,
		contribution.RepositoryHistory{Association: "MEMBER", Merged: 4},
		now, nil,
	); err != nil {
		t.Fatal(err)
	}
	ancillaryJob, err := store.ClaimAncillaryAuthorEvidence(ctx, now)
	if err != nil || ancillaryJob == nil {
		t.Fatalf("ClaimAncillaryAuthorEvidence() = %+v, %v", ancillaryJob, err)
	}
	if err := store.CompleteAncillaryAuthorEvidence(
		ctx, ancillaryJob, now.AddDate(-1, 0, 0), nil, now, nil,
	); err != nil {
		t.Fatal(err)
	}

	staleAt := now.Add(8 * 24 * time.Hour)
	if err := store.EnqueueAuthorEvidenceAfterSnapshot(
		ctx, "acme/widgets", authors, staleAt,
	); err != nil {
		t.Fatal(err)
	}
	worker := New(
		store,
		repositoryCollectorFunc(func(
			context.Context, int64, string, string, string,
		) (githubapp.ContributionResult, error) {
			return githubapp.ContributionResult{}, errors.New("owner route failed")
		}),
		ancillaryCollectorFunc(func(
			context.Context, int64, string,
		) (githubapp.AncillaryAuthorEvidence, error) {
			t.Fatal("ancillary work exceeded RunOnce concurrency")
			return githubapp.AncillaryAuthorEvidence{}, nil
		}),
		Options{Concurrency: 1, Now: func() time.Time { return staleAt }},
	)
	if err := worker.RunOnce(ctx, staleAt); err != nil {
		t.Fatal(err)
	}
	author, err := store.GetAuthorContext(ctx, "mixed", "octocat", staleAt)
	if err != nil {
		t.Fatal(err)
	}
	if author.Contribution.Completeness != contribution.CompletenessIncomplete ||
		author.Contribution.Counts.Merged != 4 ||
		author.Contribution.Association != "MEMBER" {
		t.Errorf("failed refresh contribution = %+v", author.Contribution)
	}
	refreshes, err := store.ListRepositoryRefreshes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, refresh := range refreshes {
		if refresh.Repository == "acme/widgets" && refresh.State != storage.RefreshFresh {
			t.Errorf("core refresh state = %q, want fresh", refresh.State)
		}
	}
}

func TestAncillaryFailureMarksOnlyContributionIncomplete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := evidenceStore(t, now, []storage.EvidenceAuthor{{ID: 42, Login: "octocat"}})
	worker := New(
		store,
		repositoryCollectorFunc(func(
			context.Context, int64, string, string, string,
		) (githubapp.ContributionResult, error) {
			return githubapp.ContributionResult{Evidence: githubapp.RepositoryContributionEvidence{
				History: contribution.RepositoryHistory{Merged: 1},
			}}, nil
		}),
		ancillaryCollectorFunc(func(
			context.Context, int64, string,
		) (githubapp.AncillaryAuthorEvidence, error) {
			return githubapp.AncillaryAuthorEvidence{}, errors.New("public rate limited")
		}),
		Options{Concurrency: 2, Now: func() time.Time { return now }},
	)
	if err := worker.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	author, err := store.GetAuthorContext(ctx, "mixed", "octocat", now)
	if err != nil {
		t.Fatal(err)
	}
	if author.Contribution.Completeness != contribution.CompletenessIncomplete ||
		author.Contribution.Counts.Merged != 1 {
		t.Errorf("contribution = %+v", author.Contribution)
	}
	assertCoreFresh(t, store)
	routes, err := store.ListRouteAttempts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if len(route.Attempts) != 0 {
			t.Fatalf("ancillary health created route attempts: %+v", routes)
		}
	}
}

func TestRepositoryWorkerPersistsAttemptsOnFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := evidenceStore(t, now, []storage.EvidenceAuthor{{ID: 42, Login: "octocat"}})
	worker := New(
		store,
		repositoryCollectorFunc(func(
			context.Context, int64, string, string, string,
		) (githubapp.ContributionResult, error) {
			return githubapp.ContributionResult{Attempts: githubapp.AttemptSet{{
				Operation: githubapp.OperationContributionEvidence,
				Profile:   "primary", Priority: 0, ProfileType: githubapp.ProfileGitHubApp,
				Outcome: githubapp.RouteOutcomeFailed, Failure: githubapp.FailureGitHubUnavailable,
			}}}, errors.New("provider unavailable")
		}),
		ancillaryCollectorFunc(func(context.Context, int64, string) (githubapp.AncillaryAuthorEvidence, error) {
			return githubapp.AncillaryAuthorEvidence{}, nil
		}),
		Options{Concurrency: 1, Now: func() time.Time { return now }},
	)
	if err := worker.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	routes, err := store.ListRouteAttempts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Operation != storage.RouteOperationContribution ||
		len(routes[0].Attempts) != 1 ||
		routes[0].Attempts[0].FailureCategory != storage.RouteFailureGitHubUnavailable {
		t.Fatalf("route attempts = %+v", routes)
	}
}

func TestRunOnceHonorsConcurrencyBound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := evidenceStore(t, now, []storage.EvidenceAuthor{
		{ID: 41, Login: "one"}, {ID: 42, Login: "two"}, {ID: 43, Login: "three"},
	})
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	block := func() {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
	}
	repository := repositoryCollectorFunc(func(
		context.Context, int64, string, string, string,
	) (githubapp.ContributionResult, error) {
		block()
		return githubapp.ContributionResult{}, nil
	})
	worker := New(store, repository, ancillaryCollectorFunc(func(
		context.Context, int64, string,
	) (githubapp.AncillaryAuthorEvidence, error) {
		block()
		return githubapp.AncillaryAuthorEvidence{}, nil
	}), Options{Concurrency: 2, Now: func() time.Time { return now }})
	done := make(chan error, 1)
	go func() { done <- worker.RunOnce(ctx, now) }()
	<-started
	<-started
	select {
	case <-started:
		t.Fatal("RunOnce started more than two jobs")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Errorf("maximum concurrency = %d, want 2", maximum.Load())
	}
}

func TestRunOnceAlternatesQueuesWhenRepositoryQueueIsFull(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := evidenceStore(t, now, []storage.EvidenceAuthor{
		{ID: 41, Login: "one"}, {ID: 42, Login: "two"},
	})
	var repositoryCalls atomic.Int32
	var ancillaryCalls atomic.Int32
	worker := New(
		store,
		repositoryCollectorFunc(func(
			context.Context, int64, string, string, string,
		) (githubapp.ContributionResult, error) {
			repositoryCalls.Add(1)
			return githubapp.ContributionResult{}, nil
		}),
		ancillaryCollectorFunc(func(
			context.Context, int64, string,
		) (githubapp.AncillaryAuthorEvidence, error) {
			ancillaryCalls.Add(1)
			return githubapp.AncillaryAuthorEvidence{}, nil
		}),
		Options{Concurrency: 1, Now: func() time.Time { return now }},
	)
	if err := worker.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if repositoryCalls.Load() != 1 || ancillaryCalls.Load() != 1 {
		t.Fatalf("calls repository=%d ancillary=%d, want one each",
			repositoryCalls.Load(), ancillaryCalls.Load())
	}
}

func TestRunOnceFairlyProcessesDiffQueueAndReleasesAnalysis(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "mixed", Name: "Mixed", Repositories: []string{"acme/widgets"},
		ModelProvider: "model",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRepositorySnapshot(ctx, "acme/widgets", []storage.PullRequest{{
		Repository: "acme/widgets", Number: 1, HeadSHA: "head-1", Title: "octocat",
		Author: "octocat", AuthorID: 42, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/1", ReviewState: "none",
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueAuthorEvidenceAfterSnapshot(
		ctx, "acme/widgets",
		[]storage.EvidenceAuthor{{ID: 42, Login: "octocat"}}, now,
	); err != nil {
		t.Fatal(err)
	}
	var diffCalls atomic.Int32
	worker := New(
		store,
		repositoryCollectorFunc(func(
			context.Context, int64, string, string, string,
		) (githubapp.ContributionResult, error) {
			return githubapp.ContributionResult{}, nil
		}),
		ancillaryCollectorFunc(func(
			context.Context, int64, string,
		) (githubapp.AncillaryAuthorEvidence, error) {
			return githubapp.AncillaryAuthorEvidence{}, nil
		}),
		Options{
			Concurrency: 3, Now: func() time.Time { return now },
			Diff: diffCollectorFunc(func(
				context.Context, string, int, string, int,
			) (githubapp.DiffResult, error) {
				diffCalls.Add(1)
				return githubapp.DiffResult{Evidence: githubapp.PullRequestDiffEvidence{
					Completeness: "complete",
					Sources: []githubapp.PullRequestDiffSource{{
						Path: "main.go", Patch: "patch", OriginalBytes: 5, SentBytes: 5,
					}},
					OriginalFiles: 1, SentFiles: 1, OriginalBytes: 5, SentBytes: 5,
				}}, nil
			}),
		},
	)
	if err := worker.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if diffCalls.Load() != 1 {
		t.Fatalf("diff calls = %d, want 1", diffCalls.Load())
	}
	if job, err := store.ClaimAnalysis(ctx, now); err != nil || job == nil {
		t.Fatalf("analysis job = %+v, %v", job, err)
	}
}

func TestHeadChangedDiffDoesNotReleaseAnalysis(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "mixed", Name: "Mixed", Repositories: []string{"acme/widgets"},
		ModelProvider: "model",
	}}); err != nil {
		t.Fatal(err)
	}
	pr := storage.PullRequest{
		Repository: "acme/widgets", Number: 1, HeadSHA: "head-1", Title: "Change",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/1",
	}
	if err := store.ReplaceRepositorySnapshot(ctx, pr.Repository, []storage.PullRequest{pr}, now); err != nil {
		t.Fatal(err)
	}
	worker := New(store, nil, nil, Options{
		Concurrency: 1, Now: func() time.Time { return now },
		Diff: diffCollectorFunc(func(
			context.Context, string, int, string, int,
		) (githubapp.DiffResult, error) {
			return githubapp.DiffResult{}, &githubapp.PullRequestHeadChangedError{
				Expected: "head-1", Observed: "head-2",
			}
		}),
	})
	if err := worker.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if job, err := store.ClaimAnalysis(ctx, now); err != nil || job != nil {
		t.Fatalf("analysis after superseded diff = %+v, %v, want nil", job, err)
	}
	if err := store.ReplaceRepositorySnapshot(
		ctx, pr.Repository, []storage.PullRequest{pr}, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if job, err := store.ClaimPullRequestDiff(ctx, now.Add(time.Minute)); err != nil || job == nil {
		t.Fatalf("replacement diff = %+v, %v", job, err)
	}
}

func TestChildTimeoutsCompleteEvidenceJobsAndPreservePriorEvidence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	authors := []storage.EvidenceAuthor{{ID: 42, Login: "octocat"}}
	store := evidenceStore(t, now, authors)
	repositoryJob, err := store.ClaimContributionEvidence(ctx, now)
	if err != nil || repositoryJob == nil {
		t.Fatalf("ClaimContributionEvidence() = %+v, %v", repositoryJob, err)
	}
	if err := store.CompleteContributionEvidence(
		ctx, repositoryJob, contribution.RepositoryHistory{Merged: 4}, now, nil,
	); err != nil {
		t.Fatal(err)
	}
	ancillaryJob, err := store.ClaimAncillaryAuthorEvidence(ctx, now)
	if err != nil || ancillaryJob == nil {
		t.Fatalf("ClaimAncillaryAuthorEvidence() = %+v, %v", ancillaryJob, err)
	}
	if err := store.CompleteAncillaryAuthorEvidence(
		ctx, ancillaryJob, now.AddDate(-1, 0, 0), nil, now, nil,
	); err != nil {
		t.Fatal(err)
	}
	later := now.Add(8 * 24 * time.Hour)
	if err := store.EnqueueAuthorEvidenceAfterSnapshot(
		ctx, "acme/widgets", authors, later,
	); err != nil {
		t.Fatal(err)
	}
	worker := New(
		store,
		repositoryCollectorFunc(func(
			ctx context.Context, _ int64, _, _, _ string,
		) (githubapp.ContributionResult, error) {
			<-ctx.Done()
			return githubapp.ContributionResult{}, ctx.Err()
		}),
		ancillaryCollectorFunc(func(
			ctx context.Context, _ int64, _ string,
		) (githubapp.AncillaryAuthorEvidence, error) {
			<-ctx.Done()
			return githubapp.AncillaryAuthorEvidence{}, ctx.Err()
		}),
		Options{Concurrency: 2, JobTimeout: time.Millisecond, Now: func() time.Time { return later }},
	)
	if err := worker.RunOnce(ctx, later); err != nil {
		t.Fatal(err)
	}
	author, err := store.GetAuthorContext(ctx, "mixed", "octocat", later)
	if err != nil {
		t.Fatal(err)
	}
	if author.Contribution.Completeness != contribution.CompletenessIncomplete ||
		author.Contribution.Counts.Merged != 4 {
		t.Fatalf("timed-out contribution = %+v", author.Contribution)
	}
	if err := store.RecoverEvidenceJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if job, err := store.ClaimContributionEvidence(ctx, later); err != nil || job != nil {
		t.Fatalf("repository job after recovery = %+v, %v, want nil", job, err)
	}
	if job, err := store.ClaimAncillaryAuthorEvidence(ctx, later); err != nil || job != nil {
		t.Fatalf("ancillary job after recovery = %+v, %v, want nil", job, err)
	}
}

func TestRunCancellationAndRecovery(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := evidenceStore(t, now, []storage.EvidenceAuthor{{ID: 42, Login: "octocat"}})
	claimed, err := store.ClaimContributionEvidence(context.Background(), now)
	if err != nil || claimed == nil {
		t.Fatalf("initial claim = %+v, %v", claimed, err)
	}
	if err := store.RecoverEvidenceJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.ClaimContributionEvidence(context.Background(), now)
	if err != nil || recovered == nil || recovered.ID != claimed.ID {
		t.Fatalf("recovered claim = %+v, %v, want ID %d", recovered, err, claimed.ID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker := New(
		store,
		repositoryCollectorFunc(func(
			context.Context, int64, string, string, string,
		) (githubapp.ContributionResult, error) {
			return githubapp.ContributionResult{}, nil
		}),
		ancillaryCollectorFunc(func(
			context.Context, int64, string,
		) (githubapp.AncillaryAuthorEvidence, error) {
			return githubapp.AncillaryAuthorEvidence{}, nil
		}),
		Options{},
	)
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run(canceled) error = %v", err)
	}
	if err := store.RecoverEvidenceJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterCancellation, err := store.ClaimContributionEvidence(context.Background(), now)
	if err != nil || afterCancellation == nil || afterCancellation.ID != claimed.ID {
		t.Fatalf("claim after cancellation recovery = %+v, %v", afterCancellation, err)
	}
}

func evidenceStore(
	t *testing.T, now time.Time, authors []storage.EvidenceAuthor,
) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(context.Background(), []config.Collection{{
		ID: "mixed", Name: "Mixed", Repositories: []string{"acme/widgets"},
	}}); err != nil {
		t.Fatal(err)
	}
	pullRequests := make([]storage.PullRequest, 0, len(authors))
	for index, author := range authors {
		pullRequests = append(pullRequests, storage.PullRequest{
			Repository: "acme/widgets", Number: index + 1, Title: author.Login,
			Author: author.Login, AuthorID: author.ID,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
			URL:         "https://github.com/acme/widgets/pull/" + author.Login,
			ReviewState: "none",
		})
	}
	if err := store.ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", pullRequests, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueAuthorEvidenceAfterSnapshot(
		context.Background(), "acme/widgets", authors, now,
	); err != nil {
		t.Fatal(err)
	}
	return store
}

func assertCoreFresh(t *testing.T, store *storage.Store) {
	t.Helper()
	refreshes, err := store.ListRepositoryRefreshes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshes) != 1 || refreshes[0].State != storage.RefreshFresh {
		t.Errorf("repository refreshes = %+v, want fresh", refreshes)
	}
}

type repositoryCollectorFunc func(
	context.Context, int64, string, string, string,
) (githubapp.ContributionResult, error)

func (f repositoryCollectorFunc) CollectRepositoryContribution(
	ctx context.Context, authorID int64, login, repository, association string,
) (githubapp.ContributionResult, error) {
	return f(ctx, authorID, login, repository, association)
}

type ancillaryCollectorFunc func(
	context.Context, int64, string,
) (githubapp.AncillaryAuthorEvidence, error)

func (f ancillaryCollectorFunc) CollectAncillaryAuthorEvidence(
	ctx context.Context, authorID int64, login string,
) (githubapp.AncillaryAuthorEvidence, error) {
	return f(ctx, authorID, login)
}

type diffCollectorFunc func(
	context.Context, string, int, string, int,
) (githubapp.DiffResult, error)

func (f diffCollectorFunc) CollectPullRequestDiff(
	ctx context.Context, repository string, number int, headSHA string, changedFiles int,
) (githubapp.DiffResult, error) {
	return f(ctx, repository, number, headSHA, changedFiles)
}
