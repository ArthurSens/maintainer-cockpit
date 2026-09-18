package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	genAITokenDetails metric.Int64Histogram
	genAIContentSize  metric.Int64Histogram
	genAIContentItems metric.Int64Histogram
	genAIUsageReports metric.Int64Counter
)

// ContentMeasurement describes non-sensitive semantic content shape.
// Bytes are UTF-8 content bytes and are not provider token estimates.
type ContentMeasurement struct {
	Name  string
	Bytes int64
	Items *int64
}

func registerGenAIAttributionInstruments(meter metric.Meter) error {
	var err error
	if genAITokenDetails, err = meter.Int64Histogram(
		GenAITokenDetailsMetricName, metric.WithUnit("{token}"),
	); err != nil {
		return err
	}
	if genAIContentSize, err = meter.Int64Histogram(
		GenAIContentSizeMetricName, metric.WithUnit("By"),
	); err != nil {
		return err
	}
	if genAIContentItems, err = meter.Int64Histogram(
		GenAIContentItemsMetricName, metric.WithUnit("{item}"),
	); err != nil {
		return err
	}
	genAIUsageReports, err = meter.Int64Counter(
		GenAIUsageReportsMetricName, metric.WithUnit("{report}"),
	)
	return err
}

func appendGenAIAttributionAttributes(
	attributes []attribute.KeyValue, request AnalysisRequestTelemetry,
) []attribute.KeyValue {
	if request.Workload != "" {
		attributes = append(attributes,
			attribute.String("maintainer_cockpit.gen_ai.workload", request.Workload))
	}
	if request.ProviderProfile != "" {
		attributes = append(attributes,
			attribute.String("maintainer_cockpit.model_provider", request.ProviderProfile))
	}
	return attributes
}

func appendGenAISpanAttributes(
	attributes []attribute.KeyValue, request AnalysisRequestTelemetry,
) []attribute.KeyValue {
	if request.PromptVersion != "" {
		attributes = append(attributes, attribute.String("gen_ai.prompt.version", request.PromptVersion))
	}
	if request.SchemaVersion != "" {
		attributes = append(attributes,
			attribute.String("maintainer_cockpit.schema.version", request.SchemaVersion))
	}
	if request.JobID > 0 {
		attributes = append(attributes, attribute.Int64("maintainer_cockpit.job.id", request.JobID))
	}
	if request.Repository != "" {
		attributes = append(attributes,
			attribute.String("maintainer_cockpit.repository", request.Repository))
	}
	if request.PullRequest > 0 {
		attributes = append(attributes,
			attribute.Int("maintainer_cockpit.pull_request.number", request.PullRequest))
	}
	if request.Chunk > 0 {
		attributes = append(attributes,
			attribute.Int("maintainer_cockpit.correlation.chunk", request.Chunk))
	}
	if request.Chunks > 0 {
		attributes = append(attributes,
			attribute.Int("maintainer_cockpit.correlation.chunks", request.Chunks))
	}
	return attributes
}

func recordGenAIAttribution(
	ctx context.Context,
	span trace.Span,
	metricAttributes []attribute.KeyValue,
	outcome AnalysisOutcomeTelemetry,
) {
	recordUsageReport(ctx, metricAttributes, "input", outcome.InputTokens)
	recordUsageReport(ctx, metricAttributes, "output", outcome.OutputTokens)
	if outcome.InputTokens != nil {
		span.SetAttributes(attribute.Int64("gen_ai.usage.input_tokens", *outcome.InputTokens))
	}
	if outcome.OutputTokens != nil {
		span.SetAttributes(attribute.Int64("gen_ai.usage.output_tokens", *outcome.OutputTokens))
	}
	if outcome.CachedInputTokens != nil {
		genAITokenDetails.Record(ctx, *outcome.CachedInputTokens, metric.WithAttributes(
			appendCopy(metricAttributes,
				attribute.String("maintainer_cockpit.gen_ai.token.detail", "cached_input"))...,
		))
	}
	if outcome.ReasoningOutputTokens != nil {
		genAITokenDetails.Record(ctx, *outcome.ReasoningOutputTokens, metric.WithAttributes(
			appendCopy(metricAttributes,
				attribute.String("maintainer_cockpit.gen_ai.token.detail", "reasoning_output"))...,
		))
	}
	recordContentMeasurements(ctx, metricAttributes, "input", outcome.InputComponents)
	recordContentMeasurements(ctx, metricAttributes, "output", outcome.OutputComponents)
}

func recordUsageReport(
	ctx context.Context, attributes []attribute.KeyValue, tokenType string, tokens *int64,
) {
	availability := "missing"
	if tokens != nil {
		availability = "available"
	}
	genAIUsageReports.Add(ctx, 1, metric.WithAttributes(appendCopy(attributes,
		attribute.String("gen_ai.token.type", tokenType),
		attribute.String("maintainer_cockpit.gen_ai.usage.availability", availability),
	)...))
}

func recordContentMeasurements(
	ctx context.Context,
	attributes []attribute.KeyValue,
	direction string,
	measurements []ContentMeasurement,
) {
	for _, measurement := range measurements {
		componentAttributes := appendCopy(attributes,
			attribute.String("maintainer_cockpit.gen_ai.content.direction", direction),
			attribute.String("maintainer_cockpit.gen_ai.content.component", measurement.Name),
		)
		genAIContentSize.Record(ctx, measurement.Bytes, metric.WithAttributes(componentAttributes...))
		if measurement.Items != nil {
			genAIContentItems.Record(ctx, *measurement.Items, metric.WithAttributes(componentAttributes...))
		}
	}
}

func appendCopy(values []attribute.KeyValue, additions ...attribute.KeyValue) []attribute.KeyValue {
	result := make([]attribute.KeyValue, 0, len(values)+len(additions))
	result = append(result, values...)
	return append(result, additions...)
}
