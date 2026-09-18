package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/analysis"
)

// AnalysisJob is one durable collection-specific model-analysis request.
type AnalysisJob struct {
	ID               int64
	CollectionID     string
	Repository       string
	Number           int
	HeadSHA          string
	PrimaryProvider  string
	FallbackProvider string
	Forced           bool
	ScheduledFor     time.Time
}

func enqueueAnalysisForPRTx(
	ctx context.Context,
	tx *sql.Tx,
	collectionID, repository string,
	number int,
	forced bool,
	scheduledFor time.Time,
) error {
	var provider string
	err := tx.QueryRowContext(ctx,
		"SELECT model_provider FROM collections WHERE id = ?", collectionID,
	).Scan(&provider)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q", ErrCollectionNotFound, collectionID)
	}
	if err != nil {
		return fmt.Errorf("load model provider for %q: %w", collectionID, err)
	}
	if provider == "" {
		return nil
	}
	var headSHA string
	if err := tx.QueryRowContext(ctx, `
		SELECT head_sha FROM pull_requests WHERE repository = ? AND number = ?
	`, repository, number).Scan(&headSHA); err != nil {
		return fmt.Errorf("load analysis head for %s#%d: %w", repository, number, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO analysis_jobs (
			collection_id, repository, number, head_sha, status, forced, scheduled_for
		) VALUES (?, ?, ?, ?, 'queued', ?, ?)
		ON CONFLICT DO NOTHING
	`, collectionID, repository, number, headSHA, forced, formatTime(scheduledFor)); err != nil {
		return fmt.Errorf("enqueue analysis for %s#%d in %q: %w", repository, number, collectionID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE analysis_jobs
		SET head_sha = ?, forced = CASE WHEN ? THEN 1 ELSE forced END,
			scheduled_for = MIN(scheduled_for, ?)
		WHERE collection_id = ? AND repository = ? AND number = ?
		  AND status = 'queued'
	`, headSHA, forced, formatTime(scheduledFor), collectionID, repository, number); err != nil {
		return fmt.Errorf("update queued analysis for %s#%d: %w", repository, number, err)
	}
	return nil
}

