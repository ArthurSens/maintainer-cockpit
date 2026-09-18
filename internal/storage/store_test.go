package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/analysis"
	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	listquery "github.com/ArthurSens/maintainer-cockpit/internal/query"
)

func TestOpenInitializesAndReopensCurrentSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "store.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open(current database) error = %v", err)
	}
	_ = reopened.Close()
}

func TestOpenRejectsKnownStaleSchemaWithRecreateGuidance(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "stale.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE pull_requests (
			repository TEXT NOT NULL,
			number INTEGER NOT NULL,
			PRIMARY KEY (repository, number)
		)
	`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(path)
	if err == nil ||
		!strings.Contains(err.Error(), "recreate the database") ||
		!strings.Contains(err.Error(), "readiness_json") {
		t.Fatalf("Open() error = %v, want actionable stale-schema guidance", err)
	}
}

func TestOpenRejectsRemovedAdjustedSizeSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "adjusted-size.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE pull_requests (
			repository TEXT NOT NULL,
			number INTEGER NOT NULL,
			readiness_json TEXT NOT NULL DEFAULT '{}',
			adjusted_additions INTEGER,
			PRIMARY KEY (repository, number)
		)
	`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(path)
	if err == nil ||
		!strings.Contains(err.Error(), "adjusted_additions") ||
		!strings.Contains(err.Error(), "back up and recreate") {
		t.Fatalf("Open() error = %v, want removed adjusted-size schema guidance", err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var additiveTables int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name IN (
			'pull_request_diff_evidence', 'pull_request_diff_sources',
			'pull_request_diff_jobs', 'analysis_attempt_state'
		)
	`).Scan(&additiveTables); err != nil {
		t.Fatal(err)
	}
	if additiveTables != 0 {
		t.Fatalf("failed startup created %d additive tables", additiveTables)
	}
	var columns string
	if err := db.QueryRow(`
		SELECT group_concat(name, ',') FROM pragma_table_info('pull_requests')
	`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != "repository,number,readiness_json,adjusted_additions" {
		t.Fatalf("failed startup changed pull_requests columns to %q", columns)
	}
}

func TestStorePersistsCollectionsAndPullRequests(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "maintainer-cockpit.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	collections := []config.Collection{{
		ID:           "exporters",
		Name:         "Exporter maintenance",
		Description:  "Pull requests across exporter repositories.",
		Repositories: []string{"prometheus/node_exporter"},
	}}
	if err := store.SyncCollections(ctx, collections); err != nil {
		t.Fatalf("SyncCollections() error = %v", err)
	}
	updatedAt := time.Date(2026, time.September, 8, 15, 0, 0, 0, time.UTC)
	if err := store.UpsertPullRequest(ctx, "exporters", PullRequest{
		Repository: "prometheus/node_exporter",
		Number:     123,
		Title:      "Make collector behavior explicit",
		Additions:  42,
		Deletions:  7,
		UpdatedAt:  updatedAt,
		URL:        "https://github.com/prometheus/node_exporter/pull/123",
	}); err != nil {
		t.Fatalf("UpsertPullRequest() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open(reopen) error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	gotCollections, err := reopened.ListCollections(ctx)
	if err != nil {
		t.Fatalf("ListCollections() error = %v", err)
	}
	if len(gotCollections) != 1 {
		t.Fatalf("len(ListCollections()) = %d, want 1", len(gotCollections))
	}
	if gotCollections[0].ID != "exporters" || gotCollections[0].OpenPRs != 1 {
		t.Errorf("ListCollections()[0] = %+v, want exporters with one PR", gotCollections[0])
	}

	gotPRs, err := reopened.ListPullRequests(ctx, "exporters")
	if err != nil {
		t.Fatalf("ListPullRequests() error = %v", err)
	}
	if len(gotPRs) != 1 {
		t.Fatalf("len(ListPullRequests()) = %d, want 1", len(gotPRs))
	}
	if gotPRs[0].Title != "Make collector behavior explicit" || !gotPRs[0].UpdatedAt.Equal(updatedAt) {
		t.Errorf("ListPullRequests()[0] = %+v, want persisted fixture", gotPRs[0])
	}
}

func TestAuthorEvidenceJobsAreDeduplicatedAndGateOnlyContribution(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const source = "acme/widgets"
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "mixed", Name: "Mixed",
		Repositories: []string{source, "other/tools"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRepositorySnapshot(ctx, source, []PullRequest{{
		Repository: source, Number: 1, Title: "Core remains available",
		Author: "octocat", AuthorID: 42, CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/1",
		ReviewState: "none",
	}}, now); err != nil {
		t.Fatal(err)
	}
	authors := []EvidenceAuthor{{ID: 42, Login: "octocat", Association: "CONTRIBUTOR"}}
	if err := store.EnqueueAuthorEvidenceAfterSnapshot(ctx, source, authors, now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueAuthorEvidenceAfterSnapshot(ctx, source, authors, now); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimContributionEvidence(ctx, now)
	if err != nil || first == nil {
		t.Fatalf("ClaimContributionEvidence() = %+v, %v", first, err)
	}
	second, err := store.ClaimContributionEvidence(ctx, now)
	if err != nil || second == nil || second.Repository == first.Repository {
		t.Fatalf("second ClaimContributionEvidence() = %+v, %v", second, err)
	}
	if extra, err := store.ClaimContributionEvidence(ctx, now); err != nil || extra != nil {
		t.Fatalf("deduplicated extra repository job = %+v, %v", extra, err)
	}
	if err := store.CompleteContributionEvidence(
		ctx, first, RepositoryContribution{Merged: 1}, now, nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteContributionEvidence(
		ctx, second, RepositoryContribution{}, now, errors.New("owner route failed"),
	); err != nil {
		t.Fatal(err)
	}
	ancillary, err := store.ClaimAncillaryAuthorEvidence(ctx, now)
	if err != nil || ancillary == nil {
		t.Fatalf("ClaimAncillaryAuthorEvidence() = %+v, %v", ancillary, err)
	}
	if err := store.CompleteAncillaryAuthorEvidence(
		ctx, ancillary, now.AddDate(-1, 0, 0), nil, now, nil,
	); err != nil {
		t.Fatal(err)
	}
	author, err := store.GetAuthorContext(ctx, "mixed", "octocat", now)
	if err != nil {
		t.Fatal(err)
	}
	if author.Contribution.Completeness != "incomplete" {
		t.Errorf("contribution completeness = %q, want incomplete", author.Contribution.Completeness)
	}
	if err := store.EnqueueAuthorEvidenceAfterSnapshot(ctx, source, authors, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	retry, err := store.ClaimContributionEvidence(ctx, now.Add(time.Minute))
	if err != nil || retry == nil || retry.Repository != second.Repository {
		t.Fatalf("failed-owner retry = %+v, %v", retry, err)
	}
	if err := store.CompleteContributionEvidence(
		ctx, retry, RepositoryContribution{Merged: 2}, now.Add(time.Minute), nil,
	); err != nil {
		t.Fatal(err)
	}
	author, err = store.GetAuthorContext(ctx, "mixed", "octocat", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if author.Contribution.Completeness != "complete" {
		t.Errorf("contribution completeness after all jobs = %q, want complete",
			author.Contribution.Completeness)
	}
	prs, err := store.ListPullRequests(ctx, "mixed")
	if err != nil || len(prs) != 1 || prs[0].Title != "Core remains available" {
		t.Errorf("core pull requests = %+v, %v", prs, err)
	}
	refreshes, err := store.ListRepositoryRefreshes(ctx)
	if err != nil || len(refreshes) != 2 {
		t.Fatalf("ListRepositoryRefreshes() = %+v, %v", refreshes, err)
	}
	for _, refresh := range refreshes {
		if refresh.Repository == source && refresh.State != RefreshFresh {
			t.Errorf("core refresh state = %q, want fresh", refresh.State)
		}
	}
}

func TestContributionEvidenceClaimIsAtomicAndDeduplicated(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const repository = "acme/widgets"
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{repository},
	}}); err != nil {
		t.Fatal(err)
	}
	authors := []EvidenceAuthor{{ID: 42, Login: "octocat"}}
	if err := store.EnqueueAuthorEvidenceAfterSnapshot(ctx, repository, authors, now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueAuthorEvidenceAfterSnapshot(ctx, repository, authors, now); err != nil {
		t.Fatal(err)
	}

	type result struct {
		job *ContributionEvidenceJob
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			job, err := store.ClaimContributionEvidence(ctx, now)
			results <- result{job: job, err: err}
		}()
	}
	close(start)
	claimed := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("ClaimContributionEvidence() error = %v", result.err)
		}
		if result.job != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Errorf("concurrent claims = %d, want exactly 1", claimed)
	}
}

func TestSamePullRequestIsSharedAcrossOverlappingCollections(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const repository = "prometheus/prometheus"
	if err := store.SyncCollections(ctx, []config.Collection{
		{ID: "core", Name: "Core", Repositories: []string{repository}},
		{ID: "interop", Name: "Interop", Repositories: []string{repository}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRepositorySnapshot(ctx, repository, []PullRequest{
		{
			Repository: repository, Number: 1, Title: "Shared", Author: "octocat",
			CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
			URL: "https://github.com/prometheus/prometheus/pull/1", ReviewState: "none",
		},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, collectionID := range []string{"core", "interop"} {
		pullRequests, err := store.ListPullRequests(ctx, collectionID)
		if err != nil || len(pullRequests) != 1 {
			t.Fatalf("ListPullRequests(%q) = %+v, %v", collectionID, pullRequests, err)
		}
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pull_requests").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("physical pull request rows = %d, want 1", rows)
	}
}

func TestListPullRequestsPageFiltersSortsAndPaginates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Now()
	var pullRequests []PullRequest
	for number, title := range []string{"Alpha metrics", "Beta metrics", "Unrelated"} {
		pullRequests = append(pullRequests, PullRequest{
			Repository: repository, Number: number + 1, Title: title, Author: "octocat",
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(time.Duration(number) * time.Minute),
			URL:         fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number+1),
			ReviewState: "approved",
		})
	}
	if err := store.ReplaceRepositorySnapshot(ctx, repository, pullRequests, now); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListPullRequestsPage(ctx, "acme", listquery.Options{
		Query: "metrics", Sort: "author,title", Order: "asc,asc", Limit: 1,
	})
	if err != nil {
		t.Fatalf("ListPullRequestsPage() error = %v", err)
	}
	if page.Total != 3 || page.Matched != 2 || len(page.PullRequests) != 1 ||
		page.PullRequests[0].Title != "Alpha metrics" || page.NextCursor == "" {
		t.Errorf("page = %+v", page)
	}
}

func TestReviewLoadSortFilterAndAnalysisStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Now().UTC()
	pullRequests := []PullRequest{
		{Repository: repository, Number: 1, Title: "Low", UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/1"},
		{Repository: repository, Number: 2, Title: "High", UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/2"},
		{Repository: repository, Number: 3, Title: "Unavailable", UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/3"},
	}
	if err := store.ReplaceRepositorySnapshot(ctx, repository, pullRequests, now); err != nil {
		t.Fatal(err)
	}
	for number, level := range map[int]string{1: "low", 2: "high"} {
		resultJSON := fmt.Sprintf(`{"reviewCognitiveLoad":{"overall":%q}}`, level)
		if _, err := store.db.ExecContext(ctx, `
			INSERT INTO analysis_results (
				collection_id, repository, number, status, input_revision, provider,
				fallback_from, model, analyzed_at, payload_id, result_json,
				quality_level, error, schema_version, prompt_version
			) VALUES ('acme', ?, ?, 'complete', 'revision', 'provider', '', 'model', ?,
				'', ?, 'no_concerns', '', 'schema', 'prompt')
		`, repository, number, formatTime(now), resultJSON); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `
			INSERT INTO analysis_attempt_state (
				collection_id, repository, number, status, input_revision, provider,
				fallback_from, model, attempted_at, payload_id, error, schema_version, prompt_version
			) VALUES ('acme', ?, ?, 'complete', 'revision', 'provider', '', 'model', ?,
				'', '', 'schema', 'prompt')
		`, repository, number, formatTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO analysis_attempt_state (
			collection_id, repository, number, status, input_revision, provider,
			fallback_from, model, attempted_at, payload_id, error, schema_version, prompt_version
		) VALUES ('acme', ?, 3, 'failed', '', '', '', '', ?, '', 'provider failed', 'schema', 'prompt')
	`, repository, formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO analysis_jobs (
			collection_id, repository, number, status, forced, scheduled_for
		) VALUES ('acme', ?, 1, 'queued', 0, ?)
	`, repository, formatTime(now)); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		order string
		want  []int
	}{
		{order: "asc", want: []int{1, 2, 3}},
		{order: "desc", want: []int{2, 1, 3}},
	} {
		page, err := store.ListPullRequestsPage(ctx, "acme", listquery.Options{
			Sort: "review_load", Order: tc.order, Limit: 50,
		})
		if err != nil {
			t.Fatal(err)
		}
		got := make([]int, 0, len(page.PullRequests))
		for _, pr := range page.PullRequests {
			got = append(got, pr.Number)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("review load %s order = %v, want %v", tc.order, got, tc.want)
		}
	}

	filtered, err := store.ListPullRequestsPage(ctx, "acme", listquery.Options{
		ReviewLoad: "high", AnalysisStatus: "available",
		Sort: "updated", Order: "desc", Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.PullRequests) != 1 || filtered.PullRequests[0].Number != 2 {
		t.Errorf("available high review load = %+v", filtered.PullRequests)
	}
	stale, err := store.ListPullRequestsPage(ctx, "acme", listquery.Options{
		AnalysisStatus: "stale", Sort: "updated", Order: "desc", Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale.PullRequests) != 1 || stale.PullRequests[0].Number != 1 {
		t.Errorf("stale analysis = %+v", stale.PullRequests)
	}
}

func TestQueuedAnalysisStatusUsesAnyRetainedValidSection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Now().UTC()
	if err := store.ReplaceRepositorySnapshot(ctx, repository, []PullRequest{
		{
			Repository: repository, Number: 1, Title: "Retained summary",
			UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/1",
		},
		{
			Repository: repository, Number: 2, Title: "No retained result",
			UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/2",
		},
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO analysis_results (
			collection_id, repository, number, status, input_revision, provider,
			fallback_from, model, analyzed_at, payload_id, result_json,
			quality_level, error, schema_version, prompt_version
		) VALUES ('acme', ?, 1, 'complete', 'revision', 'provider', '', 'model', ?,
			'', '{}',
			'no_concerns', '', 'analysis.v2', 'analysis-prompt.v2')
	`, repository, formatTime(now)); err != nil {
		t.Fatal(err)
	}
	for _, number := range []int{1, 2} {
		if _, err := store.db.ExecContext(ctx, `
			INSERT INTO analysis_jobs (
				collection_id, repository, number, head_sha, status, forced, scheduled_for
			) VALUES ('acme', ?, ?, '', 'queued', 0, ?)
		`, repository, number, formatTime(now)); err != nil {
			t.Fatal(err)
		}
	}

	page, err := store.ListPullRequestsPage(ctx, "acme", listquery.Options{
		Sort: "number", Order: "asc", Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.PullRequests) != 2 {
		t.Fatalf("pull requests = %+v", page.PullRequests)
	}
	if page.PullRequests[0].Analysis.AnalysisStatus != "stale" {
		t.Errorf(
			"retained-summary status = %q, want stale",
			page.PullRequests[0].Analysis.AnalysisStatus,
		)
	}
	if page.PullRequests[1].Analysis.AnalysisStatus != "pending" {
		t.Errorf(
			"no-result status = %q, want pending",
			page.PullRequests[1].Analysis.AnalysisStatus,
		)
	}
}

func TestPullRequestContextPersistsSourceIdentityTimestampsAndLimits(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Now().UTC().Truncate(time.Second)
	mergeable := false
	pr := PullRequest{
		Repository: repository, Number: 7, Title: "Context", Author: "octocat",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/7", ReviewState: "approved",
		Readiness: PullRequestReadiness{
			RequestedReviewers: []string{"alice"},
			RequestedTeams:     []string{"maintainers"},
			Mergeable:          &mergeable,
			MergeableState:     "dirty",
			Checks: CheckSummary{Total: 1, Runs: []CheckRun{{
				ID: 1, Name: "unit", Status: "completed", Conclusion: "failure",
				URL: "https://github.com/acme/widgets/actions/runs/1",
			}}},
			CollectedAt: now,
		},
		Context: &PullRequestContext{
			Completeness: "partial", CollectedAt: now,
			Limits: []ContextLimit{{Scope: "review_threads", Reason: "page_limit", Collected: 100}},
			Relationships: []Relationship{{
				Kind: "closing_issue", SourceID: "I_42", SourceRepository: repository,
				SourceNumber: 42, Title: "Tracking issue", State: "OPEN",
				URL: "https://github.com/acme/widgets/issues/42", Timestamp: now.Add(-time.Minute),
			}},
			ExternalURLs: []ExternalURL{{
				URL: "https://example.com/design", SourceKind: "pull_request", SourceID: "PR_7",
				DiscoveredAt: now,
			}},
		},
	}
	if err := store.ReplaceRepositorySnapshot(ctx, repository, []PullRequest{pr}, now); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetPullRequestDetail(ctx, "acme", repository, 7)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Context.Completeness != "partial" || len(detail.Context.Limits) != 1 ||
		len(detail.Context.Relationships) != 1 || detail.Context.Relationships[0].SourceID != "I_42" ||
		len(detail.Context.ExternalURLs) != 1 {
		t.Errorf("detail context = %+v", detail.Context)
	}
	if detail.Readiness.Mergeable == nil || *detail.Readiness.Mergeable ||
		detail.Readiness.MergeableState != "dirty" ||
		len(detail.Readiness.RequestedReviewers) != 1 ||
		detail.Readiness.Checks.Total != 1 ||
		!detail.Readiness.CollectedAt.Equal(now) {
		t.Errorf("detail readiness = %+v", detail.Readiness)
	}
}

func TestRawChurnAndPerFileValuesPersistAndSort(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Now().UTC()
	pullRequests := []PullRequest{
		{
			Repository: repository, Number: 1, Title: "Mostly generated",
			Additions: 104, Deletions: 1,
			Files: []PullRequestFile{
				{Path: "main.go", Additions: 4, Deletions: 1},
				{Path: "generated.go", Additions: 100},
			},
			UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/1",
		},
		{
			Repository: repository, Number: 2, Title: "Raw fallback",
			Additions: 10, Deletions: 2,
			Files:     []PullRequestFile{{Path: "main.go", Additions: 10, Deletions: 2}},
			UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/2",
		},
	}
	if err := store.ReplaceRepositorySnapshot(ctx, repository, pullRequests, now); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListPullRequestsPage(ctx, "acme", listquery.Options{
		Sort: "churn", Order: "asc", Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.PullRequests) != 2 || page.PullRequests[0].Number != 2 {
		t.Fatalf("raw churn sort = %#v, want PR 2 before PR 1", page.PullRequests)
	}
	detail, err := store.GetPullRequestDetail(ctx, "acme", repository, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Files) != 2 || detail.Files[0].Path != "generated.go" ||
		detail.Files[0].Additions != 100 || detail.Files[0].Deletions != 0 {
		t.Errorf("persisted raw detail = %+v", detail)
	}
}

func broadAdditionsPullRequest(repository string, number int, updatedAt time.Time) PullRequest {
	files := make([]PullRequestFile, 0, 12)
	for index := range 12 {
		files = append(files, PullRequestFile{
			Path:      fmt.Sprintf("pkg/feature/file%02d.go", index),
			Additions: 40,
		})
	}
	return PullRequest{
		Repository: repository, Number: number, Title: "Broad additions", Author: "octocat",
		Additions: 480, Deletions: 0, ChangedFiles: 12,
		Files:     files,
		CreatedAt: updatedAt.Add(-time.Hour), UpdatedAt: updatedAt,
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", repository, number),
	}
}

func TestQualityAssessmentPersistsAndSupportsFilterAndSort(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Now().UTC()
	clean := PullRequest{
		Repository: repository, Number: 1, Title: "Small fix", Author: "octocat",
		Additions: 4, Deletions: 2, ChangedFiles: 1,
		Body:      "Fix parsing.\n\n🤖 Generated with Claude Code",
		Files:     []PullRequestFile{{Path: "main.go", Additions: 4, Deletions: 2}},
		UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/1",
	}
	broad := broadAdditionsPullRequest(repository, 2, now)
	mismatch := PullRequest{
		Repository: repository, Number: 3, Title: "Mismatch", Author: "octocat",
		Additions: 5, Deletions: 0, ChangedFiles: 1,
		Body:      "Rewrites cmd/serve/main.go, internal/auth/session.go, and internal/auth/token.go.",
		Files:     []PullRequestFile{{Path: "docs/README.md", Additions: 5}},
		UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/3",
	}
	if err := store.ReplaceRepositorySnapshot(
		ctx, repository, []PullRequest{clean, broad, mismatch}, now,
	); err != nil {
		t.Fatal(err)
	}

	page, err := store.ListPullRequestsPage(ctx, "acme", listquery.Options{
		Sort: "quality", Order: "desc", Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListPullRequestsPage(sort quality) error = %v", err)
	}
	if len(page.PullRequests) != 3 ||
		page.PullRequests[0].Number != 3 || page.PullRequests[0].Quality.Level != "strong_concerns" ||
		page.PullRequests[1].Number != 2 || page.PullRequests[1].Quality.Level != "review_suggested" ||
		page.PullRequests[2].Number != 1 || page.PullRequests[2].Quality.Level != "no_concerns" {
		t.Fatalf("quality sort = %+v", page.PullRequests)
	}
	if page.PullRequests[0].Quality.Findings != 1 {
		t.Errorf("strong concerns findings = %d, want 1", page.PullRequests[0].Quality.Findings)
	}

	filtered, err := store.ListPullRequestsPage(ctx, "acme", listquery.Options{
		Quality: "strong_concerns", Sort: "updated", Order: "desc", Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListPullRequestsPage(filter quality) error = %v", err)
	}
	if filtered.Matched != 1 || len(filtered.PullRequests) != 1 || filtered.PullRequests[0].Number != 3 {
		t.Errorf("quality filter = %+v", filtered)
	}

	detail, err := store.GetPullRequestDetail(ctx, "acme", repository, 3)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Quality.Level != "strong_concerns" || len(detail.Quality.Findings) != 1 {
		t.Fatalf("detail quality = %+v", detail.Quality)
	}
	finding := detail.Quality.Findings[0]
	if finding.Rule != "description_diff_mismatch" || finding.Severity != "high" ||
		finding.Provenance != "rule_derived" || len(finding.Evidence) == 0 ||
		finding.Completeness == "" {
		t.Errorf("detail finding = %+v", finding)
	}
	if detail.Quality.Policy.BroadAdditionsOnly.MinFiles.Source != "default" {
		t.Errorf("policy = %+v, want documented defaults", detail.Quality.Policy)
	}

	cleanDetail, err := store.GetPullRequestDetail(ctx, "acme", repository, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cleanDetail.Quality.Level != "no_concerns" || len(cleanDetail.Quality.AIDisclosures) != 1 {
		t.Errorf("clean detail quality = %+v, want disclosure metadata without concerns", cleanDetail.Quality)
	}
}

func TestModelAndDeterministicQualityCombineAndRemainInspectable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const repository = "acme/widgets"
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{repository},
		ModelProvider: "model",
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	clean := PullRequest{
		Repository: repository, Number: 1, Title: "Clean", Body: "Updates main.go.",
		Additions: 5, Deletions: 1, ChangedFiles: 1,
		Files:     []PullRequestFile{{Path: "main.go", Additions: 5, Deletions: 1}},
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		URL: "https://github.com/acme/widgets/pull/1",
	}
	strong := PullRequest{
		Repository: repository, Number: 2, Title: "Mismatch",
		Body:      "Rewrites cmd/serve/main.go, internal/auth/session.go, and internal/auth/token.go.",
		Additions: 5, Deletions: 1, ChangedFiles: 1,
		Files:     []PullRequestFile{{Path: "docs/README.md", Additions: 5, Deletions: 1}},
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(time.Minute),
		URL: "https://github.com/acme/widgets/pull/2",
	}
	if err := store.ReplaceRepositorySnapshot(
		ctx, repository, []PullRequest{clean, strong}, now,
	); err != nil {
		t.Fatal(err)
	}

	job := AnalysisJob{
		CollectionID: "acme", Repository: repository, Number: 1, PrimaryProvider: "model",
	}
	request, err := store.BuildAnalysisRequest(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	evaluations := []analysis.QualityEvaluation{
		{
			Family: "unrelated_changes", Status: "finding", Completeness: "partial",
			Finding: &analysis.QualityFinding{
				Severity: "medium", Summary: "A changed file is unrelated to the stated goal.",
				SourceIDs: []string{request.PullRequest.SourceID}, Confidence: "high",
			},
		},
		{Family: "description_diff_mismatch", Status: "no_concern", Completeness: "complete"},
		{Family: "missing_expected_tests", Status: "no_concern", Completeness: "complete"},
		{
			Family: "internal_contradictions", Status: "not_evaluated", Completeness: "partial",
			Reason: "Discussion context was truncated.",
		},
		{Family: "unsupported_references", Status: "no_concern", Completeness: "complete"},
	}
	if err := store.RecordAnalysis(
		ctx, job, request, analysis.Result{
			Status: "complete", QualityLevel: "review_suggested",
			QualityEvaluations: evaluations,
		},
		"model", "", "test-model", []byte(`{}`), []byte(`{}`), now,
	); err != nil {
		t.Fatal(err)
	}

	page, err := store.ListPullRequestsPage(ctx, "acme", listquery.Options{
		Sort: "quality", Order: "desc", Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.PullRequests[0].Number != 2 ||
		page.PullRequests[0].Quality.Level != "strong_concerns" ||
		page.PullRequests[1].Number != 1 ||
		page.PullRequests[1].Quality.Level != "review_suggested" ||
		page.PullRequests[1].Quality.Findings != 1 {
		t.Fatalf("combined quality page = %+v", page.PullRequests)
	}

	detail, err := store.GetPullRequestDetail(ctx, "acme", repository, 1)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Quality.Level != "review_suggested" || len(detail.Quality.Findings) != 1 ||
		detail.Quality.Findings[0].Rule != "unrelated_changes" ||
		detail.Quality.Findings[0].Provenance != "model_inferred" ||
		detail.Quality.Findings[0].Confidence != "high" ||
		len(detail.Quality.Unevaluated) != 1 ||
		detail.Quality.Unevaluated[0].Rule != "internal_contradictions" ||
		detail.Quality.Unevaluated[0].Provenance != "model_inferred" {
		t.Fatalf("combined quality detail = %+v", detail.Quality)
	}
	if len(detail.Analysis.QualityEvaluations) != 5 {
		t.Errorf("model evaluations = %+v, want all five families", detail.Analysis.QualityEvaluations)
	}

	current, err := store.AnalysisIsCurrent(ctx, job, request.InputRevision)
	if err != nil || !current {
		t.Fatalf("AnalysisIsCurrent() = %t, %v, want current", current, err)
	}
	if _, err := store.db.ExecContext(ctx, `
		UPDATE analysis_attempt_state SET prompt_version = 'analysis-prompt.changed'
		WHERE collection_id = ? AND repository = ? AND number = ?
	`, job.CollectionID, job.Repository, job.Number); err != nil {
		t.Fatal(err)
	}
	current, err = store.AnalysisIsCurrent(ctx, job, request.InputRevision)
	if err != nil || !current {
		t.Fatalf("AnalysisIsCurrent(changed prompt) = %t, %v, want current", current, err)
	}
	if _, err := store.db.ExecContext(ctx, `
		UPDATE analysis_results SET schema_version = 'analysis.v0'
		WHERE collection_id = ? AND repository = ? AND number = ?
	`, job.CollectionID, job.Repository, job.Number); err != nil {
		t.Fatal(err)
	}
	current, err = store.AnalysisIsCurrent(ctx, job, request.InputRevision)
	if err != nil || current {
		t.Fatalf("AnalysisIsCurrent(old schema) = %t, %v, want stale", current, err)
	}
}

func TestQualityPolicyIsTunedPerCollection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const repository = "acme/widgets"
	minFiles := 30
	if err := store.SyncCollections(ctx, []config.Collection{
		{ID: "core", Name: "Core", Repositories: []string{repository}},
		{
			ID: "tuned", Name: "Tuned", Repositories: []string{repository},
			Quality: &config.Quality{
				BroadAdditionsOnly: &config.BroadAdditionsOnly{MinFiles: &minFiles},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.ReplaceRepositorySnapshot(
		ctx, repository, []PullRequest{broadAdditionsPullRequest(repository, 1, now)}, now,
	); err != nil {
		t.Fatal(err)
	}
	coreDetail, err := store.GetPullRequestDetail(ctx, "core", repository, 1)
	if err != nil {
		t.Fatal(err)
	}
	if coreDetail.Quality.Level != "review_suggested" {
		t.Errorf("core quality = %+v, want review_suggested under defaults", coreDetail.Quality)
	}
	tunedDetail, err := store.GetPullRequestDetail(ctx, "tuned", repository, 1)
	if err != nil {
		t.Fatal(err)
	}
	if tunedDetail.Quality.Level != "no_concerns" {
		t.Errorf("tuned quality = %+v, want no_concerns under local threshold", tunedDetail.Quality)
	}
	if tunedDetail.Quality.Policy.BroadAdditionsOnly.MinFiles.Source != "local" ||
		tunedDetail.Quality.Policy.BroadAdditionsOnly.MinFiles.Value != 30 {
		t.Errorf("tuned policy = %+v, want local min_files 30", tunedDetail.Quality.Policy)
	}
}

func TestSyncCollectionsRecomputesQualityWhenPolicyChanges(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
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
	now := time.Now().UTC()
	if err := store.ReplaceRepositorySnapshot(
		ctx, repository, []PullRequest{broadAdditionsPullRequest(repository, 1, now)}, now,
	); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetPullRequestDetail(ctx, "acme", repository, 1)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Quality.Level != "review_suggested" {
		t.Fatalf("quality before reload = %+v, want review_suggested", detail.Quality)
	}

	minFiles := 30
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{repository},
		Quality: &config.Quality{
			BroadAdditionsOnly: &config.BroadAdditionsOnly{MinFiles: &minFiles},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	detail, err = store.GetPullRequestDetail(ctx, "acme", repository, 1)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Quality.Level != "no_concerns" {
		t.Errorf("quality after reload = %+v, want recomputed no_concerns", detail.Quality)
	}
	if detail.Quality.Policy.BroadAdditionsOnly.MinFiles.Source != "local" {
		t.Errorf("policy after reload = %+v, want local min_files", detail.Quality.Policy)
	}
}

func TestRepositoryProviderLimitsAreVisibleOnCollection(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "maintainer-cockpit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "platform", Name: "Platform", Repositories: []string{"acme/widgets"},
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	if err := store.ReplaceRepositorySnapshotWithCompleteness(
		ctx, "acme/widgets", nil, now,
		[]ContextLimit{{
			Scope: "discovery_search", Reason: "provider_limit",
			Collected: 1000, Total: 1200,
		}},
	); err != nil {
		t.Fatal(err)
	}
	collections, err := store.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collections) != 1 || len(collections[0].Completeness) != 1 {
		t.Fatalf("collections = %#v", collections)
	}
	limit := collections[0].Completeness[0]
	if limit.Repository != "acme/widgets" || limit.Total != 1200 {
		t.Errorf("completeness = %#v", limit)
	}
}

func TestSyncCollectionsIsAtomic(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "maintainer-cockpit.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	initial := []config.Collection{{
		ID:           "exporters",
		Name:         "Exporter maintenance",
		Repositories: []string{"prometheus/node_exporter"},
	}}
	if err := store.SyncCollections(ctx, initial); err != nil {
		t.Fatalf("SyncCollections(initial) error = %v", err)
	}

	invalid := []config.Collection{{
		ID:           "other",
		Name:         "Other",
		Repositories: []string{"prometheus/blackbox_exporter", "prometheus/blackbox_exporter"},
	}}
	if err := store.SyncCollections(ctx, invalid); err == nil {
		t.Fatal("SyncCollections(invalid) error = nil, want error")
	}

	got, err := store.ListCollections(ctx)
	if err != nil {
		t.Fatalf("ListCollections() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "exporters" {
		t.Errorf("ListCollections() = %+v, want unchanged exporters collection", got)
	}
}

func TestSyncCollectionsPreservesPRsForUnchangedRepository(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "maintainer-cockpit.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	initial := []config.Collection{{
		ID:           "exporters",
		Name:         "Exporter maintenance",
		Repositories: []string{"prometheus/node_exporter"},
	}}
	if err := store.SyncCollections(ctx, initial); err != nil {
		t.Fatalf("SyncCollections(initial) error = %v", err)
	}
	if err := store.UpsertPullRequest(ctx, "exporters", PullRequest{
		Repository: "prometheus/node_exporter",
		Number:     123,
		Title:      "Make collector behavior explicit",
		UpdatedAt:  time.Date(2026, time.September, 8, 15, 0, 0, 0, time.UTC),
		URL:        "https://github.com/prometheus/node_exporter/pull/123",
	}); err != nil {
		t.Fatalf("UpsertPullRequest() error = %v", err)
	}

	renamed := []config.Collection{{
		ID:           "exporters",
		Name:         "Prometheus exporters",
		Description:  "Updated display metadata.",
		Repositories: []string{"prometheus/node_exporter"},
	}}
	if err := store.SyncCollections(ctx, renamed); err != nil {
		t.Fatalf("SyncCollections(updated) error = %v", err)
	}

	got, err := store.ListPullRequests(ctx, "exporters")
	if err != nil {
		t.Fatalf("ListPullRequests() error = %v", err)
	}
	if len(got) != 1 || got[0].Number != 123 {
		t.Errorf("ListPullRequests() = %+v, want existing PR preserved", got)
	}
}

func TestReplaceRepositorySnapshotIsAtomicAndRemovesClosedPullRequests(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "maintainer-cockpit.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "exporters", Name: "Exporters", Repositories: []string{"prometheus/node_exporter"},
	}}); err != nil {
		t.Fatal(err)
	}
	firstRefresh := time.Now().UTC().Add(-time.Hour)
	if err := store.ReplaceRepositorySnapshot(ctx, "prometheus/node_exporter", []PullRequest{
		testPullRequest(1, "First"),
		testPullRequest(2, "Second"),
	}, firstRefresh); err != nil {
		t.Fatalf("ReplaceRepositorySnapshot(first) error = %v", err)
	}
	if err := store.RecordRefreshResult(ctx, "prometheus/node_exporter", RefreshIncomplete, firstRefresh.Add(time.Hour), "detail request failed"); err != nil {
		t.Fatalf("RecordRefreshResult(incomplete) error = %v", err)
	}
	unchanged, err := store.ListPullRequests(ctx, "exporters")
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged) != 2 {
		t.Fatalf("partial refresh changed snapshot: got %d PRs", len(unchanged))
	}

	secondRefresh := time.Now().UTC()
	if err := store.ReplaceRepositorySnapshot(ctx, "prometheus/node_exporter", []PullRequest{
		testPullRequest(2, "Second updated"),
	}, secondRefresh); err != nil {
		t.Fatalf("ReplaceRepositorySnapshot(second) error = %v", err)
	}
	got, err := store.ListPullRequests(ctx, "exporters")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != 2 || got[0].Title != "Second updated" {
		t.Errorf("ListPullRequests() = %+v, want replacement snapshot", got)
	}
	collections, err := store.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if collections[0].RefreshState != RefreshFresh || collections[0].LastSuccessfulRefresh == nil ||
		!collections[0].LastSuccessfulRefresh.Equal(secondRefresh) {
		t.Errorf("collection refresh = %+v", collections[0])
	}
}

func TestRefreshJobsAreDurableAndCoalesced(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "maintainer-cockpit.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "exporters", Name: "Exporters", Repositories: []string{"prometheus/node_exporter"},
	}}); err != nil {
		t.Fatal(err)
	}
	scheduledFor := time.Date(2026, 9, 8, 16, 0, 0, 0, time.UTC)
	enqueued, err := store.EnqueueRefresh(ctx, "prometheus/node_exporter", scheduledFor)
	if err != nil || !enqueued {
		t.Fatalf("EnqueueRefresh(first) = %v, %v; want true, nil", enqueued, err)
	}
	enqueued, err = store.EnqueueRefresh(ctx, "prometheus/node_exporter", scheduledFor)
	if err != nil || enqueued {
		t.Fatalf("EnqueueRefresh(duplicate) = %v, %v; want false, nil", enqueued, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open(reopen) error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	job, err := reopened.ClaimRefresh(ctx, scheduledFor)
	if err != nil {
		t.Fatalf("ClaimRefresh() error = %v", err)
	}
	if job == nil || job.Repository != "prometheus/node_exporter" {
		t.Fatalf("ClaimRefresh() = %+v", job)
	}
	other, err := reopened.ClaimRefresh(ctx, scheduledFor)
	if err != nil || other != nil {
		t.Fatalf("ClaimRefresh(second) = %+v, %v; want nil, nil", other, err)
	}
}

func TestSyncCollectionsCancelsQueuedJobsForRemovedRepositories(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/old"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueRefresh(ctx, "acme/old", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/new"},
	}}); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimRefresh(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if job != nil {
		t.Errorf("ClaimRefresh() = %+v, want removed repository job canceled", job)
	}
}

