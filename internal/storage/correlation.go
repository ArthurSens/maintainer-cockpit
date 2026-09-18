package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/correlation"
)

var ErrCorrelationInputsChanged = errors.New("correlation inputs changed")

// CorrelationJob is one durable collection-level model request.
type CorrelationJob struct {
	ID               int64
	CollectionID     string
	PrimaryProvider  string
	FallbackProvider string
	Forced           bool
	ScheduledFor     time.Time
}

// CorrelationProvenance identifies the model operation behind a graph.
type CorrelationProvenance struct {
	Provider      string    `json:"provider"`
	FallbackFrom  string    `json:"fallbackFrom,omitempty"`
	Model         string    `json:"model"`
	InputRevision string    `json:"inputRevision"`
	SchemaVersion string    `json:"schemaVersion"`
	PromptVersion string    `json:"promptVersion"`
	CorrelatedAt  time.Time `json:"correlatedAt"`
	PayloadID     string    `json:"-"`
}

// CorrelationGroup is a public validated feature graph.
type CorrelationGroup struct {
	correlation.Group
	OpenMemberCount int                   `json:"openMemberCount"`
	TextGraph       string                `json:"textGraph"`
	Provenance      CorrelationProvenance `json:"provenance"`
}

type CorrelationGroupSummary struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	OpenMemberCount int       `json:"openMemberCount"`
	Confidence      string    `json:"confidence"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	CorrelatedAt    time.Time `json:"correlatedAt"`
}

// CorrelationStatus is public-safe health and freshness state.
type CorrelationStatus struct {
	State       string     `json:"state"`
	Error       string     `json:"error,omitempty"`
	LastAttempt *time.Time `json:"lastAttemptAt,omitempty"`
	LastSuccess *time.Time `json:"lastSuccessAt,omitempty"`
	Queued      int        `json:"queued"`
	Running     int        `json:"running"`
	Oldest      *time.Time `json:"oldestQueuedAt,omitempty"`
}

type CollectionCorrelationStatus struct {
	CollectionID string `json:"collectionID"`
	CorrelationStatus
}

type CorrelationQueueStatus struct {
	Queued     int        `json:"queued"`
	Running    int        `json:"running"`
	Failures   int        `json:"failures"`
	Oldest     *time.Time `json:"oldestQueuedAt,omitempty"`
	LastResult *time.Time `json:"lastResultAt,omitempty"`
}

func (s *Store) EnqueueCollectionCorrelation(
	ctx context.Context, collectionID string, forced bool, scheduledFor time.Time,
) (enqueued, coalesced int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin correlation enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var enabled bool
	if err := tx.QueryRowContext(ctx, `
		SELECT correlation_enabled FROM collections WHERE id = ? AND active = 1
	`, collectionID).Scan(&enabled); errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("%w: %q", ErrCollectionNotFound, collectionID)
	} else if err != nil {
		return 0, 0, err
	}
	if !enabled {
		return 0, 0, fmt.Errorf("collection %q does not enable feature correlation", collectionID)
	}
	var active int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM correlation_jobs
		WHERE collection_id = ? AND status IN ('queued', 'running')
	`, collectionID).Scan(&active); err != nil {
		return 0, 0, err
	}
	if active > 0 {
		coalesced = 1
		if forced {
			if _, err := tx.ExecContext(ctx, `
				UPDATE correlation_jobs SET forced = 1
				WHERE collection_id = ? AND status = 'queued'
			`, collectionID); err != nil {
				return 0, 0, err
			}
		}
	} else {
		enqueued = 1
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO correlation_jobs (collection_id, status, forced, scheduled_for)
			VALUES (?, 'queued', ?, ?)
		`, collectionID, forced, formatTime(scheduledFor)); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return enqueued, coalesced, nil
}

func (s *Store) RecoverCorrelationJobs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE correlation_jobs SET status = 'queued', started_at = NULL
		WHERE status = 'running'
	`)
	return err
}

