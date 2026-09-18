// Package quality evaluates deterministic, evidence-backed pull-request
// quality rules. It produces one of three transparent display levels and
// never computes an aggregate numeric score, never uses author history, and
// never infers undisclosed AI use.
package quality

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

// Quality display levels. There is intentionally no numeric score.
const (
	LevelNoConcerns      = "no_concerns"
	LevelReviewSuggested = "review_suggested"
	LevelStrongConcerns  = "strong_concerns"
)

// Finding severities are product-owned, not numeric.
const (
	SeverityLow    = "low"
	SeverityMedium = "medium"
	SeverityHigh   = "high"
)

// Finding provenance labels. Measurements are evidence rather than findings.
const (
	ProvenanceRuleDerived   = "rule_derived"
	ProvenanceModelInferred = "model_inferred"
)

// Stable v0.1 concern-family identifiers. Deterministic rules and model
// evaluations use the same taxonomy.
const (
	ConcernUnrelatedChanges        = "unrelated_changes"
	ConcernDescriptionDiffMismatch = "description_diff_mismatch"
	ConcernBroadAdditionsOnly      = "broad_additions_only"
	ConcernMissingExpectedTests    = "missing_expected_tests"
	ConcernInternalContradictions  = "internal_contradictions"
	ConcernUnsupportedReferences   = "unsupported_references"

	RuleDescriptionDiffMismatch = ConcernDescriptionDiffMismatch
	RuleBroadAdditionsOnly      = ConcernBroadAdditionsOnly
)

// Threshold sources distinguish product defaults from local policy.
const (
	SourceDefault = "default"
	SourceLocal   = "local"
)

// Documented product default thresholds.
const (
	DefaultMismatchMinReferences       = 1
	DefaultStrongMismatchMinReferences = 3
	DefaultBroadMinFiles               = 10
	DefaultBroadMinAdditions           = 400
	DefaultBroadMaxDeletions           = 10
)

// File is one changed file with provider churn.
type File struct {
	Path      string
	Additions int
	Deletions int
}

// Input carries the collected facts one assessment may use.
type Input struct {
	Repository       string
	Number           int
	Title            string
	Description      string
	URL              string
	Additions        int
	Deletions        int
	ChangedFileCount int
	Files            []File
}

// Threshold is one tunable policy value and its provenance.
type Threshold struct {
	Value  int    `json:"value"`
	Source string `json:"source"`
}

// MismatchPolicy tunes the description/diff mismatch rule.
type MismatchPolicy struct {
	MinReferences               Threshold `json:"minReferences"`
	StrongMismatchMinReferences Threshold `json:"strongMismatchMinReferences"`
}

// BroadAdditionsPolicy tunes the unusually broad additions-only rule.
type BroadAdditionsPolicy struct {
	MinFiles     Threshold `json:"minFiles"`
	MinAdditions Threshold `json:"minAdditions"`
	MaxDeletions Threshold `json:"maxDeletions"`
}

// Policy is the effective, documented rule configuration for one collection.
type Policy struct {
	DescriptionDiffMismatch MismatchPolicy       `json:"descriptionDiffMismatch"`
	BroadAdditionsOnly      BroadAdditionsPolicy `json:"broadAdditionsOnly"`
}

// Evidence links one finding back to its GitHub source.
type Evidence struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	URL    string `json:"url,omitempty"`
}

// Finding is one evidence-backed quality concern.
type Finding struct {
	Rule         string     `json:"rule"`
	Severity     string     `json:"severity"`
	Provenance   string     `json:"provenance"`
	Summary      string     `json:"summary"`
	Evidence     []Evidence `json:"evidence"`
	Completeness string     `json:"completeness"`
	Confidence   string     `json:"confidence,omitempty"`
	SourceIDs    []string   `json:"sourceIDs,omitempty"`
}

// UnevaluatedRule records a rule that could not run and why, so missing
// evidence is never silently converted into a judgment.
type UnevaluatedRule struct {
	Rule         string `json:"rule"`
	Provenance   string `json:"provenance"`
	Completeness string `json:"completeness"`
	Reason       string `json:"reason"`
}

// Disclosure is factual metadata quoting an explicit AI-assistance statement
// made by the author. It never contributes to the quality level.
type Disclosure struct {
	Statement string `json:"statement"`
	URL       string `json:"url,omitempty"`
}

// Assessment is one complete deterministic quality evaluation.
type Assessment struct {
	Level         string            `json:"level"`
	Findings      []Finding         `json:"findings"`
	Unevaluated   []UnevaluatedRule `json:"unevaluated"`
	AIDisclosures []Disclosure      `json:"aiDisclosures"`
	Policy        Policy            `json:"policy"`
}

