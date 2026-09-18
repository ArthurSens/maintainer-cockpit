package githubapp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Operation is a stable, bounded collection operation identifier.
type Operation string

const (
	OperationDiscovery            Operation = "discovery"
	OperationHydration            Operation = "hydration"
	OperationProgress             Operation = "progress"
	OperationContributionEvidence Operation = "contribution_evidence"
	OperationPullRequestDiff      Operation = "pull_request_diff"
)

// ProfileType identifies the credential boundary used by a concrete client.
type ProfileType string

const (
	ProfileGitHubApp      ProfileType = "github_app"
	ProfileFineGrainedPAT ProfileType = "fine_grained_pat"
)

// RouteOutcome describes what routing did after an attempt.
type RouteOutcome string

const (
	RouteOutcomeSelected RouteOutcome = "selected"
	RouteOutcomeAdvanced RouteOutcome = "advanced"
	RouteOutcomeFailed   RouteOutcome = "failed"
)

// FailureCategory is a stable, bounded route failure classification.
type FailureCategory string

const (
	FailureRouteMissing            FailureCategory = "route_missing"
	FailureRouteAmbiguous          FailureCategory = "route_ambiguous"
	FailureInstallationMissing     FailureCategory = "installation_missing"
	FailureInstallationSuspended   FailureCategory = "installation_suspended"
	FailureRepositoryNotGranted    FailureCategory = "repository_not_granted"
	FailurePermissionMissing       FailureCategory = "permission_missing"
	FailureCredentialInvalid       FailureCategory = "credential_invalid"
	FailurePublicCapabilityMissing FailureCategory = "public_capability_missing"
	FailurePrimaryRateLimited      FailureCategory = "primary_rate_limited"
	FailureSecondaryRateLimited    FailureCategory = "secondary_rate_limited"
	FailureGitHubUnavailable       FailureCategory = "github_unavailable"
	FailureOperationTimeout        FailureCategory = "operation_timeout"
	FailureOperationCanceled       FailureCategory = "operation_canceled"
	FailureStorage                 FailureCategory = "storage"
	FailureUnknown                 FailureCategory = "unknown"
)

// Attempt is persistence-ready, bounded metadata for one concrete route.
type Attempt struct {
	Operation   Operation
	Profile     string
	Priority    int
	ProfileType ProfileType
	Outcome     RouteOutcome
	Failure     FailureCategory
	Detail      string
	Remediation string
	Selected    bool
}

// AttemptSet is the ordered set of routes attempted by one operation.
type AttemptSet []Attempt

// RoutedDiscoveryResult combines discovery with ordered route attempts.
type RoutedDiscoveryResult struct {
	Discovery DiscoveryResult
	Attempts  AttemptSet
}

// RoutedHydrationResult combines a hydrated batch with ordered route attempts.
type RoutedHydrationResult struct {
	Hydration HydrationResult
	Attempts  AttemptSet
}

// RoutedProgressResult combines progress evidence with ordered route attempts.
type RoutedProgressResult struct {
	Progress ProgressResult
	Attempts AttemptSet
}

// ContributionResult combines repository evidence with ordered route attempts.
type ContributionResult struct {
	Evidence RepositoryContributionEvidence
	Attempts AttemptSet
}

// DiffResult combines bounded patch evidence with ordered route attempts.
type DiffResult struct {
	Evidence PullRequestDiffEvidence
	Attempts AttemptSet
}

// RouteFailure is the only error type that permits advancing to another
// configured profile.
type RouteFailure struct {
	Category FailureCategory
	err      error
	refresh  bool
}

func (e *RouteFailure) Error() string {
	if e.err == nil {
		return "GitHub route failed: " + string(e.Category)
	}
	return e.err.Error()
}

func (e *RouteFailure) Unwrap() error { return e.err }

// NewRouteFailure creates a typed bounded failure for route resolvers and
// capability probes.
func NewRouteFailure(category FailureCategory, err error) error {
	return &RouteFailure{Category: category, err: err}
}

func newRefreshableRouteFailure(category FailureCategory, err error) error {
	return &RouteFailure{Category: category, err: err, refresh: true}
}

