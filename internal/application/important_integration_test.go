package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestMineContainsAuthoredAndPersonallyImportantPullRequests(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	app := newImportantTestApplication(t, now)
	pullRequests := []storage.PullRequest{
		{
			Repository: "acme/widgets", Number: 1, Title: "Authored by Arthur",
			Author: "arthur", AuthorID: 42, UpdatedAt: now,
			URL: "https://github.com/acme/widgets/pull/1",
		},
		{
			Repository: "acme/widgets", Number: 2, Title: "Review requested",
			Author: "contributor", AuthorID: 200, UpdatedAt: now,
			URL: "https://github.com/acme/widgets/pull/2", ReviewState: "review_requested",
		},
		{
			Repository: "acme/widgets", Number: 3, Title: "Unrelated",
			Author: "contributor", AuthorID: 201, UpdatedAt: now,
			URL: "https://github.com/acme/widgets/pull/3",
		},
	}
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", pullRequests, now,
	); err != nil {
		t.Fatal(err)
	}

	arthur := createImportantSession(t, app.Store(), 42, "arthur", now)
	other := createImportantSession(t, app.Store(), 99, "other", now)
	handler := app.Handler()

	marked := performImportantRequest(
		handler, http.MethodPut,
		"/api/collections/acme/pull-requests/acme/widgets/3/important",
		arthur,
	)
	if marked.Code != http.StatusOK ||
		!strings.Contains(marked.Body.String(), `"important":true`) {
		t.Fatalf("mark Important = %d %s", marked.Code, marked.Body.String())
	}

	mine := performImportantRequest(
		handler, http.MethodGet,
		"/api/collections/acme/pull-requests?view=mine&limit=100",
		arthur,
	)
	if mine.Code != http.StatusOK {
		t.Fatalf("Mine = %d %s", mine.Code, mine.Body.String())
	}
	var payload struct {
		PullRequests []storage.PullRequest `json:"pullRequests"`
	}
	if err := json.NewDecoder(mine.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	got := make(map[int]bool)
	for _, pr := range payload.PullRequests {
		got[pr.Number] = true
	}
	if !got[1] || !got[3] {
		t.Errorf("Mine = %v, want authored PR 1 and Important PR 3", got)
	}
	if got[2] {
		t.Errorf("Mine = %v; review-requested PR 2 must not be included", got)
	}

	arthurDetail := performImportantRequest(
		handler, http.MethodGet,
		"/api/collections/acme/pull-requests/acme/widgets/3",
		arthur,
	)
	if !strings.Contains(arthurDetail.Body.String(), `"important":true`) {
		t.Fatalf("owner detail omitted Important state: %s", arthurDetail.Body.String())
	}
	otherDetail := performImportantRequest(
		handler, http.MethodGet,
		"/api/collections/acme/pull-requests/acme/widgets/3",
		other,
	)
	if strings.Contains(otherDetail.Body.String(), `"important":true`) {
		t.Fatalf("Important state leaked to another maintainer: %s", otherDetail.Body.String())
	}

	unmarkedByOther := performImportantRequest(
		handler, http.MethodDelete,
		"/api/collections/acme/pull-requests/acme/widgets/3/important",
		other,
	)
	if unmarkedByOther.Code != http.StatusNotFound {
		t.Fatalf("another maintainer unmarked private state: %d %s", unmarkedByOther.Code, unmarkedByOther.Body.String())
	}
	stillImportant := performImportantRequest(
		handler, http.MethodGet,
		"/api/collections/acme/pull-requests/acme/widgets/3",
		arthur,
	)
	if !strings.Contains(stillImportant.Body.String(), `"important":true`) {
		t.Fatalf("another user's action changed Important state: %s", stillImportant.Body.String())
	}

	unmarked := performImportantRequest(
		handler, http.MethodDelete,
		"/api/collections/acme/pull-requests/acme/widgets/3/important",
		arthur,
	)
	if unmarked.Code != http.StatusNoContent {
		t.Fatalf("unmark Important = %d %s", unmarked.Code, unmarked.Body.String())
	}
	mine = performImportantRequest(
		handler, http.MethodGet,
		"/api/collections/acme/pull-requests?view=mine&limit=100",
		arthur,
	)
	if strings.Contains(mine.Body.String(), `"number":3`) {
		t.Fatalf("unmarked PR remained in Mine: %s", mine.Body.String())
	}

	anonymousMine := performRequest(
		handler, http.MethodGet,
		"/api/collections/acme/pull-requests?view=mine",
		"", "", nil,
	)
	if anonymousMine.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous Mine = %d, want 401", anonymousMine.Code)
	}
}

func TestImportantPullRequestSurvivesRefreshAndPersonalDataDeletionRemovesIt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	app := newImportantTestApplication(t, now)
	pr := storage.PullRequest{
		Repository: "acme/widgets", Number: 7, Title: "Keep",
		Author: "contributor", AuthorID: 200, UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/7",
	}
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", []storage.PullRequest{pr}, now,
	); err != nil {
		t.Fatal(err)
	}
	arthur := createImportantSession(t, app.Store(), 42, "arthur", now)
	target := "/api/collections/acme/pull-requests/acme/widgets/7/important"
	if response := performImportantRequest(app.Handler(), http.MethodPut, target, arthur); response.Code != http.StatusOK {
		t.Fatalf("mark Important = %d %s", response.Code, response.Body.String())
	}

	pr.UpdatedAt = now.Add(time.Hour)
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(), "acme/widgets", []storage.PullRequest{pr}, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	mine := performImportantRequest(
		app.Handler(), http.MethodGet,
		"/api/collections/acme/pull-requests?view=mine",
		arthur,
	)
	if !strings.Contains(mine.Body.String(), `"number":7`) {
		t.Fatalf("Important PR did not survive refresh: %s", mine.Body.String())
	}

	deleted := performImportantRequest(
		app.Handler(), http.MethodDelete, "/api/me/data", arthur,
	)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete personal data = %d %s", deleted.Code, deleted.Body.String())
	}
	replacement := createImportantSession(t, app.Store(), 42, "arthur", now)
	mine = performImportantRequest(
		app.Handler(), http.MethodGet,
		"/api/collections/acme/pull-requests?view=mine",
		replacement,
	)
	if strings.Contains(mine.Body.String(), `"number":7`) {
		t.Fatalf("Important PR survived personal-data deletion: %s", mine.Body.String())
	}
}

func newImportantTestApplication(t *testing.T, now time.Time) *Application {
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
		Options{AuthProvider: &fakeAuthProvider{}, Now: func() time.Time { return now }},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

type importantSession struct {
	cookie *http.Cookie
	csrf   string
}

func createImportantSession(
	t *testing.T,
	store *storage.Store,
	userID int64,
	login string,
	now time.Time,
) importantSession {
	t.Helper()
	session := storage.Session{
		ID: "session-" + login, UserID: userID, Login: login,
		CSRFToken: "csrf-" + login, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}
	if err := store.CreateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	return importantSession{
		cookie: &http.Cookie{Name: sessionCookieName, Value: session.ID},
		csrf:   session.CSRFToken,
	}
}

func performImportantRequest(
	handler http.Handler,
	method, target string,
	session importantSession,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, http.NoBody)
	request.AddCookie(session.cookie)
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Origin", "https://cockpit.example.test")
		request.Header.Set("X-CSRF-Token", session.csrf)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
