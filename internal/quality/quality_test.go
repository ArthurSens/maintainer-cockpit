package quality

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

func rawInput(additions, deletions, files int) Input {
	changed := make([]File, 0, files)
	for index := range files {
		changed = append(changed, File{
			Path:      "pkg/feature/file" + string(rune('a'+index%26)) + ".go",
			Additions: additions / files,
			Deletions: deletions / files,
		})
	}
	return Input{
		Repository:       "acme/widgets",
		Number:           7,
		Title:            "Add feature",
		URL:              "https://github.com/acme/widgets/pull/7",
		Additions:        additions,
		Deletions:        deletions,
		ChangedFileCount: files,
		Files:            changed,
	}
}

func findingByRule(t *testing.T, assessment Assessment, rule string) Finding {
	t.Helper()
	for _, finding := range assessment.Findings {
		if finding.Rule == rule {
			return finding
		}
	}
	t.Fatalf("assessment %+v has no %q finding", assessment, rule)
	return Finding{}
}

func TestBroadAdditionsOnlyProducesReviewSuggested(t *testing.T) {
	t.Parallel()

	input := rawInput(800, 2, 20)
	assessment := Assess(input, DefaultPolicy())
	if assessment.Level != LevelReviewSuggested {
		t.Fatalf("Level = %q, want %q", assessment.Level, LevelReviewSuggested)
	}
	finding := findingByRule(t, assessment, RuleBroadAdditionsOnly)
	if finding.Severity != SeverityMedium {
		t.Errorf("Severity = %q, want medium", finding.Severity)
	}
	if finding.Provenance != ProvenanceRuleDerived {
		t.Errorf("Provenance = %q, want rule_derived", finding.Provenance)
	}
	if finding.Completeness != "complete" {
		t.Errorf("Completeness = %q, want complete", finding.Completeness)
	}
	if len(finding.Evidence) == 0 {
		t.Fatal("finding has no evidence")
	}
	var sawMeasurement bool
	for _, evidence := range finding.Evidence {
		if strings.Contains(evidence.Detail, "800") && evidence.URL != "" {
			sawMeasurement = true
		}
	}
	if !sawMeasurement {
		t.Errorf("Evidence = %+v, want measured churn with source link", finding.Evidence)
	}
}

func TestBroadAdditionsOnlyStaysQuietBelowThresholds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input Input
	}{
		{name: "few files", input: rawInput(800, 0, 5)},
		{name: "small addition volume", input: rawInput(100, 0, 20)},
		{name: "meaningful deletions", input: rawInput(800, 400, 20)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assessment := Assess(test.input, DefaultPolicy())
			if assessment.Level != LevelNoConcerns {
				t.Errorf("Level = %q, want %q", assessment.Level, LevelNoConcerns)
			}
			if len(assessment.Findings) != 0 {
				t.Errorf("Findings = %+v, want none", assessment.Findings)
			}
		})
	}
}

func TestBroadAdditionsOnlyUsesRawProviderChurn(t *testing.T) {
	t.Parallel()

	input := Input{
		Repository: "acme/widgets", Number: 7, URL: "https://github.com/acme/widgets/pull/7",
		Additions: 5000, Deletions: 0, ChangedFileCount: 40,
		Files: []File{
			{Path: "vendor/library.go", Additions: 4900},
			{Path: "main.go", Additions: 100},
		},
	}
	assessment := Assess(input, DefaultPolicy())
	if assessment.Level != LevelReviewSuggested {
		t.Errorf("Level = %q, want review suggested from raw provider churn", assessment.Level)
	}
}

func TestBroadAdditionsOnlyTreatsRawTotalsAsComplete(t *testing.T) {
	t.Parallel()

	input := Input{
		Repository: "acme/widgets", Number: 7, URL: "https://github.com/acme/widgets/pull/7",
		Additions: 900, Deletions: 1, ChangedFileCount: 30,
	}
	assessment := Assess(input, DefaultPolicy())
	finding := findingByRule(t, assessment, RuleBroadAdditionsOnly)
	if finding.Completeness != "complete" {
		t.Errorf("Completeness = %q, want complete for raw provider totals", finding.Completeness)
	}
}