// Profile binds a non-secret name and type to one isolated concrete client.
type Profile struct {
	Name   string
	Type   ProfileType
	Client *Client
}

// Router dispatches canonical owners through ordered concrete profiles.
type Router struct {
	routes map[string][]Profile
}

// SetupRouteResult is one independently reported profile/repository check.
type SetupRouteResult struct {
	Owner      string
	Repository string
	Profile    string
	Type       ProfileType
	Setup      Setup
	Valid      bool
	Err        error
}

// NewRouter validates and copies canonical owner routes.
func NewRouter(routes map[string][]Profile) (*Router, error) {
	copied := make(map[string][]Profile, len(routes))
	for owner, profiles := range routes {
		canonical := strings.ToLower(strings.TrimSpace(owner))
		if canonical == "" || strings.Contains(canonical, "/") {
			return nil, fmt.Errorf("GitHub route owner %q is invalid", owner)
		}
		if _, exists := copied[canonical]; exists {
			return nil, fmt.Errorf("duplicate case-insensitive GitHub route owner %q", owner)
		}
		if len(profiles) == 0 {
			return nil, fmt.Errorf("GitHub route owner %q has no profiles", owner)
		}
		names := make(map[string]struct{}, len(profiles))
		copiedProfiles := append([]Profile(nil), profiles...)
		for _, profile := range copiedProfiles {
			if profile.Name == "" || profile.Client == nil {
				return nil, fmt.Errorf("GitHub route owner %q has an invalid profile", owner)
			}
			if profile.Type != ProfileGitHubApp && profile.Type != ProfileFineGrainedPAT {
				return nil, fmt.Errorf("GitHub profile %q has unknown type %q", profile.Name, profile.Type)
			}
			if profile.Client.profileType != profile.Type {
				return nil, fmt.Errorf("GitHub profile %q type does not match its client", profile.Name)
			}
			if _, exists := names[profile.Name]; exists {
				return nil, fmt.Errorf("GitHub route owner %q repeats profile %q", owner, profile.Name)
			}
			names[profile.Name] = struct{}{}
		}
		copied[canonical] = copiedProfiles
	}
	return &Router{routes: copied}, nil
}

