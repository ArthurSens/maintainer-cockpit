package application

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestHiddenPullRequestsArePrivateAndRemainAvailableByDirectURL(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	app := newHiddenTestApplication(t, &now)
	pullRequests := hiddenTestPullRequests(now)
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", pullRequests, now,
	); err != nil {
		t.Fatal(err)
	}
	arthur := createImportantSession(t, app.Store(), 42, "arthur", now)
	other := createImportantSession(t, app.Store(), 99, "other", now)
	handler := app.Handler()

	target := "/api/collections/acme/pull-requests/acme/widgets/1/hidden"
	response := performHiddenRequest(t, handler, http.MethodPut, target, arthur, map[string]any{
		"kind":           "snoozed",
		"reason":         "Waiting for the next release",
		"snoozedUntil":   now.Add(24 * time.Hour),
		"wakeOnActivity": true,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("snooze = %d %s", response.Code, response.Body.String())
	}

	ownerAll := performImportantRequest(
		handler, http.MethodGet, "/api/collections/acme/pull-requests?limit=100", arthur,
	)
	if strings.Contains(ownerAll.Body.String(), `"number":1`) {
		t.Fatalf("snoozed PR remained in owner's default view: %s", ownerAll.Body.String())
	}
	ownerMine := performImportantRequest(
		handler, http.MethodGet,
		"/api/collections/acme/pull-requests?view=mine&limit=100", arthur,
	)
	if strings.Contains(ownerMine.Body.String(), `"number":1`) {
		t.Fatalf("snoozed PR remained in owner's Mine view: %s", ownerMine.Body.String())
	}
	for name, session := range map[string]importantSession{"teammate": other} {
		response := performImportantRequest(
			handler, http.MethodGet, "/api/collections/acme/pull-requests?limit=100", session,
		)
		if !strings.Contains(response.Body.String(), `"number":1`) {
			t.Fatalf("%s view was affected by another user's snooze: %s", name, response.Body.String())
		}
	}
	anonymous := performRequest(
		handler, http.MethodGet, "/api/collections/acme/pull-requests?limit=100", "", "", nil,
	)
	if !strings.Contains(anonymous.Body.String(), `"number":1`) {
		t.Fatalf("public view was affected by private snooze: %s", anonymous.Body.String())
	}

	snoozed := performImportantRequest(
		handler, http.MethodGet,
		"/api/collections/acme/pull-requests?view=hidden&limit=100", arthur,
	)
	if snoozed.Code != http.StatusOK ||
		!strings.Contains(snoozed.Body.String(), `"reason":"Waiting for the next release"`) {
		t.Fatalf("Snoozed view = %d %s", snoozed.Code, snoozed.Body.String())
	}
	detail := performImportantRequest(
		handler, http.MethodGet,
		"/api/collections/acme/pull-requests/acme/widgets/1", arthur,
	)
	if detail.Code != http.StatusOK ||
		!strings.Contains(detail.Body.String(), `"kind":"snoozed"`) {
		t.Fatalf("owner direct detail = %d %s", detail.Code, detail.Body.String())
	}
	otherDetail := performImportantRequest(
		handler, http.MethodGet,
		"/api/collections/acme/pull-requests/acme/widgets/1", other,
	)
	if strings.Contains(otherDetail.Body.String(), `"kind":"snoozed"`) ||
		strings.Contains(otherDetail.Body.String(), "Waiting for the next release") {
		t.Fatalf("private snooze leaked through direct detail: %s", otherDetail.Body.String())
	}

	restored := performHiddenRequest(t, handler, http.MethodDelete, target, arthur, nil)
	if restored.Code != http.StatusNoContent {
		t.Fatalf("restore = %d %s", restored.Code, restored.Body.String())
	}
	ownerAll = performImportantRequest(
		handler, http.MethodGet, "/api/collections/acme/pull-requests?limit=100", arthur,
	)
	if !strings.Contains(ownerAll.Body.String(), `"number":1`) {
		t.Fatalf("restored PR missing from default view: %s", ownerAll.Body.String())
	}
}