func TestBroadAdditionsOnlyIsUnevaluatedWithoutFileCount(t *testing.T) {
	t.Parallel()

	input := Input{
		Repository: "acme/widgets", Number: 7, URL: "https://github.com/acme/widgets/pull/7",
		Additions: 900, Deletions: 0,
	}
	assessment := Assess(input, DefaultPolicy())
	if len(assessment.Findings) != 0 {
		t.Errorf("Findings = %+v, want none for unevaluated rule", assessment.Findings)
	}
	var unevaluated *UnevaluatedRule
	for index := range assessment.Unevaluated {
		if assessment.Unevaluated[index].Rule == RuleBroadAdditionsOnly {
			unevaluated = &assessment.Unevaluated[index]
		}
	}
	if unevaluated == nil || unevaluated.Reason == "" {
		t.Fatalf("Unevaluated = %+v, want broad_additions_only with reason", assessment.Unevaluated)
	}
}

func TestDescriptionDiffMismatchFlagsUnmatchedReferences(t *testing.T) {
	t.Parallel()

	input := Input{
		Repository: "acme/widgets", Number: 7, URL: "https://github.com/acme/widgets/pull/7",
		Description: "Fixes the parser in internal/parser/lexer.go as discussed.",
		Additions:   10, Deletions: 2, ChangedFileCount: 1,
		Files: []File{{Path: "docs/README.md", Additions: 10, Deletions: 2}},
	}
	assessment := Assess(input, DefaultPolicy())
	if assessment.Level != LevelReviewSuggested {
		t.Fatalf("Level = %q, want %q", assessment.Level, LevelReviewSuggested)
	}
	finding := findingByRule(t, assessment, RuleDescriptionDiffMismatch)
	if finding.Severity != SeverityMedium {
		t.Errorf("Severity = %q, want medium", finding.Severity)
	}
	if finding.Provenance != ProvenanceRuleDerived {
		t.Errorf("Provenance = %q, want rule_derived", finding.Provenance)
	}
	var sawReference bool
	for _, evidence := range finding.Evidence {
		if strings.Contains(evidence.Detail, "internal/parser/lexer.go") &&
			evidence.URL == input.URL {
			sawReference = true
		}
	}
	if !sawReference {
		t.Errorf("Evidence = %+v, want unmatched reference linked to the description", finding.Evidence)
	}
}

func TestDescriptionDiffMismatchEscalatesCompleteMismatch(t *testing.T) {
	t.Parallel()

	input := Input{
		Repository: "acme/widgets", Number: 7, URL: "https://github.com/acme/widgets/pull/7",
		Description: "Rewrites cmd/serve/main.go, internal/auth/session.go, and internal/auth/token.go.",
		Additions:   10, Deletions: 2, ChangedFileCount: 1,
		Files: []File{{Path: "docs/README.md", Additions: 10, Deletions: 2}},
	}
	assessment := Assess(input, DefaultPolicy())
	if assessment.Level != LevelStrongConcerns {
		t.Fatalf("Level = %q, want %q", assessment.Level, LevelStrongConcerns)
	}
	finding := findingByRule(t, assessment, RuleDescriptionDiffMismatch)
	if finding.Severity != SeverityHigh {
		t.Errorf("Severity = %q, want high for a complete mismatch of three references", finding.Severity)
	}
}

