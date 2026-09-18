package application

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/auth"
	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestLoginUnlocksPersonalWorkflows(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	provider := &fakeAuthProvider{
		identity: auth.Identity{
			ID: 42, Login: "octocat",
			AvatarURL: "https://avatars.githubusercontent.com/u/42?v=4",
		},
		evidence: auth.Evidence{Teams: []string{"acme/maintainers"}},
	}
	app := newAuthenticatedTestApplication(t, provider, func() time.Time { return now })

	public := performRequest(app.Handler(), http.MethodGet,
		"/api/collections/acme/pull-requests/acme/widgets/7", "", "", nil)
	if public.Code != http.StatusOK {
		t.Fatalf("public detail = %d %s, want 200", public.Code, public.Body.String())
	}
	publicList := performRequest(app.Handler(), http.MethodGet,
		"/api/collections/acme/pull-requests", "", "", nil)
	if publicList.Code != http.StatusOK {
		t.Fatalf("public list = %d %s, want 200", publicList.Code, publicList.Body.String())
	}

	login := performRequest(app.Handler(), http.MethodGet, "/auth/login", "", "", nil)
	if login.Code != http.StatusFound {
		t.Fatalf("login status = %d, body = %s", login.Code, login.Body.String())
	}
	flowCookie := login.Result().Cookies()[0]
	location, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := location.Query().Get("state")
	if state == "" || location.Query().Get("code_challenge") == "" {
		t.Fatalf("authorization redirect = %s, want state and PKCE challenge", location)
	}
	callback := performRequest(app.Handler(), http.MethodGet,
		"/auth/callback?code=temporary&state="+url.QueryEscape(state), "", "", []*http.Cookie{flowCookie})
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, body = %s", callback.Code, callback.Body.String())
	}
	if provider.codeVerifier == "" {
		t.Fatal("Authenticate() code verifier is empty")
	}
	sessionCookie := cookieNamed(t, callback.Result().Cookies(), "__Host-maintainer-cockpit")
	if !sessionCookie.Secure || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie = %+v, want Secure, HttpOnly, SameSite=Lax", sessionCookie)
	}

	viewer := performRequest(app.Handler(), http.MethodGet, "/api/viewer", "", "", []*http.Cookie{sessionCookie})
	if viewer.Code != http.StatusOK ||
		!strings.Contains(viewer.Body.String(), `"id":42`) ||
		!strings.Contains(viewer.Body.String(), `"avatarURL":"https://avatars.githubusercontent.com/u/42?v=4"`) ||
		!strings.Contains(viewer.Body.String(), `"authorizedCollections":["acme"]`) {
		t.Fatalf("viewer = %d %s", viewer.Code, viewer.Body.String())
	}
	csrf := jsonStringField(t, viewer.Body.String(), "csrfToken")

	restricted := performRequest(app.Handler(), http.MethodGet,
		"/api/collections/acme/pull-requests/acme/widgets/7", "", "", []*http.Cookie{sessionCookie})
	if restricted.Code != http.StatusOK {
		t.Fatalf("authorized detail = %d %s, want 200", restricted.Code, restricted.Body.String())
	}
	if got := restricted.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("authorized detail Cache-Control = %q, want private, no-store", got)
	}

	badLogout := performRequest(app.Handler(), http.MethodPost, "/auth/logout", csrf, "",
		[]*http.Cookie{sessionCookie})
	if badLogout.Code != http.StatusForbidden {
		t.Fatalf("logout without trusted Origin = %d, want 403", badLogout.Code)
	}
	deleteResponse := performRequest(app.Handler(), http.MethodDelete, "/api/me/data", csrf,
		"https://cockpit.example.test", []*http.Cookie{sessionCookie})
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete personal data = %d %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	afterDelete := performRequest(app.Handler(), http.MethodGet, "/api/viewer", "", "",
		[]*http.Cookie{sessionCookie})
	if !strings.Contains(afterDelete.Body.String(), `"authenticated":false`) {
		t.Fatalf("viewer after deletion = %s", afterDelete.Body.String())
	}

	sessionCookie = loginTestUser(t, app.Handler())
	viewer = performRequest(app.Handler(), http.MethodGet, "/api/viewer", "", "",
		[]*http.Cookie{sessionCookie})
	csrf = jsonStringField(t, viewer.Body.String(), "csrfToken")
	logout := performRequest(app.Handler(), http.MethodPost, "/auth/logout", csrf,
		"https://cockpit.example.test", []*http.Cookie{sessionCookie})
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", logout.Code, logout.Body.String())
	}
}

