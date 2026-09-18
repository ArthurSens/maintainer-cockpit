package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // Embed the timezone database for deterministic contribution windows.
)

const (
	QualifyingActivityReview         = "review"
	QualifyingActivityComment        = "comment"
	QualifyingActivityThreadResolved = "thread_resolved"
	QualifyingActivityMerge          = "merge"
	QualifyingActivityClose          = "close"
)

var defaultQualifyingActivities = []string{
	QualifyingActivityReview,
	QualifyingActivityComment,
	QualifyingActivityThreadResolved,
	QualifyingActivityMerge,
	QualifyingActivityClose,
}

// ProgressEvent is one qualifying activity that may advance a maintainer's private goal.
type ProgressEvent struct {
	ID             string
	ActorID        int64
	ActivityType   string
	Repository     string
	Number         int
	Title          string
	PullRequestURL string
	ActivityURL    string
	OccurredAt     time.Time
	CollectionIDs  []string
}

// ReviewThreadState is one collected thread observation used to count only
// transitions from unresolved to resolved.
type ReviewThreadState struct {
	ID             string
	Resolved       bool
	ResolvedBy     int64
	Repository     string
	Number         int
	Title          string
	PullRequestURL string
	ActivityURL    string
	CollectionIDs  []string
}

// GoalPreferences controls one maintainer's private goal in one collection.
type GoalPreferences struct {
	Target            int      `json:"target"`
	EnabledActivities []string `json:"enabledActivities"`
	Timezone          string   `json:"timezone"`
	Configured        bool     `json:"configured"`
}

// QualifyingActivity is one counted activity shown in the private Today view.
type QualifyingActivity struct {
	Type       string    `json:"type"`
	URL        string    `json:"url"`
	OccurredAt time.Time `json:"occurredAt"`
}

// ProgressPullRequest groups all of today's qualifying activities for one PR.
type ProgressPullRequest struct {
	Repository string               `json:"repository"`
	Number     int                  `json:"number"`
	Title      string               `json:"title"`
	URL        string               `json:"url"`
	Activities []QualifyingActivity `json:"activities"`
}

// DailyProgress is one maintainer's private, timezone-aware collection progress.
type DailyProgress struct {
	GoalPreferences
	Date               string                `json:"date"`
	Count              int                   `json:"count"`
	LastCollectionTime *time.Time            `json:"lastCollectionTime,omitempty"`
	PullRequests       []ProgressPullRequest `json:"pullRequests"`
}