func TestSnoozeWakesOnFirstConfiguredConditionAndCanBeEdited(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	app := newHiddenTestApplication(t, &now)
	pullRequests := hiddenTestPullRequests(now)
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", pullRequests, now,
	); err != nil {
		t.Fatal(err)
	}
	arthur := createImportantSession(t, app.Store(), 42, "arthur", now)
	handler := app.Handler()
	target := "/api/collections/acme/pull-requests/acme/widgets/1/hidden"

	response := performHiddenRequest(t, handler, http.MethodPut, target, arthur, map[string]any{
		"kind":         "snoozed",
		"snoozedUntil": now.Add(24 * time.Hour),
		"reason":       "Initial reason",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("date snooze = %d %s", response.Code, response.Body.String())
	}
	response = performHiddenRequest(t, handler, http.MethodPut, target, arthur, map[string]any{
		"kind":         "snoozed",
		"snoozedUntil": now.Add(48 * time.Hour),
		"reason":       "Edited reason",
	})
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"reason":"Edited reason"`) {
		t.Fatalf("edit snooze = %d %s", response.Code, response.Body.String())
	}
	now = now.Add(49 * time.Hour)
	visible := performImportantRequest(
		handler, http.MethodGet, "/api/collections/acme/pull-requests?limit=100", arthur,
	)
	if !strings.Contains(visible.Body.String(), `"number":1`) {
		t.Fatalf("date-expired snooze did not wake: %s", visible.Body.String())
	}

	now = now.Add(time.Hour)
	arthur = createImportantSession(t, app.Store(), 42, "arthur-resigned", now)
	pullRequests[0].UpdatedAt = now
	response = performHiddenRequest(t, handler, http.MethodPut, target, arthur, map[string]any{
		"kind":           "snoozed",
		"snoozedUntil":   now.Add(24 * time.Hour),
		"wakeOnActivity": true,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("activity snooze = %d %s", response.Code, response.Body.String())
	}
	pullRequests[0].UpdatedAt = now.Add(time.Minute)
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", pullRequests, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	visible = performImportantRequest(
		handler, http.MethodGet, "/api/collections/acme/pull-requests?limit=100", arthur,
	)
	if !strings.Contains(visible.Body.String(), `"number":1`) {
		t.Fatalf("activity did not wake snooze: %s", visible.Body.String())
	}
}

func TestIgnorePersistsAcrossActivityAndReportsItPrivately(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	app := newHiddenTestApplication(t, &now)
	pullRequests := hiddenTestPullRequests(now)
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", pullRequests, now,
	); err != nil {
		t.Fatal(err)
	}
	arthur := createImportantSession(t, app.Store(), 42, "arthur", now)
	target := "/api/collections/acme/pull-requests/acme/widgets/2/hidden"
	response := performHiddenRequest(t, app.Handler(), http.MethodPut, target, arthur, map[string]any{
		"kind":   "ignored",
		"reason": "Not relevant to my area",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("ignore = %d %s", response.Code, response.Body.String())
	}

	pullRequests[1].UpdatedAt = now.Add(time.Hour)
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", pullRequests, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	defaultView := performImportantRequest(
		app.Handler(), http.MethodGet,
		"/api/collections/acme/pull-requests?limit=100", arthur,
	)
	if strings.Contains(defaultView.Body.String(), `"number":2`) {
		t.Fatalf("ignored PR returned after activity: %s", defaultView.Body.String())
	}
	ignored := performImportantRequest(
		app.Handler(), http.MethodGet,
		"/api/collections/acme/pull-requests?view=hidden&limit=100", arthur,
	)
	if ignored.Code != http.StatusOK ||
		!strings.Contains(ignored.Body.String(), `"newActivity":true`) ||
		!strings.Contains(ignored.Body.String(), `"reason":"Not relevant to my area"`) {
		t.Fatalf("Ignored view = %d %s", ignored.Code, ignored.Body.String())
	}
}

func TestHidingDoesNotCreateOrRemoveGoalProgress(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	app := newHiddenTestApplication(t, &now)
	pullRequests := hiddenTestPullRequests(now)
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", pullRequests, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := app.Store().RecordProgressEvents(
		context.Background(), "acme/widgets", []storage.ProgressEvent{{
			ID: "review-1", ActorID: 42, ActivityType: "review",
			Repository: "acme/widgets", Number: 1, CollectionIDs: []string{"acme"},
			Title:          "Authored by Arthur",
			PullRequestURL: "https://github.com/acme/widgets/pull/1",
			ActivityURL:    "https://github.com/acme/widgets/pull/1#pullrequestreview-1",
			OccurredAt:     now,
		}}, now,
	); err != nil {
		t.Fatal(err)
	}
	arthur := createImportantSession(t, app.Store(), 42, "arthur", now)
	progressBefore := performImportantRequest(
		app.Handler(), http.MethodGet, "/api/collections/acme/progress", arthur,
	)
	response := performHiddenRequest(
		t, app.Handler(), http.MethodPut,
		"/api/collections/acme/pull-requests/acme/widgets/1/hidden",
		arthur, map[string]any{"kind": "ignored"},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("ignore = %d %s", response.Code, response.Body.String())
	}
	progressAfter := performImportantRequest(
		app.Handler(), http.MethodGet, "/api/collections/acme/progress", arthur,
	)
	var before, after struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(progressBefore.Body).Decode(&before); err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(progressAfter.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if before.Count != 1 || after.Count != before.Count {
		t.Fatalf("progress before/after hide = %d/%d, want 1/1", before.Count, after.Count)
	}
}

func TestAuthenticatedPullRequestListPaginatesLargeCollection(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	app := newHiddenTestApplication(t, &now)
	pullRequests := make([]storage.PullRequest, 101)
	for index := range pullRequests {
		number := index + 1
		pullRequests[index] = storage.PullRequest{
			Repository:  "acme/widgets",
			Number:      number,
			Title:       fmt.Sprintf("Pull request %03d", number),
			Author:      fmt.Sprintf("author-%03d", number),
			AuthorID:    int64(number + 100),
			CreatedAt:   now.Add(-time.Hour),
			UpdatedAt:   now.Add(-time.Duration(index) * time.Minute),
			URL:         fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
			ReviewState: "none",
		}
	}
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", pullRequests, now,
	); err != nil {
		t.Fatal(err)
	}
	arthur := createImportantSession(t, app.Store(), 42, "arthur", now)
	response := performImportantRequest(
		app.Handler(), http.MethodGet,
		"/api/collections/acme/pull-requests?limit=100", arthur,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("large authenticated list = %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Counts struct {
			Total   int `json:"total"`
			Matched int `json:"matched"`
		} `json:"counts"`
		Page struct {
			NextCursor string `json:"nextCursor"`
		} `json:"page"`
		PullRequests []storage.PullRequest `json:"pullRequests"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Counts.Total != 101 || payload.Counts.Matched != 101 ||
		len(payload.PullRequests) != 100 || payload.Page.NextCursor == "" {
		t.Fatalf("large authenticated page = %+v", payload)
	}
}

func newHiddenTestApplication(t *testing.T, now *time.Time) *Application {
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
`),
		filepath.Join(t.TempDir(), "app.db"),
		Options{AuthProvider: &fakeAuthProvider{}, Now: func() time.Time { return *now }},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

func hiddenTestPullRequests(now time.Time) []storage.PullRequest {
	return []storage.PullRequest{
		{
			Repository: "acme/widgets", Number: 1, Title: "Authored by Arthur",
			Author: "arthur", AuthorID: 42, UpdatedAt: now,
			URL: "https://github.com/acme/widgets/pull/1",
		},
		{
			Repository: "acme/widgets", Number: 2, Title: "Review this",
			Author: "contributor", AuthorID: 200, UpdatedAt: now,
			URL: "https://github.com/acme/widgets/pull/2",
		},
	}
}

func performHiddenRequest(
	t *testing.T,
	handler http.Handler,
	method, target string,
	session importantSession,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	request.AddCookie(session.cookie)
	request.Header.Set("Origin", "https://cockpit.example.test")
	request.Header.Set("X-CSRF-Token", session.csrf)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
