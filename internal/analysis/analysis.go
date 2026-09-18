// Package analysis builds bounded model inputs and validates evidence-linked
// model output. It owns the product schema and Review Cognitive Load contracts;
// providers only transport JSON.
package analysis

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ArthurSens/maintainer-cockpit/internal/quality"
)

const (
	SchemaVersion = "analysis.v2"
	PromptVersion = "analysis-prompt.v2"

	MaxDescriptionBytes = 12 << 10
	MaxSourceBytes      = 2 << 10
	MaxDiffSourceBytes  = 8 << 10
	MaxSources          = 300
	MaxFiles            = 100

	MaxReasonRunes  = 500
	MaxFindingRunes = 500
)

const (
	LevelLow     = "low"
	LevelMedium  = "medium"
	LevelHigh    = "high"
	LevelUnknown = "unknown"
)

const (
	ProviderOpenAI = "openai"
	ProviderOllama = "ollama"
)

// Source is public GitHub evidence already collected by the application.
// There is deliberately no field for fetched external content.
type Source struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
}

// Input is the collection-specific factual snapshot used for analysis.
type Input struct {
	CollectionID        string
	Repository          string
	Number              int
	Title               string
	Description         string
	URL                 string
	UpdatedAt           time.Time
	Additions           int
	Deletions           int
	ContextCompleteness string
	Files               []string
	Sources             []Source
	DiffEvidence        DiffEvidence
}

// Request is the retained, credential-free model request.
type Request struct {
	SchemaVersion string             `json:"schemaVersion"`
	PromptVersion string             `json:"promptVersion"`
	InputRevision string             `json:"inputRevision"`
	PullRequest   RequestPullRequest `json:"pullRequest"`
	Sources       []Source           `json:"sources"`
	Truncation    Truncation         `json:"truncation"`
	DiffEvidence  DiffEvidence       `json:"diffEvidence"`
}

// DiffEvidence describes the independently collected current-head patch set.
type DiffEvidence struct {
	Completeness  string `json:"completeness"`
	OriginalFiles int    `json:"originalFiles"`
	SentFiles     int    `json:"sentFiles"`
	OmittedFiles  int    `json:"omittedFiles"`
	OriginalBytes int    `json:"originalBytes"`
	SentBytes     int    `json:"sentBytes"`
	Truncated     bool   `json:"truncated"`
}

type RequestPullRequest struct {
	SourceID            string    `json:"sourceID"`
	CollectionID        string    `json:"collectionID"`
	Repository          string    `json:"repository"`
	Number              int       `json:"number"`
	Title               string    `json:"title"`
	Description         string    `json:"description"`
	URL                 string    `json:"url"`
	UpdatedAt           time.Time `json:"updatedAt"`
	Additions           int       `json:"additions"`
	Deletions           int       `json:"deletions"`
	ContextCompleteness string    `json:"contextCompleteness"`
	Files               []string  `json:"files"`
}

type Truncation struct {
	Description TextTruncation `json:"description"`
	Files       ListTruncation `json:"files"`
	Sources     ListTruncation `json:"sources"`
}

type TextTruncation struct {
	OriginalBytes int  `json:"originalBytes"`
	SentBytes     int  `json:"sentBytes"`
	Truncated     bool `json:"truncated"`
}

type ListTruncation struct {
	Original int `json:"original"`
	Sent     int `json:"sent"`
	Omitted  int `json:"omitted"`
}

// Component is one evidence-backed Review Cognitive Load component.
type Component struct {
	Level                string   `json:"level"`
	Reason               string   `json:"reason"`
	SourceIDs            []string `json:"sourceIDs"`
	EvidenceCompleteness string   `json:"evidenceCompleteness"`
	PartialReason        *string  `json:"partialReason"`
}

type Components struct {
	ChangeScope          Component `json:"changeScope"`
	RequiredContext      Component `json:"requiredContext"`
	ConceptualComplexity Component `json:"conceptualComplexity"`
	ReviewRisk           Component `json:"reviewRisk"`
}

type ReviewCognitiveLoad struct {
	Components
	Overall              string   `json:"overall"`
	Rationale            string   `json:"rationale"`
	SourceIDs            []string `json:"sourceIDs"`
	EvidenceCompleteness string   `json:"evidenceCompleteness"`
	PartialReason        *string  `json:"partialReason"`
}

type WaitingState struct {
	Party     string   `json:"party"`
	Reason    string   `json:"reason"`
	SourceIDs []string `json:"sourceIDs"`
}