func TestListCollectionsMarksOverdueSnapshotStale(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const repository = "prometheus/prometheus"
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "prometheus", Name: "Prometheus", Repositories: []string{repository},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRepositorySnapshot(ctx, repository, nil, time.Now().Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	collections, err := store.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if collections[0].RefreshState != RefreshStale {
		t.Errorf("RefreshState = %q, want stale", collections[0].RefreshState)
	}
}

func TestCollectionRemainsIncompleteUntilEveryRepositoryRefreshes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/one", "acme/two"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRepositorySnapshot(ctx, "acme/one", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	collections, err := store.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if collections[0].RefreshState != RefreshIncomplete {
		t.Errorf("RefreshState = %q, want incomplete while acme/two is never refreshed", collections[0].RefreshState)
	}
}

func TestRouteAttemptsReplaceCurrentSetAndExposeSanitizedWarning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
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
	at := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if err := store.ReplaceRouteAttempts(ctx, repository, RouteOperationDiscovery, []RouteAttempt{
		{Profile: "private-app", Priority: 0, ProfileType: RouteProfileGitHubApp, Outcome: RouteOutcomeAdvanced, FailureCategory: RouteFailurePermissionMissing, Remediation: "grant required permissions"},
		{Profile: "public-fallback", Priority: 1, ProfileType: RouteProfileFineGrainedPAT, Outcome: RouteOutcomeSelected, Selected: true},
	}, at); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRouteAttempts(ctx, repository, RouteOperationDiscovery, []RouteAttempt{{
		Profile: "replacement", Priority: 0, ProfileType: RouteProfileFineGrainedPAT,
		Outcome: RouteOutcomeSelected, Selected: true,
	}}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	routes, err := store.ListRouteAttempts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || len(routes[0].Attempts) != 1 ||
		routes[0].Attempts[0].Profile != "replacement" {
		t.Fatalf("current routes = %+v, want whole-set replacement", routes)
	}
	collections, err := store.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collections[0].RouteWarnings) != 0 {
		t.Fatalf("primary success produced warning: %+v", collections[0].RouteWarnings)
	}
	if err := store.MarkRouteOperationsPending(ctx, []string{repository}, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	collections, err = store.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collections[0].RouteWarnings) != 5 {
		t.Fatalf("pending warnings = %+v, want all five operations", collections[0].RouteWarnings)
	}
	for _, warning := range collections[0].RouteWarnings {
		if warning.Repository != repository || warning.Status != RouteWarningPending {
			t.Errorf("public warning = %+v", warning)
		}
	}
}

func TestSyncCollectionsRemovesRouteStateForRemovedRepository(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/old"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRouteOperationsPending(ctx, []string{"acme/old"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/new"},
	}}); err != nil {
		t.Fatal(err)
	}
	routes, err := store.ListRouteAttempts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("removed repository route state = %+v", routes)
	}
}

func testPullRequest(number int, title string) PullRequest {
	updatedAt := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)
	return PullRequest{
		Repository:  "prometheus/node_exporter",
		Number:      number,
		Title:       title,
		Author:      "octocat",
		CreatedAt:   updatedAt.Add(-24 * time.Hour),
		UpdatedAt:   updatedAt,
		URL:         "https://github.com/prometheus/node_exporter/pull/1",
		Additions:   10,
		Deletions:   2,
		ReviewState: "none",
	}
}
