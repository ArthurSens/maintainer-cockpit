package application

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestDailyProgressDefaultsDeduplicateAndRemainPrivate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	app := newProgressTestApplication(t, now)
	events := []storage.ProgressEvent{
		progressEvent("review-1", 42, "review", 1, now.Add(-2*time.Hour), "acme", "shared"),
		progressEvent("comment-1", 42, "comment", 1, now.Add(-time.Hour), "acme", "shared"),
		progressEvent("review-2", 42, "review", 2, now.Add(-30*time.Minute), "acme", "shared"),
		progressEvent("other-user", 99, "review", 3, now.Add(-15*time.Minute), "acme"),
	}
	events = append(events, events[0])
	if err := app.Store().RecordProgressEvents(context.Background(), "acme/widgets", events, now); err != nil {
		t.Fatal(err)
	}

	arthur := createImportantSession(t, app.Store(), 42, "arthur", now)
	response := performProgressRequest(
		app.Handler(), http.MethodGet, "/api/collections/acme/progress", arthur, nil,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("progress = %d %s", response.Code, response.Body.String())
	}
	var progress storage.DailyProgress
	if err := json.NewDecoder(response.Body).Decode(&progress); err != nil {
		t.Fatal(err)
	}
	if progress.Target != 10 || progress.Timezone != "UTC" || progress.Count != 2 {
		t.Errorf("default progress = %+v, want 2 of 10 in UTC", progress)
	}
	if len(progress.PullRequests) != 2 || len(progress.PullRequests[0].Activities) != 2 {
		t.Errorf("counted pull requests = %+v, want duplicate PR activities grouped once", progress.PullRequests)
	}
	if progress.LastCollectionTime == nil || !progress.LastCollectionTime.Equal(now) {
		t.Errorf("last collection time = %v, want %v", progress.LastCollectionTime, now)
	}

	shared := performProgressRequest(
		app.Handler(), http.MethodGet, "/api/collections/shared/progress", arthur, nil,
	)
	var sharedProgress storage.DailyProgress
	if err := json.NewDecoder(shared.Body).Decode(&sharedProgress); err != nil {
		t.Fatal(err)
	}
	if sharedProgress.Count != 2 {
		t.Errorf("overlapping collection count = %d, want 2", sharedProgress.Count)
	}

	other := createImportantSession(t, app.Store(), 99, "other", now)
	private := performProgressRequest(
		app.Handler(), http.MethodGet, "/api/collections/acme/progress", other, nil,
	)
	var otherProgress storage.DailyProgress
	if err := json.NewDecoder(private.Body).Decode(&otherProgress); err != nil {
		t.Fatal(err)
	}
	if otherProgress.Count != 1 {
		t.Errorf("other maintainer count = %d, want only their own event", otherProgress.Count)
	}

	anonymous := performRequest(
		app.Handler(), http.MethodGet, "/api/collections/acme/progress", "", "", nil,
	)
	if anonymous.Code != http.StatusUnauthorized {
		t.Errorf("anonymous progress = %d, want 401", anonymous.Code)
	}
}

func TestDailyProgressPreferencesAndTimezoneBoundary(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 2, 30, 0, 0, time.UTC)
	app := newProgressTestApplication(t, now)
	if err := app.Store().RecordProgressEvents(context.Background(), "acme/widgets", []storage.ProgressEvent{
		progressEvent("before-local-midnight", 42, "comment", 1,
			time.Date(2026, 9, 10, 2, 0, 0, 0, time.UTC), "acme"),
		progressEvent("after-local-midnight", 42, "comment", 2,
			time.Date(2026, 9, 10, 4, 30, 0, 0, time.UTC), "acme"),
		progressEvent("disabled-review", 42, "review", 3,
			time.Date(2026, 9, 10, 5, 0, 0, 0, time.UTC), "acme"),
	}, now); err != nil {
		t.Fatal(err)
	}
	session := createImportantSession(t, app.Store(), 42, "arthur", now)
	body := []byte(`{"target":3,"enabledActivities":["comment"],"timezone":"America/New_York"}`)
	updated := performProgressRequest(
		app.Handler(), http.MethodPut, "/api/collections/acme/progress", session, body,
	)
	if updated.Code != http.StatusOK {
		t.Fatalf("update progress preferences = %d %s", updated.Code, updated.Body.String())
	}

	response := performProgressRequest(
		app.Handler(), http.MethodGet, "/api/collections/acme/progress", session, nil,
	)
	var progress storage.DailyProgress
	if err := json.NewDecoder(response.Body).Decode(&progress); err != nil {
		t.Fatal(err)
	}
	if progress.Date != "2026-09-09" || progress.Count != 1 || progress.Target != 3 {
		t.Errorf("timezone progress = %+v, want one comment on 2026-09-09", progress)
	}
	if len(progress.EnabledActivities) != 1 || progress.EnabledActivities[0] != "comment" {
		t.Errorf("enabled activities = %v, want comment only", progress.EnabledActivities)
	}

	invalid := performProgressRequest(
		app.Handler(), http.MethodPut, "/api/collections/acme/progress", session,
		[]byte(`{"target":0,"enabledActivities":[],"timezone":"not/a-zone"}`),
	)
	if invalid.Code != http.StatusBadRequest {
		t.Errorf("invalid preferences = %d, want 400", invalid.Code)
	}
}

