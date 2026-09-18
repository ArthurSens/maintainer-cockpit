package correlation

import (
	"strings"
	"testing"
)

func TestBuildRequestsBoundsAndChunksCollectionSnapshot(t *testing.T) {
	t.Parallel()

	pullRequests := make([]PullRequest, MaxPullRequestsPerRequest+1)
	for index := range pullRequests {
		pullRequests[index] = PullRequest{
			SourceID:   PullRequestSourceID("acme/widgets", index+1),
			Repository: "acme/widgets",
			Number:     index + 1,
			Title:      strings.Repeat("feature ", 100),
			URL:        "https://github.com/acme/widgets/pull/1",
			State:      "open",
			Files:      []string{"z.go", "a.go"},
			Relationships: []Source{{
				ID: "evidence", Kind: "closing_issue", EntityID: "ISSUE_acme_widgets_1",
			}},
		}
	}

	requests, err := BuildRequests(Input{CollectionID: "acme", PullRequests: pullRequests})
	if err != nil {
		t.Fatalf("BuildRequests() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("len(BuildRequests()) = %d, want 2", len(requests))
	}
	if len(requests[0].PullRequests) != MaxPullRequestsPerRequest ||
		len(requests[1].PullRequests) != 1 {
		t.Errorf("chunk sizes = %d, %d", len(requests[0].PullRequests), len(requests[1].PullRequests))
	}
	if requests[0].InputRevision == "" || requests[0].Chunk != 1 || requests[0].Chunks != 2 {
		t.Errorf("first request metadata = %+v", requests[0])
	}
}

func TestValidateResponseRequiresKnownMembersEvidenceAndEdgeTypes(t *testing.T) {
	t.Parallel()

	knownMembers := map[string]Member{
		"PR_acme_widgets_1": {SourceID: "PR_acme_widgets_1", State: "open"},
		"PR_acme_widgets_2": {SourceID: "PR_acme_widgets_2", State: "open"},
	}
	knownEvidence := map[string]Evidence{
		"issue:1": {Owner: "PR_acme_widgets_1", EntityID: "ISSUE_acme_widgets_1"},
		"issue:3": {Owner: "PR_acme_widgets_3", EntityID: "ISSUE_acme_widgets_3"},
	}
	valid := []byte(`{"groups":[{
		"name":"Native histogram support",
		"description":"Coordinates server and client support.",
		"confidence":"high",
		"sourceIDs":["issue:1"],
		"members":["PR_acme_widgets_1","PR_acme_widgets_2"],
		"edges":[{
			"from":"PR_acme_widgets_1",
			"to":"PR_acme_widgets_2",
			"type":"stacked_on_top_of",
			"confidence":"medium",
			"reason":"The server work depends on the client type.",
			"sourceIDs":["issue:1"]
		}]
	}]}`)
	if _, err := ValidateResponse(valid, knownMembers, knownEvidence); err != nil {
		t.Fatalf("ValidateResponse(valid) error = %v", err)
	}

	for name, replacement := range map[string]string{
		"unknown member":   `"PR_acme_widgets_2"`,
		"unknown evidence": `"issue:1"`,
		"unknown type":     `"stacked_on_top_of"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			value := map[string]string{
				"unknown member":   `"PR_unknown_repo_3"`,
				"unknown evidence": `"invented"`,
				"unknown type":     `"blocks"`,
			}[name]
			body := strings.Replace(validString(valid), replacement, value, 1)
			if _, err := ValidateResponse([]byte(body), knownMembers, knownEvidence); err == nil {
				t.Fatal("ValidateResponse() error = nil")
			}
		})
	}
	unrelated := strings.ReplaceAll(string(valid), `"issue:1"`, `"issue:3"`)
	if _, err := ValidateResponse([]byte(unrelated), knownMembers, knownEvidence); err == nil {
		t.Fatal("ValidateResponse(unrelated evidence) error = nil")
	}
}

func TestValidateResponseRejectsLowConfidenceAndDuplicateNativeEdge(t *testing.T) {
	t.Parallel()

	members := map[string]Member{
		"PR_acme_widgets_1": {SourceID: "PR_acme_widgets_1", State: "open"},
		"PR_acme_widgets_2": {SourceID: "PR_acme_widgets_2", State: "open"},
	}
	evidence := map[string]Evidence{
		"native-edge": {Owner: "PR_acme_widgets_1", EntityID: "PR_acme_widgets_2"},
	}
	body := `{"groups":[{
		"name":"Feature","description":"Description","confidence":"high","sourceIDs":["native-edge"],
		"members":["PR_acme_widgets_1","PR_acme_widgets_2"],
		"edges":[{
			"from":"PR_acme_widgets_1","to":"PR_acme_widgets_2","type":"related",
			"confidence":"low","reason":"Maybe related.","sourceIDs":["native-edge"]
		}]
	}]}`
	if _, err := ValidateResponse([]byte(body), members, evidence); err == nil {
		t.Fatal("ValidateResponse(low confidence) error = nil")
	}
}

func TestResponseSchemaRequiresNonEmptyGeneratedText(t *testing.T) {
	t.Parallel()

	schema := ResponseSchema()
	properties := schema["properties"].(map[string]any)
	groups := properties["groups"].(map[string]any)
	group := groups["items"].(map[string]any)
	groupProperties := group["properties"].(map[string]any)
	for _, field := range []string{"name", "description"} {
		value := groupProperties[field].(map[string]any)
		if value["minLength"] != 1 {
			t.Errorf("group %s minLength = %#v, want 1", field, value["minLength"])
		}
	}
	edges := groupProperties["edges"].(map[string]any)
	edge := edges["items"].(map[string]any)
	reason := edge["properties"].(map[string]any)["reason"].(map[string]any)
	if reason["minLength"] != 1 {
		t.Errorf("edge reason minLength = %#v, want 1", reason["minLength"])
	}
}

func TestTextGraphIsStableAndReadable(t *testing.T) {
	t.Parallel()

	group := Group{
		ID: "group-1", Name: "Native histogram support",
		Description: "Coordinates server and client support.",
		Members: []Member{
			{SourceID: "PR_acme_client_2", Repository: "acme/client", Number: 2, Title: "Client types", State: "open"},
			{SourceID: "PR_acme_server_1", Repository: "acme/server", Number: 1, Title: "Server support", State: "open"},
		},
		Edges: []Edge{{
			From: "PR_acme_server_1", To: "PR_acme_client_2", Type: EdgeStackedOnTopOf,
		}},
	}

	got := TextGraph(group)
	for _, want := range []string{
		"# Native histogram support",
		"PR acme/client#2 [open]: Client types",
		"PR acme/server#1 [open]: Server support",
		"PR_acme_server_1 --stacked_on_top_of--> PR_acme_client_2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("TextGraph() = %q, want %q", got, want)
		}
	}
}

func validString(body []byte) string {
	return string(body)
}
