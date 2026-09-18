// Package analysisworker processes durable model-analysis jobs.
package analysisworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/ArthurSens/maintainer-cockpit/internal/analysis"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
	"github.com/ArthurSens/maintainer-cockpit/internal/telemetry"
)

type Options struct {
	PollInterval time.Duration
	Now          func() time.Time
	Logger       *slog.Logger
}

const provider429LogWindow = 5 * time.Minute

type provider429State struct {
	lastEmitted time.Time
	suppressed  int
}

type Worker struct {
	store        *storage.Store
	providers    map[string]analysis.Provider
	pollInterval time.Duration
	now          func() time.Time
	logger       *slog.Logger
	logMu        sync.Mutex
	provider429  map[string]provider429State
}

func New(store *storage.Store, providers map[string]analysis.Provider, options Options) *Worker {
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
		logger: options.Logger, provider429: make(map[string]provider429State),
	}
}

func (worker *Worker) Run(ctx context.Context) error {
	if err := worker.store.RecoverAnalysisJobs(ctx); err != nil {
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

// RunOnce processes one due job. Provider and schema failures are persisted as
// analysis health and do not fail the factual application.
func (worker *Worker) RunOnce(ctx context.Context) error {
	now := worker.now().UTC()
	job, err := worker.store.ClaimAnalysis(ctx, now)
	if err != nil || job == nil {
		return err
	}
	if err := worker.process(ctx, *job, now); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, storage.ErrAnalysisSuperseded) {
			return worker.store.CompleteAnalysisJob(ctx, job.ID, worker.now().UTC())
		}
		worker.logger.Error("analysis job processing failed",
			"event", "analysis_job_processing_failed",
			"job_id", job.ID,
			"detail", boundedLogDetail(err.Error()),
		)
		if requeueErr := worker.store.RequeueAnalysisJob(
			ctx, job.ID, worker.now().UTC().Add(worker.pollInterval),
		); requeueErr != nil {
			return fmt.Errorf("process analysis job: %w; requeue: %w", err, requeueErr)
		}
		return nil
	}
	return worker.store.CompleteAnalysisJob(ctx, job.ID, worker.now().UTC())
}

