package telemetry

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.yaml.in/yaml/v3"
)

func TestGitHubRouteAttemptTelemetryUsesOnlyBoundedAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	original := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(original)
		instrumentsReady.Store(false)
		_ = provider.Shutdown(context.Background())
	})

	var err error
	githubRouteAttempts, err = provider.Meter(InstrumentationName).
		Int64Counter(GitHubRouteAttemptsMetricName)
	if err != nil {
		t.Fatal(err)
	}
	instrumentsReady.Store(true)
	RecordGitHubRouteAttempt(
		context.Background(), "discovery", "github_app", "advanced", "permission_missing",
	)
	RecordGitHubRouteAttempt(
		context.Background(), "contribution_evidence", "fine_grained_pat", "selected", "",
	)

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	points := routeAttemptPoints(t, metrics)
	if len(points) != 2 {
		t.Fatalf("route attempt points = %d, want 2", len(points))
	}
	want := map[string]map[string]string{
		"discovery": {
			"maintainer_cockpit.github.route.operation":  "discovery",
			"maintainer_cockpit.github.profile.type":     "github_app",
			"maintainer_cockpit.github.route.outcome":    "advanced",
			"maintainer_cockpit.github.failure.category": "permission_missing",
		},
		"contribution_evidence": {
			"maintainer_cockpit.github.route.operation":  "contribution_evidence",
			"maintainer_cockpit.github.profile.type":     "fine_grained_pat",
			"maintainer_cockpit.github.route.outcome":    "selected",
			"maintainer_cockpit.github.failure.category": "none",
		},
	}
	for _, point := range points {
		attributes := attribute.NewSet(point.Attributes.ToSlice()...)
		operation, ok := attributes.Value(
			attribute.Key("maintainer_cockpit.github.route.operation"),
		)
		if !ok {
			t.Fatal("route metric is missing operation attribute")
		}
		expected, ok := want[operation.AsString()]
		if !ok {
			t.Fatalf("unexpected route operation %q", operation.AsString())
		}
		for key, value := range expected {
			assertStringAttribute(t, attributes, key, value)
		}
		for _, forbidden := range []string{
			"maintainer_cockpit.repository", "maintainer_cockpit.owner",
			"maintainer_cockpit.github.profile.name", "maintainer_cockpit.github.installation.id",
			"maintainer_cockpit.github.app.id", "maintainer_cockpit.github.account.id",
		} {
			if _, ok := attributes.Value(attribute.Key(forbidden)); ok {
				t.Errorf("route metric includes forbidden identity attribute %q", forbidden)
			}
		}
	}
}

func TestGitHubRouteRegistryForbidsIdentityDimensions(t *testing.T) {
	body, err := os.ReadFile("../../telemetry/registry/metrics.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Metrics []struct {
			Name       string `yaml:"name"`
			Attributes []struct {
				Ref string `yaml:"ref"`
			} `yaml:"attributes"`
		} `yaml:"metrics"`
	}
	if err := yaml.Unmarshal(body, &registry); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, metric := range registry.Metrics {
		if metric.Name != "maintainer_cockpit.github.route.attempts" {
			continue
		}
		found = true
		var joined strings.Builder
		for _, item := range metric.Attributes {
			joined.WriteString(item.Ref)
			joined.WriteByte('\n')
		}
		for _, forbidden := range []string{
			"repository", "owner", "profile.name", "installation", "app.id", "account",
		} {
			if strings.Contains(joined.String(), forbidden) {
				t.Errorf("route attempt metric attributes include forbidden identity %q: %s", forbidden, joined.String())
			}
		}
	}
	if !found {
		t.Fatal("route attempt metric is not registered")
	}

	body, err = os.ReadFile("../../telemetry/registry/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var attributes struct {
		Attributes []struct {
			Key  string `yaml:"key"`
			Type any    `yaml:"type"`
		} `yaml:"attributes"`
	}
	if err := yaml.Unmarshal(body, &attributes); err != nil {
		t.Fatal(err)
	}
	wantBounded := map[string]bool{
		"maintainer_cockpit.github.route.operation":  false,
		"maintainer_cockpit.github.profile.type":     false,
		"maintainer_cockpit.github.route.outcome":    false,
		"maintainer_cockpit.github.failure.category": false,
	}
	for _, item := range attributes.Attributes {
		if _, ok := wantBounded[item.Key]; !ok {
			continue
		}
		kind, ok := item.Type.(map[string]any)
		if !ok {
			continue
		}
		if members, ok := kind["members"].([]any); ok && len(members) > 0 {
			wantBounded[item.Key] = true
		}
	}
	for key, bounded := range wantBounded {
		if !bounded {
			t.Errorf("registry attribute %q is missing a bounded member set", key)
		}
	}
}

func TestPersistedRouteAttemptCallSitesRecordTelemetry(t *testing.T) {
	for _, path := range []string{"../refresh/scheduler.go", "../evidenceworker/worker.go"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "telemetry.RecordGitHubRouteAttempt(") {
			t.Errorf("%s does not record persisted routed attempts", path)
		}
	}
}

func routeAttemptPoints(
	t *testing.T, metrics metricdata.ResourceMetrics,
) []metricdata.DataPoint[int64] {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != GitHubRouteAttemptsMetricName {
				continue
			}
			sum, ok := instrument.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s data = %T, want sum", instrument.Name, instrument.Data)
			}
			return sum.DataPoints
		}
	}
	t.Fatalf("metric %q not found", GitHubRouteAttemptsMetricName)
	return nil
}
