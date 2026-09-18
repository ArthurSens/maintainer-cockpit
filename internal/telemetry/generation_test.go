package telemetry

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestGeneratedTelemetryArtifactsMatchRegistry(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("../../telemetry/registry/metrics.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Metrics []struct {
			Name string `yaml:"name"`
		} `yaml:"metrics"`
		MetricRefinements []struct {
			Ref string `yaml:"ref"`
		} `yaml:"metric_refinements"`
	}
	if err := yaml.Unmarshal(body, &registry); err != nil {
		t.Fatal(err)
	}
	generatedNames, err := os.ReadFile("metric_names_generated.go")
	if err != nil {
		t.Fatal(err)
	}
	generatedDocs, err := os.ReadFile("../../docs/generated-telemetry.md")
	if err != nil {
		t.Fatal(err)
	}
	metricNames := make([]string, 0, len(registry.Metrics)+len(registry.MetricRefinements))
	for _, metric := range registry.Metrics {
		metricNames = append(metricNames, metric.Name)
	}
	for _, refinement := range registry.MetricRefinements {
		metricNames = append(metricNames, refinement.Ref)
	}
	if len(metricNames) == 0 {
		t.Fatal("registry has no metrics")
	}
	for _, name := range metricNames {
		if name == "" {
			t.Fatal("registry metric has no name")
		}
		if !strings.Contains(string(generatedNames), `"`+name+`"`) {
			t.Errorf("generated Go is missing %q", name)
		}
		// Route-attempt documentation is intentionally deferred with the
		// example dashboard while the runtime and UX land first.
		if name != "maintainer_cockpit.github.route.attempts" &&
			!strings.Contains(string(generatedDocs), "`"+name+"`") {
			t.Errorf("generated documentation is missing %q", name)
		}
	}
}

func TestPinnedGenAISemanticConventionRevision(t *testing.T) {
	t.Parallel()

	if GenAISemanticConventions != "v1.44.0" {
		t.Errorf("GenAISemanticConventions = %q, want v1.44.0", GenAISemanticConventions)
	}
}

func TestRegistryPinsUpstreamGenAIConventions(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("../../telemetry/registry/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Dependencies []struct {
			SchemaURL    string `yaml:"schema_url"`
			RegistryPath string `yaml:"registry_path"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	const (
		wantSchema = "https://opentelemetry.io/schemas/gen-ai-dev/1.42.0-dev"
		wantCommit = "0c87594975195608dc91b3f702e250a7b240c151"
	)
	for _, dependency := range manifest.Dependencies {
		if dependency.SchemaURL == wantSchema &&
			strings.Contains(dependency.RegistryPath, wantCommit) {
			return
		}
	}
	t.Errorf("manifest dependencies = %+v, want pinned GenAI registry %s", manifest.Dependencies, wantCommit)
}

func TestRegistryImportsGenAIContractsInsteadOfRedefiningThem(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("../../telemetry/registry/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var attributes struct {
		Attributes []struct {
			Key string `yaml:"key"`
		} `yaml:"attributes"`
	}
	if err := yaml.Unmarshal(body, &attributes); err != nil {
		t.Fatal(err)
	}
	for _, attribute := range attributes.Attributes {
		if !strings.HasPrefix(attribute.Key, "maintainer_cockpit.") {
			t.Errorf("locally redefined upstream attribute %q", attribute.Key)
		}
	}

	assertRegistryImport(t, "../../telemetry/registry/metrics.yaml",
		"metrics", "gen_ai.client.operation.duration", "gen_ai.client.token.usage")
	assertRegistryImport(t, "../../telemetry/registry/spans.yaml",
		"spans", "gen_ai.inference.client")
}

func assertRegistryImport(t *testing.T, path, signal string, want ...string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Imports map[string][]string `yaml:"imports"`
	}
	if err := yaml.Unmarshal(body, &registry); err != nil {
		t.Fatal(err)
	}
	got := registry.Imports[signal]
	for _, name := range want {
		if !contains(got, name) {
			t.Errorf("%s imports = %v, want %q", path, got, name)
		}
	}
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}

func TestGeneratedGenAIDashboardMatchesTelemetryContracts(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(
		"../../examples/otel-lgtm/grafana/dashboards/maintainer-cockpit-genai.json",
	)
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Title  string `json:"title"`
		Panels []any  `json:"panels"`
	}
	if err := json.Unmarshal(body, &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.Title != "Maintainer Cockpit — GenAI Operations" {
		t.Errorf("dashboard title = %q", dashboard.Title)
	}
	if len(dashboard.Panels) < 18 {
		t.Errorf("dashboard panels = %d, want at least 18", len(dashboard.Panels))
	}
	for _, metric := range []string{
		"gen_ai_client_operation_duration_seconds",
		"gen_ai_client_token_usage",
		"maintainer_cockpit_gen_ai_token_details",
		"maintainer_cockpit_gen_ai_content_size",
		"maintainer_cockpit_gen_ai_usage_reports",
		"maintainer_cockpit_analysis_attempts",
		"maintainer_cockpit_queue_depth",
	} {
		if !strings.Contains(string(body), metric) {
			t.Errorf("generated dashboard is missing %q", metric)
		}
	}
}

func TestLGTMExampleProvisionsGeneratedDashboard(t *testing.T) {
	t.Parallel()

	compose, err := os.ReadFile("../../compose.otel-lgtm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"./examples/otel-lgtm/grafana/provisioning/dashboards.yaml",
		"./examples/otel-lgtm/grafana/dashboards",
	} {
		if !strings.Contains(string(compose), path) {
			t.Errorf("LGTM Compose configuration is missing mount %q", path)
		}
	}

	provisioning, err := os.ReadFile(
		"../../examples/otel-lgtm/grafana/provisioning/dashboards.yaml",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"folder: Maintainer Cockpit",
		"path: /var/lib/grafana/dashboards/maintainer-cockpit",
	} {
		if !strings.Contains(string(provisioning), value) {
			t.Errorf("dashboard provisioning is missing %q", value)
		}
	}
}
