// Package config loads and validates Maintainer Cockpit configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ArthurSens/maintainer-cockpit/internal/periodic"
)

var (
	// Collection IDs are stable, URL-safe slugs with a 63-character limit.
	collectionIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	// Repository identities must use a bounded canonical owner/name form.
	repositoryRE   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9_.-]{1,100}$`)
	organizationRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	teamRE         = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[a-z0-9](?:[a-z0-9-]{0,99})$`)
)

// Config is the complete Maintainer Cockpit configuration.
type Config struct {
	ExternalBaseURL  string                   `yaml:"external_base_url,omitempty" json:"externalBaseURL,omitempty"`
	GitHub           *GitHub                  `yaml:"github,omitempty" json:"github,omitempty"`
	ModelProviders   map[string]ModelProvider `yaml:"model_providers,omitempty" json:"modelProviders,omitempty"`
	DeploymentAdmins []int64                  `yaml:"deployment_admins,omitempty" json:"deploymentAdmins,omitempty"`
	Operations       *Operations              `yaml:"operations,omitempty" json:"operations,omitempty"`
	Collections      []Collection             `yaml:"collections" json:"collections"`
}

// GitHub configures collection credentials and optional web application login.
type GitHub struct {
	Schedule       string                `yaml:"schedule" json:"schedule"`
	Authentication *GitHubAuthentication `yaml:"authentication,omitempty" json:"authentication,omitempty"`
	UserAuth       *UserAuth             `yaml:"user_auth,omitempty" json:"userAuth,omitempty"`
}

// GitHubAuthentication assigns named collection credentials to repository owners.
type GitHubAuthentication struct {
	Profiles map[string]GitHubAuthenticationProfile `yaml:"profiles" json:"profiles"`
	Owners   map[string]GitHubAuthenticationOwner   `yaml:"owners" json:"owners"`
}

// GitHubAuthenticationOwner lists credentials in attempted order.
type GitHubAuthenticationOwner struct {
	Profiles []string `yaml:"profiles" json:"profiles"`
}

// GitHubAuthenticationProfile is one explicitly typed collection credential.
type GitHubAuthenticationProfile struct {
	Type           string           `yaml:"type" json:"type"`
	AppID          *int64           `yaml:"app_id,omitempty" json:"appID,omitempty"`
	InstallationID *int64           `yaml:"installation_id,omitempty" json:"installationID,omitempty"`
	PrivateKey     *SecretReference `yaml:"private_key,omitempty" json:"privateKey,omitempty"`
	Token          *SecretReference `yaml:"token,omitempty" json:"token,omitempty"`
}

// UserAuth configures the GitHub App web application login flow.
type UserAuth struct {
	ClientID     string          `yaml:"client_id" json:"clientID"`
	ClientSecret SecretReference `yaml:"client_secret" json:"clientSecret"`
}

// SecretReference names exactly one external source for secret material.
type SecretReference struct {
	Environment string `yaml:"environment,omitempty" json:"environment,omitempty"`
	File        string `yaml:"file,omitempty" json:"file,omitempty"`
}

// Operations configures separately authenticated deployment operations.
type Operations struct {
	ReloadSecret SecretReference `yaml:"reload_secret" json:"reloadSecret"`
}

// ModelProvider is one administrator-configured remote model endpoint.
type ModelProvider struct {
	Type    string          `yaml:"type" json:"type"`
	BaseURL string          `yaml:"base_url" json:"baseURL"`
	Model   string          `yaml:"model" json:"model"`
	APIKey  SecretReference `yaml:"api_key,omitempty" json:"apiKey,omitempty"`
}

// Collection is one public pull-request backlog.
type Collection struct {
	ID                    string                         `yaml:"id" json:"id"`
	Name                  string                         `yaml:"name" json:"name"`
	Description           string                         `yaml:"description,omitempty" json:"description,omitempty"`
	Repositories          []string                       `yaml:"repositories" json:"repositories"`
	Discovery             map[string]RepositoryDiscovery `yaml:"discovery,omitempty" json:"discovery,omitempty"`
	Quality               *Quality                       `yaml:"quality,omitempty" json:"quality,omitempty"`
	Contribution          *Contribution                  `yaml:"contribution,omitempty" json:"contribution,omitempty"`
	ModelProvider         string                         `yaml:"model_provider,omitempty" json:"modelProvider,omitempty"`
	ModelFallbackProvider string                         `yaml:"model_fallback_provider,omitempty" json:"modelFallbackProvider,omitempty"`
	FeatureCorrelation    *FeatureCorrelation            `yaml:"feature_correlation,omitempty" json:"featureCorrelation,omitempty"`
	Authorization         *Authorization                 `yaml:"authorization,omitempty" json:"authorization,omitempty"`
}