func TestDescriptionDiffMismatchAcceptsMatchingReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		description string
		files       []File
	}{
		{
			name:        "exact path",
			description: "Updates internal/parser/lexer.go.",
			files:       []File{{Path: "internal/parser/lexer.go"}},
		},
		{
			name:        "suffix path",
			description: "Updates parser/lexer.go.",
			files:       []File{{Path: "internal/parser/lexer.go"}},
		},
		{
			name:        "backticked file name",
			description: "Updates `lexer.go` to handle unicode.",
			files:       []File{{Path: "internal/parser/lexer.go"}},
		},
		{
			name:        "partial match keeps quiet",
			description: "Updates internal/parser/lexer.go and mentions docs/design.md for context.",
			files:       []File{{Path: "internal/parser/lexer.go"}},
		},
		{
			name:        "urls are not file references",
			description: "See https://example.com/spec/design.md and https://github.com/acme/widgets/issues/1.",
			files:       []File{{Path: "internal/parser/lexer.go"}},
		},
		{
			name:        "no references",
			description: "General cleanup with no file mentions.",
			files:       []File{{Path: "internal/parser/lexer.go"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			input := Input{
				Repository: "acme/widgets", Number: 7,
				URL:         "https://github.com/acme/widgets/pull/7",
				Description: test.description,
				Additions:   1, ChangedFileCount: len(test.files), Files: test.files,
			}
			assessment := Assess(input, DefaultPolicy())
			for _, finding := range assessment.Findings {
				if finding.Rule == RuleDescriptionDiffMismatch {
					t.Errorf("unexpected mismatch finding: %+v", finding)
				}
			}
		})
	}
}

func TestDescriptionDiffMismatchIsUnevaluatedWithoutChangedFiles(t *testing.T) {
	t.Parallel()

	input := Input{
		Repository: "acme/widgets", Number: 7, URL: "https://github.com/acme/widgets/pull/7",
		Description: "Rewrites internal/auth/session.go.",
		Additions:   100, Deletions: 5, ChangedFileCount: 4,
	}
	assessment := Assess(input, DefaultPolicy())
	var sawUnevaluated bool
	for _, unevaluated := range assessment.Unevaluated {
		if unevaluated.Rule == RuleDescriptionDiffMismatch && unevaluated.Reason != "" {
			sawUnevaluated = true
		}
	}
	if !sawUnevaluated {
		t.Errorf("Unevaluated = %+v, want description_diff_mismatch with reason", assessment.Unevaluated)
	}
}

func TestExplicitAIDisclosureIsFactualMetadataOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		statement string
	}{
		{name: "generated with tool", statement: "🤖 Generated with Claude Code"},
		{name: "co-authored trailer", statement: "Co-authored-by: Copilot <copilot@users.noreply.github.com>"},
		{name: "assisted marker", statement: "This change was AI-assisted."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			input := rawInput(10, 5, 2)
			input.Description = "Improve widget parsing.\n\n" + test.statement
			assessment := Assess(input, DefaultPolicy())
			if len(assessment.AIDisclosures) != 1 {
				t.Fatalf("AIDisclosures = %+v, want one disclosure", assessment.AIDisclosures)
			}
			disclosure := assessment.AIDisclosures[0]
			if !strings.Contains(disclosure.Statement, strings.TrimSpace(test.statement)) {
				t.Errorf("Statement = %q, want quoted disclosure %q", disclosure.Statement, test.statement)
			}
			if disclosure.URL != input.URL {
				t.Errorf("URL = %q, want link to the pull request", disclosure.URL)
			}
			if assessment.Level != LevelNoConcerns {
				t.Errorf("Level = %q, want disclosure to never change the level", assessment.Level)
			}
		})
	}
}

func TestUndisclosedAIUseIsNeverInferred(t *testing.T) {
	t.Parallel()

	input := rawInput(10, 5, 2)
	input.Description = "Adds AI model configuration options and documents the ai_provider flag."
	assessment := Assess(input, DefaultPolicy())
	if len(assessment.AIDisclosures) != 0 {
		t.Errorf("AIDisclosures = %+v, want none without an explicit disclosure", assessment.AIDisclosures)
	}
}

func TestLevelFromFindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		severities []string
		want       string
	}{
		{name: "no findings", severities: nil, want: LevelNoConcerns},
		{name: "single low", severities: []string{SeverityLow}, want: LevelNoConcerns},
		{name: "multiple low", severities: []string{SeverityLow, SeverityLow}, want: LevelReviewSuggested},
		{name: "single medium", severities: []string{SeverityMedium}, want: LevelReviewSuggested},
		{name: "high wins", severities: []string{SeverityLow, SeverityHigh}, want: LevelStrongConcerns},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			findings := make([]Finding, 0, len(test.severities))
			for _, severity := range test.severities {
				findings = append(findings, Finding{Rule: "future_rule", Severity: severity})
			}
			if got := Level(findings); got != test.want {
				t.Errorf("Level(%v) = %q, want %q", test.severities, got, test.want)
			}
		})
	}
}

