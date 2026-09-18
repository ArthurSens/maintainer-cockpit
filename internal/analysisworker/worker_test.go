package analysisworker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/ArthurSens/maintainer-cockpit/internal/analysis"
	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	listquery "github.com/ArthurSens/maintainer-cockpit/internal/query"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func TestWorkerPersistsValidatedAnalysisUsesExplicitFallbackAndCachesByInputRevision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
		ModelProvider: "primary", ModelFallbackProvider: "fallback",
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if err := store.ReplaceRepositorySnapshot(ctx, "acme/widgets", []storage.PullRequest{{
		Repository: "acme/widgets", Number: 7, Title: "Change parser",
		Body: "Updates parser behavior.", Additions: 200, Deletions: 5,
		Files:     []storage.PullRequestFile{{Path: "main.go", Additions: 120, Deletions: 5}},
		UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/7",
		Context: &storage.PullRequestContext{
			Completeness: "complete", CollectedAt: now,
			Relationships: []storage.Relationship{{
				Kind: "review", SourceID: "review-1", Title: "CHANGES_REQUESTED",
				Text: "Please cover the public parser behavior.", URL: "https://github.com/acme/widgets/pull/7#review-1",
				Timestamp: now,
			}},
			ExternalURLs: []storage.ExternalURL{{
				URL: "https://example.com/do-not-fetch", SourceKind: "review",
				SourceID: "review-1", DiscoveredAt: now,
			}},
		},
	}}, now); err != nil {
		t.Fatal(err)
	}
	primary := &fakeProvider{err: errors.New("primary unavailable")}
	fallback := &fakeProvider{model: "fallback-model"}
	worker := New(store, map[string]analysis.Provider{
		"primary": primary, "fallback": fallback,
	}, Options{Now: func() time.Time { return now }})
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetPullRequestDetail(ctx, "acme", "acme/widgets", 7)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Analysis.Status != "complete" ||
		detail.Analysis.Provider != "fallback" ||
		detail.Analysis.FallbackFrom != "primary" ||
		detail.Analysis.ReviewCognitiveLoad == nil ||
		detail.Analysis.ReviewCognitiveLoad.Overall != analysis.LevelMedium ||
		len(detail.Analysis.WaitingOn) != 1 ||
		len(detail.Analysis.QualityEvaluations) != 5 ||
		detail.Quality.Level != "review_suggested" ||
		len(detail.Quality.Findings) != 1 ||
		detail.Quality.Findings[0].Rule != "missing_expected_tests" ||
		detail.Quality.Findings[0].Provenance != "model_inferred" ||
		detail.Analysis.PayloadID == "" {
		t.Fatalf("detail = %+v", detail)
	}
	foundReviewText := false
	for _, source := range fallback.request.Sources {
		foundReviewText = foundReviewText || strings.Contains(source.Text, "parser behavior")
	}
	if !foundReviewText {
		t.Errorf("bounded request sources = %+v, want collected review text", fallback.request.Sources)
	}
	requestJSON, err := json.Marshal(fallback.request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requestJSON), "do-not-fetch") {
		t.Fatal("model request included external URL metadata")
	}
	page, err := store.ListPullRequestsPage(ctx, "acme", listquery.Options{
		ReviewLoad: "medium", WaitingOn: []string{"author", "maintainer"},
		WaitingMode: "any", Sort: "review_load", Order: "desc", Limit: 50,
	})
	if err != nil || page.Matched != 1 {
		t.Fatalf("analysis any filter page = %+v, %v", page, err)
	}
	page, err = store.ListPullRequestsPage(ctx, "acme", listquery.Options{
		WaitingOn:   []string{"author", "maintainer"},
		WaitingMode: "all", Sort: "updated", Order: "desc", Limit: 50,
	})
	if err != nil || page.Matched != 0 {
		t.Fatalf("analysis all filter page = %+v, %v", page, err)
	}

	enqueued, _, err := store.EnqueueCollectionAnalysis(ctx, "acme", false, now)
	if err != nil || enqueued != 1 {
		t.Fatalf("EnqueueCollectionAnalysis() = %d, %v", enqueued, err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fallback.calls != 1 {
		t.Errorf("fallback provider calls = %d, want cached result to avoid second call", fallback.calls)
	}
}

func TestWorkerMakesMalformedOutputUnknownWithoutBreakingFactualData(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
		ModelProvider: "primary",
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.ReplaceRepositorySnapshot(ctx, "acme/widgets", []storage.PullRequest{{
		Repository: "acme/widgets", Number: 7, Title: "Still factual",
		Body:      "Rewrites cmd/serve/main.go, internal/auth/session.go, and internal/auth/token.go.",
		Additions: 5, Deletions: 1, ChangedFiles: 1,
		Files:     []storage.PullRequestFile{{Path: "docs/README.md", Additions: 5, Deletions: 1}},
		UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/7",
	}}, now); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{output: `{"requiredContext":{"level":"invented"}}`}
	worker := New(store, map[string]analysis.Provider{"primary": provider}, Options{
		Now: func() time.Time { return now.Add(time.Minute) },
	})
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetPullRequestDetail(ctx, "acme", "acme/widgets", 7)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Title != "Still factual" || detail.Analysis.Status != "unknown" ||
		detail.Analysis.ReviewCognitiveLoad != nil ||
		detail.Quality.Level != "strong_concerns" ||
		len(detail.Quality.Findings) != 1 ||
		detail.Quality.Findings[0].Provenance != "rule_derived" ||
		!strings.Contains(detail.Analysis.Error, "invalid") {
		t.Errorf("detail = %+v", detail)
	}
}

func TestWorkerPreservesLastValidLoadAfterProviderFailure(t *testing.T) {
	t.Parallel()

	ctx, store, now := analysisWorkerTestStore(t, 1)
	provider := &fakeProvider{errs: []error{nil, errors.New("provider unavailable")}}
	worker := New(store, map[string]analysis.Provider{"primary": provider}, Options{
		Now: func() time.Time { return now },
	})
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueCollectionAnalysis(ctx, "acme", true, now); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetPullRequestDetail(ctx, "acme", "acme/widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Analysis.Status != "failed" || detail.Analysis.ReviewCognitiveLoad == nil ||
		detail.Analysis.ReviewCognitiveLoad.Overall != analysis.LevelMedium ||
		!strings.Contains(detail.Analysis.Error, "unavailable") {
		t.Fatalf("analysis after provider failure = %+v", detail.Analysis)
	}
	payload, err := store.RetainedModelPayload(ctx, detail.Analysis.PayloadID)
	if err != nil {
		t.Fatal(err)
	}
	if payload == nil || string(payload.Request) != `{"request":true}` ||
		string(payload.Response) != `{"providerError":true}` {
		t.Fatalf("retained provider failure payload = %+v", payload)
	}
}

func TestWorkerMergesPartialSectionsAndLeavesAnalysisRetryable(t *testing.T) {
	t.Parallel()

	ctx, store, now := analysisWorkerTestStore(t, 1)
	valid := (&fakeProvider{}).response(analysis.Request{
		PullRequest: analysis.RequestPullRequest{SourceID: "PR_acme_widgets_1"},
	})
	partial := strings.Replace(
		valid,
		`"overall":"medium"`,
		`"overall":"high"`,
		1,
	)
	partial = strings.Replace(
		partial,
		`"family":"unsupported_references"`,
		`"family":"invented_quality_family"`,
		1,
	)
	provider := &fakeProvider{outputs: []string{valid, partial}}
	worker := New(store, map[string]analysis.Provider{"primary": provider}, Options{
		Now: func() time.Time { return now },
	})
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueCollectionAnalysis(ctx, "acme", true, now); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetPullRequestDetail(ctx, "acme", "acme/widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Analysis.Status != "partial" || detail.Analysis.ReviewCognitiveLoad == nil ||
		detail.Analysis.ReviewCognitiveLoad.Overall != analysis.LevelHigh ||
		len(detail.Analysis.QualityEvaluations) != len(analysis.ModelQualityFamilies) {
		t.Fatalf("partial merged analysis = %+v", detail.Analysis)
	}
	job := storage.AnalysisJob{
		CollectionID: "acme", Repository: "acme/widgets", Number: 1,
		PrimaryProvider: "primary",
	}
	request, err := store.BuildAnalysisRequest(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.AnalysisIsCurrent(ctx, job, request.InputRevision)
	if err != nil || current {
		t.Fatalf("AnalysisIsCurrent() = %t, %v, want retryable partial", current, err)
	}
}

func TestWorkerContinuesAfterBadJob(t *testing.T) {
	t.Parallel()

	ctx, store, now := analysisWorkerTestStore(t, 2)
	provider := &fakeProvider{outputs: []string{
		`{"reviewCognitiveLoad":{"overall":"invented"}}`,
		"",
	}}
	worker := New(store, map[string]analysis.Provider{"primary": provider}, Options{
		Now: func() time.Time { return now },
	})
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := store.GetPullRequestDetail(ctx, "acme", "acme/widgets", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.GetPullRequestDetail(ctx, "acme", "acme/widgets", 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Analysis.Status != "unknown" || second.Analysis.Status != "complete" ||
		second.Analysis.ReviewCognitiveLoad == nil {
		t.Fatalf("first = %+v, second = %+v", first.Analysis, second.Analysis)
	}
}

func TestWorkerRequeuesTransientPersistenceFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	store, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
		ModelProvider: "primary",
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	if err := store.ReplaceRepositorySnapshot(ctx, "acme/widgets", []storage.PullRequest{{
		Repository: "acme/widgets", Number: 7, Title: "Change parser",
		UpdatedAt: now, URL: "https://github.com/acme/widgets/pull/7",
	}}, now); err != nil {
		t.Fatal(err)
	}
	worker := New(store, map[string]analysis.Provider{
		"primary": &fakeProvider{},
	}, Options{Now: func() time.Time { return now }, PollInterval: time.Second})
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueCollectionAnalysis(ctx, "acme", true, now); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		UPDATE analysis_results SET result_json = '{'
		WHERE collection_id = 'acme' AND repository = 'acme/widgets' AND number = 7
	`); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() error = %v, want recovered transient failure", err)
	}
	var status string
	if err := db.QueryRow(`
		SELECT status FROM analysis_jobs
		WHERE collection_id = 'acme' AND repository = 'acme/widgets' AND number = 7
		ORDER BY id DESC LIMIT 1
	`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("job status = %q, want queued for retry", status)
	}
}

func analysisWorkerTestStore(
	t *testing.T, pullRequests int,
) (context.Context, *storage.Store, time.Time) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncCollections(ctx, []config.Collection{{
		ID: "acme", Name: "Acme", Repositories: []string{"acme/widgets"},
		ModelProvider: "primary",
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	prs := make([]storage.PullRequest, 0, pullRequests)
	for number := 1; number <= pullRequests; number++ {
		prs = append(prs, storage.PullRequest{
			Repository: "acme/widgets", Number: number,
			Title: "Change parser " + strconv.Itoa(number),
			Body:  "Updates parser behavior.", UpdatedAt: now,
			URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(number),
		})
	}
	if err := store.ReplaceRepositorySnapshot(ctx, "acme/widgets", prs, now); err != nil {
		t.Fatal(err)
	}
	return ctx, store, now
}

func TestWorkerAggregatesProviderRateLimitLogsPerProvider(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	worker := New(nil, nil, Options{
		Now:    func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(&output, nil)),
	})
	job := storage.AnalysisJob{
		ID: 9, CollectionID: "acme", Repository: "acme/widgets", Number: 7,
	}
	rateLimited := &analysis.HTTPStatusError{
		StatusCode: 429, Code: "rate_limit_exceeded", Type: "tokens",
		Message:   "Rate limit reached for tokens per minute.",
		RequestID: "req_123", RetryAfter: "17",
		RateLimitRemainingRequests: "48", RateLimitRemainingTokens: "0",
		RateLimitResetRequests: "1s", RateLimitResetTokens: "2m15s",
	}
	traceContext := testTraceContext()
	worker.logProviderFailure(traceContext, job, "openai primary", "gpt-5-mini", rateLimited)
	worker.logProviderFailure(traceContext, job, "openai primary", "gpt-5-mini", rateLimited)
	worker.logProviderFailure(traceContext, job, "openai primary", "gpt-5-mini", rateLimited)
	logs := loggedLines(&output)
	if len(logs) != 1 {
		t.Fatalf("initial logs = %#v, want first event only", logs)
	}
	for _, field := range []string{
		`event="model_provider_rate_limited"`, `provider="openai primary"`,
		`model="gpt-5-mini"`,
		`http_status=429`, `job_id=9`, `repository="acme/widgets"`,
		`pull_request=7`, `collection="acme"`,
		`error_code="rate_limit_exceeded"`, `error_type="tokens"`,
		`detail="Rate limit reached for tokens per minute."`,
		`remediation="Wait for the provider reset or Retry-After delay, then reduce request concurrency or token volume."`,
		`request_id="req_123"`, `retry_after="17"`,
		`rate_limit_remaining_requests="48"`, `rate_limit_remaining_tokens="0"`,
		`rate_limit_reset_requests="1s"`, `rate_limit_reset_tokens="2m15s"`,
		`trace_id="4bf92f3577b34da6a3ce929d0e0e4736"`,
		`span_id="00f067aa0ba902b7"`,
	} {
		if !containsLogField(logs[0], field) {
			t.Errorf("first log %q missing %q", logs[0], field)
		}
	}
	if strings.Contains(logs[0], "suppressed_count=") {
		t.Errorf("first log unexpectedly reports suppression: %q", logs[0])
	}
	now = now.Add(provider429LogWindow)
	worker.logProviderFailure(traceContext, job, "openai primary", "gpt-5-mini", rateLimited)
	logs = loggedLines(&output)
	if len(logs) != 2 || !strings.Contains(logs[1], "suppressed_count=2") {
		t.Fatalf("aggregated logs = %#v", logs)
	}
	worker.logProviderFailure(traceContext, job, "different", "gpt-5-mini", rateLimited)
	logs = loggedLines(&output)
	if len(logs) != 3 {
		t.Fatalf("per-provider logs = %#v", logs)
	}
}

func TestWorkerLogsQuotaRemediationWithoutControlCharacters(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	worker := New(nil, nil, Options{
		Logger: slog.New(slog.NewTextHandler(&output, nil)),
	})
	worker.logProviderFailure(context.Background(), storage.AnalysisJob{
		ID: 9, CollectionID: "acme", Repository: "acme/widgets", Number: 7,
	}, "openai", "gpt-5-mini", &analysis.HTTPStatusError{
		StatusCode: 429, Code: "insufficient_quota",
		Message: "You exceeded your current quota.\nCheck billing.",
	})

	logs := loggedLines(&output)
	if len(logs) != 1 {
		t.Fatalf("logs = %#v, want one event", logs)
	}
	for _, field := range []string{
		`error_code="insufficient_quota"`,
		`detail="You exceeded your current quota. Check billing."`,
		`remediation="Check the provider project billing, credits, monthly usage limit, and API key project."`,
	} {
		if !containsLogField(logs[0], field) {
			t.Errorf("log %q missing %q", logs[0], field)
		}
	}
	if strings.ContainsRune(logs[0], '\n') {
		t.Errorf("log contains newline: %q", logs[0])
	}
}

func TestWorkerLogsExactSchemaValidationFailure(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	worker := New(nil, nil, Options{
		Logger: slog.New(slog.NewTextHandler(&output, nil)),
	})
	worker.logAnalysisFailure(
		trace.SpanContextFromContext(testTraceContext()),
		storage.AnalysisJob{
			ID: 9, CollectionID: "acme", Repository: "acme/widgets", Number: 7,
		},
		"openai",
		true,
		errors.New("qualityEvaluations[2] finding requires finding data\nand no reason"),
	)

	logs := loggedLines(&output)
	if len(logs) != 1 {
		t.Fatalf("logs = %#v, want one failure event", logs)
	}
	for _, field := range []string{
		`event="model_analysis_failed"`, `category="schema"`,
		`detail="qualityEvaluations[2] finding requires finding data and no reason"`,
		`trace_id="4bf92f3577b34da6a3ce929d0e0e4736"`,
		`span_id="00f067aa0ba902b7"`,
	} {
		if !containsLogField(logs[0], field) {
			t.Errorf("log %q missing %q", logs[0], field)
		}
	}
}

func TestWorkerLogsEverySuccessfulProviderCall(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	worker := New(nil, nil, Options{
		Logger: slog.New(slog.NewTextHandler(&output, nil)),
	})
	inputTokens, outputTokens := int64(120), int64(35)
	cachedTokens, reasoningTokens := int64(80), int64(10)
	worker.logProviderSuccess(testTraceContext(), storage.AnalysisJob{
		ID: 9, CollectionID: "acme", Repository: "acme/widgets", Number: 7,
	}, "openai", analysis.ProviderResult{
		Model: "gpt-5-mini", ResponseModel: "gpt-5-mini-2026-08-01",
		ResponseID: "chatcmpl-123", FinishReasons: []string{"stop"},
		InputTokens: &inputTokens, OutputTokens: &outputTokens,
		CachedInputTokens: &cachedTokens, ReasoningOutputTokens: &reasoningTokens,
	}, 1250*time.Millisecond)

	logs := loggedLines(&output)
	if len(logs) != 1 {
		t.Fatalf("logs = %#v, want one success event", logs)
	}
	for _, field := range []string{
		`event="model_provider_call_succeeded"`, `operation="analysis"`,
		`job_id=9`, `repository="acme/widgets"`, `pull_request=7`,
		`collection="acme"`, `provider="openai"`, `model="gpt-5-mini"`,
		`response_model="gpt-5-mini-2026-08-01"`, `response_id="chatcmpl-123"`,
		`finish_reasons="stop"`, `input_tokens=120`, `output_tokens=35`,
		`cached_input_tokens=80`, `reasoning_output_tokens=10`,
		`duration_ms=1250`,
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

type fakeProvider struct {
	model   string
	output  string
	err     error
	outputs []string
	errs    []error
	calls   int
	request analysis.Request
}

func (provider *fakeProvider) Analyze(_ context.Context, request analysis.Request) (analysis.ProviderResult, error) {
	provider.calls++
	provider.request = request
	call := provider.calls - 1
	providerErr := provider.err
	if call < len(provider.errs) {
		providerErr = provider.errs[call]
	}
	if providerErr != nil {
		return analysis.ProviderResult{
			Model: provider.model, RequestBody: []byte(`{"request":true}`),
			ResponseBody: []byte(`{"providerError":true}`),
		}, providerErr
	}
	output := provider.output
	if call < len(provider.outputs) {
		output = provider.outputs[call]
	}
	if output == "" {
		output = provider.response(request)
	}
	if provider.model == "" {
		provider.model = "test-model"
	}
	return analysis.ProviderResult{
		Model: provider.model, RequestBody: []byte(`{"request":true}`),
		ResponseBody: []byte(`{"response":true}`), Output: []byte(output),
	}, nil
}

func (*fakeProvider) response(request analysis.Request) string {
	return `{
			"reviewCognitiveLoad":{
				"overall":"medium","rationale":"Two APIs interact.","sourceIDs":["` + request.PullRequest.SourceID + `"],"evidenceCompleteness":"complete","partialReason":null,
				"changeScope":{"level":"medium","reason":"Two APIs interact.","sourceIDs":["` + request.PullRequest.SourceID + `"],"evidenceCompleteness":"complete","partialReason":null},
				"requiredContext":{"level":"medium","reason":"Two APIs interact.","sourceIDs":["` + request.PullRequest.SourceID + `"],"evidenceCompleteness":"complete","partialReason":null},
				"conceptualComplexity":{"level":"low","reason":"Localized.","sourceIDs":["` + request.PullRequest.SourceID + `"],"evidenceCompleteness":"complete","partialReason":null},
				"reviewRisk":{"level":"low","reason":"Low risk.","sourceIDs":["` + request.PullRequest.SourceID + `"],"evidenceCompleteness":"complete","partialReason":null}
			},
			"waitingOn":[{"party":"maintainer","reason":"Review is pending.","sourceIDs":["` + request.PullRequest.SourceID + `"]}],
			"qualityEvaluations":[
				{"family":"unrelated_changes","status":"no_concern","completeness":"complete","finding":null,"reason":""},
				{"family":"description_diff_mismatch","status":"no_concern","completeness":"complete","finding":null,"reason":""},
				{"family":"missing_expected_tests","status":"finding","completeness":"complete","finding":{"severity":"medium","summary":"Coverage should be checked.","sourceIDs":["` + request.PullRequest.SourceID + `"],"confidence":"medium"},"reason":""},
				{"family":"internal_contradictions","status":"not_evaluated","completeness":"partial","finding":null,"reason":"Discussion context was truncated."},
				{"family":"unsupported_references","status":"no_concern","completeness":"complete","finding":null,"reason":""}
			]
		}`
}