// enqueueAnalysisOrDiffForPRTx requests analysis for a PR, first routing
// through diff-evidence collection when none is cached yet for the current
// head. This keeps explicit analysis requests (e.g. reanalyze) from wedging
// a queued job that ClaimAnalysis's diff-evidence join can never satisfy.
func enqueueAnalysisOrDiffForPRTx(
	ctx context.Context,
	tx *sql.Tx,
	collectionID, repository string,
	number int,
	forced bool,
	scheduledFor time.Time,
) error {
	var headSHA string
	var changedFiles int
	if err := tx.QueryRowContext(ctx, `
		SELECT head_sha, changed_files FROM pull_requests WHERE repository = ? AND number = ?
	`, repository, number).Scan(&headSHA, &changedFiles); err != nil {
		return fmt.Errorf("load diff head for %s#%d: %w", repository, number, err)
	}
	if headSHA == "" {
		return enqueueAnalysisForPRTx(ctx, tx, collectionID, repository, number, forced, scheduledFor)
	}
	var completeness string
	err := tx.QueryRowContext(ctx, `
		SELECT completeness FROM pull_request_diff_evidence
		WHERE repository = ? AND number = ? AND head_sha = ?
	`, repository, number, headSHA).Scan(&completeness)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load cached diff evidence for %s#%d: %w", repository, number, err)
	}
	if completeness == "complete" || completeness == "partial" || completeness == "unavailable" {
		return enqueueAnalysisForPRTx(ctx, tx, collectionID, repository, number, forced, scheduledFor)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO pull_request_diff_jobs (
			repository, number, head_sha, changed_files, status, scheduled_for
		) VALUES (?, ?, ?, ?, 'queued', ?)
	`, repository, number, headSHA, changedFiles, formatTime(scheduledFor)); err != nil {
		return fmt.Errorf("enqueue diff for %s#%d: %w", repository, number, err)
	}
	return nil
}

// EnqueueCollectionAnalysis explicitly requests every current PR in a
// collection. Active duplicate work is coalesced.
func (s *Store) EnqueueCollectionAnalysis(
	ctx context.Context, collectionID string, forced bool, scheduledFor time.Time,
) (enqueued, coalesced int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin collection analysis enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var provider string
	if err := tx.QueryRowContext(ctx,
		"SELECT model_provider FROM collections WHERE id = ?", collectionID,
	).Scan(&provider); errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("%w: %q", ErrCollectionNotFound, collectionID)
	} else if err != nil {
		return 0, 0, fmt.Errorf("load collection model provider: %w", err)
	}
	if provider == "" {
		return 0, 0, fmt.Errorf("collection %q has no model provider", collectionID)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT repository, number
		FROM collection_pull_requests
		WHERE collection_id = ?
		ORDER BY repository, number
	`, collectionID)
	if err != nil {
		return 0, 0, fmt.Errorf("list collection pull requests for analysis: %w", err)
	}
	var identities []struct {
		repository string
		number     int
	}
	for rows.Next() {
		var identity struct {
			repository string
			number     int
		}
		if err := rows.Scan(&identity.repository, &identity.number); err != nil {
			_ = rows.Close()
			return 0, 0, fmt.Errorf("scan pull request for analysis: %w", err)
		}
		identities = append(identities, identity)
	}
	if err := rows.Close(); err != nil {
		return 0, 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, identity := range identities {
		var active int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM analysis_jobs
			WHERE collection_id = ? AND repository = ? AND number = ?
			  AND status IN ('queued', 'running')
		`, collectionID, identity.repository, identity.number).Scan(&active); err != nil {
			return 0, 0, err
		}
		if active > 0 {
			coalesced++
		} else {
			enqueued++
		}
		if err := enqueueAnalysisOrDiffForPRTx(
			ctx, tx, collectionID, identity.repository, identity.number, forced, scheduledFor,
		); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit collection analysis enqueue: %w", err)
	}
	return enqueued, coalesced, nil
}

// RecoverAnalysisJobs requeues work interrupted by a prior process.
func (s *Store) RecoverAnalysisJobs(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE analysis_jobs SET status = 'queued', started_at = NULL
		WHERE status = 'running'
	`); err != nil {
		return fmt.Errorf("recover interrupted analysis jobs: %w", err)
	}
	return nil
}