// RepositoryProgressCollectedAt returns the last durable progress checkpoint.
func (s *Store) RepositoryProgressCollectedAt(
	ctx context.Context,
	repository string,
) (time.Time, error) {
	var collected sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT last_collected_at
		FROM repository_progress_refreshes
		WHERE repository = ?
	`, repository).Scan(&collected); errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	} else if err != nil {
		return time.Time{}, fmt.Errorf("load progress collection time for %q: %w", repository, err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, collected.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse progress collection time for %q: %w", repository, err)
	}
	return parsed, nil
}

// RecordProgressEvents persists attributed GitHub activity from one successful
// repository collection and prunes progress history older than three months.
func (s *Store) RecordProgressEvents(
	ctx context.Context,
	repository string,
	events []ProgressEvent,
	collectedAt time.Time,
) error {
	return s.RecordProgressSnapshot(ctx, repository, events, nil, collectedAt)
}

// RecordProgressSnapshot persists events and review-thread transitions from
// one successful repository collection.
func (s *Store) RecordProgressSnapshot(
	ctx context.Context,
	repository string,
	events []ProgressEvent,
	threads []ReviewThreadState,
	collectedAt time.Time,
) error {
	if repository == "" || collectedAt.IsZero() {
		return errors.New("progress collection has missing repository or collection time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin progress event collection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var configured int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM collection_repositories WHERE repository = ?
	`, repository).Scan(&configured); err != nil {
		return fmt.Errorf("find progress repository: %w", err)
	}
	if configured == 0 {
		return fmt.Errorf("%w: %q", ErrRepositoryNotConfigured, repository)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO repository_progress_refreshes (repository, last_collected_at)
		VALUES (?, ?)
		ON CONFLICT(repository) DO UPDATE SET last_collected_at = excluded.last_collected_at
	`, repository, formatTime(collectedAt)); err != nil {
		return fmt.Errorf("record progress collection time: %w", err)
	}
	for _, event := range events {
		if err := validateProgressEvent(event, repository); err != nil {
			return err
		}
		collectionIDs := event.CollectionIDs
		if len(collectionIDs) == 0 {
			rows, queryErr := tx.QueryContext(ctx, `
				SELECT collection_id FROM collection_repositories WHERE repository = ?
			`, repository)
			if queryErr != nil {
				return fmt.Errorf("list progress collections: %w", queryErr)
			}
			for rows.Next() {
				var collectionID string
				if scanErr := rows.Scan(&collectionID); scanErr != nil {
					_ = rows.Close()
					return fmt.Errorf("scan progress collection: %w", scanErr)
				}
				collectionIDs = append(collectionIDs, collectionID)
			}
			if closeErr := rows.Close(); closeErr != nil {
				return closeErr
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				return rowsErr
			}
		}
		for _, collectionID := range collectionIDs {
			result, insertErr := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO progress_events (
					collection_id, repository, number, event_id, actor_id, activity_type,
					title, pull_request_url, activity_url, occurred_at, collected_at
				)
				SELECT collection_id, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
				FROM collection_repositories
				WHERE collection_id = ? AND repository = ?
			`, repository, event.Number, event.ID, event.ActorID, event.ActivityType,
				event.Title, event.PullRequestURL, event.ActivityURL,
				formatTime(event.OccurredAt), formatTime(collectedAt),
				collectionID, repository)
			if insertErr != nil {
				return fmt.Errorf("record progress event %q: %w", event.ID, insertErr)
			}
			if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
				return fmt.Errorf("inspect progress event %q: %w", event.ID, rowsErr)
			} else if affected == 0 {
				var exists int
				if queryErr := tx.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM progress_events
					WHERE collection_id = ? AND repository = ? AND number = ?
						AND event_id = ? AND activity_type = ?
				`, collectionID, repository, event.Number, event.ID, event.ActivityType).Scan(&exists); queryErr != nil {
					return fmt.Errorf("find progress event %q: %w", event.ID, queryErr)
				}
				if exists == 0 {
					return fmt.Errorf("collection %q does not include repository %q", collectionID, repository)
				}
			}
		}
	}
	for _, thread := range threads {
		if thread.ID == "" || thread.Repository != repository || thread.Number <= 0 ||
			thread.Title == "" || thread.PullRequestURL == "" || thread.ActivityURL == "" ||
			(thread.Resolved && thread.ResolvedBy <= 0) {
			return errors.New("review thread state has missing or inconsistent required fields")
		}
		collectionIDs := thread.CollectionIDs
		if len(collectionIDs) == 0 {
			rows, queryErr := tx.QueryContext(ctx, `
				SELECT collection_id FROM collection_repositories WHERE repository = ?
			`, repository)
			if queryErr != nil {
				return fmt.Errorf("list review thread collections: %w", queryErr)
			}
			for rows.Next() {
				var collectionID string
				if scanErr := rows.Scan(&collectionID); scanErr != nil {
					_ = rows.Close()
					return fmt.Errorf("scan review thread collection: %w", scanErr)
				}
				collectionIDs = append(collectionIDs, collectionID)
			}
			if closeErr := rows.Close(); closeErr != nil {
				return closeErr
			}
		}
		for _, collectionID := range collectionIDs {
			var wasResolved bool
			err := tx.QueryRowContext(ctx, `
				SELECT resolved FROM progress_review_threads
				WHERE collection_id = ? AND repository = ? AND number = ? AND thread_id = ?
			`, collectionID, repository, thread.Number, thread.ID).Scan(&wasResolved)
			firstObservation := errors.Is(err, sql.ErrNoRows)
			if err != nil && !firstObservation {
				return fmt.Errorf("load review thread state: %w", err)
			}
			if !firstObservation && !wasResolved && thread.Resolved {
				event := ProgressEvent{
					ID:      thread.ID + "@" + formatTime(collectedAt),
					ActorID: thread.ResolvedBy, ActivityType: QualifyingActivityThreadResolved,
					Repository: repository, Number: thread.Number, Title: thread.Title,
					PullRequestURL: thread.PullRequestURL, ActivityURL: thread.ActivityURL,
					OccurredAt: collectedAt, CollectionIDs: []string{collectionID},
				}
				if err := insertProgressEvent(ctx, tx, event, collectionID, collectedAt); err != nil {
					return err
				}
			}
			result, upsertErr := tx.ExecContext(ctx, `
				INSERT INTO progress_review_threads (
					collection_id, repository, number, thread_id, resolved, resolved_by,
					title, pull_request_url, activity_url, observed_at
				)
				SELECT collection_id, ?, ?, ?, ?, ?, ?, ?, ?, ?
				FROM collection_repositories
				WHERE collection_id = ? AND repository = ?
				ON CONFLICT(collection_id, repository, number, thread_id) DO UPDATE SET
					resolved = excluded.resolved,
					resolved_by = excluded.resolved_by,
					title = excluded.title,
					pull_request_url = excluded.pull_request_url,
					activity_url = excluded.activity_url,
					observed_at = excluded.observed_at
			`, repository, thread.Number, thread.ID, thread.Resolved, thread.ResolvedBy,
				thread.Title, thread.PullRequestURL, thread.ActivityURL, formatTime(collectedAt),
				collectionID, repository)
			if upsertErr != nil {
				return fmt.Errorf("record review thread state %q: %w", thread.ID, upsertErr)
			}
			if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
				return fmt.Errorf("inspect review thread state %q: %w", thread.ID, rowsErr)
			} else if affected == 0 {
				return fmt.Errorf("collection %q does not include repository %q", collectionID, repository)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM progress_events WHERE occurred_at < ?
	`, formatTime(collectedAt.AddDate(0, -3, 0))); err != nil {
		return fmt.Errorf("prune progress history: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM progress_review_threads WHERE observed_at < ?
	`, formatTime(collectedAt.AddDate(0, -3, 0))); err != nil {
		return fmt.Errorf("prune review thread observations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit progress event collection: %w", err)
	}
	return nil
}

func insertProgressEvent(
	ctx context.Context,
	tx *sql.Tx,
	event ProgressEvent,
	collectionID string,
	collectedAt time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO progress_events (
			collection_id, repository, number, event_id, actor_id, activity_type,
			title, pull_request_url, activity_url, occurred_at, collected_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, collectionID, event.Repository, event.Number, event.ID, event.ActorID, event.ActivityType,
		event.Title, event.PullRequestURL, event.ActivityURL,
		formatTime(event.OccurredAt), formatTime(collectedAt)); err != nil {
		return fmt.Errorf("record progress event %q: %w", event.ID, err)
	}
	return nil
}

func validateProgressEvent(event ProgressEvent, repository string) error {
	if event.ID == "" || event.ActorID <= 0 || event.Repository != repository ||
		event.Number <= 0 || event.Title == "" || event.PullRequestURL == "" ||
		event.ActivityURL == "" || event.OccurredAt.IsZero() {
		return errors.New("progress event has missing or inconsistent required fields")
	}
	if !slices.Contains(defaultQualifyingActivities, event.ActivityType) {
		return fmt.Errorf("unsupported qualifying activity %q", event.ActivityType)
	}
	return nil
}

// SetGoalPreferences validates and persists one private collection goal.
func (s *Store) SetGoalPreferences(
	ctx context.Context,
	userID int64,
	collectionID string,
	preferences GoalPreferences,
) error {
	if userID <= 0 || preferences.Target < 1 || preferences.Target > 1000 {
		return errors.New("target must be between 1 and 1000")
	}
	activities, err := normalizeQualifyingActivities(preferences.EnabledActivities)
	if err != nil {
		return err
	}
	if _, err := time.LoadLocation(preferences.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q", preferences.Timezone)
	}
	encoded, err := json.Marshal(activities)
	if err != nil {
		return fmt.Errorf("encode qualifying activities: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO goal_preferences (
			user_id, collection_id, target, enabled_activities_json, timezone
		)
		SELECT ?, id, ?, ?, ? FROM collections WHERE id = ?
		ON CONFLICT(user_id, collection_id) DO UPDATE SET
			target = excluded.target,
			enabled_activities_json = excluded.enabled_activities_json,
			timezone = excluded.timezone
	`, userID, preferences.Target, string(encoded), preferences.Timezone, collectionID)
	if err != nil {
		return fmt.Errorf("set goal preferences: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("inspect goal preferences write: %w", err)
	} else if affected == 0 {
		return ErrCollectionNotFound
	}
	return nil
}

func normalizeQualifyingActivities(activities []string) ([]string, error) {
	seen := make(map[string]struct{}, len(activities))
	for _, activity := range activities {
		if !slices.Contains(defaultQualifyingActivities, activity) {
			return nil, fmt.Errorf("unsupported qualifying activity %q", activity)
		}
		seen[activity] = struct{}{}
	}
	if len(seen) == 0 {
		return nil, errors.New("at least one qualifying activity must be enabled")
	}
	result := make([]string, 0, len(seen))
	for _, activity := range defaultQualifyingActivities {
		if _, exists := seen[activity]; exists {
			result = append(result, activity)
		}
	}
	return result, nil
}

func (s *Store) goalPreferences(
	ctx context.Context,
	userID int64,
	collectionID string,
) (GoalPreferences, error) {
	preferences := GoalPreferences{
		Target: 10, EnabledActivities: append([]string(nil), defaultQualifyingActivities...),
		Timezone: "UTC",
	}
	var encoded string
	err := s.db.QueryRowContext(ctx, `
		SELECT target, enabled_activities_json, timezone
		FROM goal_preferences WHERE user_id = ? AND collection_id = ?
	`, userID, collectionID).Scan(&preferences.Target, &encoded, &preferences.Timezone)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if collectionErr := s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM collections WHERE id = ?", collectionID,
		).Scan(&exists); collectionErr != nil {
			return GoalPreferences{}, fmt.Errorf("find collection goal: %w", collectionErr)
		}
		if exists == 0 {
			return GoalPreferences{}, ErrCollectionNotFound
		}
		return preferences, nil
	}
	if err != nil {
		return GoalPreferences{}, fmt.Errorf("load goal preferences: %w", err)
	}
	if err := json.Unmarshal([]byte(encoded), &preferences.EnabledActivities); err != nil {
		return GoalPreferences{}, fmt.Errorf("decode qualifying activities: %w", err)
	}
	preferences.Configured = true
	return preferences, nil
}

