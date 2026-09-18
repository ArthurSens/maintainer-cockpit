package query

import (
	"net/url"
	"testing"
)

func TestParseDefaultsAndValidatesListQuery(t *testing.T) {
	t.Parallel()

	got, err := Parse(url.Values{})
	if err != nil {
		t.Fatalf("Parse(defaults) error = %v", err)
	}
	if got.Sort != "updated" || got.Order != "desc" || got.Limit != 50 || got.Offset != 0 {
		t.Errorf("defaults = %+v", got)
	}

	_, err = Parse(url.Values{"limit": {"101"}})
	if err == nil {
		t.Fatal("Parse(limit=101) error = nil")
	}
	_, err = Parse(url.Values{"sort": {"unsupported"}})
	if err == nil {
		t.Fatal("Parse(unsupported sort) error = nil")
	}
	got, err = Parse(url.Values{
		"sort": {"author,updated"}, "order": {"asc,desc"},
	})
	if err != nil {
		t.Fatalf("Parse(multi-sort) error = %v", err)
	}
	if got.Sort != "author,updated" || got.Order != "asc,desc" {
		t.Errorf("multi-sort = %+v", got)
	}
	for name, values := range map[string]url.Values{
		"mismatched directions": {"sort": {"author,updated"}, "order": {"asc"}},
		"duplicate column":      {"sort": {"author,author"}, "order": {"asc,desc"}},
	} {
		if _, err := Parse(values); err == nil {
			t.Errorf("Parse(%s) error = nil", name)
		}
	}
}

func TestParseValidatesQualityFilterAndSort(t *testing.T) {
	t.Parallel()

	got, err := Parse(url.Values{
		"quality": {"review_suggested"}, "sort": {"quality"}, "order": {"desc"},
	})
	if err != nil {
		t.Fatalf("Parse(quality) error = %v", err)
	}
	if got.Quality != "review_suggested" || got.Sort != "quality" {
		t.Errorf("quality query = %+v", got)
	}
	for _, quality := range []string{"unsupported", "9", "no concerns"} {
		if _, err := Parse(url.Values{"quality": {quality}}); err == nil {
			t.Errorf("Parse(quality=%q) error = nil, want validation error", quality)
		}
	}
}

func TestParseValidatesReviewLoadAnalysisStatusAndWaitingOnSet(t *testing.T) {
	t.Parallel()

	got, err := Parse(url.Values{
		"review_load":     {"medium"},
		"analysis_status": {"partial"},
		"waiting":         {"maintainer,author"},
		"waiting_mode":    {"all"},
		"sort":            {"review_load"},
		"order":           {"desc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewLoad != "medium" || got.AnalysisStatus != "partial" ||
		got.WaitingMode != "all" ||
		len(got.WaitingOn) != 2 || got.WaitingOn[0] != "author" {
		t.Errorf("analysis filters = %+v", got)
	}
	for _, status := range []string{
		"available", "pending", "stale", "partial", "failed", "invalid",
	} {
		if _, err := Parse(url.Values{"analysis_status": {status}}); err != nil {
			t.Errorf("Parse(analysis_status=%q) error = %v", status, err)
		}
	}
	for name, values := range map[string]url.Values{
		"unknown review load":     {"review_load": {"unknown"}},
		"invalid review load":     {"review_load": {"critical"}},
		"invalid analysis status": {"analysis_status": {"complete"}},
		"unknown waiting party":   {"waiting": {"bot"}},
		"duplicate waiting party": {"waiting": {"author,author"}},
		"mode without parties":    {"waiting_mode": {"all"}},
	} {
		if _, err := Parse(values); err == nil {
			t.Errorf("Parse(%s) error = nil", name)
		}
	}
}

func TestCursorIsBoundToQualityFilter(t *testing.T) {
	t.Parallel()

	first := Options{Quality: "strong_concerns", Sort: "updated", Order: "desc", Limit: 50}
	cursor, err := first.NextCursor(50)
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{"quality": {"strong_concerns"}, "cursor": {cursor}}
	got, err := Parse(values)
	if err != nil {
		t.Fatalf("Parse(quality cursor) error = %v", err)
	}
	if got.Offset != 50 {
		t.Errorf("Offset = %d, want 50", got.Offset)
	}
	values.Set("quality", "no_concerns")
	if _, err := Parse(values); err == nil {
		t.Fatal("Parse(mismatched quality cursor) error = nil")
	}
}

func TestCursorIsBoundToFiltersAndSorting(t *testing.T) {
	t.Parallel()

	first := Options{
		Query: "metrics", Repository: "prometheus/prometheus", Review: "approved",
		Sort: "title,updated", Order: "asc,desc", Limit: 25,
	}
	cursor, err := first.NextCursor(25)
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{
		"q": {"metrics"}, "repo": {"prometheus/prometheus"}, "review": {"approved"},
		"sort": {"title,updated"}, "order": {"asc,desc"}, "limit": {"25"}, "cursor": {cursor},
	}
	got, err := Parse(values)
	if err != nil {
		t.Fatalf("Parse(cursor) error = %v", err)
	}
	if got.Offset != 25 {
		t.Errorf("Offset = %d, want 25", got.Offset)
	}
	values.Set("q", "different")
	if _, err := Parse(values); err == nil {
		t.Fatal("Parse(mismatched cursor query) error = nil")
	}
}

func TestCursorIsBoundToPrivateView(t *testing.T) {
	t.Parallel()

	for _, view := range []string{"mine", "hidden"} {
		got, err := Parse(url.Values{"view": {view}})
		if err != nil || got.View != view {
			t.Errorf("Parse(view=%q) = %+v, %v", view, got, err)
		}
	}
	first := Options{View: "mine", Sort: "updated", Order: "desc", Limit: 50}
	cursor, err := first.NextCursor(50)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(url.Values{"view": {"mine"}, "cursor": {cursor}})
	if err != nil {
		t.Fatalf("Parse(Mine cursor) error = %v", err)
	}
	if got.View != "mine" || got.Offset != 50 {
		t.Errorf("Mine cursor = %+v", got)
	}
	if _, err := Parse(url.Values{"cursor": {cursor}}); err == nil {
		t.Fatal("Mine cursor was accepted for the public collection view")
	}
	if _, err := Parse(url.Values{"view": {"unsupported"}}); err == nil {
		t.Fatal("unsupported view was accepted")
	}
}
