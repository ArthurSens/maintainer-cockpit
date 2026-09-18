package telemetry_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"

	"github.com/ArthurSens/maintainer-cockpit/internal/application"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
	"github.com/ArthurSens/maintainer-cockpit/internal/telemetry"
)

func TestExporterOutageDoesNotAffectReadinessOrFactualDashboard(t *testing.T) {
	previousMeter := otel.GetMeterProvider()
	previousTracer := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	previousLogger := global.GetLoggerProvider()
	previousErrorHandler := otel.GetErrorHandler()
	t.Cleanup(func() {
		otel.SetMeterProvider(previousMeter)
		otel.SetTracerProvider(previousTracer)
		otel.SetTextMapPropagator(previousPropagator)
		global.SetLoggerProvider(previousLogger)
		otel.SetErrorHandler(previousErrorHandler)
	})

	exportAttempted := make(chan struct{}, 1)
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case exportAttempted <- struct{}{}:
		default:
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer exporter.Close()

	directory := t.TempDir()
	configPath := filepath.Join(directory, "maintainer-cockpit.yaml")
	if err := os.WriteFile(configPath, []byte(`
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
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := application.New(
		context.Background(), configPath, filepath.Join(directory, "app.db"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	if err := app.Store().UpsertPullRequest(context.Background(), "acme", storage.PullRequest{
		Repository: "acme/widgets", Number: 7, Title: "Factual data remains available",
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
		URL: "https://github.com/acme/widgets/pull/7", ReviewState: "none",
	}); err != nil {
		t.Fatal(err)
	}

	telemetryPath := filepath.Join(directory, "otel.yaml")
	telemetryConfig := fmt.Sprintf(`
file_format: "1.0-rc.2"
meter_provider:
  readers:
    - periodic:
        interval: 20
        timeout: 100
        exporter:
          otlp_http:
            endpoint: %s/v1/metrics
            timeout: 100
`, exporter.URL)
	if err := os.WriteFile(telemetryPath, []byte(telemetryConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_CONFIG_FILE", telemetryPath)
	runtime, err := telemetry.Initialize(context.Background(), app.Store())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = runtime.Shutdown(ctx)
	})

	select {
	case <-exportAttempted:
	case <-time.After(5 * time.Second):
		t.Fatal("telemetry exporter was not called")
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.Status().State != "degraded" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if status := runtime.Status(); status.State != "degraded" ||
		strings.Contains(status.Message, exporter.URL) {
		t.Fatalf("telemetry status = %+v, want sanitized degraded state", status)
	}

	for _, target := range []string{
		"/-/healthy", "/-/ready", "/api/collections",
		"/api/collections/acme/pull-requests",
	} {
		request := httptest.NewRequest(http.MethodGet, target, http.NoBody)
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, body = %s", target, response.Code, response.Body.String())
		}
		if target == "/api/collections/acme/pull-requests" &&
			!strings.Contains(response.Body.String(), `"title":"Factual data remains available"`) {
			t.Errorf("factual dashboard during exporter outage = %s", response.Body.String())
		}
	}
}
