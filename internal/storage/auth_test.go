package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestOpaqueSessionsExpireAndCanBeRevoked(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	session := Session{
		ID: "opaque-session", UserID: 42, Login: "octocat",
		AvatarURL:     "https://avatars.githubusercontent.com/u/42?v=4",
		Organizations: []string{"acme"}, Teams: []string{"acme/maintainers"},
		CSRFToken: "csrf-token", ExpiresAt: now.Add(24 * time.Hour),
	}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSession(ctx, session.ID, now.Add(23*time.Hour))
	if err != nil || got == nil || got.UserID != 42 || got.CSRFToken != "csrf-token" ||
		got.AvatarURL != session.AvatarURL {
		t.Fatalf("GetSession() = %+v, %v", got, err)
	}
	if got, err := store.GetSession(ctx, session.ID, now.Add(24*time.Hour)); err != nil || got != nil {
		t.Fatalf("expired GetSession() = %+v, %v; want nil, nil", got, err)
	}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetSession(ctx, session.ID, now); err != nil || got != nil {
		t.Fatalf("revoked GetSession() = %+v, %v; want nil, nil", got, err)
	}
}

func TestDeletePersonalDataRemovesEverySessionForNumericIdentity(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	for _, session := range []Session{
		{ID: "one", UserID: 42, Login: "old-name", CSRFToken: "one", ExpiresAt: now.Add(time.Hour)},
		{ID: "two", UserID: 42, Login: "new-name", CSRFToken: "two", ExpiresAt: now.Add(time.Hour)},
		{ID: "other", UserID: 99, Login: "other", CSRFToken: "other", ExpiresAt: now.Add(time.Hour)},
	} {
		if err := store.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DeletePersonalData(ctx, 42, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if got, err := store.GetSession(ctx, id, now); err != nil || got != nil {
			t.Errorf("GetSession(%q) = %+v, %v; want deleted", id, got, err)
		}
	}
	if got, err := store.GetSession(ctx, "other", now); err != nil || got == nil {
		t.Errorf("other session = %+v, %v; want retained", got, err)
	}
}