// Profiles returns all isolated concrete clients in deterministic route order.
func (r *Router) Profiles() []Profile {
	owners := make([]string, 0, len(r.routes))
	for owner := range r.routes {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	var profiles []Profile
	seen := make(map[string]struct{})
	for _, owner := range owners {
		for _, profile := range r.routes[owner] {
			if _, exists := seen[profile.Name]; exists {
				continue
			}
			seen[profile.Name] = struct{}{}
			profiles = append(profiles, profile)
		}
	}
	return profiles
}

// SetupCheck validates every configured route independently and returns all
// results so callers can apply command-specific severity.
func (r *Router) SetupCheck(
	ctx context.Context, repositories ...string,
) []SetupRouteResult {
	var results []SetupRouteResult
	for _, repository := range repositories {
		owner, _, valid := strings.Cut(repository, "/")
		profiles := r.routes[strings.ToLower(owner)]
		if !valid || owner == "" || len(profiles) == 0 {
			results = append(results, SetupRouteResult{
				Owner: owner, Repository: repository,
				Err: NewRouteFailure(
					FailureRouteMissing, fmt.Errorf("no GitHub route for %q", repository),
				),
			})
			continue
		}
		for _, profile := range profiles {
			result := SetupRouteResult{
				Owner: owner, Repository: repository, Profile: profile.Name, Type: profile.Type,
			}
			switch profile.Type {
			case ProfileGitHubApp:
				result.Setup, result.Err = profile.Client.SetupCheckOwner(ctx, owner, repository)
			case ProfileFineGrainedPAT:
				var setup SetupProbe
				setup, result.Err = profile.Client.SetupCheckPAT(ctx, repository)
				result.Setup.AccountLogin = setup.AccountLogin
			}
			result.Valid = result.Err == nil
			results = append(results, result)
		}
	}
	return results
}

// Discover routes discovery from profile zero and omits routing metadata.
func (r *Router) Discover(
	ctx context.Context, repository string,
) (DiscoveryResult, error) {
	result, err := r.DiscoverResult(ctx, repository)
	return result.Discovery, err
}

// DiscoverResult routes discovery and returns ordered routing metadata.
func (r *Router) DiscoverResult(
	ctx context.Context, repository string,
) (RoutedDiscoveryResult, error) {
	discovery, attempts, err := routeRepositoryPhase(
		ctx, r, repository, OperationDiscovery,
		func(client *Client) (DiscoveryResult, error) {
			return client.Discover(ctx, repository)
		},
	)
	return RoutedDiscoveryResult{Discovery: discovery, Attempts: attempts}, err
}

// HydratePullRequests routes one supplied bounded hydration batch from profile
// zero and omits routing metadata.
func (r *Router) HydratePullRequests(
	ctx context.Context,
	repository string,
	pullRequests []DiscoveredPullRequest,
	options HydrationOptions,
) (HydrationResult, error) {
	result, err := r.HydratePullRequestsResult(ctx, repository, pullRequests, options)
	return result.Hydration, err
}

// HydratePullRequestsResult routes one supplied bounded hydration batch and
// returns ordered routing metadata.
func (r *Router) HydratePullRequestsResult(
	ctx context.Context,
	repository string,
	pullRequests []DiscoveredPullRequest,
	options HydrationOptions,
) (RoutedHydrationResult, error) {
	hydration, attempts, err := routeRepositoryPhase(
		ctx, r, repository, OperationHydration,
		func(client *Client) (HydrationResult, error) {
			return client.HydratePullRequests(ctx, repository, pullRequests, options)
		},
	)
	return RoutedHydrationResult{Hydration: hydration, Attempts: attempts}, err
}

// CollectProgress routes incremental progress collection from profile zero and
// omits routing metadata.
func (r *Router) CollectProgress(
	ctx context.Context, repository string, options ProgressOptions,
) (ProgressResult, error) {
	result, err := r.CollectProgressResult(ctx, repository, options)
	return result.Progress, err
}

// CollectProgressResult routes incremental progress collection and returns
// ordered routing metadata.
func (r *Router) CollectProgressResult(
	ctx context.Context, repository string, options ProgressOptions,
) (RoutedProgressResult, error) {
	progress, attempts, err := routeRepositoryPhase(
		ctx, r, repository, OperationProgress,
		func(client *Client) (ProgressResult, error) {
			return client.CollectProgress(ctx, repository, options)
		},
	)
	return RoutedProgressResult{Progress: progress, Attempts: attempts}, err
}

// CollectRepositoryContribution routes one author/repository fact collection
// through that repository owner's ordered profile chain.
func (r *Router) CollectRepositoryContribution(
	ctx context.Context, authorID int64, login, repository, association string,
) (ContributionResult, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ContributionResult{}, errors.New("repository must use owner/name")
	}
	profiles := r.routes[strings.ToLower(parts[0])]
	if len(profiles) == 0 {
		return ContributionResult{}, NewRouteFailure(
			FailureRouteMissing, fmt.Errorf("no GitHub route for owner %q", parts[0]),
		)
	}
	attempts := make(AttemptSet, 0, len(profiles))
	for index, profile := range profiles {
		evidence, err := profile.Client.CollectRepositoryContribution(
			ctx, authorID, login, repository, association,
		)
		if err == nil {
			attempts = append(attempts, Attempt{
				Operation: OperationContributionEvidence, Profile: profile.Name,
				Priority: index, ProfileType: profile.Type, Outcome: RouteOutcomeSelected,
				Selected: true,
			})
			return ContributionResult{Evidence: evidence, Attempts: attempts}, nil
		}
		if ctxErr := contextError(ctx, err); ctxErr != nil {
			return ContributionResult{Attempts: attempts}, ctxErr
		}
		var routeFailure *RouteFailure
		if !errors.As(err, &routeFailure) || !routeFailure.canAdvance() {
			category, detail, remediation := classifyNonAdvanceFailure(err)
			attempts = append(attempts, Attempt{
				Operation: OperationContributionEvidence, Profile: profile.Name,
				Priority: index, ProfileType: profile.Type, Outcome: RouteOutcomeFailed,
				Failure: category, Detail: detail, Remediation: remediation,
			})
			return ContributionResult{Attempts: attempts}, err
		}
		outcome := RouteOutcomeFailed
		if index+1 < len(profiles) {
			outcome = RouteOutcomeAdvanced
		}
		attempts = append(attempts, Attempt{
			Operation: OperationContributionEvidence, Profile: profile.Name,
			Priority: index, ProfileType: profile.Type, Outcome: outcome,
			Failure:     routeFailure.Category,
			Detail:      routeFailureDetail(routeFailure.Category),
			Remediation: routeFailureRemediation(routeFailure.Category),
		})
		if index+1 == len(profiles) {
			return ContributionResult{Attempts: attempts}, err
		}
	}
	panic("unreachable")
}