func (s *Store) ClaimCorrelation(ctx context.Context, now time.Time) (*CorrelationJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var job CorrelationJob
	var scheduled string
	err = tx.QueryRowContext(ctx, `
		SELECT jobs.id, jobs.collection_id, jobs.forced, jobs.scheduled_for,
			collections.model_provider, collections.model_fallback_provider
		FROM correlation_jobs jobs
		JOIN collections ON collections.id = jobs.collection_id
		WHERE jobs.status = 'queued' AND jobs.scheduled_for <= ?
		  AND collections.active = 1 AND collections.correlation_enabled = 1
		  AND NOT EXISTS (
			SELECT 1
			FROM collection_repositories cr
			JOIN refresh_jobs refresh ON refresh.repository = cr.repository
			WHERE cr.collection_id = jobs.collection_id
			  AND refresh.status IN ('queued', 'running')
		  )
		ORDER BY jobs.scheduled_for, jobs.id LIMIT 1
	`, formatTime(now)).Scan(
		&job.ID, &job.CollectionID, &job.Forced, &scheduled,
		&job.PrimaryProvider, &job.FallbackProvider,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE correlation_jobs SET status = 'running', started_at = ?
		WHERE id = ? AND status = 'queued'
	`, formatTime(now), job.ID)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return nil, fmt.Errorf("correlation job %d was claimed concurrently", job.ID)
	}
	job.ScheduledFor, err = time.Parse(time.RFC3339Nano, scheduled)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Store) CompleteCorrelationJob(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE correlation_jobs SET status = 'completed', finished_at = ? WHERE id = ?
	`, formatTime(now), id)
	return err
}

// BuildCorrelationRequests loads only bounded public collection evidence.
func (s *Store) BuildCorrelationRequests(
	ctx context.Context, job CorrelationJob,
) ([]correlation.Request, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	requests, err := buildCorrelationRequests(ctx, tx, job)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return requests, nil
}

type correlationQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func buildCorrelationRequests(
	ctx context.Context, database correlationQueryer, job CorrelationJob,
) ([]correlation.Request, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT pr.repository, pr.number, pr.title, pr.author, pr.url
		FROM collection_pull_requests cpr
		JOIN pull_requests pr ON pr.repository = cpr.repository AND pr.number = cpr.number
		WHERE cpr.collection_id = ?
		ORDER BY pr.repository, pr.number
	`, job.CollectionID)
	if err != nil {
		return nil, err
	}
	var pullRequests []correlation.PullRequest
	for rows.Next() {
		var pr correlation.PullRequest
		if err := rows.Scan(&pr.Repository, &pr.Number, &pr.Title, &pr.Author, &pr.URL); err != nil {
			_ = rows.Close()
			return nil, err
		}
		pr.SourceID = correlation.PullRequestSourceID(pr.Repository, pr.Number)
		pr.State = "open"
		pullRequests = append(pullRequests, pr)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range pullRequests {
		pr := &pullRequests[index]
		fileRows, err := database.QueryContext(ctx, `
			SELECT path FROM pull_request_files
			WHERE repository = ? AND number = ? ORDER BY path
		`, pr.Repository, pr.Number)
		if err != nil {
			return nil, err
		}
		for fileRows.Next() {
			var file string
			if err := fileRows.Scan(&file); err != nil {
				_ = fileRows.Close()
				return nil, err
			}
			pr.Files = append(pr.Files, file)
		}
		_ = fileRows.Close()
		relationshipRows, err := database.QueryContext(ctx, `
			SELECT source_id, kind, source_repository, source_number, title, state, url
			FROM pull_request_relationships
			WHERE repository = ? AND number = ?
			  AND kind IN ('closing_issue', 'cross_reference', 'associated_pull_request')
			ORDER BY source_id
		`, pr.Repository, pr.Number)
		if err != nil {
			return nil, err
		}
		for relationshipRows.Next() {
			var source correlation.Source
			if err := relationshipRows.Scan(
				&source.ID, &source.Kind, &source.Repository, &source.Number,
				&source.Title, &source.State, &source.URL,
			); err != nil {
				_ = relationshipRows.Close()
				return nil, err
			}
			switch source.Kind {
			case "associated_pull_request":
				source.EntityID = correlation.EntitySourceID("pr", source.Repository, source.Number)
			case "closing_issue", "cross_reference":
				entityKind := "issue"
				if strings.Contains(source.URL, "/pull/") {
					entityKind = "pr"
				}
				source.EntityID = correlation.EntitySourceID(entityKind, source.Repository, source.Number)
			}
			pr.Relationships = append(pr.Relationships, source)
		}
		_ = relationshipRows.Close()
	}
	return correlation.BuildRequests(correlation.Input{
		CollectionID: job.CollectionID, PullRequests: pullRequests,
	})
}

func correlationInputRevision(requests []correlation.Request) string {
	revisions := make([]string, 0, len(requests))
	for _, request := range requests {
		revisions = append(revisions, request.InputRevision)
	}
	sort.Strings(revisions)
	sum := sha256.Sum256([]byte(strings.Join(revisions, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (s *Store) RecordCorrelationSuccess(
	ctx context.Context,
	job CorrelationJob,
	requests []correlation.Request,
	groups []correlation.Group,
	provider, fallbackFrom, model string,
	requestBody, responseBody []byte,
	correlatedAt time.Time,
) error {
	payloadID, err := newPayloadID()
	if err != nil {
		return err
	}
	provenance := CorrelationProvenance{
		Provider: provider, FallbackFrom: fallbackFrom, Model: model,
		InputRevision: correlationInputRevision(requests),
		SchemaVersion: correlation.SchemaVersion, PromptVersion: correlation.PromptVersion,
		CorrelatedAt: correlatedAt, PayloadID: payloadID,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	currentRequests, err := buildCorrelationRequests(ctx, tx, job)
	if err != nil {
		return err
	}
	if currentRevision := correlationInputRevision(currentRequests); currentRevision != provenance.InputRevision {
		return fmt.Errorf("%w: started at %s, current %s",
			ErrCorrelationInputsChanged, provenance.InputRevision, currentRevision)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO retained_model_payloads (id, request_json, response_json, created_at)
		VALUES (?, ?, ?, ?)
	`, payloadID, string(requestBody), string(responseBody), formatTime(correlatedAt)); err != nil {
		return err
	}
	if err := replaceCorrelationGroupsTx(ctx, tx, job.CollectionID, groups, provenance); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO correlation_state (
			collection_id, status, last_attempt_at, last_success_at, error,
			provider, model, input_revision, schema_version, prompt_version, payload_id
		) VALUES (?, 'complete', ?, ?, '', ?, ?, ?, ?, ?, ?)
		ON CONFLICT(collection_id) DO UPDATE SET
			status = 'complete', last_attempt_at = excluded.last_attempt_at,
			last_success_at = excluded.last_success_at, error = '',
			provider = excluded.provider, model = excluded.model,
			input_revision = excluded.input_revision,
			schema_version = excluded.schema_version,
			prompt_version = excluded.prompt_version, payload_id = excluded.payload_id
	`, job.CollectionID, formatTime(correlatedAt), formatTime(correlatedAt),
		provider, model, provenance.InputRevision, provenance.SchemaVersion,
		provenance.PromptVersion, payloadID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordCorrelationFailure(
	ctx context.Context,
	job CorrelationJob,
	provider, model string,
	requestBody, responseBody []byte,
	attemptedAt time.Time,
	publicMessage string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	payloadID := ""
	if len(requestBody) > 0 || len(responseBody) > 0 {
		payloadID, err = newPayloadID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO retained_model_payloads (id, request_json, response_json, created_at)
			VALUES (?, ?, ?, ?)
		`, payloadID, string(requestBody), string(responseBody), formatTime(attemptedAt)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO correlation_state (
			collection_id, status, last_attempt_at, error, provider, model,
			schema_version, prompt_version, payload_id
		) VALUES (?, 'failed', ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(collection_id) DO UPDATE SET
			status = 'failed', last_attempt_at = excluded.last_attempt_at,
			error = excluded.error, provider = excluded.provider, model = excluded.model,
			schema_version = excluded.schema_version, prompt_version = excluded.prompt_version,
			payload_id = excluded.payload_id
	`, job.CollectionID, formatTime(attemptedAt), publicMessage, provider, model,
		correlation.SchemaVersion, correlation.PromptVersion, payloadID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceCorrelationGroups is a test/admin seam for an already validated graph.
func (s *Store) ReplaceCorrelationGroups(
	ctx context.Context,
	collectionID string,
	groups []correlation.Group,
	provenance CorrelationProvenance,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := replaceCorrelationGroupsTx(ctx, tx, collectionID, groups, provenance); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceCorrelationGroupsTx(
	ctx context.Context,
	tx *sql.Tx,
	collectionID string,
	groups []correlation.Group,
	provenance CorrelationProvenance,
) error {
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM correlation_groups WHERE collection_id = ?", collectionID,
	); err != nil {
		return err
	}
	for _, group := range groups {
		groupJSON, err := json.Marshal(group)
		if err != nil {
			return err
		}
		sourceIDs, err := json.Marshal(group.SourceIDs)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO correlation_groups (
				collection_id, id, name, description, confidence, source_ids_json,
				group_json, text_graph, provider, fallback_from, model, input_revision,
				schema_version, prompt_version, correlated_at, payload_id
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, collectionID, group.ID, group.Name, group.Description, group.Confidence,
			string(sourceIDs), string(groupJSON), correlation.TextGraph(group),
			provenance.Provider, provenance.FallbackFrom, provenance.Model,
			provenance.InputRevision, provenance.SchemaVersion, provenance.PromptVersion,
			formatTime(provenance.CorrelatedAt), provenance.PayloadID); err != nil {
			return err
		}
		for _, member := range group.Members {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO correlation_group_members (
					collection_id, group_id, source_id, repository, number, state
				) VALUES (?, ?, ?, ?, ?, ?)
			`, collectionID, group.ID, member.SourceID, member.Repository,
				member.Number, member.State); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) ListCorrelationGroups(
	ctx context.Context, collectionID string,
) ([]CorrelationGroup, error) {
	var active bool
	if err := s.db.QueryRowContext(ctx,
		"SELECT active FROM collections WHERE id = ?", collectionID,
	).Scan(&active); errors.Is(err, sql.ErrNoRows) || err == nil && !active {
		return nil, ErrCollectionNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT group_json, text_graph, provider, fallback_from, model, input_revision,
			schema_version, prompt_version, correlated_at, payload_id
		FROM correlation_groups WHERE collection_id = ? ORDER BY name, id
	`, collectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]CorrelationGroup, 0)
	for rows.Next() {
		var group CorrelationGroup
		var groupJSON, correlatedAt string
		if err := rows.Scan(
			&groupJSON, &group.TextGraph, &group.Provenance.Provider,
			&group.Provenance.FallbackFrom, &group.Provenance.Model,
			&group.Provenance.InputRevision, &group.Provenance.SchemaVersion,
			&group.Provenance.PromptVersion, &correlatedAt, &group.Provenance.PayloadID,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(groupJSON), &group.Group); err != nil {
			return nil, err
		}
		group.Provenance.CorrelatedAt, err = time.Parse(time.RFC3339Nano, correlatedAt)
		if err != nil {
			return nil, err
		}
		for _, member := range group.Members {
			if member.State == "open" {
				group.OpenMemberCount++
			}
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Store) GetCorrelationGroup(
	ctx context.Context, collectionID, groupID string,
) (*CorrelationGroup, error) {
	groups, err := s.ListCorrelationGroups(ctx, collectionID)
	if err != nil {
		return nil, err
	}
	for index := range groups {
		if groups[index].ID == groupID {
			return &groups[index], nil
		}
	}
	return nil, nil
}

func (s *Store) correlationGroupsForPullRequest(
	ctx context.Context, collectionID, repository string, number int,
) ([]CorrelationGroupSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT groups.id, groups.name, groups.description, groups.confidence,
			groups.provider, groups.model, groups.correlated_at,
			SUM(CASE WHEN members.state = 'open' THEN 1 ELSE 0 END)
		FROM correlation_groups groups
		JOIN correlation_group_members target
		  ON target.collection_id = groups.collection_id AND target.group_id = groups.id
		JOIN correlation_group_members members
		  ON members.collection_id = groups.collection_id AND members.group_id = groups.id
		WHERE target.collection_id = ? AND target.repository = ? AND target.number = ?
		GROUP BY groups.id, groups.name, groups.description, groups.confidence,
			groups.provider, groups.model, groups.correlated_at
		ORDER BY groups.name, groups.id
	`, collectionID, repository, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []CorrelationGroupSummary
	for rows.Next() {
		var group CorrelationGroupSummary
		var correlatedAt string
		if err := rows.Scan(
			&group.ID, &group.Name, &group.Description, &group.Confidence,
			&group.Provider, &group.Model, &correlatedAt, &group.OpenMemberCount,
		); err != nil {
			return nil, err
		}
		group.CorrelatedAt, err = time.Parse(time.RFC3339Nano, correlatedAt)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Store) CorrelationStatus(
	ctx context.Context, collectionID string,
) (CorrelationStatus, error) {
	result := CorrelationStatus{State: "never"}
	var lastAttempt, lastSuccess sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT status, error, last_attempt_at, last_success_at
		FROM correlation_state WHERE collection_id = ?
	`, collectionID).Scan(&result.State, &result.Error, &lastAttempt, &lastSuccess)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if lastAttempt.Valid {
		value, err := time.Parse(time.RFC3339Nano, lastAttempt.String)
		if err != nil {
			return result, err
		}
		result.LastAttempt = &value
	}
	if lastSuccess.Valid {
		value, err := time.Parse(time.RFC3339Nano, lastSuccess.String)
		if err != nil {
			return result, err
		}
		result.LastSuccess = &value
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT status, COUNT(*), MIN(scheduled_for)
		FROM correlation_jobs
		WHERE collection_id = ? AND status IN ('queued', 'running')
		GROUP BY status
	`, collectionID)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		var oldest sql.NullString
		if err := rows.Scan(&status, &count, &oldest); err != nil {
			return result, err
		}
		if status == "queued" {
			result.Queued = count
			if oldest.Valid {
				value, err := time.Parse(time.RFC3339Nano, oldest.String)
				if err != nil {
					return result, err
				}
				result.Oldest = &value
			}
		} else {
			result.Running = count
		}
	}
	return result, rows.Err()
}