// ClaimAnalysis atomically claims the next due analysis job.
func (s *Store) ClaimAnalysis(ctx context.Context, now time.Time) (*AnalysisJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin analysis claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var job AnalysisJob
	var scheduledFor string
	err = tx.QueryRowContext(ctx, `
		SELECT j.id, j.collection_id, j.repository, j.number, j.head_sha,
			j.forced, j.scheduled_for,
			c.model_provider, c.model_fallback_provider
		FROM analysis_jobs j
		JOIN collections c ON c.id = j.collection_id
		JOIN pull_requests pr
		  ON pr.repository = j.repository AND pr.number = j.number
		LEFT JOIN pull_request_diff_evidence diff
		  ON diff.repository = pr.repository AND diff.number = pr.number
		 AND diff.head_sha = pr.head_sha
		WHERE j.status = 'queued' AND j.scheduled_for <= ?
		  AND c.active = 1 AND c.model_provider != ''
		  AND j.head_sha = pr.head_sha
		  AND (
			pr.head_sha = '' OR
			diff.completeness IN ('complete', 'partial', 'unavailable')
		  )
		ORDER BY j.scheduled_for, j.id
		LIMIT 1
	`, formatTime(now)).Scan(
		&job.ID, &job.CollectionID, &job.Repository, &job.Number, &job.HeadSHA, &job.Forced,
		&scheduledFor, &job.PrimaryProvider, &job.FallbackProvider,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select analysis job: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE analysis_jobs SET status = 'running', started_at = ?
		WHERE id = ? AND status = 'queued'
	`, formatTime(now), job.ID)
	if err != nil {
		return nil, fmt.Errorf("claim analysis job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return nil, fmt.Errorf("analysis job %d was claimed concurrently", job.ID)
	}
	job.ScheduledFor, err = time.Parse(time.RFC3339Nano, scheduledFor)
	if err != nil {
		return nil, fmt.Errorf("parse analysis schedule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit analysis claim: %w", err)
	}
	return &job, nil
}

// BuildAnalysisRequest loads public, already-collected evidence for one job.
func (s *Store) BuildAnalysisRequest(ctx context.Context, job AnalysisJob) (analysis.Request, error) {
	var input analysis.Input
	input.CollectionID, input.Repository, input.Number = job.CollectionID, job.Repository, job.Number
	var updatedAt, headSHA string
	err := s.db.QueryRowContext(ctx, `
		SELECT pr.title, pr.body, pr.url, pr.updated_at, pr.head_sha, pr.additions, pr.deletions,
			COALESCE(context.completeness, 'unknown')
		FROM pull_requests pr
		JOIN collection_pull_requests cpr
			ON cpr.repository = pr.repository AND cpr.number = pr.number
		LEFT JOIN pull_request_contexts context
			ON context.repository = pr.repository AND context.number = pr.number
		WHERE cpr.collection_id = ? AND pr.repository = ? AND pr.number = ?
	`, job.CollectionID, job.Repository, job.Number).Scan(
		&input.Title, &input.Description, &input.URL, &updatedAt, &headSHA,
		&input.Additions, &input.Deletions, &input.ContextCompleteness,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return analysis.Request{}, fmt.Errorf("%w: analysis input", ErrCollectionNotFound)
	}
	if err != nil {
		return analysis.Request{}, fmt.Errorf("load analysis input: %w", err)
	}
	if headSHA != job.HeadSHA {
		return analysis.Request{}, ErrAnalysisSuperseded
	}
	if headSHA != "" {
		var terminal int
		if err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM pull_request_diff_evidence
			WHERE repository = ? AND number = ? AND head_sha = ?
			  AND completeness IN ('complete', 'partial', 'unavailable')
		`, job.Repository, job.Number, headSHA).Scan(&terminal); err != nil {
			return analysis.Request{}, fmt.Errorf("check terminal diff evidence: %w", err)
		}
		if terminal == 0 {
			return analysis.Request{}, errors.New("current-head diff evidence is pending")
		}
	}
	input.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return analysis.Request{}, fmt.Errorf("parse analysis input time: %w", err)
	}
	fileRows, err := s.db.QueryContext(ctx, `
		SELECT path FROM pull_request_files
		WHERE repository = ? AND number = ?
		ORDER BY path
	`, job.Repository, job.Number)
	if err != nil {
		return analysis.Request{}, fmt.Errorf("load analysis files: %w", err)
	}
	for fileRows.Next() {
		var file string
		if err := fileRows.Scan(&file); err != nil {
			_ = fileRows.Close()
			return analysis.Request{}, err
		}
		input.Files = append(input.Files, file)
	}
	if err := fileRows.Close(); err != nil {
		return analysis.Request{}, err
	}
	sourceRows, err := s.db.QueryContext(ctx, `
		SELECT source_id, kind, title, text, state, url
		FROM pull_request_relationships
		WHERE repository = ? AND number = ?
		ORDER BY source_id
	`, job.Repository, job.Number)
	if err != nil {
		return analysis.Request{}, fmt.Errorf("load analysis sources: %w", err)
	}
	for sourceRows.Next() {
		var source analysis.Source
		var title, text, state string
		if err := sourceRows.Scan(
			&source.ID, &source.Kind, &title, &text, &state, &source.URL,
		); err != nil {
			_ = sourceRows.Close()
			return analysis.Request{}, err
		}
		source.Text = text
		if source.Text == "" {
			source.Text = title
		} else if title != "" && title != text {
			source.Text = title + "\n" + text
		}
		if state != "" {
			source.Text += "\nState: " + state
		}
		input.Sources = append(input.Sources, source)
	}
	if err := sourceRows.Close(); err != nil {
		return analysis.Request{}, err
	}
	input.DiffEvidence.Completeness = "unavailable"
	err = s.db.QueryRowContext(ctx, `
		SELECT completeness, original_files, sent_files, omitted_files,
			original_bytes, sent_bytes, truncated
		FROM pull_request_diff_evidence
		WHERE repository = ? AND number = ? AND head_sha = ?
	`, job.Repository, job.Number, headSHA).Scan(
		&input.DiffEvidence.Completeness,
		&input.DiffEvidence.OriginalFiles,
		&input.DiffEvidence.SentFiles,
		&input.DiffEvidence.OmittedFiles,
		&input.DiffEvidence.OriginalBytes,
		&input.DiffEvidence.SentBytes,
		&input.DiffEvidence.Truncated,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return analysis.Request{}, fmt.Errorf("load diff evidence: %w", err)
	}
	diffRows, err := s.db.QueryContext(ctx, `
		SELECT source_id, patch
		FROM pull_request_diff_sources
		WHERE repository = ? AND number = ? AND head_sha = ?
		ORDER BY path
	`, job.Repository, job.Number, headSHA)
	if err != nil {
		return analysis.Request{}, fmt.Errorf("load diff sources: %w", err)
	}
	for diffRows.Next() {
		var source analysis.Source
		if err := diffRows.Scan(&source.ID, &source.Text); err != nil {
			_ = diffRows.Close()
			return analysis.Request{}, err
		}
		source.Kind = "diff"
		source.URL = input.URL + "/files"
		input.Sources = append(input.Sources, source)
	}
	if err := diffRows.Close(); err != nil {
		return analysis.Request{}, err
	}
	correlationRows, err := s.db.QueryContext(ctx, `
		SELECT groups.id, groups.text_graph
		FROM correlation_groups groups
		JOIN correlation_group_members members
		  ON members.collection_id = groups.collection_id AND members.group_id = groups.id
		WHERE groups.collection_id = ? AND members.repository = ? AND members.number = ?
		ORDER BY groups.id
	`, job.CollectionID, job.Repository, job.Number)
	if err != nil {
		return analysis.Request{}, fmt.Errorf("load correlation sources: %w", err)
	}
	for correlationRows.Next() {
		var id, text string
		if err := correlationRows.Scan(&id, &text); err != nil {
			_ = correlationRows.Close()
			return analysis.Request{}, err
		}
		input.Sources = append(input.Sources, analysis.Source{
			ID: "correlation-group:" + id, Kind: "correlation_group", Text: text,
		})
	}
	if err := correlationRows.Close(); err != nil {
		return analysis.Request{}, err
	}
	return analysis.BuildRequest(input)
}

