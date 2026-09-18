package githubapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPATUsesBearerAuthorizationForRESTAndGraphQL(t *testing.T) {
	t.Parallel()

	const token = "github_pat_secret"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q, want PAT bearer token", got)
		}
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	client, err := NewPAT(PATOptions{
		Token: token, BaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewPAT() error = %v", err)
	}
	credential, err := client.accessToken(context.Background(), []string{"acme/widgets"})
	if err != nil {
		t.Fatalf("accessToken() error = %v", err)
	}
	var response map[string]any
	if err := client.requestJSON(context.Background(), http.MethodGet, "/repos/acme/widgets", credential, nil, &response); err != nil {
		t.Fatalf("REST request error = %v", err)
	}
	if err := client.requestJSON(context.Background(), http.MethodPost, "/graphql", credential, map[string]string{"query": "{}"}, &response); err != nil {
		t.Fatalf("GraphQL request error = %v", err)
	}
	if len(paths) != 2 || paths[0] != "/repos/acme/widgets" || paths[1] != "/graphql" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestPATTokenIsRedactedFromErrors(t *testing.T) {
	t.Parallel()

	const token = "github_pat_never_print_me"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, token, http.StatusUnauthorized)
	}))
	defer server.Close()

	client, err := NewPAT(PATOptions{
		Token: token, BaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewPAT() error = %v", err)
	}
	var response map[string]any
	err = client.requestJSON(context.Background(), http.MethodGet, "/user", token, nil, &response)
	if err == nil {
		t.Fatal("requestJSON() error = nil")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error exposed PAT: %v", err)
	}
}