func TestPersonalDataDeletionRemovesGoalPreferencesAndProgress(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	app := newProgressTestApplication(t, now)
	if err := app.Store().RecordProgressEvents(context.Background(), "acme/widgets", []storage.ProgressEvent{
		progressEvent("review", 42, "review", 1, now, "acme"),
	}, now); err != nil {
		t.Fatal(err)
	}
	session := createImportantSession(t, app.Store(), 42, "arthur", now)
	updated := performProgressRequest(
		app.Handler(), http.MethodPut, "/api/collections/acme/progress", session,
		[]byte(`{"target":4,"enabledActivities":["review"],"timezone":"UTC"}`),
	)
	if updated.Code != http.StatusOK {
		t.Fatalf("update preferences = %d %s", updated.Code, updated.Body.String())
	}
	deleted := performProgressRequest(
		app.Handler(), http.MethodDelete, "/api/me/data", session, nil,
	)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete personal data = %d %s", deleted.Code, deleted.Body.String())
	}
	replacement := createImportantSession(t, app.Store(), 42, "arthur", now)
	response := performProgressRequest(
		app.Handler(), http.MethodGet, "/api/collections/acme/progress", replacement, nil,
	)
	var progress storage.DailyProgress
	if err := json.NewDecoder(response.Body).Decode(&progress); err != nil {
		t.Fatal(err)
	}
	if progress.Target != 10 || progress.Count != 0 {
		t.Errorf("progress after personal-data deletion = %+v, want empty defaults", progress)
	}
}

func newProgressTestApplication(t *testing.T, now time.Time) *Application {
	t.Helper()
	app, err := NewWithOptions(
		context.Background(),
		writeConfig(t, `
external_base_url: https://cockpit.example.test
github:
  schedule: "0 * * * *"
  authentication:
    profiles:
      test:
        type: fine_grained_pat
        token:
          environment: UNUSED_COLLECTION_PAT
    owners:
      acme:
        profiles: [test]
  user_auth:
    client_id: Iv1.example
    client_secret:
      environment: UNUSED_CLIENT_SECRET
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
    authorization:
      users: [42, 99]
  - id: shared
    name: Shared
    repositories: [acme/widgets]
    authorization:
      users: [42]
`),
		filepath.Join(t.TempDir(), "app.db"),
		Options{AuthProvider: &fakeAuthProvider{}, Now: func() time.Time { return now }},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

func progressEvent(
	id string,
	actorID int64,
	activityType string,
	number int,
	occurredAt time.Time,
	collectionIDs ...string,
) storage.ProgressEvent {
	return storage.ProgressEvent{
		ID: id, ActorID: actorID, ActivityType: activityType,
		Repository: "acme/widgets", Number: number, Title: "Progressed PR",
		PullRequestURL: "https://github.com/acme/widgets/pull/1",
		ActivityURL:    "https://github.com/acme/widgets/pull/1#event-" + id,
		OccurredAt:     occurredAt, CollectionIDs: collectionIDs,
	}
}

func performProgressRequest(
	handler http.Handler,
	method, target string,
	session importantSession,
	body []byte,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.AddCookie(session.cookie)
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Origin", "https://cockpit.example.test")
		request.Header.Set("X-CSRF-Token", session.csrf)
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
