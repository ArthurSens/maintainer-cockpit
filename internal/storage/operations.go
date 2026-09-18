package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	removedCollectionRetention = 7 * 24 * time.Hour
)

// RetentionResult reports only deletion counts, never retained content.
type RetentionResult struct {
	AnalysisJobs       int64 `json:"analysisJobs"`
	CorrelationJobs    int64 `json:"correlationJobs"`
	RefreshGenerations int64 `json:"refreshGenerations"`
	AnalysisResults    int64 `json:"analysisResults"`
	ModelPayloads      int64 `json:"modelPayloads"`
	Progress           int64 `json:"progress"`
	Collections        int64 `json:"collections"`
	Sessions           int64 `json:"sessions"`
}

// RunRetention transactionally removes data beyond product retention windows.
func (s *Store) RunRetention(ctx context.Context, now time.Time) (RetentionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RetentionResult{}, fmt.Errorf("begin retention: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var result RetentionResult
	steps := []struct {
		destination *int64
		query       string
		cutoff      time.Time
	}{
		{&result.AnalysisJobs, "DELETE FROM analysis_jobs WHERE status = 'completed' AND finished_at < ?", now.AddDate(0, -3, 0)},
		{&result.CorrelationJobs, "DELETE FROM correlation_jobs WHERE status = 'completed' AND finished_at < ?", now.AddDate(0, -3, 0)},
		{&result.RefreshGenerations, "DELETE FROM refresh_generations WHERE refresh_job_id IN (SELECT id FROM refresh_jobs WHERE status = 'completed' AND finished_at < ?)", now.AddDate(0, -3, 0)},
		{&result.AnalysisResults, "DELETE FROM retained_analysis_results WHERE closed_at < ?", now.AddDate(0, -3, 0)},
		{&result.ModelPayloads, "DELETE FROM retained_model_payloads WHERE created_at < ?", now.AddDate(0, -3, 0)},
		{&result.Progress, "DELETE FROM progress_events WHERE occurred_at < ?", now.AddDate(0, -3, 0)},
		{&result.Sessions, "DELETE FROM sessions WHERE expires_at <= ?", now},
	}
	for _, step := range steps {
		deleted, deleteErr := tx.ExecContext(ctx, step.query, formatTime(step.cutoff))
		if deleteErr != nil {
			return RetentionResult{}, fmt.Errorf("apply retention: %w", deleteErr)
		}
		*step.destination, err = deleted.RowsAffected()
		if err != nil {
			return RetentionResult{}, fmt.Errorf("count retained rows: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO retained_analysis_results (
			collection_id, repository, number, result_json,
			provider, model, analyzed_at, payload_id, closed_at
		)
		SELECT ar.collection_id, ar.repository, ar.number, ar.result_json,
			ar.provider, ar.model, ar.analyzed_at, ar.payload_id, c.removed_at
		FROM analysis_results ar
		JOIN collections c ON c.id = ar.collection_id
		WHERE c.active = 0 AND c.removed_at <= ?
		ON CONFLICT(collection_id, repository, number) DO UPDATE SET
			result_json = excluded.result_json,
			provider = excluded.provider,
			model = excluded.model,
			analyzed_at = excluded.analyzed_at,
			payload_id = excluded.payload_id,
			closed_at = excluded.closed_at
	`, formatTime(now.Add(-removedCollectionRetention))); err != nil {
		return RetentionResult{}, fmt.Errorf("retain analyses for expired collections: %w", err)
	}
	deleted, err := tx.ExecContext(ctx, `
		DELETE FROM collections
		WHERE active = 0 AND removed_at <= ?
	`, formatTime(now.Add(-removedCollectionRetention)))
	if err != nil {
		return RetentionResult{}, fmt.Errorf("expire removed collections: %w", err)
	}
	result.Collections, err = deleted.RowsAffected()
	if err != nil {
		return RetentionResult{}, fmt.Errorf("count expired collections: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM auth_flows WHERE expires_at <= ?", formatTime(now)); err != nil {
		return RetentionResult{}, fmt.Errorf("expire authentication flows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RetentionResult{}, fmt.Errorf("commit retention: %w", err)
	}
	return result, nil
}

// Health verifies the storage dependency used by readiness.
func (s *Store) Health(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping SQLite: %w", err)
	}
	var result int
	if err := s.db.QueryRowContext(ctx, "SELECT 1").Scan(&result); err != nil {
		return fmt.Errorf("query SQLite: %w", err)
	}
	if result != 1 {
		return errors.New("query SQLite returned unexpected result")
	}
	return nil
}

// RefreshQueueStatus is non-sensitive refresh queue state.
type RefreshQueueStatus struct {
	Queued       int                 `json:"queued"`
	Running      int                 `json:"running"`
	Oldest       *time.Time          `json:"oldestQueuedAt,omitempty"`
	Repositories []RepositoryRefresh `json:"repositories"`
}

func (s *Store) RefreshStatus(ctx context.Context) (RefreshQueueStatus, error) {
	var status RefreshQueueStatus
	rows, err := s.db.QueryContext(ctx, `
		SELECT status, COUNT(*), MIN(scheduled_for)
		FROM refresh_jobs
		WHERE status IN ('queued', 'running')
		GROUP BY status
	`)
	if err != nil {
		return status, fmt.Errorf("load refresh queue: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int
		var oldest sql.NullString
		if err := rows.Scan(&state, &count, &oldest); err != nil {
			return status, err
		}
		if state == "queued" {
			status.Queued = count
			if oldest.Valid {
				value, err := time.Parse(time.RFC3339Nano, oldest.String)
				if err != nil {
					return status, err
				}
				status.Oldest = &value
			}
		} else {
			status.Running = count
		}
	}
	if err := rows.Err(); err != nil {
		return status, err
	}
	status.Repositories, err = s.ListRepositoryRefreshes(ctx)
	if err != nil {
		return status, err
	}
	return status, nil
}

// GitHubRateLimit is the last observed non-secret installation limit.
type GitHubRateLimit struct {
	State      string     `json:"state"`
	Remaining  int        `json:"remaining,omitempty"`
	Limit      int        `json:"limit,omitempty"`
	ResetAt    *time.Time `json:"resetAt,omitempty"`
	ObservedAt *time.Time `json:"observedAt,omitempty"`
}

func (s *Store) RecordGitHubRateLimit(
	ctx context.Context, remaining, limit int, resetAt, observedAt time.Time,
) error {
	if remaining < 0 || limit <= 0 || resetAt.IsZero() || observedAt.IsZero() {
		return errors.New("invalid GitHub rate limit observation")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO github_rate_limit (id, remaining, limit_value, reset_at, observed_at)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			remaining = excluded.remaining,
			limit_value = excluded.limit_value,
			reset_at = excluded.reset_at,
			observed_at = excluded.observed_at
	`, remaining, limit, formatTime(resetAt), formatTime(observedAt))
	if err != nil {
		return fmt.Errorf("record GitHub rate limit: %w", err)
	}
	return nil
}

func (s *Store) GitHubRateLimit(ctx context.Context) (GitHubRateLimit, error) {
	result := GitHubRateLimit{State: "unknown"}
	var resetAt, observedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT remaining, limit_value, reset_at, observed_at FROM github_rate_limit WHERE id = 1
	`).Scan(&result.Remaining, &result.Limit, &resetAt, &observedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("load GitHub rate limit: %w", err)
	}
	reset, err := time.Parse(time.RFC3339Nano, resetAt)
	if err != nil {
		return result, err
	}
	observed, err := time.Parse(time.RFC3339Nano, observedAt)
	if err != nil {
		return result, err
	}
	result.State, result.ResetAt, result.ObservedAt = "observed", &reset, &observed
	return result, nil
}

// GitHubRetryStatus is the last bounded rate-limit retry event.
type GitHubRetryStatus struct {
	State       string     `json:"state"`
	Reason      string     `json:"reason,omitempty"`
	WaitSeconds int64      `json:"waitSeconds,omitempty"`
	Retry       int        `json:"retry,omitempty"`
	ObservedAt  *time.Time `json:"observedAt,omitempty"`
}

func (s *Store) RecordGitHubRetry(
	ctx context.Context,
	state, reason string,
	wait time.Duration,
	retry int,
	observedAt time.Time,
) error {
	if state != "waiting" && state != "recovered" && state != "failed" {
		return fmt.Errorf("invalid GitHub retry state %q", state)
	}
	if reason != "primary" && reason != "secondary" {
		return fmt.Errorf("invalid GitHub retry reason %q", reason)
	}
	if wait < 0 || retry < 0 || observedAt.IsZero() {
		return errors.New("invalid GitHub retry observation")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO github_retry_status (
			id, state, reason, wait_seconds, retry, observed_at
		) VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			state = excluded.state,
			reason = excluded.reason,
			wait_seconds = excluded.wait_seconds,
			retry = excluded.retry,
			observed_at = excluded.observed_at
	`, state, reason, int64(wait/time.Second), retry, formatTime(observedAt))
	if err != nil {
		return fmt.Errorf("record GitHub retry status: %w", err)
	}
	return nil
}

func (s *Store) GitHubRetry(ctx context.Context) (GitHubRetryStatus, error) {
	result := GitHubRetryStatus{State: "none"}
	var observedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT state, reason, wait_seconds, retry, observed_at
		FROM github_retry_status WHERE id = 1
	`).Scan(
		&result.State, &result.Reason, &result.WaitSeconds, &result.Retry, &observedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("load GitHub retry status: %w", err)
	}
	observed, err := time.Parse(time.RFC3339Nano, observedAt)
	if err != nil {
		return result, err
	}
	result.ObservedAt = &observed
	return result, nil
}

// OperationalFailure is sanitized persisted failure state.
type OperationalFailure struct {
	Component string     `json:"component"`
	Subject   string     `json:"subject"`
	Message   string     `json:"message"`
	At        *time.Time `json:"at,omitempty"`
}

func (s *Store) RecentFailures(ctx context.Context, limit int) ([]OperationalFailure, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT 'collection', repository, error, last_attempt_at
		FROM repository_refreshes WHERE error != ''
		UNION ALL
		SELECT 'analysis', collection_id || ':' || repository || '#' || number, error, attempted_at
		FROM analysis_attempt_state WHERE status != 'complete' AND error != ''
		UNION ALL
		SELECT 'correlation', collection_id, error, last_attempt_at
		FROM correlation_state WHERE status != 'complete' AND error != ''
		ORDER BY 4 DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("load recent failures: %w", err)
	}
	defer rows.Close()
	result := make([]OperationalFailure, 0)
	for rows.Next() {
		var item OperationalFailure
		var at sql.NullString
		if err := rows.Scan(&item.Component, &item.Subject, &item.Message, &at); err != nil {
			return nil, err
		}
		if at.Valid {
			value, err := time.Parse(time.RFC3339Nano, at.String)
			if err != nil {
				return nil, err
			}
			item.At = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// ModelProviderStatus summarizes persisted provider outcomes without payloads.
type ModelProviderStatus struct {
	Name       string     `json:"name"`
	State      string     `json:"state"`
	LastResult *time.Time `json:"lastResultAt,omitempty"`
	Failures   int        `json:"failures"`
}

func (s *Store) ModelProviderStatuses(ctx context.Context) ([]ModelProviderStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, MAX(result_at), SUM(failed)
		FROM (
			SELECT provider, attempted_at AS result_at,
				CASE WHEN status = 'complete' THEN 0 ELSE 1 END AS failed
			FROM analysis_attempt_state
			WHERE provider != ''
			UNION ALL
			SELECT provider, last_attempt_at AS result_at,
				CASE WHEN status = 'complete' THEN 0 ELSE 1 END AS failed
			FROM correlation_state
			WHERE provider != ''
		)
		GROUP BY provider ORDER BY provider
	`)
	if err != nil {
		return nil, fmt.Errorf("load model provider status: %w", err)
	}
	defer rows.Close()
	result := make([]ModelProviderStatus, 0)
	for rows.Next() {
		var item ModelProviderStatus
		var latest sql.NullString
		if err := rows.Scan(&item.Name, &latest, &item.Failures); err != nil {
			return nil, err
		}
		item.State = "healthy"
		if item.Failures > 0 {
			item.State = "degraded"
		}
		if latest.Valid {
			value, err := time.Parse(time.RFC3339Nano, latest.String)
			if err != nil {
				return nil, err
			}
			item.LastResult = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// AuditEvent is a non-sensitive operations trail entry.
type AuditEvent struct {
	OccurredAt time.Time `json:"occurredAt"`
	Event      string    `json:"event"`
	UserID     *int64    `json:"userID,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

func (s *Store) RecentAuditEvents(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT occurred_at, event, user_id, detail
		FROM audit_events ORDER BY occurred_at DESC, id DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("load audit events: %w", err)
	}
	defer rows.Close()
	result := make([]AuditEvent, 0)
	for rows.Next() {
		var item AuditEvent
		var at string
		var userID sql.NullInt64
		if err := rows.Scan(&at, &item.Event, &userID, &item.Detail); err != nil {
			return nil, err
		}
		item.OccurredAt, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		if userID.Valid {
			value := userID.Int64
			item.UserID = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// RetainedModelPayload is restricted deployment-administrator data.
type RetainedModelPayload struct {
	ID        string          `json:"id"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`
	CreatedAt time.Time       `json:"createdAt"`
}

func (s *Store) RetainedModelPayload(ctx context.Context, id string) (*RetainedModelPayload, error) {
	var payload RetainedModelPayload
	var request, response, createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, request_json, response_json, created_at
		FROM retained_model_payloads WHERE id = ?
	`, id).Scan(&payload.ID, &request, &response, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load retained model payload: %w", err)
	}
	payload.Request, payload.Response = json.RawMessage(request), json.RawMessage(response)
	payload.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, err
	}
	return &payload, nil
}

// DatabaseSize reports the local SQLite file size when file-backed.
func (s *Store) DatabaseSize() int64 {
	var paths []struct {
		sequence int
		name     string
		path     string
	}
	rows, err := s.db.Query("PRAGMA database_list")
	if err != nil {
		return 0
	}
	defer rows.Close()
	for rows.Next() {
		var item struct {
			sequence int
			name     string
			path     string
		}
		if rows.Scan(&item.sequence, &item.name, &item.path) == nil {
			paths = append(paths, item)
		}
	}
	for _, item := range paths {
		if item.name == "main" && item.path != "" {
			if info, err := os.Stat(item.path); err == nil {
				return info.Size()
			}
		}
	}
	return 0
}
