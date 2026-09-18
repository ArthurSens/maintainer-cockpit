package correlationworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/ArthurSens/maintainer-cockpit/internal/analysis"
	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/correlation"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

type fakeProvider struct {
	err   error
	name  string
	calls int
	hook  func()
}

func (provider *fakeProvider) Correlate(
	_ context.Context, request correlation.Request,
) (analysis.ProviderResult, error) {
	provider.calls++
	if provider.hook != nil {
		provider.hook()
	}
	if provider.err != nil {
		return analysis.ProviderResult{Model: provider.name}, provider.err
	}
	first := request.PullRequests[0].SourceID
	second := request.PullRequests[1].SourceID
	output := fmt.Sprintf(`{"groups":[{
		"name":"Current feature","description":"Current correlated work.",
		"confidence":"high","sourceIDs":[%q,%q],"members":[%q,%q],
		"edges":[{
			"from":%q,"to":%q,"type":"related","confidence":"medium",
			"reason":"Both changes deliver the same feature.","sourceIDs":[%q,%q]
		}]
	}]}`, first, second, first, second, first, second, first, second)
	return analysis.ProviderResult{
		Model: provider.name, Output: []byte(output),
		RequestBody: []byte(`{"request":true}`), ResponseBody: []byte(`{"response":true}`),
	}, nil
}

