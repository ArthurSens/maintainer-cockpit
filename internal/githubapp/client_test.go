package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSetupCheckAndCollectUseReadOnlyInstallationAuthentication(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var requests []string
	var tokenRepositories [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /app":
			assertBearerJWT(t, r)
			_, _ = io.WriteString(w, `{"id":123,"slug":"maintainer-cockpit-test"}`)
		case "GET /app/installations/456":
			assertBearerJWT(t, r)
			_, _ = io.WriteString(w, `{"id":456,"app_id":123,"account":{"login":"acme"},"permissions":{"metadata":"read","pull_requests":"read","checks":"read"}}`)
		case "POST /app/installations/456/access_tokens":
			assertBearerJWT(t, r)
			var body struct {
				Permissions  map[string]string `json:"permissions"`
				Repositories []string          `json:"repositories"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode access-token request: %v", err)
			}
			if len(body.Permissions) != 2 ||
				body.Permissions["pull_requests"] != "read" ||
				body.Permissions["checks"] != "read" {
				t.Errorf("permissions = %#v, want pull_requests:read and checks:read", body.Permissions)
			}
			mu.Lock()
			tokenRepositories = append(tokenRepositories, body.Repositories)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"token":"installation-token","expires_at":"2026-09-08T21:00:00Z","permissions":{"pull_requests":"read"}}`)
		case "GET /repos/acme/widgets/pulls":
			assertInstallationToken(t, r)
			_, _ = io.WriteString(w, `[{"number":7}]`)
		case "GET /repos/acme/widgets/pulls/7":
			assertInstallationToken(t, r)
			_, _ = io.WriteString(w, `{
				"number":7,
				"title":"Improve the widget",
				"body":"Improves parsing.\n\n🤖 Generated with Claude Code",
				"user":{"login":"octocat"},
				"created_at":"2026-09-01T10:00:00Z",
				"updated_at":"2026-09-08T19:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/7",
				"additions":42,
				"deletions":8,
				"changed_files":3,
				"draft":false,
				"mergeable":false,
				"mergeable_state":"dirty",
				"requested_reviewers":[{"login":"alice"}],
				"requested_teams":[{"slug":"maintainers"}],
				"head":{"sha":"abc123"}
			}`)
		case "GET /repos/acme/widgets/pulls/7/reviews":
			assertInstallationToken(t, r)
			_, _ = io.WriteString(w, `[
				{"user":{"login":"reviewer"},"state":"APPROVED","submitted_at":"2026-09-08T18:00:00Z"},
				{"user":{"login":"reviewer"},"state":"COMMENTED","submitted_at":"2026-09-08T19:00:00Z"}
			]`)
		case "GET /repos/acme/widgets/commits/abc123/check-runs":
			assertInstallationToken(t, r)
			_, _ = io.WriteString(w, `{
				"total_count":2,
				"check_runs":[
					{"id":1,"name":"unit","status":"completed","conclusion":"success",
					 "html_url":"https://github.com/acme/widgets/actions/runs/1"},
					{"id":2,"name":"lint","status":"completed","conclusion":"failure",
					 "html_url":"https://github.com/acme/widgets/actions/runs/2"}
				]
			}`)
		case "GET /repos/acme/widgets/pulls/7/files":
			assertInstallationToken(t, r)
			_, _ = io.WriteString(w, `[
				{"filename":"main.go","additions":20,"deletions":4},
				{"filename":"parser.go","additions":12,"deletions":2},
				{"filename":"parser_test.go","additions":10,"deletions":2}
			]`)
		default:
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := New(Options{
		AppID:               123,
		InstallationID:      456,
		PrivateKeyPEM:       testPrivateKey(t),
		BaseURL:             server.URL,
		HTTPClient:          server.Client(),
		Now:                 func() time.Time { return now },
		CollectContribution: true,
		ContributionRepositories: map[string][]string{
			"acme/widgets": {"acme/widgets", "other/widgets"},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	setup, err := client.SetupCheck(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("SetupCheck() error = %v", err)
	}
	if setup.AppSlug != "maintainer-cockpit-test" || setup.AccountLogin != "acme" {
		t.Errorf("SetupCheck() = %+v", setup)
	}

	snapshot, err := client.Collect(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(snapshot.PullRequests) != 1 {
		t.Fatalf("len(PullRequests) = %d, want 1", len(snapshot.PullRequests))
	}
	if len(snapshot.Authors) != 0 {
		t.Errorf("Authors = %+v, want contribution evidence decoupled from core collection", snapshot.Authors)
	}
	pr := snapshot.PullRequests[0]
	if pr.Author != "octocat" || pr.ReviewState != "review_requested" ||
		pr.Additions != 42 || pr.Deletions != 8 {
		t.Errorf("PullRequest = %+v", pr)
	}
	if pr.ChangedFiles != 3 {
		t.Errorf("ChangedFiles = %d, want provider changed-file count 3", pr.ChangedFiles)
	}
	if pr.Readiness.Mergeable == nil || *pr.Readiness.Mergeable ||
		pr.Readiness.MergeableState != "dirty" {
		t.Errorf("Readiness mergeability = %+v, want dirty conflict", pr.Readiness)
	}
	if len(pr.Readiness.RequestedReviewers) != 1 ||
		pr.Readiness.RequestedReviewers[0] != "alice" ||
		len(pr.Readiness.RequestedTeams) != 1 ||
		pr.Readiness.RequestedTeams[0] != "maintainers" {
		t.Errorf("Readiness review requests = %+v", pr.Readiness)
	}
	if pr.Readiness.Checks.Total != 2 || len(pr.Readiness.Checks.Runs) != 2 ||
		pr.Readiness.Checks.Runs[1].Conclusion != "failure" {
		t.Errorf("Readiness checks = %+v", pr.Readiness.Checks)
	}
	if !pr.Readiness.CollectedAt.Equal(now) {
		t.Errorf("Readiness collected at = %s, want %s", pr.Readiness.CollectedAt, now)
	}
	if !strings.Contains(pr.Body, "Generated with Claude Code") {
		t.Errorf("Body = %q, want the collected PR description", pr.Body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(tokenRepositories) != 1 || len(tokenRepositories[0]) != 1 || tokenRepositories[0][0] != "widgets" {
		t.Errorf("token repository scopes = %#v, want collection token scoped to widgets", tokenRepositories)
	}
	for _, request := range requests {
		if strings.Contains(request, "/users/") || strings.Contains(request, "/search/issues") {
			t.Errorf("core collection made contribution evidence request: %s", request)
		}
		if strings.HasPrefix(request, "PATCH ") || strings.HasPrefix(request, "PUT ") || strings.HasPrefix(request, "DELETE ") {
			t.Errorf("write request made: %s", request)
		}
		if strings.HasPrefix(request, "POST ") && !strings.Contains(request, "/access_tokens") {
			t.Errorf("unexpected POST request made: %s", request)
		}
	}
}

func TestCollectCheckRunsPaginates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		if r.URL.Path != "/repos/acme/widgets/commits/abc/check-runs" {
			http.NotFound(w, r)
			return
		}
		if page == "1" {
			runs := make([]map[string]any, 100)
			for index := range runs {
				runs[index] = map[string]any{
					"id": index + 1, "name": fmt.Sprintf("check-%d", index+1),
					"status": "completed", "conclusion": "success",
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "check_runs": runs})
			return
		}
		_, _ = io.WriteString(w, `{
			"total_count":101,
			"check_runs":[{"id":101,"name":"last","status":"in_progress","conclusion":null}]
		}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewPAT(PATOptions{
		Token: "token", BaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.collectCheckRuns(context.Background(), "token", "acme", "widgets", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 101 || len(got.Runs) != 101 || got.Runs[100].Name != "last" {
		t.Errorf("checks = %+v, want 101 runs across two pages", got)
	}
}

func TestCollectAttributesQualifyingActivities(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			_, _ = io.WriteString(w, `{"token":"token","expires_at":"2026-09-10T16:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls" &&
			r.URL.Query().Get("state") == "open":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls" &&
			r.URL.Query().Get("state") == "all":
			_, _ = io.WriteString(w, `[{
				"number":7,"title":"Ship the widget",
				"html_url":"https://github.com/acme/widgets/pull/7",
				"updated_at":"2026-09-10T14:00:00Z"
			},{
				"number":6,"title":"Already collected",
				"html_url":"https://github.com/acme/widgets/pull/6",
				"updated_at":"2026-09-10T12:00:00Z"
			}]`)
		case r.Method == http.MethodGet &&
			r.URL.Path == "/repos/acme/widgets/issues/7/timeline":
			_, _ = io.WriteString(w, `[
				{"id":101,"event":"reviewed","user":{"id":42},"submitted_at":"2026-09-10T12:00:00Z","html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-101"},
				{"id":102,"event":"commented","actor":{"id":42},"created_at":"2026-09-10T13:00:00Z","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-102"},
				{"id":103,"event":"merged","actor":{"id":99},"created_at":"2026-09-10T14:00:00Z","html_url":"https://github.com/acme/widgets/pull/7#event-103"},
				{"id":104,"event":"closed","actor":{"id":99},"created_at":"2026-09-10T14:00:01Z","html_url":"https://github.com/acme/widgets/pull/7#event-104"},
				{"id":102,"event":"commented","actor":{"id":42},"created_at":"2026-09-10T13:00:00Z","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-102"}
			]`)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{
				"nodes":[{
					"id":"PRRT_kwDO1","isResolved":true,"resolvedBy":{"databaseId":42},
					"comments":{"nodes":[{"updatedAt":"2026-09-10T13:30:00Z","url":"https://github.com/acme/widgets/pull/7#discussion_r1"}]}
				}],"pageInfo":{"hasNextPage":false,"endCursor":null}
			}}}}}`)
		default:
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now },
		CollectProgress: true,
		DiscoveryPlans: map[string][]DiscoveryTarget{
			"acme/widgets": {
				{CollectionID: "acme", Mode: "all_open"},
				{CollectionID: "shared", Mode: "all_open"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.CollectWithOptions(context.Background(), "acme/widgets", CollectOptions{
		ForceFull:     true,
		ProgressSince: time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ProgressEvents) != 3 {
		t.Fatalf("progress events = %+v, want three new unique timeline activities", snapshot.ProgressEvents)
	}
	got := make(map[string]int64)
	for _, event := range snapshot.ProgressEvents {
		got[event.ActivityType] = event.ActorID
		if !slices.Equal(event.CollectionIDs, []string{"acme", "shared"}) {
			t.Errorf("event collections = %v, want both overlapping collections", event.CollectionIDs)
		}
	}
	for activityType, actorID := range map[string]int64{
		"comment": 42, "merge": 99, "close": 99,
	} {
		if got[activityType] != actorID {
			t.Errorf("%s actor = %d, want %d", activityType, got[activityType], actorID)
		}
	}
	if len(snapshot.ReviewThreads) != 1 || !snapshot.ReviewThreads[0].Resolved ||
		snapshot.ReviewThreads[0].ResolvedBy != 42 {
		t.Errorf("review thread state = %+v, want resolved by 42", snapshot.ReviewThreads)
	}
}

func TestSetupCheckRequiresMembersReadForConfiguredAuthorization(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app":
			_, _ = io.WriteString(w, `{"id":123,"slug":"cockpit"}`)
		case "/app/installations/456":
			_, _ = io.WriteString(w, `{
				"id":456,"app_id":123,"account":{"login":"acme"},
				"permissions":{"metadata":"read","pull_requests":"read","checks":"read"}
			}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now },
		RequireMembersRead: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SetupCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "members: read") {
		t.Fatalf("SetupCheck() error = %v, want missing members permission", err)
	}
}

func TestCollectReportsIncompleteWithoutReturningPartialSnapshot(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /app/installations/456/access_tokens":
			_, _ = io.WriteString(w, `{"token":"token","expires_at":"2026-09-08T21:00:00Z"}`)
		case "GET /repos/acme/widgets/pulls":
			_, _ = io.WriteString(w, `[{"number":7},{"number":8}]`)
		case "GET /repos/acme/widgets/pulls/7":
			_, _ = io.WriteString(w, `{"number":7,"title":"First","user":{"login":"one"},"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-08T19:00:00Z","html_url":"https://github.com/acme/widgets/pull/7","additions":1,"deletions":1}`)
		case "GET /repos/acme/widgets/pulls/7/reviews":
			_, _ = io.WriteString(w, `[]`)
		case "GET /repos/acme/widgets/pulls/8":
			http.Error(w, `{"message":"upstream failure"}`, http.StatusBadGateway)
		default:
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := New(Options{
		AppID:          123,
		InstallationID: 456,
		PrivateKeyPEM:  testPrivateKey(t),
		BaseURL:        server.URL,
		HTTPClient:     server.Client(),
		Now:            func() time.Time { return time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	snapshot, err := client.Collect(context.Background(), "acme/widgets")
	if err == nil || !IsIncomplete(err) {
		t.Fatalf("Collect() error = %v, want incomplete error", err)
	}
	if len(snapshot.PullRequests) != 0 {
		t.Errorf("Collect() returned %d partial PRs", len(snapshot.PullRequests))
	}
}

func TestCollectContributionEvidenceUsesSeparateAuthenticatedAndAncillaryPaths(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	historyRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			_, _ = io.WriteString(w, `{"token":"token","expires_at":"2026-09-09T13:00:00Z"}`)
		case r.URL.Path == "/repos/acme/widgets/pulls":
			_, _ = io.WriteString(w, `[{"number":7}]`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7":
			_, _ = io.WriteString(w, `{
				"number":7,"title":"History","user":{"id":42,"login":"octocat"},
				"author_association":"CONTRIBUTOR",
				"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-09T11:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/7"
			}`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7/reviews":
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/users/octocat":
			if r.Header.Get("Authorization") != "" {
				t.Error("public user metadata request unexpectedly used installation authorization")
			}
			historyRequests++
			_, _ = io.WriteString(w, `{"id":42,"login":"octocat","created_at":"2026-08-01T10:00:00Z"}`)
		case r.URL.Path == "/search/issues":
			query := r.URL.Query().Get("q")
			switch {
			case strings.Contains(query, "repo:acme/widgets"):
				if r.Header.Get("Authorization") != "Bearer token" {
					t.Errorf("repository history authorization = %q", r.Header.Get("Authorization"))
				}
				_, _ = io.WriteString(w, `{
					"total_count":4,"incomplete_results":false,"items":[
						{"state":"closed","author_association":"CONTRIBUTOR","pull_request":{"merged_at":"2026-08-10T10:00:00Z"}},
						{"state":"closed","author_association":"MEMBER","pull_request":{"merged_at":null}},
						{"state":"open","pull_request":{}},
						{"state":"open","pull_request":{}}
					]
				}`)
			case strings.Contains(query, "created:>="):
				if r.Header.Get("Authorization") != "" {
					t.Error("public cross-organization search unexpectedly used installation authorization")
				}
				_, _ = io.WriteString(w, `{
					"total_count":3,"incomplete_results":false,"items":[
						{"repository_url":"https://api.github.com/repos/one/a","created_at":"2026-09-08T10:00:00Z"},
						{"repository_url":"https://api.github.com/repos/two/b","created_at":"2026-09-07T10:00:00Z"},
						{"repository_url":"https://api.github.com/repos/three/c","created_at":"2026-09-06T10:00:00Z"}
					]
				}`)
			default:
				http.Error(w, "unexpected search", http.StatusBadRequest)
			}
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(), CollectContribution: true,
		ContributionRefreshIntervals: map[string]time.Duration{
			"acme/widgets": 7 * 24 * time.Hour,
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Collect(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(snapshot.Authors) != 0 {
		t.Fatalf("Authors = %#v, want core collection without author evidence", snapshot.Authors)
	}
	repositoryResult, err := client.CollectRepositoryContribution(
		context.Background(), 42, "octocat", "acme/widgets", "CONTRIBUTOR",
	)
	if err != nil {
		t.Fatalf("CollectRepositoryContribution() error = %v", err)
	}
	repository := repositoryResult.History
	if repository.Association != "MEMBER" ||
		repository.Merged != 1 || repository.ClosedUnmerged != 1 ||
		repository.Open != 2 {
		t.Errorf("repository evidence = %+v", repositoryResult)
	}
	ancillary, err := NewAncillaryClient(client).CollectAncillaryAuthorEvidence(
		context.Background(), 42, "octocat",
	)
	if err != nil {
		t.Fatalf("CollectAncillaryAuthorEvidence() error = %v", err)
	}
	if ancillary.AccountCreatedAt.IsZero() || len(ancillary.RecentActivity) != 3 {
		t.Errorf("ancillary evidence = %+v", ancillary)
	}
	if historyRequests != 1 {
		t.Errorf("ancillary profile requests = %d, want 1", historyRequests)
	}
}

func TestAncillarySearchPreservesRequestError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/users/") {
			_, _ = io.WriteString(w, `{"id":42,"login":"octocat","created_at":"2020-01-01T00:00:00Z"}`)
			return
		}
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
	}))
	defer server.Close()
	client, err := NewPAT(PATOptions{
		Token: "unused", BaseURL: server.URL, HTTPClient: server.Client(),
		Now: func() time.Time {
			return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAncillaryClient(client).CollectAncillaryAuthorEvidence(
		context.Background(), 42, "octocat",
	)
	var routeFailure *RouteFailure
	if !errors.As(err, &routeFailure) ||
		routeFailure.Category != FailurePermissionMissing {
		t.Fatalf("CollectAncillaryAuthorEvidence() error = %v, want permission route failure", err)
	}
}

func TestCollectPreservesPerFileChurn(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			_, _ = io.WriteString(w, `{"token":"token","expires_at":"2026-09-08T21:00:00Z"}`)
		case r.URL.Path == "/repos/acme/widgets/pulls":
			_, _ = io.WriteString(w, `[{"number":7}]`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7":
			_, _ = io.WriteString(w, `{
				"number":7,"title":"Size","user":{"login":"octocat"},
				"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-08T19:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/7",
				"additions":104,"deletions":1,"changed_files":2,"head":{"sha":"head"}
			}`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7/reviews":
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/repos/acme/widgets/commits/head/check-runs":
			_, _ = io.WriteString(w, `{"total_count":0,"check_runs":[]}`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7/files":
			_, _ = io.WriteString(w, `[
				{"filename":"main.go","additions":4,"deletions":1},
				{"filename":"generated.go","additions":100,"deletions":0}
			]`)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(),
		Now: func() time.Time { return time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Collect(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	pr := snapshot.PullRequests[0]
	if pr.Additions != 104 || pr.Deletions != 1 || pr.ChangedFiles != 2 ||
		len(pr.Files) != 2 || pr.Files[0].Path != "main.go" ||
		pr.Files[0].Additions != 4 || pr.Files[0].Deletions != 1 {
		t.Errorf("collected churn = %+v", pr)
	}
}

func TestSetupCheckRejectsWritePermissions(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app":
			_, _ = io.WriteString(w, `{"id":123,"slug":"test"}`)
		case "/app/installations/456":
			_, _ = io.WriteString(w, `{"id":456,"app_id":123,"permissions":{"pull_requests":"write"}}`)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SetupCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pull_requests: read") {
		t.Fatalf("SetupCheck() error = %v, want read-only permission error", err)
	}
}

func TestSetupCheckRejectsUnnecessaryContentsPermission(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app":
			_, _ = io.WriteString(w, `{"id":123,"slug":"test"}`)
		case "/app/installations/456":
			_, _ = io.WriteString(w, `{
				"id":456,"app_id":123,
				"permissions":{"metadata":"read","pull_requests":"read","checks":"read","contents":"read"}
			}`)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SetupCheck(context.Background())
	if err == nil || !strings.Contains(err.Error(), `unnecessary permission "contents"`) {
		t.Fatalf("SetupCheck() error = %v, want unnecessary contents permission error", err)
	}
}

func TestCollectPaginatesAllOpenPullRequests(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/456/access_tokens":
			_, _ = io.WriteString(w, `{"token":"token","expires_at":"2026-09-08T21:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls":
			start, end := 1, 100
			if r.URL.Query().Get("page") == "2" {
				start, end = 101, 101
			}
			var page []map[string]int
			for number := start; number <= end; number++ {
				page = append(page, map[string]int{"number": number})
			}
			_ = json.NewEncoder(w).Encode(page)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/pulls/"):
			number, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/pulls/"))
			if err != nil {
				http.Error(w, "bad number", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(w, `{
				"number":%d,"title":"PR %d","user":{"login":"octocat"},
				"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-08T19:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/%d","additions":1,"deletions":1
			}`, number, number, number)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(),
		Now: func() time.Time { return time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Collect(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(snapshot.PullRequests) != 101 {
		t.Errorf("len(PullRequests) = %d, want 101", len(snapshot.PullRequests))
	}
}

func TestCollectUnionsDiscoveryAndFetchesEachPullRequestOnce(t *testing.T) {
	t.Parallel()

	detailCalls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			_, _ = io.WriteString(w, `{"token":"token","expires_at":"2026-09-08T21:00:00Z"}`)
		case r.URL.Path == "/repos/acme/widgets/pulls":
			_, _ = io.WriteString(w, `[{"number":1}]`)
		case r.URL.Path == "/search/issues":
			if query := r.URL.Query().Get("q"); !strings.Contains(query, "repo:acme/widgets is:pr is:open metrics") {
				t.Errorf("search query = %q", query)
			}
			_, _ = io.WriteString(w, `{"total_count":2,"incomplete_results":false,"items":[{"number":1},{"number":2}]}`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = io.WriteString(w, `[]`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/pulls/"):
			number := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/pulls/")
			detailCalls[number]++
			_, _ = fmt.Fprintf(w, `{
				"number":%s,"title":"PR %s","user":{"login":"octocat"},
				"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-08T19:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/%s","additions":1,"deletions":1
			}`, number, number, number)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(),
		Now: func() time.Time { return time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC) },
		DiscoveryPlans: map[string][]DiscoveryTarget{
			"acme/widgets": {
				{CollectionID: "all", Mode: "all_open"},
				{CollectionID: "metrics", Mode: "search", Query: "metrics"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Collect(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.PullRequests) != 2 || detailCalls["1"] != 1 || detailCalls["2"] != 1 {
		t.Fatalf("snapshot PRs = %d, detail calls = %#v", len(snapshot.PullRequests), detailCalls)
	}
	if got := snapshot.PullRequests[0].CollectionIDs; !slices.Equal(got, []string{"all", "metrics"}) {
		t.Errorf("PR 1 collections = %#v", got)
	}
	if got := snapshot.PullRequests[1].CollectionIDs; !slices.Equal(got, []string{"metrics"}) {
		t.Errorf("PR 2 collections = %#v", got)
	}
}

func TestCollectPersistsOneHopContextAndNeverFetchesExternalURLs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			_, _ = io.WriteString(w, `{"token":"token","expires_at":"2026-09-08T21:00:00Z"}`)
		case r.URL.Path == "/repos/acme/widgets/pulls":
			_, _ = io.WriteString(w, `[{"number":7}]`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7":
			_, _ = io.WriteString(w, `{
				"number":7,"title":"Context","body":"Fixes #10. See https://example.com/design.",
				"user":{"login":"octocat"},"created_at":"2026-09-01T10:00:00Z",
				"updated_at":"2026-09-08T19:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/7","additions":1,"deletions":1
			}`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7/reviews":
			_, _ = io.WriteString(w, `[{
				"id":91,"user":{"login":"reviewer"},"state":"COMMENTED",
				"body":"Compare acme/other#2","submitted_at":"2026-09-08T18:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-91"
			}]`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7/comments":
			_, _ = io.WriteString(w, `[{
				"id":92,"body":"Thread","path":"main.go","created_at":"2026-09-08T18:10:00Z",
				"updated_at":"2026-09-08T18:20:00Z",
				"html_url":"https://github.com/acme/widgets/pull/7#discussion_r92"
			}]`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7/commits":
			_, _ = io.WriteString(w, `[{
				"sha":"abc123","html_url":"https://github.com/acme/widgets/commit/abc123",
				"commit":{"message":"Implement context","author":{"date":"2026-09-08T17:00:00Z"}}
			}]`)
		case r.URL.Path == "/repos/acme/widgets/commits/abc123/pulls":
			_, _ = io.WriteString(w, `[{
				"number":8,"title":"Related PR","state":"open",
				"updated_at":"2026-09-08T16:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/8",
				"base":{"repo":{"full_name":"acme/widgets"}}
			}]`)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	httpClient := server.Client()
	transport := httpClient.Transport
	allowedHost := strings.TrimPrefix(server.URL, "http://")
	httpClient.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != allowedHost {
			return nil, fmt.Errorf("attempted to fetch untrusted host %q", request.URL.Host)
		}
		return transport.RoundTrip(request)
	})
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: httpClient, CollectContext: true,
		Now: func() time.Time { return time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Collect(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	context := snapshot.PullRequests[0].Context
	kinds := make(map[string]int)
	for _, relationship := range context.Relationships {
		kinds[relationship.Kind]++
		if relationship.SourceID == "" || relationship.Timestamp.IsZero() {
			t.Errorf("relationship missing source identity or timestamp: %#v", relationship)
		}
	}
	for _, kind := range []string{
		"closing_issue", "cross_reference", "commit", "review", "review_thread",
		"associated_pull_request",
	} {
		if kinds[kind] == 0 {
			t.Errorf("missing %s relationship in %#v", kind, context.Relationships)
		}
	}
	if len(context.ExternalURLs) != 1 || context.ExternalURLs[0].URL != "https://example.com/design" {
		t.Errorf("external URLs = %#v", context.ExternalURLs)
	}
}

func TestSearchDiscoveryReportsProviderTruncation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"total_count":1200,"incomplete_results":true,"items":[{"number":7}]
		}`)
	}))
	defer server.Close()
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	numbers, limit, err := client.discoverSearch(
		context.Background(), "token", "acme/widgets", `label:"help wanted"`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(numbers, []int{7}) {
		t.Errorf("numbers = %#v", numbers)
	}
	if limit == nil || limit.Scope != "discovery_search" ||
		limit.Collected != 1 || limit.Total != 1200 {
		t.Errorf("limit = %#v", limit)
	}
}

func TestCollectReportsCacheHitsAndInvalidatesRelatedChanges(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	reviewBody := "first review"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			_, _ = io.WriteString(w, `{"token":"token","expires_at":"2026-09-10T13:00:00Z"}`)
		case r.URL.Path == "/repos/acme/widgets/pulls":
			_, _ = io.WriteString(w, `[{"number":7}]`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7":
			_, _ = io.WriteString(w, `{
				"number":7,"title":"Reuse","body":"description","user":{"id":42,"login":"octocat"},
				"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-10T11:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/7","head":{"sha":"abc"}
			}`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7/reviews":
			_, _ = fmt.Fprintf(w, `[{
				"id":91,"user":{"login":"reviewer"},"state":"COMMENTED",
				"body":%q,"submitted_at":"2026-09-10T10:00:00Z"
			}]`, reviewBody)
		case r.URL.Path == "/repos/acme/widgets/commits/abc/check-runs":
			_, _ = io.WriteString(w, `{"total_count":0,"check_runs":[]}`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7/comments":
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/repos/acme/widgets/pulls/7/commits":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(), CollectContext: true,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.CollectWithOptions(
		context.Background(), "acme/widgets", CollectOptions{ForceFull: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.CacheBypasses != 1 || first.CacheHits != 0 || first.CacheMisses != 0 {
		t.Fatalf("first collection = %+v, want one cache bypass", first)
	}
	cached := map[int]CachedPullRequest{7: {
		PullRequest: first.PullRequests[0], Fingerprint: first.Fingerprints[7],
	}}
	second, err := client.CollectWithOptions(
		context.Background(), "acme/widgets", CollectOptions{Cached: cached},
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.CacheHits != 1 || second.CacheMisses != 0 || second.CacheBypasses != 0 {
		t.Fatalf("unchanged collection = %+v, want one cache hit", second)
	}

	reviewBody = "edited related review"
	third, err := client.CollectWithOptions(
		context.Background(), "acme/widgets", CollectOptions{Cached: cached},
	)
	if err != nil {
		t.Fatal(err)
	}
	if third.CacheHits != 0 || third.CacheMisses != 1 || third.CacheBypasses != 0 ||
		third.Fingerprints[7] == first.Fingerprints[7] {
		t.Fatalf("related-input change did not produce a cache miss: %+v", third)
	}
}

func TestDiscoverReturnsMembershipAndLimitsWithoutHydrating(t *testing.T) {
	t.Parallel()

	var detailCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls":
			_, _ = io.WriteString(w, `[{"number":7}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			_, _ = io.WriteString(w, `{
				"total_count":1200,"incomplete_results":true,
				"items":[{"number":7},{"number":8}]
			}`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/pulls/"):
			detailCalls++
			http.Error(w, "unexpected hydration", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewPAT(PATOptions{
		Token: "token", BaseURL: server.URL, HTTPClient: server.Client(),
		DiscoveryPlans: map[string][]DiscoveryTarget{"acme/widgets": {
			{CollectionID: "all", Mode: "all_open"},
			{CollectionID: "search", Mode: "search", Query: "metrics"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := client.Discover(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if result.Repository != "acme/widgets" || detailCalls != 0 ||
		len(result.PullRequests) != 2 || len(result.Limits) != 1 {
		t.Fatalf("discovery = %+v, detail calls = %d", result, detailCalls)
	}
	if got := result.PullRequests[0]; got.Number != 7 ||
		!slices.Equal(got.CollectionIDs, []string{"all", "search"}) ||
		!slices.Equal(got.ContextLimits, result.Limits) {
		t.Errorf("PR 7 discovery = %+v", got)
	}
	if got := result.PullRequests[1]; got.Number != 8 ||
		!slices.Equal(got.CollectionIDs, []string{"search"}) ||
		!slices.Equal(got.ContextLimits, result.Limits) {
		t.Errorf("PR 8 discovery = %+v", got)
	}
}

func TestHydratePullRequestsUsesSuppliedBatchAndPreservesCacheSemantics(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	var detailCalls, fileCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/7":
			detailCalls++
			_, _ = io.WriteString(w, `{
				"number":7,"title":"Batch","body":"same","user":{"id":42,"login":"octocat"},
				"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-12T11:00:00Z",
				"html_url":"https://github.com/acme/widgets/pull/7",
				"changed_files":1,"mergeable":true,"mergeable_state":"clean",
				"head":{"sha":"head"}
			}`)
		case "/repos/acme/widgets/pulls/7/reviews":
			_, _ = io.WriteString(w, `[]`)
		case "/repos/acme/widgets/commits/head/check-runs":
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{
				"id":%d,"name":"unit","status":"completed","conclusion":"success"
			}]}`, detailCalls)
		case "/repos/acme/widgets/pulls/7/files":
			fileCalls++
			_, _ = io.WriteString(w, `[{"filename":"main.go","additions":1}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewPAT(PATOptions{
		Token: "token", BaseURL: server.URL, HTTPClient: server.Client(),
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	batch := []DiscoveredPullRequest{{
		Number: 7, CollectionIDs: []string{"one"},
		ContextLimits: []ContextLimit{{Scope: "discovery_search", Reason: "provider_limit"}},
	}}
	first, err := client.HydratePullRequests(
		context.Background(), "acme/widgets", batch, HydrationOptions{ForceFull: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.CacheBypasses != 1 || fileCalls != 1 || len(first.PullRequests) != 1 {
		t.Fatalf("first hydration = %+v, file calls = %d", first, fileCalls)
	}
	cached := map[int]CachedPullRequest{7: {
		PullRequest: first.PullRequests[0], Fingerprint: first.Fingerprints[7],
	}}
	second, err := client.HydratePullRequests(
		context.Background(), "acme/widgets", batch, HydrationOptions{Cached: cached},
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.Repository != "acme/widgets" || second.CacheHits != 1 ||
		second.CacheMisses != 0 || second.CacheBypasses != 0 || fileCalls != 1 {
		t.Fatalf("cached hydration = %+v, file calls = %d", second, fileCalls)
	}
	pr := second.PullRequests[0]
	if pr.Readiness.Checks.Runs[0].ID != 2 ||
		!slices.Equal(pr.CollectionIDs, []string{"one"}) ||
		!slices.Equal(pr.Context.Limits, batch[0].ContextLimits) {
		t.Errorf("cached PR = %+v", pr)
	}
}

func TestCollectProgressUsesProgressSinceIndependently(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 15, 0, 0, 0, time.UTC)
	var candidateQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/acme/widgets/pulls" && r.URL.Query().Get("state") == "all":
			candidateQuery = r.URL.Query()
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewPAT(PATOptions{
		Token: "token", BaseURL: server.URL, HTTPClient: server.Client(),
		Now: func() time.Time { return now }, CollectProgress: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Hour)
	result, err := client.CollectProgress(
		context.Background(), "acme/widgets", ProgressOptions{ProgressSince: since},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Repository != "acme/widgets" || !result.CollectedAt.Equal(now) ||
		len(result.ProgressEvents) != 0 || candidateQuery.Get("state") != "all" {
		t.Fatalf("progress = %+v, query = %v", result, candidateQuery)
	}
}

func TestRequestJSONHonorsPrimaryRateLimitReset(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	requests := 0
	var waits []time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(30*time.Second).Unix(), 10))
			http.Error(w, `{"message":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now },
		RateLimitBudget: time.Minute, RateLimitMaxRetries: 2,
		Sleep: func(_ context.Context, duration time.Duration) error {
			waits = append(waits, duration)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]bool
	if err := client.requestJSON(context.Background(), http.MethodGet, "/resource", "token", nil, &response); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(waits) != 1 || waits[0] != 30*time.Second || !response["ok"] {
		t.Fatalf("requests=%d waits=%v response=%v", requests, waits, response)
	}
}

func TestRequestJSONRejectsRateLimitWaitBeyondBudget(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(2*time.Minute).Unix(), 10))
		http.Error(w, `{"message":"rate limit exceeded"}`, http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now },
		RateLimitBudget: time.Minute, RateLimitMaxRetries: 2,
		Sleep: func(context.Context, time.Duration) error {
			t.Fatal("wait beyond budget must not sleep")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.requestJSON(context.Background(), http.MethodGet, "/resource", "token", nil, nil)
	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) || rateLimitErr.Reason != "primary" ||
		!rateLimitErr.BudgetExceeded {
		t.Fatalf("error = %v, want primary budget-exceeded RateLimitError", err)
	}
}

func TestRequestJSONStopsRateLimitWaitOnCancellation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, `{"message":"secondary rate limit"}`, http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(),
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.requestJSON(ctx, http.MethodGet, "/resource", "token", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestRequestJSONStopsAfterBoundedRateLimitRetries(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Retry-After", "1")
		http.Error(w, `{"message":"secondary rate limit"}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(),
		RateLimitBudget: time.Minute, RateLimitMaxRetries: 2,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.requestJSON(context.Background(), http.MethodGet, "/resource", "token", nil, nil)
	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) || rateLimitErr.Reason != "secondary" ||
		rateLimitErr.Retries != 2 || rateLimitErr.BudgetExceeded || requests != 3 {
		t.Fatalf("error=%v requests=%d, want terminal failure after three attempts", err, requests)
	}
}

func TestRequestJSONRetainsBoundedSafeFailureMetadata(t *testing.T) {
	t.Parallel()

	const secret = "secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GitHub-Request-Id", "REQ_123")
		w.Header().Set("Retry-After", "17")
		w.Header().Set("X-RateLimit-Reset", "1789200000")
		http.Error(w, `{"message":"raw contributor-controlled body `+secret+`"}`, http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.requestJSON(
		context.Background(), http.MethodGet,
		"/repos/secret-owner/secret-repo/pulls/42/comments?access_token="+secret,
		secret, nil, nil,
	)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v, want APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || apiErr.Method != http.MethodGet ||
		apiErr.Endpoint != "/repos/{owner}/{repo}/pulls/{id}/comments" ||
		apiErr.RequestID != "REQ_123" || apiErr.RetryAfter != 17*time.Second ||
		apiErr.RateLimitResetUnix != 1789200000 {
		t.Errorf("APIError = %+v", apiErr)
	}
	encoded := fmt.Sprintf("%+v %s", apiErr, err)
	for _, forbidden := range []string{
		secret, "secret-owner", "secret-repo", "42", "access_token",
		"raw contributor-controlled body",
	} {
		if strings.Contains(encoded, forbidden) {
			t.Errorf("bounded error %q contains forbidden value %q", encoded, forbidden)
		}
	}
}

func TestCollectPullRequestDiffAppliesDeterministicUTF8SafeLimits(t *testing.T) {
	t.Parallel()

	var authorization string
	var headChecks int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/acme/widgets/pulls/7" {
			headChecks++
			_, _ = io.WriteString(w, `{"head":{"sha":"head"}}`)
			return
		}
		files := make([]map[string]any, 0, 101)
		files = append(files,
			map[string]any{"filename": "z.go", "patch": strings.Repeat("é", (8<<10)/2+10)},
			map[string]any{"filename": "a.go", "patch": "@@ -1 +1 @@\n-old\n+new"},
		)
		for index := range 99 {
			files = append(files, map[string]any{
				"filename": fmt.Sprintf("mid-%03d.go", index), "patch": "small",
			})
		}
		_ = json.NewEncoder(w).Encode(files)
	}))
	t.Cleanup(server.Close)
	client, err := NewPAT(PATOptions{
		Token: "secret", BaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.CollectPullRequestDiff(
		context.Background(), "acme/widgets", 7, "head", 101,
	)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer secret" {
		t.Fatalf("Authorization = %q", authorization)
	}
	if headChecks != 2 {
		t.Fatalf("head checks = %d, want before and after collection", headChecks)
	}
	if result.Completeness != "partial" || len(result.Sources) != 100 ||
		result.OriginalFiles != 101 || result.OmittedFiles != 1 || !result.Truncated {
		t.Fatalf("result = %+v", result)
	}
	if result.Sources[0].Path != "z.go" || result.Sources[1].Path != "a.go" {
		t.Fatalf("sources start = %q, %q, want GitHub returned order",
			result.Sources[0].Path, result.Sources[1].Path)
	}
	for _, source := range result.Sources {
		if len(source.Patch) > 8<<10 || !utf8.ValidString(source.Patch) {
			t.Fatalf("invalid bounded patch for %q: %d bytes", source.Path, len(source.Patch))
		}
	}
	if result.SentBytes > 128<<10 {
		t.Fatalf("sent bytes = %d", result.SentBytes)
	}
}

func TestCollectPullRequestDiffRejectsHeadChange(t *testing.T) {
	t.Parallel()

	var headChecks int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/7":
			headChecks++
			head := "expected"
			if headChecks == 2 {
				head = "new-head"
			}
			_, _ = fmt.Fprintf(w, `{"head":{"sha":%q}}`, head)
		case "/repos/acme/widgets/pulls/7/files":
			_, _ = io.WriteString(w, `[{"filename":"main.go","patch":"patch"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewPAT(PATOptions{
		Token: "secret", BaseURL: server.URL, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CollectPullRequestDiff(
		context.Background(), "acme/widgets", 7, "expected", 1,
	)
	if !IsPullRequestHeadChanged(err) {
		t.Fatalf("CollectPullRequestDiff() error = %T %v, want head changed", err, err)
	}
}

func TestRequestJSONClassifiesHTTPClientTimeoutAsTransportFailure(t *testing.T) {
	t.Parallel()

	client, err := New(Options{
		AppID: 123, InstallationID: 456, PrivateKeyPEM: testPrivateKey(t),
		BaseURL: "https://api.github.test",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, &url.Error{
				Op: "Get", URL: "https://api.github.test/secret?token=secret",
				Err: context.DeadlineExceeded,
			}
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = client.requestJSON(ctx, http.MethodGet, "/users/secret-login?token=secret", "", nil, nil)
	var transportErr *TransportError
	if !errors.As(err, &transportErr) {
		t.Fatalf("error = %T %v, want TransportError", err, err)
	}
	if transportErr.Method != http.MethodGet || transportErr.Endpoint != "/users/{login}" ||
		!transportErr.TimedOut || ctx.Err() != nil {
		t.Errorf("TransportError = %+v, context error = %v", transportErr, ctx.Err())
	}
	if strings.Contains(fmt.Sprintf("%+v", transportErr), "secret") {
		t.Errorf("TransportError leaked request data: %+v", transportErr)
	}
}

func TestSanitizeEndpointKeepsOnlyBoundedTemplates(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"/search/issues?q=secret+contributor+text": "/search/issues",
		"/graphql?token=secret":                    "/graphql",
		"/unknown/secret/path?token=secret":        "/{endpoint}",
	}
	for requestPath, want := range tests {
		if got := sanitizeEndpoint(requestPath); got != want {
			t.Errorf("sanitizeEndpoint(%q) = %q, want %q", requestPath, got, want)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func testPrivateKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func assertBearerJWT(t *testing.T, r *http.Request) {
	t.Helper()
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") || strings.Count(authorization, ".") != 2 {
		t.Errorf("Authorization = %q, want Bearer JWT", authorization)
	}
}

func assertInstallationToken(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer installation-token" {
		t.Errorf("Authorization = %q, want installation token", got)
	}
}