// DefaultPolicy returns the documented product default thresholds.
func DefaultPolicy() Policy {
	return Policy{
		DescriptionDiffMismatch: MismatchPolicy{
			MinReferences:               Threshold{Value: DefaultMismatchMinReferences, Source: SourceDefault},
			StrongMismatchMinReferences: Threshold{Value: DefaultStrongMismatchMinReferences, Source: SourceDefault},
		},
		BroadAdditionsOnly: BroadAdditionsPolicy{
			MinFiles:     Threshold{Value: DefaultBroadMinFiles, Source: SourceDefault},
			MinAdditions: Threshold{Value: DefaultBroadMinAdditions, Source: SourceDefault},
			MaxDeletions: Threshold{Value: DefaultBroadMaxDeletions, Source: SourceDefault},
		},
	}
}

// PolicyFromConfig resolves one collection's configured overrides against the
// documented defaults and records which values are local policy.
func PolicyFromConfig(configured *config.Quality) Policy {
	policy := DefaultPolicy()
	if configured == nil {
		return policy
	}
	if mismatch := configured.DescriptionDiffMismatch; mismatch != nil {
		applyOverride(&policy.DescriptionDiffMismatch.MinReferences, mismatch.MinReferences)
		applyOverride(&policy.DescriptionDiffMismatch.StrongMismatchMinReferences, mismatch.StrongMismatchMinReferences)
	}
	if broad := configured.BroadAdditionsOnly; broad != nil {
		applyOverride(&policy.BroadAdditionsOnly.MinFiles, broad.MinFiles)
		applyOverride(&policy.BroadAdditionsOnly.MinAdditions, broad.MinAdditions)
		applyOverride(&policy.BroadAdditionsOnly.MaxDeletions, broad.MaxDeletions)
	}
	strong := &policy.DescriptionDiffMismatch.StrongMismatchMinReferences
	if strong.Value < policy.DescriptionDiffMismatch.MinReferences.Value {
		strong.Value = policy.DescriptionDiffMismatch.MinReferences.Value
	}
	return policy
}

func applyOverride(threshold *Threshold, value *int) {
	if value == nil {
		return
	}
	threshold.Value = *value
	threshold.Source = SourceLocal
}

// Level derives the display level from validated findings. A high-severity
// finding produces strong concerns; a medium finding or multiple low
// findings produce review suggested.
func Level(findings []Finding) string {
	lowCount := 0
	level := LevelNoConcerns
	for _, finding := range findings {
		switch finding.Severity {
		case SeverityHigh:
			return LevelStrongConcerns
		case SeverityMedium:
			level = LevelReviewSuggested
		case SeverityLow:
			lowCount++
		}
	}
	if lowCount >= 2 {
		return LevelReviewSuggested
	}
	return level
}

// Assess deterministically evaluates every quality rule for one PR.
func Assess(input Input, policy Policy) Assessment {
	assessment := Assessment{
		Findings:      make([]Finding, 0, 2),
		Unevaluated:   make([]UnevaluatedRule, 0, 2),
		AIDisclosures: findDisclosures(input),
		Policy:        policy,
	}
	if finding, unevaluated := assessMismatch(input, policy.DescriptionDiffMismatch); unevaluated != nil {
		assessment.Unevaluated = append(assessment.Unevaluated, *unevaluated)
	} else if finding != nil {
		assessment.Findings = append(assessment.Findings, *finding)
	}
	if finding, unevaluated := assessBroadAdditions(input, policy.BroadAdditionsOnly); unevaluated != nil {
		assessment.Unevaluated = append(assessment.Unevaluated, *unevaluated)
	} else if finding != nil {
		assessment.Findings = append(assessment.Findings, *finding)
	}
	assessment.Level = Level(assessment.Findings)
	return assessment
}

var (
	urlPattern           = regexp.MustCompile(`https?://[^\s<>"']+`)
	pathReferencePattern = regexp.MustCompile(`[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+`)
	backtickPattern      = regexp.MustCompile("`([^`\\s]+)`")
	fileExtensionPattern = regexp.MustCompile(`\.[A-Za-z0-9]{1,10}$`)
)

func assessMismatch(input Input, policy MismatchPolicy) (*Finding, *UnevaluatedRule) {
	references := extractFileReferences(input.Description)
	if len(references) == 0 {
		return nil, nil
	}
	if len(input.Files) == 0 {
		return nil, &UnevaluatedRule{
			Rule:         RuleDescriptionDiffMismatch,
			Provenance:   ProvenanceRuleDerived,
			Completeness: "partial",
			Reason:       "The changed file list was not collected, so description references could not be compared with the diff.",
		}
	}
	unmatched := make([]string, 0, len(references))
	for _, reference := range references {
		if !referenceMatchesDiff(reference, input.Files) {
			unmatched = append(unmatched, reference)
		}
	}
	if len(unmatched) != len(references) || len(references) < policy.MinReferences.Value {
		return nil, nil
	}
	severity := SeverityMedium
	if len(references) >= policy.StrongMismatchMinReferences.Value {
		severity = SeverityHigh
	}
	completeness := "complete"
	if input.ChangedFileCount > len(input.Files) {
		completeness = "partial"
	}
	evidence := make([]Evidence, 0, len(unmatched)+1)
	for _, reference := range unmatched {
		evidence = append(evidence, Evidence{
			Kind:   "description_reference",
			Detail: fmt.Sprintf("The description references %s, which is not part of the diff.", reference),
			URL:    input.URL,
		})
	}
	evidence = append(evidence, Evidence{
		Kind:   "changed_files",
		Detail: fmt.Sprintf("%d changed files were collected for comparison.", len(input.Files)),
		URL:    filesURL(input.URL),
	})
	return &Finding{
		Rule:       RuleDescriptionDiffMismatch,
		Severity:   severity,
		Provenance: ProvenanceRuleDerived,
		Summary: fmt.Sprintf(
			"The description references %d file(s); none of them appear in the diff.",
			len(references),
		),
		Evidence:     evidence,
		Completeness: completeness,
	}, nil
}

