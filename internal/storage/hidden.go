package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	listquery "github.com/ArthurSens/maintainer-cockpit/internal/query"
)

const maxHiddenReasonRunes = 500

var ErrHiddenPullRequestNotFound = errors.New("hidden pull request not found")

// HiddenPullRequestState is one maintainer's private collection-specific state.
type HiddenPullRequestState struct {
	Kind           string     `json:"kind"`
	Reason         string     `json:"reason,omitempty"`
	SnoozedUntil   *time.Time `json:"snoozedUntil,omitempty"`
	WakeOnActivity bool       `json:"wakeOnActivity,omitempty"`
	NewActivity    bool       `json:"newActivity,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// SetPullRequestHidden creates or edits one private snooze or ignore.
func (s *Store) SetPullRequestHidden(
	ctx context.Context,
	userID int64,
	collectionID, repository string,
	number int,
	state HiddenPullRequestState,
	now time.Time,
) (HiddenPullRequestState, error) {
	if userID <= 0 {
		return HiddenPullRequestState{}, errors.New("user ID must be positive")
	}
	state.Reason = strings.TrimSpace(state.Reason)
	if !utf8.ValidString(state.Reason) || utf8.RuneCountInString(state.Reason) > maxHiddenReasonRunes {
		return HiddenPullRequestState{}, fmt.Errorf("reason must not exceed %d characters", maxHiddenReasonRunes)
	}
	switch state.Kind {
	case "snoozed":
		if state.SnoozedUntil == nil && !state.WakeOnActivity {
			return HiddenPullRequestState{}, errors.New("snooze requires a date and/or next activity")
		}
		if state.SnoozedUntil != nil {
			until := state.SnoozedUntil.UTC()
			if !until.After(now.UTC()) {
				return HiddenPullRequestState{}, errors.New("snooze date must be in the future")
			}
			state.SnoozedUntil = &until
		}
	case "ignored":
		state.SnoozedUntil = nil
		state.WakeOnActivity = false
	default:
		return HiddenPullRequestState{}, errors.New("kind must be snoozed or ignored")
	}

	var pullRequestUpdatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT pr.updated_at
		FROM pull_requests pr
		JOIN collection_pull_requests cpr
			ON cpr.repository = pr.repository AND cpr.number = pr.number
		WHERE cpr.collection_id = ? AND pr.repository = ? AND pr.number = ?
	`, collectionID, repository, number).Scan(&pullRequestUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return HiddenPullRequestState{}, ErrPullRequestNotFound
	}
	if err != nil {
		return HiddenPullRequestState{}, fmt.Errorf("find pull request to hide: %w", err)
	}
	var snoozedUntil any
	if state.SnoozedUntil != nil {
		snoozedUntil = formatTime(*state.SnoozedUntil)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO hidden_pull_requests (
			user_id, collection_id, repository, number, kind, reason,
			snoozed_until, wake_on_activity, hidden_at_updated_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, collection_id, repository, number) DO UPDATE SET
			kind = excluded.kind,
			reason = excluded.reason,
			snoozed_until = excluded.snoozed_until,
			wake_on_activity = excluded.wake_on_activity,
			hidden_at_updated_at = excluded.hidden_at_updated_at,
			updated_at = excluded.updated_at
	`, userID, collectionID, repository, number, state.Kind, state.Reason,
		snoozedUntil, state.WakeOnActivity, pullRequestUpdatedAt,
		formatTime(now), formatTime(now)); err != nil {
		return HiddenPullRequestState{}, fmt.Errorf("hide pull request: %w", err)
	}
	return s.hiddenPullRequestState(ctx, userID, collectionID, repository, number)
}

// RestorePullRequest removes one private snooze or ignore.
func (s *Store) RestorePullRequest(
	ctx context.Context,
	userID int64,
	collectionID, repository string,
	number int,
) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM hidden_pull_requests
		WHERE user_id = ? AND collection_id = ? AND repository = ? AND number = ?
	`, userID, collectionID, repository, number)
	if err != nil {
		return fmt.Errorf("restore pull request: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect pull request restoration: %w", err)
	}
	if affected == 0 {
		return ErrHiddenPullRequestNotFound
	}
	return nil
}

// AttachHiddenPullRequests adds active state belonging only to the requesting user.
func (s *Store) AttachHiddenPullRequests(
	ctx context.Context,
	userID int64,
	collectionID string,
	pullRequests []PullRequest,
	now time.Time,
) error {
	if err := s.wakeSnoozedPullRequests(ctx, userID, collectionID, now); err != nil {
		return err
	}
	return s.attachHiddenPullRequests(ctx, userID, collectionID, pullRequests)
}