func TestAuthorizationChangesAndExpiredSessionsFailClosed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	current := now
	provider := &fakeAuthProvider{
		identity: auth.Identity{ID: 42, Login: "octocat"},
		evidence: auth.Evidence{Organizations: []string{"acme"}},
	}
	app := newAuthenticatedTestApplication(t, provider, func() time.Time { return current })
	sessionCookie := loginTestUser(t, app.Handler())

	next := authenticatedTestConfig()
	next.Collections[0].Authorization = &config.Authorization{Users: []int64{99}}
	if err := app.ApplyConfig(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	response := performRequest(app.Handler(), http.MethodGet,
		"/api/collections/acme/pull-requests/acme/widgets/7", "", "", []*http.Cookie{sessionCookie})
	if response.Code != http.StatusOK {
		t.Fatalf("public detail after authorization policy changed = %d %s, want 200", response.Code, response.Body.String())
	}

	current = now.Add(24 * time.Hour)
	viewer := performRequest(app.Handler(), http.MethodGet, "/api/viewer", "", "",
		[]*http.Cookie{sessionCookie})
	if !strings.Contains(viewer.Body.String(), `"authenticated":false`) {
		t.Fatalf("viewer at session expiry = %s", viewer.Body.String())
	}
}

func TestUnverifiableLoginCreatesNoSessionAndPublicDashboardRemainsAvailable(t *testing.T) {
	t.Parallel()

	provider := &fakeAuthProvider{err: errors.New("GitHub unavailable")}
	app := newAuthenticatedTestApplication(t, provider, time.Now)
	flow := performRequest(app.Handler(), http.MethodGet, "/auth/login", "", "", nil)
	location, _ := url.Parse(flow.Header().Get("Location"))
	callback := performRequest(app.Handler(), http.MethodGet,
		"/auth/callback?code=temporary&state="+url.QueryEscape(location.Query().Get("state")),
		"", "", []*http.Cookie{flow.Result().Cookies()[0]})
	if callback.Code != http.StatusBadGateway ||
		cookieByName(callback.Result().Cookies(), "__Host-maintainer-cockpit") != nil {
		t.Fatalf("unverifiable callback = %d, cookies=%v", callback.Code, callback.Result().Cookies())
	}
	public := performRequest(app.Handler(), http.MethodGet, "/api/collections", "", "", nil)
	if public.Code != http.StatusOK {
		t.Fatalf("public collections status = %d after login failure", public.Code)
	}
}

type fakeAuthProvider struct {
	identity     auth.Identity
	evidence     auth.Evidence
	err          error
	codeVerifier string
}

func (*fakeAuthProvider) AuthorizationURL(state, challenge, redirectURI string) string {
	values := url.Values{
		"state": {state}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "redirect_uri": {redirectURI},
	}
	return "https://github.example.test/login/oauth/authorize?" + values.Encode()
}

func (provider *fakeAuthProvider) Authenticate(
	_ context.Context, _, verifier, _ string, _ auth.Requirements,
) (auth.Identity, auth.Evidence, error) {
	provider.codeVerifier = verifier
	return provider.identity, provider.evidence, provider.err
}

func newAuthenticatedTestApplication(
	t *testing.T, provider auth.Provider, now func() time.Time,
) *Application {
	t.Helper()
	configPath := writeConfig(t, authenticatedTestConfigYAML)
	app, err := NewWithOptions(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"),
		Options{AuthProvider: provider, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	if err := app.Store().UpsertPullRequest(context.Background(), "acme", storage.PullRequest{
		Repository: "acme/widgets", Number: 7, Title: "Authentication",
		UpdatedAt: time.Now().UTC(), URL: "https://github.com/acme/widgets/pull/7",
	}); err != nil {
		t.Fatal(err)
	}
	return app
}

const authenticatedTestConfigYAML = `
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
    model_provider: ""
    authorization:
      organizations: [acme]
      teams: [acme/maintainers]
`

func authenticatedTestConfig() config.Config {
	parsed, err := config.Parse([]byte(authenticatedTestConfigYAML))
	if err != nil {
		panic(err)
	}
	return parsed
}

func loginTestUser(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	login := performRequest(handler, http.MethodGet, "/auth/login", "", "", nil)
	location, _ := url.Parse(login.Header().Get("Location"))
	callback := performRequest(handler, http.MethodGet,
		"/auth/callback?code=temporary&state="+url.QueryEscape(location.Query().Get("state")),
		"", "", []*http.Cookie{login.Result().Cookies()[0]})
	return cookieNamed(t, callback.Result().Cookies(), "__Host-maintainer-cockpit")
}

func performRequest(
	handler http.Handler, method, target, csrf, origin string, cookies []*http.Cookie,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, http.NoBody)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func cookieNamed(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	cookie := cookieByName(cookies, name)
	if cookie == nil {
		t.Fatalf("cookie %q missing from %v", name, cookies)
	}
	return cookie
}

func cookieByName(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func jsonStringField(t *testing.T, body, name string) string {
	t.Helper()
	prefix := `"` + name + `":"`
	start := strings.Index(body, prefix)
	if start < 0 {
		t.Fatalf("field %q missing from %s", name, body)
	}
	value := body[start+len(prefix):]
	end := strings.IndexByte(value, '"')
	if end < 0 {
		t.Fatalf("field %q malformed in %s", name, body)
	}
	return value[:end]
}
