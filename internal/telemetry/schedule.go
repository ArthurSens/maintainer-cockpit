package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ArthurSens/maintainer-cockpit/internal/periodic"
)

// RegisterScheduleInstruments exports the same live next-run timestamps used
// by the administrator API. Cron expressions are deliberately not attributes.
func RegisterScheduleInstruments(status func() []periodic.Status) error {
	meter := otel.Meter(InstrumentationName, metric.WithInstrumentationVersion(InstrumentationVersion))
	nextRun, err := meter.Int64ObservableGauge(
		ScheduleNextRunMetricName, metric.WithUnit("s"),
	)
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		for _, schedule := range status() {
			if !schedule.Enabled || schedule.NextRun.IsZero() {
				continue
			}
			attributes := []attribute.KeyValue{
				attribute.String("maintainer_cockpit.schedule.operation", schedule.Operation),
			}
			if schedule.Collection != "" {
				attributes = append(attributes,
					attribute.String("maintainer_cockpit.collection", schedule.Collection),
				)
			}
			observer.ObserveInt64(
				nextRun, schedule.NextRun.Unix(), metric.WithAttributes(attributes...),
			)
		}
		return nil
	}, nextRun)
	return err
}
