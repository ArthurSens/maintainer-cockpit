// Package refresh coordinates durable, coalesced repository refresh jobs.
package refresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/githubapp"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
	"github.com/ArthurSens/maintainer-cockpit/internal/telemetry"
)

// Collector exposes the durable repository refresh phases.
type Collector interface {
	DiscoverResult(context.Context, string) (githubapp.RoutedDiscoveryResult, error)
	HydratePullRequestsResult(
		context.Context, string, []githubapp.DiscoveredPullRequest, githubapp.HydrationOptions,
	) (githubapp.RoutedHydrationResult, error)
	CollectProgressResult(
		context.Context, string, githubapp.ProgressOptions,
	) (githubapp.RoutedProgressResult, error)
}

const refreshHydrationBatchSize = 20

type schedulerOptions struct {
	Concurrency  int
	PollInterval time.Duration
	JobTimeout   time.Duration
	Now          func() time.Time
	Logger       *slog.Logger
}

// Scheduler runs durable refresh work.
type Scheduler struct {
	store        *storage.Store
	collector    Collector
	concurrency  int
	pollInterval time.Duration
	jobTimeout   time.Duration
	now          func() time.Time
	logger       *slog.Logger
}

// NewScheduler creates a durable refresh worker with bounded concurrency.
func NewScheduler(store *storage.Store, collector Collector) *Scheduler {
	return newScheduler(store, collector, schedulerOptions{})
}