type QualityFinding struct {
	Severity   string   `json:"severity"`
	Summary    string   `json:"summary"`
	SourceIDs  []string `json:"sourceIDs"`
	Confidence string   `json:"confidence"`
}

const (
	QualityStatusNoConcern    = "no_concern"
	QualityStatusFinding      = "finding"
	QualityStatusNotEvaluated = "not_evaluated"
)

var ModelQualityFamilies = []string{
	quality.ConcernUnrelatedChanges,
	quality.ConcernDescriptionDiffMismatch,
	quality.ConcernMissingExpectedTests,
	quality.ConcernInternalContradictions,
	quality.ConcernUnsupportedReferences,
}

// QualityEvaluation makes a clean model evaluation distinguishable from one
// that lacked enough evidence. Every model response contains exactly one
// evaluation for each permitted model-inferred concern family.
type QualityEvaluation struct {
	Family       string          `json:"family"`
	Status       string          `json:"status"`
	Completeness string          `json:"completeness"`
	Finding      *QualityFinding `json:"finding"`
	Reason       string          `json:"reason"`
}

// ProviderResponse is the strict schema returned by model providers.
type ProviderResponse struct {
	ReviewCognitiveLoad ReviewCognitiveLoad `json:"reviewCognitiveLoad"`
	WaitingOn           []WaitingState      `json:"waitingOn"`
	QualityEvaluations  []QualityEvaluation `json:"qualityEvaluations"`
	validSections       map[Section]bool
}

type Section string

const (
	SectionReviewCognitiveLoad Section = "reviewCognitiveLoad"
	SectionWaitingOn           Section = "waitingOn"
	SectionQualityEvaluations  Section = "qualityEvaluations"
)

var responseSections = []Section{
	SectionReviewCognitiveLoad,
	SectionWaitingOn,
	SectionQualityEvaluations,
}

func (response ProviderResponse) SectionValid(section Section) bool {
	return response.validSections[section]
}

func (response ProviderResponse) Complete() bool {
	return !slices.ContainsFunc(responseSections, func(section Section) bool {
		return !response.SectionValid(section)
	})
}

func (response ProviderResponse) HasValidSections() bool {
	return slices.ContainsFunc(responseSections, response.SectionValid)
}

// Result is the validated collection-specific analysis.
type Result struct {
	Status              string               `json:"status"`
	ReviewCognitiveLoad *ReviewCognitiveLoad `json:"reviewCognitiveLoad,omitempty"`
	WaitingOn           []WaitingState       `json:"waitingOn"`
	QualityLevel        string               `json:"qualityLevel"`
	QualityEvaluations  []QualityEvaluation  `json:"qualityEvaluations"`
}

func PullRequestSourceID(repository string, number int) string {
	replacer := strings.NewReplacer("/", "_", "#", "_", " ", "_")
	return fmt.Sprintf("PR_%s_%d", replacer.Replace(repository), number)
}

// BuildRequest applies deterministic byte and item limits and hashes the exact
// resulting factual inputs. Prompt and schema versions intentionally do not
// participate in this revision; cache checks compare the schema separately.
func BuildRequest(input Input) (Request, error) {
	if input.CollectionID == "" || input.Repository == "" || input.Number <= 0 || input.Title == "" {
		return Request{}, errors.New("analysis input has missing pull request identity")
	}
	description := truncateUTF8(input.Description, MaxDescriptionBytes)
	files := append([]string(nil), input.Files...)
	sort.Strings(files)
	files = unique(files)
	originalFiles := len(files)
	if len(files) > MaxFiles {
		files = files[:MaxFiles]
	}
	sources := append([]Source(nil), input.Sources...)
	sourceID := PullRequestSourceID(input.Repository, input.Number)
	sources = append(sources, Source{ID: sourceID, Kind: "pull_request", Text: description, URL: input.URL})
	for _, file := range files {
		sources = append(sources, Source{ID: "file:" + file, Kind: "changed_file", Text: file, URL: input.URL + "/files"})
	}
	sort.Slice(sources, func(i, j int) bool {
		if sourcePriority(sources[i]) != sourcePriority(sources[j]) {
			return sourcePriority(sources[i]) < sourcePriority(sources[j])
		}
		if sources[i].ID == sources[j].ID {
			return sources[i].Kind < sources[j].Kind
		}
		return sources[i].ID < sources[j].ID
	})
	sources = uniqueSources(sources)
	for index := range sources {
		limit := MaxSourceBytes
		if sources[index].Kind == "diff" {
			limit = MaxDiffSourceBytes
		}
		sources[index].Text = truncateUTF8(sources[index].Text, limit)
	}
	originalSources := len(sources)
	if len(sources) > MaxSources {
		sources = sources[:MaxSources]
	}
	request := Request{
		SchemaVersion: SchemaVersion,
		PromptVersion: PromptVersion,
		PullRequest: RequestPullRequest{
			SourceID: sourceID, CollectionID: input.CollectionID,
			Repository: input.Repository, Number: input.Number, Title: input.Title,
			Description: description, URL: input.URL, UpdatedAt: input.UpdatedAt,
			Additions: input.Additions, Deletions: input.Deletions,
			ContextCompleteness: input.ContextCompleteness, Files: files,
		},
		Sources:      sources,
		DiffEvidence: input.DiffEvidence,
		Truncation: Truncation{
			Description: TextTruncation{
				OriginalBytes: len(input.Description), SentBytes: len(description),
				Truncated: len(description) != len(input.Description),
			},
			Files:   listTruncation(originalFiles, len(files)),
			Sources: listTruncation(originalSources, len(sources)),
		},
	}
	revisionValue := struct {
		PullRequest  RequestPullRequest `json:"pullRequest"`
		Sources      []Source           `json:"sources"`
		Truncation   Truncation         `json:"truncation"`
		DiffEvidence DiffEvidence       `json:"diffEvidence"`
	}{request.PullRequest, request.Sources, request.Truncation, request.DiffEvidence}
	encoded, err := json.Marshal(revisionValue)
	if err != nil {
		return Request{}, fmt.Errorf("encode analysis input revision: %w", err)
	}
	sum := sha256.Sum256(encoded)
	request.InputRevision = hex.EncodeToString(sum[:])
	return request, nil
}