func assessBroadAdditions(input Input, policy BroadAdditionsPolicy) (*Finding, *UnevaluatedRule) {
	additions, deletions := input.Additions, input.Deletions
	fileCount := input.ChangedFileCount
	if fileCount == 0 {
		fileCount = len(input.Files)
	}
	if fileCount == 0 {
		if additions == 0 && deletions == 0 {
			return nil, nil
		}
		return nil, &UnevaluatedRule{
			Rule:         RuleBroadAdditionsOnly,
			Provenance:   ProvenanceRuleDerived,
			Completeness: "partial",
			Reason:       "The changed file count was not collected, so change breadth could not be measured.",
		}
	}
	if fileCount < policy.MinFiles.Value ||
		additions < policy.MinAdditions.Value ||
		deletions > policy.MaxDeletions.Value {
		return nil, nil
	}
	return &Finding{
		Rule:       RuleBroadAdditionsOnly,
		Severity:   SeverityMedium,
		Provenance: ProvenanceRuleDerived,
		Summary: fmt.Sprintf(
			"Unusually broad additions-only change: +%d lines across %d files with only %d deletions.",
			additions, fileCount, deletions,
		),
		Evidence: []Evidence{
			{
				Kind:   "measured_churn",
				Detail: fmt.Sprintf("+%d/−%d across %d files (raw provider totals).", additions, deletions, fileCount),
				URL:    filesURL(input.URL),
			},
			{
				Kind: "policy_threshold",
				Detail: fmt.Sprintf(
					"Policy flags at least %d files, at least %d additions, and at most %d deletions.",
					policy.MinFiles.Value, policy.MinAdditions.Value, policy.MaxDeletions.Value,
				),
			},
		},
		Completeness: "complete",
	}, nil
}

func extractFileReferences(description string) []string {
	withoutURLs := urlPattern.ReplaceAllString(description, " ")
	seen := make(map[string]struct{})
	references := make([]string, 0, 4)
	add := func(candidate string) {
		candidate = strings.Trim(candidate, ".,;:!?)]}('\"")
		if candidate == "" || !fileExtensionPattern.MatchString(candidate) {
			return
		}
		key := strings.ToLower(candidate)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		references = append(references, candidate)
	}
	for _, match := range pathReferencePattern.FindAllString(withoutURLs, -1) {
		add(match)
	}
	for _, match := range backtickPattern.FindAllStringSubmatch(withoutURLs, -1) {
		add(match[1])
	}
	sort.Strings(references)
	return references
}

func referenceMatchesDiff(reference string, files []File) bool {
	normalized := strings.ToLower(strings.TrimPrefix(reference, "./"))
	for _, file := range files {
		filePath := strings.ToLower(file.Path)
		if filePath == normalized ||
			strings.HasSuffix(filePath, "/"+normalized) ||
			strings.HasSuffix(normalized, "/"+filePath) {
			return true
		}
		if !strings.Contains(normalized, "/") && path.Base(filePath) == normalized {
			return true
		}
	}
	return false
}

// Explicit AI-disclosure markers. Detection quotes the author's own words as
// factual metadata; anything short of an explicit statement is ignored.
var disclosurePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)co-authored-by:.*\b(copilot|claude|chatgpt|codex|gemini|cursor|devin|aider)\b`),
	regexp.MustCompile(`(?i)\b(generated|written|created|authored|drafted)\s+(with|by|using)\b.*\b(copilot|claude|chatgpt|gpt|codex|gemini|cursor|devin|aider|ai)\b`),
	regexp.MustCompile(`(?i)\bai[\s-]assisted\b`),
}

func findDisclosures(input Input) []Disclosure {
	disclosures := make([]Disclosure, 0, 1)
	seen := make(map[string]struct{})
	for line := range strings.SplitSeq(input.Description, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		for _, pattern := range disclosurePatterns {
			if !pattern.MatchString(trimmed) {
				continue
			}
			statement := truncateRunes(trimmed, 200)
			if _, exists := seen[statement]; exists {
				break
			}
			seen[statement] = struct{}{}
			disclosures = append(disclosures, Disclosure{Statement: statement, URL: input.URL})
			break
		}
	}
	return disclosures
}

func filesURL(prURL string) string {
	if prURL == "" {
		return ""
	}
	return prURL + "/files"
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
