package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// PullRequestDiffJob is durable current-head patch collection work.
type PullRequestDiffJob struct {
	ID           int64
	Repository   string
	Number       int
	HeadSHA      string
	ChangedFiles int
	ScheduledFor time.Time
}

// PullRequestDiffSource is one bounded, stable analysis source.
type PullRequestDiffSource struct {
	Path          string
	Patch         string
	OriginalBytes int
	SentBytes     int
	Truncated     bool
}

// PullRequestDiffEvidence is durable metadata and bounded patch content.
type PullRequestDiffEvidence struct {
	Completeness  string
	OriginalFiles int
	SentFiles     int
	OmittedFiles  int
	OriginalBytes int
	SentBytes     int
	Truncated     bool
	Sources       []PullRequestDiffSource
}

func enqueuePullRequestDiffTx(
	ctx context.Context, tx *sql.Tx, pr PullRequest, scheduledFor time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE analysis_jobs
		SET status = 'completed', finished_at = ?
		WHERE repository = ? AND number = ? AND status = 'queued' AND head_sha != ?
	`, formatTime(scheduledFor), pr.Repository, pr.Number, pr.HeadSHA); err != nil {
		return fmt.Errorf("cancel superseded queued analysis for %s#%d: %w",
			pr.Repository, pr.Number, err)
	}
	if pr.HeadSHA == "" {
		for _, collectionID := range pr.CollectionIDs {
			if err := enqueueAnalysisForPRTx(
				ctx, tx, collectionID, pr.Repository, pr.Number, false, scheduledFor,
			); err != nil {
				return err
			}
		}
		return nil
	}
	var completeness string
	err := tx.QueryRowContext(ctx, `
		SELECT completeness FROM pull_request_diff_evidence
		WHERE repository = ? AND number = ? AND head_sha = ?
	`, pr.Repository, pr.Number, pr.HeadSHA).Scan(&completeness)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load cached diff evidence: %w", err)
	}
	if completeness == "complete" || completeness == "partial" {
		for _, collectionID := range pr.CollectionIDs {
			if err := enqueueAnalysisForPRTx(
				ctx, tx, collectionID, pr.Repository, pr.Number, false, scheduledFor,
			); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO pull_request_diff_jobs (
			repository, number, head_sha, changed_files, status, scheduled_for
		) VALUES (?, ?, ?, ?, 'queued', ?)
	`, pr.Repository, pr.Number, pr.HeadSHA, pr.ChangedFiles, formatTime(scheduledFor)); err != nil {
		return fmt.Errorf("enqueue diff for %s#%d: %w", pr.Repository, pr.Number, err)
	}
	return nil
}

