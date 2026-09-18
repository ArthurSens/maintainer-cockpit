// Package correlation builds bounded collection-level model inputs and
// validates model-inferred feature groups.
package correlation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	SchemaVersion = "correlation.v1"
	PromptVersion = "correlation-prompt.v1"

	MaxPullRequestsPerRequest = 200
	MaxFilesPerPullRequest    = 20
	MaxRelationshipsPerPR     = 50
	MaxTitleBytes             = 500
	MaxGroupsPerResponse      = 100
	MaxMembersPerGroup        = 50
	MaxEdgesPerGroup          = 200
	MaxNameRunes              = 120
	MaxDescriptionRunes       = 500
	MaxReasonRunes            = 500
)

const (
	EdgeDuplicated      = "duplicated"
	EdgeCompetingDesign = "competing_design"
	EdgeStackedOnTopOf  = "stacked_on_top_of"
	EdgeRelated         = "related"
)

var edgeTypes = map[string]struct{}{
	EdgeDuplicated: {}, EdgeCompetingDesign: {}, EdgeStackedOnTopOf: {}, EdgeRelated: {},
}

// Source is collected GitHub evidence. EntityID identifies the referenced
// issue or pull request while ID identifies the exact evidence record.
type Source struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	EntityID   string `json:"entityID,omitempty"`
	Repository string `json:"repository,omitempty"`
	Number     int    `json:"number,omitempty"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state,omitempty"`
	URL        string `json:"url,omitempty"`
}

// PullRequest is one bounded open collection member sent for correlation.
type PullRequest struct {
	SourceID      string   `json:"sourceID"`
	Repository    string   `json:"repository"`
	Number        int      `json:"number"`
	Title         string   `json:"title"`
	Author        string   `json:"author,omitempty"`
	URL           string   `json:"url"`
	State         string   `json:"state"`
	Files         []string `json:"files"`
	Relationships []Source `json:"relationships"`
}

type Input struct {
	CollectionID string
	PullRequests []PullRequest
}

// Request is one independently correlated collection chunk.
type Request struct {
	SchemaVersion string        `json:"schemaVersion"`
	PromptVersion string        `json:"promptVersion"`
	InputRevision string        `json:"inputRevision"`
	CollectionID  string        `json:"collectionID"`
	Chunk         int           `json:"chunk"`
	Chunks        int           `json:"chunks"`
	PullRequests  []PullRequest `json:"pullRequests"`
}

// Member is a validated graph member.
type Member struct {
	SourceID   string `json:"sourceID"`
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"`
	URL        string `json:"url,omitempty"`
}

// Evidence records which pull request supplied an evidence source and which
// referenced entity, if any, it describes.
type Evidence struct {
	Owner    string
	EntityID string
}

type Edge struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Type       string   `json:"type"`
	Confidence string   `json:"confidence"`
	Reason     string   `json:"reason"`
	SourceIDs  []string `json:"sourceIDs"`
}

type Group struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Confidence  string   `json:"confidence"`
	SourceIDs   []string `json:"sourceIDs"`
	Members     []Member `json:"members"`
	Edges       []Edge   `json:"edges"`
}

type Result struct {
	Groups []Group `json:"groups"`
}

type providerResponse struct {
	Groups []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Confidence  string   `json:"confidence"`
		SourceIDs   []string `json:"sourceIDs"`
		Members     []string `json:"members"`
		Edges       []Edge   `json:"edges"`
	} `json:"groups"`
}

func PullRequestSourceID(repository string, number int) string {
	replacer := strings.NewReplacer("/", "_", "#", "_", " ", "_")
	return fmt.Sprintf("PR_%s_%d", replacer.Replace(repository), number)
}

func EntitySourceID(kind, repository string, number int) string {
	replacer := strings.NewReplacer("/", "_", "#", "_", " ", "_")
	return fmt.Sprintf("%s_%s_%d", strings.ToUpper(kind), replacer.Replace(repository), number)
}

