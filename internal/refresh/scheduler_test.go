package refresh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/githubapp"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestRunOnceOnlyProcessesExplicitlyQueuedRefreshes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
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
	collector := &phasedCollectorStub{discovery: githubapp.DiscoveryResult{Repository: repository}}
	scheduler := newScheduler(store, collector, schedulerOptions{Concurrency: 1})
	now := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	if err := scheduler.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if collector.discoveryCalls != 0 {
		t.Fatalf("discovery calls without queued work = %d, want 0", collector.discoveryCalls)
	}
	if _, err := store.EnqueueRefresh(ctx, repository, now); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if collector.discoveryCalls != 1 {
		t.Errorf("discovery calls for queued work = %d, want 1", collector.discoveryCalls)
	}
}

func TestRunOncePreservesSnapshotOnIncompleteRefreshThenPublishesCompleteResult(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const repository = "prometheus/prometheus"
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "prometheus", Name: "Prometheus", Repositories: []string{repository},
	}}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	if err := store.ReplaceRepositorySnapshot(ctx, repository, []storage.PullRequest{
		storedPullRequest(repository, 1, "Last complete"),
	}, at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueRefresh(ctx, repository, at); err != nil {
		t.Fatal(err)
	}

	discoveryAttempt := 0
	collector := &phasedCollectorStub{
		discovery: githubapp.DiscoveryResult{
			Repository: repository,
			PullRequests: []githubapp.DiscoveredPullRequest{{
				Number: 2, CollectionIDs: []string{"prometheus"},
			}},
		},
		now: at.Add(time.Hour),
		discover: func(
			context.Context, string,
		) (githubapp.RoutedDiscoveryResult, error) {
			discoveryAttempt++
			if discoveryAttempt == 1 {
				return githubapp.RoutedDiscoveryResult{},
					incompleteTestError{errors.New("discovery failed")}
			}
			return githubapp.RoutedDiscoveryResult{Discovery: githubapp.DiscoveryResult{
				Repository: repository,
				PullRequests: []githubapp.DiscoveredPullRequest{{
					Number: 2, CollectionIDs: []string{"prometheus"},
				}},
			}}, nil
		},
		hydrate: func(
			_ context.Context, _ string, _ []githubapp.DiscoveredPullRequest,
			_ githubapp.HydrationOptions,
		) (githubapp.RoutedHydrationResult, error) {
			pr := githubapp.PullRequest{
				Repository: repository, Number: 2, Title: "New complete",
				Author: "octocat", CreatedAt: at.Add(-24 * time.Hour), UpdatedAt: at,
				URL:       "https://github.com/prometheus/prometheus/pull/2",
				Additions: 4, Deletions: 1, ReviewState: "approved",
			}
			return githubapp.RoutedHydrationResult{Hydration: githubapp.HydrationResult{
				Repository: repository, CollectedAt: at.Add(time.Hour),
				PullRequests: []githubapp.PullRequest{pr},
				Fingerprints: map[int]githubapp.InputFingerprint{2: {
					Version: githubapp.InputFingerprintVersion, Value: "two",
				}},
				CacheMisses: 1,
			}}, nil
		},
	}
	currentTime := at
	scheduler := newScheduler(store, collector, schedulerOptions{
		Concurrency: 1, Now: func() time.Time { return currentTime },
	})
	if err := scheduler.RunOnce(ctx, at); err != nil {
		t.Fatalf("RunOnce(incomplete) error = %v", err)
	}
	got, err := store.ListPullRequests(ctx, "prometheus")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("incomplete refresh replaced snapshot: %+v", got)
	}
	collections, err := store.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if collections[0].RefreshState != storage.RefreshIncomplete {
		t.Errorf("RefreshState = %q, want incomplete", collections[0].RefreshState)
	}

	if _, err := store.EnqueueRefresh(ctx, repository, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	currentTime = at.Add(time.Hour)
	if err := scheduler.RunOnce(ctx, at.Add(time.Hour)); err != nil {
		t.Fatalf("RunOnce(success) error = %v", err)
	}
	for range 4 {
		if err := scheduler.RunOnce(ctx, at.Add(time.Hour)); err != nil {
			t.Fatalf("RunOnce(success phase) error = %v", err)
		}
	}
	got, err = store.ListPullRequests(ctx, "prometheus")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != 2 || got[0].ReviewState != "approved" {
		t.Errorf("complete refresh snapshot = %+v", got)
	}
}

