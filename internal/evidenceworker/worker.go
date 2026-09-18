// Package evidenceworker processes durable contribution evidence independently
// from core repository refreshes.
package evidenceworker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/githubapp"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
	"github.com/ArthurSens/maintainer-cockpit/internal/telemetry"
)

// RepositoryCollector routes repository-scoped contribution facts.
type RepositoryCollector interface {
	CollectRepositoryContribution(
		context.Context, int64, string, string, string,
	) (githubapp.ContributionResult, error)
}

// AncillaryCollector collects bounded unauthenticated author facts.
type AncillaryCollector interface {
	CollectAncillaryAuthorEvidence(
		context.Context, int64, string,
	) (githubapp.AncillaryAuthorEvidence, error)
}

// DiffCollector routes authenticated current-head patch collection.
type DiffCollector interface {
	CollectPullRequestDiff(
		context.Context, string, int, string, int,
	) (githubapp.DiffResult, error)
}

// Options controls bounded evidence execution.
type Options struct {
	Concurrency  int
	PollInterval time.Duration
	JobTimeout   time.Duration
	Now          func() time.Time
	Diff         DiffCollector
}

// Worker processes both durable evidence queues.
type Worker struct {
	store       *storage.Store
	repository  RepositoryCollector
	ancillary   AncillaryCollector
	diff        DiffCollector
	concurrency int
	poll        time.Duration
	timeout     time.Duration
	now         func() time.Time
	queueMu     sync.Mutex
	queueNext   int
}