// BuildRequests bounds, sorts, and chunks a collection snapshot.
func BuildRequests(input Input) ([]Request, error) {
	if input.CollectionID == "" {
		return nil, errors.New("correlation input has no collection identity")
	}
	pullRequests := append([]PullRequest(nil), input.PullRequests...)
	sort.Slice(pullRequests, func(i, j int) bool {
		return pullRequests[i].SourceID < pullRequests[j].SourceID
	})
	for index := range pullRequests {
		pr := &pullRequests[index]
		if pr.SourceID == "" || pr.Repository == "" || pr.Number < 1 || pr.Title == "" || pr.URL == "" {
			return nil, fmt.Errorf("correlation pull request %d has missing identity", index)
		}
		pr.Title = truncateUTF8(pr.Title, MaxTitleBytes)
		pr.Files = sortedUnique(pr.Files)
		if len(pr.Files) > MaxFilesPerPullRequest {
			pr.Files = pr.Files[:MaxFilesPerPullRequest]
		}
		sort.Slice(pr.Relationships, func(i, j int) bool {
			return pr.Relationships[i].ID < pr.Relationships[j].ID
		})
		pr.Relationships = uniqueSources(pr.Relationships)
		if len(pr.Relationships) > MaxRelationshipsPerPR {
			pr.Relationships = pr.Relationships[:MaxRelationshipsPerPR]
		}
		for sourceIndex := range pr.Relationships {
			pr.Relationships[sourceIndex].Title = truncateUTF8(
				pr.Relationships[sourceIndex].Title, MaxTitleBytes,
			)
		}
	}
	if len(pullRequests) == 0 {
		return []Request{}, nil
	}
	chunks := (len(pullRequests) + MaxPullRequestsPerRequest - 1) / MaxPullRequestsPerRequest
	requests := make([]Request, 0, chunks)
	for chunk := range chunks {
		start := chunk * MaxPullRequestsPerRequest
		end := min(start+MaxPullRequestsPerRequest, len(pullRequests))
		request := Request{
			SchemaVersion: SchemaVersion, PromptVersion: PromptVersion,
			CollectionID: input.CollectionID, Chunk: chunk + 1, Chunks: chunks,
			PullRequests: pullRequests[start:end],
		}
		revisionBody, err := json.Marshal(struct {
			CollectionID string        `json:"collectionID"`
			Chunk        int           `json:"chunk"`
			Chunks       int           `json:"chunks"`
			PullRequests []PullRequest `json:"pullRequests"`
		}{request.CollectionID, request.Chunk, request.Chunks, request.PullRequests})
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(revisionBody)
		request.InputRevision = hex.EncodeToString(sum[:])
		requests = append(requests, request)
	}
	return requests, nil
}

// KnownSources returns strict member and evidence allowlists for validation.
func KnownSources(request Request) (map[string]Member, map[string]Evidence) {
	members := make(map[string]Member)
	evidence := make(map[string]Evidence)
	for _, pr := range request.PullRequests {
		member := Member{
			SourceID: pr.SourceID, Repository: pr.Repository, Number: pr.Number,
			Title: pr.Title, State: pr.State, URL: pr.URL,
		}
		members[pr.SourceID] = member
		evidence[pr.SourceID] = Evidence{Owner: pr.SourceID, EntityID: pr.SourceID}
		for _, source := range pr.Relationships {
			evidence[source.ID] = Evidence{Owner: pr.SourceID, EntityID: source.EntityID}
			if strings.HasPrefix(source.EntityID, "PR_") &&
				(source.State == "closed" || source.State == "merged") {
				if _, exists := members[source.EntityID]; !exists {
					members[source.EntityID] = Member{
						SourceID: source.EntityID, Repository: source.Repository,
						Number: source.Number, Title: source.Title,
						State: source.State, URL: source.URL,
					}
				}
			}
		}
	}
	return members, evidence
}

