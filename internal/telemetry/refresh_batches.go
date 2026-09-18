package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

var (
	refreshBatchCompletions  metric.Int64Counter
	refreshBatchPullRequests metric.Int64Histogram
	refreshBatchRetries      metric.Int64Counter
)

func registerRefreshBatchInstruments(store *storage.Store) error {
	meter := otel.Meter(
		InstrumentationName,
		metric.WithInstrumentationVersion(InstrumentationVersion),
	)
	var err error
	if refreshBatchCompletions, err = meter.Int64Counter(
		CollectionBatchCompletionsMetricName,
	); err != nil {
		return err
	}
	if refreshBatchPullRequests, err = meter.Int64Histogram(
		CollectionBatchPullRequestsMetricName,
		metric.WithUnit("{pull_request}"),
	); err != nil {
		return err
	}
	if refreshBatchRetries, err = meter.Int64Counter(
		CollectionBatchRetriesMetricName,
	); err != nil {
		return err
	}
	generationAge, err := meter.Float64ObservableGauge(
		CollectionGenerationAgeMetricName,
		metric.WithUnit("s"),
	)
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		generations, err := store.ActiveRefreshGenerations(ctx)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, generation := range generations {
			observer.ObserveFloat64(
				generationAge,
				max(0, now.Sub(generation.CreatedAt).Seconds()),
				metric.WithAttributes(
					attribute.String("maintainer_cockpit.repository", generation.Repository),
				),
			)
		}
		return nil
	}, generationAge)
	return err
}

// RecordRefreshBatch records one successfully published hydration batch.
func RecordRefreshBatch(repository string, pullRequests int) {
	if !instrumentsReady.Load() {
		return
	}
	attributes := metric.WithAttributes(
		attribute.String("maintainer_cockpit.repository", repository),
	)
	refreshBatchCompletions.Add(context.Background(), 1, attributes)
	refreshBatchPullRequests.Record(context.Background(), int64(pullRequests), attributes)
}

// RecordRefreshBatchRetry records one retryable hydration failure.
func RecordRefreshBatchRetry(repository string) {
	if !instrumentsReady.Load() {
		return
	}
	refreshBatchRetries.Add(
		context.Background(), 1,
		metric.WithAttributes(
			attribute.String("maintainer_cockpit.repository", repository),
		),
	)
}
