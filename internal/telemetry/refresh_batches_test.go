package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRecordRefreshBatchAndRetry(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	original := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(original)
		instrumentsReady.Store(false)
		_ = provider.Shutdown(context.Background())
	})

	meter := provider.Meter(InstrumentationName)
	var err error
	if refreshBatchCompletions, err = meter.Int64Counter(
		CollectionBatchCompletionsMetricName,
	); err != nil {
		t.Fatal(err)
	}
	if refreshBatchPullRequests, err = meter.Int64Histogram(
		CollectionBatchPullRequestsMetricName,
	); err != nil {
		t.Fatal(err)
	}
	if refreshBatchRetries, err = meter.Int64Counter(
		CollectionBatchRetriesMetricName,
	); err != nil {
		t.Fatal(err)
	}
	instrumentsReady.Store(true)

	RecordRefreshBatch("acme/widgets", 20)
	RecordRefreshBatchRetry("acme/widgets")

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	assertCounterValue(t, metrics, CollectionBatchCompletionsMetricName, 1)
	assertHistogramCount(t, metrics, CollectionBatchPullRequestsMetricName, 1)
	assertCounterValue(t, metrics, CollectionBatchRetriesMetricName, 1)
}