func TestPolicyFromConfigMarksLocalValues(t *testing.T) {
	t.Parallel()

	minFiles := 25
	policy := PolicyFromConfig(&config.Quality{
		BroadAdditionsOnly: &config.BroadAdditionsOnly{MinFiles: &minFiles},
	})
	if policy.BroadAdditionsOnly.MinFiles.Value != 25 ||
		policy.BroadAdditionsOnly.MinFiles.Source != SourceLocal {
		t.Errorf("MinFiles = %+v, want local 25", policy.BroadAdditionsOnly.MinFiles)
	}
	if policy.BroadAdditionsOnly.MinAdditions.Source != SourceDefault {
		t.Errorf("MinAdditions = %+v, want default", policy.BroadAdditionsOnly.MinAdditions)
	}
	if policy.DescriptionDiffMismatch.MinReferences.Source != SourceDefault {
		t.Errorf("MinReferences = %+v, want default", policy.DescriptionDiffMismatch.MinReferences)
	}
}

func TestPolicyFromConfigNilUsesDefaults(t *testing.T) {
	t.Parallel()

	policy := PolicyFromConfig(nil)
	if policy != DefaultPolicy() {
		t.Errorf("PolicyFromConfig(nil) = %+v, want defaults %+v", policy, DefaultPolicy())
	}
	defaults := DefaultPolicy()
	for name, threshold := range map[string]Threshold{
		"min_references":                 defaults.DescriptionDiffMismatch.MinReferences,
		"strong_mismatch_min_references": defaults.DescriptionDiffMismatch.StrongMismatchMinReferences,
		"min_files":                      defaults.BroadAdditionsOnly.MinFiles,
		"min_additions":                  defaults.BroadAdditionsOnly.MinAdditions,
	} {
		if threshold.Source != SourceDefault || threshold.Value < 1 {
			t.Errorf("%s = %+v, want positive default", name, threshold)
		}
	}
	if defaults.BroadAdditionsOnly.MaxDeletions.Source != SourceDefault ||
		defaults.BroadAdditionsOnly.MaxDeletions.Value < 0 {
		t.Errorf("max_deletions = %+v, want non-negative default", defaults.BroadAdditionsOnly.MaxDeletions)
	}
}

func TestTunedThresholdsChangeTheOutcome(t *testing.T) {
	t.Parallel()

	input := rawInput(800, 2, 20)
	minFiles := 30
	tuned := PolicyFromConfig(&config.Quality{
		BroadAdditionsOnly: &config.BroadAdditionsOnly{MinFiles: &minFiles},
	})
	assessment := Assess(input, tuned)
	if assessment.Level != LevelNoConcerns {
		t.Errorf("Level = %q, want tuned threshold to suppress the finding", assessment.Level)
	}
	if assessment.Policy.BroadAdditionsOnly.MinFiles.Source != SourceLocal {
		t.Errorf("assessment policy = %+v, want local min_files", assessment.Policy.BroadAdditionsOnly)
	}
}

func TestAssessmentJSONShapeIsStable(t *testing.T) {
	t.Parallel()

	input := rawInput(800, 2, 20)
	input.Description = "🤖 Generated with Claude Code"
	body, err := json.Marshal(Assess(input, DefaultPolicy()))
	if err != nil {
		t.Fatalf("marshal assessment: %v", err)
	}
	for _, want := range []string{
		`"level":"review_suggested"`, `"findings":[`, `"rule":"broad_additions_only"`,
		`"severity":"medium"`, `"provenance":"rule_derived"`, `"completeness":"complete"`,
		`"evidence":[`, `"aiDisclosures":[`, `"policy":{`, `"source":"default"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("assessment JSON = %s, want %s", body, want)
		}
	}
}