func TestWorkerUsesExplicitFallbackAndOnlyProcessesQueuedCorrelation(t *testing.T) {
	t.Parallel()

	store, now := correlationWorkerStore(t)
	if err := store.ReplaceCorrelationGroups(t.Context(), "acme", []correlation.Group{{
		ID: "stale", Name: "Stale feature", Description: "Old result.", Confidence: "high",
		Members: []correlation.Member{
			{SourceID: "PR_acme_widgets_1", Repository: "acme/widgets", Number: 1, State: "open"},
			{SourceID: "PR_acme_widgets_2", Repository: "acme/widgets", Number: 2, State: "open"},
		},
	}}, storage.CorrelationProvenance{
		Provider: "primary", Model: "old", SchemaVersion: correlation.SchemaVersion,
		PromptVersion: correlation.PromptVersion, CorrelatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	primary := &fakeProvider{name: "primary-model", err: errors.New("offline")}
	fallback := &fakeProvider{name: "fallback-model"}
	current := now
	worker := New(store, map[string]Provider{
		"primary": primary, "fallback": fallback,
	}, Options{Now: func() time.Time { return current }})
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	groups, err := store.ListCorrelationGroups(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Name != "Current feature" ||
		groups[0].Provenance.FallbackFrom != "primary" {
		t.Errorf("groups = %+v", groups)
	}
	if primary.calls != 1 || fallback.calls != 1 {
		t.Errorf("provider calls = %d, %d", primary.calls, fallback.calls)
	}
	current = now.Add(23 * time.Hour)
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if primary.calls != 1 || fallback.calls != 1 {
		t.Errorf("providers ran before interval: %d, %d", primary.calls, fallback.calls)
	}
	current = now.Add(24 * time.Hour)
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if primary.calls != 1 || fallback.calls != 1 {
		t.Errorf("worker scheduled providers itself: %d, %d", primary.calls, fallback.calls)
	}
	if _, _, err := store.EnqueueCollectionCorrelation(t.Context(), "acme", false, current); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if primary.calls != 2 || fallback.calls != 2 {
		t.Errorf("providers did not run for queued correlation: %d, %d", primary.calls, fallback.calls)
	}
	analysisJob, err := store.ClaimAnalysis(t.Context(), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if analysisJob != nil {
		t.Errorf("correlation unexpectedly enqueued analysis: %+v", analysisJob)
	}
}

func TestWorkerLogsEverySuccessfulProviderCall(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	worker := New(nil, nil, Options{
		Logger: slog.New(slog.NewTextHandler(&output, nil)),
	})
	inputTokens, outputTokens := int64(250), int64(42)
	cachedTokens, reasoningTokens := int64(180), int64(12)
	worker.logProviderSuccess(
		testTraceContext(),
		storage.CorrelationJob{ID: 11, CollectionID: "acme"},
		correlation.Request{Chunk: 2, Chunks: 3},
		"openai",
		analysis.ProviderResult{
			Model: "gpt-5-mini", ResponseModel: "gpt-5-mini-2026-08-01",
			ResponseID: "chatcmpl-456", FinishReasons: []string{"stop"},
			InputTokens: &inputTokens, OutputTokens: &outputTokens,
			CachedInputTokens: &cachedTokens, ReasoningOutputTokens: &reasoningTokens,
		},
		875*time.Millisecond,
	)

	logs := loggedLines(&output)
	if len(logs) != 1 {
		t.Fatalf("logs = %#v, want one success event", logs)
	}
	for _, field := range []string{
		`event="model_provider_call_succeeded"`, `operation="correlation"`,
		`job_id=11`, `collection="acme"`, `chunk=2`, `chunks=3`,
		`provider="openai"`, `model="gpt-5-mini"`,
		`response_model="gpt-5-mini-2026-08-01"`, `response_id="chatcmpl-456"`,
		`finish_reasons="stop"`, `input_tokens=250`, `output_tokens=42`,
		`cached_input_tokens=180`, `reasoning_output_tokens=12`,
		`duration_ms=875`,
		`trace_id="4bf92f3577b34da6a3ce929d0e0e4736"`,
		`span_id="00f067aa0ba902b7"`,
	} {
		if !containsLogField(logs[0], field) {
			t.Errorf("log %q missing %q", logs[0], field)
		}
	}
}

func TestWorkerAddsTraceContextToFailureLogs(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	worker := New(nil, nil, Options{
		Logger: slog.New(slog.NewTextHandler(&output, nil)),
	})
	worker.logCorrelationFailure(
		trace.SpanContextFromContext(testTraceContext()),
		storage.CorrelationJob{ID: 11, CollectionID: "acme"},
		"openai",
		errors.New("provider unavailable"),
	)

	logs := loggedLines(&output)
	if len(logs) != 1 {
		t.Fatalf("logs = %#v, want one failure event", logs)
	}
	for _, field := range []string{
		`event="feature_correlation_failed"`, `job_id=11`,
		`collection="acme"`, `provider="openai"`,
		`detail="provider unavailable"`,
		`trace_id="4bf92f3577b34da6a3ce929d0e0e4736"`,
		`span_id="00f067aa0ba902b7"`,
	} {
		if !containsLogField(logs[0], field) {
			t.Errorf("log %q missing %q", logs[0], field)
		}
	}
}

func TestWorkerLogsActionableProviderRateLimit(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	worker := New(nil, nil, Options{
		Logger: slog.New(slog.NewTextHandler(&output, nil)),
	})
	worker.logProviderFailure(
		testTraceContext(),
		storage.CorrelationJob{ID: 11, CollectionID: "acme"},
		correlation.Request{Chunk: 2, Chunks: 3},
		"openai",
		"gpt-5-mini",
		&analysis.HTTPStatusError{
			StatusCode:                 http.StatusTooManyRequests,
			Code:                       "rate_limit_exceeded",
			Type:                       "tokens",
			Message:                    "Rate limit reached for tokens per minute.",
			RequestID:                  "req_123",
			RetryAfter:                 "17",
			RateLimitRemainingRequests: "48",
			RateLimitRemainingTokens:   "0",
			RateLimitResetRequests:     "1s",
			RateLimitResetTokens:       "2m15s",
		},
	)

	logs := loggedLines(&output)
	if len(logs) != 1 {
		t.Fatalf("logs = %#v, want one rate-limit event", logs)
	}
	for _, field := range []string{
		`event="model_provider_rate_limited"`, `operation="correlation"`,
		`job_id=11`, `collection="acme"`, `chunk=2`, `chunks=3`,
		`provider="openai"`, `model="gpt-5-mini"`, `http_status=429`,
		`error_code="rate_limit_exceeded"`, `error_type="tokens"`,
		`detail="Rate limit reached for tokens per minute."`,
		`request_id="req_123"`, `retry_after="17"`,
		`rate_limit_remaining_requests="48"`, `rate_limit_remaining_tokens="0"`,
		`rate_limit_reset_requests="1s"`, `rate_limit_reset_tokens="2m15s"`,
		`trace_id="4bf92f3577b34da6a3ce929d0e0e4736"`,
		`span_id="00f067aa0ba902b7"`,
	} {
		if !containsLogField(logs[0], field) {
			t.Errorf("log %q missing %q", logs[0], field)
		}
	}
}

func loggedLines(output *bytes.Buffer) []string {
	return strings.Split(strings.TrimSpace(output.String()), "\n")
}

func containsLogField(line, field string) bool {
	if strings.Contains(line, field) {
		return true
	}
	name, quoted, ok := strings.Cut(field, "=")
	value, err := strconv.Unquote(quoted)
	return ok && err == nil && strings.Contains(line, name+"="+value)
}

func testTraceContext() context.Context {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(
		trace.SpanContextConfig{TraceID: traceID, SpanID: spanID},
	))
}

func TestWorkerPreservesLastSuccessWhenOutputIsMalformed(t *testing.T) {
	t.Parallel()

	store, now := correlationWorkerStore(t)
	if err := store.ReplaceCorrelationGroups(t.Context(), "acme", []correlation.Group{{
		ID: "existing", Name: "Existing feature", Description: "Keep me.", Confidence: "high",
		Members: []correlation.Member{
			{SourceID: "PR_acme_widgets_1", Repository: "acme/widgets", Number: 1, State: "open"},
			{SourceID: "PR_acme_widgets_2", Repository: "acme/widgets", Number: 2, State: "open"},
		},
	}}, storage.CorrelationProvenance{
		Provider: "primary", Model: "model", SchemaVersion: correlation.SchemaVersion,
		PromptVersion: correlation.PromptVersion, CorrelatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	provider := &malformedProvider{}
	worker := New(store, map[string]Provider{"primary": provider}, Options{
		Now: func() time.Time { return now },
	})
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	groups, err := store.ListCorrelationGroups(t.Context(), "acme")
	if err != nil || len(groups) != 1 || groups[0].Name != "Existing feature" {
		t.Errorf("groups = %+v, %v", groups, err)
	}
	status, err := store.CorrelationStatus(t.Context(), "acme")
	if err != nil || status.State != "failed" {
		t.Errorf("status = %+v, %v", status, err)
	}
}

func TestWorkerDiscardsStaleResponseAndRequeuesCorrelation(t *testing.T) {
	t.Parallel()

	store, now := correlationWorkerStore(t)
	provider := &fakeProvider{name: "model"}
	provider.hook = func() {
		provider.hook = nil
		if err := store.UpsertPullRequest(t.Context(), "acme", storage.PullRequest{
			Repository: "acme/widgets", Number: 1, Title: "Changed while correlating",
			UpdatedAt: now.Add(time.Minute), URL: "https://github.com/acme/widgets/pull/1",
		}); err != nil {
			t.Error(err)
		}
	}
	worker := New(store, map[string]Provider{"primary": provider}, Options{
		Now: func() time.Time { return now },
	})
	if err := worker.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	groups, err := store.ListCorrelationGroups(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Errorf("stale response published groups: %+v", groups)
	}
	job, err := store.ClaimCorrelation(t.Context(), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Fatal("changed correlation input was not requeued")
	}
}

type malformedProvider struct{}

func (*malformedProvider) Correlate(
	_ context.Context, _ correlation.Request,
) (analysis.ProviderResult, error) {
	return analysis.ProviderResult{
		Model: "model", Output: []byte(`{"groups":[{"members":["invented"]}]}`),
		ResponseBody: []byte(`{"invalid":true}`),
	}, nil
}

func correlationWorkerStore(t *testing.T) (*storage.Store, time.Time) {
	t.Helper()
	now := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)
	store, err := storage.Open(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	interval := config.FeatureCorrelation{Schedule: "0 4 * * *"}
	if err := store.SyncCollectionsAt(t.Context(), []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
		ModelProvider: "primary", ModelFallbackProvider: "fallback",
		FeatureCorrelation: &interval,
	}}, now); err != nil {
		t.Fatal(err)
	}
	for number := 1; number <= 2; number++ {
		if err := store.UpsertPullRequest(t.Context(), "acme", storage.PullRequest{
			Repository: "acme/widgets", Number: number, Title: fmt.Sprintf("PR %d", number),
			UpdatedAt: now, URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for {
		job, err := store.ClaimAnalysis(t.Context(), now)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			break
		}
		if err := store.CompleteAnalysisJob(t.Context(), job.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.EnqueueCollectionCorrelation(t.Context(), "acme", false, now); err != nil {
		t.Fatal(err)
	}
	return store, now
}