func TestRunOnceClaimsNoMoreThanConfiguredConcurrency(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repositories := []string{"acme/one", "acme/two", "acme/three"}
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: repositories,
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, repository := range repositories {
		if _, err := store.EnqueueRefresh(ctx, repository, now); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	calls := 0
	collector := &phasedCollectorStub{discover: func(
		_ context.Context, repository string,
	) (githubapp.RoutedDiscoveryResult, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return githubapp.RoutedDiscoveryResult{Discovery: githubapp.DiscoveryResult{
			Repository: repository,
		}}, nil
	}}
	scheduler := newScheduler(store, collector, schedulerOptions{Concurrency: 2})
	if err := scheduler.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("collector calls = %d, want concurrency bound 2", calls)
	}
}

func TestRunOnceRecordsFailedRefresh(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Now().UTC()
	if _, err := store.EnqueueRefresh(ctx, repository, now); err != nil {
		t.Fatal(err)
	}
	scheduler := newScheduler(store, &phasedCollectorStub{discover: func(
		context.Context, string,
	) (githubapp.RoutedDiscoveryResult, error) {
		return githubapp.RoutedDiscoveryResult{}, errors.New("installation authentication failed")
	}}, schedulerOptions{Concurrency: 1})
	if err := scheduler.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	collections, err := store.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if collections[0].RefreshState != storage.RefreshFailed {
		t.Errorf("RefreshState = %q, want failed", collections[0].RefreshState)
	}
	if collections[0].RefreshError != "GitHub refresh failed." {
		t.Errorf("RefreshError = %q, want sanitized public message", collections[0].RefreshError)
	}
}

func TestRunOnceLogsActionableSecretSafeFailureSummary(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	lastSuccess := now.Add(-time.Hour)
	if err := store.ReplaceRepositorySnapshot(ctx, repository, nil, lastSuccess); err != nil {
		t.Fatal(err)
	}
	jobID, err := store.EnqueueRefresh(ctx, repository, now)
	if err != nil || !jobID {
		t.Fatalf("EnqueueRefresh() = %v, %v", jobID, err)
	}
	const secret = "secret-token"
	apiErr := &githubapp.APIError{
		StatusCode: http.StatusUnprocessableEntity, Method: http.MethodGet,
		Endpoint: "/repos/{owner}/{repo}/pulls", RequestID: "REQ_123",
		RetryAfter: 17 * time.Second, RateLimitResetUnix: now.Add(time.Minute).Unix(),
	}
	collector := &phasedCollectorStub{
		discover: func(context.Context, string) (githubapp.RoutedDiscoveryResult, error) {
			result := githubapp.RoutedDiscoveryResult{
				Attempts: githubapp.AttemptSet{
					{
						Operation: githubapp.OperationDiscovery, Profile: "primary",
						Priority: 0, ProfileType: githubapp.ProfileGitHubApp,
						Outcome:     githubapp.RouteOutcomeAdvanced,
						Failure:     githubapp.FailurePermissionMissing,
						Detail:      "The profile lacks a required GitHub permission.",
						Remediation: "Grant the required GitHub App or token permissions.",
					},
					{
						Operation: githubapp.OperationDiscovery, Profile: "fallback profile",
						Priority: 1, ProfileType: githubapp.ProfileFineGrainedPAT,
						Outcome:     githubapp.RouteOutcomeFailed,
						Failure:     githubapp.FailureSecondaryRateLimited,
						Detail:      "GitHub secondary rate limit prevented collection.",
						Remediation: "Reduce request concurrency and retry after GitHub's limit clears.",
					},
				},
			}
			return result, githubapp.NewRouteFailure(
				githubapp.FailurePermissionMissing,
				fmt.Errorf("raw body %s: %w", secret, apiErr),
			)
		},
	}
	var output bytes.Buffer
	scheduler := newScheduler(store, collector, schedulerOptions{
		Concurrency: 1, Now: func() time.Time { return now.Add(2500 * time.Millisecond) },
		Logger: slog.New(slog.NewTextHandler(&output, nil)),
	})
	if err := scheduler.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	logs := loggedLines(&output)
	if len(logs) != 1 {
		t.Fatalf("logs = %#v, want one failure summary", logs)
	}
	for _, field := range []string{
		`event="repository_refresh_failed"`, `repository="acme/widgets"`,
		`operation="discovery"`, `job_id=1`, `duration_ms=2500`,
		`last_successful_refresh="2026-09-11T11:00:00Z"`,
		`profile="fallback profile"`, `profile_type="fine_grained_pat"`,
		`priority=1`, `attempt_count=2`, `fallback_attempted=true`,
		`fallback_selected=false`,
		`category="secondary_rate_limited"`, `phase="pull_requests"`,
		`http_status=422`, `endpoint="/repos/{owner}/{repo}/pulls"`,
		`request_id="REQ_123"`, `retry_after_ms=17000`,
		`rate_limit_reset_unix=` + strconv.FormatInt(now.Add(time.Minute).Unix(), 10),
	} {
		if !containsLogField(logs[0], field) {
			t.Errorf("log %q missing %q", logs[0], field)
		}
	}
	for _, forbidden := range []string{secret, "raw body", "access_token", "response body"} {
		if strings.Contains(logs[0], forbidden) {
			t.Errorf("log %q contains forbidden %q", logs[0], forbidden)
		}
	}
}