func (worker *Worker) process(ctx context.Context, job storage.AnalysisJob, now time.Time) error {
	request, err := worker.store.BuildAnalysisRequest(ctx, job)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		recordErr := worker.store.RecordAnalysisBuildFailure(
			ctx, job, now, "Analysis input could not be built; prior valid analysis is preserved.",
		)
		if recordErr != nil {
			return fmt.Errorf(
				"build analysis request for %s#%d: %w; record failure: %w",
				job.Repository, job.Number, err, recordErr,
			)
		}
		return nil
	}
	if !job.Forced {
		current, err := worker.store.AnalysisIsCurrent(ctx, job, request.InputRevision)
		if err != nil {
			return err
		}
		if current {
			return nil
		}
	}
	providerNames := []string{job.PrimaryProvider}
	if job.FallbackProvider != "" {
		providerNames = append(providerNames, job.FallbackProvider)
	}
	var lastProvider, lastModel string
	var lastRequest, lastResponse []byte
	var lastErr error
	var lastFailureKind string
	var lastAttemptSpanContext trace.SpanContext
	for _, providerName := range providerNames {
		provider := worker.providers[providerName]
		requestTelemetry := telemetry.AnalysisRequestTelemetry{
			Collection: job.CollectionID, ProviderProfile: providerName,
			Workload: "analysis", PromptVersion: request.PromptVersion,
			SchemaVersion: request.SchemaVersion, JobID: job.ID,
			Repository: job.Repository, PullRequest: job.Number,
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
		attemptContext, finishAttempt := telemetry.StartAnalysis(
			ctx, requestTelemetry,
		)
		lastAttemptSpanContext = trace.SpanContextFromContext(attemptContext)
		if provider == nil {
			lastProvider = providerName
			lastErr = fmt.Errorf("configured provider %q is unavailable", providerName)
			lastFailureKind = "provider"
			finishAttempt(telemetry.AnalysisOutcomeTelemetry{
				Status: "failed", FailureKind: "provider", ErrorType: "provider_error",
			})
			continue
		}
		started := time.Now()
		providerResult, providerErr := provider.Analyze(attemptContext, request)
		duration := time.Since(started)
		lastProvider, lastModel = providerName, providerResult.Model
		lastRequest, lastResponse = providerResult.RequestBody, providerResult.ResponseBody
		if providerErr != nil {
			lastErr = providerErr
			lastFailureKind = "provider"
			worker.logProviderFailure(
				attemptContext, job, providerName, providerResult.Model, providerErr,
			)
			finishAttempt(providerTelemetryOutcome(
				providerResult, "failed", "provider", "provider_error",
			))
			continue
		}
		worker.logProviderSuccess(attemptContext, job, providerName, providerResult, duration)
		response, validationErr := analysis.ValidateResponse(
			providerResult.Output, storage.KnownSourceIDs(request),
		)
		if validationErr != nil {
			lastErr = validationErr
			lastFailureKind = "schema"
			if response.HasValidSections() {
				fallbackFrom := ""
				if providerName != job.PrimaryProvider {
					fallbackFrom = job.PrimaryProvider
				}
				err := worker.store.RecordAnalysisSections(
					ctx, job, request, response, providerName, fallbackFrom,
					providerResult.Model, providerResult.RequestBody, providerResult.ResponseBody,
					now, "partial", "Some model analysis sections were invalid; valid sections were retained.",
				)
				outcome := providerTelemetryOutcome(
					providerResult, "partial", "schema", "schema_validation_error",
				)
				outcome.OutputComponents = analysisOutputMeasurements(response)
				finishAttempt(outcome)
				return err
			}
			finishAttempt(providerTelemetryOutcome(
				providerResult, "unknown", "schema", "schema_validation_error",
			))
			continue
		}
		fallbackFrom := ""
		if providerName != job.PrimaryProvider {
			fallbackFrom = job.PrimaryProvider
		}
		err := worker.store.RecordAnalysisSections(
			ctx, job, request, response, providerName, fallbackFrom, providerResult.Model,
			providerResult.RequestBody, providerResult.ResponseBody, now, "complete", "",
		)
		if err != nil {
			finishAttempt(providerTelemetryOutcome(
				providerResult, "failed", "", "storage_error",
			))
			return err
		}
		outcome := providerTelemetryOutcome(providerResult, "complete", "", "")
		outcome.OutputComponents = analysisOutputMeasurements(response)
		finishAttempt(outcome)
		return nil
	}
	var statusError *analysis.HTTPStatusError
	if !errors.As(lastErr, &statusError) || statusError.StatusCode != http.StatusTooManyRequests {
		worker.logAnalysisFailure(
			lastAttemptSpanContext, job, lastProvider, lastFailureKind == "schema", lastErr,
		)
	}
	status := "failed"
	message := "Model provider unavailable; factual pull request data remains available."
	if lastFailureKind == "schema" {
		status = "unknown"
		message = "Model response was invalid; analysis is unknown."
	}
	return worker.store.RecordAnalysisFailure(
		ctx, job, request, lastProvider, lastModel, lastRequest, lastResponse,
		now, status, message,
	)
}

func (worker *Worker) logProviderFailure(
	ctx context.Context, job storage.AnalysisJob, provider, model string, err error,
) {
	var statusError *analysis.HTTPStatusError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusTooManyRequests {
		return
	}
	now := worker.now().UTC()
	worker.logMu.Lock()
	defer worker.logMu.Unlock()
	state, exists := worker.provider429[provider]
	if exists && now.Sub(state.lastEmitted) < provider429LogWindow {
		state.suppressed++
		worker.provider429[provider] = state
		return
	}
	fields := analysisLogIdentity(job)
	fields = append(fields,
		slog.String("provider", provider),
		slog.Int("http_status", http.StatusTooManyRequests),
	)
	fields = appendOptionalLogField(fields, "model", model)
	fields = appendOptionalLogField(fields, "error_code", statusError.Code)
	fields = appendOptionalLogField(fields, "error_type", statusError.Type)
	fields = appendOptionalLogField(fields, "detail", statusError.Message)
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
	if exists && state.suppressed > 0 {
		fields = append(fields, slog.Int("suppressed_count", state.suppressed))
	}
	fields = appendTraceFields(ctx, fields)
	worker.logger.LogAttrs(ctx, slog.LevelWarn, "model provider rate limited",
		append([]slog.Attr{slog.String("event", "model_provider_rate_limited")}, fields...)...)
	worker.provider429[provider] = provider429State{lastEmitted: now}
}

