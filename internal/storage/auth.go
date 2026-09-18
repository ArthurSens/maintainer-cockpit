package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Session is opaque browser authentication state stored only on the server.
type Session struct {
	ID            string
	UserID        int64
	Login         string
	AvatarURL     string
	Organizations []string
	Teams         []string
	CSRFToken     string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// AuthFlow is short-lived server-side OAuth state and its PKCE verifier.
type AuthFlow struct {
	ID, State, CodeVerifier string
	ExpiresAt               time.Time
}

// CreateAuthFlow stores a one-time OAuth flow.
func (s *Store) CreateAuthFlow(ctx context.Context, flow AuthFlow) error {
	if flow.ID == "" || flow.State == "" || flow.CodeVerifier == "" || flow.ExpiresAt.IsZero() {
		return errors.New("auth flow has missing required fields")
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_flows (id, state, code_verifier, expires_at) VALUES (?, ?, ?, ?)
	`, flow.ID, flow.State, flow.CodeVerifier, formatTime(flow.ExpiresAt)); err != nil {
		return fmt.Errorf("create auth flow: %w", err)
	}
	return nil
}

// ConsumeAuthFlow atomically removes and returns a matching unexpired flow.
func (s *Store) ConsumeAuthFlow(
	ctx context.Context, id, state string, now time.Time,
) (*AuthFlow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin auth flow consumption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var flow AuthFlow
	var expiresAt string
	err = tx.QueryRowContext(ctx, `
		DELETE FROM auth_flows
		WHERE id = ? AND state = ? AND expires_at > ?
		RETURNING id, state, code_verifier, expires_at
	`, id, state, formatTime(now)).Scan(&flow.ID, &flow.State, &flow.CodeVerifier, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume auth flow: %w", err)
	}
	flow.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse auth flow expiry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit auth flow consumption: %w", err)
	}
	return &flow, nil
}

// CreateSession stores an opaque session and its authorization evidence.
func (s *Store) CreateSession(ctx context.Context, session Session) error {
	if session.ID == "" || session.UserID <= 0 || session.Login == "" ||
		session.CSRFToken == "" || session.ExpiresAt.IsZero() {
		return errors.New("session has missing required fields")
	}
	organizations, err := json.Marshal(nonNilStrings(session.Organizations))
	if err != nil {
		return fmt.Errorf("encode session organizations: %w", err)
	}
	teams, err := json.Marshal(nonNilStrings(session.Teams))
	if err != nil {
		return fmt.Errorf("encode session teams: %w", err)
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (
			id, user_id, login, avatar_url, organizations_json, teams_json,
			csrf_token, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, session.ID, session.UserID, session.Login, session.AvatarURL,
		string(organizations), string(teams),
		session.CSRFToken, formatTime(session.CreatedAt), formatTime(session.ExpiresAt)); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession returns nil for missing or expired sessions.
func (s *Store) GetSession(ctx context.Context, id string, now time.Time) (*Session, error) {
	if id == "" {
		return nil, nil
	}
	var session Session
	var organizations, teams, createdAt, expiresAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, login, avatar_url, organizations_json, teams_json,
			csrf_token, created_at, expires_at
		FROM sessions WHERE id = ? AND expires_at > ?
	`, id, formatTime(now)).Scan(
		&session.ID, &session.UserID, &session.Login, &session.AvatarURL,
		&organizations, &teams,
		&session.CSRFToken, &createdAt, &expiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		_, _ = s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if err := json.Unmarshal([]byte(organizations), &session.Organizations); err != nil {
		return nil, fmt.Errorf("decode session organizations: %w", err)
	}
	if err := json.Unmarshal([]byte(teams), &session.Teams); err != nil {
		return nil, fmt.Errorf("decode session teams: %w", err)
	}
	session.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse session creation time: %w", err)
	}
	session.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse session expiry: %w", err)
	}
	return &session, nil
}

// DeleteSession explicitly logs one browser session out.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeletePersonalData removes all user-owned product state.
func (s *Store) DeletePersonalData(ctx context.Context, userID int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin personal data deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM important_pull_requests WHERE user_id = ?", userID); err != nil {
		return fmt.Errorf("delete Important pull requests: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM hidden_pull_requests WHERE user_id = ?", userID); err != nil {
		return fmt.Errorf("delete hidden pull requests: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM goal_preferences WHERE user_id = ?", userID); err != nil {
		return fmt.Errorf("delete goal preferences: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM progress_events WHERE actor_id = ?", userID); err != nil {
		return fmt.Errorf("delete private progress: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID); err != nil {
		return fmt.Errorf("delete personal sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events (occurred_at, event, user_id, detail)
		VALUES (?, 'personal_data_deleted', ?, '')
	`, formatTime(now), userID); err != nil {
		return fmt.Errorf("audit personal data deletion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit personal data deletion: %w", err)
	}
	return nil
}

// RecordAudit records a non-sensitive security boundary event.
func (s *Store) RecordAudit(ctx context.Context, event string, userID *int64, detail string, at time.Time) error {
	allowed := map[string]struct{}{
		"authentication_succeeded": {}, "authentication_denied": {},
		"authorization_denied": {}, "logout": {}, "personal_data_deleted": {},
		"reload_succeeded": {}, "reload_failed": {}, "policy_changed": {},
		"model_payload_accessed": {}, "admin_refresh_requested": {},
	}
	if _, ok := allowed[event]; !ok {
		return fmt.Errorf("unsupported audit event %q", event)
	}
	if len(detail) > 100 {
		return errors.New("audit detail exceeds 100 characters")
	}
	lowerDetail := strings.ToLower(detail)
	for _, sensitive := range []string{"token", "secret", "prompt", "request_json", "response_json"} {
		if strings.Contains(lowerDetail, sensitive) {
			return errors.New("audit detail contains prohibited sensitive category")
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events (occurred_at, event, user_id, detail) VALUES (?, ?, ?, ?)
	`, formatTime(at), event, userID, detail); err != nil {
		return fmt.Errorf("record audit event: %w", err)
	}
	return nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