func TestRunOnceRequeuesChildTimeoutForRetry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if _, err := store.EnqueueRefresh(ctx, repository, now); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	scheduler := newScheduler(
		store,
		&phasedCollectorStub{discover: func(ctx context.Context, _ string) (githubapp.RoutedDiscoveryResult, error) {
			<-ctx.Done()
			return githubapp.RoutedDiscoveryResult{}, ctx.Err()
		}},
		schedulerOptions{
			Concurrency: 1, JobTimeout: time.Millisecond,
			Logger: slog.New(slog.NewTextHandler(&output, nil)),
		},
	)
	if err := scheduler.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	collections, err := store.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if collections[0].RefreshState != storage.RefreshRunning {
		t.Fatalf("RefreshState = %q, want running retry", collections[0].RefreshState)
	}
	enqueued, err := store.EnqueueRefresh(ctx, repository, now)
	if err != nil {
		t.Fatal(err)
	}
	if enqueued {
		t.Fatal("timed-out refresh created a duplicate job")
	}
	if strings.TrimSpace(output.String()) != "" {
		t.Errorf("retryable timeout emitted terminal failure logs = %q", output.String())
	}
}

func TestProcessLogsBoundedStorageFailureWithoutRouteData(t *testing.T) {
	t.Parallel()

	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	scheduler := newScheduler(store, &phasedCollectorStub{discover: func(
		context.Context, string,
	) (githubapp.RoutedDiscoveryResult, error) {
		t.Fatal("collector called after storage failure")
		return githubapp.RoutedDiscoveryResult{}, nil
	}}, schedulerOptions{
		Now:    func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(&output, nil)),
	})
	err = scheduler.process(context.Background(), &storage.RefreshJob{
		ID: 3, Repository: "acme/widgets",
	}, now)
	if err == nil {
		t.Fatal("process() error = nil")
	}
	logs := loggedLines(&output)
	if len(logs) != 1 ||
		!containsLogField(logs[0], `category="storage"`) ||
		!containsLogField(logs[0], `phase="storage"`) ||
		!strings.Contains(logs[0], `attempt_count=0`) ||
		strings.Contains(logs[0], "profile=") ||
		strings.Contains(logs[0], "endpoint=") ||
		strings.Contains(logs[0], err.Error()) {
		t.Errorf("storage logs = %#v, error = %v", logs, err)
	}
}