func (s *Store) ListCorrelationStatuses(
	ctx context.Context,
) ([]CollectionCorrelationStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT collections.id
		FROM collections
		WHERE collections.active = 1 AND collections.correlation_enabled = 1
		ORDER BY collections.id
	`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	result := make([]CollectionCorrelationStatus, 0, len(ids))
	for _, id := range ids {
		status, err := s.CorrelationStatus(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, CollectionCorrelationStatus{
			CollectionID: id, CorrelationStatus: status,
		})
	}
	return result, nil
}

func (s *Store) CorrelationQueueStatus(ctx context.Context) (CorrelationQueueStatus, error) {
	statuses, err := s.ListCorrelationStatuses(ctx)
	if err != nil {
		return CorrelationQueueStatus{}, err
	}
	var result CorrelationQueueStatus
	for _, status := range statuses {
		result.Queued += status.Queued
		result.Running += status.Running
		if status.State == "failed" || status.State == "unknown" {
			result.Failures++
		}
		if status.Oldest != nil && (result.Oldest == nil || status.Oldest.Before(*result.Oldest)) {
			value := *status.Oldest
			result.Oldest = &value
		}
		if status.LastSuccess != nil &&
			(result.LastResult == nil || status.LastSuccess.After(*result.LastResult)) {
			value := *status.LastSuccess
			result.LastResult = &value
		}
	}
	return result, nil
}
