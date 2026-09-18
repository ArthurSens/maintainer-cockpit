package application

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/periodic"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestPublicProbesDiscloseOnlyStatus(t *testing.T) {
	t.Parallel()

	app := newOperationsTestApplication(t, nil)
	for _, path := range []string{"/-/healthy", "/-/ready"} {
		request := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, response.Code, response.Body.String())
		}
		if got := strings.TrimSpace(response.Body.String()); got != `{"status":"ok"}` {
			t.Errorf("GET %s body = %s, want non-sensitive status only", path, got)
		}
	}
}

func TestReloadRequiresSeparateSecretAndUsesRuntimeCallback(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	app := newOperationsTestApplication(t, func(context.Context) error {
		calls.Add(1)
		return nil
	})
	for _, test := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "missing", status: http.StatusUnauthorized},
		{name: "wrong", token: "wrong", status: http.StatusUnauthorized},
		{name: "configured", token: "reload-test-secret", status: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/-/reload", http.NoBody)
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
	if calls.Load() != 1 {
		t.Errorf("runtime reload calls = %d, want 1", calls.Load())
	}
}

func TestAdminStatusRequiresDeploymentAdministrator(t *testing.T) {
	t.Parallel()

	app := newOperationsTestApplication(t, nil)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	app.SetScheduleStatus(func() []periodic.Status {
		return []periodic.Status{{
			Operation: "github_refresh", Expression: "0 * * * *", Enabled: true,
			NextRun: now.Add(time.Hour),
		}}
	})
	for _, user := range []struct {
		id     int64
		status int
	}{
		{id: 7, status: http.StatusForbidden},
		{id: 42, status: http.StatusOK},
	} {
		session := storage.Session{
			ID: "session-" + strconv.FormatInt(user.id, 10), UserID: user.id, Login: "operator",
			CSRFToken: "csrf", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		if err := app.Store().CreateSession(context.Background(), session); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/api/admin/status", http.NoBody)
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session.ID})
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		if response.Code != user.status {
			t.Fatalf("user %d status = %d, body = %s", user.id, response.Code, response.Body.String())
		}
		if user.status == http.StatusOK {
			body := response.Body.String()
			for _, field := range []string{
				`"collectionJobs"`, `"analysisJobs"`, `"correlationJobs"`, `"githubRateLimit"`,
				`"githubRetry"`,
				`"analysisConfiguration":{"configuredProviders":0,"enabledCollections":0}`,
				`"correlationConfiguration":{"enabledCollections":0}`,
				`"modelProviders"`, `"storage"`, `"policyVersions"`, `"telemetry"`,
				`"schedules":[{"operation":"github_refresh","expression":"0 * * * *","enabled":true,` +
					`"nextRun":"2026-09-10T13:00:00Z"}]`,
				`"recentFailures"`,
			} {
				if !strings.Contains(body, field) {
					t.Errorf("admin status missing %s: %s", field, body)
				}
			}
		}
	}

	anonymous := httptest.NewRecorder()
	app.Handler().ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/admin/status", http.NoBody))
	if anonymous.Code != http.StatusUnauthorized {
		t.Errorf("anonymous status = %d, want 401", anonymous.Code)
	}
}

func TestAdminRefreshRequiresDeploymentAdministratorMutationAndEnqueuesForcedRefresh(t *testing.T) {
	t.Parallel()

	app := newOperationsTestApplication(t, nil)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	sessions := map[int64]storage.Session{}
	for _, userID := range []int64{7, 42} {
		session := storage.Session{
			ID: "refresh-session-" + strconv.FormatInt(userID, 10), UserID: userID,
			Login: "operator", CSRFToken: "csrf", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		if err := app.Store().CreateSession(context.Background(), session); err != nil {
			t.Fatal(err)
		}
		sessions[userID] = session
	}

	for _, test := range []struct {
		name   string
		userID int64
		csrf   string
		origin string
		status int
	}{
		{name: "anonymous", csrf: "csrf", origin: "https://cockpit.example.test", status: http.StatusUnauthorized},
		{name: "non administrator", userID: 7, csrf: "csrf", origin: "https://cockpit.example.test", status: http.StatusForbidden},
		{name: "missing CSRF", userID: 42, origin: "https://cockpit.example.test", status: http.StatusForbidden},
		{name: "untrusted origin", userID: 42, csrf: "csrf", origin: "https://elsewhere.example.test", status: http.StatusForbidden},
		{name: "administrator", userID: 42, csrf: "csrf", origin: "https://cockpit.example.test", status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/admin/refresh", http.NoBody)
			if session, ok := sessions[test.userID]; ok {
				request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session.ID})
			}
			request.Header.Set("X-CSRF-Token", test.csrf)
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if test.status == http.StatusOK {
				for _, field := range []string{`"repositories":1`, `"enqueued":1`, `"coalesced":0`} {
					if !strings.Contains(response.Body.String(), field) {
						t.Errorf("response missing %s: %s", field, response.Body.String())
					}
				}
			}
		})
	}

	coalescedRequest := httptest.NewRequest(http.MethodPost, "/api/admin/refresh", http.NoBody)
	coalescedRequest.AddCookie(&http.Cookie{
		Name: sessionCookieName, Value: sessions[42].ID,
	})
	coalescedRequest.Header.Set("X-CSRF-Token", "csrf")
	coalescedRequest.Header.Set("Origin", "https://cockpit.example.test")
	coalescedResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(coalescedResponse, coalescedRequest)
	if coalescedResponse.Code != http.StatusOK ||
		!strings.Contains(coalescedResponse.Body.String(), `"enqueued":0`) ||
		!strings.Contains(coalescedResponse.Body.String(), `"coalesced":1`) {
		t.Fatalf(
			"coalesced refresh status = %d, body = %s",
			coalescedResponse.Code, coalescedResponse.Body.String(),
		)
	}

	job, err := app.Store().ClaimRefresh(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.Repository != "acme/widgets" || !job.Forced {
		t.Fatalf("refresh job = %+v, want forced acme/widgets refresh", job)
	}
}