func TestRunOncePreservesSnapshotWhenRateLimitRetriesExhausted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if err := store.ReplaceRepositorySnapshot(ctx, repository, []storage.PullRequest{
		storedPullRequest(repository, 1, "Last complete"),
	}, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueRefresh(ctx, repository, now); err != nil {
		t.Fatal(err)
	}
	scheduler := newScheduler(
		store,
		&phasedCollectorStub{discover: func(
			context.Context, string,
		) (githubapp.RoutedDiscoveryResult, error) {
			return githubapp.RoutedDiscoveryResult{}, &githubapp.RateLimitError{
				Reason: "secondary", Retries: 3,
			}
		}},
		schedulerOptions{Concurrency: 1},
	)
	if err := scheduler.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListPullRequests(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != 1 || got[0].Title != "Last complete" {
		t.Fatalf("rate-limit failure replaced last complete snapshot: %+v", got)
	}
}

func TestRunOnceOnlyFullyReconcilesExplicitForcedJobs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
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
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var hydrationOptions []githubapp.HydrationOptions
	var progressOptions []githubapp.ProgressOptions
	currentTime := start
	collector := &phasedCollectorStub{
		discovery: githubapp.DiscoveryResult{
			Repository: repository,
			PullRequests: []githubapp.DiscoveredPullRequest{{
				Number: 7, CollectionIDs: []string{"acme"},
			}},
		},
		now: start,
		hydrate: func(
			_ context.Context, _ string, _ []githubapp.DiscoveredPullRequest,
			current githubapp.HydrationOptions,
		) (githubapp.RoutedHydrationResult, error) {
			hydrationOptions = append(hydrationOptions, current)
			pr := githubapp.PullRequest{
				Repository: repository, Number: 7, Title: "Reusable", Author: "octocat",
				CreatedAt: start.Add(-time.Hour), UpdatedAt: start,
				URL: "https://github.com/acme/widgets/pull/7", CollectionIDs: []string{"acme"},
			}
			fingerprint := githubapp.InputFingerprint{
				Version: githubapp.InputFingerprintVersion, Value: "stable",
			}
			cacheHits, cacheMisses, cacheBypasses := 0, 0, 1
			if !current.ForceFull {
				cached, ok := current.Cached[7]
				if !ok {
					t.Fatal("scheduled refresh did not receive the persisted cache entry")
				}
				pr, fingerprint = cached.PullRequest, cached.Fingerprint
				cacheHits, cacheBypasses = 1, 0
			}
			return githubapp.RoutedHydrationResult{Hydration: githubapp.HydrationResult{
				Repository: repository, PullRequests: []githubapp.PullRequest{pr},
				Fingerprints: map[int]githubapp.InputFingerprint{7: fingerprint},
				CacheHits:    cacheHits, CacheMisses: cacheMisses, CacheBypasses: cacheBypasses,
				ForceFull: current.ForceFull, CollectedAt: currentTime,
			}}, nil
		},
		progress: func(
			_ context.Context, _ string, current githubapp.ProgressOptions,
		) (githubapp.RoutedProgressResult, error) {
			progressOptions = append(progressOptions, current)
			return githubapp.RoutedProgressResult{Progress: githubapp.ProgressResult{
				Repository: repository, CollectedAt: currentTime,
			}}, nil
		},
	}
	scheduler := newScheduler(store, collector, schedulerOptions{
		Concurrency: 1, Now: func() time.Time { return currentTime },
	})
	run := func(at time.Time, forced bool) {
		t.Helper()
		currentTime = at
		if _, err := store.EnqueueRefreshWithForce(ctx, repository, at, forced); err != nil {
			t.Fatal(err)
		}
		if err := scheduler.RunOnce(ctx, at); err != nil {
			t.Fatal(err)
		}
		for range 4 {
			if err := scheduler.RunOnce(ctx, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	run(start, true)
	refreshes, err := store.ListRepositoryRefreshes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if refreshes[0].LastCacheHits != 0 || refreshes[0].LastCacheMisses != 0 ||
		refreshes[0].LastCacheBypasses != 1 {
		t.Errorf("forced refresh cache status = %+v", refreshes[0])
	}
	run(start.Add(6*24*time.Hour), false)
	run(start.Add(7*24*time.Hour), false)
	if len(hydrationOptions) != 3 || !hydrationOptions[0].ForceFull ||
		hydrationOptions[1].ForceFull || hydrationOptions[2].ForceFull {
		t.Fatalf("full reconciliation decisions = %#v", hydrationOptions)
	}
	if len(progressOptions) != 3 || !progressOptions[0].ProgressSince.IsZero() ||
		!progressOptions[1].ProgressSince.Equal(start) ||
		!progressOptions[2].ProgressSince.Equal(start.Add(6*24*time.Hour)) {
		t.Errorf("progress checkpoints = %#v", progressOptions)
	}
	refreshes, err = store.ListRepositoryRefreshes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if refreshes[0].LastFullReconciliation == nil ||
		!refreshes[0].LastFullReconciliation.Equal(start) ||
		refreshes[0].LastCacheHits != 1 || refreshes[0].LastCacheMisses != 0 ||
		refreshes[0].LastCacheBypasses != 0 {
		t.Errorf("refresh status = %+v", refreshes[0])
	}
}

func TestAdministratorRefreshForcesFullReconciliation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	var forced bool
	collector := &phasedCollectorStub{
		discovery: githubapp.DiscoveryResult{
			Repository: repository,
			PullRequests: []githubapp.DiscoveredPullRequest{{
				Number: 1, CollectionIDs: []string{"acme"},
			}},
		},
		hydrate: func(
			_ context.Context, _ string, _ []githubapp.DiscoveredPullRequest,
			options githubapp.HydrationOptions,
		) (githubapp.RoutedHydrationResult, error) {
			forced = options.ForceFull
			return githubapp.RoutedHydrationResult{Hydration: githubapp.HydrationResult{
				Repository: repository,
				PullRequests: []githubapp.PullRequest{{
					Repository: repository, Number: 1, Title: "Forced",
					Author: "octocat", CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
					URL: "https://github.com/acme/widgets/pull/1",
				}},
				Fingerprints: map[int]githubapp.InputFingerprint{1: {
					Version: githubapp.InputFingerprintVersion, Value: "one",
				}},
			}}, nil
		},
		now: now,
	}
	if _, err := store.EnqueueRefreshWithForce(ctx, repository, now, true); err != nil {
		t.Fatal(err)
	}
	scheduler := newScheduler(store, collector, schedulerOptions{
		Concurrency: 1, Now: func() time.Time { return now },
	})
	if err := scheduler.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := scheduler.RunOnce(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	if !forced {
		t.Fatal("administrator refresh did not force full reconciliation")
	}
	refreshes, err := store.ListRepositoryRefreshes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshes[0].LastForced {
		t.Errorf("refresh status = %+v, want forced reconciliation", refreshes[0])
	}
}

func TestPhasedRefreshPublishesBatchesAndDefersRemovals(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
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
	old := storage.PullRequest{
		Repository: repository, Number: 99, Title: "Last complete",
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour),
		URL: "https://github.com/acme/widgets/pull/99",
	}
	if err := store.ReplaceRepositorySnapshot(ctx, repository, []storage.PullRequest{old}, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	discovered := make([]githubapp.DiscoveredPullRequest, 0, 21)
	for number := 1; number <= 21; number++ {
		discovered = append(discovered, githubapp.DiscoveredPullRequest{
			Number: number, CollectionIDs: []string{"acme"},
		})
	}
	collector := &phasedCollectorStub{
		discovery: githubapp.DiscoveryResult{
			Repository: repository, PullRequests: discovered,
		},
		failHydrationOnce: true,
		now:               now,
	}
	scheduler := newScheduler(store, collector, schedulerOptions{
		Concurrency: 1, Now: func() time.Time { return now },
	})
	if _, err := store.EnqueueRefresh(ctx, repository, now); err != nil {
		t.Fatal(err)
	}
	run := func(at time.Time) {
		t.Helper()
		now = at
		if err := scheduler.RunOnce(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	run(now) // discovery
	run(now) // first batch fails and is durably requeued
	got, err := store.ListPullRequests(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != 99 {
		t.Fatalf("failed batch changed published snapshot: %+v", got)
	}
	resumedAt := now.Add(time.Minute)
	run(resumedAt) // retry PRs 1-20
	got, err = store.ListPullRequests(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 21 || got[0].Number != 1 || got[19].Number != 20 || got[20].Number != 99 {
		t.Fatalf("first published batch = %+v, want new PRs 1-20 plus prior PR 99", got)
	}
	if diff, err := store.ClaimPullRequestDiff(ctx, resumedAt); err != nil {
		t.Fatal(err)
	} else if diff != nil {
		t.Fatalf("diff work released before generation completion: %+v", diff)
	}
	run(resumedAt) // PR 21
	got, err = store.ListPullRequests(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 22 || got[20].Number != 21 || got[21].Number != 99 {
		t.Fatalf("second published batch = %+v, want new PRs 1-21 plus prior PR 99", got)
	}
	run(resumedAt) // hydration -> progress
	run(resumedAt) // progress -> publishing
	got, err = store.ListPullRequests(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 22 || got[21].Number != 99 {
		t.Fatalf("pre-final snapshot = %+v, want published batches without removals", got)
	}
	run(resumedAt) // final reconciliation and downstream release
	got, err = store.ListPullRequests(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 21 || got[0].Number != 1 || got[20].Number != 21 {
		t.Fatalf("published snapshot = %+v, want 21 batched PRs", got)
	}
	if collector.discoveryCalls != 1 ||
		len(collector.hydrationCalls) != 3 ||
		len(collector.hydrationCalls[0]) != refreshHydrationBatchSize ||
		len(collector.hydrationCalls[1]) != refreshHydrationBatchSize ||
		len(collector.hydrationCalls[2]) != 1 {
		t.Fatalf(
			"discovery calls = %d, hydration batches = %+v",
			collector.discoveryCalls, collector.hydrationCalls,
		)
	}
}

func TestRunOncePersistsPhasedDiscoveryFallbackAttempts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Now().UTC()
	collector := &phasedCollectorStub{
		discover: func(_ context.Context, _ string) (githubapp.RoutedDiscoveryResult, error) {
			return githubapp.RoutedDiscoveryResult{
				Discovery: githubapp.DiscoveryResult{Repository: repository},
				Attempts: githubapp.AttemptSet{
					{Operation: githubapp.OperationDiscovery, Profile: "primary", Priority: 0, ProfileType: githubapp.ProfileGitHubApp, Outcome: githubapp.RouteOutcomeAdvanced, Failure: githubapp.FailurePermissionMissing},
					{Operation: githubapp.OperationDiscovery, Profile: "fallback", Priority: 1, ProfileType: githubapp.ProfileFineGrainedPAT, Outcome: githubapp.RouteOutcomeSelected, Selected: true},
				},
			}, nil
		},
		now: now,
	}
	if _, err := store.EnqueueRefresh(ctx, repository, now); err != nil {
		t.Fatal(err)
	}
	if err := newScheduler(store, collector, schedulerOptions{Concurrency: 1}).RunOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	routes, err := store.ListRouteAttempts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || len(routes[0].Attempts) != 2 || routes[0].Attempts[1].Profile != "fallback" {
		t.Fatalf("persisted routes = %+v", routes)
	}
	collections, err := store.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collections[0].RouteWarnings) != 1 ||
		collections[0].RouteWarnings[0].Status != storage.RouteWarningFallback {
		t.Fatalf("collection = %+v", collections[0])
	}
}

type phasedCollectorStub struct {
	discovery         githubapp.DiscoveryResult
	discoveryCalls    int
	hydrationCalls    [][]int
	failHydrationOnce bool
	now               time.Time
	discover          func(context.Context, string) (githubapp.RoutedDiscoveryResult, error)
	hydrate           func(
		context.Context, string, []githubapp.DiscoveredPullRequest, githubapp.HydrationOptions,
	) (githubapp.RoutedHydrationResult, error)
	progress func(
		context.Context, string, githubapp.ProgressOptions,
	) (githubapp.RoutedProgressResult, error)
}

func (collector *phasedCollectorStub) DiscoverResult(
	ctx context.Context, repository string,
) (githubapp.RoutedDiscoveryResult, error) {
	collector.discoveryCalls++
	if collector.discover != nil {
		return collector.discover(ctx, repository)
	}
	return githubapp.RoutedDiscoveryResult{Discovery: collector.discovery}, nil
}

func (collector *phasedCollectorStub) HydratePullRequestsResult(
	ctx context.Context,
	repository string,
	discovered []githubapp.DiscoveredPullRequest,
	options githubapp.HydrationOptions,
) (githubapp.RoutedHydrationResult, error) {
	numbers := make([]int, 0, len(discovered))
	for _, item := range discovered {
		numbers = append(numbers, item.Number)
	}
	collector.hydrationCalls = append(collector.hydrationCalls, numbers)
	if collector.hydrate != nil {
		return collector.hydrate(ctx, repository, discovered, options)
	}
	if collector.failHydrationOnce {
		collector.failHydrationOnce = false
		return githubapp.RoutedHydrationResult{}, context.DeadlineExceeded
	}
	result := githubapp.HydrationResult{
		Repository: repository, CollectedAt: collector.now,
		Fingerprints: make(map[int]githubapp.InputFingerprint, len(discovered)),
	}
	for _, item := range discovered {
		result.PullRequests = append(result.PullRequests, githubapp.PullRequest{
			Repository: repository, Number: item.Number,
			Title:     fmt.Sprintf("PR %d", item.Number),
			CreatedAt: collector.now.Add(-time.Hour), UpdatedAt: collector.now,
			URL:           fmt.Sprintf("https://github.com/%s/pull/%d", repository, item.Number),
			CollectionIDs: append([]string(nil), item.CollectionIDs...),
		})
		result.Fingerprints[item.Number] = githubapp.InputFingerprint{
			Version: githubapp.InputFingerprintVersion,
			Value:   fmt.Sprintf("fingerprint-%d", item.Number),
		}
		result.CacheMisses++
	}
	return githubapp.RoutedHydrationResult{Hydration: result}, nil
}

func (collector *phasedCollectorStub) CollectProgressResult(
	ctx context.Context, repository string, options githubapp.ProgressOptions,
) (githubapp.RoutedProgressResult, error) {
	if collector.progress != nil {
		return collector.progress(ctx, repository, options)
	}
	return githubapp.RoutedProgressResult{Progress: githubapp.ProgressResult{
		Repository: collector.discovery.Repository, CollectedAt: collector.now,
	}}, nil
}

type incompleteTestError struct{ error }

func (incompleteTestError) Incomplete() bool { return true }

func loggedLines(output *bytes.Buffer) []string {
	return strings.Split(strings.TrimSpace(output.String()), "\n")
}

func containsLogField(line, field string) bool {
	if strings.Contains(line, field) {
		return true
	}
	name, quoted, ok := strings.Cut(field, "=")
	value, err := strconv.Unquote(quoted)
	return ok && err == nil && strings.Contains(line, name+"="+value)
}

func storedPullRequest(repository string, number int, title string) storage.PullRequest {
	at := time.Date(2026, 9, 8, 19, 0, 0, 0, time.UTC)
	return storage.PullRequest{
		Repository: repository, Number: number, Title: title, Author: "octocat",
		CreatedAt: at.Add(-24 * time.Hour), UpdatedAt: at,
		URL: "https://github.com/prometheus/prometheus/pull/1", ReviewState: "none",
	}
}
