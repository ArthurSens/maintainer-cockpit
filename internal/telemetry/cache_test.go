package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRecordCollectionCacheSeparatesHitsMissesAndBypasses(t *testing.T) {
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
	if collectionCacheHits, err = meter.Int64Counter(CollectionCacheHitsMetricName); err != nil {
		t.Fatal(err)
	}
	if collectionCacheMisses, err = meter.Int64Counter(CollectionCacheMissesMetricName); err != nil {
		t.Fatal(err)
	}
	if collectionCacheBypasses, err = meter.Int64Counter(CollectionCacheBypassesMetricName); err != nil {
		t.Fatal(err)
	}
	instrumentsReady.Store(true)

	RecordCollectionCache("acme/widgets", 7, 2, 3)

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	assertCounterValue(t, metrics, CollectionCacheHitsMetricName, 7)
	assertCounterValue(t, metrics, CollectionCacheMissesMetricName, 2)
	assertCounterValue(t, metrics, CollectionCacheBypassesMetricName, 3)
}
