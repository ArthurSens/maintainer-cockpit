// Package correlationworker processes durable collection-level correlation jobs.
package correlationworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/ArthurSens/maintainer-cockpit/internal/analysis"
	"github.com/ArthurSens/maintainer-cockpit/internal/correlation"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
	"github.com/ArthurSens/maintainer-cockpit/internal/telemetry"
)

// Provider transports one strict correlation request.
type Provider interface {
	Correlate(context.Context, correlation.Request) (analysis.ProviderResult, error)
}

type Options struct {
	PollInterval time.Duration
	Now          func() time.Time
	Logger       *slog.Logger
}

type Worker struct {
	store        *storage.Store
	providers    map[string]Provider
	pollInterval time.Duration
	now          func() time.Time
	logger       *slog.Logger
}

func New(store *storage.Store, providers map[string]Provider, options Options) *Worker {
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Worker{
		store: store, providers: providers, pollInterval: options.PollInterval, now: options.Now,
		logger: options.Logger,
	}
}

func (worker *Worker) Run(ctx context.Context) error {
	if err := worker.store.RecoverCorrelationJobs(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(worker.pollInterval)
	defer ticker.Stop()
	for {
		if err := worker.RunOnce(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (worker *Worker) RunOnce(ctx context.Context) error {
	now := worker.now().UTC()
	job, err := worker.store.ClaimCorrelation(ctx, now)
	if err != nil || job == nil {
		return err
	}
	if err := worker.process(ctx, *job, now); err != nil {
		if errors.Is(err, storage.ErrCorrelationInputsChanged) {
			if completeErr := worker.store.CompleteCorrelationJob(
				ctx, job.ID, worker.now().UTC(),
			); completeErr != nil {
				return completeErr
			}
			_, _, enqueueErr := worker.store.EnqueueCollectionCorrelation(
				ctx, job.CollectionID, false, worker.now().UTC(),
			)
			return enqueueErr
		}
		return err
	}
	return worker.store.CompleteCorrelationJob(ctx, job.ID, worker.now().UTC())
}

func (worker *Worker) process(
	ctx context.Context, job storage.CorrelationJob, now time.Time,
) error {
	requests, err := worker.store.BuildCorrelationRequests(ctx, job)
	if err != nil {
		return fmt.Errorf("build correlation request for %s: %w", job.CollectionID, err)
	}
	if len(requests) == 0 {
		return worker.store.RecordCorrelationSuccess(
			ctx, job, requests, nil, job.PrimaryProvider, "", "",
			[]byte("[]"), []byte("[]"), now,
		)
	}
	providerNames := []string{job.PrimaryProvider}
	if job.FallbackProvider != "" {
		providerNames = append(providerNames, job.FallbackProvider)
	}
	var lastProvider, lastModel string
	var lastRequest, lastResponse []byte
	var lastErr error
	var lastAttemptSpanContext trace.SpanContext
	for _, providerName := range providerNames {
		provider := worker.providers[providerName]
		if provider == nil {
			lastProvider = providerName
			lastErr = fmt.Errorf("configured provider %q is unavailable", providerName)
			attemptContext, finish := telemetry.StartCorrelation(ctx, telemetry.AnalysisRequestTelemetry{
				Collection: job.CollectionID, ProviderProfile: providerName,
				Workload: "correlation", PromptVersion: correlation.PromptVersion,
				SchemaVersion: correlation.SchemaVersion, JobID: job.ID,
			})
			lastAttemptSpanContext = trace.SpanContextFromContext(attemptContext)
			finish(telemetry.AnalysisOutcomeTelemetry{
				Status: "failed", FailureKind: "provider", ErrorType: "provider_error",
			})
			continue
		}
		groups := make([]correlation.Group, 0)
		groupIDs := make(map[string]struct{})
		requestBodies := make([]json.RawMessage, 0, len(requests))
		responseBodies := make([]json.RawMessage, 0, len(requests))
		valid := true
		for _, request := range requests {
			requestTelemetry := telemetry.AnalysisRequestTelemetry{
				Collection: job.CollectionID, ProviderProfile: providerName,
				Workload: "correlation", PromptVersion: request.PromptVersion,
				SchemaVersion: request.SchemaVersion, JobID: job.ID,
				Chunk: request.Chunk, Chunks: request.Chunks,
			}
			switch instrumented := provider.(type) {
			case analysis.TelemetryMetadataProvider:
				metadata := instrumented.TelemetryMetadata()
				requestTelemetry.ProviderName = metadata.Name
				requestTelemetry.RequestModel = metadata.RequestModel
				requestTelemetry.ServerAddress = metadata.ServerAddress
				requestTelemetry.ServerPort = metadata.ServerPort
			case analysis.ModelNamer:
				requestTelemetry.RequestModel = instrumented.ModelName()
			}
			attemptContext, finish := telemetry.StartCorrelation(ctx, requestTelemetry)
			lastAttemptSpanContext = trace.SpanContextFromContext(attemptContext)
			started := time.Now()
			providerResult, providerErr := provider.Correlate(attemptContext, request)
			duration := time.Since(started)
			lastProvider, lastModel = providerName, providerResult.Model
			if len(providerResult.RequestBody) > 0 {
				requestBodies = append(requestBodies, providerResult.RequestBody)
			}
			if len(providerResult.ResponseBody) > 0 {
				responseBodies = append(responseBodies, providerResult.ResponseBody)
			}
			lastRequest, _ = json.Marshal(requestBodies)
			lastResponse, _ = json.Marshal(responseBodies)
			if providerErr != nil {
				lastErr = providerErr
				worker.logProviderFailure(
					attemptContext, job, request, providerName, providerResult.Model, providerErr,
				)
				finish(correlationTelemetryOutcome(
					providerResult, "failed", "provider", "provider_error",
				))
				valid = false
				break
			}
			worker.logProviderSuccess(
				attemptContext, job, request, providerName, providerResult, duration,
			)
			members, evidence := correlation.KnownSources(request)
			result, validationErr := correlation.ValidateResponse(
				providerResult.Output, members, evidence,
			)
			if validationErr != nil {
				lastErr = validationErr
				finish(correlationTelemetryOutcome(
					providerResult, "failed", "schema", "schema_validation_error",
				))
				valid = false
				break
			}
			for _, group := range result.Groups {
				if _, exists := groupIDs[group.ID]; exists {
					lastErr = fmt.Errorf("correlation group %q appeared in more than one chunk", group.ID)
					valid = false
					break
				}
				groupIDs[group.ID] = struct{}{}
				groups = append(groups, group)
			}
			if !valid {
				finish(correlationTelemetryOutcome(
					providerResult, "failed", "schema", "schema_validation_error",
				))
				break
			}
			outcome := correlationTelemetryOutcome(providerResult, "complete", "", "")
			outcome.OutputComponents = correlationOutputMeasurements(result)
			finish(outcome)
		}
		if !valid {
			continue
		}
		requestBody, _ := json.Marshal(requestBodies)
		responseBody, _ := json.Marshal(responseBodies)
		fallbackFrom := ""
		if providerName != job.PrimaryProvider {
			fallbackFrom = job.PrimaryProvider
		}
		return worker.store.RecordCorrelationSuccess(
			ctx, job, requests, groups, providerName, fallbackFrom, lastModel,
			requestBody, responseBody, now,
		)
	}
	worker.logCorrelationFailure(lastAttemptSpanContext, job, lastProvider, lastErr)
	return worker.store.RecordCorrelationFailure(
		ctx, job, lastProvider, lastModel, lastRequest, lastResponse, now,
		"Model provider unavailable or returned invalid correlation data; the last successful groups remain available.",
	)
}

func (worker *Worker) logCorrelationFailure(
	spanContext trace.SpanContext,
	job storage.CorrelationJob,
	provider string,
	err error,
) {
	fields := []slog.Attr{
		slog.String("event", "feature_correlation_failed"),
		slog.Int64("job_id", job.ID),
		slog.String("collection", job.CollectionID),
		slog.String("provider", provider),
	}
	if err != nil {
		fields = appendOptionalLogField(fields, "detail", boundedLogDetail(err.Error()))
	}
	fields = appendSpanContextFields(spanContext, fields)
	logContext := trace.ContextWithSpanContext(context.Background(), spanContext)
	worker.logger.LogAttrs(logContext, slog.LevelError, "feature correlation failed", fields...)
}

func (worker *Worker) logProviderFailure(
	ctx context.Context,
	job storage.CorrelationJob,
	request correlation.Request,
	provider, model string,
	err error,
) {
	var statusError *analysis.HTTPStatusError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusTooManyRequests {
		return
	}
	fields := []slog.Attr{
		slog.String("event", "model_provider_rate_limited"),
		slog.String("operation", "correlation"),
		slog.Int64("job_id", job.ID),
		slog.String("collection", job.CollectionID),
		slog.Int("chunk", request.Chunk),
		slog.Int("chunks", request.Chunks),
		slog.String("provider", provider),
		slog.Int("http_status", http.StatusTooManyRequests),
	}
	fields = appendOptionalLogField(fields, "model", model)
	fields = appendOptionalLogField(fields, "error_code", statusError.Code)
	fields = appendOptionalLogField(fields, "error_type", statusError.Type)
	fields = appendOptionalLogField(fields, "detail", boundedLogDetail(statusError.Message))
	fields = append(fields, slog.String("remediation", providerRateLimitRemediation(statusError)))
	fields = appendOptionalLogField(fields, "request_id", statusError.RequestID)
	fields = appendOptionalLogField(fields, "retry_after", statusError.RetryAfter)
	fields = appendOptionalLogField(
		fields, "rate_limit_remaining_requests", statusError.RateLimitRemainingRequests,
	)
	fields = appendOptionalLogField(
		fields, "rate_limit_remaining_tokens", statusError.RateLimitRemainingTokens,
	)
	fields = appendOptionalLogField(
		fields, "rate_limit_reset_requests", statusError.RateLimitResetRequests,
	)
	fields = appendOptionalLogField(
		fields, "rate_limit_reset_tokens", statusError.RateLimitResetTokens,
	)
	fields = appendTraceFields(ctx, fields)
	worker.logger.LogAttrs(ctx, slog.LevelWarn, "model provider rate limited", fields...)
}

func providerRateLimitRemediation(statusError *analysis.HTTPStatusError) string {
	code := strings.ToLower(statusError.Code)
	errorType := strings.ToLower(statusError.Type)
	if strings.Contains(code, "quota") || strings.Contains(errorType, "quota") {
		return "Check the provider project billing, credits, monthly usage limit, and API key project."
	}
	return "Wait for the provider reset or Retry-After delay, then reduce request concurrency or token volume."
}

func (worker *Worker) logProviderSuccess(
	ctx context.Context,
	job storage.CorrelationJob,
	request correlation.Request,
	provider string,
	result analysis.ProviderResult,
	duration time.Duration,
) {
	fields := []slog.Attr{
		slog.String("event", "model_provider_call_succeeded"),
		slog.String("operation", "correlation"),
		slog.Int64("job_id", job.ID),
		slog.String("collection", job.CollectionID),
		slog.Int("chunk", request.Chunk),
		slog.Int("chunks", request.Chunks),
		slog.String("provider", provider),
		slog.Int64("duration_ms", duration.Milliseconds()),
	}
	fields = appendOptionalLogField(fields, "model", result.Model)
	fields = appendOptionalLogField(fields, "response_model", result.ResponseModel)
	fields = appendOptionalLogField(fields, "response_id", result.ResponseID)
	fields = appendOptionalLogField(fields, "finish_reasons", strings.Join(result.FinishReasons, ","))
	if result.InputTokens != nil {
		fields = append(fields, slog.Int64("input_tokens", *result.InputTokens))
	}
	if result.OutputTokens != nil {
		fields = append(fields, slog.Int64("output_tokens", *result.OutputTokens))
	}
	if result.CachedInputTokens != nil {
		fields = append(fields, slog.Int64("cached_input_tokens", *result.CachedInputTokens))
	}
	if result.ReasoningOutputTokens != nil {
		fields = append(fields, slog.Int64("reasoning_output_tokens", *result.ReasoningOutputTokens))
	}
	fields = appendTraceFields(ctx, fields)
	worker.logger.LogAttrs(ctx, slog.LevelInfo, "model provider call succeeded", fields...)
}

func appendOptionalLogField(fields []slog.Attr, name, value string) []slog.Attr {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return fields
	}
	return append(fields, slog.String(name, value))
}

func boundedLogDetail(value string) string {
	const maxRunes = 500
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return value
}

func appendTraceFields(ctx context.Context, fields []slog.Attr) []slog.Attr {
	return appendSpanContextFields(trace.SpanContextFromContext(ctx), fields)
}

func appendSpanContextFields(
	spanContext trace.SpanContext, fields []slog.Attr,
) []slog.Attr {
	if !spanContext.IsValid() {
		return fields
	}
	return append(fields,
		slog.String("trace_id", spanContext.TraceID().String()),
		slog.String("span_id", spanContext.SpanID().String()),
	)
}

func correlationTelemetryOutcome(
	result analysis.ProviderResult, status, failureKind, errorType string,
) telemetry.AnalysisOutcomeTelemetry {
	return telemetry.AnalysisOutcomeTelemetry{
		Status: status, FailureKind: failureKind, ErrorType: errorType,
		ResponseModel: result.ResponseModel, ResponseID: result.ResponseID,
		FinishReasons: result.FinishReasons,
		InputTokens:   result.InputTokens, OutputTokens: result.OutputTokens,
		CachedInputTokens:     result.CachedInputTokens,
		ReasoningOutputTokens: result.ReasoningOutputTokens,
		InputComponents:       correlationTelemetryContentMeasurements(result.InputComponents),
	}
}

func correlationTelemetryContentMeasurements(
	measurements []analysis.ContentMeasurement,
) []telemetry.ContentMeasurement {
	result := make([]telemetry.ContentMeasurement, len(measurements))
	for index, measurement := range measurements {
		result[index] = telemetry.ContentMeasurement{
			Name: measurement.Name, Bytes: measurement.Bytes, Items: measurement.Items,
		}
	}
	return result
}

func correlationOutputMeasurements(result correlation.Result) []telemetry.ContentMeasurement {
	groupItems := int64(len(result.Groups))
	groupBytes, edgeBytes := 0, 0
	edgeItems := int64(0)
	for _, group := range result.Groups {
		groupBytes += len(group.Name) + len(group.Description)
		edgeItems += int64(len(group.Edges))
		for _, edge := range group.Edges {
			edgeBytes += len(edge.Reason)
		}
	}
	return []telemetry.ContentMeasurement{
		{Name: "groups", Bytes: int64(groupBytes), Items: &groupItems},
		{Name: "edges", Bytes: int64(edgeBytes), Items: &edgeItems},
	}
}
