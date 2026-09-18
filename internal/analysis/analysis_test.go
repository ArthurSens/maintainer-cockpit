package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/correlation"
)

func TestAnalysisPromptVersionTracksReviewCognitiveLoadContract(t *testing.T) {
	t.Parallel()

	if PromptVersion != "analysis-prompt.v2" {
		t.Fatalf("PromptVersion = %q, want analysis-prompt.v2", PromptVersion)
	}
	request, err := BuildRequest(Input{
		CollectionID: "acme", Repository: "acme/widgets", Number: 7,
		Title: "Track prompt provenance",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.PromptVersion != PromptVersion {
		t.Errorf("request prompt version = %q, want %q", request.PromptVersion, PromptVersion)
	}
}

func TestBuildRequestBoundsAndDeterministicallyDescribesTruncation(t *testing.T) {
	t.Parallel()

	files := make([]string, MaxFiles+2)
	for index := range files {
		files[index] = fmt.Sprintf("pkg/file-%03d.go", index)
	}
	sources := make([]Source, MaxSources+2)
	for index := range sources {
		sources[index] = Source{
			ID:   "source-" + string(rune('A'+index)),
			Kind: "review",
			Text: strings.Repeat("discussion ", MaxSourceBytes),
		}
	}
	input := Input{
		CollectionID: "acme", Repository: "acme/widgets", Number: 7,
		Title: "Bound the model input", Description: strings.Repeat("é", MaxDescriptionBytes),
		URL: "https://github.com/acme/widgets/pull/7", UpdatedAt: time.Unix(123, 0).UTC(),
		Files: files, Sources: sources,
	}

	first, err := BuildRequest(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildRequest(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.InputRevision != second.InputRevision {
		t.Fatalf("revisions differ: %q and %q", first.InputRevision, second.InputRevision)
	}
	if len(first.PullRequest.Description) > MaxDescriptionBytes ||
		len(first.PullRequest.Files) != MaxFiles || len(first.Sources) != MaxSources {
		t.Fatalf("request is not bounded: %+v", first.Truncation)
	}
	if !first.Truncation.Description.Truncated ||
		first.Truncation.Files.Omitted != 2 || first.Truncation.Sources.Omitted == 0 {
		t.Errorf("truncation = %+v, want explicit omitted counts", first.Truncation)
	}
	for _, source := range first.Sources {
		if len(source.Text) > MaxSourceBytes {
			t.Errorf("source %q has %d bytes, limit %d", source.ID, len(source.Text), MaxSourceBytes)
		}
	}
}

func TestValidateResponseRejectsUnknownEnumsSourcesFieldsAndOversizedText(t *testing.T) {
	t.Parallel()

	valid := `{
		"reviewCognitiveLoad":{
			"overall":"medium","rationale":"The behavioral change needs focused API review.","sourceIDs":["PR_acme_widgets_7"],
			"evidenceCompleteness":"complete","partialReason":null,
			"changeScope":{"level":"medium","reason":"Several parsing paths change.","sourceIDs":["PR_acme_widgets_7"],"evidenceCompleteness":"complete","partialReason":null},
			"requiredContext":{"level":"medium","reason":"Two APIs interact.","sourceIDs":["PR_acme_widgets_7"],"evidenceCompleteness":"complete","partialReason":null},
			"conceptualComplexity":{"level":"low","reason":"Localized behavior.","sourceIDs":["file:main.go"],"evidenceCompleteness":"complete","partialReason":null},
			"reviewRisk":{"level":"medium","reason":"Public behavior changes.","sourceIDs":["PR_acme_widgets_7"],"evidenceCompleteness":"complete","partialReason":null}
		},
		"waitingOn":[{"party":"maintainer","reason":"Review is requested.","sourceIDs":["review-1"]}],
		"qualityEvaluations":[
			{"family":"unrelated_changes","status":"no_concern","completeness":"complete","finding":null,"reason":""},
			{"family":"description_diff_mismatch","status":"no_concern","completeness":"complete","finding":null,"reason":""},
			{"family":"missing_expected_tests","status":"finding","completeness":"complete","finding":{"severity":"medium","summary":"Tests do not cover the public behavior.","sourceIDs":["file:main.go"],"confidence":"medium"},"reason":""},
			{"family":"internal_contradictions","status":"not_evaluated","completeness":"partial","finding":null,"reason":"Discussion context was truncated."},
			{"family":"unsupported_references","status":"no_concern","completeness":"complete","finding":null,"reason":""}
		]
	}`
	known := map[string]struct{}{
		"PR_acme_widgets_7": {}, "file:main.go": {}, "review-1": {},
	}
	if _, err := ValidateResponse([]byte(valid), known); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}

	tests := []struct {
		name string
		body string
	}{
		{"unknown level", strings.Replace(valid, `"level":"medium"`, `"level":"critical"`, 1)},
		{"unknown party", strings.Replace(valid, `"party":"maintainer"`, `"party":"bot"`, 1)},
		{"unknown source", strings.Replace(valid, `"review-1"`, `"invented"`, 1)},
		{"unknown field", strings.Replace(valid, `"waitingOn":`, `"extra":true,"waitingOn":`, 1)},
		{"oversized text", strings.Replace(valid, `"Several parsing paths change."`, `"`+strings.Repeat("x", MaxReasonRunes+1)+`"`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateResponse([]byte(test.body), known); err == nil {
				t.Fatalf("ValidateResponse() error = nil for %s", test.name)
			}
		})
	}
}

func TestValidateResponsePreservesValidLoadWhenQualityIsInvalid(t *testing.T) {
	t.Parallel()

	body := strings.Replace(
		validProviderResponse(),
		`"family":"unsupported_references"`,
		`"family":"invented_quality_family"`,
		1,
	)
	response, err := ValidateResponse(
		[]byte(body), map[string]struct{}{"PR_acme_widgets_7": {}},
	)
	if err == nil {
		t.Fatal("ValidateResponse() error = nil, want partial validation error")
	}
	if !response.SectionValid(SectionReviewCognitiveLoad) {
		t.Fatalf("reviewCognitiveLoad validity = false, error = %v", err)
	}
	if response.SectionValid(SectionQualityEvaluations) {
		t.Fatal("qualityEvaluations validity = true for invalid family")
	}
	if response.ReviewCognitiveLoad.Overall != LevelLow {
		t.Errorf("reviewCognitiveLoad.overall = %q, want low", response.ReviewCognitiveLoad.Overall)
	}
}

func TestValidateResponseEnforcesQualityTaxonomyEvidenceAndConfidenceContract(t *testing.T) {
	t.Parallel()

	valid := validProviderResponse()
	known := map[string]struct{}{"PR_acme_widgets_7": {}}
	tests := []struct {
		name    string
		replace string
		with    string
	}{
		{
			name:    "unknown family",
			replace: `"family":"unrelated_changes"`,
			with:    `"family":"style_concern"`,
		},
		{
			name:    "rule only family",
			replace: `"family":"unrelated_changes"`,
			with:    `"family":"broad_additions_only"`,
		},
		{
			name:    "duplicate family",
			replace: `"family":"unsupported_references"`,
			with:    `"family":"unrelated_changes"`,
		},
		{
			name:    "low confidence finding",
			replace: `"confidence":"medium"`,
			with:    `"confidence":"low"`,
		},
		{
			name:    "high severity without high confidence",
			replace: `"severity":"medium"`,
			with:    `"severity":"high"`,
		},
		{
			name:    "partial high severity",
			replace: `"status":"no_concern","completeness":"complete","finding":null`,
			with:    `"status":"finding","completeness":"partial","finding":{"severity":"high","summary":"Unrelated changes are present.","sourceIDs":["PR_acme_widgets_7"],"confidence":"high"}`,
		},
		{
			name:    "absence based finding with partial evidence",
			replace: `"family":"unsupported_references","status":"no_concern","completeness":"complete","finding":null`,
			with:    `"family":"unsupported_references","status":"finding","completeness":"partial","finding":{"severity":"medium","summary":"A collected reference is unsupported.","sourceIDs":["PR_acme_widgets_7"],"confidence":"high"}`,
		},
		{
			name:    "not evaluated without reason",
			replace: `"reason":"Discussion context was truncated."`,
			with:    `"reason":""`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := strings.Replace(valid, test.replace, test.with, 1)
			if _, err := ValidateResponse([]byte(body), known); err == nil {
				t.Fatalf("ValidateResponse() error = nil for %s", test.name)
			}
		})
	}
}

func TestProviderSchemaPublishesStableQualityFamilies(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(responseSchema())
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	for _, family := range ModelQualityFamilies {
		if !strings.Contains(body, `"`+family+`"`) {
			t.Errorf("response schema does not contain family %q", family)
		}
	}
	if !strings.Contains(body, `"qualityEvaluations"`) ||
		strings.Contains(body, `"qualityFindings"`) ||
		strings.Contains(body, `"proposedChange"`) ||
		strings.Contains(body, `"uniqueItems"`) {
		t.Errorf("response schema = %s, want supported typed quality evaluations", body)
	}
}

func TestQualityEvaluationsDeriveDisplayLevelWithoutANumericScore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		evaluations []QualityEvaluation
		want        string
	}{
		{name: "no findings", want: "no_concerns"},
		{
			name: "one medium",
			evaluations: []QualityEvaluation{qualityFinding(
				"missing_expected_tests", "medium", "medium", "complete",
			)},
			want: "review_suggested",
		},
		{
			name: "multiple low",
			evaluations: []QualityEvaluation{
				qualityFinding("unrelated_changes", "low", "medium", "complete"),
				qualityFinding("internal_contradictions", "low", "high", "partial"),
			},
			want: "review_suggested",
		},
		{
			name: "high wins",
			evaluations: []QualityEvaluation{qualityFinding(
				"description_diff_mismatch", "high", "high", "complete",
			)},
			want: "strong_concerns",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := qualityLevel(test.evaluations); got != test.want {
				t.Errorf("qualityLevel() = %q, want %q", got, test.want)
			}
		})
	}
}