func (s *Store) attachHiddenPullRequests(
	ctx context.Context,
	userID int64,
	collectionID string,
	pullRequests []PullRequest,
) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT hidden.repository, hidden.number, hidden.kind, hidden.reason,
			hidden.snoozed_until, hidden.wake_on_activity,
			julianday(hidden.hidden_at_updated_at) < julianday(pr.updated_at),
			hidden.created_at, hidden.updated_at
		FROM hidden_pull_requests hidden
		JOIN pull_requests pr
			ON pr.repository = hidden.repository AND pr.number = hidden.number
		WHERE hidden.user_id = ? AND hidden.collection_id = ?
	`, userID, collectionID)
	if err != nil {
		return fmt.Errorf("list hidden pull requests: %w", err)
	}
	defer rows.Close()
	states := make(map[string]HiddenPullRequestState)
	for rows.Next() {
		var repository, snoozedUntil, createdAt, updatedAt string
		var number int
		var state HiddenPullRequestState
		var nullableUntil sql.NullString
		if err := rows.Scan(
			&repository, &number, &state.Kind, &state.Reason,
			&nullableUntil, &state.WakeOnActivity, &state.NewActivity,
			&createdAt, &updatedAt,
		); err != nil {
			return fmt.Errorf("scan hidden pull request: %w", err)
		}
		if nullableUntil.Valid {
			snoozedUntil = nullableUntil.String
			parsed, err := time.Parse(time.RFC3339Nano, snoozedUntil)
			if err != nil {
				return fmt.Errorf("parse snooze date: %w", err)
			}
			state.SnoozedUntil = &parsed
		}
		var err error
		state.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return fmt.Errorf("parse hidden-state creation: %w", err)
		}
		state.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return fmt.Errorf("parse hidden-state update: %w", err)
		}
		states[importantKey(repository, number)] = state
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate hidden pull requests: %w", err)
	}
	for index := range pullRequests {
		state, exists := states[importantKey(pullRequests[index].Repository, pullRequests[index].Number)]
		if !exists {
			continue
		}
		if pullRequests[index].Personal == nil {
			pullRequests[index].Personal = &PersonalPullRequestState{}
		}
		pullRequests[index].Personal.Hidden = &state
	}
	return nil
}

// AttachHiddenPullRequest adds active private state to a direct detail response.
func (s *Store) AttachHiddenPullRequest(
	ctx context.Context,
	userID int64,
	collectionID string,
	detail *PullRequestDetail,
	now time.Time,
) error {
	pullRequests := []PullRequest{detail.PullRequest}
	if err := s.AttachHiddenPullRequests(ctx, userID, collectionID, pullRequests, now); err != nil {
		return err
	}
	detail.Personal = pullRequests[0].Personal
	return nil
}

// ListPersonalPullRequestsPage applies owner-only visibility before pagination.
func (s *Store) ListPersonalPullRequestsPage(
	ctx context.Context,
	collectionID string,
	userID int64,
	options listquery.Options,
	now time.Time,
) (PullRequestPage, error) {
	if err := s.wakeSnoozedPullRequests(ctx, userID, collectionID, now); err != nil {
		return PullRequestPage{}, err
	}
	page, err := s.listPullRequestsPage(ctx, collectionID, options, &personalListQuery{
		userID: userID,
		view:   options.View,
	})
	if err != nil {
		return PullRequestPage{}, err
	}
	if err := s.AttachImportantPullRequests(
		ctx, userID, collectionID, page.PullRequests,
	); err != nil {
		return PullRequestPage{}, err
	}
	if err := s.attachHiddenPullRequests(
		ctx, userID, collectionID, page.PullRequests,
	); err != nil {
		return PullRequestPage{}, err
	}
	return page, nil
}

func (s *Store) wakeSnoozedPullRequests(
	ctx context.Context, userID int64, collectionID string, now time.Time,
) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM hidden_pull_requests
		WHERE user_id = ? AND collection_id = ? AND kind = 'snoozed'
		  AND (
			(snoozed_until IS NOT NULL AND julianday(snoozed_until) <= julianday(?))
			OR (
				wake_on_activity = 1 AND EXISTS (
					SELECT 1 FROM pull_requests pr
					WHERE pr.repository = hidden_pull_requests.repository
					  AND pr.number = hidden_pull_requests.number
					  AND julianday(pr.updated_at) >
						julianday(hidden_pull_requests.hidden_at_updated_at)
				)
			)
		  )
	`, userID, collectionID, formatTime(now)); err != nil {
		return fmt.Errorf("wake snoozed pull requests: %w", err)
	}
	return nil
}

func (s *Store) hiddenPullRequestState(
	ctx context.Context, userID int64, collectionID, repository string, number int,
) (HiddenPullRequestState, error) {
	var state HiddenPullRequestState
	var snoozedUntil sql.NullString
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT kind, reason, snoozed_until, wake_on_activity, created_at, updated_at
		FROM hidden_pull_requests
		WHERE user_id = ? AND collection_id = ? AND repository = ? AND number = ?
	`, userID, collectionID, repository, number).Scan(
		&state.Kind, &state.Reason, &snoozedUntil, &state.WakeOnActivity,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return HiddenPullRequestState{}, fmt.Errorf("load hidden pull request: %w", err)
	}
	if snoozedUntil.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, snoozedUntil.String)
		if err != nil {
			return HiddenPullRequestState{}, fmt.Errorf("parse snooze date: %w", err)
		}
		state.SnoozedUntil = &parsed
	}
	state.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return HiddenPullRequestState{}, fmt.Errorf("parse hidden-state creation: %w", err)
	}
	state.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return HiddenPullRequestState{}, fmt.Errorf("parse hidden-state update: %w", err)
	}
	return state, nil
}