func sourcePriority(source Source) int {
	switch source.Kind {
	case "diff":
		return 0
	case "pull_request":
		return 1
	default:
		return 2
	}
}

func listTruncation(original, sent int) ListTruncation {
	return ListTruncation{Original: original, Sent: sent, Omitted: original - sent}
}

func unique(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if value == "" || len(result) > 0 && result[len(result)-1] == value {
			continue
		}
		result = append(result, value)
	}
	return result
}

func uniqueSources(values []Source) []Source {
	result := values[:0]
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value.ID == "" {
			continue
		}
		if _, exists := seen[value.ID]; exists {
			continue
		}
		seen[value.ID] = struct{}{}
		result = append(result, value)
	}
	return result
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// ValidateResponse decodes the root once, then validates each semantic section
// independently so one invalid section cannot erase unrelated valid output.
func ValidateResponse(body []byte, knownSourceIDs map[string]struct{}) (ProviderResponse, error) {
	var root map[string]json.RawMessage
	if err := strictDecodeJSON(body, &root, false); err != nil {
		return ProviderResponse{}, fmt.Errorf("decode model response: %w", err)
	}
	response := ProviderResponse{validSections: make(map[Section]bool, len(responseSections))}
	var validationErrors []error
	for key := range root {
		if !slices.Contains(responseSections, Section(key)) {
			validationErrors = append(validationErrors, fmt.Errorf("model response has unknown root field %q", key))
		}
	}
	validateSection := func(section Section, target any, validate func() error) {
		raw, exists := root[string(section)]
		if !exists {
			validationErrors = append(validationErrors, fmt.Errorf("model response is missing %s", section))
			return
		}
		if err := strictDecodeJSON(raw, target, true); err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("decode %s: %w", section, err))
			return
		}
		if err := validate(); err != nil {
			validationErrors = append(validationErrors, err)
			return
		}
		response.validSections[section] = true
	}
	validateSection(SectionReviewCognitiveLoad, &response.ReviewCognitiveLoad, func() error {
		return validateReviewCognitiveLoad(response.ReviewCognitiveLoad, knownSourceIDs)
	})
	validateSection(SectionWaitingOn, &response.WaitingOn, func() error {
		return validateWaitingOn(response.WaitingOn, knownSourceIDs)
	})
	validateSection(SectionQualityEvaluations, &response.QualityEvaluations, func() error {
		return validateQualityEvaluations(response.QualityEvaluations, knownSourceIDs)
	})
	if response.SectionValid(SectionWaitingOn) && response.WaitingOn == nil {
		response.WaitingOn = make([]WaitingState, 0)
	}
	return response, errors.Join(validationErrors...)
}

func strictDecodeJSON(body []byte, target any, disallowUnknown bool) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("must contain exactly one JSON document")
	}
	return nil
}