// AnalysisIsCurrent reports whether a complete result already covers the
// exact factual input revision under the current product-owned schema.
func (s *Store) AnalysisIsCurrent(ctx context.Context, job AnalysisJob, revision string) (bool, error) {
	var current, provider, schemaVersion string
	err := s.db.QueryRowContext(ctx, `
		SELECT attempt.input_revision, attempt.provider, attempt.schema_version
		FROM analysis_attempt_state attempt
		JOIN analysis_results result
		  ON result.collection_id = attempt.collection_id
		 AND result.repository = attempt.repository
		 AND result.number = attempt.number
		 AND result.schema_version = attempt.schema_version
		WHERE attempt.collection_id = ? AND attempt.repository = ? AND attempt.number = ?
		  AND attempt.status = 'complete'
	`, job.CollectionID, job.Repository, job.Number).Scan(
		&current, &provider, &schemaVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load cached analysis revision: %w", err)
	}
	providerStillAssigned := provider == job.PrimaryProvider ||
		job.FallbackProvider != "" && provider == job.FallbackProvider
	return current == revision &&
		providerStillAssigned &&
		schemaVersion == analysis.SchemaVersion, nil
}

// RecordAnalysisSections merges independently valid sections into the
// last-valid result and records the latest attempt health separately.
func (s *Store) RecordAnalysisSections(
	ctx context.Context,
	job AnalysisJob,
	request analysis.Request,
	response analysis.ProviderResponse,
	provider, fallbackFrom, model string,
	requestBody, responseBody []byte,
	analyzedAt time.Time,
	status, publicMessage string,
) error {
	if status != "complete" && status != "partial" {
		return fmt.Errorf("invalid analysis section status %q", status)
	}
	var previous analysis.Result
	var encoded string
	err := s.db.QueryRowContext(ctx, `
		SELECT result_json FROM analysis_results
		WHERE collection_id = ? AND repository = ? AND number = ?
	`, job.CollectionID, job.Repository, job.Number).Scan(&encoded)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load last valid analysis: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal([]byte(encoded), &previous); err != nil {
			return fmt.Errorf("decode last valid analysis: %w", err)
		}
	}
	result := analysis.Merge(previous, response)
	SortWaiting(result.WaitingOn)
	return s.writeAnalysis(
		ctx, job, request, result, status, provider, fallbackFrom, model,
		requestBody, responseBody, analyzedAt, publicMessage,
	)
}

