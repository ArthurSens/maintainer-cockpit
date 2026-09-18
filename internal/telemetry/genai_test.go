package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestAnalysisTelemetryFollowsGenAISemanticConventions(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	traceExporter := tracetest.NewInMemoryExporter()
	traceProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(traceExporter))
	originalMeterProvider := otel.GetMeterProvider()
	originalTraceProvider := otel.GetTracerProvider()
	otel.SetMeterProvider(meterProvider)
	otel.SetTracerProvider(traceProvider)
	t.Cleanup(func() {
		otel.SetMeterProvider(originalMeterProvider)
		otel.SetTracerProvider(originalTraceProvider)
		_ = meterProvider.Shutdown(context.Background())
		_ = traceProvider.Shutdown(context.Background())
	})

	meter := meterProvider.Meter(InstrumentationName)
	var err error
	analysisAttempts, err = meter.Int64Counter(AnalysisAttemptsMetricName)
	if err != nil {
		t.Fatal(err)
	}
	analysisSuccesses, _ = meter.Int64Counter(AnalysisSuccessesMetricName)
	providerFailures, _ = meter.Int64Counter(ProviderFailuresMetricName)
	schemaFailures, _ = meter.Int64Counter(SchemaValidationFailuresMetricName)
	unknownAnalyses, _ = meter.Int64Counter(UnknownAnalysesMetricName)
	genAIOperationDuration, _ = meter.Float64Histogram(
		GenAIClientOperationDurationMetricName, otelmetric.WithUnit("s"),
	)
	genAITokenUsage, _ = meter.Int64Histogram(
		GenAIClientTokenUsageMetricName, otelmetric.WithUnit("{token}"),
	)
	genAITokenDetails, _ = meter.Int64Histogram(
		"maintainer_cockpit.gen_ai.token.details", otelmetric.WithUnit("{token}"),
	)
	genAIContentSize, _ = meter.Int64Histogram(
		"maintainer_cockpit.gen_ai.content.size", otelmetric.WithUnit("By"),
	)
	genAIContentItems, _ = meter.Int64Histogram(
		"maintainer_cockpit.gen_ai.content.items", otelmetric.WithUnit("{item}"),
	)
	genAIUsageReports, _ = meter.Int64Counter(
		"maintainer_cockpit.gen_ai.usage.reports",
	)
	instrumentsReady.Store(true)
	t.Cleanup(func() { instrumentsReady.Store(false) })

	ctx, finish := StartAnalysis(context.Background(), AnalysisRequestTelemetry{
		Collection: "prometheus", ProviderName: "openai", RequestModel: "gpt-5-mini",
		ServerAddress: "api.openai.com", ServerPort: 443,
		ProviderProfile: "production", Workload: "analysis",
		PromptVersion: "analysis-prompt.v2", SchemaVersion: "analysis.v2",
		JobID: 9, Repository: "prometheus/prometheus", PullRequest: 123,
	})
	inputTokens, outputTokens := int64(101), int64(23)
	cachedTokens, reasoningTokens := int64(61), int64(7)
	sourceItems := int64(4)
	finish(AnalysisOutcomeTelemetry{
		Status: "complete", ResponseModel: "gpt-5-mini-2026-08-01",
		ResponseID: "chatcmpl-123", FinishReasons: []string{"stop"},
		InputTokens: &inputTokens, OutputTokens: &outputTokens,
		CachedInputTokens: &cachedTokens, ReasoningOutputTokens: &reasoningTokens,
		InputComponents: []ContentMeasurement{
			{Name: "system_instruction", Bytes: 200},
			{Name: "evidence", Bytes: 800, Items: &sourceItems},
		},
		OutputComponents: []ContentMeasurement{{Name: "evaluations", Bytes: 120}},
	})
	_ = ctx

	spans := traceExporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != "chat gpt-5-mini" || span.SpanKind != trace.SpanKindClient {
		t.Errorf("span = %q kind %v, want CLIENT chat span", span.Name, span.SpanKind)
	}
	attributes := attribute.NewSet(span.Attributes...)
	assertStringAttribute(t, attributes, "gen_ai.operation.name", "chat")
	assertStringAttribute(t, attributes, "gen_ai.provider.name", "openai")
	assertStringAttribute(t, attributes, "gen_ai.request.model", "gpt-5-mini")
	assertStringAttribute(t, attributes, "gen_ai.response.model", "gpt-5-mini-2026-08-01")
	assertStringAttribute(t, attributes, "gen_ai.response.id", "chatcmpl-123")
	assertStringAttribute(t, attributes, "gen_ai.output.type", "json")
	assertStringAttribute(t, attributes, "gen_ai.prompt.name", "maintainer_cockpit.analysis")
	assertStringAttribute(t, attributes, "gen_ai.prompt.version", "analysis-prompt.v2")
	assertStringAttribute(t, attributes, "maintainer_cockpit.gen_ai.workload", "analysis")
	assertStringAttribute(t, attributes, "maintainer_cockpit.model_provider", "production")
	assertStringAttribute(t, attributes, "maintainer_cockpit.repository", "prometheus/prometheus")
	assertStringAttribute(t, attributes, "server.address", "api.openai.com")
	finishReasons, ok := attributes.Value("gen_ai.response.finish_reasons")
	if !ok || len(finishReasons.AsStringSlice()) != 1 || finishReasons.AsStringSlice()[0] != "stop" {
		t.Errorf("gen_ai.response.finish_reasons = %v, present %v", finishReasons, ok)
	}

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	assertHistogramCount(t, metrics, GenAIClientOperationDurationMetricName, 1)
	assertTokenUsage(t, metrics, 101, 23)
	assertHistogramCount(t, metrics, "maintainer_cockpit.gen_ai.token.details", 2)
	assertHistogramCount(t, metrics, "maintainer_cockpit.gen_ai.content.size", 3)
	assertHistogramCount(t, metrics, "maintainer_cockpit.gen_ai.content.items", 1)
	assertCounterValue(t, metrics, "maintainer_cockpit.gen_ai.usage.reports", 2)
}