func validateReviewCognitiveLoad(load ReviewCognitiveLoad, knownSourceIDs map[string]struct{}) error {
	if !oneOf(load.Overall, LevelLow, LevelMedium, LevelHigh) {
		return fmt.Errorf(
			"reviewCognitiveLoad.overall has unknown value %q", load.Overall,
		)
	}
	if err := validateTextAndSources(
		"reviewCognitiveLoad", load.Rationale, MaxReasonRunes,
		load.SourceIDs, knownSourceIDs,
	); err != nil {
		return err
	}
	if err := validateCompleteness(
		"reviewCognitiveLoad", load.EvidenceCompleteness, load.PartialReason,
	); err != nil {
		return err
	}
	for name, component := range map[string]Component{
		"reviewCognitiveLoad.changeScope":          load.ChangeScope,
		"reviewCognitiveLoad.requiredContext":      load.RequiredContext,
		"reviewCognitiveLoad.conceptualComplexity": load.ConceptualComplexity,
		"reviewCognitiveLoad.reviewRisk":           load.ReviewRisk,
	} {
		if err := validateComponent(name, component, knownSourceIDs); err != nil {
			return err
		}
	}
	return nil
}

func validateWaitingOn(waitingOn []WaitingState, knownSourceIDs map[string]struct{}) error {
	if len(waitingOn) > 4 {
		return errors.New("waitingOn must not exceed four states")
	}
	parties := make(map[string]struct{}, len(waitingOn))
	for index, waiting := range waitingOn {
		if !oneOf(waiting.Party, "triager", "maintainer", "author", "external_dependency") {
			return fmt.Errorf("waitingOn[%d].party has unknown value %q", index, waiting.Party)
		}
		if _, exists := parties[waiting.Party]; exists {
			return fmt.Errorf("waitingOn contains duplicate party %q", waiting.Party)
		}
		parties[waiting.Party] = struct{}{}
		if err := validateTextAndSources(fmt.Sprintf("waitingOn[%d]", index), waiting.Reason, MaxReasonRunes, waiting.SourceIDs, knownSourceIDs); err != nil {
			return err
		}
	}
	return nil
}

func validateQualityEvaluations(evaluations []QualityEvaluation, knownSourceIDs map[string]struct{}) error {
	if len(evaluations) != len(ModelQualityFamilies) {
		return fmt.Errorf(
			"qualityEvaluations must contain exactly %d families",
			len(ModelQualityFamilies),
		)
	}
	families := make(map[string]struct{}, len(evaluations))
	for index, evaluation := range evaluations {
		if !oneOf(evaluation.Family, ModelQualityFamilies...) {
			return fmt.Errorf(
				"qualityEvaluations[%d].family has unknown value %q", index, evaluation.Family,
			)
		}
		if _, exists := families[evaluation.Family]; exists {
			return fmt.Errorf(
				"qualityEvaluations contains duplicate family %q", evaluation.Family,
			)
		}
		families[evaluation.Family] = struct{}{}
		if err := validateQualityEvaluation(index, evaluation, knownSourceIDs); err != nil {
			return err
		}
	}
	return nil
}

func validateQualityEvaluation(
	index int,
	evaluation QualityEvaluation,
	knownSourceIDs map[string]struct{},
) error {
	name := fmt.Sprintf("qualityEvaluations[%d]", index)
	if !oneOf(evaluation.Completeness, "complete", "partial") {
		return fmt.Errorf("%s.completeness has unknown value %q", name, evaluation.Completeness)
	}
	switch evaluation.Status {
	case QualityStatusNoConcern:
		if evaluation.Completeness != "complete" {
			return fmt.Errorf("%s no_concern requires complete evidence", name)
		}
		if evaluation.Finding != nil || evaluation.Reason != "" {
			return fmt.Errorf("%s no_concern must not contain a finding or reason", name)
		}
	case QualityStatusNotEvaluated:
		if evaluation.Completeness != "partial" {
			return fmt.Errorf("%s not_evaluated requires partial evidence", name)
		}
		if evaluation.Finding != nil {
			return fmt.Errorf("%s not_evaluated must not contain a finding", name)
		}
		if strings.TrimSpace(evaluation.Reason) == "" ||
			utf8.RuneCountInString(evaluation.Reason) > MaxReasonRunes {
			return fmt.Errorf("%s reason must contain 1 through %d characters", name, MaxReasonRunes)
		}
	case QualityStatusFinding:
		if evaluation.Finding == nil || evaluation.Reason != "" {
			return fmt.Errorf("%s finding requires finding data and no reason", name)
		}
		finding := evaluation.Finding
		if !oneOf(finding.Severity, "low", "medium", "high") {
			return fmt.Errorf("%s.finding.severity has unknown value %q", name, finding.Severity)
		}
		if !oneOf(finding.Confidence, "medium", "high") {
			return fmt.Errorf("%s.finding.confidence has unknown value %q", name, finding.Confidence)
		}
		if finding.Severity == "high" &&
			(evaluation.Completeness != "complete" || finding.Confidence != "high") {
			return fmt.Errorf("%s high severity requires complete evidence and high confidence", name)
		}
		if evaluation.Completeness == "partial" && finding.Severity == "high" {
			return fmt.Errorf("%s partial findings are capped at medium severity", name)
		}
		if (evaluation.Family == quality.ConcernMissingExpectedTests ||
			evaluation.Family == quality.ConcernUnsupportedReferences) &&
			evaluation.Completeness != "complete" {
			return fmt.Errorf("%s family %q requires complete evidence", name, evaluation.Family)
		}
		if err := validateTextAndSources(
			name+".finding", finding.Summary, MaxFindingRunes,
			finding.SourceIDs, knownSourceIDs,
		); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s.status has unknown value %q", name, evaluation.Status)
	}
	return nil
}