// RecordAnalysis stores a complete validated result and retained payload.
func (s *Store) RecordAnalysis(
	ctx context.Context,
	job AnalysisJob,
	request analysis.Request,
	result analysis.Result,
	provider, fallbackFrom, model string,
	requestBody, responseBody []byte,
	analyzedAt time.Time,
) error {
	return s.writeAnalysis(
		ctx, job, request, result, "complete", provider, fallbackFrom, model,
		requestBody, responseBody, analyzedAt, "",
	)
}

// RecordAnalysisFailure records attempt health without replacing last-valid
// analysis sections.
func (s *Store) RecordAnalysisFailure(
	ctx context.Context,
	job AnalysisJob,
	request analysis.Request,
	provider, model string,
	requestBody, responseBody []byte,
	analyzedAt time.Time,
	status, publicMessage string,
) error {
	if status != "unknown" && status != "failed" {
		return fmt.Errorf("invalid analysis failure status %q", status)
	}
	return s.writeAnalysisAttempt(
		ctx, job, request.InputRevision, request.SchemaVersion, request.PromptVersion,
		status, provider, "", model, requestBody, responseBody, analyzedAt, publicMessage,
	)
}

func (s *Store) writeAnalysis(
	ctx context.Context,
	job AnalysisJob,
	request analysis.Request,
	result analysis.Result,
	status, provider, fallbackFrom, model string,
	requestBody, responseBody []byte,
	analyzedAt time.Time,
	publicMessage string,
) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode analysis result: %w", err)
	}
	payloadID, err := newPayloadID()
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin analysis persistence: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureAnalysisJobCurrentTx(ctx, tx, job); err != nil {
		return err
	}
	if err := retainAnalysisPayload(ctx, tx, payloadID, requestBody, responseBody, analyzedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO analysis_results (
			collection_id, repository, number, status, input_revision,
			provider, fallback_from, model, analyzed_at, payload_id, result_json,
			quality_level, error, schema_version, prompt_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(collection_id, repository, number) DO UPDATE SET
			status = excluded.status,
			input_revision = excluded.input_revision,
			provider = excluded.provider,
			fallback_from = excluded.fallback_from,
			model = excluded.model,
			analyzed_at = excluded.analyzed_at,
			payload_id = excluded.payload_id,
			result_json = excluded.result_json,
			quality_level = excluded.quality_level,
			error = excluded.error,
			schema_version = excluded.schema_version,
			prompt_version = excluded.prompt_version
	`, job.CollectionID, job.Repository, job.Number, "complete", request.InputRevision,
		provider, fallbackFrom, model, formatTime(analyzedAt), payloadID, string(resultJSON),
		result.QualityLevel, "", request.SchemaVersion, request.PromptVersion); err != nil {
		return fmt.Errorf("persist analysis result: %w", err)
	}
	if err := upsertAnalysisAttempt(
		ctx, tx, job, status, request.InputRevision, provider, fallbackFrom, model,
		analyzedAt, payloadID, publicMessage, request.SchemaVersion, request.PromptVersion,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit analysis persistence: %w", err)
	}
	return nil
}

// RecordAnalysisBuildFailure bounds a job-local input failure and leaves any
// prior valid analysis untouched.
func (s *Store) RecordAnalysisBuildFailure(
	ctx context.Context, job AnalysisJob, analyzedAt time.Time, publicMessage string,
) error {
	return s.writeAnalysisAttempt(
		ctx, job, "", analysis.SchemaVersion, analysis.PromptVersion,
		"failed", "", "", "", nil, nil, analyzedAt, publicMessage,
	)
}

func (s *Store) writeAnalysisAttempt(
	ctx context.Context,
	job AnalysisJob,
	revision, schemaVersion, promptVersion, status, provider, fallbackFrom, model string,
	requestBody, responseBody []byte,
	analyzedAt time.Time,
	publicMessage string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin analysis attempt persistence: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureAnalysisJobCurrentTx(ctx, tx, job); err != nil {
		return err
	}
	payloadID := ""
	if requestBody != nil || responseBody != nil {
		payloadID, err = newPayloadID()
		if err != nil {
			return err
		}
		if err := retainAnalysisPayload(ctx, tx, payloadID, requestBody, responseBody, analyzedAt); err != nil {
			return err
		}
	}
	if err := upsertAnalysisAttempt(
		ctx, tx, job, status, revision, provider, fallbackFrom, model, analyzedAt,
		payloadID, publicMessage, schemaVersion, promptVersion,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit analysis attempt persistence: %w", err)
	}
	return nil
}

func retainAnalysisPayload(
	ctx context.Context, tx *sql.Tx, payloadID string,
	requestBody, responseBody []byte, analyzedAt time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO retained_model_payloads (id, request_json, response_json, created_at)
		VALUES (?, ?, ?, ?)
	`, payloadID, string(requestBody), string(responseBody), formatTime(analyzedAt)); err != nil {
		return fmt.Errorf("retain model payload: %w", err)
	}
	return nil
}