func TestAnalysisTelemetryRecordsErrorType(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	original := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	instrumentsReady.Store(true)
	t.Cleanup(func() {
		instrumentsReady.Store(false)
		otel.SetTracerProvider(original)
		_ = provider.Shutdown(context.Background())
	})

	_, finish := StartAnalysis(context.Background(), AnalysisRequestTelemetry{
		ProviderName: "ollama", RequestModel: "qwen3:8b",
	})
	finish(AnalysisOutcomeTelemetry{
		Status: "failed", FailureKind: "provider", ErrorType: "provider_error",
	})

	attributes := attribute.NewSet(exporter.GetSpans()[0].Attributes...)
	assertStringAttribute(t, attributes, "error.type", "provider_error")
}

func TestCorrelationTelemetryUsesSeparatePromptAndCounters(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	traceExporter := tracetest.NewInMemoryExporter()
	traceProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(traceExporter))
	originalMeterProvider := otel.GetMeterProvider()
	originalTraceProvider := otel.GetTracerProvider()
	otel.SetMeterProvider(meterProvider)
	otel.SetTracerProvider(traceProvider)
	t.Cleanup(func() {
		otel.SetMeterProvider(originalMeterProvider)
		otel.SetTracerProvider(originalTraceProvider)
		_ = meterProvider.Shutdown(context.Background())
		_ = traceProvider.Shutdown(context.Background())
	})

	meter := meterProvider.Meter(InstrumentationName)
	correlationAttempts, _ = meter.Int64Counter(CorrelationAttemptsMetricName)
	correlationSuccesses, _ = meter.Int64Counter(CorrelationSuccessesMetricName)
	correlationProviderErrors, _ = meter.Int64Counter(CorrelationProviderFailuresMetricName)
	correlationSchemaErrors, _ = meter.Int64Counter(CorrelationSchemaValidationFailuresMetricName)
	genAIOperationDuration, _ = meter.Float64Histogram(
		GenAIClientOperationDurationMetricName, otelmetric.WithUnit("s"),
	)
	genAITokenUsage, _ = meter.Int64Histogram(
		GenAIClientTokenUsageMetricName, otelmetric.WithUnit("{token}"),
	)
	genAITokenDetails, _ = meter.Int64Histogram(
		"maintainer_cockpit.gen_ai.token.details", otelmetric.WithUnit("{token}"),
	)
	genAIContentSize, _ = meter.Int64Histogram(
		"maintainer_cockpit.gen_ai.content.size", otelmetric.WithUnit("By"),
	)
	genAIContentItems, _ = meter.Int64Histogram(
		"maintainer_cockpit.gen_ai.content.items", otelmetric.WithUnit("{item}"),
	)
	genAIUsageReports, _ = meter.Int64Counter(
		"maintainer_cockpit.gen_ai.usage.reports",
	)
	instrumentsReady.Store(true)
	t.Cleanup(func() { instrumentsReady.Store(false) })

	_, finish := StartCorrelation(context.Background(), AnalysisRequestTelemetry{
		Collection: "prometheus", ProviderName: "openai", RequestModel: "gpt-5-mini",
		ProviderProfile: "production", Workload: "correlation",
		PromptVersion: "correlation-prompt.v1", SchemaVersion: "correlation.v1",
		JobID: 10, Chunk: 2, Chunks: 3,
	})
	finish(AnalysisOutcomeTelemetry{Status: "complete"})

	spans := traceExporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	attributes := attribute.NewSet(spans[0].Attributes...)
	assertStringAttribute(t, attributes, "gen_ai.prompt.name", "maintainer_cockpit.correlation")
	assertStringAttribute(t, attributes, "maintainer_cockpit.gen_ai.workload", "correlation")
	assertStringAttribute(t, attributes, "gen_ai.prompt.version", "correlation-prompt.v1")
	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	assertCounterValue(t, metrics, CorrelationAttemptsMetricName, 1)
	assertCounterValue(t, metrics, CorrelationSuccessesMetricName, 1)
	assertCounterValue(t, metrics, "maintainer_cockpit.gen_ai.usage.reports", 2)
}

