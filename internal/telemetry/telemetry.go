// Package telemetry configures optional OpenTelemetry and exposes generated
// application-owned instruments.
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"sync"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/otelconf"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"

	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

// Status is non-sensitive exporter configuration and error state.
type Status struct {
	Configured bool   `json:"configured"`
	State      string `json:"state"`
	Message    string `json:"message,omitempty"`
}

// Runtime owns optional declaratively configured SDK providers.
type Runtime struct {
	mu     sync.RWMutex
	sdk    *otelconf.SDK
	status Status
}

// Initialize loads OTEL_CONFIG_FILE when present. With no file, all providers
// remain no-op and the application exports nothing.
func Initialize(ctx context.Context, store *storage.Store) (*Runtime, error) {
	runtime := &Runtime{status: Status{State: "disabled"}}
	if os.Getenv("OTEL_CONFIG_FILE") == "" {
		return runtime, nil
	}
	sdk, err := otelconf.NewSDK(otelconf.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	runtime.sdk = &sdk
	runtime.status = Status{Configured: true, State: "configured"}
	otel.SetTracerProvider(sdk.TracerProvider())
	otel.SetMeterProvider(sdk.MeterProvider())
	otel.SetTextMapPropagator(sdk.Propagator())
	global.SetLoggerProvider(sdk.LoggerProvider())
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(_ error) {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		runtime.status.State = "degraded"
		runtime.status.Message = "An exporter reported a failure."
	}))
	if err := registerGitHubRouteInstruments(); err != nil {
		_ = sdk.Shutdown(context.Background())
		return nil, err
	}
	if err := registerGeneratedInstruments(store); err != nil {
		_ = sdk.Shutdown(context.Background())
		return nil, err
	}
	if err := registerRefreshBatchInstruments(store); err != nil {
		_ = sdk.Shutdown(context.Background())
		return nil, err
	}
	if err := registerCorrelationInstruments(store); err != nil {
		_ = sdk.Shutdown(context.Background())
		return nil, err
	}
	return runtime, nil
}

func (r *Runtime) Status() Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status
}

// Logger returns a slog logger connected to the configured OpenTelemetry
// provider, or the process default when telemetry is disabled.
func (r *Runtime) Logger(name string) *slog.Logger {
	if r == nil || r.sdk == nil {
		return slog.Default()
	}
	return otelslog.NewLogger(name, otelslog.WithLoggerProvider(r.sdk.LoggerProvider()))
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil || r.sdk == nil {
		return nil
	}
	return r.sdk.Shutdown(ctx)
}
