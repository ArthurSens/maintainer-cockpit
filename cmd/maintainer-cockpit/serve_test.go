package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/githubapp"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestModelProviderHTTPClientAllowsLongRunningInference(t *testing.T) {
	t.Parallel()

	client := modelProviderHTTPClient()
	if client.Timeout != 15*time.Minute {
		t.Errorf("model provider timeout = %s, want 15m", client.Timeout)
	}
}

func TestServerHandlerExposesPprofAndDelegatesApplication(t *testing.T) {
	t.Parallel()

	handler := newServerHandler(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusTeapot)
	}))
	for _, test := range []struct {
		path       string
		statusCode int
		contains   string
	}{
		{path: "/debug/pprof/", statusCode: http.StatusOK, contains: "Types of profiles available"},
		{path: "/debug/pprof/goroutine?debug=1", statusCode: http.StatusOK, contains: "goroutine profile"},
		{path: "/", statusCode: http.StatusTeapot},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, http.NoBody)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != test.statusCode {
			t.Errorf("GET %s status = %d, want %d", test.path, response.Code, test.statusCode)
		}
		if test.contains != "" && !strings.Contains(response.Body.String(), test.contains) {
			t.Errorf("GET %s body does not contain %q", test.path, test.contains)
		}
	}
}

func TestExternalWorkersAreDisabledForFixtureMode(t *testing.T) {
	t.Parallel()

	for _, fixture := range []string{"demo.json", "-"} {
		if externalWorkersEnabled(fixture) {
			t.Errorf("externalWorkersEnabled(%q) = true, want false", fixture)
		}
	}
	if !externalWorkersEnabled("") {
		t.Fatal("externalWorkersEnabled(no fixture) = false, want true")
	}
}