func assertStringAttribute(t *testing.T, attributes attribute.Set, key, want string) {
	t.Helper()
	value, ok := attributes.Value(attribute.Key(key))
	if !ok || value.AsString() != want {
		t.Errorf("%s = %v, present %v, want %q", key, value, ok, want)
	}
}

func assertTokenUsage(t *testing.T, metrics metricdata.ResourceMetrics, input, output int64) {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != GenAIClientTokenUsageMetricName {
				continue
			}
			data, ok := instrument.Data.(metricdata.Histogram[int64])
			if !ok {
				t.Fatalf("%s has unexpected data: %+v", instrument.Name, instrument.Data)
			}
			got := make(map[string]int64)
			for _, point := range data.DataPoints {
				tokenType, _ := point.Attributes.Value("gen_ai.token.type")
				got[tokenType.AsString()] = point.Sum
			}
			if got["input"] != input || got["output"] != output {
				t.Errorf("token usage = %+v, want input %d and output %d", got, input, output)
			}
			return
		}
	}
	t.Errorf("metric %q not found", GenAIClientTokenUsageMetricName)
}

func assertHistogramCount(t *testing.T, metrics metricdata.ResourceMetrics, name string, want uint64) {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != name {
				continue
			}
			switch data := instrument.Data.(type) {
			case metricdata.Histogram[float64]:
				if len(data.DataPoints) == 1 && data.DataPoints[0].Count == want {
					return
				}
			case metricdata.Histogram[int64]:
				var count uint64
				for _, point := range data.DataPoints {
					count += point.Count
				}
				if count == want {
					return
				}
			}
			t.Fatalf("%s has unexpected data: %+v", name, instrument.Data)
		}
	}
	t.Errorf("metric %q not found", name)
}

func assertCounterValue(t *testing.T, metrics metricdata.ResourceMetrics, name string, want int64) {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != name {
				continue
			}
			data, ok := instrument.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s has unexpected data: %+v", name, instrument.Data)
			}
			var got int64
			for _, point := range data.DataPoints {
				got += point.Value
			}
			if got != want {
				t.Errorf("%s = %d, want %d", name, got, want)
			}
			return
		}
	}
	t.Errorf("metric %q not found", name)
}
