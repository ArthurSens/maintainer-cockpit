package telemetry_test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"

	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
	"github.com/ArthurSens/maintainer-cockpit/internal/telemetry"
)

func TestLGTMExampleConfigurationLoads(t *testing.T) {
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

	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	telemetryPath, err := filepath.Abs("../../examples/otel-lgtm/otel-sdk.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_CONFIG_FILE", telemetryPath)
	runtime, err := telemetry.Initialize(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = runtime.Shutdown(ctx)
	})
	if status := runtime.Status(); !status.Configured || status.State != "configured" {
		t.Errorf("telemetry status = %+v, want configured", status)
	}
	if handlerType := fmt.Sprintf("%T", runtime.Logger("test").Handler()); handlerType != "*otelslog.Handler" {
		t.Errorf("logger handler = %s, want OpenTelemetry slog bridge", handlerType)
	}
}

func TestDisabledRuntimeLoggerUsesSlogDefault(t *testing.T) {
	runtime, err := telemetry.Initialize(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Logger("test").Handler() != slog.Default().Handler() {
		t.Error("disabled telemetry runtime did not return the default slog handler")
	}
}