// FeatureCorrelation opts a collection into periodic feature correlation.
type FeatureCorrelation struct {
	Schedule string `yaml:"schedule" json:"schedule"`
}

// Authorization identifies GitHub memberships allowed to use restricted
// collection features. Teams use organization/team-slug identities.
type Authorization struct {
	Organizations []string `yaml:"organizations,omitempty" json:"organizations,omitempty"`
	Teams         []string `yaml:"teams,omitempty" json:"teams,omitempty"`
	Users         []int64  `yaml:"users,omitempty" json:"users,omitempty"`
}

// RepositoryDiscovery selects all-open or GitHub search discovery.
type RepositoryDiscovery struct {
	Mode  string `yaml:"mode" json:"mode"`
	Query string `yaml:"query,omitempty" json:"query,omitempty"`
}

// Quality tunes documented deterministic PR quality rule thresholds for one
// collection. Unset values keep the documented product defaults.
type Quality struct {
	DescriptionDiffMismatch *DescriptionDiffMismatch `yaml:"description_diff_mismatch,omitempty" json:"descriptionDiffMismatch,omitempty"`
	BroadAdditionsOnly      *BroadAdditionsOnly      `yaml:"broad_additions_only,omitempty" json:"broadAdditionsOnly,omitempty"`
}

// DescriptionDiffMismatch tunes the description/diff mismatch rule.
type DescriptionDiffMismatch struct {
	MinReferences               *int `yaml:"min_references,omitempty" json:"minReferences,omitempty"`
	StrongMismatchMinReferences *int `yaml:"strong_mismatch_min_references,omitempty" json:"strongMismatchMinReferences,omitempty"`
}

// BroadAdditionsOnly tunes the unusually broad additions-only rule.
type BroadAdditionsOnly struct {
	MinFiles     *int `yaml:"min_files,omitempty" json:"minFiles,omitempty"`
	MinAdditions *int `yaml:"min_additions,omitempty" json:"minAdditions,omitempty"`
	MaxDeletions *int `yaml:"max_deletions,omitempty" json:"maxDeletions,omitempty"`
}

// Contribution tunes factual author-context thresholds for one collection.
type Contribution struct {
	Schedule             string           `yaml:"schedule" json:"schedule"`
	EstablishedMergedPRs *int             `yaml:"established_merged_prs,omitempty" json:"establishedMergedPRs,omitempty"`
	UnusualActivity      *UnusualActivity `yaml:"unusual_activity,omitempty" json:"unusualActivity,omitempty"`
}

// UnusualActivity tunes the independent experimental public author signal.
type UnusualActivity struct {
	AccountAgeDays   *int `yaml:"account_age_days,omitempty" json:"accountAgeDays,omitempty"`
	WindowDays       *int `yaml:"window_days,omitempty" json:"windowDays,omitempty"`
	MinRepositories  *int `yaml:"min_repositories,omitempty" json:"minRepositories,omitempty"`
	MinOrganizations *int `yaml:"min_organizations,omitempty" json:"minOrganizations,omitempty"`
}

// Load reads and validates configuration from path.
func Load(path string) (Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	config, err := Parse(body)
	if err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	return config, nil
}

// ResolveSecret loads referenced secret material without placing it in normal
// configuration data or error messages.
func ResolveSecret(reference SecretReference) ([]byte, error) {
	if reference.Environment != "" {
		value, exists := os.LookupEnv(reference.Environment)
		if !exists || value == "" {
			return nil, fmt.Errorf("secret environment variable %q is empty or unset", reference.Environment)
		}
		return []byte(value), nil
	}
	if reference.File != "" {
		file, err := os.Open(reference.File)
		if err != nil {
			return nil, fmt.Errorf("open secret file %q: %w", reference.File, err)
		}
		defer file.Close()
		body, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if err != nil {
			return nil, fmt.Errorf("read secret file %q: %w", reference.File, err)
		}
		if len(body) > 1<<20 {
			return nil, fmt.Errorf("secret file %q exceeds 1 MiB", reference.File)
		}
		if len(body) == 0 {
			return nil, fmt.Errorf("secret file %q is empty", reference.File)
		}
		return body, nil
	}
	return nil, errors.New("secret reference has no source")
}