// ValidateResponse rejects malformed, unsupported, and ungrounded groups.
func ValidateResponse(
	body []byte, knownMembers map[string]Member, knownEvidence map[string]Evidence,
) (Result, error) {
	var response providerResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return Result{}, fmt.Errorf("decode correlation response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Result{}, errors.New("correlation response must contain exactly one JSON document")
	}
	if len(response.Groups) > MaxGroupsPerResponse {
		return Result{}, fmt.Errorf("correlation response has more than %d groups", MaxGroupsPerResponse)
	}
	result := Result{Groups: make([]Group, 0, len(response.Groups))}
	seenGroups := make(map[string]struct{})
	for index, candidate := range response.Groups {
		candidate.Name = normalizeText(candidate.Name)
		candidate.Description = normalizeText(candidate.Description)
		if candidate.Name == "" || utf8.RuneCountInString(candidate.Name) > MaxNameRunes {
			return Result{}, fmt.Errorf("group %d has invalid name", index)
		}
		if candidate.Description == "" ||
			utf8.RuneCountInString(candidate.Description) > MaxDescriptionRunes {
			return Result{}, fmt.Errorf("group %d has invalid description", index)
		}
		if !validConfidence(candidate.Confidence) {
			return Result{}, fmt.Errorf("group %d has invalid confidence", index)
		}
		if len(candidate.Members) < 2 || len(candidate.Members) > MaxMembersPerGroup {
			return Result{}, fmt.Errorf("group %d must contain 2 through %d members", index, MaxMembersPerGroup)
		}
		memberIDs := sortedUnique(candidate.Members)
		if len(memberIDs) != len(candidate.Members) {
			return Result{}, fmt.Errorf("group %d has duplicate members", index)
		}
		group := Group{
			Name: candidate.Name, Description: candidate.Description,
			Confidence: candidate.Confidence, SourceIDs: sortedUnique(candidate.SourceIDs),
			Edges: candidate.Edges,
		}
		openMembers := 0
		memberSet := make(map[string]struct{}, len(memberIDs))
		for _, id := range memberIDs {
			member, exists := knownMembers[id]
			if !exists {
				return Result{}, fmt.Errorf("group %d cites unknown member %q", index, id)
			}
			memberSet[id] = struct{}{}
			group.Members = append(group.Members, member)
			if member.State == "open" {
				openMembers++
			}
		}
		if openMembers == 0 {
			return Result{}, fmt.Errorf("group %d has no open pull requests", index)
		}
		if err := validateEvidence(candidate.SourceIDs, knownEvidence, memberSet); err != nil {
			return Result{}, fmt.Errorf("group %d: %w", index, err)
		}
		if len(group.Edges) > MaxEdgesPerGroup {
			return Result{}, fmt.Errorf("group %d has too many edges", index)
		}
		seenEdges := make(map[string]struct{})
		for edgeIndex := range group.Edges {
			edge := &group.Edges[edgeIndex]
			if _, exists := edgeTypes[edge.Type]; !exists {
				return Result{}, fmt.Errorf("group %d edge %d has invalid type %q", index, edgeIndex, edge.Type)
			}
			if !validConfidence(edge.Confidence) {
				return Result{}, fmt.Errorf("group %d edge %d has invalid confidence", index, edgeIndex)
			}
			edge.Reason = normalizeText(edge.Reason)
			if edge.Reason == "" || utf8.RuneCountInString(edge.Reason) > MaxReasonRunes {
				return Result{}, fmt.Errorf("group %d edge %d has invalid reason", index, edgeIndex)
			}
			if edge.From == edge.To {
				return Result{}, fmt.Errorf("group %d edge %d is self-referential", index, edgeIndex)
			}
			if _, exists := memberSet[edge.From]; !exists {
				return Result{}, fmt.Errorf("group %d edge %d cites unknown from member", index, edgeIndex)
			}
			if _, exists := memberSet[edge.To]; !exists {
				return Result{}, fmt.Errorf("group %d edge %d cites unknown to member", index, edgeIndex)
			}
			endpoints := map[string]struct{}{edge.From: {}, edge.To: {}}
			if err := validateEvidence(edge.SourceIDs, knownEvidence, endpoints); err != nil {
				return Result{}, fmt.Errorf("group %d edge %d: %w", index, edgeIndex, err)
			}
			key := edgeKey(*edge)
			if _, exists := seenEdges[key]; exists {
				return Result{}, fmt.Errorf("group %d has duplicate edge", index)
			}
			seenEdges[key] = struct{}{}
			edge.SourceIDs = sortedUnique(edge.SourceIDs)
		}
		group.ID = groupID(memberIDs)
		if _, exists := seenGroups[group.ID]; exists {
			return Result{}, fmt.Errorf("duplicate group %q", group.ID)
		}
		seenGroups[group.ID] = struct{}{}
		sort.Slice(group.Members, func(i, j int) bool { return group.Members[i].SourceID < group.Members[j].SourceID })
		sort.Slice(group.Edges, func(i, j int) bool { return edgeKey(group.Edges[i]) < edgeKey(group.Edges[j]) })
		result.Groups = append(result.Groups, group)
	}
	sort.Slice(result.Groups, func(i, j int) bool { return result.Groups[i].ID < result.Groups[j].ID })
	return result, nil
}

