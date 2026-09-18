package telemetry

import (
	"context"
	"slices"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var githubRouteAttempts metric.Int64Counter

func registerGitHubRouteInstruments() error {
	var err error
	githubRouteAttempts, err = otel.Meter(
		InstrumentationName,
		metric.WithInstrumentationVersion(InstrumentationVersion),
	).Int64Counter(GitHubRouteAttemptsMetricName)
	return err
}

// RecordGitHubRouteAttempt records one successfully persisted routed attempt.
func RecordGitHubRouteAttempt(
	ctx context.Context, operation, profileType, outcome, failureCategory string,
) {
	if !instrumentsReady.Load() {
		return
	}
	if failureCategory == "" {
		failureCategory = "none"
	}
	githubRouteAttempts.Add(ctx, 1, metric.WithAttributes(
		attribute.String(
			"maintainer_cockpit.github.route.operation",
			boundedValue(
				operation, "discovery", "hydration", "progress",
				"contribution_evidence", "pull_request_diff",
			),
		),
		attribute.String(
			"maintainer_cockpit.github.profile.type",
			boundedValue(profileType, "github_app", "fine_grained_pat"),
		),
		attribute.String(
			"maintainer_cockpit.github.route.outcome",
			boundedValue(outcome, "selected", "advanced", "failed"),
		),
		attribute.String(
			"maintainer_cockpit.github.failure.category",
			boundedValue(
				failureCategory, "none", "route_missing", "route_ambiguous",
				"installation_missing", "installation_suspended", "repository_not_granted",
				"permission_missing", "credential_invalid", "public_capability_missing",
				"primary_rate_limited", "secondary_rate_limited", "github_unavailable",
			),
		),
	))
}

func boundedValue(value string, allowed ...string) string {
	if slices.Contains(allowed, value) {
		return value
	}
	return "unknown"
}
