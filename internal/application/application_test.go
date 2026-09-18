package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	listquery "github.com/ArthurSens/maintainer-cockpit/internal/query"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestApplicationServesConfiguredCollectionAndPersistedPR(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("prometheus", `
collections:
  - id: exporters
    name: Exporter maintenance
    description: Pull requests across exporter repositories.
    repositories: [prometheus/node_exporter]
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	if err := app.Store().UpsertPullRequest(context.Background(), "exporters", storage.PullRequest{
		Repository: "prometheus/node_exporter",
		Number:     123,
		Title:      "Make collector behavior explicit",
		Additions:  42,
		Deletions:  7,
		UpdatedAt:  time.Date(2026, time.September, 8, 15, 0, 0, 0, time.UTC),
		URL:        "https://github.com/prometheus/node_exporter/pull/123",
	}); err != nil {
		t.Fatalf("UpsertPullRequest() error = %v", err)
	}

	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	collectionsResponse, err := http.Get(server.URL + "/api/collections")
	if err != nil {
		t.Fatalf("GET collections error = %v", err)
	}
	defer collectionsResponse.Body.Close()
	if collectionsResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET collections status = %d, want 200", collectionsResponse.StatusCode)
	}
	var collections struct {
		Collections []struct {
			ID      string `json:"id"`
			OpenPRs int    `json:"openPRs"`
		} `json:"collections"`
	}
	if err := json.NewDecoder(collectionsResponse.Body).Decode(&collections); err != nil {
		t.Fatalf("decode collections error = %v", err)
	}
	if len(collections.Collections) != 1 || collections.Collections[0].ID != "exporters" || collections.Collections[0].OpenPRs != 1 {
		t.Errorf("collections = %+v, want exporters with one PR", collections.Collections)
	}

	prsResponse, err := http.Get(server.URL + "/api/collections/exporters/pull-requests")
	if err != nil {
		t.Fatalf("GET PRs error = %v", err)
	}
	defer prsResponse.Body.Close()
	if prsResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET PRs status = %d, want 200", prsResponse.StatusCode)
	}
	var prs struct {
		PullRequests []storage.PullRequest `json:"pullRequests"`
	}
	if err := json.NewDecoder(prsResponse.Body).Decode(&prs); err != nil {
		t.Fatalf("decode PRs error = %v", err)
	}
	if len(prs.PullRequests) != 1 || prs.PullRequests[0].Number != 123 {
		t.Errorf("pullRequests = %+v, want PR 123", prs.PullRequests)
	}

	pageResponse, err := http.Get(server.URL + "/collections/exporters")
	if err != nil {
		t.Fatalf("GET collection page error = %v", err)
	}
	defer pageResponse.Body.Close()
	if pageResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET collection page status = %d, want 200", pageResponse.StatusCode)
	}
	page, err := io.ReadAll(pageResponse.Body)
	if err != nil {
		t.Fatalf("read collection page error = %v", err)
	}
	if !strings.Contains(string(page), "Maintainer Cockpit") {
		t.Errorf("collection page does not contain application title")
	}
}

func TestPullRequestLoadFailureLoggingIsBounded(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	logPullRequestLoadFailure(
		context.Background(),
		logger,
		"prometheus-exporters",
		listquery.Options{View: "mine", Limit: 50, Offset: 100},
		true,
		fmt.Errorf("decode recent author activity: %w", errors.New("token=secret-value")),
	)
	logged := output.String()
	for _, want := range []string{
		"event=pull_request_list_failed",
		"collection=prometheus-exporters",
		"view=mine",
		"limit=50",
		"offset=100",
		"authenticated=true",
		"category=storage",
		"detail=decode_author_activity",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log = %q, want %q", logged, want)
		}
	}
	for _, forbidden := range []string{"secret-value", "token=", "recent author activity"} {
		if strings.Contains(logged, forbidden) {
			t.Errorf("log leaked %q: %s", forbidden, logged)
		}
	}
}

func TestCollectionAPIExposesIncompleteRefreshAndLastCompleteSnapshot(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("prometheus", `
collections:
  - id: exporters
    name: Exporters
    repositories: [prometheus/node_exporter]
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	completedAt := time.Now().UTC().Add(-time.Hour)
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(),
		"prometheus/node_exporter",
		nil,
		completedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := app.Store().RecordRefreshResult(
		context.Background(),
		"prometheus/node_exporter",
		storage.RefreshIncomplete,
		time.Now().UTC(),
		"pull request detail failed",
	); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/collections", http.NoBody)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	for _, want := range []string{
		`"refreshState":"incomplete"`,
		`"lastSuccessfulRefresh":`,
		`"refreshError":"pull request detail failed"`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Errorf("response = %s, want %s", response.Body.String(), want)
		}
	}
}