func (worker *Worker) logProviderSuccess(
	ctx context.Context,
	job storage.AnalysisJob,
	provider string,
	result analysis.ProviderResult,
	duration time.Duration,
) {
	fields := analysisLogIdentity(job)
	fields = append(fields,
		slog.String("operation", "analysis"),
		slog.String("provider", provider),
		slog.Int64("duration_ms", duration.Milliseconds()),
	)
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
	worker.logger.LogAttrs(ctx, slog.LevelInfo, "model provider call succeeded",
		append([]slog.Attr{slog.String("event", "model_provider_call_succeeded")}, fields...)...)
}

func providerRateLimitRemediation(statusError *analysis.HTTPStatusError) string {
	code := strings.ToLower(statusError.Code)
	errorType := strings.ToLower(statusError.Type)
	if strings.Contains(code, "quota") || strings.Contains(errorType, "quota") {
		return "Check the provider project billing, credits, monthly usage limit, and API key project."
	}
	return "Wait for the provider reset or Retry-After delay, then reduce request concurrency or token volume."
}

func appendOptionalLogField(fields []slog.Attr, name, value string) []slog.Attr {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return fields
	}
	return append(fields, slog.String(name, value))
}

func (worker *Worker) logAnalysisFailure(
	spanContext trace.SpanContext,
	job storage.AnalysisJob,
	provider string,
	invalidResponse bool,
	err error,
) {
	category := "provider"
	if invalidResponse {
		category = "schema"
	}
	fields := analysisLogIdentity(job)
	fields = append(fields,
		slog.String("provider", provider),
		slog.String("category", category),
	)
	if err != nil {
		fields = appendOptionalLogField(fields, "detail", boundedLogDetail(err.Error()))
	}
	fields = appendSpanContextFields(spanContext, fields)
	logContext := trace.ContextWithSpanContext(context.Background(), spanContext)
	worker.logger.LogAttrs(logContext, slog.LevelError, "model analysis failed",
		append([]slog.Attr{slog.String("event", "model_analysis_failed")}, fields...)...)
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

func analysisLogIdentity(job storage.AnalysisJob) []slog.Attr {
	return []slog.Attr{
		slog.Int64("job_id", job.ID),
		slog.String("repository", job.Repository),
		slog.Int("pull_request", job.Number),
		slog.String("collection", job.CollectionID),
	}
}

func providerTelemetryOutcome(
	result analysis.ProviderResult, status, failureKind, errorType string,
) telemetry.AnalysisOutcomeTelemetry {
	return telemetry.AnalysisOutcomeTelemetry{
		Status: status, FailureKind: failureKind, ErrorType: errorType,
		ResponseModel: result.ResponseModel, ResponseID: result.ResponseID,
		FinishReasons: result.FinishReasons,
		InputTokens:   result.InputTokens, OutputTokens: result.OutputTokens,
		CachedInputTokens:     result.CachedInputTokens,
		ReasoningOutputTokens: result.ReasoningOutputTokens,
		InputComponents:       telemetryContentMeasurements(result.InputComponents),
	}
}

func telemetryContentMeasurements(
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

func analysisOutputMeasurements(response analysis.ProviderResponse) []telemetry.ContentMeasurement {
	var result []telemetry.ContentMeasurement
	if response.SectionValid(analysis.SectionReviewCognitiveLoad) {
		items := int64(5)
		load := response.ReviewCognitiveLoad
		bytes := len(load.Rationale) +
			len(load.ChangeScope.Reason) +
			len(load.RequiredContext.Reason) +
			len(load.ConceptualComplexity.Reason) +
			len(load.ReviewRisk.Reason)
		result = append(result, telemetry.ContentMeasurement{
			Name: "review_cognitive_load", Bytes: int64(bytes), Items: &items,
		})
	}
	if response.SectionValid(analysis.SectionQualityEvaluations) {
		items := int64(len(response.QualityEvaluations))
		bytes := 0
		for _, evaluation := range response.QualityEvaluations {
			bytes += len(evaluation.Reason)
			if evaluation.Finding != nil {
				bytes += len(evaluation.Finding.Summary)
			}
		}
		result = append(result, telemetry.ContentMeasurement{
			Name: "evaluations", Bytes: int64(bytes), Items: &items,
		})
	}
	if response.SectionValid(analysis.SectionWaitingOn) {
		items := int64(len(response.WaitingOn))
		bytes := 0
		for _, waiting := range response.WaitingOn {
			bytes += len(waiting.Reason)
		}
		result = append(result, telemetry.ContentMeasurement{
			Name: "waiting_states", Bytes: int64(bytes), Items: &items,
		})
	}
	return result
}
