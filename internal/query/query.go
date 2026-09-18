// Package query validates public pull-request table queries and cursors.
package query

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Options is one validated server-side list query.
type Options struct {
	View           string
	Query          string
	Repository     string
	Review         string
	Quality        string
	ReviewLoad     string
	AnalysisStatus string
	WaitingOn      []string
	WaitingMode    string
	Sort           string
	Order          string
	Limit          int
	Offset         int
}

type cursor struct {
	Version        int    `json:"v"`
	View           string `json:"x,omitempty"`
	Query          string `json:"q,omitempty"`
	Repository     string `json:"r,omitempty"`
	Review         string `json:"w,omitempty"`
	Quality        string `json:"y,omitempty"`
	ReviewLoad     string `json:"c,omitempty"`
	AnalysisStatus string `json:"h,omitempty"`
	WaitingOn      string `json:"a,omitempty"`
	WaitingMode    string `json:"m,omitempty"`
	Sort           string `json:"s"`
	Order          string `json:"o"`
	Limit          int    `json:"l"`
	Offset         int    `json:"p"`
}

var allowedSorts = map[string]struct{}{
	"updated": {}, "number": {}, "repository": {}, "title": {}, "author": {}, "churn": {}, "review": {}, "quality": {}, "review_load": {},
}

var allowedReviews = map[string]struct{}{
	"none": {}, "review_requested": {}, "changes_requested": {}, "approved": {}, "draft": {}, "reviewed": {},
}

var allowedQualities = map[string]struct{}{
	"no_concerns": {}, "review_suggested": {}, "strong_concerns": {},
}

var allowedReviewLoads = map[string]struct{}{
	"low": {}, "medium": {}, "high": {},
}

var allowedAnalysisStatuses = map[string]struct{}{
	"available": {}, "pending": {}, "stale": {}, "partial": {}, "failed": {}, "invalid": {},
}

var allowedWaitingParties = map[string]struct{}{
	"triager": {}, "maintainer": {}, "author": {}, "external_dependency": {},
}