func TestPullRequestsAPIFiltersSortsAndCursorPaginates(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("acme", `
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	now := time.Now().UTC()
	for number, title := range []string{"Alpha metrics", "Beta metrics", "Unrelated"} {
		if err := app.Store().UpsertPullRequest(context.Background(), "acme", storage.PullRequest{
			Repository: "acme/widgets", Number: number + 1, Title: title, Author: "octocat",
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(time.Duration(number) * time.Minute),
			URL: "https://github.com/acme/widgets/pull/1", ReviewState: "approved",
		}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	requestURL := server.URL + "/api/collections/acme/pull-requests?q=metrics&sort=author,title&order=asc,asc&limit=1"
	response, err := http.Get(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var first struct {
		Counts struct {
			Total   int `json:"total"`
			Matched int `json:"matched"`
		} `json:"counts"`
		Page struct {
			NextCursor string `json:"nextCursor"`
		} `json:"page"`
		PullRequests []storage.PullRequest `json:"pullRequests"`
	}
	if err := json.NewDecoder(response.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first.Counts.Total != 3 || first.Counts.Matched != 2 ||
		len(first.PullRequests) != 1 || first.PullRequests[0].Title != "Alpha metrics" ||
		first.Page.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	secondURL, err := url.Parse(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	values := secondURL.Query()
	values.Set("cursor", first.Page.NextCursor)
	secondURL.RawQuery = values.Encode()
	response, err = http.Get(secondURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var second struct {
		PullRequests []storage.PullRequest `json:"pullRequests"`
	}
	if err := json.NewDecoder(response.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if len(second.PullRequests) != 1 || second.PullRequests[0].Title != "Beta metrics" {
		t.Errorf("second page = %+v", second.PullRequests)
	}
}

func TestPullRequestDetailAPIProvidesBoundedContextAndUntrustedURLs(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("acme", `
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	now := time.Now().UTC()
	mergeable := true
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(),
		"acme/widgets",
		[]storage.PullRequest{{
			Repository: "acme/widgets", Number: 7, Title: "Context", Author: "octocat",
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
			URL: "https://github.com/acme/widgets/pull/7", ReviewState: "approved",
			Readiness: storage.PullRequestReadiness{
				RequestedReviewers: []string{"alice"},
				RequestedTeams:     []string{"maintainers"},
				Mergeable:          &mergeable,
				MergeableState:     "clean",
				Checks: storage.CheckSummary{Total: 1, Runs: []storage.CheckRun{{
					ID: 1, Name: "unit", Status: "completed", Conclusion: "success",
					URL: "https://github.com/acme/widgets/actions/runs/1",
				}}},
				CollectedAt: now,
			},
			Additions: 101, Deletions: 2,
			Files: []storage.PullRequestFile{
				{Path: "main.go", Additions: 1},
				{Path: "generated.go", Additions: 100, Deletions: 2},
			},
			Context: &storage.PullRequestContext{
				Completeness: "partial", CollectedAt: now,
				Limits: []storage.ContextLimit{{Scope: "review_threads", Reason: "page_limit", Collected: 100}},
				Relationships: []storage.Relationship{{
					Kind: "closing_issue", SourceID: "I_42", SourceRepository: "acme/widgets",
					SourceNumber: 42, Title: "Tracking issue", State: "OPEN",
					URL: "https://github.com/acme/widgets/issues/42", Timestamp: now,
				}},
				ExternalURLs: []storage.ExternalURL{{
					URL: "https://example.com/design", SourceKind: "pull_request",
					SourceID: "PR_7", DiscoveredAt: now,
				}},
			},
		}},
		now,
	); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/collections/acme/pull-requests/acme/widgets/7", http.NoBody)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, want := range []string{
		`"completeness":"partial"`, `"kind":"closing_issue"`,
		`"sourceID":"I_42"`, `"url":"https://example.com/design"`,
		`"pullRequest":{"repository":"acme/widgets"`,
		`"additions":101`, `"deletions":2`, `"changedFiles":2`,
		`"path":"generated.go"`,
		`"requestedReviewers":["alice"]`, `"mergeableState":"clean"`,
		`"name":"unit"`, `"conclusion":"success"`,
		`"analysisStatus":"pending"`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Errorf("detail body = %s, want %s", response.Body.String(), want)
		}
	}
	for _, removed := range []string{
		`"adjustedAdditions"`, `"adjustedDeletions"`, `"sizeClassification"`,
		`"generated"`, `"vendored"`, `"cognitiveLoad"`,
	} {
		if strings.Contains(response.Body.String(), removed) {
			t.Errorf("detail body = %s, contains removed field %s", response.Body.String(), removed)
		}
	}
}