func upsertAnalysisAttempt(
	ctx context.Context,
	tx *sql.Tx,
	job AnalysisJob,
	status, revision, provider, fallbackFrom, model string,
	analyzedAt time.Time,
	payloadID, publicMessage, schemaVersion, promptVersion string,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO analysis_attempt_state (
			collection_id, repository, number, status, input_revision,
			provider, fallback_from, model, attempted_at, payload_id, error,
			schema_version, prompt_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(collection_id, repository, number) DO UPDATE SET
			status = excluded.status,
			input_revision = excluded.input_revision,
			provider = excluded.provider,
			fallback_from = excluded.fallback_from,
			model = excluded.model,
			attempted_at = excluded.attempted_at,
			payload_id = excluded.payload_id,
			error = excluded.error,
			schema_version = excluded.schema_version,
			prompt_version = excluded.prompt_version
	`, job.CollectionID, job.Repository, job.Number, status, revision,
		provider, fallbackFrom, model, formatTime(analyzedAt), payloadID, publicMessage,
		schemaVersion, promptVersion); err != nil {
		return fmt.Errorf("persist analysis attempt: %w", err)
	}
	return nil
}

func newPayloadID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create retained payload identity: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func ensureAnalysisJobCurrentTx(ctx context.Context, tx *sql.Tx, job AnalysisJob) error {
	var currentHead string
	if err := tx.QueryRowContext(ctx, `
		SELECT head_sha FROM pull_requests WHERE repository = ? AND number = ?
	`, job.Repository, job.Number).Scan(&currentHead); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAnalysisSuperseded
		}
		return fmt.Errorf("verify analysis head: %w", err)
	}
	if currentHead != job.HeadSHA {
		return ErrAnalysisSuperseded
	}
	return nil
}

// RequeueAnalysisJob releases a transiently failed claim for another attempt.
func (s *Store) RequeueAnalysisJob(ctx context.Context, id int64, scheduledFor time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE analysis_jobs
		SET status = 'queued', started_at = NULL, scheduled_for = ?
		WHERE id = ? AND status = 'running'
	`, formatTime(scheduledFor), id); err != nil {
		return fmt.Errorf("requeue analysis job %d: %w", id, err)
	}
	return nil
}

