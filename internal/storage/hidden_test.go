package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	listquery "github.com/ArthurSens/maintainer-cockpit/internal/query"
)

func TestListPersonalPullRequestsPageEnrichesOnlyReturnedPage(t *testing.T) {
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
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	pullRequests := make([]PullRequest, 101)
	for index := range pullRequests {
		number := index + 1
		pullRequests[index] = PullRequest{
			Repository:  repository,
			Number:      number,
			Title:       fmt.Sprintf("Pull request %03d", number),
			Author:      fmt.Sprintf("author-%03d", number),
			AuthorID:    int64(number),
			CreatedAt:   now.Add(-time.Hour),
			UpdatedAt:   now.Add(-time.Duration(index) * time.Minute),
			URL:         fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
			ReviewState: "none",
		}
	}
	pullRequests[100].AuthorHistory = &AuthorHistory{
		ID: 101, Login: "author-101", CollectedAt: now,
	}
	if err := store.ReplaceRepositorySnapshot(ctx, repository, pullRequests, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(
		ctx, "UPDATE authors SET recent_activity_json = 'invalid' WHERE id = 101",
	); err != nil {
		t.Fatal(err)
	}

	page, err := store.ListPersonalPullRequestsPage(ctx, "acme", 42, listquery.Options{
		Sort: "updated", Order: "desc", Limit: 10,
	}, now)
	if err != nil {
		t.Fatalf("ListPersonalPullRequestsPage() error = %v", err)
	}
	if page.Total != 101 || page.Matched != 101 || len(page.PullRequests) != 10 ||
		page.PullRequests[0].Number != 1 || page.NextCursor == "" {
		t.Fatalf("page = %+v, want first 10 of 101", page)
	}
}

func TestListPersonalPullRequestsPageFiltersBeforePagination(t *testing.T) {
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
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	pullRequests := make([]PullRequest, 6)
	for index := range pullRequests {
		number := index + 1
		authorID := int64(100 + number)
		if number <= 2 {
			authorID = 42
		}
		pullRequests[index] = PullRequest{
			Repository: repository, Number: number,
			Title:  fmt.Sprintf("Pull request %d", number),
			Author: fmt.Sprintf("author-%d", authorID), AuthorID: authorID,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Duration(index) * time.Minute),
			URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number), ReviewState: "none",
		}
	}
	if err := store.ReplaceRepositorySnapshot(ctx, repository, pullRequests, now); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPullRequestImportant(ctx, 42, "acme", repository, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetPullRequestHidden(ctx, 42, "acme", repository, 1, HiddenPullRequestState{
		Kind: "ignored", Reason: "not actionable",
	}, now); err != nil {
		t.Fatal(err)
	}

	defaultPage, err := store.ListPersonalPullRequestsPage(ctx, "acme", 42, listquery.Options{
		Sort: "updated", Order: "desc", Limit: 2,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if defaultPage.Total != 5 || defaultPage.Matched != 5 ||
		len(defaultPage.PullRequests) != 2 || defaultPage.PullRequests[0].Number != 2 ||
		defaultPage.NextCursor == "" {
		t.Errorf("default page = %+v", defaultPage)
	}

	minePage, err := store.ListPersonalPullRequestsPage(ctx, "acme", 42, listquery.Options{
		View: "mine", Sort: "updated", Order: "desc", Limit: 10,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if minePage.Total != 2 || minePage.Matched != 2 ||
		len(minePage.PullRequests) != 2 ||
		minePage.PullRequests[0].Number != 2 || minePage.PullRequests[1].Number != 3 ||
		minePage.PullRequests[1].Personal == nil || !minePage.PullRequests[1].Personal.Important {
		t.Errorf("mine page = %+v", minePage)
	}

	hiddenPage, err := store.ListPersonalPullRequestsPage(ctx, "acme", 42, listquery.Options{
		View: "hidden", Sort: "updated", Order: "desc", Limit: 10,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if hiddenPage.Total != 1 || hiddenPage.Matched != 1 ||
		len(hiddenPage.PullRequests) != 1 || hiddenPage.PullRequests[0].Number != 1 ||
		hiddenPage.PullRequests[0].Personal == nil ||
		hiddenPage.PullRequests[0].Personal.Hidden == nil {
		t.Errorf("hidden page = %+v", hiddenPage)
	}
}
