package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/quality"
)

const (
	RefreshGenerationHydrating  = "hydrating"
	RefreshGenerationProgress   = "progress"
	RefreshGenerationPublishing = "publishing"

	RefreshCacheHit    = "hit"
	RefreshCacheMiss   = "miss"
	RefreshCacheBypass = "bypass"
)

// RefreshGenerationDiscovery is the durable result of open-PR discovery.
type RefreshGenerationDiscovery struct {
	Members       map[int][]string
	Limits        []ContextLimit
	DiscoveredAt  time.Time
	ProgressSince time.Time
}

// RefreshGeneration coordinates batched publication and final reconciliation.
type RefreshGeneration struct {
	ID                   int64
	RefreshJobID         int64
	Repository           string
	Phase                string
	Forced               bool
	DiscoveredAt         time.Time
	ProgressSince        time.Time
	Limits               []ContextLimit
	ProgressPayload      []byte
	RediscoveryRequested bool
	Pending              int
	Completed            int
	CacheHits            int
	CacheMisses          int
	CacheBypasses        int
	RetryCount           int
	FailureCategory      string
	FailureDetail        string
	FailureRemediation   string
	FailurePhase         string
}

// RefreshGenerationItem identifies one discovered PR and its memberships.
type RefreshGenerationItem struct {
	Number        int
	CollectionIDs []string
}

// StagedRefreshPullRequest is one opaque, completely hydrated PR payload.
type StagedRefreshPullRequest struct {
	Number             int
	CollectionIDs      []string
	Payload            []byte
	FingerprintVersion string
	Fingerprint        string
	CacheOutcome       string
}

// ActiveRefreshGeneration identifies an in-progress generation for telemetry.
type ActiveRefreshGeneration struct {
	Repository string
	CreatedAt  time.Time
}

// ActiveRefreshGenerations returns generations whose durable jobs can still run.
func (s *Store) ActiveRefreshGenerations(ctx context.Context) ([]ActiveRefreshGeneration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.repository, g.created_at
		FROM refresh_generations g
		JOIN refresh_jobs j ON j.id = g.refresh_job_id
		WHERE j.status IN ('queued', 'running')
		ORDER BY g.repository
	`)
	if err != nil {
		return nil, fmt.Errorf("load active refresh generations: %w", err)
	}
	defer rows.Close()
	var result []ActiveRefreshGeneration
	for rows.Next() {
		var generation ActiveRefreshGeneration
		var createdAt string
		if err := rows.Scan(&generation.Repository, &createdAt); err != nil {
			return nil, fmt.Errorf("scan active refresh generation: %w", err)
		}
		generation.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse refresh generation creation time: %w", err)
		}
		result = append(result, generation)
	}
	return result, rows.Err()
}

// StartRefreshGeneration replaces any staged work for a claimed refresh job.
func (s *Store) StartRefreshGeneration(
	ctx context.Context,
	job *RefreshJob,
	discovery RefreshGenerationDiscovery,
) error {
	if job == nil || job.ID <= 0 || job.Repository == "" || discovery.DiscoveredAt.IsZero() {
		return errors.New("refresh generation has missing job or discovery data")
	}
	limits, err := json.Marshal(discovery.Limits)
	if err != nil {
		return fmt.Errorf("encode refresh generation limits: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin refresh generation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var repository, status string
	var forced bool
	if err := tx.QueryRowContext(ctx, `
		SELECT repository, status, forced FROM refresh_jobs WHERE id = ?
	`, job.ID).Scan(&repository, &status, &forced); err != nil {
		return fmt.Errorf("load refresh job %d: %w", job.ID, err)
	}
	if repository != job.Repository || status != "running" {
		return fmt.Errorf("refresh job %d changed while discovery was running", job.ID)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM refresh_generations WHERE refresh_job_id = ?
	`, job.ID); err != nil {
		return fmt.Errorf("replace refresh generation: %w", err)
	}
	var progressSince any
	if !discovery.ProgressSince.IsZero() {
		progressSince = formatTime(discovery.ProgressSince)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO refresh_generations (
			refresh_job_id, repository, phase, forced, discovered_at,
			progress_since, limits_json, created_at
		) VALUES (?, ?, 'hydrating', ?, ?, ?, ?, ?)
	`, job.ID, job.Repository, forced, formatTime(discovery.DiscoveredAt),
		progressSince, string(limits), formatTime(discovery.DiscoveredAt))
	if err != nil {
		return fmt.Errorf("insert refresh generation: %w", err)
	}
	generationID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("inspect refresh generation: %w", err)
	}
	numbers := make([]int, 0, len(discovery.Members))
	for number := range discovery.Members {
		if number <= 0 {
			return errors.New("refresh generation contains invalid pull request number")
		}
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	for _, number := range numbers {
		collectionIDs, marshalErr := json.Marshal(discovery.Members[number])
		if marshalErr != nil {
			return fmt.Errorf("encode memberships for PR %d: %w", number, marshalErr)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO refresh_generation_pull_requests (
				generation_id, number, collection_ids_json, status
			) VALUES (?, ?, ?, 'pending')
		`, generationID, number, string(collectionIDs)); err != nil {
			return fmt.Errorf("stage discovered PR %d: %w", number, err)
		}
	}
	return tx.Commit()
}

