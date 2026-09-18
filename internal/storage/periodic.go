package storage

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// InitializeSchedule records a schedule target once and reports whether it
// needs its one-time bootstrap dispatch.
func (s *Store) InitializeSchedule(
	ctx context.Context, operation, target string, now time.Time,
) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO schedule_dispatches (operation, target, last_dispatched_at)
		VALUES (?, ?, ?)
	`, operation, target, formatTime(now))
	if err != nil {
		return false, fmt.Errorf("initialize %s schedule: %w", operation, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (s *Store) RecordScheduleDispatch(
	ctx context.Context, operation, target string, now time.Time,
) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO schedule_dispatches (operation, target, last_dispatched_at)
		VALUES (?, ?, ?)
		ON CONFLICT(operation, target) DO UPDATE SET
			last_dispatched_at = excluded.last_dispatched_at
	`, operation, target, formatTime(now))
	if err != nil {
		return fmt.Errorf("record %s schedule dispatch: %w", operation, err)
	}
	return nil
}

func (s *Store) DeleteScheduleState(ctx context.Context, operation, target string) error {
	_, err := s.db.ExecContext(
		ctx, `DELETE FROM schedule_dispatches WHERE operation = ? AND target = ?`,
		operation, target,
	)
	if err != nil {
		return fmt.Errorf("delete %s schedule state: %w", operation, err)
	}
	return nil
}

func (s *Store) EnqueueAllRefreshes(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin scheduled repository refreshes: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		UPDATE refresh_generations
		SET rediscovery_requested = 1
		WHERE refresh_job_id IN (
			SELECT id FROM refresh_jobs WHERE status IN ('queued', 'running')
		)
	`); err != nil {
		return fmt.Errorf("request scheduled repository rediscovery: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO refresh_jobs (repository, status, scheduled_for, forced)
		SELECT repository, 'queued', ?, 0 FROM repository_refreshes
	`, formatTime(now)); err != nil {
		return fmt.Errorf("enqueue scheduled repository refreshes: %w", err)
	}
	return tx.Commit()
}

