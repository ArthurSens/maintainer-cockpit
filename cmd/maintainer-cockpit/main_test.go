package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestConfigCheckCommand(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(testCommandConfig(`
collections:
  - id: exporters
    name: Exporter maintenance
    repositories: [prometheus/node_exporter]
`)), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run([]string{"config-check", "--config", path}, &stdout, &stderr); err != nil {
		t.Fatalf("run(config-check) error = %v, stderr = %q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "valid") || !strings.Contains(stdout.String(), "1 collection") {
		t.Errorf("stdout = %q, want valid collection summary", stdout.String())
	}
}

func TestConfigCheckCommandRejectsEmptyCollections(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(testCommandConfig("collections: []\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run([]string{"config-check", "--config", path}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run(config-check) error = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "collections") {
		t.Errorf("error = %q, want collections detail", err)
	}
}

func TestUnknownCommandReturnsUsageError(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run([]string{"unknown"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run(unknown) error = nil, want usage error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q, want unknown command", err)
	}
}

func TestHelpListsAvailableCommands(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run([]string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(--help) error = %v", err)
	}
	for _, command := range []string{"serve", "config-check", "setup-check", "refresh", "reanalyze", "correlate", "version"} {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("stdout = %q, want command %q", stdout.String(), command)
		}
	}
}

func TestVersionCommandReportsBuildInformation(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := newRootCommandWithBuildInformation(&stdout, &stderr, buildInformation{
		Version: "v0.0.1",
		Commit:  "0123456789abcdef",
		Date:    "2026-09-12T17:30:00Z",
	})
	command.SetArgs([]string{"version"})
	if err := command.Execute(); err != nil {
		t.Fatalf("run(version) error = %v", err)
	}
	for _, want := range []string{"v0.0.1", "0123456789abcdef", "2026-09-12T17:30:00Z"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want %q", stdout.String(), want)
		}
	}
}

func TestVersionCommandShortOutput(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := newRootCommandWithBuildInformation(&stdout, &stderr, buildInformation{
		Version: "v0.0.1",
		Commit:  "0123456789abcdef",
		Date:    "2026-09-12T17:30:00Z",
	})
	command.SetArgs([]string{"version", "--short"})
	if err := command.Execute(); err != nil {
		t.Fatalf("run(version --short) error = %v", err)
	}
	if got, want := stdout.String(), "v0.0.1\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestCorrelateCommandForcesCollectionWork(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	databasePath := filepath.Join(directory, "app.db")
	if err := os.WriteFile(configPath, []byte(testCommandConfig(`
model_providers:
  local:
    type: ollama
    base_url: http://ollama:11434
    model: qwen3
collections:
  - id: exporters
    name: Exporters
    repositories: [prometheus/node_exporter]
    model_provider: local
    feature_correlation:
      schedule: "0 4 * * *"
`)), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	correlationConfig := config.FeatureCorrelation{Schedule: "0 4 * * *"}
	if err := store.SyncCollections(t.Context(), []config.Collection{{
		ID: "exporters", Name: "Exporters",
		Repositories:  []string{"prometheus/node_exporter"},
		ModelProvider: "local", FeatureCorrelation: &correlationConfig,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{
		"correlate", "--config", configPath, "--database", databasePath,
		"--collection", "exporters",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("run(correlate) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "1 enqueued") {
		t.Errorf("stdout = %q", stdout.String())
	}
	store, err = storage.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	job, err := store.ClaimCorrelation(t.Context(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || !job.Forced {
		t.Errorf("correlation job = %+v, want forced", job)
	}
}

func TestReanalyzeCommandForcesAndCoalescesCollectionWork(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	databasePath := filepath.Join(directory, "app.db")
	if err := os.WriteFile(configPath, []byte(testCommandConfig(`
model_providers:
  local:
    type: ollama
    base_url: http://ollama:11434
    model: qwen3
collections:
  - id: exporters
    name: Exporters
    repositories: [prometheus/node_exporter]
    model_provider: local
`)), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	collection := config.Collection{
		ID: "exporters", Name: "Exporters", Repositories: []string{"prometheus/node_exporter"},
		ModelProvider: "local",
	}
	if err := store.SyncCollections(t.Context(), []config.Collection{collection}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPullRequest(t.Context(), "exporters", storage.PullRequest{
		Repository: "prometheus/node_exporter", Number: 1, Title: "Analyze",
		UpdatedAt: time.Now().UTC(), URL: "https://github.com/prometheus/node_exporter/pull/1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{
		"reanalyze", "--config", configPath, "--database", databasePath,
		"--collection", "exporters",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("run(reanalyze) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "1 coalesced") {
		t.Errorf("stdout = %q", stdout.String())
	}
	store, err = storage.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	job, err := store.ClaimAnalysis(t.Context(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || !job.Forced {
		t.Errorf("analysis job = %+v, want forced", job)
	}
}

func TestRefreshCommandDurablyEnqueuesAndCoalesces(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	databasePath := filepath.Join(directory, "app.db")
	if err := os.WriteFile(configPath, []byte(testCommandConfig(`
collections:
  - id: exporters
    name: Exporters
    repositories: [prometheus/node_exporter]
`)), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SyncCollections(t.Context(), []config.Collection{{
		ID: "exporters", Name: "Exporters", Repositories: []string{"prometheus/node_exporter"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	args := []string{"refresh", "--config", configPath, "--database", databasePath}
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("run(refresh) error = %v", err)
	}
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("run(refresh duplicate) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "1 coalesced") {
		t.Errorf("stdout = %q, want duplicate coalesced", stdout.String())
	}
	store, err = storage.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	job, err := store.ClaimRefresh(t.Context(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.Repository != "prometheus/node_exporter" || !job.Forced {
		t.Errorf("claimed job = %+v, want forced administrator refresh", job)
	}
}

func TestServeHelpHidesFixtureFlag(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run([]string{"serve", "--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(serve --help) error = %v", err)
	}
	if strings.Contains(stdout.String(), "--fixture") {
		t.Errorf("stdout = %q, fixture flag must remain hidden", stdout.String())
	}
}

func testCommandConfig(body string) string {
	return `
github:
  schedule: "0 * * * *"
  authentication:
    profiles:
      test-pat:
        type: fine_grained_pat
        token:
          environment: TEST_GITHUB_TOKEN
    owners:
      prometheus:
        profiles: [test-pat]
` + body
}