// CollectPullRequestDiff routes current-head patch collection through the
// repository owner's ordered authenticated profile chain.
func (r *Router) CollectPullRequestDiff(
	ctx context.Context, repository string, number int, headSHA string, changedFiles int,
) (DiffResult, error) {
	owner, _, valid := strings.Cut(repository, "/")
	if !valid || owner == "" {
		return DiffResult{}, errors.New("repository must use owner/name")
	}
	profiles := r.routes[strings.ToLower(owner)]
	if len(profiles) == 0 {
		return DiffResult{}, NewRouteFailure(
			FailureRouteMissing, fmt.Errorf("no GitHub route for owner %q", owner),
		)
	}
	attempts := make(AttemptSet, 0, len(profiles))
	for index, profile := range profiles {
		evidence, err := profile.Client.CollectPullRequestDiff(
			ctx, repository, number, headSHA, changedFiles,
		)
		if err == nil {
			attempts = append(attempts, Attempt{
				Operation: OperationPullRequestDiff, Profile: profile.Name,
				Priority: index, ProfileType: profile.Type,
				Outcome: RouteOutcomeSelected, Selected: true,
			})
			return DiffResult{Evidence: evidence, Attempts: attempts}, nil
		}
		if ctxErr := contextError(ctx, err); ctxErr != nil {
			return DiffResult{Attempts: attempts}, ctxErr
		}
		var routeFailure *RouteFailure
		if !errors.As(err, &routeFailure) || !routeFailure.canAdvance() {
			category, detail, remediation := classifyNonAdvanceFailure(err)
			attempts = append(attempts, Attempt{
				Operation: OperationPullRequestDiff, Profile: profile.Name,
				Priority: index, ProfileType: profile.Type, Outcome: RouteOutcomeFailed,
				Failure: category, Detail: detail, Remediation: remediation,
			})
			return DiffResult{Attempts: attempts}, err
		}
		outcome := RouteOutcomeFailed
		if index+1 < len(profiles) {
			outcome = RouteOutcomeAdvanced
		}
		attempts = append(attempts, Attempt{
			Operation: OperationPullRequestDiff, Profile: profile.Name,
			Priority: index, ProfileType: profile.Type, Outcome: outcome,
			Failure: routeFailure.Category, Detail: routeFailureDetail(routeFailure.Category),
			Remediation: routeFailureRemediation(routeFailure.Category),
		})
		if index+1 == len(profiles) {
			return DiffResult{Attempts: attempts}, err
		}
	}
	panic("unreachable")
}

func routeRepositoryPhase[T any](
	ctx context.Context,
	router *Router,
	repository string,
	operation Operation,
	collect func(*Client) (T, error),
) (T, AttemptSet, error) {
	var zero T
	owner, _, err := splitRepository(repository)
	if err != nil {
		return zero, nil, err
	}
	profiles := router.routes[strings.ToLower(owner)]
	if len(profiles) == 0 {
		return zero, nil, NewRouteFailure(
			FailureRouteMissing, fmt.Errorf("no GitHub route for owner %q", owner),
		)
	}
	attempts := make(AttemptSet, 0, len(profiles))
	for index, profile := range profiles {
		result, err := collect(profile.Client)
		if err == nil {
			attempts = append(attempts, Attempt{
				Operation: operation, Profile: profile.Name, Priority: index,
				ProfileType: profile.Type, Outcome: RouteOutcomeSelected, Selected: true,
			})
			return result, attempts, nil
		}
		if ctxErr := contextError(ctx, err); ctxErr != nil {
			return zero, attempts, ctxErr
		}
		var routeFailure *RouteFailure
		if !errors.As(err, &routeFailure) || !routeFailure.canAdvance() {
			category, detail, remediation := classifyNonAdvanceFailure(err)
			attempts = append(attempts, Attempt{
				Operation: operation, Profile: profile.Name, Priority: index,
				ProfileType: profile.Type, Outcome: RouteOutcomeFailed,
				Failure: category, Detail: detail, Remediation: remediation,
			})
			return zero, attempts, err
		}
		outcome := RouteOutcomeFailed
		if index+1 < len(profiles) {
			outcome = RouteOutcomeAdvanced
		}
		attempts = append(attempts, Attempt{
			Operation: operation, Profile: profile.Name, Priority: index,
			ProfileType: profile.Type, Outcome: outcome, Failure: routeFailure.Category,
			Detail:      routeFailureDetail(routeFailure.Category),
			Remediation: routeFailureRemediation(routeFailure.Category),
		})
		if index+1 == len(profiles) {
			return zero, attempts, err
		}
	}
	panic("unreachable")
}

