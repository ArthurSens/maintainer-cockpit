package main

import (
	"strings"
	"testing"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

func TestNewGitHubClientsBuildsEveryProfileAndRedactsSecretFailures(t *testing.T) {
	const secret = "not-a-private-key-secret"
	t.Setenv("APP_KEY", secret)
	t.Setenv("PAT_TOKEN", "github_pat_test")
	appID, installationID := int64(1), int64(2)
	loaded := config.Config{
		GitHub: &config.GitHub{Schedule: "0 * * * *", Authentication: &config.GitHubAuthentication{
			Profiles: map[string]config.GitHubAuthenticationProfile{
				"app": {
					Type: "github_app", AppID: &appID, InstallationID: &installationID,
					PrivateKey: &config.SecretReference{Environment: "APP_KEY"},
				},
				"pat": {
					Type:  "fine_grained_pat",
					Token: &config.SecretReference{Environment: "PAT_TOKEN"},
				},
			},
			Owners: map[string]config.GitHubAuthenticationOwner{
				"acme": {Profiles: []string{"app", "pat"}},
			},
		}},
		Collections: []config.Collection{{
			ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
		}},
	}

	_, err := newGitHubClients(loaded)
	if err == nil || !strings.Contains(err.Error(), `profile "app"`) {
		t.Fatalf("newGitHubClients() error = %v, want profile-scoped error", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("newGitHubClients() exposed secret: %v", err)
	}
}

func TestNewGitHubClientsRejectsMissingAuthentication(t *testing.T) {
	_, err := newGitHubClients(config.Config{
		GitHub: &config.GitHub{},
		Collections: []config.Collection{{
			ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "github.authentication is required") {
		t.Fatalf("newGitHubClients() error = %v", err)
	}
}

func TestNewGitHubClientsResolvesAllSecretsBeforeReturning(t *testing.T) {
	t.Setenv("FIRST_PAT", "github_pat_first")
	t.Setenv("MISSING_PAT", "")
	loaded := patConfig("FIRST_PAT", "MISSING_PAT")
	_, err := newGitHubClients(loaded)
	if err == nil || !strings.Contains(err.Error(), `profile "second"`) {
		t.Fatalf("newGitHubClients() error = %v, want second profile error", err)
	}
}

func patConfig(environmentNames ...string) config.Config {
	profiles := make(map[string]config.GitHubAuthenticationProfile, len(environmentNames))
	names := make([]string, 0, len(environmentNames))
	for index, environment := range environmentNames {
		name := []string{"first", "second"}[index]
		profiles[name] = config.GitHubAuthenticationProfile{
			Type:  "fine_grained_pat",
			Token: &config.SecretReference{Environment: environment},
		}
		names = append(names, name)
	}
	return config.Config{
		GitHub: &config.GitHub{Schedule: "0 * * * *", Authentication: &config.GitHubAuthentication{
			Profiles: profiles,
			Owners: map[string]config.GitHubAuthenticationOwner{
				"acme": {Profiles: names},
			},
		}},
		Collections: []config.Collection{{
			ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
		}},
	}
}