func qualityFinding(family, severity, confidence, completeness string) QualityEvaluation {
	return QualityEvaluation{
		Family: family, Status: "finding", Completeness: completeness,
		Finding: &QualityFinding{
			Severity: severity, Summary: "Evidence-backed concern.",
			SourceIDs: []string{"PR_acme_widgets_7"}, Confidence: confidence,
		},
	}
}

func TestReviewCognitiveLoadAllowsUnknownComponentsButRequiresRankedOverall(t *testing.T) {
	t.Parallel()

	body := strings.Replace(
		validProviderResponse(),
		`"conceptualComplexity":{"level":"low","reason":"Straightforward.","sourceIDs":["PR_acme_widgets_7"],"evidenceCompleteness":"complete","partialReason":null}`,
		`"conceptualComplexity":{"level":"unknown","reason":"The excerpt omits generated parser context.","sourceIDs":["PR_acme_widgets_7"],"evidenceCompleteness":"partial","partialReason":"Required context was omitted."}`,
		1,
	)
	response, err := ValidateResponse(
		[]byte(body), map[string]struct{}{"PR_acme_widgets_7": {}},
	)
	if err != nil {
		t.Fatalf("component unknown rejected: %v", err)
	}
	if response.ReviewCognitiveLoad.Overall != LevelLow ||
		response.ReviewCognitiveLoad.ConceptualComplexity.Level != LevelUnknown {
		t.Errorf("review cognitive load = %+v", response.ReviewCognitiveLoad)
	}

	invalid := strings.Replace(body, `"overall":"low"`, `"overall":"unknown"`, 1)
	if _, err := ValidateResponse(
		[]byte(invalid), map[string]struct{}{"PR_acme_widgets_7": {}},
	); err == nil {
		t.Fatal("ValidateResponse() accepted unknown overall")
	}
}

