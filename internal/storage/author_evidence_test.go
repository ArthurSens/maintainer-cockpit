package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAncillaryPacingAndCooldownSurviveRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "authors.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC)
	for id := 1; id <= 2; id++ {
		if _, err := store.db.Exec(`INSERT INTO authors
			(id, login, account_created_at, history_complete, recent_activity_json, collected_at)
			VALUES (?, ?, '', 0, '[]', ?)`, id, string(rune('a'+id)), formatTime(now)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`INSERT INTO ancillary_author_jobs
			(author_id, login, status, scheduled_for) VALUES (?, ?, 'queued', ?)`,
			id, string(rune('a'+id)), formatTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	job, err := store.ClaimAncillaryAuthorEvidence(ctx, now)
	if err != nil || job == nil {
		t.Fatalf("first claim = %v, %v", job, err)
	}
	if extra, err := store.ClaimAncillaryAuthorEvidence(ctx, now); err != nil || extra != nil {
		t.Fatalf("concurrent claim = %v, %v", extra, err)
	}
	retryAt := now.Add(time.Hour)
	if err := store.RequeueAncillaryAuthorEvidence(ctx, job, now, retryAt, "GitHub primary rate limit prevented collection."); err != nil {
		t.Fatal(err)
	}
	status, err := store.AuthorEvidenceStatus(ctx, job.AuthorID)
	if err != nil || status.LastSuccessfulAt != nil || status.LastAttemptAt == nil ||
		status.NextAttemptAt == nil || !status.NextAttemptAt.Equal(retryAt) || status.Error == "" {
		t.Fatalf("deferred health = %+v, %v", status, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if early, err := store.ClaimAncillaryAuthorEvidence(ctx, retryAt.Add(-time.Second)); err != nil || early != nil {
		t.Fatalf("claim during persisted cooldown = %v, %v", early, err)
	}
	next, err := store.ClaimAncillaryAuthorEvidence(ctx, retryAt)
	if err != nil || next == nil || next.AuthorID == job.AuthorID {
		t.Fatalf("next author after cooldown = %v, %v", next, err)
	}
	if err := store.CompleteAncillaryAuthorEvidence(ctx, next, now.AddDate(-1, 0, 0), nil, retryAt, nil); err != nil {
		t.Fatal(err)
	}
	if early, err := store.ClaimAncillaryAuthorEvidence(ctx, retryAt.Add(64*time.Second)); err != nil || early != nil {
		t.Fatalf("claim before paced slot = %v, %v", early, err)
	}
	resumed, err := store.ClaimAncillaryAuthorEvidence(ctx, retryAt.Add(AncillaryRequestInterval))
	if err != nil || resumed == nil || resumed.ID != job.ID {
		t.Fatalf("resumed job = %v, %v", resumed, err)
	}
	if err := store.CompleteAncillaryAuthorEvidence(ctx, resumed, now.AddDate(-1, 0, 0), nil, retryAt.Add(AncillaryRequestInterval), nil); err != nil {
		t.Fatal(err)
	}
	status, err = store.AuthorEvidenceStatus(ctx, job.AuthorID)
	if err != nil || status.Status != "complete" || status.LastSuccessfulAt == nil || status.Error != "" || status.NextAttemptAt != nil {
		t.Fatalf("recovered health = %+v, %v", status, err)
	}
}

func TestAuthorEvidenceMigrationPreservesExistingData(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "migration.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE authors DROP COLUMN ancillary_attempted_at;
		ALTER TABLE authors DROP COLUMN ancillary_error;
		INSERT INTO authors (id, login, account_created_at, history_complete, recent_activity_json, collected_at)
		VALUES (42, 'octocat', '', 0, '[]', '2026-09-17T03:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	status, err := store.AuthorEvidenceStatus(context.Background(), 42)
	if err != nil || status.LastSuccessfulAt != nil || status.LastAttemptAt != nil {
		t.Fatalf("migrated status = %+v, %v", status, err)
	}
}
