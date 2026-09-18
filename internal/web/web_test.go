package web

import (
	"io/fs"
	"strings"
	"testing"
)

func TestFilesIncludesEveryBrowserModule(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"app.js", "filters.js", "groups.js", "hidden.js", "important.js", "router.js",
		"row-action.js", "sorting.js", "table-columns.js", "tooltip.js", "view-model.js",
	} {
		if _, err := fs.ReadFile(Files(), name); err != nil {
			t.Errorf("ReadFile(%q) error = %v", name, err)
		}
	}
}

func TestAuthorizedPullRequestRowsOfferCompactHiddenStateAction(t *testing.T) {
	t.Parallel()

	app, err := fs.ReadFile(Files(), "app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`class: "hide-row-button"`,
		`text: "🙈"`,
		`value: "forever"`,
		`value: "activity"`,
		`value: "period"`,
		`name: "customUntil"`,
		`type: "datetime-local"`,
		`text: "Additional notes (optional)"`,
		`"Hidden until", "Notes", "Action"`,
		`text: "Restore"`,
		`openHiddenStateDialog`,
		`hiddenTableFor`,
	} {
		if !strings.Contains(string(app), want) {
			t.Errorf("browser row actions do not contain %q", want)
		}
	}
}

func TestSearchOffersGuidanceUntilFocused(t *testing.T) {
	t.Parallel()

	app, err := fs.ReadFile(Files(), "app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		string(app),
		`placeholder: "Search title, author, repository, or PR number"`,
	) {
		t.Error("search input does not explain its supported fields")
	}

	styles, err := fs.ReadFile(Files(), "styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(styles), `.list-controls input:focus::placeholder`) {
		t.Error("search guidance does not disappear on focus")
	}
}

func TestBrowserOffersSessionControlsAndDecisionBrief(t *testing.T) {
	t.Parallel()

	index, err := fs.ReadFile(Files(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(Files(), "app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`id="viewer-controls"`,
		`loadJSON("/api/viewer")`,
		`href: "/auth/login"`,
		`class: "auth-button"`,
		`class: "account-menu"`,
		`class: "account-avatar"`,
		`viewer.avatarURL`,
		`fetch("/auth/logout"`,
		`fetch("/api/me/data"`,
		`presentDecisionBrief(detail)`,
		`text: "Readiness"`,
		`text: "People"`,
		`class: "drawer-evidence"`,
		`text: "Modify table"`,
	} {
		if !strings.Contains(string(index)+"\n"+string(app), want) {
			t.Errorf("browser assets do not contain %q", want)
		}
	}

	styles, err := fs.ReadFile(Files(), "styles.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`.account-popover`, `.account-menu-item`, `.decision-risk`, `.readiness-grid`,
		`.drawer-evidence`, `.auth-button`,
	} {
		if !strings.Contains(string(styles), want) {
			t.Errorf("browser styles do not contain %q", want)
		}
	}
}

func TestAdminPageOffersConfirmedManualRefresh(t *testing.T) {
	t.Parallel()

	app, err := fs.ReadFile(Files(), "app.js")
	if err != nil {
		t.Fatal(err)
	}
	styles, err := fs.ReadFile(Files(), "styles.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`text: "Refresh data"`,
		`text: "Refresh all repositories?"`,
		`text: "Request refresh"`,
		`mutateJSON("/api/admin/refresh"`,
		`class: "admin-refresh-dialog"`,
	} {
		if !strings.Contains(string(app), want) {
			t.Errorf("admin refresh controls do not contain %q", want)
		}
	}
	for _, want := range []string{`.admin-heading`, `.admin-refresh-dialog`} {
		if !strings.Contains(string(styles), want) {
			t.Errorf("admin refresh styles do not contain %q", want)
		}
	}
}

func TestCollectionNavigationUsesCollapsibleSidebarAndHeaderProgress(t *testing.T) {
	t.Parallel()

	index, err := fs.ReadFile(Files(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(Files(), "app.js")
	if err != nil {
		t.Fatal(err)
	}
	styles, err := fs.ReadFile(Files(), "styles.css")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`id="context-controls"`,
		`class: "collection-sidebar"`,
		`class: "sidebar-toggle"`,
		`aria-label": "Collection views"`,
		`icon: "🕸️"`,
		`icon: "⭐"`,
		`icon: "🙈"`,
		`label: "Snoozed / Ignored"`,
		"class: `today-progress",
	} {
		if !strings.Contains(string(index)+"\n"+string(app), want) {
			t.Errorf("browser navigation does not contain %q", want)
		}
	}
	for _, want := range []string{
		`.collection-layout`, `.collection-sidebar`, `.sidebar-collapsed`, `.today-progress`,
	} {
		if !strings.Contains(string(styles), want) {
			t.Errorf("browser styles do not contain %q", want)
		}
	}
	if strings.Contains(string(app), `text: "All collections"`) {
		t.Error("redundant All collections link is still rendered")
	}
}

func TestApplicationLayoutUsesAvailableViewportWidth(t *testing.T) {
	t.Parallel()

	styles, err := fs.ReadFile(Files(), "styles.css")
	if err != nil {
		t.Fatal(err)
	}
	const want = `#app {
  width: 100%;
  max-width: none;
  margin: 0;
  padding: clamp(16px, 2vw, 32px);
}`
	if !strings.Contains(string(styles), want) {
		t.Error("application layout does not use the full available viewport width")
	}
}

func TestBrowserOmitsGeneratedSummaryFeature(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"app.js", "table-columns.js", "view-model.js", "styles.css"} {
		body, err := fs.ReadFile(Files(), name)
		if err != nil {
			t.Fatal(err)
		}
		for _, removed := range []string{"presentSummary", "summary-cell", "summary-heading", "proposedChange"} {
			if strings.Contains(string(body), removed) {
				t.Errorf("%s still contains removed generated-summary marker %q", name, removed)
			}
		}
	}
}

func TestOptionalFiltersUseAddFilterMenu(t *testing.T) {
	t.Parallel()

	app, err := fs.ReadFile(Files(), "app.js")
	if err != nil {
		t.Fatal(err)
	}
	styles, err := fs.ReadFile(Files(), "styles.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`class: "add-filter"`,
		`text: "Add filter"`,
		`class: "active-filters"`,
		`class: "remove-filter"`,
	} {
		if !strings.Contains(string(app), want) {
			t.Errorf("filter toolbar does not contain %q", want)
		}
	}
	for _, want := range []string{
		".add-filter", ".active-filters", ".filter-control", ".remove-filter",
	} {
		if !strings.Contains(string(styles), want) {
			t.Errorf("filter toolbar styles do not contain %q", want)
		}
	}
}

func TestHeaderOffersCollectionSwitcher(t *testing.T) {
	t.Parallel()

	index, err := fs.ReadFile(Files(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(Files(), "app.js")
	if err != nil {
		t.Fatal(err)
	}
	styles, err := fs.ReadFile(Files(), "styles.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`id="collection-controls"`,
		`class: "collection-switcher"`,
		`placeholder: "Search collections"`,
		`aria-label": "Switch collection"`,
		`class: "collection-summary"`,
	} {
		if !strings.Contains(string(index)+"\n"+string(app), want) {
			t.Errorf("collection switcher does not contain %q", want)
		}
	}
	for _, want := range []string{
		".collection-switcher", ".collection-switcher-popover", ".collection-switcher-list",
	} {
		if !strings.Contains(string(styles), want) {
			t.Errorf("collection switcher styles do not contain %q", want)
		}
	}
}