// ReconcileRefreshGenerationDiscovery updates the desired open-PR set in
// place, preserving completed hydration for PRs that remain open.
func (s *Store) ReconcileRefreshGenerationDiscovery(
	ctx context.Context,
	refreshJobID int64,
	discovery RefreshGenerationDiscovery,
) error {
	if discovery.DiscoveredAt.IsZero() {
		return errors.New("refresh rediscovery has no collection time")
	}
	limits, err := json.Marshal(discovery.Limits)
	if err != nil {
		return fmt.Errorf("encode refresh generation limits: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin refresh generation rediscovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var generationID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM refresh_generations WHERE refresh_job_id = ?
	`, refreshJobID).Scan(&generationID); err != nil {
		return fmt.Errorf("load refresh generation for rediscovery: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE IF NOT EXISTS rediscovered_pull_requests (
			number INTEGER PRIMARY KEY,
			collection_ids_json TEXT NOT NULL
		);
		DELETE FROM rediscovered_pull_requests;
	`); err != nil {
		return fmt.Errorf("prepare refresh rediscovery: %w", err)
	}
	numbers := make([]int, 0, len(discovery.Members))
	for number := range discovery.Members {
		if number <= 0 {
			return errors.New("refresh rediscovery contains invalid pull request number")
		}
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	for _, number := range numbers {
		collectionIDs, marshalErr := json.Marshal(discovery.Members[number])
		if marshalErr != nil {
			return fmt.Errorf("encode memberships for PR %d: %w", number, marshalErr)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rediscovered_pull_requests (number, collection_ids_json)
			VALUES (?, ?)
		`, number, string(collectionIDs)); err != nil {
			return fmt.Errorf("stage rediscovered PR %d: %w", number, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM refresh_generation_pull_requests
		WHERE generation_id = ?
		  AND number NOT IN (SELECT number FROM rediscovered_pull_requests)
	`, generationID); err != nil {
		return fmt.Errorf("remove closed PRs from refresh generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO refresh_generation_pull_requests (
			generation_id, number, collection_ids_json, status
		)
		SELECT ?, number, collection_ids_json, 'pending'
		FROM rediscovered_pull_requests
		WHERE true
		ON CONFLICT(generation_id, number) DO UPDATE SET
			collection_ids_json = excluded.collection_ids_json
	`, generationID); err != nil {
		return fmt.Errorf("reconcile rediscovered PRs: %w", err)
	}
	var progressSince any
	if !discovery.ProgressSince.IsZero() {
		progressSince = formatTime(discovery.ProgressSince)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE refresh_generations
		SET phase = 'hydrating', discovered_at = ?, progress_since = ?,
			limits_json = ?, progress_payload = X'', rediscovery_requested = 0,
			retry_count = 0, failure_category = '', failure_detail = '',
			failure_remediation = '', failure_phase = ''
		WHERE id = ?
	`, formatTime(discovery.DiscoveredAt), progressSince, string(limits), generationID); err != nil {
		return fmt.Errorf("complete refresh rediscovery: %w", err)
	}
	return tx.Commit()
}

// LoadRefreshGeneration returns the staged coordinator state for one job.
func (s *Store) LoadRefreshGeneration(
	ctx context.Context,
	refreshJobID int64,
) (*RefreshGeneration, error) {
	var generation RefreshGeneration
	var discoveredAt, progressSince, limits string
	err := s.db.QueryRowContext(ctx, `
		SELECT g.id, g.refresh_job_id, g.repository, g.phase, g.forced,
			g.discovered_at, COALESCE(g.progress_since, ''), g.limits_json,
			g.progress_payload, g.rediscovery_requested, g.retry_count, g.failure_category,
			g.failure_detail, g.failure_remediation, g.failure_phase,
			COUNT(CASE WHEN i.status = 'pending' THEN 1 END),
			COUNT(CASE WHEN i.status = 'completed' THEN 1 END),
			COUNT(CASE WHEN i.cache_outcome = 'hit' THEN 1 END),
			COUNT(CASE WHEN i.cache_outcome = 'miss' THEN 1 END),
			COUNT(CASE WHEN i.cache_outcome = 'bypass' THEN 1 END)
		FROM refresh_generations g
		LEFT JOIN refresh_generation_pull_requests i ON i.generation_id = g.id
		WHERE g.refresh_job_id = ?
		GROUP BY g.id
	`, refreshJobID).Scan(
		&generation.ID, &generation.RefreshJobID, &generation.Repository,
		&generation.Phase, &generation.Forced, &discoveredAt, &progressSince,
		&limits, &generation.ProgressPayload, &generation.RediscoveryRequested, &generation.RetryCount,
		&generation.FailureCategory, &generation.FailureDetail,
		&generation.FailureRemediation, &generation.FailurePhase, &generation.Pending,
		&generation.Completed, &generation.CacheHits, &generation.CacheMisses,
		&generation.CacheBypasses,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load refresh generation for job %d: %w", refreshJobID, err)
	}
	generation.DiscoveredAt, err = time.Parse(time.RFC3339Nano, discoveredAt)
	if err != nil {
		return nil, fmt.Errorf("parse refresh discovery time: %w", err)
	}
	if progressSince != "" {
		generation.ProgressSince, err = time.Parse(time.RFC3339Nano, progressSince)
		if err != nil {
			return nil, fmt.Errorf("parse refresh progress checkpoint: %w", err)
		}
	}
	if err := json.Unmarshal([]byte(limits), &generation.Limits); err != nil {
		return nil, fmt.Errorf("decode refresh generation limits: %w", err)
	}
	return &generation, nil
}

// LoadRefreshGenerationBatch returns the next pending PRs in stable order.
func (s *Store) LoadRefreshGenerationBatch(
	ctx context.Context,
	refreshJobID int64,
	limit int,
) ([]RefreshGenerationItem, error) {
	if limit <= 0 {
		return nil, errors.New("refresh generation batch size must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.number, i.collection_ids_json
		FROM refresh_generation_pull_requests i
		JOIN refresh_generations g ON g.id = i.generation_id
		WHERE g.refresh_job_id = ? AND g.phase = 'hydrating' AND i.status = 'pending'
		ORDER BY i.number
		LIMIT ?
	`, refreshJobID, limit)
	if err != nil {
		return nil, fmt.Errorf("load refresh generation batch: %w", err)
	}
	defer rows.Close()
	var result []RefreshGenerationItem
	for rows.Next() {
		var item RefreshGenerationItem
		var collectionIDs string
		if err := rows.Scan(&item.Number, &collectionIDs); err != nil {
			return nil, fmt.Errorf("scan refresh generation batch: %w", err)
		}
		if err := json.Unmarshal([]byte(collectionIDs), &item.CollectionIDs); err != nil {
			return nil, fmt.Errorf("decode memberships for PR %d: %w", item.Number, err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// PublishRefreshGenerationBatch atomically stages and publishes one hydrated
// batch. It intentionally does not remove stale PRs or release downstream work;
// those actions remain generation-finalization responsibilities.
func (s *Store) PublishRefreshGenerationBatch(
	ctx context.Context,
	refreshJobID int64,
	items []StagedRefreshPullRequest,
	pullRequests []PullRequest,
	completedAt time.Time,
) error {
	if len(items) != len(pullRequests) {
		return fmt.Errorf("stage %d items with %d published pull requests", len(items), len(pullRequests))
	}
	published := make(map[int]*PullRequest, len(pullRequests))
	for index := range pullRequests {
		if pullRequests[index].Number <= 0 {
			return errors.New("published batch contains invalid pull request number")
		}
		if err := normalizePullRequest(&pullRequests[index]); err != nil {
			return fmt.Errorf("pull request %d: %w", pullRequests[index].Number, err)
		}
		published[pullRequests[index].Number] = &pullRequests[index]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin refresh batch completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range items {
		if item.Number <= 0 || len(item.Payload) == 0 ||
			item.FingerprintVersion == "" || item.Fingerprint == "" ||
			(item.CacheOutcome != RefreshCacheHit &&
				item.CacheOutcome != RefreshCacheMiss &&
				item.CacheOutcome != RefreshCacheBypass) {
			return fmt.Errorf("staged PR %d has incomplete hydration data", item.Number)
		}
		pullRequest, ok := published[item.Number]
		if !ok {
			return fmt.Errorf("staged PR %d has no published pull request", item.Number)
		}
		var repository string
		if err := tx.QueryRowContext(ctx, `
			SELECT repository FROM refresh_generations
			WHERE refresh_job_id = ? AND phase = 'hydrating'
		`, refreshJobID).Scan(&repository); err != nil {
			return fmt.Errorf("load refresh generation repository: %w", err)
		}
		if pullRequest.Repository != repository {
			return fmt.Errorf("pull request %d belongs to %q, want %q", item.Number, pullRequest.Repository, repository)
		}
		if len(item.CollectionIDs) > 0 {
			pullRequest.CollectionIDs = append([]string(nil), item.CollectionIDs...)
		}
		if err := upsertPullRequestRow(ctx, tx, *pullRequest); err != nil {
			return err
		}
		if err := replacePullRequestFiles(ctx, tx, *pullRequest); err != nil {
			return err
		}
		if err := replacePullRequestContext(ctx, tx, *pullRequest); err != nil {
			return err
		}
		if pullRequest.AuthorHistory != nil {
			if err := writeAuthorHistory(ctx, tx, *pullRequest.AuthorHistory); err != nil {
				return err
			}
		}
		for _, collectionID := range pullRequest.CollectionIDs {
			result, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO collection_pull_requests (collection_id, repository, number)
				SELECT cr.collection_id, cr.repository, ?
				FROM collection_repositories cr
				JOIN collections c ON c.id = cr.collection_id
				WHERE cr.collection_id = ? AND cr.repository = ? AND c.active = 1
			`, item.Number, collectionID, repository)
			if err != nil {
				return fmt.Errorf("publish %s#%d to %q: %w", repository, item.Number, collectionID, err)
			}
			if affected, rowsErr := result.RowsAffected(); rowsErr != nil || affected > 1 {
				return fmt.Errorf("publish %s#%d to %q: invalid membership", repository, item.Number, collectionID)
			}
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE refresh_generation_pull_requests
			SET status = 'completed', payload = ?, fingerprint_version = ?,
				fingerprint = ?, cache_outcome = ?
			WHERE generation_id = (
				SELECT id FROM refresh_generations
				WHERE refresh_job_id = ? AND phase = 'hydrating'
			) AND number = ? AND status = 'pending'
		`, item.Payload, item.FingerprintVersion, item.Fingerprint,
			item.CacheOutcome, refreshJobID, item.Number)
		if err != nil {
			return fmt.Errorf("stage hydrated PR %d: %w", item.Number, err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return fmt.Errorf("PR %d is not pending in refresh job %d", item.Number, refreshJobID)
		}
	}
	policies, err := loadQualityPolicies(ctx, tx)
	if err != nil {
		return err
	}
	for _, pullRequest := range pullRequests {
		for _, collectionID := range pullRequest.CollectionIDs {
			policy, exists := policies[collectionID]
			if !exists {
				policy = quality.DefaultPolicy()
			}
			if err := writeQualityAssessment(ctx, tx, collectionID, pullRequest, policy, completedAt); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE refresh_generations
		SET retry_count = 0, failure_category = '', failure_detail = '',
			failure_remediation = '', failure_phase = ''
		WHERE refresh_job_id = ?
	`, refreshJobID); err != nil {
		return fmt.Errorf("reset refresh generation retries: %w", err)
	}
	return tx.Commit()
}

// AdvanceRefreshGeneration records the next durable coordinator phase.
func (s *Store) AdvanceRefreshGeneration(
	ctx context.Context,
	refreshJobID int64,
	from, to string,
	progressPayload []byte,
) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE refresh_generations
		SET phase = ?, progress_payload = CASE WHEN length(?) > 0 THEN ? ELSE progress_payload END,
			retry_count = 0, failure_category = '', failure_detail = '',
			failure_remediation = '', failure_phase = ''
		WHERE refresh_job_id = ? AND phase = ?
	`, to, progressPayload, progressPayload, refreshJobID, from)
	if err != nil {
		return fmt.Errorf("advance refresh generation: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return fmt.Errorf("refresh job %d is not in phase %q", refreshJobID, from)
	}
	return nil
}

// RecordRefreshGenerationRetry increments and returns the durable retry count.
func (s *Store) RecordRefreshGenerationRetry(
	ctx context.Context,
	refreshJobID int64,
	category, detail, remediation, phase string,
) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin refresh retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		UPDATE refresh_generations
		SET retry_count = retry_count + 1, failure_category = ?,
			failure_detail = ?, failure_remediation = ?, failure_phase = ?
		WHERE refresh_job_id = ?
	`, category, detail, remediation, phase, refreshJobID); err != nil {
		return 0, fmt.Errorf("record refresh retry: %w", err)
	}
	var retries int
	if err := tx.QueryRowContext(ctx, `
		SELECT retry_count FROM refresh_generations WHERE refresh_job_id = ?
	`, refreshJobID).Scan(&retries); err != nil {
		return 0, fmt.Errorf("load refresh retry count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit refresh retry: %w", err)
	}
	return retries, nil
}

// LoadStagedRefreshPullRequests returns a complete generation in number order.
func (s *Store) LoadStagedRefreshPullRequests(
	ctx context.Context,
	refreshJobID int64,
) ([]StagedRefreshPullRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.number, i.collection_ids_json, i.payload,
			i.fingerprint_version, i.fingerprint, i.cache_outcome
		FROM refresh_generation_pull_requests i
		JOIN refresh_generations g ON g.id = i.generation_id
		WHERE g.refresh_job_id = ? AND i.status = 'completed'
		ORDER BY i.number
	`, refreshJobID)
	if err != nil {
		return nil, fmt.Errorf("load staged refresh snapshot: %w", err)
	}
	defer rows.Close()
	var result []StagedRefreshPullRequest
	for rows.Next() {
		var item StagedRefreshPullRequest
		var collectionIDs string
		if err := rows.Scan(
			&item.Number, &collectionIDs, &item.Payload,
			&item.FingerprintVersion, &item.Fingerprint, &item.CacheOutcome,
		); err != nil {
			return nil, fmt.Errorf("scan staged refresh snapshot: %w", err)
		}
		if err := json.Unmarshal([]byte(collectionIDs), &item.CollectionIDs); err != nil {
			return nil, fmt.Errorf("decode staged memberships for PR %d: %w", item.Number, err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// DeleteRefreshGeneration abandons staged work without changing published PRs.
func (s *Store) DeleteRefreshGeneration(ctx context.Context, refreshJobID int64) error {
	if _, err := s.db.ExecContext(
		ctx, "DELETE FROM refresh_generations WHERE refresh_job_id = ?", refreshJobID,
	); err != nil {
		return fmt.Errorf("delete refresh generation: %w", err)
	}
	return nil
}

// RequeueRefreshJob releases a claimed coordinator for its next bounded unit.
func (s *Store) RequeueRefreshJob(
	ctx context.Context,
	refreshJobID int64,
	scheduledFor time.Time,
) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE refresh_jobs
		SET status = 'queued', scheduled_for = ?, started_at = NULL
		WHERE id = ? AND status = 'running'
	`, formatTime(scheduledFor), refreshJobID)
	if err != nil {
		return fmt.Errorf("requeue refresh job %d: %w", refreshJobID, err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return fmt.Errorf("refresh job %d is not running", refreshJobID)
	}
	return nil
}
