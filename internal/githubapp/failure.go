package githubapp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// FailureMetadata is bounded, secret-safe diagnostic data for one collection
// failure. It never includes request bodies, response bodies, or raw URLs.
type FailureMetadata struct {
	Category           FailureCategory
	Detail             string
	Remediation        string
	Phase              string
	Method             string
	Endpoint           string
	RequestID          string
	HTTPStatus         int
	TimedOut           bool
	RetryAfter         time.Duration
	RateLimitResetUnix int64
}

// BoundedFailureMetadata extracts safe operational diagnostics from an error.
func BoundedFailureMetadata(err error) FailureMetadata {
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureMetadata{
			Category: FailureOperationTimeout, Phase: "collection",
			Detail:      "The repository collection exceeded its operation deadline.",
			Remediation: "Retry the collection or increase the bounded refresh timeout.",
		}
	}
	if errors.Is(err, context.Canceled) {
		return FailureMetadata{
			Category: FailureOperationCanceled, Phase: "collection",
			Detail:      "The repository collection was canceled.",
			Remediation: "Retry the collection when the service is available.",
		}
	}
	if rateLimit, ok := errors.AsType[*RateLimitError](err); ok {
		category, detail, remediation := classifyNonAdvanceFailure(err)
		return FailureMetadata{
			Category: category, Detail: detail, Remediation: remediation,
			Phase: collectionPhase(rateLimit.Endpoint), Method: rateLimit.Method, Endpoint: rateLimit.Endpoint,
			RequestID: rateLimit.RequestID, HTTPStatus: rateLimit.StatusCode,
			RetryAfter: rateLimit.RetryAfter, RateLimitResetUnix: rateLimit.RateLimitResetUnix,
		}
	}
	if apiError, ok := errors.AsType[*APIError](err); ok {
		category, detail, remediation := classifyNonAdvanceFailure(err)
		if apiError.StatusCode == http.StatusTooManyRequests {
			category = FailureSecondaryRateLimited
			detail = "GitHub secondary rate limit prevented collection."
			remediation = "Reduce request concurrency and retry after GitHub's limit clears."
		}
		return FailureMetadata{
			Category: category, Detail: detail, Remediation: remediation,
			Phase: collectionPhase(apiError.Endpoint), Method: apiError.Method, Endpoint: apiError.Endpoint,
			RequestID: apiError.RequestID, HTTPStatus: apiError.StatusCode,
			RetryAfter: apiError.RetryAfter, RateLimitResetUnix: apiError.RateLimitResetUnix,
		}
	}
	if transportError, ok := errors.AsType[*TransportError](err); ok {
		category, detail, remediation := classifyNonAdvanceFailure(err)
		return FailureMetadata{
			Category: category, Detail: detail, Remediation: remediation,
			Phase: collectionPhase(transportError.Endpoint), Method: transportError.Method,
			Endpoint: transportError.Endpoint, TimedOut: transportError.TimedOut,
		}
	}
	if routeFailure, ok := errors.AsType[*RouteFailure](err); ok {
		return FailureMetadata{
			Category: routeFailure.Category, Phase: "routing",
			Detail:      routeFailureDetail(routeFailure.Category),
			Remediation: routeFailureRemediation(routeFailure.Category),
		}
	}
	category, detail, remediation := classifyNonAdvanceFailure(err)
	return FailureMetadata{
		Category: category, Detail: detail, Remediation: remediation, Phase: "collection",
	}
}

func collectionPhase(endpoint string) string {
	switch {
	case strings.HasPrefix(endpoint, "/app"),
		strings.HasPrefix(endpoint, "/installations"):
		return "authentication"
	case endpoint == "/search/issues":
		return "search"
	case endpoint == "/graphql":
		return "graphql"
	case strings.HasPrefix(endpoint, "/users/"):
		return "contributor_identity"
	case strings.Contains(endpoint, "/issues/") && strings.HasSuffix(endpoint, "/timeline"):
		return "progress"
	case strings.Contains(endpoint, "/pulls/") && strings.HasSuffix(endpoint, "/files"):
		return "pull_request_files"
	case strings.Contains(endpoint, "/pulls/") &&
		(strings.HasSuffix(endpoint, "/comments") || strings.HasSuffix(endpoint, "/reviews")):
		return "reviews"
	case strings.Contains(endpoint, "/pulls/") && strings.HasSuffix(endpoint, "/commits"):
		return "commits"
	case strings.HasSuffix(endpoint, "/pulls") || strings.Contains(endpoint, "/pulls/"):
		return "pull_requests"
	case strings.Contains(endpoint, "/contents/") || strings.Contains(endpoint, "/git/"):
		return "repository_content"
	case strings.HasPrefix(endpoint, "/repos/"):
		return "repository"
	default:
		return "github_request"
	}
}