func validateComponent(name string, component Component, known map[string]struct{}) error {
	if !oneOf(component.Level, LevelLow, LevelMedium, LevelHigh, LevelUnknown) {
		return fmt.Errorf("%s.level has unknown value %q", name, component.Level)
	}
	if err := validateTextAndSources(
		name, component.Reason, MaxReasonRunes, component.SourceIDs, known,
	); err != nil {
		return err
	}
	return validateCompleteness(name, component.EvidenceCompleteness, component.PartialReason)
}

func validateCompleteness(name, completeness string, partialReason *string) error {
	switch completeness {
	case "complete":
		if partialReason != nil {
			return fmt.Errorf("%s complete evidence requires a null partialReason", name)
		}
	case "partial":
		if partialReason == nil || strings.TrimSpace(*partialReason) == "" ||
			utf8.RuneCountInString(*partialReason) > MaxReasonRunes {
			return fmt.Errorf(
				"%s partial evidence requires a partialReason with 1 through %d characters",
				name, MaxReasonRunes,
			)
		}
	default:
		return fmt.Errorf("%s.evidenceCompleteness has unknown value %q", name, completeness)
	}
	return nil
}

func validateTextAndSources(name, text string, limit int, sourceIDs []string, known map[string]struct{}) error {
	if strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > limit {
		return fmt.Errorf("%s text must contain 1 through %d characters", name, limit)
	}
	if len(sourceIDs) == 0 || len(sourceIDs) > 20 {
		return fmt.Errorf("%s must cite 1 through 20 sources", name)
	}
	seen := make(map[string]struct{}, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		if _, exists := known[sourceID]; !exists {
			return fmt.Errorf("%s cites unknown source ID %q", name, sourceID)
		}
		if _, exists := seen[sourceID]; exists {
			return fmt.Errorf("%s cites source ID %q more than once", name, sourceID)
		}
		seen[sourceID] = struct{}{}
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	return slices.Contains(allowed, value)
}

func Assemble(_ Request, response ProviderResponse) Result {
	return Merge(Result{}, response)
}

// Merge replaces only independently valid sections, preserving the last valid
// value for every section rejected in the latest attempt.
func Merge(previous Result, response ProviderResponse) Result {
	result := previous
	if response.SectionValid(SectionReviewCognitiveLoad) {
		load := response.ReviewCognitiveLoad
		result.ReviewCognitiveLoad = &load
	}
	if response.SectionValid(SectionWaitingOn) {
		result.WaitingOn = response.WaitingOn
	}
	if response.SectionValid(SectionQualityEvaluations) {
		result.QualityEvaluations = response.QualityEvaluations
		result.QualityLevel = qualityLevel(response.QualityEvaluations)
	}
	if result.WaitingOn == nil {
		result.WaitingOn = make([]WaitingState, 0)
	}
	if result.QualityEvaluations == nil {
		result.QualityEvaluations = make([]QualityEvaluation, 0)
	}
	if result.QualityLevel == "" {
		result.QualityLevel = quality.LevelNoConcerns
	}
	result.Status = "complete"
	return result
}

func qualityLevel(evaluations []QualityEvaluation) string {
	findings := make([]quality.Finding, 0, len(evaluations))
	for _, evaluation := range evaluations {
		if evaluation.Status != QualityStatusFinding || evaluation.Finding == nil {
			continue
		}
		findings = append(findings, quality.Finding{
			Severity: evaluation.Finding.Severity,
		})
	}
	return quality.Level(findings)
}