func TestPullRequestDetailAPIUsesEmptyArraysForAbsentContextData(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("acme", `
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	now := time.Now().UTC()
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(),
		"acme/widgets",
		[]storage.PullRequest{{
			Repository: "acme/widgets", Number: 8, Title: "No context items",
			UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/8",
			Context: &storage.PullRequestContext{
				Completeness: "complete",
				CollectedAt:  now,
			},
		}},
		now,
	); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/collections/acme/pull-requests/acme/widgets/8", http.NoBody)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, want := range []string{
		`"limits":[]`, `"relationships":[]`, `"externalURLs":[]`, `"files":[]`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Errorf("detail body = %s, want %s", response.Body.String(), want)
		}
	}
}

func TestQualityLevelsAreVisibleFilterableAndSortableThroughTheAPI(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("acme", `
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	now := time.Now().UTC()
	broadFiles := make([]storage.PullRequestFile, 0, 12)
	for index := range 12 {
		broadFiles = append(broadFiles, storage.PullRequestFile{
			Path: fmt.Sprintf("pkg/feature/file%02d.go", index), Additions: 40,
		})
	}
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(),
		"acme/widgets",
		[]storage.PullRequest{
			{
				Repository: "acme/widgets", Number: 1, Title: "Small fix", Author: "octocat",
				Additions: 4, Deletions: 2, ChangedFiles: 1,
				Body:      "Fix parsing.\n\n🤖 Generated with Claude Code",
				Files:     []storage.PullRequestFile{{Path: "main.go", Additions: 4, Deletions: 2}},
				UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/1",
			},
			{
				Repository: "acme/widgets", Number: 2, Title: "Broad additions", Author: "octocat",
				Additions: 480, Deletions: 0, ChangedFiles: 12,
				Files:     broadFiles,
				UpdatedAt: now.Add(time.Minute), URL: "https://github.com/acme/widgets/pull/2",
			},
		},
		now,
	); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/api/collections/acme/pull-requests?sort=quality&order=desc")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var sorted struct {
		PullRequests []storage.PullRequest `json:"pullRequests"`
	}
	if err := json.NewDecoder(response.Body).Decode(&sorted); err != nil {
		t.Fatal(err)
	}
	if len(sorted.PullRequests) != 2 ||
		sorted.PullRequests[0].Number != 2 ||
		sorted.PullRequests[0].Quality.Level != "review_suggested" ||
		sorted.PullRequests[1].Quality.Level != "no_concerns" {
		t.Fatalf("quality sort = %+v", sorted.PullRequests)
	}

	response, err = http.Get(server.URL + "/api/collections/acme/pull-requests?quality=review_suggested")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var filtered struct {
		Counts struct {
			Matched int `json:"matched"`
		} `json:"counts"`
		PullRequests []storage.PullRequest `json:"pullRequests"`
	}
	if err := json.NewDecoder(response.Body).Decode(&filtered); err != nil {
		t.Fatal(err)
	}
	if filtered.Counts.Matched != 1 || len(filtered.PullRequests) != 1 ||
		filtered.PullRequests[0].Number != 2 {
		t.Errorf("quality filter = %+v", filtered)
	}

	response, err = http.Get(server.URL + "/api/collections/acme/pull-requests?quality=bogus")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid quality filter status = %d, want 400", response.StatusCode)
	}
}