// New creates an evidence worker.
func New(
	store *storage.Store, repository RepositoryCollector,
	ancillary AncillaryCollector, options Options,
) *Worker {
	if options.Concurrency <= 0 {
		options.Concurrency = 2
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	if options.JobTimeout <= 0 {
		options.JobTimeout = 5 * time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Worker{
		store: store, repository: repository, ancillary: ancillary,
		diff:        options.Diff,
		concurrency: options.Concurrency, poll: options.PollInterval,
		timeout: options.JobTimeout, now: options.Now,
	}
}

// Run recovers interrupted claims and processes work until cancellation.
func (w *Worker) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	if err := w.store.RecoverEvidenceJobs(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := w.RunOnce(ctx, w.now().UTC()); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce processes at most the configured concurrency across both queues.
func (w *Worker) RunOnce(ctx context.Context, now time.Time) error {
	type work func() error
	items := make([]work, 0, w.concurrency)
	for len(items) < w.concurrency {
		item, err := w.claimFair(ctx, now)
		if err != nil {
			return err
		}
		if item == nil {
			break
		}
		items = append(items, item)
	}
	var wait sync.WaitGroup
	errs := make(chan error, len(items))
	for _, item := range items {
		wait.Add(1)
		go func(item work) {
			defer wait.Done()
			if err := item(); err != nil {
				errs <- err
			}
		}(item)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

func (w *Worker) claimFair(ctx context.Context, now time.Time) (func() error, error) {
	w.queueMu.Lock()
	defer w.queueMu.Unlock()

	claimAncillary := func() (func() error, error) {
		job, err := w.store.ClaimAncillaryAuthorEvidence(ctx, now)
		if err != nil || job == nil {
			return nil, err
		}
		return func() error { return w.processAncillary(ctx, job) }, nil
	}
	claimRepository := func() (func() error, error) {
		job, err := w.store.ClaimContributionEvidence(ctx, now)
		if err != nil || job == nil {
			return nil, err
		}
		return func() error { return w.processRepository(ctx, job) }, nil
	}
	claimDiff := func() (func() error, error) {
		if w.diff == nil {
			return nil, nil
		}
		job, err := w.store.ClaimPullRequestDiff(ctx, now)
		if err != nil || job == nil {
			return nil, err
		}
		return func() error { return w.processDiff(ctx, job) }, nil
	}
	claims := []func() (func() error, error){claimRepository, claimAncillary, claimDiff}
	for offset := range claims {
		index := (w.queueNext + offset) % len(claims)
		item, err := claims[index]()
		if err != nil {
			return nil, err
		}
		if item != nil {
			w.queueNext = (index + 1) % len(claims)
			return item, nil
		}
	}
	return nil, nil
}

func (w *Worker) processRepository(
	parent context.Context, job *storage.ContributionEvidenceJob,
) error {
	ctx, cancel := context.WithTimeout(parent, w.timeout)
	defer cancel()
	result, collectErr := w.repository.CollectRepositoryContribution(
		ctx, job.AuthorID, job.Login, job.Repository, job.Association,
	)
	at := w.now().UTC()
	if collectErr == nil || (!errors.Is(collectErr, context.Canceled) &&
		!errors.Is(collectErr, context.DeadlineExceeded)) {
		err := w.store.ReplaceRouteAttempts(
			parent, job.Repository, storage.RouteOperationContribution,
			storageRouteAttempts(result.Attempts), at,
		)
		if err != nil {
			if !errors.Is(err, storage.ErrRepositoryNotConfigured) {
				return fmt.Errorf("persist repository evidence route attempts: %w", err)
			}
		} else {
			recordRouteAttempts(parent, storage.RouteOperationContribution, result.Attempts)
		}
	}
	if parent.Err() != nil {
		return nil
	}
	history := storage.RepositoryContribution{}
	if collectErr == nil {
		history = result.Evidence.History
	}
	if err := w.store.CompleteContributionEvidence(parent, job, history, at, collectErr); err != nil {
		return fmt.Errorf("complete repository evidence job: %w", err)
	}
	return nil
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

func recordRouteAttempts(ctx context.Context, operation string, attempts githubapp.AttemptSet) {
	for _, attempt := range attempts {
		telemetry.RecordGitHubRouteAttempt(
			ctx, operation, string(attempt.ProfileType), string(attempt.Outcome),
			string(attempt.Failure),
		)
	}
}

func (w *Worker) processDiff(
	parent context.Context, job *storage.PullRequestDiffJob,
) error {
	ctx, cancel := context.WithTimeout(parent, w.timeout)
	defer cancel()
	result, collectErr := w.diff.CollectPullRequestDiff(
		ctx, job.Repository, job.Number, job.HeadSHA, job.ChangedFiles,
	)
	at := w.now().UTC()
	if githubapp.IsPullRequestHeadChanged(collectErr) {
		if parent.Err() != nil {
			return nil
		}
		if err := w.store.SupersedePullRequestDiff(parent, job, at); err != nil {
			return fmt.Errorf("supersede diff evidence job: %w", err)
		}
		return nil
	}
	if collectErr == nil || (!errors.Is(collectErr, context.Canceled) &&
		!errors.Is(collectErr, context.DeadlineExceeded)) {
		err := w.store.ReplaceRouteAttempts(
			parent, job.Repository, storage.RouteOperationDiff,
			storageRouteAttempts(result.Attempts), at,
		)
		if err != nil {
			if !errors.Is(err, storage.ErrRepositoryNotConfigured) {
				return fmt.Errorf("persist diff route attempts: %w", err)
			}
		} else {
			recordRouteAttempts(parent, storage.RouteOperationDiff, result.Attempts)
		}
	}
	if parent.Err() != nil {
		return nil
	}
	evidence := storage.PullRequestDiffEvidence{
		Completeness:  result.Evidence.Completeness,
		OriginalFiles: result.Evidence.OriginalFiles,
		SentFiles:     result.Evidence.SentFiles,
		OmittedFiles:  result.Evidence.OmittedFiles,
		OriginalBytes: result.Evidence.OriginalBytes,
		SentBytes:     result.Evidence.SentBytes,
		Truncated:     result.Evidence.Truncated,
	}
	for _, source := range result.Evidence.Sources {
		evidence.Sources = append(evidence.Sources, storage.PullRequestDiffSource{
			Path: source.Path, Patch: source.Patch,
			OriginalBytes: source.OriginalBytes, SentBytes: source.SentBytes,
			Truncated: source.Truncated,
		})
	}
	if err := w.store.CompletePullRequestDiff(
		parent, job, evidence, at, collectErr,
	); err != nil {
		return fmt.Errorf("complete diff evidence job: %w", err)
	}
	return nil
}

func (w *Worker) processAncillary(
	parent context.Context, job *storage.AncillaryAuthorJob,
) error {
	ctx, cancel := context.WithTimeout(parent, w.timeout)
	defer cancel()
	evidence, collectErr := w.ancillary.CollectAncillaryAuthorEvidence(
		ctx, job.AuthorID, job.Login,
	)
	if parent.Err() != nil {
		return nil
	}
	if err := w.store.CompleteAncillaryAuthorEvidence(
		parent, job, evidence.AccountCreatedAt, evidence.RecentActivity,
		w.now().UTC(), collectErr,
	); err != nil {
		return fmt.Errorf("complete ancillary evidence job: %w", err)
	}
	return nil
}