// Parse validates URL query parameters and an optional query-bound cursor.
func Parse(values url.Values) (Options, error) {
	options := Options{
		View:  values.Get("view"),
		Query: strings.TrimSpace(values.Get("q")), Repository: values.Get("repo"),
		Review: values.Get("review"), Quality: values.Get("quality"),
		ReviewLoad: values.Get("review_load"), AnalysisStatus: values.Get("analysis_status"),
		WaitingMode: values.Get("waiting_mode"),
		Sort:        values.Get("sort"), Order: values.Get("order"),
		Limit: 50,
	}
	if len(options.Query) > 200 {
		return Options{}, errors.New("q must not exceed 200 characters")
	}
	if options.View != "" && options.View != "mine" && options.View != "hidden" {
		return Options{}, fmt.Errorf("unsupported view %q", options.View)
	}
	if options.Sort == "" {
		options.Sort = "updated"
	}
	if options.Sort == "none" {
		if options.Order != "" {
			return Options{}, errors.New("order must be empty when sort is none")
		}
		options.Sort = ""
	} else {
		sorts := strings.Split(options.Sort, ",")
		if options.Order == "" {
			if len(sorts) != 1 || sorts[0] != "updated" {
				return Options{}, errors.New("order is required for custom sorting")
			}
			options.Order = "desc"
		}
		orders := strings.Split(options.Order, ",")
		if len(sorts) != len(orders) {
			return Options{}, errors.New("sort and order must contain the same number of values")
		}
		if len(sorts) > len(allowedSorts) {
			return Options{}, errors.New("too many sort columns")
		}
		seen := make(map[string]struct{}, len(sorts))
		for index, sort := range sorts {
			if _, exists := allowedSorts[sort]; !exists {
				return Options{}, fmt.Errorf("unsupported sort %q", sort)
			}
			if _, exists := seen[sort]; exists {
				return Options{}, fmt.Errorf("duplicate sort %q", sort)
			}
			seen[sort] = struct{}{}
			if orders[index] != "asc" && orders[index] != "desc" {
				return Options{}, errors.New("order must be asc or desc")
			}
		}
	}
	if options.Review != "" {
		if _, exists := allowedReviews[options.Review]; !exists {
			return Options{}, fmt.Errorf("unsupported review state %q", options.Review)
		}
	}
	if options.Quality != "" {
		if _, exists := allowedQualities[options.Quality]; !exists {
			return Options{}, fmt.Errorf("unsupported quality level %q", options.Quality)
		}
	}
	if options.ReviewLoad != "" {
		if _, exists := allowedReviewLoads[options.ReviewLoad]; !exists {
			return Options{}, fmt.Errorf("unsupported review load %q", options.ReviewLoad)
		}
	}
	if options.AnalysisStatus != "" {
		if _, exists := allowedAnalysisStatuses[options.AnalysisStatus]; !exists {
			return Options{}, fmt.Errorf("unsupported analysis status %q", options.AnalysisStatus)
		}
	}
	if rawWaiting := values.Get("waiting"); rawWaiting != "" {
		seen := make(map[string]struct{})
		for party := range strings.SplitSeq(rawWaiting, ",") {
			if _, exists := allowedWaitingParties[party]; !exists {
				return Options{}, fmt.Errorf("unsupported waiting-on party %q", party)
			}
			if _, exists := seen[party]; exists {
				return Options{}, fmt.Errorf("duplicate waiting-on party %q", party)
			}
			seen[party] = struct{}{}
			options.WaitingOn = append(options.WaitingOn, party)
		}
		sort.Strings(options.WaitingOn)
	}
	if len(options.WaitingOn) > 0 && options.WaitingMode == "" {
		options.WaitingMode = "any"
	}
	if options.WaitingMode != "" && options.WaitingMode != "any" && options.WaitingMode != "all" {
		return Options{}, errors.New("waiting_mode must be any or all")
	}
	if len(options.WaitingOn) == 0 && values.Get("waiting_mode") != "" {
		return Options{}, errors.New("waiting_mode requires waiting")
	}
	if rawLimit := values.Get("limit"); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > 100 {
			return Options{}, errors.New("limit must be between 1 and 100")
		}
		options.Limit = limit
	}
	if rawCursor := values.Get("cursor"); rawCursor != "" {
		decoded, err := decodeCursor(rawCursor)
		if err != nil {
			return Options{}, err
		}
		expected := cursorFromOptions(options, decoded.Offset)
		if decoded != expected {
			return Options{}, errors.New("cursor does not match the current filters and sorting")
		}
		options.Offset = decoded.Offset
	}
	return options, nil
}

// NextCursor returns an opaque cursor anchored at offset.
func (options Options) NextCursor(offset int) (string, error) {
	if offset < 0 {
		return "", errors.New("cursor offset must not be negative")
	}
	body, err := json.Marshal(cursorFromOptions(options, offset))
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

func cursorFromOptions(options Options, offset int) cursor {
	return cursor{
		Version: 1, View: options.View,
		Query: options.Query, Repository: options.Repository,
		Review: options.Review, Quality: options.Quality,
		ReviewLoad: options.ReviewLoad, AnalysisStatus: options.AnalysisStatus,
		WaitingOn: strings.Join(options.WaitingOn, ","), WaitingMode: options.WaitingMode,
		Sort: options.Sort, Order: options.Order,
		Limit: options.Limit, Offset: offset,
	}
}

func decodeCursor(raw string) (cursor, error) {
	if len(raw) > 2048 {
		return cursor{}, errors.New("cursor is too large")
	}
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor{}, errors.New("invalid cursor encoding")
	}
	var decoded cursor
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return cursor{}, errors.New("invalid cursor")
	}
	if decoded.Version != 1 || decoded.Offset < 0 || decoded.Offset > 10_000_000 {
		return cursor{}, errors.New("unsupported cursor")
	}
	return decoded, nil
}