func TestHTTPProvidersUseTheirCompatibleContractsWithoutRetainingCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind             string
		path             string
		responseEnvelope func(string) any
		assertRequest    func(*testing.T, map[string]any)
	}{
		{
			kind: ProviderOpenAI, path: "/v1/chat/completions",
			responseEnvelope: func(content string) any {
				return map[string]any{
					"id": "chatcmpl-123", "model": "gpt-5-mini-2026-08-01",
					"choices": []any{map[string]any{
						"message": map[string]any{"content": content}, "finish_reason": "stop",
					}},
					"usage": map[string]any{
						"prompt_tokens": 101, "completion_tokens": 23,
						"prompt_tokens_details":     map[string]any{"cached_tokens": 61},
						"completion_tokens_details": map[string]any{"reasoning_tokens": 7},
					},
				}
			},
			assertRequest: func(t *testing.T, body map[string]any) {
				if _, ok := body["response_format"]; !ok {
					t.Error("OpenAI-compatible request has no response_format")
				}
				messages, ok := body["messages"].([]any)
				if !ok || len(messages) == 0 {
					t.Fatalf("messages = %#v", body["messages"])
				}
				system, _ := messages[0].(map[string]any)
				if !strings.Contains(
					fmt.Sprint(system["content"]),
					"Set reason to null unless status is not_evaluated",
				) {
					t.Errorf("system prompt = %q", system["content"])
				}
			},
		},
		{
			kind: ProviderOllama, path: "/api/chat",
			responseEnvelope: func(content string) any {
				return map[string]any{
					"model": "qwen3:8b-q4_K_M", "done_reason": "stop",
					"prompt_eval_count": 89, "prompt_eval_cached_count": 55,
					"eval_count": 17,
					"message":    map[string]any{"content": content},
				}
			},
			assertRequest: func(t *testing.T, body map[string]any) {
				if body["stream"] != false || body["format"] == nil {
					t.Errorf("Ollama-compatible request = %+v, want stream false and format schema", body)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			t.Parallel()
			var retainedRequest []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.path {
					t.Errorf("path = %q, want %q", r.URL.Path, test.path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer top-secret" {
					t.Errorf("Authorization = %q", got)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				test.assertRequest(t, body)
				response := validProviderResponse()
				_ = json.NewEncoder(w).Encode(test.responseEnvelope(response))
			}))
			t.Cleanup(server.Close)

			provider, err := NewHTTPProvider(HTTPProviderOptions{
				Kind: test.kind, BaseURL: server.URL, Model: "test-model",
				APIKey: "top-secret", Client: server.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := provider.ModelName(); got != "test-model" {
				t.Errorf("ModelName() = %q, want test-model", got)
			}
			metadata := provider.TelemetryMetadata()
			if metadata.Name != test.kind || metadata.RequestModel != "test-model" ||
				metadata.ServerAddress == "" || metadata.ServerPort == 0 {
				t.Errorf("TelemetryMetadata() = %+v", metadata)
			}
			result, err := provider.Analyze(context.Background(), Request{
				SchemaVersion: SchemaVersion,
				PullRequest:   RequestPullRequest{SourceID: "PR_acme_widgets_7"},
			})
			if err != nil {
				t.Fatal(err)
			}
			retainedRequest = result.RequestBody
			if strings.Contains(string(retainedRequest), "top-secret") {
				t.Fatal("retained request contains provider credential")
			}
			if result.Model != "test-model" || len(result.ResponseBody) == 0 {
				t.Errorf("result = %+v", result)
			}
			if len(result.FinishReasons) != 1 || result.FinishReasons[0] != "stop" ||
				result.InputTokens == nil || result.OutputTokens == nil {
				t.Errorf("provider response metadata = %+v", result)
			}
			switch test.kind {
			case ProviderOpenAI:
				if result.ResponseID != "chatcmpl-123" ||
					result.ResponseModel != "gpt-5-mini-2026-08-01" ||
					*result.InputTokens != 101 || *result.OutputTokens != 23 ||
					result.CachedInputTokens == nil || *result.CachedInputTokens != 61 ||
					result.ReasoningOutputTokens == nil || *result.ReasoningOutputTokens != 7 {
					t.Errorf("OpenAI response metadata = %+v", result)
				}
			case ProviderOllama:
				if result.ResponseID != "" || result.ResponseModel != "qwen3:8b-q4_K_M" ||
					*result.InputTokens != 89 || *result.OutputTokens != 17 ||
					result.CachedInputTokens == nil || *result.CachedInputTokens != 55 ||
					result.ReasoningOutputTokens != nil {
					t.Errorf("Ollama response metadata = %+v", result)
				}
			}
			components := make(map[string]int64, len(result.InputComponents))
			for _, component := range result.InputComponents {
				components[component.Name] = component.Bytes
			}
			for _, name := range []string{"system_instruction", "output_schema"} {
				if components[name] <= 0 {
					t.Errorf("input component %q = %d, want positive bytes", name, components[name])
				}
			}
		})
	}
}

func TestResponseSchemaEncodesQualityEvaluationInvariants(t *testing.T) {
	t.Parallel()

	schema := responseSchema()
	properties := schema["properties"].(map[string]any)
	evaluations := properties["qualityEvaluations"].(map[string]any)
	evaluation := evaluations["items"].(map[string]any)
	variants, ok := evaluation["anyOf"].([]any)
	if !ok || len(variants) != 4 {
		t.Fatalf("evaluation anyOf = %#v, want four valid state variants", evaluation["anyOf"])
	}
	seen := make(map[string]int)
	for _, rawVariant := range variants {
		variant := rawVariant.(map[string]any)
		variantProperties := variant["properties"].(map[string]any)
		status := variantProperties["status"].(map[string]any)["enum"].([]string)[0]
		completeness := variantProperties["completeness"].(map[string]any)["enum"].([]string)[0]
		key := status + "/" + completeness
		seen[key]++

		reasonType := variantProperties["reason"].(map[string]any)["type"]
		if status == QualityStatusNotEvaluated {
			if reasonType != "string" {
				t.Errorf("%s reason type = %#v, want string", key, reasonType)
			}
		} else if reasonType != "null" {
			t.Errorf("%s reason type = %#v, want null", key, reasonType)
		}
	}
	for _, key := range []string{
		"no_concern/complete", "not_evaluated/partial",
		"finding/complete", "finding/partial",
	} {
		if seen[key] != 1 {
			t.Errorf("variant %s count = %d, want 1", key, seen[key])
		}
	}

	response := strings.ReplaceAll(validProviderResponse(), `"reason":""`, `"reason":null`)
	if _, err := ValidateResponse(
		[]byte(response), map[string]struct{}{"PR_acme_widgets_7": {}},
	); err != nil {
		t.Fatalf("ValidateResponse() rejected null conditional reasons: %v", err)
	}
}

func TestAnalysisInputComponentsSeparateDiffEvidence(t *testing.T) {
	t.Parallel()

	components := analysisInputComponents(Request{
		PullRequest: RequestPullRequest{Description: "description"},
		Sources: []Source{
			{ID: "comment:1", Kind: "comment", Text: "discussion"},
			{ID: "diff:main.go", Kind: "diff", Text: "@@ patch"},
		},
	})
	got := make(map[string]ContentMeasurement, len(components))
	for _, component := range components {
		got[component.Name] = component
	}
	if got["evidence"].Bytes != int64(len("discussion")) ||
		got["evidence"].Items == nil || *got["evidence"].Items != 1 {
		t.Errorf("evidence component = %+v", got["evidence"])
	}
	if got["diff_evidence"].Bytes != int64(len("@@ patch")) ||
		got["diff_evidence"].Items == nil || *got["diff_evidence"].Items != 1 {
		t.Errorf("diff evidence component = %+v", got["diff_evidence"])
	}
}

func TestHTTPProviderUsesSeparateCorrelationPromptAndSchema(t *testing.T) {
	t.Parallel()

	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "correlation-1", "model": "test-model",
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": `{"groups":[]}`},
				"finish_reason": "stop",
			}},
		})
	}))
	t.Cleanup(server.Close)
	provider, err := NewHTTPProvider(HTTPProviderOptions{
		Kind: ProviderOpenAI, BaseURL: server.URL, Model: "test-model",
		Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Correlate(context.Background(), correlation.Request{
		SchemaVersion: correlation.SchemaVersion, PromptVersion: correlation.PromptVersion,
		CollectionID: "acme", Chunk: 1, Chunks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"groups":[]}` {
		t.Errorf("Output = %s", result.Output)
	}
	responseFormat, ok := requestBody["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format = %#v", requestBody["response_format"])
	}
	jsonSchema, ok := responseFormat["json_schema"].(map[string]any)
	if !ok || jsonSchema["name"] != "maintainer_cockpit_correlation" {
		t.Errorf("json_schema = %#v", responseFormat["json_schema"])
	}
	messages, ok := requestBody["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v", requestBody["messages"])
	}
	system, _ := messages[0].(map[string]any)
	systemPrompt := fmt.Sprint(system["content"])
	for _, instruction := range []string{
		"compound into one feature",
		"at least one open pull request",
		"distinct members of that group",
		"non-empty names, descriptions, and edge reasons",
		"Do not repeat members, source IDs, groups, or edges",
	} {
		if !strings.Contains(systemPrompt, instruction) {
			t.Errorf("system prompt missing %q: %q", instruction, systemPrompt)
		}
	}
}

func TestHTTPProviderReturnsBoundedTypedStatusError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.Header().Set("X-Request-Id", "req_123")
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "0")
		w.Header().Set("X-Ratelimit-Reset-Tokens", "2m15s")
		http.Error(w, `{"error":{"message":"Rate limit reached for tokens per minute.\nRetry later.","type":"tokens","code":"rate_limit_exceeded"}}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	provider, err := NewHTTPProvider(HTTPProviderOptions{
		Kind: ProviderOpenAI, BaseURL: server.URL, Model: "test-model",
		Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Analyze(context.Background(), Request{
		SchemaVersion: SchemaVersion,
		PullRequest:   RequestPullRequest{SourceID: "PR_acme_widgets_7"},
	})
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("error = %T %v, want typed 429 status error", err, err)
	}
	if statusErr.Code != "rate_limit_exceeded" || statusErr.Type != "tokens" ||
		statusErr.Message != "Rate limit reached for tokens per minute. Retry later." ||
		statusErr.RequestID != "req_123" || statusErr.RetryAfter != "17" ||
		statusErr.RateLimitRemainingTokens != "0" ||
		statusErr.RateLimitResetTokens != "2m15s" {
		t.Errorf("status error = %+v", statusErr)
	}
	if err.Error() != "provider returned HTTP status 429 (rate_limit_exceeded)" ||
		strings.Contains(err.Error(), "Retry later") {
		t.Errorf("error = %q", err)
	}
}

func validProviderResponse() string {
	return `{
		"reviewCognitiveLoad":{
			"overall":"low","rationale":"The change is localized and straightforward to review.","sourceIDs":["PR_acme_widgets_7"],
			"evidenceCompleteness":"complete","partialReason":null,
			"changeScope":{"level":"low","reason":"Localized.","sourceIDs":["PR_acme_widgets_7"],"evidenceCompleteness":"complete","partialReason":null},
			"requiredContext":{"level":"low","reason":"Localized.","sourceIDs":["PR_acme_widgets_7"],"evidenceCompleteness":"complete","partialReason":null},
			"conceptualComplexity":{"level":"low","reason":"Straightforward.","sourceIDs":["PR_acme_widgets_7"],"evidenceCompleteness":"complete","partialReason":null},
			"reviewRisk":{"level":"low","reason":"Low risk.","sourceIDs":["PR_acme_widgets_7"],"evidenceCompleteness":"complete","partialReason":null}
		},
		"waitingOn":[],
		"qualityEvaluations":[
			{"family":"unrelated_changes","status":"no_concern","completeness":"complete","finding":null,"reason":""},
			{"family":"description_diff_mismatch","status":"no_concern","completeness":"complete","finding":null,"reason":""},
			{"family":"missing_expected_tests","status":"finding","completeness":"complete","finding":{"severity":"medium","summary":"Expected tests are absent.","sourceIDs":["PR_acme_widgets_7"],"confidence":"medium"},"reason":""},
			{"family":"internal_contradictions","status":"not_evaluated","completeness":"partial","finding":null,"reason":"Discussion context was truncated."},
			{"family":"unsupported_references","status":"no_concern","completeness":"complete","finding":null,"reason":""}
		]
	}`
}