// EnqueueCollectionEvidence schedules contribution calls from the collection's
// latest stored open-PR authors without requiring a fresh core snapshot.
func (s *Store) EnqueueCollectionEvidence(
	ctx context.Context, collectionID string, now time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin collection evidence enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var active bool
	if err := tx.QueryRowContext(
		ctx, `SELECT active FROM collections WHERE id = ?`, collectionID,
	).Scan(&active); err != nil {
		return fmt.Errorf("load contribution collection %q: %w", collectionID, err)
	}
	if !active {
		return fmt.Errorf("collection %q is not active", collectionID)
	}
	at := formatTime(now)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO authors (
			id, login, account_created_at, history_complete, recent_activity_json,
			collected_at, ancillary_status
		)
		SELECT DISTINCT pr.author_id, pr.author, '', 0, '[]', ?, 'pending'
		FROM collection_pull_requests cpr
		JOIN pull_requests pr
			ON pr.repository = cpr.repository AND pr.number = cpr.number
		WHERE cpr.collection_id = ? AND pr.author_id > 0 AND pr.author != ''
		ON CONFLICT(id) DO UPDATE SET login = excluded.login
	`, at, collectionID); err != nil {
		return fmt.Errorf("upsert contribution authors for %q: %w", collectionID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO contribution_evidence_jobs (
			author_id, login, repository, association, status, scheduled_for
		)
		SELECT DISTINCT pr.author_id, pr.author, target.repository,
			CASE
				WHEN target.repository = pr.repository AND json_valid(pr.reuse_payload)
				THEN COALESCE(
					json_extract(CAST(pr.reuse_payload AS TEXT), '$.AuthorAssociation'),
					existing.association, ''
				)
				ELSE COALESCE(existing.association, '')
			END,
			'queued', ?
		FROM collection_pull_requests cpr
		JOIN pull_requests pr
			ON pr.repository = cpr.repository AND pr.number = cpr.number
		JOIN collection_repositories target ON target.collection_id = cpr.collection_id
		LEFT JOIN author_repository_contributions existing
			ON existing.author_id = pr.author_id AND existing.repository = target.repository
		WHERE cpr.collection_id = ? AND pr.author_id > 0 AND pr.author != ''
	`, at, collectionID); err != nil {
		return fmt.Errorf("enqueue contribution evidence for %q: %w", collectionID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO author_repository_contributions (
			author_id, repository, association, merged, closed_unmerged, open,
			status, collected_at
		)
		SELECT DISTINCT jobs.author_id, jobs.repository, jobs.association,
			0, 0, 0, 'pending', NULL
		FROM contribution_evidence_jobs jobs
		WHERE jobs.status IN ('queued', 'running')
		ON CONFLICT(author_id, repository) DO UPDATE SET status = 'pending'
	`); err != nil {
		return fmt.Errorf("mark contribution evidence pending: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO ancillary_author_jobs (
			author_id, login, status, scheduled_for
		)
		SELECT DISTINCT pr.author_id, pr.author, 'queued', ?
		FROM collection_pull_requests cpr
		JOIN pull_requests pr
			ON pr.repository = cpr.repository AND pr.number = cpr.number
		WHERE cpr.collection_id = ? AND pr.author_id > 0 AND pr.author != ''
	`, at, collectionID); err != nil {
		return fmt.Errorf("enqueue ancillary evidence for %q: %w", collectionID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE authors SET ancillary_status = 'pending'
		WHERE id IN (
			SELECT author_id FROM ancillary_author_jobs
			WHERE status IN ('queued', 'running')
		)
	`); err != nil {
		return fmt.Errorf("mark ancillary evidence pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit contribution evidence for %q: %w", collectionID, err)
	}
	return nil
}

// CancelDisabledContributionEvidence cancels only queued work that is no
// longer reachable from an enabled contribution collection.
func (s *Store) CancelDisabledContributionEvidence(
	ctx context.Context, enabledCollections []string, now time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin contribution cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	arguments := make([]any, len(enabledCollections))
	placeholders := make([]string, len(enabledCollections))
	for index, collectionID := range enabledCollections {
		arguments[index] = collectionID
		placeholders[index] = "?"
	}
	at := formatTime(now)
	if len(enabledCollections) == 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE contribution_evidence_jobs SET status = 'completed', finished_at = ?
			WHERE status = 'queued'
		`, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE ancillary_author_jobs SET status = 'completed', finished_at = ?
			WHERE status = 'queued'
		`, at); err != nil {
			return err
		}
		return tx.Commit()
	}
	in := strings.Join(placeholders, ",")
	repositoryQuery := `
		UPDATE contribution_evidence_jobs SET status = 'completed', finished_at = ?
		WHERE status = 'queued' AND repository NOT IN (
			SELECT repository FROM collection_repositories
			WHERE collection_id IN (` + in + `)
		)`
	repositoryArguments := append([]any{at}, arguments...)
	if _, err := tx.ExecContext(ctx, repositoryQuery, repositoryArguments...); err != nil {
		return fmt.Errorf("cancel disabled repository evidence: %w", err)
	}
	ancillaryQuery := `
		UPDATE ancillary_author_jobs AS jobs SET status = 'completed', finished_at = ?
		WHERE jobs.status = 'queued' AND NOT EXISTS (
			SELECT 1
			FROM collection_pull_requests cpr
			JOIN pull_requests pr
				ON pr.repository = cpr.repository AND pr.number = cpr.number
			WHERE cpr.collection_id IN (` + in + `)
			  AND pr.author_id = jobs.author_id
		)`
	ancillaryArguments := append([]any{at}, arguments...)
	if _, err := tx.ExecContext(ctx, ancillaryQuery, ancillaryArguments...); err != nil {
		return fmt.Errorf("cancel disabled ancillary evidence: %w", err)
	}
	return tx.Commit()
}