func validateEvidence(
	ids []string, known map[string]Evidence, relevant map[string]struct{},
) error {
	if len(ids) == 0 || len(ids) > 20 {
		return errors.New("sourceIDs must contain 1 through 20 items")
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		evidence, exists := known[id]
		if !exists {
			return fmt.Errorf("unknown source ID %q", id)
		}
		_, ownerRelevant := relevant[evidence.Owner]
		_, entityRelevant := relevant[evidence.EntityID]
		if !ownerRelevant && !entityRelevant {
			return fmt.Errorf("source ID %q is unrelated to the cited members", id)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate source ID %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func validConfidence(value string) bool {
	return value == "medium" || value == "high"
}

func groupID(memberIDs []string) string {
	sum := sha256.Sum256([]byte(strings.Join(memberIDs, "\x00")))
	return "group-" + hex.EncodeToString(sum[:8])
}

func edgeKey(edge Edge) string {
	from, to := edge.From, edge.To
	if edge.Type != EdgeStackedOnTopOf && to < from {
		from, to = to, from
	}
	return strings.Join([]string{edge.Type, from, to}, "\x00")
}

// TextGraph renders the validated graph for accessible UI and later analysis.
func TextGraph(group Group) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# %s\n%s\n", group.Name, group.Description)
	members := append([]Member(nil), group.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].SourceID < members[j].SourceID })
	for _, member := range members {
		fmt.Fprintf(
			&builder, "PR %s#%d [%s]: %s\n",
			member.Repository, member.Number, member.State, member.Title,
		)
	}
	edges := append([]Edge(nil), group.Edges...)
	sort.Slice(edges, func(i, j int) bool { return edgeKey(edges[i]) < edgeKey(edges[j]) })
	for _, edge := range edges {
		arrow := "--" + edge.Type + "--"
		if edge.Type == EdgeStackedOnTopOf {
			arrow = "--" + edge.Type + "-->"
		}
		fmt.Fprintf(&builder, "%s %s %s", edge.From, arrow, edge.To)
		if edge.Reason != "" {
			fmt.Fprintf(&builder, ": %s", edge.Reason)
		}
		builder.WriteByte('\n')
	}
	return builder.String()
}

// ResponseSchema is the strict structured-output schema supplied to providers.
func ResponseSchema() map[string]any {
	sourceIDs := map[string]any{
		"type": "array", "minItems": 1, "maxItems": 20,
		"items": map[string]any{"type": "string"},
	}
	edge := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"from": map[string]any{"type": "string"},
			"to":   map[string]any{"type": "string"},
			"type": map[string]any{
				"type": "string",
				"enum": []string{
					EdgeDuplicated, EdgeCompetingDesign, EdgeStackedOnTopOf, EdgeRelated,
				},
			},
			"confidence": map[string]any{"type": "string", "enum": []string{"medium", "high"}},
			"reason": map[string]any{
				"type": "string", "minLength": 1, "maxLength": MaxReasonRunes,
			},
			"sourceIDs": sourceIDs,
		},
		"required": []string{"from", "to", "type", "confidence", "reason", "sourceIDs"},
	}
	group := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"name": map[string]any{
				"type": "string", "minLength": 1, "maxLength": MaxNameRunes,
			},
			"description": map[string]any{
				"type": "string", "minLength": 1, "maxLength": MaxDescriptionRunes,
			},
			"confidence": map[string]any{"type": "string", "enum": []string{"medium", "high"}},
			"sourceIDs":  sourceIDs,
			"members": map[string]any{
				"type": "array", "minItems": 2, "maxItems": MaxMembersPerGroup,
				"items": map[string]any{"type": "string"},
			},
			"edges": map[string]any{
				"type": "array", "maxItems": MaxEdgesPerGroup, "items": edge,
			},
		},
		"required": []string{
			"name", "description", "confidence", "sourceIDs", "members", "edges",
		},
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"groups": map[string]any{
				"type": "array", "maxItems": MaxGroupsPerResponse, "items": group,
			},
		},
		"required": []string{"groups"},
	}
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

func normalizeText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func sortedUnique(values []string) []string {
	values = append([]string(nil), values...)
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || value != result[len(result)-1] {
			result = append(result, value)
		}
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
