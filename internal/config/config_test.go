package config

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const acmeGitHub = `github:
  schedule: "0 * * * *"
  authentication:
    profiles:
      app:
        type: fine_grained_pat
        token: {environment: GITHUB_TOKEN}
    owners:
      acme:
        profiles: [app]
`

func acmeConfig(collections string) string {
	return acmeGitHub + "collections:\n" + collections
}

func TestJSONSchemaIsDerivedFromConfigurationStructs(t *testing.T) {
	t.Parallel()

	body, err := JSONSchema()
	if err != nil {
		t.Fatalf("JSONSchema() error = %v", err)
	}
	var schema struct {
		Reference   string `json:"$ref"`
		Definitions map[string]struct {
			AdditionalProperties bool                       `json:"additionalProperties"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatalf("JSONSchema() returned invalid JSON: %v", err)
	}
	if schema.Reference != "#/$defs/Config" {
		t.Errorf("$ref = %q, want Config definition", schema.Reference)
	}
	for _, value := range []any{
		Config{},
		GitHub{},
		GitHubAuthentication{},
		GitHubAuthenticationOwner{},
		GitHubAuthenticationProfile{},
		UserAuth{},
		SecretReference{},
		Operations{},
		ModelProvider{},
		Collection{},
		FeatureCorrelation{},
		Authorization{},
		RepositoryDiscovery{},
		Quality{},
		DescriptionDiffMismatch{},
		BroadAdditionsOnly{},
		Contribution{},
		UnusualActivity{},
	} {
		typ := reflect.TypeOf(value)
		definition, ok := schema.Definitions[typ.Name()]
		if !ok {
			t.Errorf("$defs does not define %q", typ.Name())
			continue
		}
		if definition.AdditionalProperties {
			t.Errorf("$defs.%s allows unknown fields", typ.Name())
		}
		for field := range typ.Fields() {
			name, options, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			if name == "-" {
				continue
			}
			if _, ok := definition.Properties[name]; !ok {
				t.Errorf("$defs.%s does not describe field %q", typ.Name(), name)
			}
			if !strings.Contains(options, "omitempty") &&
				!slices.Contains(definition.Required, name) {
				t.Errorf("$defs.%s does not require field %q", typ.Name(), name)
			}
		}
	}
	if _, exists := schema.Definitions["Config"].Properties["version"]; exists {
		t.Error("$defs.Config unexpectedly describes versioned configuration")
	}
}

func TestConfigurationRequiresCurrentSchedules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "GitHub",
			body: strings.Replace(acmeConfig(`  - id: acme
    name: Acme
    repositories: [acme/widgets]
`), `  schedule: "0 * * * *"`+"\n", "", 1),
			want: "github.schedule",
		},
		{
			name: "contribution",
			body: acmeConfig(`  - id: acme
    name: Acme
    repositories: [acme/widgets]
    contribution:
      established_merged_prs: 5
`),
			want: "contribution.schedule",
		},
		{
			name: "feature correlation",
			body: acmeGitHub + `model_providers:
  hosted:
    type: openai
    base_url: https://models.example.test
    model: example-model
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
    model_provider: hosted
    feature_correlation: {}
`,
			want: "feature_correlation.schedule",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(test.body))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCronSchedulesUseFiveFields(t *testing.T) {
	t.Parallel()

	for _, schedule := range []string{"", "@daily", "0 0 0 * * *", "CRON_TZ=America/New_York 0 9 * * *"} {
		body := strings.Replace(acmeConfig(`  - id: acme
    name: Acme
    repositories: [acme/widgets]
`), "0 * * * *", schedule, 1)
		if _, err := Parse([]byte(body)); err == nil ||
			!strings.Contains(err.Error(), "standard five-field UTC cron expression") {
			t.Errorf("Parse(schedule %q) error = %v", schedule, err)
		}
	}
}

func TestParseValidConfiguration(t *testing.T) {
	t.Parallel()

	got, err := Parse([]byte(acmeConfig(`  - id: exporters
    name: Exporter maintenance
    description: Pull requests across exporter repositories.
    repositories:
      - acme/node_exporter
      - acme/blackbox_exporter
`)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(got.Collections) != 1 {
		t.Fatalf("len(Collections) = %d, want 1", len(got.Collections))
	}
	if got.Collections[0].ID != "exporters" {
		t.Errorf("Collection.ID = %q, want exporters", got.Collections[0].ID)
	}
	if len(got.Collections[0].Repositories) != 2 {
		t.Errorf("len(Collection.Repositories) = %d, want 2", len(got.Collections[0].Repositories))
	}
}

func TestConfigurationRequiresGitHubAuthentication(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte(`github:
  schedule: "0 * * * *"
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`))
	if err == nil || !strings.Contains(err.Error(), "collections require github.authentication") {
		t.Fatalf("Parse() error = %v, want github.authentication requirement", err)
	}
}

func TestParseRejectsVersionField(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte(`
version: 12
collections:
  - id: acme
    name: Acme
    repositories: [acme/widgets]
`))
	if err == nil || !strings.Contains(err.Error(), "field version not found") {
		t.Fatalf("Parse() error = %v, want unknown version field", err)
	}
}

func TestParseGitHubAuthenticationProfiles(t *testing.T) {
	t.Parallel()

	got, err := Parse([]byte(`github:
  schedule: "0 * * * *"
  authentication:
    profiles:
      acme-app:
        type: github_app
        app_id: 12345
        installation_id: 67890
        private_key: {environment: ACME_GITHUB_PRIVATE_KEY}
      other-pat:
        type: fine_grained_pat
        token: {file: /run/secrets/other-token}
    owners:
      acme:
        profiles: [acme-app]
      other:
        profiles: [other-pat]
collections:
  - id: projects
    name: Projects
    repositories: [acme/widgets, other/tools]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	authentication := got.GitHub.Authentication
	if authentication == nil {
		t.Fatal("GitHub.Authentication = nil, want configured authentication")
	}
	app := authentication.Profiles["acme-app"]
	if app.Type != "github_app" || app.AppID == nil || *app.AppID != 12345 ||
		app.InstallationID == nil || *app.InstallationID != 67890 ||
		app.PrivateKey == nil || app.PrivateKey.Environment != "ACME_GITHUB_PRIVATE_KEY" {
		t.Errorf("app profile = %+v, want configured GitHub App", app)
	}
	pat := authentication.Profiles["other-pat"]
	if pat.Type != "fine_grained_pat" || pat.Token == nil || pat.Token.File != "/run/secrets/other-token" {
		t.Errorf("PAT profile = %+v, want configured token", pat)
	}
}

func TestGitHubAuthenticationProfileValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		authentication string
		repositories   string
		want           string
	}{
		{
			name: "app ID is positive",
			authentication: `
    profiles:
      app: {type: github_app, app_id: 0, installation_id: 2, private_key: {environment: KEY}}
    owners:
      acme: {profiles: [app]}`,
			repositories: "[acme/widgets]",
			want:         "app_id must be positive",
		},
		{
			name: "app installation ID is positive",
			authentication: `
    profiles:
      app: {type: github_app, app_id: 1, installation_id: 0, private_key: {environment: KEY}}
    owners:
      acme: {profiles: [app]}`,
			repositories: "[acme/widgets]",
			want:         "installation_id must be positive",
		},
		{
			name: "app private key is required",
			authentication: `
    profiles:
      app: {type: github_app, app_id: 1, installation_id: 2}
    owners:
      acme: {profiles: [app]}`,
			repositories: "[acme/widgets]",
			want:         "private_key must configure exactly one",
		},
		{
			name: "PAT token is required",
			authentication: `
    profiles:
      pat: {type: fine_grained_pat}
    owners:
      acme: {profiles: [pat]}`,
			repositories: "[acme/widgets]",
			want:         "token must configure exactly one",
		},
		{
			name: "profile type is explicit",
			authentication: `
    profiles:
      credential: {token: {environment: TOKEN}}
    owners:
      acme: {profiles: [credential]}`,
			repositories: "[acme/widgets]",
			want:         "type must be github_app or fine_grained_pat",
		},
		{
			name: "app rejects PAT fields",
			authentication: `
    profiles:
      app: {type: github_app, app_id: 1, installation_id: 2, private_key: {environment: KEY}, token: {environment: TOKEN}}
    owners:
      acme: {profiles: [app]}`,
			repositories: "[acme/widgets]",
			want:         "github_app must not configure token",
		},
		{
			name: "PAT rejects app fields",
			authentication: `
    profiles:
      pat: {type: fine_grained_pat, token: {environment: TOKEN}, app_id: 1}
    owners:
      acme: {profiles: [pat]}`,
			repositories: "[acme/widgets]",
			want:         "fine_grained_pat must not configure",
		},
		{
			name: "owner profiles have no duplicates",
			authentication: `
    profiles:
      app: {type: fine_grained_pat, token: {environment: TOKEN}}
    owners:
      acme: {profiles: [app, app]}`,
			repositories: "[acme/widgets]",
			want:         "duplicate profile",
		},
		{
			name: "owner profiles are known",
			authentication: `
    profiles:
      app: {type: fine_grained_pat, token: {environment: TOKEN}}
    owners:
      acme: {profiles: [missing]}`,
			repositories: "[acme/widgets]",
			want:         `unknown profile "missing"`,
		},
		{
			name: "profiles belong to exactly one owner",
			authentication: `
    profiles:
      app: {type: fine_grained_pat, token: {environment: TOKEN}}
    owners:
      acme: {profiles: [app]}
      other: {profiles: [app]}`,
			repositories: "[acme/widgets, other/widgets]",
			want:         "referenced by more than one owner",
		},
		{
			name: "profiles cannot be unreferenced",
			authentication: `
    profiles:
      app: {type: fine_grained_pat, token: {environment: TOKEN}}
      spare: {type: fine_grained_pat, token: {environment: SPARE_TOKEN}}
    owners:
      acme: {profiles: [app]}`,
			repositories: "[acme/widgets]",
			want:         `profile "spare" is not referenced`,
		},
		{
			name: "all repository owners are covered",
			authentication: `
    profiles:
      app: {type: fine_grained_pat, token: {environment: TOKEN}}
    owners:
      acme: {profiles: [app]}`,
			repositories: "[acme/widgets, other/widgets]",
			want:         `repository owner "other" has no authentication profiles`,
		},
		{
			name: "profile names are unique ignoring case",
			authentication: `
    profiles:
      app: {type: fine_grained_pat, token: {environment: TOKEN}}
      App: {type: fine_grained_pat, token: {environment: OTHER_TOKEN}}
    owners:
      acme: {profiles: [app]}`,
			repositories: "[acme/widgets]",
			want:         "profile names differ only by case",
		},
		{
			name: "owner names are unique ignoring case",
			authentication: `
    profiles:
      first: {type: fine_grained_pat, token: {environment: TOKEN}}
      second: {type: fine_grained_pat, token: {environment: OTHER_TOKEN}}
    owners:
      acme: {profiles: [first]}
      Acme: {profiles: [second]}`,
			repositories: "[acme/widgets]",
			want:         "owner names differ only by case",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := `github:
  schedule: "0 * * * *"
  authentication:` + test.authentication + `
collections:
  - id: projects
    name: Projects
    repositories: ` + test.repositories + "\n"
			_, err := Parse([]byte(body))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseRejectsLegacyGitHubAppFields(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte(`github:
  schedule: "0 * * * *"
  app_id: 1
  installation_id: 2
  private_key: {environment: KEY}
collections:
  - id: projects
    name: Projects
    repositories: [acme/widgets]
`))
	if err == nil || !strings.Contains(err.Error(), "field app_id not found") {
		t.Fatalf("Parse() error = %v, want strict legacy-field rejection", err)
	}
}

func TestUserAuthSupportsCollectionAuthorization(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte(`external_base_url: https://cockpit.example.test
github:
  schedule: "0 * * * *"
  user_auth:
    client_id: Iv1.example
    client_secret: {environment: GITHUB_CLIENT_SECRET}
  authentication:
    profiles:
      app: {type: fine_grained_pat, token: {environment: GITHUB_TOKEN}}
    owners:
      acme: {profiles: [app]}
collections:
  - id: projects
    name: Projects
    repositories: [acme/widgets]
    authorization:
      users: [42]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestParseRepositoryDiscoveryConfiguration(t *testing.T) {
	t.Parallel()

	got, err := Parse([]byte(acmeConfig(`  - id: interoperability
    name: Interoperability
    repositories: [acme/widgets, acme/tools]
    discovery:
      acme/widgets:
        mode: all_open
      acme/tools:
        mode: search
        query: prometheus in:title,body
`)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	discovery := got.Collections[0].Discovery["acme/tools"]
	if discovery.Mode != "search" || discovery.Query != "prometheus in:title,body" {
		t.Errorf("search discovery = %+v", discovery)
	}
}

func TestParseQualityThresholdConfiguration(t *testing.T) {
	t.Parallel()

	got, err := Parse([]byte(acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    quality:
      description_diff_mismatch:
        min_references: 2
        strong_mismatch_min_references: 5
      broad_additions_only:
        min_files: 25
        min_additions: 1000
        max_deletions: 3
`)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	quality := got.Collections[0].Quality
	if quality == nil {
		t.Fatal("Collection.Quality = nil, want configured thresholds")
	}
	mismatch := quality.DescriptionDiffMismatch
	if mismatch == nil || mismatch.MinReferences == nil || *mismatch.MinReferences != 2 ||
		mismatch.StrongMismatchMinReferences == nil || *mismatch.StrongMismatchMinReferences != 5 {
		t.Errorf("DescriptionDiffMismatch = %+v, want min 2 and strong 5", mismatch)
	}
	broad := quality.BroadAdditionsOnly
	if broad == nil || broad.MinFiles == nil || *broad.MinFiles != 25 ||
		broad.MinAdditions == nil || *broad.MinAdditions != 1000 ||
		broad.MaxDeletions == nil || *broad.MaxDeletions != 3 {
		t.Errorf("BroadAdditionsOnly = %+v, want files 25, additions 1000, deletions 3", broad)
	}
}

func TestParseContributionThresholdConfiguration(t *testing.T) {
	t.Parallel()

	got, err := Parse([]byte(acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    contribution:
      schedule: "0 3 * * 1"
      established_merged_prs: 5
      unusual_activity:
        account_age_days: 120
        window_days: 21
        min_repositories: 30
        min_organizations: 4
`)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	contribution := got.Collections[0].Contribution
	if contribution == nil || contribution.Schedule != "0 3 * * 1" ||
		contribution.EstablishedMergedPRs == nil || *contribution.EstablishedMergedPRs != 5 ||
		contribution.UnusualActivity == nil ||
		contribution.UnusualActivity.MinOrganizations == nil ||
		*contribution.UnusualActivity.MinOrganizations != 4 {
		t.Errorf("Contribution = %+v, want configured thresholds", contribution)
	}
}

func TestParseModelProviderProfilesAndCollectionAssignment(t *testing.T) {
	t.Parallel()

	got, err := Parse([]byte(acmeGitHub + `model_providers:
  hosted:
    type: openai
    base_url: https://models.example.test
    model: example-model
    api_key: {environment: MODEL_API_KEY}
  local:
    type: ollama
    base_url: http://ollama:11434
    model: qwen3
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    model_provider: hosted
    model_fallback_provider: local
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(got.ModelProviders) != 2 ||
		got.ModelProviders["hosted"].Type != "openai" ||
		got.Collections[0].ModelProvider != "hosted" ||
		got.Collections[0].ModelFallbackProvider != "local" {
		t.Errorf("model configuration = %+v, collections = %+v", got.ModelProviders, got.Collections)
	}
}

func TestParseCollectionFeatureCorrelationSchedule(t *testing.T) {
	t.Parallel()

	got, err := Parse([]byte(acmeGitHub + `model_providers:
  hosted:
    type: openai
    base_url: https://models.example.test
    model: example-model
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    model_provider: hosted
    feature_correlation:
      schedule: "0 4 * * *"
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	correlation := got.Collections[0].FeatureCorrelation
	if correlation == nil || correlation.Schedule != "0 4 * * *" {
		t.Errorf("FeatureCorrelation = %+v, want daily schedule", correlation)
	}
}

func TestFeatureCorrelationRequiresProviderAndValidSchedule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "provider",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    feature_correlation:
      schedule: "0 4 * * *"
`),
			want: "requires model_provider",
		},
		{
			name: "valid schedule",
			body: acmeGitHub + `model_providers:
  hosted:
    type: openai
    base_url: https://models.example.test
    model: example-model
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    model_provider: hosted
    feature_correlation:
      schedule: "not a cron"
`,
			want: "standard five-field UTC cron expression",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(test.body))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Parse() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseAuthenticationAndAuthorizationConfiguration(t *testing.T) {
	t.Parallel()

	got, err := Parse([]byte(`external_base_url: https://cockpit.example.test
github:
  schedule: "0 * * * *"
  user_auth:
    client_id: Iv1.example
    client_secret: {environment: GITHUB_CLIENT_SECRET}
  authentication:
    profiles:
      app: {type: fine_grained_pat, token: {environment: GITHUB_TOKEN}}
    owners:
      acme: {profiles: [app]}
deployment_admins: [42]
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    authorization:
      organizations: [acme]
      teams: [acme/maintainers]
      users: [99]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.ExternalBaseURL != "https://cockpit.example.test" ||
		got.GitHub == nil || got.GitHub.UserAuth == nil ||
		got.GitHub.UserAuth.ClientID != "Iv1.example" ||
		len(got.DeploymentAdmins) != 1 || got.DeploymentAdmins[0] != 42 {
		t.Fatalf("authentication configuration = %+v", got)
	}
	authorization := got.Collections[0].Authorization
	if authorization == nil || authorization.Teams[0] != "acme/maintainers" ||
		authorization.Users[0] != 99 {
		t.Errorf("collection authorization = %+v", authorization)
	}
}

func TestParseOperationsConfiguration(t *testing.T) {
	t.Parallel()

	got, err := Parse([]byte(acmeGitHub + `operations:
  reload_secret: {environment: RELOAD_SECRET}
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Operations == nil || got.Operations.ReloadSecret.Environment != "RELOAD_SECRET" {
		t.Errorf("Operations = %+v, want reload secret reference", got.Operations)
	}
}

func TestOperationsConfigurationRequiresReloadSecret(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte(acmeGitHub + `operations: {}
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
`))
	if err == nil || !strings.Contains(err.Error(), "operations.reload_secret") {
		t.Fatalf("Parse() error = %v, want reload secret requirement", err)
	}
}

func TestParseAllowsPartialQualityConfiguration(t *testing.T) {
	t.Parallel()

	got, err := Parse([]byte(acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    quality:
      broad_additions_only:
        min_files: 30
`)))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	quality := got.Collections[0].Quality
	if quality == nil || quality.BroadAdditionsOnly == nil ||
		quality.BroadAdditionsOnly.MinFiles == nil || *quality.BroadAdditionsOnly.MinFiles != 30 {
		t.Fatalf("Quality = %+v, want only min_files configured", quality)
	}
	if quality.DescriptionDiffMismatch != nil {
		t.Errorf("DescriptionDiffMismatch = %+v, want nil for untouched rule", quality.DescriptionDiffMismatch)
	}
	if quality.BroadAdditionsOnly.MinAdditions != nil || quality.BroadAdditionsOnly.MaxDeletions != nil {
		t.Errorf("BroadAdditionsOnly = %+v, want unset thresholds to stay nil", quality.BroadAdditionsOnly)
	}
}

func TestParseRejectsInconsistentRepositoryCasingAcrossCollections(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte(`github:
  schedule: "0 * * * *"
  authentication:
    profiles:
      app: {type: fine_grained_pat, token: {environment: GITHUB_TOKEN}}
    owners:
      Acme: {profiles: [app]}
collections:
  - id: first
    name: First
    repositories: [Acme/Widgets]
  - id: second
    name: Second
    repositories: [acme/widgets]
`))
	if err == nil || !strings.Contains(err.Error(), "canonical casing") {
		t.Fatalf("Parse() error = %v, want canonical casing error", err)
	}
}

func TestParseRejectsInvalidCurrentConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "duplicate collection ID",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
  - id: exporters
    name: Other
    repositories: [acme/tools]
`),
			want: `duplicate collection ID "exporters"`,
		},
		{
			name: "invalid collection ID",
			body: acmeConfig(`  - id: Exporters
    name: Exporters
    repositories: [acme/widgets]
`),
			want: "collection ID",
		},
		{
			name: "missing display name",
			body: acmeConfig(`  - id: exporters
    repositories: [acme/widgets]
`),
			want: "name",
		},
		{
			name: "non-canonical repository",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [https://github.com/acme/widgets]
`),
			want: "owner/repository",
		},
		{
			name: "duplicate repository",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets, acme/widgets]
`),
			want: "duplicate repository",
		},
		{
			name: "case-insensitive duplicate repository",
			body: `github:
  schedule: "0 * * * *"
  authentication:
    profiles:
      app: {type: fine_grained_pat, token: {environment: GITHUB_TOKEN}}
    owners:
      Acme: {profiles: [app]}
collections:
  - id: exporters
    name: Exporters
    repositories: [Acme/widgets, acme/widgets]
`,
			want: "duplicate repository",
		},
		{
			name: "search discovery requires query",
			body: acmeConfig(`  - id: interoperability
    name: Interoperability
    repositories: [acme/widgets]
    discovery:
      acme/widgets:
        mode: search
`),
			want: "query must not be empty",
		},
		{
			name: "discovery repository belongs to collection",
			body: acmeConfig(`  - id: interoperability
    name: Interoperability
    repositories: [acme/widgets]
    discovery:
      acme/tools:
        mode: all_open
`),
			want: "is not listed in repositories",
		},
		{
			name: "quality thresholds are positive",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    quality:
      broad_additions_only:
        min_files: 0
`),
			want: "quality.broad_additions_only.min_files",
		},
		{
			name: "quality deletions are not negative",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    quality:
      broad_additions_only:
        max_deletions: -1
`),
			want: "quality.broad_additions_only.max_deletions",
		},
		{
			name: "strong mismatch does not undercut minimum",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    quality:
      description_diff_mismatch:
        min_references: 4
        strong_mismatch_min_references: 2
`),
			want: "strong_mismatch_min_references",
		},
		{
			name: "contribution thresholds are positive",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    contribution:
      schedule: "0 3 * * 1"
      unusual_activity:
        min_organizations: 0
`),
			want: "contribution.unusual_activity.min_organizations",
		},
		{
			name: "contribution schedule is valid",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    contribution:
      schedule: "not a cron"
`),
			want: "contribution.schedule",
		},
		{
			name: "collection references known primary provider",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    model_provider: missing
`),
			want: `unknown model provider "missing"`,
		},
		{
			name: "fallback is explicit and distinct",
			body: acmeGitHub + `model_providers:
  local:
    type: ollama
    base_url: http://ollama:11434
    model: qwen3
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    model_provider: local
    model_fallback_provider: local
`,
			want: "fallback provider must differ",
		},
		{
			name: "fallback requires primary",
			body: acmeGitHub + `model_providers:
  local:
    type: ollama
    base_url: http://ollama:11434
    model: qwen3
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    model_fallback_provider: local
`,
			want: "fallback provider requires model_provider",
		},
		{
			name: "provider URL is HTTP",
			body: acmeGitHub + `model_providers:
  local:
    type: ollama
    base_url: file:///tmp/model
    model: qwen3
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
`,
			want: "http or https",
		},
		{
			name: "OpenAI API key uses one source",
			body: acmeGitHub + `model_providers:
  hosted:
    type: openai
    base_url: https://models.example.test
    model: example-model
    api_key: {environment: MODEL_KEY, file: /run/secrets/model-key}
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
`,
			want: "exactly one",
		},
		{
			name: "restricted collections require user authentication",
			body: acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    authorization:
      users: [42]
`),
			want: "github.user_auth",
		},
		{
			name: "authentication requires HTTPS external URL",
			body: `external_base_url: http://cockpit.example.test
github:
  schedule: "0 * * * *"
  user_auth:
    client_id: Iv1.example
    client_secret: {environment: GITHUB_CLIENT_SECRET}
  authentication:
    profiles:
      app: {type: fine_grained_pat, token: {environment: GITHUB_TOKEN}}
    owners:
      acme: {profiles: [app]}
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
`,
			want: "external_base_url",
		},
		{
			name: "team uses organization and slug",
			body: `external_base_url: https://cockpit.example.test
github:
  schedule: "0 * * * *"
  user_auth:
    client_id: Iv1.example
    client_secret: {environment: GITHUB_CLIENT_SECRET}
  authentication:
    profiles:
      app: {type: fine_grained_pat, token: {environment: GITHUB_TOKEN}}
    owners:
      acme: {profiles: [app]}
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    authorization:
      teams: [maintainers]
`,
			want: "organization/team-slug",
		},
		{
			name: "numeric identities are positive",
			body: `external_base_url: https://cockpit.example.test
github:
  schedule: "0 * * * *"
  user_auth:
    client_id: Iv1.example
    client_secret: {environment: GITHUB_CLIENT_SECRET}
  authentication:
    profiles:
      app: {type: fine_grained_pat, token: {environment: GITHUB_TOKEN}}
    owners:
      acme: {profiles: [app]}
deployment_admins: [0]
collections:
  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
`,
			want: "deployment_admins",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(test.body))
			if err == nil {
				t.Fatal("Parse() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("Parse() error = %q, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte(acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
    typo: true
`)))
	if err == nil {
		t.Fatal("Parse() error = nil, want unknown-field error")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("Parse() error = %q, want unknown field name", err)
	}
}

func TestParseRejectsMultipleYAMLDocuments(t *testing.T) {
	t.Parallel()

	body := acmeConfig(`  - id: exporters
    name: Exporters
    repositories: [acme/widgets]
`) + "---\n" + acmeConfig(`  - id: other
    name: Other
    repositories: [acme/tools]
`)
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("Parse() error = nil, want multiple-document error")
	}
	if !strings.Contains(err.Error(), "one YAML document") {
		t.Errorf("Parse() error = %q, want one-document detail", err)
	}
}