// Parse strictly decodes and validates one configuration document.
func Parse(body []byte) (Config, error) {
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return Config{}, fmt.Errorf("decode trailing YAML: %w", err)
		}
		return Config{}, errors.New("configuration must contain exactly one YAML document")
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate checks all configuration invariants without changing it.
func (c Config) Validate() error {
	if len(c.Collections) == 0 {
		return errors.New("collections must contain at least one collection")
	}
	if c.GitHub == nil || c.GitHub.Authentication == nil {
		return errors.New("collections require github.authentication")
	}
	if c.GitHub != nil {
		if c.GitHub.Authentication != nil {
			if err := validateGitHubAuthentication(*c.GitHub.Authentication); err != nil {
				return err
			}
			if err := validateSchedule("github.schedule", c.GitHub.Schedule); err != nil {
				return err
			}
		}
		if c.GitHub.UserAuth != nil {
			if strings.TrimSpace(c.GitHub.UserAuth.ClientID) == "" {
				return errors.New("github.user_auth.client_id must not be empty")
			}
			if err := validateSecretReference("github.user_auth.client_secret", c.GitHub.UserAuth.ClientSecret); err != nil {
				return err
			}
			parsed, err := url.Parse(c.ExternalBaseURL)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
				parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
				parsed.Path != "" && parsed.Path != "/" {
				return errors.New("external_base_url must be an HTTPS origin when github.user_auth is configured")
			}
		}
	}
	seenAdmins := make(map[int64]struct{}, len(c.DeploymentAdmins))
	for _, id := range c.DeploymentAdmins {
		if id <= 0 {
			return errors.New("deployment_admins must contain positive GitHub user IDs")
		}
		if _, exists := seenAdmins[id]; exists {
			return fmt.Errorf("deployment_admins contains duplicate GitHub user ID %d", id)
		}
		seenAdmins[id] = struct{}{}
	}
	if len(c.DeploymentAdmins) > 0 && (c.GitHub == nil || c.GitHub.UserAuth == nil) {
		return errors.New("deployment_admins requires github.user_auth")
	}
	if c.Operations != nil {
		if err := validateSecretReference("operations.reload_secret", c.Operations.ReloadSecret); err != nil {
			return err
		}
	}
	for name, provider := range c.ModelProviders {
		if !collectionIDRE.MatchString(name) {
			return fmt.Errorf("model provider name %q must match %s", name, collectionIDRE)
		}
		if provider.Type != "openai" && provider.Type != "ollama" {
			return fmt.Errorf("model provider %q type must be openai or ollama", name)
		}
		parsedURL, err := url.Parse(provider.BaseURL)
		if err != nil || parsedURL.Host == "" ||
			(parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
			return fmt.Errorf("model provider %q base_url must be an absolute http or https URL", name)
		}
		if strings.TrimSpace(provider.Model) == "" || len(provider.Model) > 200 {
			return fmt.Errorf("model provider %q model must contain 1 through 200 characters", name)
		}
		hasEnvironment := strings.TrimSpace(provider.APIKey.Environment) != ""
		hasFile := strings.TrimSpace(provider.APIKey.File) != ""
		if hasEnvironment && hasFile {
			return fmt.Errorf("model provider %q api_key must configure exactly one of environment or file", name)
		}
	}

	ids := make(map[string]struct{}, len(c.Collections))
	canonicalRepositories := make(map[string]string)
	for index, collection := range c.Collections {
		prefix := fmt.Sprintf("collections[%d]", index)
		if !collectionIDRE.MatchString(collection.ID) {
			return fmt.Errorf("%s: collection ID %q must match %s", prefix, collection.ID, collectionIDRE)
		}
		if _, exists := ids[collection.ID]; exists {
			return fmt.Errorf("duplicate collection ID %q", collection.ID)
		}
		ids[collection.ID] = struct{}{}

		if strings.TrimSpace(collection.Name) == "" {
			return fmt.Errorf("%s: name must not be empty", prefix)
		}
		if len(collection.Name) > 100 {
			return fmt.Errorf("%s: name must not exceed 100 characters", prefix)
		}
		if len(collection.Description) > 500 {
			return fmt.Errorf("%s: description must not exceed 500 characters", prefix)
		}
		if len(collection.Repositories) == 0 {
			return fmt.Errorf("%s: repositories must not be empty", prefix)
		}

		repositories := make(map[string]struct{}, len(collection.Repositories))
		for repoIndex, repository := range collection.Repositories {
			if !repositoryRE.MatchString(repository) {
				return fmt.Errorf("%s.repositories[%d]: %q must be a canonical owner/repository identity", prefix, repoIndex, repository)
			}
			identity := strings.ToLower(repository)
			if _, exists := repositories[identity]; exists {
				return fmt.Errorf("%s: duplicate repository %q", prefix, repository)
			}
			if canonical, exists := canonicalRepositories[identity]; exists && canonical != repository {
				return fmt.Errorf(
					"%s.repositories[%d]: repository %q must use the same canonical casing as %q",
					prefix, repoIndex, repository, canonical,
				)
			}
			canonicalRepositories[identity] = repository
			repositories[identity] = struct{}{}
		}
		for repository, discovery := range collection.Discovery {
			if _, exists := repositories[strings.ToLower(repository)]; !exists {
				return fmt.Errorf("%s.discovery: repository %q is not listed in repositories", prefix, repository)
			}
			switch discovery.Mode {
			case "all_open":
				if strings.TrimSpace(discovery.Query) != "" {
					return fmt.Errorf("%s.discovery[%q]: all_open must not define a query", prefix, repository)
				}
			case "search":
				if strings.TrimSpace(discovery.Query) == "" {
					return fmt.Errorf("%s.discovery[%q]: search query must not be empty", prefix, repository)
				}
				if len(discovery.Query) > 256 {
					return fmt.Errorf("%s.discovery[%q]: search query must not exceed 256 characters", prefix, repository)
				}
			default:
				return fmt.Errorf("%s.discovery[%q]: mode must be all_open or search", prefix, repository)
			}
		}
		if err := validateQuality(prefix, collection.Quality); err != nil {
			return err
		}
		if err := validateContribution(prefix, collection.Contribution); err != nil {
			return err
		}
		if collection.ModelProvider != "" {
			if _, exists := c.ModelProviders[collection.ModelProvider]; !exists {
				return fmt.Errorf("%s: unknown model provider %q", prefix, collection.ModelProvider)
			}
		}
		if collection.ModelFallbackProvider != "" {
			if collection.ModelProvider == "" {
				return fmt.Errorf("%s: fallback provider requires model_provider", prefix)
			}
			if collection.ModelFallbackProvider == collection.ModelProvider {
				return fmt.Errorf("%s: fallback provider must differ from model_provider", prefix)
			}
			if _, exists := c.ModelProviders[collection.ModelFallbackProvider]; !exists {
				return fmt.Errorf("%s: unknown model fallback provider %q", prefix, collection.ModelFallbackProvider)
			}
		}
		if collection.FeatureCorrelation != nil {
			if collection.ModelProvider == "" {
				return fmt.Errorf("%s.feature_correlation requires model_provider", prefix)
			}
			if err := validateSchedule(
				prefix+".feature_correlation.schedule", collection.FeatureCorrelation.Schedule,
			); err != nil {
				return err
			}
		}
		if collection.Authorization != nil {
			if c.GitHub == nil || c.GitHub.UserAuth == nil {
				return fmt.Errorf("%s.authorization requires github.user_auth", prefix)
			}
			if err := validateAuthorization(prefix, *collection.Authorization); err != nil {
				return err
			}
		}
	}
	if c.GitHub != nil && c.GitHub.Authentication != nil {
		if err := validateGitHubOwnerCoverage(*c.GitHub.Authentication, canonicalRepositories); err != nil {
			return err
		}
	}
	return nil
}