func TestPullRequestDetailExplainsQualityFindingsAndLocalPolicy(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("acme", `
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
    quality:
      broad_additions_only:
        min_files: 5
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	now := time.Now().UTC()
	files := make([]storage.PullRequestFile, 0, 6)
	for index := range 6 {
		files = append(files, storage.PullRequestFile{
			Path: fmt.Sprintf("pkg/feature/file%02d.go", index), Additions: 80,
		})
	}
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(),
		"acme/widgets",
		[]storage.PullRequest{{
			Repository: "acme/widgets", Number: 9, Title: "Broad additions", Author: "octocat",
			Additions: 480, Deletions: 0, ChangedFiles: 6,
			Body:      "Rewrites internal/auth/session.go entirely.\n\n🤖 Generated with Claude Code",
			Files:     files,
			UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/9",
		}},
		now,
	); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/collections/acme/pull-requests/acme/widgets/9", http.NoBody)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, want := range []string{
		// Level derives from validated findings; two medium findings fired.
		`"level":"review_suggested"`,
		`"rule":"broad_additions_only"`, `"rule":"description_diff_mismatch"`,
		`"severity":"medium"`, `"provenance":"rule_derived"`,
		`"completeness":"complete"`, `"evidence":[`,
		`"url":"https://github.com/acme/widgets/pull/9/files"`,
		// Explicit AI disclosure is factual metadata only.
		`"aiDisclosures":[{"statement":"🤖 Generated with Claude Code"`,
		// Administrators see which thresholds are local policy.
		`"minFiles":{"value":5,"source":"local"}`,
		`"minAdditions":{"value":400,"source":"default"}`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Errorf("detail body = %s, want %s", response.Body.String(), want)
		}
	}
}

func TestPublicAuthorContextShowsFactualCollectionHistoryAndRequestsNoIndex(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("acme", `
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(),
		"acme/widgets",
		[]storage.PullRequest{{
			Repository: "acme/widgets", Number: 7, Title: "Contributor context",
			Author: "octocat", AuthorID: 42, UpdatedAt: now,
			URL: "https://github.com/acme/widgets/pull/7",
			AuthorHistory: &storage.AuthorHistory{
				ID: 42, Login: "octocat", Complete: true,
				AccountCreatedAt: now.Add(-30 * 24 * time.Hour),
				CollectedAt:      now,
				Repositories: map[string]storage.RepositoryContribution{
					"acme/widgets": {
						Association: "CONTRIBUTOR", Merged: 3, ClosedUnmerged: 1, Open: 2,
					},
				},
			},
		}},
		now,
	); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/collections/acme/authors/octocat", http.NoBody)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("author API status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, want := range []string{
		`"level":"established"`, `"association":"CONTRIBUTOR"`,
		`"merged":3`, `"closedUnmerged":1`, `"open":2`,
		`"establishedMergedPRs":{"value":3,"source":"default"}`,
		`"qualityIndependent":true`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Errorf("author API body = %s, want %s", response.Body.String(), want)
		}
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "/collections/acme/authors/octocat", http.NoBody)
	pageResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK {
		t.Fatalf("author page status = %d", pageResponse.Code)
	}
	if got := pageResponse.Header().Get("X-Robots-Tag"); got != "noindex, nofollow" {
		t.Errorf("X-Robots-Tag = %q, want noindex, nofollow", got)
	}
}

func TestPublicAuthorContextSuppressesJudgmentsForIncompleteHistory(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("acme", `
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	now := time.Now().UTC()
	if err := app.Store().ReplaceRepositorySnapshot(
		context.Background(),
		"acme/widgets",
		[]storage.PullRequest{{
			Repository: "acme/widgets", Number: 7, Title: "Incomplete",
			Author: "octocat", AuthorID: 42, UpdatedAt: now,
			URL: "https://github.com/acme/widgets/pull/7",
			AuthorHistory: &storage.AuthorHistory{
				ID: 42, Login: "octocat", Complete: false, CollectedAt: now,
			},
		}},
		now,
	); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/collections/acme/authors/octocat", http.NoBody)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"level":`) ||
		strings.Contains(response.Body.String(), `"detected":`) ||
		!strings.Contains(response.Body.String(), `"completeness":"incomplete"`) {
		t.Errorf("incomplete author body = %s", response.Body.String())
	}
}

func TestReloadRejectsInvalidConfigWithoutChangingActiveCollection(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("prometheus", `
collections:
  - id: exporters
    name: Exporter maintenance
    repositories: [prometheus/node_exporter]
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	if err := os.WriteFile(configPath, []byte(testConfig("not-a-repository", `
collections:
  - id: INVALID
    name: Broken
    repositories: [not-a-repository]
`)), 0o600); err != nil {
		t.Fatalf("write invalid config error = %v", err)
	}
	if err := app.Reload(context.Background()); err == nil {
		t.Fatal("Reload() error = nil, want validation error")
	}

	request := httptest.NewRequest(http.MethodGet, "/api/collections", http.NoBody)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET collections status = %d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"id":"exporters"`) {
		t.Errorf("active collections changed after failed reload: %s", response.Body.String())
	}
}

func TestApplicationServesEmptyPullRequestListAsArray(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("prometheus", `
collections:
  - id: exporters
    name: Exporter maintenance
    repositories: [prometheus/node_exporter]
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })

	request := httptest.NewRequest(http.MethodGet, "/api/collections/exporters/pull-requests", http.NoBody)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET PRs status = %d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"pullRequests":[]`) {
		t.Errorf("GET PRs body = %s, want empty JSON array", response.Body.String())
	}
}

func TestSeedFixturePersistsPullRequestForCollection(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("prometheus", `
collections:
  - id: prometheus
    name: Prometheus
    repositories: [prometheus/prometheus]
`))
	databasePath := filepath.Join(t.TempDir(), "app.db")
	app, err := New(context.Background(), configPath, databasePath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	fixturePath := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(fixturePath, []byte(`{
  "pullRequests": [
    {
      "collectionID": "prometheus",
      "repository": "prometheus/prometheus",
      "number": 123,
      "title": "Demonstrate the SQLite read path",
      "additions": 42,
      "deletions": 7,
      "updatedAt": "2026-09-08T15:00:00Z",
      "url": "https://github.com/prometheus/prometheus/pull/123"
    }
  ]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.SeedFixture(context.Background(), fixturePath); err != nil {
		t.Fatalf("SeedFixture() error = %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := New(context.Background(), configPath, databasePath)
	if err != nil {
		t.Fatalf("New(reopen) error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	pullRequests, err := reopened.Store().ListPullRequests(context.Background(), "prometheus")
	if err != nil {
		t.Fatalf("ListPullRequests() error = %v", err)
	}
	if len(pullRequests) != 1 || pullRequests[0].Number != 123 {
		t.Errorf("ListPullRequests() = %+v, want persisted fixture PR", pullRequests)
	}
}

func TestNewAllowsGitHubUserAuthWithCollectionAuthentication(t *testing.T) {
	t.Setenv("USER_AUTH_ONLY_SECRET", "test-secret")
	configPath := writeConfig(t, `
external_base_url: https://cockpit.example.test
github:
  schedule: "0 * * * *"
  authentication:
    profiles:
      test-pat:
        type: fine_grained_pat
        token:
          environment: TEST_GITHUB_TOKEN
    owners:
      acme:
        profiles: [test-pat]
  user_auth:
    client_id: test-client
    client_secret:
      environment: USER_AUTH_ONLY_SECRET
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`)
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
}

func testConfig(owner, body string) string {
	return fmt.Sprintf(`
github:
  schedule: "0 * * * *"
  authentication:
    profiles:
      test-pat:
        type: fine_grained_pat
        token:
          environment: TEST_GITHUB_TOKEN
    owners:
      %s:
        profiles: [test-pat]
`, owner) + body
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