// SupersedePullRequestDiff completes a job whose GitHub head changed without
// caching unavailable evidence or releasing analysis for the obsolete head.
func (s *Store) SupersedePullRequestDiff(
	ctx context.Context, job *PullRequestDiffJob, completedAt time.Time,
) error {
	if job == nil {
		return errors.New("diff job is required")
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE pull_request_diff_jobs
		SET status = 'completed', finished_at = ?
		WHERE id = ? AND status = 'running'
	`, formatTime(completedAt), job.ID); err != nil {
		return fmt.Errorf("supersede diff job %d: %w", job.ID, err)
	}
	return nil
}

// ClaimPullRequestDiff atomically claims the next due diff job.
func (s *Store) ClaimPullRequestDiff(
	ctx context.Context, now time.Time,
) (*PullRequestDiffJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin diff claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var job PullRequestDiffJob
	var scheduled string
	err = tx.QueryRowContext(ctx, `
		SELECT id, repository, number, head_sha, changed_files, scheduled_for
		FROM pull_request_diff_jobs
		WHERE status = 'queued' AND scheduled_for <= ?
		ORDER BY scheduled_for, id
		LIMIT 1
	`, formatTime(now)).Scan(
		&job.ID, &job.Repository, &job.Number, &job.HeadSHA,
		&job.ChangedFiles, &scheduled,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select diff job: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE pull_request_diff_jobs SET status = 'running', started_at = ?
		WHERE id = ? AND status = 'queued'
	`, formatTime(now), job.ID)
	if err != nil {
		return nil, fmt.Errorf("claim diff job: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return nil, fmt.Errorf("diff job %d was claimed concurrently", job.ID)
	}
	job.ScheduledFor, err = time.Parse(time.RFC3339Nano, scheduled)
	if err != nil {
		return nil, fmt.Errorf("parse diff schedule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit diff claim: %w", err)
	}
	return &job, nil
}

// CompletePullRequestDiff caches a terminal attempt and releases current-head
// analysis. Unavailable attempts remain eligible on a later snapshot refresh.
func (s *Store) CompletePullRequestDiff(
	ctx context.Context, job *PullRequestDiffJob, evidence PullRequestDiffEvidence,
	collectedAt time.Time, collectErr error,
) error {
	if job == nil {
		return errors.New("diff job is required")
	}
	if collectErr != nil {
		evidence = PullRequestDiffEvidence{
			Completeness: "unavailable", OriginalFiles: job.ChangedFiles,
			OmittedFiles: job.ChangedFiles, Truncated: job.ChangedFiles > 0,
		}
	}
	if evidence.Completeness != "complete" &&
		evidence.Completeness != "partial" &&
		evidence.Completeness != "unavailable" {
		return fmt.Errorf("invalid diff completeness %q", evidence.Completeness)
	}
	if err := validatePullRequestDiffEvidence(evidence); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var pullRequestExists int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pull_requests WHERE repository = ? AND number = ?
	`, job.Repository, job.Number).Scan(&pullRequestExists); err != nil {
		return err
	}
	if pullRequestExists == 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pull_request_diff_evidence (
			repository, number, head_sha, completeness,
			original_files, sent_files, omitted_files,
			original_bytes, sent_bytes, truncated, collected_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repository, number, head_sha) DO UPDATE SET
			completeness = excluded.completeness,
			original_files = excluded.original_files,
			sent_files = excluded.sent_files,
			omitted_files = excluded.omitted_files,
			original_bytes = excluded.original_bytes,
			sent_bytes = excluded.sent_bytes,
			truncated = excluded.truncated,
			collected_at = excluded.collected_at
	`, job.Repository, job.Number, job.HeadSHA, evidence.Completeness,
		evidence.OriginalFiles, evidence.SentFiles, evidence.OmittedFiles,
		evidence.OriginalBytes, evidence.SentBytes, evidence.Truncated,
		formatTime(collectedAt)); err != nil {
		return fmt.Errorf("persist diff evidence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM pull_request_diff_sources
		WHERE repository = ? AND number = ? AND head_sha = ?
	`, job.Repository, job.Number, job.HeadSHA); err != nil {
		return fmt.Errorf("replace diff sources: %w", err)
	}
	for _, source := range evidence.Sources {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pull_request_diff_sources (
				repository, number, head_sha, path, source_id, patch,
				original_bytes, sent_bytes, truncated
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, job.Repository, job.Number, job.HeadSHA, source.Path, "diff:"+source.Path,
			source.Patch, source.OriginalBytes, source.SentBytes, source.Truncated); err != nil {
			return fmt.Errorf("persist diff source %q: %w", source.Path, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE pull_request_diff_jobs
		SET status = 'completed', finished_at = ? WHERE id = ?
	`, formatTime(collectedAt), job.ID); err != nil {
		return fmt.Errorf("complete diff job: %w", err)
	}
	var currentHead string
	if err := tx.QueryRowContext(ctx, `
		SELECT head_sha FROM pull_requests WHERE repository = ? AND number = ?
	`, job.Repository, job.Number).Scan(&currentHead); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if currentHead == job.HeadSHA {
		rows, err := tx.QueryContext(ctx, `
			SELECT collection_id FROM collection_pull_requests
			WHERE repository = ? AND number = ?
			ORDER BY collection_id
		`, job.Repository, job.Number)
		if err != nil {
			return err
		}
		var collectionIDs []string
		for rows.Next() {
			var collectionID string
			if err := rows.Scan(&collectionID); err != nil {
				_ = rows.Close()
				return err
			}
			collectionIDs = append(collectionIDs, collectionID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, collectionID := range collectionIDs {
			if err := enqueueAnalysisForPRTx(
				ctx, tx, collectionID, job.Repository, job.Number, false, collectedAt,
			); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func validatePullRequestDiffEvidence(evidence PullRequestDiffEvidence) error {
	if len(evidence.Sources) > 100 || evidence.SentFiles != len(evidence.Sources) ||
		evidence.OriginalFiles < evidence.SentFiles ||
		evidence.OmittedFiles != evidence.OriginalFiles-evidence.SentFiles {
		return errors.New("diff evidence has inconsistent file metadata")
	}
	sentBytes := 0
	truncated := evidence.OmittedFiles > 0
	for _, source := range evidence.Sources {
		if source.Path == "" || source.Patch == "" || !utf8.ValidString(source.Patch) ||
			len(source.Patch) > 8<<10 || source.SentBytes != len(source.Patch) ||
			source.OriginalBytes < source.SentBytes {
			return fmt.Errorf("diff source %q is invalid", source.Path)
		}
		sentBytes += source.SentBytes
		truncated = truncated || source.Truncated
	}
	if sentBytes != evidence.SentBytes || sentBytes > 128<<10 ||
		evidence.OriginalBytes < evidence.SentBytes ||
		evidence.Truncated != truncated {
		return errors.New("diff evidence has inconsistent byte metadata")
	}
	if evidence.Completeness == "complete" && evidence.Truncated ||
		evidence.Completeness == "partial" && !evidence.Truncated ||
		evidence.Completeness == "unavailable" &&
			(len(evidence.Sources) != 0 || evidence.SentBytes != 0) {
		return errors.New("diff evidence completeness does not match its content")
	}
	return nil
}