func validateGitHubAuthentication(authentication GitHubAuthentication) error {
	if len(authentication.Profiles) == 0 {
		return errors.New("github.authentication.profiles must contain at least one profile")
	}
	if len(authentication.Owners) == 0 {
		return errors.New("github.authentication.owners must contain at least one owner")
	}

	canonicalProfiles := make(map[string]string, len(authentication.Profiles))
	for name, profile := range authentication.Profiles {
		if !collectionIDRE.MatchString(strings.ToLower(name)) {
			return fmt.Errorf("github.authentication profile name %q must match %s", name, collectionIDRE)
		}
		key := strings.ToLower(name)
		if existing, exists := canonicalProfiles[key]; exists {
			return fmt.Errorf("github.authentication profile names differ only by case: %q and %q", existing, name)
		}
		canonicalProfiles[key] = name

		prefix := fmt.Sprintf("github.authentication.profiles[%q]", name)
		switch profile.Type {
		case "github_app":
			if profile.AppID == nil || *profile.AppID <= 0 {
				return fmt.Errorf("%s.app_id must be positive", prefix)
			}
			if profile.InstallationID == nil || *profile.InstallationID <= 0 {
				return fmt.Errorf("%s.installation_id must be positive", prefix)
			}
			if profile.PrivateKey == nil {
				return fmt.Errorf("%s.private_key must configure exactly one of environment or file", prefix)
			}
			if err := validateSecretReference(prefix+".private_key", *profile.PrivateKey); err != nil {
				return err
			}
			if profile.Token != nil {
				return fmt.Errorf("%s: github_app must not configure token", prefix)
			}
		case "fine_grained_pat":
			if profile.Token == nil {
				return fmt.Errorf("%s.token must configure exactly one of environment or file", prefix)
			}
			if err := validateSecretReference(prefix+".token", *profile.Token); err != nil {
				return err
			}
			if profile.AppID != nil || profile.InstallationID != nil || profile.PrivateKey != nil {
				return fmt.Errorf("%s: fine_grained_pat must not configure app_id, installation_id, or private_key", prefix)
			}
		default:
			return fmt.Errorf("%s.type must be github_app or fine_grained_pat", prefix)
		}
	}

	canonicalOwners := make(map[string]string, len(authentication.Owners))
	profileOwner := make(map[string]string, len(authentication.Profiles))
	for owner, assignment := range authentication.Owners {
		if !organizationRE.MatchString(owner) {
			return fmt.Errorf("github.authentication owner %q is invalid", owner)
		}
		ownerKey := strings.ToLower(owner)
		if existing, exists := canonicalOwners[ownerKey]; exists {
			return fmt.Errorf("github.authentication owner names differ only by case: %q and %q", existing, owner)
		}
		canonicalOwners[ownerKey] = owner
		if len(assignment.Profiles) == 0 {
			return fmt.Errorf("github.authentication.owners[%q].profiles must not be empty", owner)
		}

		seen := make(map[string]struct{}, len(assignment.Profiles))
		for _, profileName := range assignment.Profiles {
			profileKey := strings.ToLower(profileName)
			if _, exists := seen[profileKey]; exists {
				return fmt.Errorf("github.authentication.owners[%q].profiles contains duplicate profile %q", owner, profileName)
			}
			seen[profileKey] = struct{}{}
			canonicalName, exists := canonicalProfiles[profileKey]
			if !exists || canonicalName != profileName {
				return fmt.Errorf("github.authentication.owners[%q].profiles references unknown profile %q", owner, profileName)
			}
			if existingOwner, exists := profileOwner[profileKey]; exists {
				return fmt.Errorf("github.authentication profile %q is referenced by more than one owner (%q and %q)", profileName, existingOwner, owner)
			}
			profileOwner[profileKey] = owner
		}
	}
	for key, name := range canonicalProfiles {
		if _, exists := profileOwner[key]; !exists {
			return fmt.Errorf("github.authentication profile %q is not referenced by an owner", name)
		}
	}
	return nil
}