func (e *RouteFailure) canAdvance() bool {
	switch e.Category {
	case FailureRouteMissing, FailureRouteAmbiguous, FailureInstallationMissing,
		FailureInstallationSuspended, FailureRepositoryNotGranted,
		FailurePermissionMissing, FailureCredentialInvalid,
		FailurePublicCapabilityMissing:
		return true
	default:
		return false
	}
}

func contextError(ctx context.Context, _ error) error {
	return ctx.Err()
}

func classifyNonAdvanceFailure(err error) (FailureCategory, string, string) {
	if rateLimit, ok := errors.AsType[*RateLimitError](err); ok {
		if rateLimit.Reason == "primary" {
			return FailurePrimaryRateLimited,
				"GitHub primary rate limit prevented collection.",
				"Wait for the rate-limit reset or adjust collection scheduling."
		}
		if rateLimit.Reason == "secondary" {
			return FailureSecondaryRateLimited,
				"GitHub secondary rate limit prevented collection.",
				"Reduce request concurrency and retry after GitHub's limit clears."
		}
	}
	var apiError *APIError
	if errors.As(err, &apiError) && apiError.StatusCode >= 500 && apiError.StatusCode <= 599 {
		return FailureGitHubUnavailable,
			"GitHub was unavailable during collection.",
			"Retry after GitHub service availability recovers."
	}
	if _, ok := errors.AsType[*TransportError](err); ok {
		return FailureGitHubUnavailable,
			"GitHub could not be reached during collection.",
			"Check outbound connectivity and retry the collection."
	}
	return FailureUnknown,
		"GitHub collection failed for an unclassified reason.",
		"Review bounded server logs and retry the collection."
}

func routeFailureDetail(category FailureCategory) string {
	switch category {
	case FailureRouteMissing:
		return "No configured route matched the repository owner."
	case FailureRouteAmbiguous:
		return "More than one route matched the repository owner."
	case FailureInstallationMissing:
		return "The configured installation could not be found."
	case FailureInstallationSuspended:
		return "The configured installation is suspended."
	case FailureRepositoryNotGranted:
		return "The profile is not granted access to the repository."
	case FailurePermissionMissing:
		return "The profile lacks a required GitHub permission."
	case FailureCredentialInvalid:
		return "The configured GitHub credential is invalid."
	case FailurePublicCapabilityMissing:
		return "The public route cannot provide required repository data."
	default:
		return "GitHub collection failed for an unclassified reason."
	}
}

func routeFailureRemediation(category FailureCategory) string {
	switch category {
	case FailureRouteMissing, FailureRouteAmbiguous:
		return "Correct the repository owner routing configuration."
	case FailureInstallationMissing, FailureInstallationSuspended:
		return "Install or reactivate the GitHub App for this owner."
	case FailureRepositoryNotGranted:
		return "Grant the configured installation access to this repository."
	case FailurePermissionMissing:
		return "Grant the required GitHub App or token permissions."
	case FailureCredentialInvalid:
		return "Replace or correct the configured credential."
	case FailurePublicCapabilityMissing:
		return "Configure an authenticated profile with the required access."
	default:
		return "Review bounded server logs and retry the collection."
	}
}