func TestRouteWarningsAreSanitizedPubliclyAndDetailedForAdmin(t *testing.T) {
	t.Parallel()

	app := newOperationsTestApplication(t, nil)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if err := app.Store().ReplaceRouteAttempts(
		context.Background(), "acme/widgets", storage.RouteOperationDiscovery,
		[]storage.RouteAttempt{
			{Profile: "secret-profile-name", Priority: 0, ProfileType: storage.RouteProfileGitHubApp, Outcome: storage.RouteOutcomeAdvanced, FailureCategory: storage.RouteFailureCredentialInvalid, Detail: "bounded credential failure", Remediation: "replace configured credential"},
			{Profile: "public-fallback", Priority: 1, ProfileType: storage.RouteProfileFineGrainedPAT, Outcome: storage.RouteOutcomeSelected, Selected: true},
		}, now,
	); err != nil {
		t.Fatal(err)
	}
	public := httptest.NewRecorder()
	app.Handler().ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/api/collections", http.NoBody))
	if public.Code != http.StatusOK {
		t.Fatalf("public status = %d: %s", public.Code, public.Body.String())
	}
	if !strings.Contains(public.Body.String(), `"routeWarnings"`) ||
		!strings.Contains(public.Body.String(), `"status":"fallback"`) {
		t.Fatalf("public response missing route warning: %s", public.Body.String())
	}
	for _, forbidden := range []string{
		"secret-profile-name", "public-fallback", "github_app", "fine_grained_pat",
		"credential_invalid", "bounded credential failure", "replace configured credential",
	} {
		if strings.Contains(public.Body.String(), forbidden) {
			t.Errorf("public response exposed %q: %s", forbidden, public.Body.String())
		}
	}

	session := storage.Session{
		ID: "route-admin", UserID: 42, Login: "admin", CSRFToken: "csrf",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := app.Store().CreateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/admin/status", http.NoBody)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session.ID})
	admin := httptest.NewRecorder()
	app.Handler().ServeHTTP(admin, request)
	if admin.Code != http.StatusOK {
		t.Fatalf("admin status = %d: %s", admin.Code, admin.Body.String())
	}
	for _, expected := range []string{
		`"githubRoutes"`, "secret-profile-name", "public-fallback", "github_app",
		"credential_invalid", "replace configured credential",
	} {
		if !strings.Contains(admin.Body.String(), expected) {
			t.Errorf("admin response missing %q: %s", expected, admin.Body.String())
		}
	}
}

func newOperationsTestApplication(
	t *testing.T,
	reload func(context.Context) error,
) *Application {
	t.Helper()
	directory := t.TempDir()
	secretPath := filepath.Join(directory, "reload-secret")
	if err := os.WriteFile(secretPath, []byte("reload-test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "config.yaml")
	body := `external_base_url: https://cockpit.example.test
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
    client_id: test
    client_secret:
      file: ` + secretPath + `
deployment_admins: [42]
operations:
  reload_secret:
    file: ` + secretPath + `
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := NewWithOptions(
		context.Background(), configPath, filepath.Join(directory, "app.db"),
		Options{AuthProvider: &fakeAuthProvider{}, Now: func() time.Time {
			return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
		}, Reload: reload},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}
