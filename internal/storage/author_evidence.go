package storage

import (
	"context"
	"database/sql"
	"time"
)

// AncillaryRequestInterval keeps public profile requests below 60/hour and
// public searches below 10/minute, independently of worker concurrency.
const AncillaryRequestInterval = 65 * time.Second

// EnqueueAncillaryAuthorRefresh retries public evidence without recollecting
// repository counts, PR snapshots, or resetting a shared quota cooldown.
func (s *Store) EnqueueAncillaryAuthorRefresh(ctx context.Context, collectionID string, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT active FROM collections WHERE id = ?`, collectionID).Scan(&active); err != nil {
		return 0, err
	}
	if !active {
		return 0, ErrCollectionNotFound
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ancillary_author_jobs
		(author_id, login, status, scheduled_for)
		SELECT DISTINCT a.id, a.login, 'queued', ? FROM authors a
		JOIN pull_requests pr ON pr.author_id = a.id
		JOIN collection_pull_requests cpr USING(repository, number)
		WHERE cpr.collection_id = ?
		ORDER BY a.ancillary_collected_at, a.id`, formatTime(now), collectionID)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE authors SET ancillary_status = 'pending'
		WHERE id IN (SELECT author_id FROM ancillary_author_jobs WHERE status IN ('queued', 'running'))`); err != nil {
		return 0, err
	}
	return count, tx.Commit()
}

// AuthorEvidenceStatus separates successful collection from attempted work.
type AuthorEvidenceStatus struct {
	Status           string     `json:"status"`
	LastAttemptAt    *time.Time `json:"lastAttemptAt,omitempty"`
	LastSuccessfulAt *time.Time `json:"lastSuccessfulAt,omitempty"`
	NextAttemptAt    *time.Time `json:"nextAttemptAt,omitempty"`
	Error            string     `json:"error,omitempty"`
}

// RequeueAncillaryAuthorEvidence durably pauses the entire shared public queue.
// The failed job moves behind other authors instead of monopolizing retries.
func (s *Store) RequeueAncillaryAuthorEvidence(
	ctx context.Context, job *AncillaryAuthorJob, now, retryAt time.Time, detail string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE ancillary_author_jobs
		SET status = 'queued', scheduled_for = ?, finished_at = NULL
		WHERE id = ? AND status = 'running'`, formatTime(retryAt), job.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE authors SET ancillary_status = 'pending',
		ancillary_attempted_at = ?, ancillary_error = ? WHERE id = ?`,
		formatTime(now), detail, job.AuthorID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ancillary_queue (id, next_attempt_at) VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET next_attempt_at = MAX(next_attempt_at, excluded.next_attempt_at)`,
		formatTime(retryAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// AuthorEvidenceStatus returns bounded collection health and truthful freshness.
func (s *Store) AuthorEvidenceStatus(ctx context.Context, authorID int64) (AuthorEvidenceStatus, error) {
	var result AuthorEvidenceStatus
	var attempt, success, next sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT a.ancillary_status,
		a.ancillary_attempted_at, a.ancillary_collected_at, a.ancillary_error,
		(SELECT MAX(j.scheduled_for, COALESCE(q.next_attempt_at, j.scheduled_for))
		 FROM ancillary_author_jobs j LEFT JOIN ancillary_queue q ON q.id = 1
		 WHERE j.author_id = a.id AND j.status = 'queued' LIMIT 1)
		FROM authors a WHERE a.id = ?`, authorID).Scan(
		&result.Status, &attempt, &success, &result.Error, &next)
	if err != nil {
		return result, err
	}
	for _, item := range []struct {
		value  sql.NullString
		target **time.Time
	}{{attempt, &result.LastAttemptAt}, {success, &result.LastSuccessfulAt}, {next, &result.NextAttemptAt}} {
		if item.value.Valid {
			at, err := time.Parse(time.RFC3339Nano, item.value.String)
			if err != nil {
				return result, err
			}
			*item.target = &at
		}
	}
	return result, nil
}

// AncillaryQueueDepth includes deferred jobs so quota waits remain visible.
func (s *Store) AncillaryQueueDepth(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ancillary_author_jobs
		WHERE status = 'queued'`).Scan(&count)
	return count, err
}