func validateGitHubOwnerCoverage(authentication GitHubAuthentication, repositories map[string]string) error {
	owners := make(map[string]string, len(authentication.Owners))
	for owner := range authentication.Owners {
		owners[strings.ToLower(owner)] = owner
	}
	for _, repository := range repositories {
		owner, _, _ := strings.Cut(repository, "/")
		configuredOwner, exists := owners[strings.ToLower(owner)]
		if !exists {
			return fmt.Errorf("repository owner %q has no authentication profiles", owner)
		}
		if configuredOwner != owner {
			return fmt.Errorf("github.authentication owner %q must use repository owner casing %q", configuredOwner, owner)
		}
	}
	return nil
}

func validateSecretReference(name string, reference SecretReference) error {
	hasEnvironment := strings.TrimSpace(reference.Environment) != ""
	hasFile := strings.TrimSpace(reference.File) != ""
	if hasEnvironment == hasFile {
		return fmt.Errorf("%s must configure exactly one of environment or file", name)
	}
	return nil
}

func validateAuthorization(prefix string, authorization Authorization) error {
	if len(authorization.Organizations)+len(authorization.Teams)+len(authorization.Users) == 0 {
		return fmt.Errorf("%s.authorization must contain an organization, team, or user", prefix)
	}
	seen := make(map[string]struct{})
	for _, organization := range authorization.Organizations {
		key := strings.ToLower(organization)
		if !organizationRE.MatchString(organization) {
			return fmt.Errorf("%s.authorization organization %q is invalid", prefix, organization)
		}
		if _, exists := seen["org:"+key]; exists {
			return fmt.Errorf("%s.authorization contains duplicate organization %q", prefix, organization)
		}
		seen["org:"+key] = struct{}{}
	}
	for _, team := range authorization.Teams {
		key := strings.ToLower(team)
		if !teamRE.MatchString(team) {
			return fmt.Errorf("%s.authorization team %q must use organization/team-slug", prefix, team)
		}
		if _, exists := seen["team:"+key]; exists {
			return fmt.Errorf("%s.authorization contains duplicate team %q", prefix, team)
		}
		seen["team:"+key] = struct{}{}
	}
	seenUsers := make(map[int64]struct{}, len(authorization.Users))
	for _, id := range authorization.Users {
		if id <= 0 {
			return fmt.Errorf("%s.authorization users must contain positive GitHub user IDs", prefix)
		}
		if _, exists := seenUsers[id]; exists {
			return fmt.Errorf("%s.authorization contains duplicate GitHub user ID %d", prefix, id)
		}
		seenUsers[id] = struct{}{}
	}
	return nil
}

