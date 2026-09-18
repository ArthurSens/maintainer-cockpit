package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

var (
	correlationAttempts       metric.Int64Counter
	correlationSuccesses      metric.Int64Counter
	correlationProviderErrors metric.Int64Counter
	correlationSchemaErrors   metric.Int64Counter
)

func registerCorrelationInstruments(store *storage.Store) error {
	meter := otel.Meter(InstrumentationName, metric.WithInstrumentationVersion(InstrumentationVersion))
	var err error
	if correlationAttempts, err = meter.Int64Counter(CorrelationAttemptsMetricName); err != nil {
		return err
	}
	if correlationSuccesses, err = meter.Int64Counter(CorrelationSuccessesMetricName); err != nil {
		return err
	}
	if correlationProviderErrors, err = meter.Int64Counter(CorrelationProviderFailuresMetricName); err != nil {
		return err
	}
	if correlationSchemaErrors, err = meter.Int64Counter(CorrelationSchemaValidationFailuresMetricName); err != nil {
		return err
	}
	queueDepth, err := meter.Int64ObservableGauge(QueueDepthMetricName)
	if err != nil {
		return err
	}
	queueAge, err := meter.Float64ObservableGauge(QueueOldestAgeMetricName, metric.WithUnit("s"))
	if err != nil {
		return err
	}
	lastSuccess, err := meter.Int64ObservableGauge(
		CorrelationLastSuccessMetricName, metric.WithUnit("s"),
	)
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		statuses, err := store.ListCorrelationStatuses(ctx)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		var queued int
		var oldest *time.Time
		for _, status := range statuses {
			queued += status.Queued
			if status.Oldest != nil && (oldest == nil || status.Oldest.Before(*oldest)) {
				value := *status.Oldest
				oldest = &value
			}
			if status.LastSuccess != nil {
				observer.ObserveInt64(
					lastSuccess, status.LastSuccess.Unix(),
					metric.WithAttributes(attribute.String(
						"maintainer_cockpit.collection", status.CollectionID,
					)),
				)
			}
		}
		observer.ObserveInt64(
			queueDepth, int64(queued),
			metric.WithAttributes(attribute.String("maintainer_cockpit.queue.type", "correlation")),
		)
		if oldest != nil {
			observer.ObserveFloat64(
				queueAge, now.Sub(*oldest).Seconds(),
				metric.WithAttributes(attribute.String("maintainer_cockpit.queue.type", "correlation")),
			)
		}
		return nil
	}, queueDepth, queueAge, lastSuccess)
	return err
}

// StartCorrelation records one feature correlation without prompt content.
func StartCorrelation(
	ctx context.Context, request AnalysisRequestTelemetry,
) (context.Context, func(AnalysisOutcomeTelemetry)) {
	if !instrumentsReady.Load() {
		return ctx, func(AnalysisOutcomeTelemetry) {}
	}
	counterAttributes := []attribute.KeyValue{
		attribute.String("maintainer_cockpit.collection", request.Collection),
	}
	if request.ProviderName != "" {
		counterAttributes = append(counterAttributes,
			attribute.String("gen_ai.provider.name", request.ProviderName))
	}
	if request.RequestModel != "" {
		counterAttributes = append(counterAttributes,
			attribute.String("gen_ai.request.model", request.RequestModel))
	}
	metricAttributes := append([]attribute.KeyValue{}, counterAttributes...)
	metricAttributes = appendGenAIAttributionAttributes(metricAttributes, request)
	metricAttributes = append(metricAttributes, attribute.String("gen_ai.operation.name", "chat"))
	if request.ServerAddress != "" {
		metricAttributes = append(metricAttributes, attribute.String("server.address", request.ServerAddress))
		if request.ServerPort > 0 {
			metricAttributes = append(metricAttributes, attribute.Int("server.port", request.ServerPort))
		}
	}
	spanAttributes := append([]attribute.KeyValue{}, metricAttributes...)
	spanAttributes = append(spanAttributes,
		attribute.String("gen_ai.output.type", "json"),
		attribute.String("gen_ai.prompt.name", "maintainer_cockpit.correlation"),
	)
	spanAttributes = appendGenAISpanAttributes(spanAttributes, request)
	correlationAttempts.Add(ctx, 1, metric.WithAttributes(counterAttributes...))
	spanName := "chat"
	if request.RequestModel != "" {
		spanName += " " + request.RequestModel
	}
	ctx, span := otel.Tracer(InstrumentationName).Start(
		ctx, spanName, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(spanAttributes...),
	)
	started := time.Now()
	return ctx, func(outcome AnalysisOutcomeTelemetry) {
		finalAttributes := append([]attribute.KeyValue{}, metricAttributes...)
		if outcome.ResponseModel != "" {
			span.SetAttributes(attribute.String("gen_ai.response.model", outcome.ResponseModel))
			finalAttributes = append(finalAttributes,
				attribute.String("gen_ai.response.model", outcome.ResponseModel))
		}
		if outcome.ResponseID != "" {
			span.SetAttributes(attribute.String("gen_ai.response.id", outcome.ResponseID))
		}
		if len(outcome.FinishReasons) > 0 {
			span.SetAttributes(attribute.StringSlice(
				"gen_ai.response.finish_reasons", outcome.FinishReasons,
			))
		}
		if outcome.ErrorType != "" {
			span.SetAttributes(attribute.String("error.type", outcome.ErrorType))
			finalAttributes = append(finalAttributes, attribute.String("error.type", outcome.ErrorType))
		}
		genAIOperationDuration.Record(
			ctx, time.Since(started).Seconds(), metric.WithAttributes(finalAttributes...),
		)
		if outcome.InputTokens != nil {
			genAITokenUsage.Record(ctx, *outcome.InputTokens, metric.WithAttributes(
				append(finalAttributes, attribute.String("gen_ai.token.type", "input"))...,
			))
		}
		if outcome.OutputTokens != nil {
			genAITokenUsage.Record(ctx, *outcome.OutputTokens, metric.WithAttributes(
				append(finalAttributes, attribute.String("gen_ai.token.type", "output"))...,
			))
		}
		recordGenAIAttribution(ctx, span, finalAttributes, outcome)
		switch {
		case outcome.Status == "complete":
			correlationSuccesses.Add(ctx, 1, metric.WithAttributes(counterAttributes...))
		case outcome.FailureKind == "provider":
			correlationProviderErrors.Add(ctx, 1, metric.WithAttributes(counterAttributes...))
			span.SetStatus(codes.Error, "provider failed")
		case outcome.FailureKind == "schema":
			correlationSchemaErrors.Add(ctx, 1, metric.WithAttributes(counterAttributes...))
			span.SetStatus(codes.Error, "schema validation failed")
		default:
			span.SetStatus(codes.Error, "correlation unavailable")
		}
		span.End()
	}
}