func newScheduler(store *storage.Store, collector Collector, options schedulerOptions) *Scheduler {
	if options.Concurrency <= 0 {
		options.Concurrency = 2
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	if options.JobTimeout <= 0 {
		options.JobTimeout = 15 * time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Scheduler{
		store: store, collector: collector, concurrency: options.Concurrency,
		pollInterval: options.PollInterval, jobTimeout: options.JobTimeout,
		now: options.Now, logger: options.Logger,
	}
}

// Run processes durable work until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.store.RecoverRefreshJobs(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		if err := s.RunOnce(ctx, s.now().UTC()); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce processes at most the configured concurrency. Collection failures
// are persisted as health, not returned.
func (s *Scheduler) RunOnce(ctx context.Context, now time.Time) error {
	var jobs []*storage.RefreshJob
	for len(jobs) < s.concurrency {
		job, err := s.store.ClaimRefresh(ctx, now)
		if err != nil {
			return err
		}
		if job == nil {
			break
		}
		jobs = append(jobs, job)
	}
	var wait sync.WaitGroup
	errorsChannel := make(chan error, len(jobs))
	for _, job := range jobs {
		wait.Add(1)
		go func(job *storage.RefreshJob) {
			defer wait.Done()
			if err := s.process(ctx, job, now); err != nil {
				errorsChannel <- err
			}
		}(job)
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		return err
	}
	return nil
}

func (s *Scheduler) process(
	ctx context.Context, job *storage.RefreshJob, attemptedAt time.Time,
) (resultErr error) {
	parentContext := ctx
	lastSuccessful := s.lastSuccessfulRefresh(parentContext, job.Repository)
	failureLogged := false
	defer func() {
		if resultErr != nil && parentContext.Err() == nil && !failureLogged {
			s.logRefreshFailureMetadata(job, attemptedAt, lastSuccessful, nil, githubapp.FailureMetadata{
				Category: githubapp.FailureStorage, Phase: "storage",
				Detail:      "Refresh state could not be read or persisted.",
				Remediation: "Check storage availability and retry the refresh.",
			})
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, s.jobTimeout)
	defer cancel()
	ctx, finishTelemetry := telemetry.StartCollection(ctx, job.Repository)
	succeeded := false
	defer func() { finishTelemetry(succeeded) }()
	if err := s.store.RecordRefreshResult(
		parentContext, job.Repository, storage.RefreshRunning, attemptedAt, "",
	); err != nil {
		return err
	}
	err := s.processPhased(
		ctx, parentContext, job, attemptedAt, lastSuccessful,
		s.collector, &succeeded, &failureLogged,
	)
	if err == nil && !failureLogged {
		succeeded = true
	}
	return err
}

func (s *Scheduler) processPhased(
	ctx, parentContext context.Context,
	job *storage.RefreshJob,
	attemptedAt time.Time,
	lastSuccessful *time.Time,
	collector Collector,
	succeeded, failureLogged *bool,
) error {
	generation, err := s.store.LoadRefreshGeneration(parentContext, job.ID)
	if err != nil {
		return err
	}
	if generation != nil && generation.Forced != job.Forced {
		if err := s.store.DeleteRefreshGeneration(parentContext, job.ID); err != nil {
			return err
		}
		generation = nil
	}
	if generation == nil {
		result, collectErr := collector.DiscoverResult(ctx, job.Repository)
		if err := s.persistPhaseAttempts(
			parentContext, job.Repository, storage.RouteOperationDiscovery,
			result.Attempts, attemptedAt,
		); err != nil {
			return err
		}
		if collectErr != nil {
			return s.handlePhasedFailure(
				parentContext, job, attemptedAt, lastSuccessful,
				result.Attempts, collectErr, nil, failureLogged,
			)
		}
		progressSince, err := s.store.RepositoryProgressCollectedAt(
			parentContext, job.Repository,
		)
		if err != nil {
			return err
		}
		members := make(map[int][]string, len(result.Discovery.PullRequests))
		for _, pullRequest := range result.Discovery.PullRequests {
			members[pullRequest.Number] = append([]string(nil), pullRequest.CollectionIDs...)
		}
		limits := storageContextLimits(result.Discovery.Limits)
		if err := s.store.StartRefreshGeneration(
			parentContext, job, storage.RefreshGenerationDiscovery{
				Members: members, Limits: limits, DiscoveredAt: s.now().UTC(),
				ProgressSince: progressSince,
			},
		); err != nil {
			return err
		}
		return s.store.RequeueRefreshJob(parentContext, job.ID, s.now().UTC())
	}
	if generation.RediscoveryRequested {
		result, collectErr := collector.DiscoverResult(ctx, job.Repository)
		if err := s.persistPhaseAttempts(
			parentContext, job.Repository, storage.RouteOperationDiscovery,
			result.Attempts, attemptedAt,
		); err != nil {
			return err
		}
		if collectErr != nil {
			return s.handlePhasedFailure(
				parentContext, job, attemptedAt, lastSuccessful,
				result.Attempts, collectErr, generation, failureLogged,
			)
		}
		progressSince, err := s.store.RepositoryProgressCollectedAt(
			parentContext, job.Repository,
		)
		if err != nil {
			return err
		}
		members := make(map[int][]string, len(result.Discovery.PullRequests))
		for _, pullRequest := range result.Discovery.PullRequests {
			members[pullRequest.Number] = append([]string(nil), pullRequest.CollectionIDs...)
		}
		if err := s.store.ReconcileRefreshGenerationDiscovery(
			parentContext, job.ID, storage.RefreshGenerationDiscovery{
				Members: members, Limits: storageContextLimits(result.Discovery.Limits),
				DiscoveredAt: s.now().UTC(), ProgressSince: progressSince,
			},
		); err != nil {
			return err
		}
		return s.store.RequeueRefreshJob(parentContext, job.ID, s.now().UTC())
	}

	switch generation.Phase {
	case storage.RefreshGenerationHydrating:
		return s.processHydrationBatch(
			ctx, parentContext, job, attemptedAt, lastSuccessful,
			collector, generation, failureLogged,
		)
	case storage.RefreshGenerationProgress:
		result, collectErr := collector.CollectProgressResult(
			ctx, job.Repository,
			githubapp.ProgressOptions{ProgressSince: generation.ProgressSince},
		)
		if err := s.persistPhaseAttempts(
			parentContext, job.Repository, storage.RouteOperationProgress,
			result.Attempts, attemptedAt,
		); err != nil {
			return err
		}
		if collectErr != nil {
			return s.handlePhasedFailure(
				parentContext, job, attemptedAt, lastSuccessful,
				result.Attempts, collectErr, generation, failureLogged,
			)
		}
		payload, err := json.Marshal(result.Progress)
		if err != nil {
			return fmt.Errorf("encode staged progress for %q: %w", job.Repository, err)
		}
		if err := s.store.AdvanceRefreshGeneration(
			parentContext, job.ID, storage.RefreshGenerationProgress,
			storage.RefreshGenerationPublishing, payload,
		); err != nil {
			return err
		}
		return s.store.RequeueRefreshJob(parentContext, job.ID, s.now().UTC())
	case storage.RefreshGenerationPublishing:
		items, err := s.store.LoadStagedRefreshPullRequests(parentContext, job.ID)
		if err != nil {
			return err
		}
		if len(items) != generation.Completed || generation.Pending != 0 {
			return fmt.Errorf("refresh generation for %q is not complete", job.Repository)
		}
		snapshot := githubapp.Snapshot{
			Repository: job.Repository, CollectedAt: s.now().UTC(),
			Limits:       githubContextLimits(generation.Limits),
			Fingerprints: make(map[int]githubapp.InputFingerprint, len(items)),
			CacheHits:    generation.CacheHits, CacheMisses: generation.CacheMisses,
			CacheBypasses: generation.CacheBypasses, FullReconciliation: job.Forced,
		}
		for _, item := range items {
			var pullRequest githubapp.PullRequest
			if err := json.Unmarshal(item.Payload, &pullRequest); err != nil {
				return fmt.Errorf("decode staged PR %s#%d: %w", job.Repository, item.Number, err)
			}
			pullRequest.CollectionIDs = append([]string(nil), item.CollectionIDs...)
			snapshot.PullRequests = append(snapshot.PullRequests, pullRequest)
			snapshot.Fingerprints[item.Number] = githubapp.InputFingerprint{
				Version: item.FingerprintVersion, Value: item.Fingerprint,
			}
		}
		if len(generation.ProgressPayload) > 0 {
			var progress githubapp.ProgressResult
			if err := json.Unmarshal(generation.ProgressPayload, &progress); err != nil {
				return fmt.Errorf("decode staged progress for %q: %w", job.Repository, err)
			}
			snapshot.ProgressEvents = progress.ProgressEvents
			snapshot.ReviewThreads = progress.ReviewThreads
			if !progress.CollectedAt.IsZero() {
				snapshot.CollectedAt = progress.CollectedAt
			}
		}
		published, err := s.publishSnapshot(
			ctx, job, snapshot, job.Forced,
		)
		if err != nil {
			return err
		}
		*succeeded = published
		if err := s.store.CompleteRefreshJob(parentContext, job.ID, s.now().UTC()); err != nil {
			return fmt.Errorf("complete refresh for %q: %w", job.Repository, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported refresh generation phase %q", generation.Phase)
	}
}

func (s *Scheduler) publishSnapshot(
	ctx context.Context,
	job *storage.RefreshJob,
	snapshot githubapp.Snapshot,
	fullReconciliation bool,
) (bool, error) {
	completedAt := snapshot.CollectedAt
	if completedAt.IsZero() {
		completedAt = s.now().UTC()
	}
	pullRequests := make([]storage.PullRequest, 0, len(snapshot.PullRequests))
	authors := make(map[string]*storage.AuthorHistory, len(snapshot.Authors))
	for _, author := range snapshot.Authors {
		authors[author.Login] = &storage.AuthorHistory{
			ID: author.ID, Login: author.Login, Complete: author.Complete,
			AccountCreatedAt: author.AccountCreatedAt, CollectedAt: author.CollectedAt,
			Repositories: author.Repositories, RecentActivity: author.RecentActivity,
		}
	}
	for _, pr := range snapshot.PullRequests {
		var pullRequestContext *storage.PullRequestContext
		if pr.Context.Completeness != "" || len(pr.Context.Limits) > 0 ||
			len(pr.Context.Relationships) > 0 || len(pr.Context.ExternalURLs) > 0 {
			pullRequestContext = &storage.PullRequestContext{
				Completeness: pr.Context.Completeness, CollectedAt: pr.Context.CollectedAt,
			}
		}
		for _, limit := range pr.Context.Limits {
			pullRequestContext.Limits = append(pullRequestContext.Limits, storage.ContextLimit{
				Scope: limit.Scope, Reason: limit.Reason,
				Collected: limit.Collected, Total: limit.Total,
			})
		}
		for _, relationship := range pr.Context.Relationships {
			pullRequestContext.Relationships = append(
				pullRequestContext.Relationships, storage.Relationship{
					Kind: relationship.Kind, SourceID: relationship.SourceID,
					SourceRepository: relationship.SourceRepository,
					SourceNumber:     relationship.SourceNumber, Title: relationship.Title,
					Text: relationship.Text, State: relationship.State, URL: relationship.URL,
					Timestamp: relationship.Timestamp,
				},
			)
		}
		for _, external := range pr.Context.ExternalURLs {
			pullRequestContext.ExternalURLs = append(
				pullRequestContext.ExternalURLs, storage.ExternalURL{
					URL: external.URL, SourceKind: external.SourceKind,
					SourceID: external.SourceID, DiscoveredAt: external.DiscoveredAt,
				},
			)
		}
		files := make([]storage.PullRequestFile, 0, len(pr.Files))
		for _, file := range pr.Files {
			files = append(files, storage.PullRequestFile{
				Path: file.Path, Additions: file.Additions, Deletions: file.Deletions,
			})
		}
		checkRuns := make([]storage.CheckRun, 0, len(pr.Readiness.Checks.Runs))
		for _, run := range pr.Readiness.Checks.Runs {
			checkRuns = append(checkRuns, storage.CheckRun{
				ID: run.ID, Name: run.Name, Status: run.Status,
				Conclusion: run.Conclusion, URL: run.URL,
			})
		}
		cachePayload, err := json.Marshal(pr)
		if err != nil {
			return false, fmt.Errorf("encode cache entry for %s#%d: %w", pr.Repository, pr.Number, err)
		}
		fingerprint := snapshot.Fingerprints[pr.Number]
		pullRequests = append(pullRequests, storage.PullRequest{
			Repository: pr.Repository, Number: pr.Number, HeadSHA: pr.HeadSHA,
			Title: pr.Title, Body: pr.Body, Author: pr.Author, AuthorID: pr.AuthorID,
			AuthorHistory: authors[pr.Author], Additions: pr.Additions,
			Deletions: pr.Deletions, ChangedFiles: pr.ChangedFiles, CreatedAt: pr.CreatedAt,
			UpdatedAt: pr.UpdatedAt, URL: pr.URL, ReviewState: pr.ReviewState,
			Readiness: storage.PullRequestReadiness{
				RequestedReviewers: append([]string(nil), pr.Readiness.RequestedReviewers...),
				RequestedTeams:     append([]string(nil), pr.Readiness.RequestedTeams...),
				Mergeable:          pr.Readiness.Mergeable, MergeableState: pr.Readiness.MergeableState,
				Checks: storage.CheckSummary{
					Total: pr.Readiness.Checks.Total, Runs: checkRuns,
				},
				CollectedAt: pr.Readiness.CollectedAt,
			},
			CollectionIDs: pr.CollectionIDs, Context: pullRequestContext, Files: files,
			InputFingerprintVersion: fingerprint.Version,
			InputFingerprint:        fingerprint.Value, CachePayload: cachePayload,
		})
	}
	limits := storageContextLimits(snapshot.Limits)
	replaceErr := s.store.ReplaceRepositorySnapshotWithMetadata(
		ctx, job.Repository, pullRequests, completedAt, limits,
		storage.RepositorySnapshotMetadata{
			FullReconciliation: fullReconciliation, Forced: job.Forced,
			CacheHits: snapshot.CacheHits, CacheMisses: snapshot.CacheMisses,
			CacheBypasses: snapshot.CacheBypasses,
		},
	)
	if replaceErr != nil {
		if errors.Is(replaceErr, storage.ErrRepositoryNotConfigured) {
			return false, nil
		}
		return false, replaceErr
	}
	telemetry.RecordCollectionCache(
		job.Repository, snapshot.CacheHits, snapshot.CacheMisses, snapshot.CacheBypasses,
	)
	telemetry.RecordForcedReconciliation(job.Repository, job.Forced)
	progressEvents := make([]storage.ProgressEvent, 0, len(snapshot.ProgressEvents))
	for _, event := range snapshot.ProgressEvents {
		progressEvents = append(progressEvents, storage.ProgressEvent{
			ID: event.ID, ActorID: event.ActorID, ActivityType: event.ActivityType,
			Repository: event.Repository, Number: event.Number, Title: event.Title,
			PullRequestURL: event.PullRequestURL, ActivityURL: event.ActivityURL,
			OccurredAt: event.OccurredAt, CollectionIDs: event.CollectionIDs,
		})
	}
	reviewThreads := make([]storage.ReviewThreadState, 0, len(snapshot.ReviewThreads))
	for _, thread := range snapshot.ReviewThreads {
		reviewThreads = append(reviewThreads, storage.ReviewThreadState{
			ID: thread.ID, Resolved: thread.Resolved, ResolvedBy: thread.ResolvedBy,
			Repository: thread.Repository, Number: thread.Number, Title: thread.Title,
			PullRequestURL: thread.PullRequestURL, ActivityURL: thread.ActivityURL,
			CollectionIDs: thread.CollectionIDs,
		})
	}
	if err := s.store.RecordProgressSnapshot(
		ctx, job.Repository, progressEvents, reviewThreads, completedAt,
	); err != nil && !errors.Is(err, storage.ErrRepositoryNotConfigured) {
		return false, err
	}
	return true, nil
}

func (s *Scheduler) processHydrationBatch(
	ctx, parentContext context.Context,
	job *storage.RefreshJob,
	attemptedAt time.Time,
	lastSuccessful *time.Time,
	collector Collector,
	generation *storage.RefreshGeneration,
	failureLogged *bool,
) error {
	batch, err := s.store.LoadRefreshGenerationBatch(
		parentContext, job.ID, refreshHydrationBatchSize,
	)
	if err != nil {
		return err
	}
	if len(batch) == 0 {
		if err := s.store.AdvanceRefreshGeneration(
			parentContext, job.ID, storage.RefreshGenerationHydrating,
			storage.RefreshGenerationProgress, nil,
		); err != nil {
			return err
		}
		return s.store.RequeueRefreshJob(parentContext, job.ID, s.now().UTC())
	}
	cached := make(map[int]githubapp.CachedPullRequest)
	if !job.Forced {
		cached, err = s.cachedPullRequests(parentContext, job.Repository)
		if err != nil {
			return err
		}
	}
	discovered := make([]githubapp.DiscoveredPullRequest, 0, len(batch))
	for _, item := range batch {
		discovered = append(discovered, githubapp.DiscoveredPullRequest{
			Number: item.Number, CollectionIDs: append([]string(nil), item.CollectionIDs...),
			ContextLimits: githubContextLimits(generation.Limits),
		})
	}
	result, collectErr := collector.HydratePullRequestsResult(
		ctx, job.Repository, discovered,
		githubapp.HydrationOptions{Cached: cached, ForceFull: job.Forced},
	)
	if err := s.persistPhaseAttempts(
		parentContext, job.Repository, storage.RouteOperationHydration,
		result.Attempts, attemptedAt,
	); err != nil {
		return err
	}
	if collectErr != nil {
		return s.handlePhasedFailure(
			parentContext, job, attemptedAt, lastSuccessful,
			result.Attempts, collectErr, generation, failureLogged,
		)
	}
	staged := make([]storage.StagedRefreshPullRequest, 0, len(result.Hydration.PullRequests))
	published := make([]storage.PullRequest, 0, len(result.Hydration.PullRequests))
	memberships := make(map[int][]string, len(batch))
	for _, item := range batch {
		memberships[item.Number] = item.CollectionIDs
	}
	for _, pullRequest := range result.Hydration.PullRequests {
		payload, err := json.Marshal(pullRequest)
		if err != nil {
			return fmt.Errorf("encode staged PR %s#%d: %w", job.Repository, pullRequest.Number, err)
		}
		fingerprint := result.Hydration.Fingerprints[pullRequest.Number]
		outcome := storage.RefreshCacheMiss
		if job.Forced {
			outcome = storage.RefreshCacheBypass
		} else if previous, ok := cached[pullRequest.Number]; ok && previous.Fingerprint == fingerprint {
			outcome = storage.RefreshCacheHit
		}
		staged = append(staged, storage.StagedRefreshPullRequest{
			Number: pullRequest.Number, CollectionIDs: memberships[pullRequest.Number],
			Payload:            payload,
			FingerprintVersion: fingerprint.Version, Fingerprint: fingerprint.Value,
			CacheOutcome: outcome,
		})
		pullRequest.CollectionIDs = append([]string(nil), memberships[pullRequest.Number]...)
		published = append(published, storagePullRequest(pullRequest, fingerprint, payload))
	}
	if len(staged) != len(batch) {
		return fmt.Errorf(
			"hydration returned %d PRs for a %d-PR batch", len(staged), len(batch),
		)
	}
	if err := s.store.PublishRefreshGenerationBatch(
		parentContext, job.ID, staged, published, s.now().UTC(),
	); err != nil {
		return err
	}
	telemetry.RecordRefreshBatch(job.Repository, len(published))
	return s.store.RequeueRefreshJob(parentContext, job.ID, s.now().UTC())
}

func (s *Scheduler) handlePhasedFailure(
	ctx context.Context,
	job *storage.RefreshJob,
	attemptedAt time.Time,
	lastSuccessful *time.Time,
	attempts githubapp.AttemptSet,
	collectErr error,
	generation *storage.RefreshGeneration,
	failureLogged *bool,
) error {
	if ctx.Err() != nil {
		return nil
	}
	metadata := githubapp.BoundedFailureMetadata(collectErr)
	retryable := metadata.Category == githubapp.FailurePrimaryRateLimited ||
		metadata.Category == githubapp.FailureSecondaryRateLimited ||
		metadata.Category == githubapp.FailureGitHubUnavailable ||
		metadata.Category == githubapp.FailureOperationTimeout
	if retryable && generation == nil {
		return s.store.RequeueRefreshJob(ctx, job.ID, s.now().UTC().Add(time.Minute))
	}
	if retryable && generation != nil &&
		s.now().UTC().Sub(generation.DiscoveredAt) < 24*time.Hour {
		if generation.Phase == storage.RefreshGenerationHydrating {
			telemetry.RecordRefreshBatchRetry(job.Repository)
		}
		retries, err := s.store.RecordRefreshGenerationRetry(
			ctx, job.ID, string(metadata.Category), metadata.Detail,
			metadata.Remediation, metadata.Phase,
		)
		if err != nil {
			return err
		}
		delay := time.Minute * time.Duration(1<<min(retries-1, 4))
		return s.store.RequeueRefreshJob(ctx, job.ID, s.now().UTC().Add(delay))
	}
	state := storage.RefreshFailed
	message := "GitHub refresh failed."
	if isIncomplete(collectErr) {
		state = storage.RefreshIncomplete
		message = "GitHub refresh was incomplete."
	}
	if err := s.store.RecordRefreshResult(ctx, job.Repository, state, attemptedAt, message); err != nil {
		return err
	}
	s.logRefreshFailure(job, attemptedAt, lastSuccessful, attempts, collectErr)
	*failureLogged = true
	if err := s.store.DeleteRefreshGeneration(ctx, job.ID); err != nil {
		return err
	}
	return s.store.CompleteRefreshJob(ctx, job.ID, s.now().UTC())
}

func (s *Scheduler) persistPhaseAttempts(
	ctx context.Context,
	repository, operation string,
	attempts githubapp.AttemptSet,
	attemptedAt time.Time,
) error {
	if err := s.store.ReplaceRouteAttempts(
		ctx, repository, operation, storageRouteAttempts(attempts), attemptedAt,
	); err != nil {
		if errors.Is(err, storage.ErrRepositoryNotConfigured) {
			return nil
		}
		return err
	}
	recordRouteAttempts(ctx, operation, attempts)
	return nil
}

func storageContextLimits(limits []githubapp.ContextLimit) []storage.ContextLimit {
	result := make([]storage.ContextLimit, 0, len(limits))
	for _, limit := range limits {
		result = append(result, storage.ContextLimit{
			Scope: limit.Scope, Reason: limit.Reason,
			Collected: limit.Collected, Total: limit.Total,
		})
	}
	return result
}

func githubContextLimits(limits []storage.ContextLimit) []githubapp.ContextLimit {
	result := make([]githubapp.ContextLimit, 0, len(limits))
	for _, limit := range limits {
		result = append(result, githubapp.ContextLimit{
			Scope: limit.Scope, Reason: limit.Reason,
			Collected: limit.Collected, Total: limit.Total,
		})
	}
	return result
}

func (s *Scheduler) cachedPullRequests(
	ctx context.Context,
	repository string,
) (map[int]githubapp.CachedPullRequest, error) {
	persisted, err := s.store.LoadCachedPullRequests(ctx, repository)
	if err != nil {
		return nil, err
	}
	result := make(map[int]githubapp.CachedPullRequest, len(persisted))
	for number, item := range persisted {
		var pr githubapp.PullRequest
		if err := json.Unmarshal(item.Payload, &pr); err != nil {
			continue
		}
		result[number] = githubapp.CachedPullRequest{
			PullRequest: pr,
			Fingerprint: githubapp.InputFingerprint{
				Version: item.FingerprintVersion, Value: item.Fingerprint,
			},
		}
	}
	return result, nil
}

func storagePullRequest(
	pr githubapp.PullRequest,
	fingerprint githubapp.InputFingerprint,
	cachePayload []byte,
) storage.PullRequest {
	var pullRequestContext *storage.PullRequestContext
	if pr.Context.Completeness != "" || len(pr.Context.Limits) > 0 ||
		len(pr.Context.Relationships) > 0 || len(pr.Context.ExternalURLs) > 0 {
		pullRequestContext = &storage.PullRequestContext{
			Completeness: pr.Context.Completeness,
			CollectedAt:  pr.Context.CollectedAt,
		}
		for _, limit := range pr.Context.Limits {
			pullRequestContext.Limits = append(pullRequestContext.Limits, storage.ContextLimit{
				Scope: limit.Scope, Reason: limit.Reason,
				Collected: limit.Collected, Total: limit.Total,
			})
		}
		for _, relationship := range pr.Context.Relationships {
			pullRequestContext.Relationships = append(
				pullRequestContext.Relationships, storage.Relationship{
					Kind: relationship.Kind, SourceID: relationship.SourceID,
					SourceRepository: relationship.SourceRepository,
					SourceNumber:     relationship.SourceNumber, Title: relationship.Title,
					Text: relationship.Text, State: relationship.State, URL: relationship.URL,
					Timestamp: relationship.Timestamp,
				},
			)
		}
		for _, external := range pr.Context.ExternalURLs {
			pullRequestContext.ExternalURLs = append(
				pullRequestContext.ExternalURLs, storage.ExternalURL{
					URL: external.URL, SourceKind: external.SourceKind,
					SourceID: external.SourceID, DiscoveredAt: external.DiscoveredAt,
				},
			)
		}
	}
	files := make([]storage.PullRequestFile, 0, len(pr.Files))
	for _, file := range pr.Files {
		files = append(files, storage.PullRequestFile{
			Path: file.Path, Additions: file.Additions, Deletions: file.Deletions,
		})
	}
	checkRuns := make([]storage.CheckRun, 0, len(pr.Readiness.Checks.Runs))
	for _, run := range pr.Readiness.Checks.Runs {
		checkRuns = append(checkRuns, storage.CheckRun{
			ID: run.ID, Name: run.Name, Status: run.Status,
			Conclusion: run.Conclusion, URL: run.URL,
		})
	}
	return storage.PullRequest{
		Repository: pr.Repository, Number: pr.Number, HeadSHA: pr.HeadSHA,
		Title: pr.Title, Body: pr.Body, Author: pr.Author, AuthorID: pr.AuthorID,
		Additions: pr.Additions, Deletions: pr.Deletions, ChangedFiles: pr.ChangedFiles,
		CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt, URL: pr.URL,
		ReviewState: pr.ReviewState,
		Readiness: storage.PullRequestReadiness{
			RequestedReviewers: append([]string(nil), pr.Readiness.RequestedReviewers...),
			RequestedTeams:     append([]string(nil), pr.Readiness.RequestedTeams...),
			Mergeable:          pr.Readiness.Mergeable, MergeableState: pr.Readiness.MergeableState,
			Checks:      storage.CheckSummary{Total: pr.Readiness.Checks.Total, Runs: checkRuns},
			CollectedAt: pr.Readiness.CollectedAt,
		},
		CollectionIDs: append([]string(nil), pr.CollectionIDs...),
		Context:       pullRequestContext, Files: files,
		InputFingerprintVersion: fingerprint.Version,
		InputFingerprint:        fingerprint.Value,
		CachePayload:            append([]byte(nil), cachePayload...),
	}
}

func storageRouteAttempts(attempts githubapp.AttemptSet) []storage.RouteAttempt {
	result := make([]storage.RouteAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		result = append(result, storage.RouteAttempt{
			Profile: attempt.Profile, Priority: attempt.Priority,
			ProfileType: string(attempt.ProfileType), Outcome: string(attempt.Outcome),
			FailureCategory: string(attempt.Failure), Detail: attempt.Detail,
			Remediation: attempt.Remediation, Selected: attempt.Selected,
		})
	}
	return result
}

func (s *Scheduler) lastSuccessfulRefresh(ctx context.Context, repository string) *time.Time {
	refreshes, err := s.store.ListRepositoryRefreshes(ctx)
	if err != nil {
		return nil
	}
	for _, refresh := range refreshes {
		if refresh.Repository == repository {
			return refresh.LastSuccessful
		}
	}
	return nil
}

func (s *Scheduler) logRefreshFailure(
	job *storage.RefreshJob,
	attemptedAt time.Time,
	lastSuccessful *time.Time,
	attempts githubapp.AttemptSet,
	err error,
) {
	s.logRefreshFailureMetadata(
		job, attemptedAt, lastSuccessful, attempts, githubapp.BoundedFailureMetadata(err),
	)
}

func (s *Scheduler) logRefreshFailureMetadata(
	job *storage.RefreshJob,
	attemptedAt time.Time,
	lastSuccessful *time.Time,
	attempts githubapp.AttemptSet,
	metadata githubapp.FailureMetadata,
) {
	duration := max(s.now().UTC().Sub(attemptedAt), 0)
	fields := []slog.Attr{
		slog.String("event", "repository_refresh_failed"),
		slog.String("repository", job.Repository),
		slog.String("operation", string(githubapp.OperationDiscovery)),
		slog.Int64("job_id", job.ID),
		slog.Int64("duration_ms", duration.Milliseconds()),
	}
	if lastSuccessful != nil {
		fields = append(fields,
			slog.String("last_successful_refresh", lastSuccessful.UTC().Format(time.RFC3339Nano)),
		)
	}
	fallbackAttempted := false
	fallbackSelected := false
	if len(attempts) > 0 {
		final := attempts[len(attempts)-1]
		fallbackAttempted = len(attempts) > 1 || final.Priority > 0
		for _, attempt := range attempts {
			if attempt.Selected && attempt.Priority > 0 {
				fallbackSelected = true
				break
			}
		}
		fields = append(fields,
			slog.String("profile", final.Profile),
			slog.String("profile_type", string(final.ProfileType)),
			slog.Int("priority", final.Priority),
		)
		if final.Operation != "" {
			fields[2] = slog.String("operation", string(final.Operation))
		}
		if final.Failure != "" {
			metadata.Category = final.Failure
		}
		if final.Detail != "" {
			metadata.Detail = final.Detail
		}
		if final.Remediation != "" {
			metadata.Remediation = final.Remediation
		}
	}
	fields = append(fields,
		slog.Int("attempt_count", len(attempts)),
		slog.Bool("fallback_attempted", fallbackAttempted),
		slog.Bool("fallback_selected", fallbackSelected),
		slog.String("category", string(metadata.Category)),
		slog.String("detail", metadata.Detail),
		slog.String("remediation", metadata.Remediation),
		slog.String("phase", metadata.Phase),
	)
	if metadata.HTTPStatus != 0 {
		fields = append(fields, slog.Int("http_status", metadata.HTTPStatus))
	}
	if metadata.Method != "" {
		fields = append(fields, slog.String("method", metadata.Method))
	}
	if metadata.Endpoint != "" {
		fields = append(fields, slog.String("endpoint", metadata.Endpoint))
	}
	if metadata.RequestID != "" {
		fields = append(fields, slog.String("request_id", metadata.RequestID))
	}
	if metadata.TimedOut {
		fields = append(fields, slog.Bool("timed_out", true))
	}
	if metadata.RetryAfter > 0 {
		fields = append(fields, slog.Int64("retry_after_ms", metadata.RetryAfter.Milliseconds()))
	}
	if metadata.RateLimitResetUnix > 0 {
		fields = append(fields, slog.Int64("rate_limit_reset_unix", metadata.RateLimitResetUnix))
	}
	s.logger.LogAttrs(context.Background(), slog.LevelError, "repository refresh failed", fields...)
}

func recordRouteAttempts(ctx context.Context, operation string, attempts githubapp.AttemptSet) {
	for _, attempt := range attempts {
		telemetry.RecordGitHubRouteAttempt(
			ctx, operation, string(attempt.ProfileType), string(attempt.Outcome),
			string(attempt.Failure),
		)
	}
}

func isIncomplete(err error) bool {
	type incomplete interface {
		Incomplete() bool
	}
	if value, ok := err.(incomplete); ok && value.Incomplete() {
		return true
	}
	return githubapp.IsIncomplete(err)
}