// CompleteAnalysisJob releases active-job coalescing while retaining history.
func (s *Store) CompleteAnalysisJob(ctx context.Context, id int64, completedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin analysis completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var job AnalysisJob
	err = tx.QueryRowContext(ctx, `
		SELECT collection_id, repository, number, head_sha
		FROM analysis_jobs WHERE id = ?
	`, id).Scan(&job.CollectionID, &job.Repository, &job.Number, &job.HeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("load analysis job %d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE analysis_jobs SET status = 'completed', finished_at = ? WHERE id = ?
	`, formatTime(completedAt), id); err != nil {
		return fmt.Errorf("complete analysis job %d: %w", id, err)
	}
	var currentHead string
	if err := tx.QueryRowContext(ctx, `
		SELECT head_sha FROM pull_requests WHERE repository = ? AND number = ?
	`, job.Repository, job.Number).Scan(&currentHead); err == nil && currentHead != job.HeadSHA {
		var terminal int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM pull_request_diff_evidence
			WHERE repository = ? AND number = ? AND head_sha = ?
			  AND completeness IN ('complete', 'partial', 'unavailable')
		`, job.Repository, job.Number, currentHead).Scan(&terminal); err != nil {
			return err
		}
		if currentHead == "" || terminal > 0 {
			if err := enqueueAnalysisForPRTx(
				ctx, tx, job.CollectionID, job.Repository, job.Number, false, completedAt,
			); err != nil {
				return err
			}
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return tx.Commit()
}

// AnalysisQueueStatus exposes non-sensitive operational queue state.
type AnalysisQueueStatus struct {
	Queued     int        `json:"queued"`
	Running    int        `json:"running"`
	Oldest     *time.Time `json:"oldestQueuedAt,omitempty"`
	Failures   int        `json:"failures"`
	Stale      int        `json:"stale"`
	LastResult *time.Time `json:"lastResultAt,omitempty"`
}

func (s *Store) AnalysisStatus(ctx context.Context) (AnalysisQueueStatus, error) {
	var result AnalysisQueueStatus
	rows, err := s.db.QueryContext(ctx, `
		SELECT status, COUNT(*), MIN(scheduled_for)
		FROM analysis_jobs
		WHERE status IN ('queued', 'running')
		GROUP BY status
	`)
	if err != nil {
		return result, fmt.Errorf("load analysis queue status: %w", err)
	}
	for rows.Next() {
		var status string
		var count int
		var oldest sql.NullString
		if err := rows.Scan(&status, &count, &oldest); err != nil {
			_ = rows.Close()
			return result, err
		}
		if status == "queued" {
			result.Queued = count
			if oldest.Valid {
				value, parseErr := time.Parse(time.RFC3339Nano, oldest.String)
				if parseErr != nil {
					_ = rows.Close()
					return result, parseErr
				}
				result.Oldest = &value
			}
		} else {
			result.Running = count
		}
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM analysis_attempt_state WHERE status != 'complete'",
	).Scan(&result.Failures); err != nil {
		return result, err
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM analysis_jobs jobs
		JOIN analysis_results results
		  ON results.collection_id = jobs.collection_id
		 AND results.repository = jobs.repository
		 AND results.number = jobs.number
		WHERE jobs.status IN ('queued', 'running')
	`).Scan(&result.Stale); err != nil {
		return result, err
	}
	var latest sql.NullString
	if err := s.db.QueryRowContext(ctx,
		"SELECT MAX(attempted_at) FROM analysis_attempt_state",
	).Scan(&latest); err != nil {
		return result, err
	}
	if latest.Valid {
		value, err := time.Parse(time.RFC3339Nano, latest.String)
		if err != nil {
			return result, err
		}
		result.LastResult = &value
	}
	return result, nil
}

// KnownSourceIDs returns the source-ID allowlist from a bounded request.
func KnownSourceIDs(request analysis.Request) map[string]struct{} {
	ids := make(map[string]struct{}, len(request.Sources)+1)
	ids[request.PullRequest.SourceID] = struct{}{}
	for _, source := range request.Sources {
		ids[source.ID] = struct{}{}
	}
	return ids
}

// SortWaiting keeps the public set stable independently of model ordering.
func SortWaiting(states []analysis.WaitingState) {
	sort.Slice(states, func(i, j int) bool { return states[i].Party < states[j].Party })
}