// GetDailyProgress returns today's private progress in the maintainer's timezone.
func (s *Store) GetDailyProgress(
	ctx context.Context,
	userID int64,
	collectionID string,
	now time.Time,
) (DailyProgress, error) {
	preferences, err := s.goalPreferences(ctx, userID, collectionID)
	if err != nil {
		return DailyProgress{}, err
	}
	location, err := time.LoadLocation(preferences.Timezone)
	if err != nil {
		return DailyProgress{}, fmt.Errorf("load goal timezone: %w", err)
	}
	localNow := now.In(location)
	start := time.Date(
		localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location,
	)
	end := start.AddDate(0, 0, 1)
	progress := DailyProgress{
		GoalPreferences: preferences,
		Date:            start.Format("2006-01-02"),
		PullRequests:    make([]ProgressPullRequest, 0),
	}
	var lastCollection sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT MIN(rpr.last_collected_at)
		FROM repository_progress_refreshes rpr
		JOIN collection_repositories cr ON cr.repository = rpr.repository
		WHERE cr.collection_id = ?
	`, collectionID).Scan(&lastCollection); err != nil {
		return DailyProgress{}, fmt.Errorf("load last progress collection time: %w", err)
	}
	if lastCollection.Valid {
		parsed, parseErr := time.Parse(time.RFC3339Nano, lastCollection.String)
		if parseErr != nil {
			return DailyProgress{}, fmt.Errorf("parse progress collection time: %w", parseErr)
		}
		progress.LastCollectionTime = &parsed
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(preferences.EnabledActivities)), ",")
	args := []any{userID, collectionID, formatTime(start), formatTime(end)}
	for _, activity := range preferences.EnabledActivities {
		args = append(args, activity)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT repository, number, title, pull_request_url, activity_type, activity_url, occurred_at
		FROM progress_events
		WHERE actor_id = ? AND collection_id = ?
			AND occurred_at >= ? AND occurred_at < ?
			AND activity_type IN (`+placeholders+`)
		ORDER BY occurred_at, repository, number, activity_type
	`, args...)
	if err != nil {
		return DailyProgress{}, fmt.Errorf("list daily progress: %w", err)
	}
	defer rows.Close()
	indexes := make(map[string]int)
	for rows.Next() {
		var repository, title, pullRequestURL, activityType, activityURL, occurredAt string
		var number int
		if err := rows.Scan(
			&repository, &number, &title, &pullRequestURL, &activityType, &activityURL, &occurredAt,
		); err != nil {
			return DailyProgress{}, fmt.Errorf("scan daily progress: %w", err)
		}
		timestamp, err := time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			return DailyProgress{}, fmt.Errorf("parse progress event time: %w", err)
		}
		key := importantKey(repository, number)
		index, exists := indexes[key]
		if !exists {
			index = len(progress.PullRequests)
			indexes[key] = index
			progress.PullRequests = append(progress.PullRequests, ProgressPullRequest{
				Repository: repository, Number: number, Title: title, URL: pullRequestURL,
				Activities: make([]QualifyingActivity, 0),
			})
		}
		progress.PullRequests[index].Activities = append(
			progress.PullRequests[index].Activities,
			QualifyingActivity{
				Type: activityType, URL: activityURL, OccurredAt: timestamp,
			},
		)
	}
	if err := rows.Err(); err != nil {
		return DailyProgress{}, fmt.Errorf("iterate daily progress: %w", err)
	}
	progress.Count = len(progress.PullRequests)
	return progress, nil
}
