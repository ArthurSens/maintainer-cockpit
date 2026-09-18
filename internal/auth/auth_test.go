package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGitHubProviderVerifiesIdentityOrganizationsAndInheritedTeamMembership(t *testing.T) {
	t.Parallel()

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/login/oauth/access_token":
			body, _ := io.ReadAll(r.Body)
			values, _ := url.ParseQuery(string(body))
			if values.Get("code_verifier") != "verifier" {
				t.Errorf("code_verifier = %q", values.Get("code_verifier"))
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "ghu_ephemeral"})
		case "/user":
			if r.Header.Get("Authorization") != "Bearer ghu_ephemeral" {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, `{
				"id":42,
				"login":"octocat",
				"avatar_url":"https://avatars.githubusercontent.com/u/42?v=4"
			}`)
		case "/user/memberships/orgs/acme":
			_, _ = io.WriteString(w, `{"state":"active"}`)
		case "/orgs/acme/teams/maintainers/memberships/octocat":
			// GitHub documents that this endpoint includes inherited
			// membership from child teams.
			_, _ = io.WriteString(w, `{"state":"active","role":"member"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	provider, err := NewGitHubProvider(GitHubOptions{
		ClientID: "Iv1.example", ClientSecret: "secret",
		OAuthBaseURL: server.URL, APIBaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, evidence, err := provider.Authenticate(
		context.Background(), "temporary-code", "verifier", "https://cockpit.example.test/auth/callback",
		Requirements{Organizations: []string{"acme"}, Teams: []string{"acme/maintainers"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != 42 || identity.Login != "octocat" ||
		identity.AvatarURL != "https://avatars.githubusercontent.com/u/42?v=4" ||
		len(evidence.Organizations) != 1 || len(evidence.Teams) != 1 {
		t.Fatalf("identity = %+v, evidence = %+v", identity, evidence)
	}
	if got := strings.Join(requests, "\n"); !strings.Contains(got,
		"GET /orgs/acme/teams/maintainers/memberships/octocat") {
		t.Errorf("requests = %s, want team membership verification", got)
	}
}

func TestGitHubProviderFailsClosedWhenMembershipCannotBeVerified(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			_, _ = io.WriteString(w, `{"access_token":"ghu_ephemeral"}`)
		case "/user":
			_, _ = io.WriteString(w, `{"id":42,"login":"octocat"}`)
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	provider, err := NewGitHubProvider(GitHubOptions{
		ClientID: "Iv1.example", ClientSecret: "secret",
		OAuthBaseURL: server.URL, APIBaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = provider.Authenticate(
		context.Background(), "temporary-code", "verifier", "https://cockpit.example.test/auth/callback",
		Requirements{Organizations: []string{"acme"}},
	)
	if err == nil || !strings.Contains(err.Error(), "verify organization") {
		t.Fatalf("Authenticate() error = %v, want unverifiable membership failure", err)
	}
}
