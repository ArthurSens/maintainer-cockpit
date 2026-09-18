package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/correlation"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestPublicCorrelationGroupAPIAndPullRequestDetail(t *testing.T) {
	t.Parallel()

	configPath := writeConfig(t, testConfig("acme", `
model_providers:
  local:
    type: ollama
    base_url: http://ollama:11434
    model: qwen3
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
    model_provider: local
    feature_correlation:
      schedule: "0 4 * * *"
`))
	app, err := New(context.Background(), configPath, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	now := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)
	for number, title := range map[int]string{1: "Server support", 2: "Client types"} {
		if err := app.Store().UpsertPullRequest(t.Context(), "acme", storage.PullRequest{
			Repository: "acme/widgets", Number: number, Title: title, UpdatedAt: now,
			URL: "https://github.com/acme/widgets/pull/1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	group := correlation.Group{
		ID: "native-histograms", Name: "Native histogram support",
		Description: "Coordinates server and client support.", Confidence: "high",
		Members: []correlation.Member{
			{SourceID: "PR_acme_widgets_1", Repository: "acme/widgets", Number: 1, Title: "Server support", State: "open"},
			{SourceID: "PR_acme_widgets_2", Repository: "acme/widgets", Number: 2, Title: "Client types", State: "open"},
		},
		Edges: []correlation.Edge{{
			From: "PR_acme_widgets_1", To: "PR_acme_widgets_2",
			Type: correlation.EdgeStackedOnTopOf, Confidence: "high",
			Reason: "Server support builds on client types.",
		}},
	}
	if err := app.Store().ReplaceCorrelationGroups(
		t.Context(), "acme", []correlation.Group{group}, storage.CorrelationProvenance{
			Provider: "local", Model: "qwen3", SchemaVersion: correlation.SchemaVersion,
			PromptVersion: correlation.PromptVersion, CorrelatedAt: now,
		},
	); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/api/collections/acme/groups")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var list struct {
		Groups []storage.CorrelationGroupSummary `json:"groups"`
	}
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(list.Groups) != 1 ||
		list.Groups[0].OpenMemberCount != 2 || list.Groups[0].Confidence != "high" ||
		list.Groups[0].Provider != "local" || list.Groups[0].Model != "qwen3" ||
		!list.Groups[0].CorrelatedAt.Equal(now) {
		t.Errorf("status = %d, groups = %+v", response.StatusCode, list.Groups)
	}

	detailResponse, err := http.Get(
		server.URL + "/api/collections/acme/pull-requests/acme/widgets/1",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer detailResponse.Body.Close()
	var detail struct {
		Groups []storage.CorrelationGroupSummary `json:"groups"`
	}
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Groups) != 1 || detail.Groups[0].Name != group.Name ||
		detail.Groups[0].Confidence != "high" || detail.Groups[0].Provider != "local" {
		t.Errorf("detail groups = %+v", detail.Groups)
	}
}

func TestPublicCorrelationAPIExposesLastFailureWithoutRemovingGroups(t *testing.T) {
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
	if err := app.Store().RecordCorrelationFailure(
		t.Context(), storage.CorrelationJob{CollectionID: "acme"}, "local", "qwen3",
		nil, nil,
		time.Now().UTC(), "Model provider unavailable.",
	); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/collections/acme/groups", http.NoBody)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !json.Valid(response.Body.Bytes()) {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	if got := response.Body.String(); !containsAll(got, `"state":"failed"`, `"groups":[]`) {
		t.Errorf("response = %s", got)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !contains(value, part) {
			return false
		}
	}
	return true
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}