func validateQuality(prefix string, quality *Quality) error {
	if quality == nil {
		return nil
	}
	if mismatch := quality.DescriptionDiffMismatch; mismatch != nil {
		if mismatch.MinReferences != nil && *mismatch.MinReferences < 1 {
			return fmt.Errorf("%s.quality.description_diff_mismatch.min_references must be positive", prefix)
		}
		if mismatch.StrongMismatchMinReferences != nil && *mismatch.StrongMismatchMinReferences < 1 {
			return fmt.Errorf("%s.quality.description_diff_mismatch.strong_mismatch_min_references must be positive", prefix)
		}
		if mismatch.MinReferences != nil && mismatch.StrongMismatchMinReferences != nil &&
			*mismatch.StrongMismatchMinReferences < *mismatch.MinReferences {
			return fmt.Errorf(
				"%s.quality.description_diff_mismatch.strong_mismatch_min_references must not be lower than min_references",
				prefix,
			)
		}
	}
	if broad := quality.BroadAdditionsOnly; broad != nil {
		if broad.MinFiles != nil && *broad.MinFiles < 1 {
			return fmt.Errorf("%s.quality.broad_additions_only.min_files must be positive", prefix)
		}
		if broad.MinAdditions != nil && *broad.MinAdditions < 1 {
			return fmt.Errorf("%s.quality.broad_additions_only.min_additions must be positive", prefix)
		}
		if broad.MaxDeletions != nil && *broad.MaxDeletions < 0 {
			return fmt.Errorf("%s.quality.broad_additions_only.max_deletions must not be negative", prefix)
		}
	}
	return nil
}

func validateContribution(prefix string, contribution *Contribution) error {
	if contribution == nil {
		return nil
	}
	if err := validateSchedule(prefix+".contribution.schedule", contribution.Schedule); err != nil {
		return err
	}
	if contribution.EstablishedMergedPRs != nil && *contribution.EstablishedMergedPRs < 1 {
		return fmt.Errorf("%s.contribution.established_merged_prs must be positive", prefix)
	}
	if unusual := contribution.UnusualActivity; unusual != nil {
		thresholds := []struct {
			name  string
			value *int
		}{
			{"account_age_days", unusual.AccountAgeDays},
			{"window_days", unusual.WindowDays},
			{"min_repositories", unusual.MinRepositories},
			{"min_organizations", unusual.MinOrganizations},
		}
		for _, threshold := range thresholds {
			if threshold.value != nil && *threshold.value < 1 {
				return fmt.Errorf(
					"%s.contribution.unusual_activity.%s must be positive",
					prefix, threshold.name,
				)
			}
		}
	}
	return nil
}

func validateSchedule(name, expression string) error {
	if _, err := periodic.Parse(expression); err != nil {
		return fmt.Errorf("%s %w", name, err)
	}
	return nil
}
