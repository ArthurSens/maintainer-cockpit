package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

var authorEvidenceAttempts metric.Int64Counter

func registerAuthorEvidenceInstruments(store *storage.Store) error {
	meter := otel.Meter(InstrumentationName, metric.WithInstrumentationVersion(InstrumentationVersion))
	var err error
	authorEvidenceAttempts, err = meter.Int64Counter(AuthorEvidenceAttemptsMetricName)
	if err != nil {
		return err
	}
	depth, err := meter.Int64ObservableGauge(QueueDepthMetricName)
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		count, err := store.AncillaryQueueDepth(ctx)
		if err != nil {
			return err
		}
		observer.ObserveInt64(depth, count, metric.WithAttributes(
			attribute.String("maintainer_cockpit.queue.type", "author_evidence")))
		return nil
	}, depth)
	return err
}

// RecordAuthorEvidence counts persisted outcomes without author identities.
func RecordAuthorEvidence(ctx context.Context, outcome string) {
	if authorEvidenceAttempts != nil {
		authorEvidenceAttempts.Add(ctx, 1, metric.WithAttributes(
			attribute.String("maintainer_cockpit.author_evidence.outcome", outcome)))
	}
}