func TestSchedulerManagerWaitsForOldCollectorBeforeStartingReplacement(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const repository = "acme/widgets"
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{repository},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueRefresh(ctx, repository, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	manager := &schedulerManager{
		parent: ctx, store: store, runtimeErrors: make(chan error, 2),
	}
	first := &reloadTestCollector{started: make(chan struct{}), waitForCancellation: true}
	manager.replace(first)
	waitForCollector(t, first.started)

	second := &reloadTestCollector{started: make(chan struct{})}
	manager.replace(second)
	waitForCollector(t, second.started)
	manager.stop()
}

func TestRepositoriesWithChangedGitHubRouting(t *testing.T) {
	t.Parallel()

	oldConfig := routingTestConfig(map[string][]string{
		"acme":  {"primary", "fallback"},
		"other": {"other"},
	}, []string{"acme/one", "acme/two", "other/repo"})
	newConfig := routingTestConfig(map[string][]string{
		"acme":  {"fallback", "primary"},
		"other": {"other"},
	}, []string{"acme/one", "acme/two", "other/repo", "new/repo"})
	newConfig.GitHub.Authentication.Profiles["new"] = config.GitHubAuthenticationProfile{
		Type:  "fine_grained_pat",
		Token: &config.SecretReference{Environment: "NEW_PAT"},
	}
	newConfig.GitHub.Authentication.Owners["new"] = config.GitHubAuthenticationOwner{
		Profiles: []string{"new"},
	}

	got := repositoriesWithChangedGitHubRouting(oldConfig, newConfig)
	want := []string{"acme/one", "acme/two", "new/repo"}
	if len(got) != len(want) {
		t.Fatalf("changed repositories = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("changed repositories = %v, want %v", got, want)
		}
	}
}

func TestGitHubAuthenticationIdentityIncludesSecretReferences(t *testing.T) {
	t.Parallel()

	first := routingTestConfig(map[string][]string{"acme": {"primary"}}, []string{"acme/repo"})
	second := first
	second.GitHub = &config.GitHub{Authentication: &config.GitHubAuthentication{
		Profiles: map[string]config.GitHubAuthenticationProfile{
			"primary": {
				Type:  "fine_grained_pat",
				Token: &config.SecretReference{File: "/rotated/token"},
			},
		},
		Owners: map[string]config.GitHubAuthenticationOwner{
			"acme": {Profiles: []string{"primary"}},
		},
	}}
	if githubAuthenticationEqual(first, second) {
		t.Fatal("secret reference change did not change authentication identity")
	}
	got := repositoriesWithChangedGitHubRouting(first, second)
	if len(got) != 1 || got[0] != "acme/repo" {
		t.Fatalf("changed repositories = %v, want [acme/repo]", got)
	}

	appID, installationID := int64(1), int64(2)
	first.GitHub.Authentication.Profiles["primary"] = config.GitHubAuthenticationProfile{
		Type: "github_app", AppID: &appID, InstallationID: &installationID,
		PrivateKey: &config.SecretReference{Environment: "APP_KEY"},
	}
	second.GitHub.Authentication.Profiles["primary"] = config.GitHubAuthenticationProfile{
		Type: "github_app", AppID: &appID, InstallationID: &installationID,
		PrivateKey: &config.SecretReference{File: "/rotated/private-key.pem"},
	}
	if githubAuthenticationEqual(first, second) {
		t.Fatal("private key reference change did not change authentication identity")
	}
}

func TestUserAuthenticationAloneDoesNotConfigureCollectionAuthentication(t *testing.T) {
	t.Parallel()

	loaded := config.Config{GitHub: &config.GitHub{UserAuth: &config.UserAuth{
		ClientID:     "Iv1.example",
		ClientSecret: config.SecretReference{Environment: "GITHUB_CLIENT_SECRET"},
	}}}
	if hasGitHubCollectionAuthentication(loaded) {
		t.Fatal("user authentication was treated as scheduled collection authentication")
	}
	loaded.GitHub.Authentication = &config.GitHubAuthentication{}
	if !hasGitHubCollectionAuthentication(loaded) {
		t.Fatal("collection authentication was not detected")
	}
}

func TestRuntimeCredentialGenerationRevalidatesEveryAuthenticatedRepository(t *testing.T) {
	t.Parallel()

	previous := routingTestConfig(map[string][]string{
		"acme":  {"primary"},
		"other": {"other"},
	}, []string{"acme/one", "other/two"})
	next := routingTestConfig(map[string][]string{
		"acme":  {"primary"},
		"other": {"other"},
	}, []string{"acme/one", "other/two"})

	if got := repositoriesWithChangedGitHubRouting(previous, next); len(got) != 0 {
		t.Fatalf("structurally changed repositories = %v, want none", got)
	}
	got := repositoriesRequiringCredentialGenerationValidation(next)
	want := []string{"acme/one", "other/two"}
	if len(got) != len(want) {
		t.Fatalf("credential-generation repositories = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("credential-generation repositories = %v, want %v", got, want)
		}
	}
}

func TestEnqueueRoutingRefreshesRetriesWithIndependentContext(t *testing.T) {
	t.Parallel()

	reloadContext, cancel := context.WithCancel(context.Background())
	cancel()
	var contexts []context.Context
	warnings := enqueueRoutingRefreshes(
		reloadContext, []string{"acme/repo"}, time.Now(),
		func(ctx context.Context, repository string, _ time.Time, forced bool) (bool, error) {
			contexts = append(contexts, ctx)
			if repository != "acme/repo" || !forced {
				t.Fatalf("enqueue repository=%q forced=%v", repository, forced)
			}
			if len(contexts) == 1 {
				return false, context.Canceled
			}
			if ctx.Err() != nil {
				t.Fatalf("fallback context error = %v", ctx.Err())
			}
			return true, nil
		},
	)
	if len(warnings) != 0 {
		t.Fatalf("enqueue warnings = %v", warnings)
	}
	if len(contexts) != 2 || contexts[0] == contexts[1] {
		t.Fatalf("enqueue contexts = %v, want reload and independent fallback", contexts)
	}
}

func TestEnqueueRoutingRefreshesReturnsNonFatalWarningAfterRetryFailure(t *testing.T) {
	t.Parallel()

	attempts := 0
	warnings := enqueueRoutingRefreshes(
		context.Background(), []string{"acme/repo"}, time.Now(),
		func(context.Context, string, time.Time, bool) (bool, error) {
			attempts++
			return false, context.DeadlineExceeded
		},
	)
	if attempts != 2 {
		t.Fatalf("enqueue attempts = %d, want 2", attempts)
	}
	if len(warnings) != 1 {
		t.Fatalf("enqueue warnings = %v, want one non-fatal warning", warnings)
	}
}

func routingTestConfig(routes map[string][]string, repositories []string) config.Config {
	profiles := make(map[string]config.GitHubAuthenticationProfile)
	owners := make(map[string]config.GitHubAuthenticationOwner)
	for owner, names := range routes {
		owners[owner] = config.GitHubAuthenticationOwner{Profiles: append([]string(nil), names...)}
		for _, name := range names {
			profiles[name] = config.GitHubAuthenticationProfile{
				Type:  "fine_grained_pat",
				Token: &config.SecretReference{Environment: "TEST_PAT"},
			}
		}
	}
	return config.Config{
		GitHub: &config.GitHub{Schedule: "0 * * * *", Authentication: &config.GitHubAuthentication{
			Profiles: profiles, Owners: owners,
		}},
		Collections: []config.Collection{{
			ID: "test", Name: "Test", Repositories: repositories,
		}},
	}
}

type reloadTestCollector struct {
	started             chan struct{}
	waitForCancellation bool
}

func (collector *reloadTestCollector) DiscoverResult(
	ctx context.Context, repository string,
) (githubapp.RoutedDiscoveryResult, error) {
	close(collector.started)
	if collector.waitForCancellation {
		<-ctx.Done()
		return githubapp.RoutedDiscoveryResult{}, ctx.Err()
	}
	return githubapp.RoutedDiscoveryResult{Discovery: githubapp.DiscoveryResult{
		Repository: repository,
	}}, nil
}

func (*reloadTestCollector) HydratePullRequestsResult(
	context.Context,
	string,
	[]githubapp.DiscoveredPullRequest,
	githubapp.HydrationOptions,
) (githubapp.RoutedHydrationResult, error) {
	return githubapp.RoutedHydrationResult{}, nil
}

func (*reloadTestCollector) CollectProgressResult(
	context.Context, string, githubapp.ProgressOptions,
) (githubapp.RoutedProgressResult, error) {
	return githubapp.RoutedProgressResult{}, nil
}

func waitForCollector(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("collector did not start")
	}
}
