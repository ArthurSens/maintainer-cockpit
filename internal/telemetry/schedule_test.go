package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/ArthurSens/maintainer-cockpit/internal/periodic"
)

func TestScheduleTelemetryExportsTheSameNextRunTimestamp(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	original := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(original)
		_ = provider.Shutdown(context.Background())
	})
	next := time.Date(2026, 9, 14, 4, 0, 0, 0, time.UTC)
	if err := RegisterScheduleInstruments(func() []periodic.Status {
		return []periodic.Status{{
			Operation: "feature_correlation", Collection: "acme",
			Expression: "0 4 * * *", Enabled: true, NextRun: next,
		}}
	}); err != nil {
		t.Fatal(err)
	}
	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != ScheduleNextRunMetricName {
				continue
			}
			gauge, ok := metric.Data.(metricdata.Gauge[int64])
			if !ok || len(gauge.DataPoints) != 1 {
				t.Fatalf("schedule gauge = %#v", metric.Data)
			}
			if gauge.DataPoints[0].Value != next.Unix() {
				t.Fatalf("next run = %d, want %d", gauge.DataPoints[0].Value, next.Unix())
			}
			return
		}
	}
	t.Fatalf("metric %q not found", ScheduleNextRunMetricName)
}