func TestRouterRecordsHTTPClientTimeoutAsProviderAttempt(t *testing.T) {
	t.Parallel()

	client, err := NewPAT(PATOptions{
		Token: "secret-token", BaseURL: "https://api.github.test",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, context.DeadlineExceeded
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(map[string][]Profile{
		"acme": {{Name: "primary", Type: ProfileFineGrainedPAT, Client: client}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	result, err := router.DiscoverResult(ctx, "acme/widgets")
	if err == nil {
		t.Fatal("DiscoverResult() error = nil")
	}
	if ctx.Err() != nil || len(result.Attempts) != 1 ||
		result.Attempts[0].Failure != FailureGitHubUnavailable ||
		result.Attempts[0].Outcome != RouteOutcomeFailed {
		t.Fatalf("context error = %v, attempts = %+v, error = %v", ctx.Err(), result.Attempts, err)
	}
	var transportErr *TransportError
	if !errors.As(err, &transportErr) || !transportErr.TimedOut {
		t.Fatalf("error = %T %v, want timed-out TransportError", err, err)
	}
}

func TestPATSetupCheckProbesTargetRESTAndAuthenticatedGraphQL(t *testing.T) {
	t.Parallel()

	const token = "github_pat_setup_secret"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets":
			_, _ = io.WriteString(w, `{"full_name":"acme/widgets"}`)
		case "/graphql":
			_, _ = io.WriteString(w, `{"data":{"viewer":{"login":"operator"},"repository":{"nameWithOwner":"acme/widgets"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewPAT(PATOptions{
		Token: token, BaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	setup, err := client.SetupCheckPAT(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("SetupCheckPAT() error = %v", err)
	}
	if setup.AccountLogin != "operator" {
		t.Errorf("SetupCheckPAT() = %+v", setup)
	}
	if len(paths) != 2 || paths[0] != "/repos/acme/widgets" || paths[1] != "/graphql" {
		t.Errorf("probe paths = %v", paths)
	}
}

func TestRouterDoesNotFallbackForTransportFailureAndRedactsPAT(t *testing.T) {
	t.Parallel()

	const token = "github_pat_transport_secret"
	primary, err := NewPAT(PATOptions{
		Token: token,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport echoed " + token)
		})},
	})
	if err != nil {
		t.Fatalf("NewPAT() error = %v", err)
	}
	fallback, fallbackCalls := emptyPATClient(t, http.StatusOK, "[]")
	router, err := NewRouter(map[string][]Profile{
		"acme": {
			{Name: "primary", Type: ProfileFineGrainedPAT, Client: primary},
			{Name: "fallback", Type: ProfileFineGrainedPAT, Client: fallback},
		},
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	_, err = router.DiscoverResult(context.Background(), "acme/widgets")
	if err == nil {
		t.Fatal("DiscoverResult() error = nil")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error exposed PAT: %v", err)
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback calls = %d, want 0", fallbackCalls.Load())
	}
}

func TestDiscoveryIgnoresOtherOwnerRouteFailure(t *testing.T) {
	t.Parallel()

	acme, acmeCalls := emptyPATClient(t, http.StatusOK, "[]")
	other, otherCalls := emptyPATClient(t, http.StatusBadGateway, `{"message":"unavailable"}`)
	router, err := NewRouter(map[string][]Profile{
		"Acme":  {{Name: "acme-primary", Type: ProfileFineGrainedPAT, Client: acme}},
		"other": {{Name: "other-primary", Type: ProfileFineGrainedPAT, Client: other}},
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	if _, err := router.DiscoverResult(context.Background(), "aCmE/widgets"); err != nil {
		t.Fatalf("DiscoverResult() error = %v", err)
	}
	if acmeCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("calls acme=%d other=%d", acmeCalls.Load(), otherCalls.Load())
	}
}

func TestRepositoryContributionEvidenceUsesRepositoryOwnerRoute(t *testing.T) {
	t.Parallel()

	acme, acmeCalls := emptyPATClient(t, http.StatusBadGateway, `{"message":"unavailable"}`)
	other, otherCalls := emptyPATClient(
		t, http.StatusOK, `{"total_count":0,"incomplete_results":false,"items":[]}`,
	)
	router, err := NewRouter(map[string][]Profile{
		"acme":  {{Name: "acme-primary", Type: ProfileFineGrainedPAT, Client: acme}},
		"other": {{Name: "other-primary", Type: ProfileFineGrainedPAT, Client: other}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := router.CollectRepositoryContribution(
		context.Background(), 42, "octocat", "other/tools", "",
	)
	if err != nil {
		t.Fatalf("CollectRepositoryContribution() error = %v", err)
	}
	if acmeCalls.Load() != 0 || otherCalls.Load() != 1 {
		t.Fatalf("calls acme=%d other=%d", acmeCalls.Load(), otherCalls.Load())
	}
	if len(result.Attempts) != 1 ||
		result.Attempts[0].Operation != OperationContributionEvidence ||
		result.Attempts[0].Outcome != RouteOutcomeSelected {
		t.Errorf("attempts = %+v", result.Attempts)
	}
}

func TestRepositoryContributionEvidencePreservesRouteFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		status       int
		body         string
		rateLimited  bool
		wantFallback bool
		wantFailure  FailureCategory
	}{
		{
			name: "credential advances", status: http.StatusUnauthorized,
			body: `{"message":"bad credentials"}`, wantFallback: true,
			wantFailure: FailureCredentialInvalid,
		},
		{
			name: "permission advances", status: http.StatusForbidden,
			body: `{"message":"forbidden"}`, wantFallback: true,
			wantFailure: FailurePermissionMissing,
		},
		{
			name: "rate limit does not advance", status: http.StatusForbidden,
			body: `{"message":"rate limit"}`, rateLimited: true,
			wantFailure: FailurePrimaryRateLimited,
		},
		{
			name: "server error does not advance", status: http.StatusBadGateway,
			body: `{"message":"unavailable"}`, wantFailure: FailureGitHubUnavailable,
		},
		{
			name: "malformed response does not advance", status: http.StatusOK,
			body: `{`, wantFailure: FailureUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			primary, _ := emptyPATClientWithResponse(
				t, test.status, test.body, test.rateLimited,
			)
			fallback, fallbackCalls := emptyPATClient(
				t, http.StatusOK,
				`{"total_count":0,"incomplete_results":false,"items":[]}`,
			)
			router, err := NewRouter(map[string][]Profile{
				"acme": {
					{Name: "primary", Type: ProfileFineGrainedPAT, Client: primary},
					{Name: "fallback", Type: ProfileFineGrainedPAT, Client: fallback},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := router.CollectRepositoryContribution(
				context.Background(), 42, "octocat", "acme/widgets", "",
			)
			if test.wantFallback {
				if err != nil {
					t.Fatalf("CollectRepositoryContribution() error = %v", err)
				}
				if fallbackCalls.Load() != 1 {
					t.Fatalf("fallback calls = %d, want 1", fallbackCalls.Load())
				}
				if len(result.Attempts) != 2 ||
					result.Attempts[0].Outcome != RouteOutcomeAdvanced ||
					result.Attempts[0].Failure != test.wantFailure {
					t.Fatalf("attempts = %+v", result.Attempts)
				}
				return
			}
			if err == nil {
				t.Fatal("CollectRepositoryContribution() error = nil")
			}
			if fallbackCalls.Load() != 0 {
				t.Fatalf("fallback calls = %d, want 0", fallbackCalls.Load())
			}
			if len(result.Attempts) != 1 ||
				result.Attempts[0].Failure != test.wantFailure {
				t.Fatalf("attempts = %+v", result.Attempts)
			}
		})
	}
}

func TestContributionAppCredentialFailureRefreshesTokenBeforeFallback(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	var tokenMints atomic.Int32
	var searchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			mint := tokenMints.Add(1)
			_, _ = io.WriteString(w, `{"token":"token-`+string('0'+mint)+`","expires_at":"2026-09-11T14:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			call := searchCalls.Add(1)
			if call == 1 {
				http.Error(w, `{"message":"bad credentials"}`, http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"total_count":0,"incomplete_results":false,"items":[]}`)
		default:
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	app, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	fallback, fallbackCalls := emptyPATClient(
		t, http.StatusOK, `{"total_count":0,"incomplete_results":false,"items":[]}`,
	)
	router, err := NewRouter(map[string][]Profile{"acme": {
		{Name: "app", Type: ProfileGitHubApp, Client: app},
		{Name: "fallback", Type: ProfileFineGrainedPAT, Client: fallback},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.CollectRepositoryContribution(
		context.Background(), 42, "octocat", "acme/widgets", "",
	); err != nil {
		t.Fatal(err)
	}
	if tokenMints.Load() != 2 || searchCalls.Load() != 2 || fallbackCalls.Load() != 0 {
		t.Fatalf("token mints=%d search calls=%d fallback calls=%d",
			tokenMints.Load(), searchCalls.Load(), fallbackCalls.Load())
	}
}

func TestRouterFallsBackInOrderAndRetriesPrimaryEveryOperation(t *testing.T) {
	t.Parallel()

	primary, primaryCalls := emptyPATClient(t, http.StatusNotFound, `{"message":"not found"}`)
	secondary, secondaryCalls := emptyPATClient(t, http.StatusOK, "[]")
	tertiary, tertiaryCalls := emptyPATClient(t, http.StatusOK, "[]")
	router, err := NewRouter(map[string][]Profile{
		"acme": {
			{Name: "primary", Type: ProfileFineGrainedPAT, Client: primary},
			{Name: "secondary", Type: ProfileFineGrainedPAT, Client: secondary},
			{Name: "tertiary", Type: ProfileFineGrainedPAT, Client: tertiary},
		},
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	result, err := router.DiscoverResult(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("DiscoverResult() error = %v", err)
	}
	if len(result.Attempts) != 2 ||
		result.Attempts[0].Outcome != RouteOutcomeAdvanced ||
		result.Attempts[0].Failure != FailureRepositoryNotGranted ||
		result.Attempts[1].Outcome != RouteOutcomeSelected {
		t.Fatalf("attempts = %+v", result.Attempts)
	}
	if _, err := router.DiscoverResult(context.Background(), "acme/widgets"); err != nil {
		t.Fatalf("DiscoverResult() error = %v", err)
	}
	if primaryCalls.Load() != 2 || secondaryCalls.Load() != 2 || tertiaryCalls.Load() != 0 {
		t.Fatalf(
			"calls primary=%d secondary=%d tertiary=%d",
			primaryCalls.Load(), secondaryCalls.Load(), tertiaryCalls.Load(),
		)
	}
}

func TestRouterRoutesDiscoveryHydrationAndProgressPhases(t *testing.T) {
	t.Parallel()

	primary, primaryCalls := emptyPATClient(
		t, http.StatusUnauthorized, `{"message":"bad credentials"}`,
	)
	var selectedCalls atomic.Int32
	selectedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selectedCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/acme/widgets/pulls" && r.URL.Query().Get("state") == "open":
			_, _ = io.WriteString(w, `[{"number":7}]`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7":
			_, _ = io.WriteString(w, `{
				"number":7,"title":"Routed","user":{"login":"octocat"},
				"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-12T11:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/7"
			}`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7/reviews":
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/repos/acme/widgets/pulls" && r.URL.Query().Get("state") == "all":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(selectedServer.Close)
	selected, err := NewPAT(PATOptions{
		Token: "token", BaseURL: selectedServer.URL, HTTPClient: selectedServer.Client(),
		CollectProgress: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(map[string][]Profile{"acme": {
		{Name: "primary", Type: ProfileFineGrainedPAT, Client: primary},
		{Name: "selected", Type: ProfileFineGrainedPAT, Client: selected},
	}})
	if err != nil {
		t.Fatal(err)
	}

	discovery, err := router.DiscoverResult(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Attempts) != 2 ||
		discovery.Attempts[0].Operation != OperationDiscovery ||
		discovery.Attempts[0].Outcome != RouteOutcomeAdvanced ||
		discovery.Attempts[1].Outcome != RouteOutcomeSelected {
		t.Fatalf("discovery attempts = %+v", discovery.Attempts)
	}
	hydration, err := router.HydratePullRequestsResult(
		context.Background(), "acme/widgets", discovery.Discovery.PullRequests, HydrationOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(hydration.Hydration.PullRequests) != 1 || len(hydration.Attempts) != 2 ||
		hydration.Attempts[0].Operation != OperationHydration {
		t.Fatalf("hydration = %+v", hydration)
	}
	progress, err := router.CollectProgressResult(
		context.Background(), "acme/widgets",
		ProgressOptions{ProgressSince: time.Now().Add(-time.Hour)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Progress.Repository != "acme/widgets" || len(progress.Attempts) != 2 ||
		progress.Attempts[0].Operation != OperationProgress {
		t.Fatalf("progress = %+v", progress)
	}
	if primaryCalls.Load() != 3 || selectedCalls.Load() != 4 {
		t.Fatalf("calls primary=%d selected=%d", primaryCalls.Load(), selectedCalls.Load())
	}
}

func TestRouterHydrationRateLimitDoesNotFallback(t *testing.T) {
	t.Parallel()

	primary, _ := emptyPATClientWithResponse(
		t, http.StatusForbidden, `{"message":"rate limit"}`, true,
	)
	fallback, fallbackCalls := emptyPATClient(t, http.StatusOK, `{}`)
	router, err := NewRouter(map[string][]Profile{"acme": {
		{Name: "primary", Type: ProfileFineGrainedPAT, Client: primary},
		{Name: "fallback", Type: ProfileFineGrainedPAT, Client: fallback},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := router.HydratePullRequestsResult(
		context.Background(), "acme/widgets",
		[]DiscoveredPullRequest{{Number: 7}}, HydrationOptions{},
	)
	if err == nil {
		t.Fatal("HydratePullRequestsResult() error = nil")
	}
	if fallbackCalls.Load() != 0 || len(result.Attempts) != 1 ||
		result.Attempts[0].Failure != FailurePrimaryRateLimited ||
		result.Attempts[0].Outcome != RouteOutcomeFailed {
		t.Fatalf("attempts = %+v, fallback calls = %d", result.Attempts, fallbackCalls.Load())
	}
}

func TestRouterDoesNotFallbackForUnboundedFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		body        string
		cancel      bool
		rateLimited bool
	}{
		{name: "rate limit", status: http.StatusForbidden, body: `{"message":"rate limit"}`, rateLimited: true},
		{name: "server error", status: http.StatusBadGateway, body: `{"message":"unavailable"}`},
		{name: "cancelled", cancel: true},
		{name: "decode", status: http.StatusOK, body: `{`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			primary, _ := emptyPATClientWithResponse(t, tt.status, tt.body, tt.rateLimited)
			fallback, fallbackCalls := emptyPATClient(t, http.StatusOK, "[]")
			router, err := NewRouter(map[string][]Profile{
				"acme": {
					{Name: "primary", Type: ProfileFineGrainedPAT, Client: primary},
					{Name: "fallback", Type: ProfileFineGrainedPAT, Client: fallback},
				},
			})
			if err != nil {
				t.Fatalf("NewRouter() error = %v", err)
			}
			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := router.DiscoverResult(ctx, "acme/widgets"); err == nil {
				t.Fatal("DiscoverResult() error = nil")
			}
			if fallbackCalls.Load() != 0 {
				t.Fatalf("fallback calls = %d, want 0", fallbackCalls.Load())
			}
			if tt.cancel {
				return
			}
			result, _ := router.DiscoverResult(context.Background(), "acme/widgets")
			if len(result.Attempts) != 1 || result.Attempts[0].Outcome != RouteOutcomeFailed {
				t.Fatalf("attempts = %+v, want current profile failure", result.Attempts)
			}
		})
	}
}

func TestClassifyNonAdvanceFailureIsDeterministicAndBounded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want FailureCategory
	}{
		{name: "primary limit", err: &RateLimitError{Reason: "primary"}, want: FailurePrimaryRateLimited},
		{name: "secondary limit", err: &RateLimitError{Reason: "secondary"}, want: FailureSecondaryRateLimited},
		{name: "server", err: &APIError{StatusCode: http.StatusBadGateway}, want: FailureGitHubUnavailable},
		{name: "transport", err: &TransportError{}, want: FailureGitHubUnavailable},
		{name: "decode", err: errors.New("secret arbitrary detail"), want: FailureUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			category, detail, remediation := classifyNonAdvanceFailure(tt.err)
			if category != tt.want {
				t.Fatalf("category = %q, want %q", category, tt.want)
			}
			if strings.Contains(detail+remediation, "secret arbitrary detail") {
				t.Fatalf("classification retained arbitrary error: %q / %q", detail, remediation)
			}
		})
	}
}

func TestAppAuthenticationFailureRefreshesTokenOnceBeforeFallback(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	var tokenMints atomic.Int32
	var collectionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			mint := tokenMints.Add(1)
			_, _ = io.WriteString(w, `{"token":"token-`+string('0'+mint)+`","expires_at":"2026-09-11T14:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls":
			call := collectionCalls.Add(1)
			if call == 1 {
				http.Error(w, `{"message":"bad credentials"}`, http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer token-2" {
				t.Errorf("Authorization after refresh = %q", got)
			}
			_, _ = io.WriteString(w, `[]`)
		default:
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	app, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	fallback, fallbackCalls := emptyPATClient(t, http.StatusOK, "[]")
	router, err := NewRouter(map[string][]Profile{
		"acme": {
			{Name: "app", Type: ProfileGitHubApp, Client: app},
			{Name: "fallback", Type: ProfileFineGrainedPAT, Client: fallback},
		},
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	if _, err := router.DiscoverResult(context.Background(), "acme/widgets"); err != nil {
		t.Fatalf("DiscoverResult() error = %v", err)
	}
	if tokenMints.Load() != 2 || collectionCalls.Load() != 2 || fallbackCalls.Load() != 0 {
		t.Fatalf(
			"token mints=%d collection calls=%d fallback calls=%d",
			tokenMints.Load(), collectionCalls.Load(), fallbackCalls.Load(),
		)
	}
}

func TestRoutingTypesAreStableAndBounded(t *testing.T) {
	t.Parallel()

	if OperationDiscovery != "discovery" ||
		OperationHydration != "hydration" ||
		OperationProgress != "progress" ||
		OperationContributionEvidence != "contribution_evidence" ||
		OperationPullRequestDiff != "pull_request_diff" {
		t.Fatalf("operation constants changed")
	}
	attempts := AttemptSet{{
		Operation: OperationDiscovery, Profile: "primary",
		ProfileType: ProfileGitHubApp, Outcome: RouteOutcomeFailed,
		Failure: FailureRouteMissing,
	}}
	result := RoutedDiscoveryResult{Attempts: attempts}
	if len(result.Attempts) != 1 || result.Attempts[0].Failure != FailureRouteMissing {
		t.Fatalf("RoutedDiscoveryResult attempts = %+v", result.Attempts)
	}
}

func emptyPATClient(t *testing.T, status int, body string) (*Client, *atomic.Int32) {
	t.Helper()
	return emptyPATClientWithResponse(t, status, body, false)
}

func emptyPATClientWithResponse(
	t *testing.T, status int, body string, rateLimited bool,
) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if rateLimited {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "0")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	client, err := NewPAT(PATOptions{
		Token: "test-token", BaseURL: server.URL, HTTPClient: server.Client(),
		RateLimitMaxRetries: 1,
		Sleep:               func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewPAT() error = %v", err)
	}
	return client, &calls
}

func TestRouterPreservesContextErrors(t *testing.T) {
	t.Parallel()

	client, _ := emptyPATClient(t, http.StatusOK, "[]")
	router, err := NewRouter(map[string][]Profile{
		"acme": {{Name: "primary", Type: ProfileFineGrainedPAT, Client: client}},
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = router.DiscoverResult(ctx, "acme/widgets")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DiscoverResult() error = %v, want context.Canceled", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
