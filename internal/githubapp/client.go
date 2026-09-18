// Package githubapp provides the read-only GitHub App integration.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ArthurSens/maintainer-cockpit/internal/contribution"
)

const (
	apiVersion              = "2026-03-10"
	InputFingerprintVersion = "github-input-v1"
)

// Options configures a GitHub App client.
type Options struct {
	AppID                        int64
	InstallationID               int64
	PrivateKeyPEM                []byte
	BaseURL                      string
	HTTPClient                   *http.Client
	Now                          func() time.Time
	DiscoveryPlans               map[string][]DiscoveryTarget
	CollectContext               bool
	CollectContribution          bool
	CollectProgress              bool
	RequireMembersRead           bool
	ContributionWindowDays       int
	ContributionRefreshIntervals map[string]time.Duration
	ContributionRepositories     map[string][]string
	RateLimitBudget              time.Duration
	RateLimitMaxRetries          int
	Sleep                        func(context.Context, time.Duration) error
}

// PATOptions configures a collector with one static fine-grained PAT.
type PATOptions struct {
	Token                        string
	BaseURL                      string
	HTTPClient                   *http.Client
	Now                          func() time.Time
	DiscoveryPlans               map[string][]DiscoveryTarget
	CollectContext               bool
	CollectContribution          bool
	CollectProgress              bool
	RequireMembersRead           bool
	ContributionWindowDays       int
	ContributionRefreshIntervals map[string]time.Duration
	ContributionRepositories     map[string][]string
	RateLimitBudget              time.Duration
	RateLimitMaxRetries          int
	Sleep                        func(context.Context, time.Duration) error
}

// DiscoveryTarget is one collection's policy for a shared repository.
type DiscoveryTarget struct {
	CollectionID string
	Mode         string
	Query        string
}

// Client authenticates as one GitHub App installation.
type Client struct {
	appID                  int64
	installationID         int64
	privateKey             *rsa.PrivateKey
	profileType            ProfileType
	staticToken            string
	baseURL                *url.URL
	httpClient             *http.Client
	now                    func() time.Time
	discoveryPlans         map[string][]DiscoveryTarget
	collectContext         bool
	collectProgress        bool
	requireMembersRead     bool
	contributionWindowDays int
	rateLimitObserver      func(remaining, limit int, resetAt, observedAt time.Time)
	retryObserver          func(RetryEvent)
	rateLimitBudget        time.Duration
	rateLimitMaxRetries    int
	sleep                  func(context.Context, time.Duration) error

	tokenMu sync.Mutex
	tokens  map[string]cachedToken
}

// SetRateLimitObserver installs a non-blocking observer for response limit
// headers. It is intended for deployment operations status.
func (c *Client) SetRateLimitObserver(
	observer func(remaining, limit int, resetAt, observedAt time.Time),
) {
	c.rateLimitObserver = observer
}

// SetRetryObserver installs a non-blocking observer for bounded rate-limit
// waits and terminal failures.
func (c *Client) SetRetryObserver(observer func(RetryEvent)) {
	c.retryObserver = observer
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

// Setup describes the validated GitHub App installation.
type Setup struct {
	AppSlug      string
	AccountLogin string
}

// SetupProbe describes a credential capability check without collecting data.
type SetupProbe struct {
	AccountLogin string
}

// Snapshot is one complete all-open repository collection.
type Snapshot struct {
	Repository         string
	PullRequests       []PullRequest
	Authors            []AuthorHistory
	ProgressEvents     []ProgressEvent
	ReviewThreads      []ReviewThreadState
	CollectedAt        time.Time
	Limits             []ContextLimit
	Fingerprints       map[int]InputFingerprint
	CacheHits          int
	CacheMisses        int
	CacheBypasses      int
	FullReconciliation bool
}

// InputFingerprint is the stable, explicitly versioned identity of every
// GitHub input used to populate one pull request.
type InputFingerprint struct {
	Version string `json:"version"`
	Value   string `json:"value"`
}

// CachedPullRequest is a previously complete pull request eligible for a cache hit.
type CachedPullRequest struct {
	PullRequest PullRequest
	Fingerprint InputFingerprint
}

// CollectOptions controls persisted pull-request caching and incremental progress collection.
type CollectOptions struct {
	Cached        map[int]CachedPullRequest
	ForceFull     bool
	ProgressSince time.Time
}

// DiscoveryResult is the bounded open pull-request set for one repository.
type DiscoveryResult struct {
	Repository   string
	PullRequests []DiscoveredPullRequest
	Limits       []ContextLimit
}

// DiscoveredPullRequest carries current collection membership and discovery
// limits into a later hydration phase.
type DiscoveredPullRequest struct {
	Number        int
	CollectionIDs []string
	ContextLimits []ContextLimit
}

// MaxHydrationBatchSize is the largest pull-request list accepted by one
// hydration call.
const MaxHydrationBatchSize = 100

// HydrationOptions controls persisted pull-request reuse for one bounded batch.
type HydrationOptions struct {
	Cached    map[int]CachedPullRequest
	ForceFull bool
}

// HydrationResult is one complete bounded pull-request batch.
type HydrationResult struct {
	Repository    string
	PullRequests  []PullRequest
	CollectedAt   time.Time
	Fingerprints  map[int]InputFingerprint
	CacheHits     int
	CacheMisses   int
	CacheBypasses int
	ForceFull     bool
}

// ProgressOptions controls incremental repository progress collection.
type ProgressOptions struct {
	ProgressSince time.Time
}

// ProgressResult is independently collected repository progress evidence.
type ProgressResult struct {
	Repository     string
	ProgressEvents []ProgressEvent
	ReviewThreads  []ReviewThreadState
	CollectedAt    time.Time
}

// RetryEvent exposes low-cardinality rate-limit retry state to operations.
type RetryEvent struct {
	State      string
	Reason     string
	Wait       time.Duration
	Retry      int
	ObservedAt time.Time
}

// RateLimitError reports a bounded terminal primary or secondary limit.
type RateLimitError struct {
	Reason             string
	Retries            int
	BudgetExceeded     bool
	Method             string
	Endpoint           string
	RequestID          string
	RetryAfter         time.Duration
	RateLimitResetUnix int64
	StatusCode         int
}

// APIError is bounded HTTP failure metadata. Provider response bodies and
// request paths are intentionally excluded.
type APIError struct {
	StatusCode         int
	Method             string
	Endpoint           string
	RequestID          string
	RetryAfter         time.Duration
	RateLimitResetUnix int64
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub API returned status %d", e.StatusCode)
}

// TransportError identifies a provider transport failure without retaining
// arbitrary network errors, local paths, or credentials.
type TransportError struct {
	Method   string
	Endpoint string
	TimedOut bool
}

func (*TransportError) Error() string { return "GitHub transport unavailable" }

func (e *RateLimitError) Error() string {
	if e.BudgetExceeded {
		return "GitHub " + e.Reason + " rate-limit wait exceeds the job budget"
	}
	return "GitHub " + e.Reason + " rate limit retries exhausted"
}

type requestFailureMetadata struct {
	method             string
	endpoint           string
	requestID          string
	retryAfter         time.Duration
	rateLimitResetUnix int64
	statusCode         int
}

func boundedRequestMetadata(method, requestPath string, header http.Header, now time.Time) requestFailureMetadata {
	metadata := requestFailureMetadata{
		method: method, endpoint: sanitizeEndpoint(requestPath),
		requestID: boundedHeaderToken(header.Get("X-GitHub-Request-Id")),
	}
	if wait, ok := parseRetryAfter(header.Get("Retry-After"), now); ok {
		metadata.retryAfter = min(wait, 24*time.Hour)
	}
	if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil &&
		reset >= 0 {
		metadata.rateLimitResetUnix = reset
	}
	return metadata
}

func boundedHeaderToken(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		value = value[:128]
	}
	for _, character := range value {
		if character != '-' && character != '_' &&
			(character < '0' || character > '9') &&
			(character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') {
			return ""
		}
	}
	return value
}

func sanitizeEndpoint(requestPath string) string {
	parsed, err := url.Parse(requestPath)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "/"
	}
	switch parts[0] {
	case "repos":
		if len(parts) < 3 {
			return "/repos"
		}
		result := []string{"repos", "{owner}", "{repo}"}
		for index := 3; index < len(parts); index++ {
			part := parts[index]
			switch {
			case index == 4 && (parts[3] == "pulls" || parts[3] == "issues"):
				part = "{id}"
			case index == 4 && parts[3] == "commits":
				part = "{id}"
			case index == 5 && parts[3] == "git":
				part = "{id}"
			case parts[3] == "contents" && index >= 4:
				part = "{path}"
				result = append(result, part)
				return "/" + strings.Join(result, "/")
			case isNumericPathSegment(part):
				part = "{id}"
			}
			result = append(result, part)
		}
		return "/" + strings.Join(result, "/")
	case "users":
		if len(parts) == 1 {
			return "/users"
		}
		parts[1] = "{login}"
	case "orgs":
		if len(parts) == 1 {
			return "/orgs"
		}
		parts[1] = "{owner}"
	case "app":
		if len(parts) >= 3 && parts[1] == "installations" {
			parts[2] = "{id}"
		}
	case "search":
		if len(parts) >= 2 && parts[1] == "issues" {
			return "/search/issues"
		}
		return "/search"
	case "installations", "repositories":
		if len(parts) >= 2 {
			parts[1] = "{id}"
		}
	default:
		switch parts[0] {
		case "graphql", "user", "rate_limit":
			return "/" + parts[0]
		default:
			// Unknown endpoint paths could themselves contain secret or
			// contributor-controlled data.
			return "/{endpoint}"
		}
	}
	return "/" + strings.Join(parts, "/")
}

func isNumericPathSegment(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// ProgressEvent is one attributed qualifying activity eligible for private goals.
type ProgressEvent struct {
	ID, ActivityType, Repository, Title, PullRequestURL, ActivityURL string
	ActorID                                                          int64
	Number                                                           int
	OccurredAt                                                       time.Time
	CollectionIDs                                                    []string
}

// ReviewThreadState records enough evidence to detect a resolved transition.
type ReviewThreadState struct {
	ID, Repository, Title, PullRequestURL, ActivityURL string
	Resolved                                           bool
	ResolvedBy                                         int64
	Number                                             int
	CollectionIDs                                      []string
}

// AuthorHistory is repository history plus bounded recent public activity.
type AuthorHistory struct {
	ID               int64
	Login            string
	Complete         bool
	AccountCreatedAt time.Time
	Repositories     map[string]contribution.RepositoryHistory
	RecentActivity   []contribution.RepositoryActivity
	CollectedAt      time.Time
}

// RepositoryContributionEvidence is one independently refreshable author and
// repository fact set.
type RepositoryContributionEvidence struct {
	AuthorID    int64
	Login       string
	Repository  string
	History     contribution.RepositoryHistory
	Complete    bool
	CollectedAt time.Time
}

// AncillaryAuthorEvidence contains bounded facts available only through
// unauthenticated public endpoints.
type AncillaryAuthorEvidence struct {
	AuthorID         int64
	Login            string
	AccountCreatedAt time.Time
	RecentActivity   []contribution.RepositoryActivity
	Complete         bool
	CollectedAt      time.Time
}

// AncillaryClient is an authentication-free client path. It has no router or
// configured-profile fallback capability.
type AncillaryClient struct {
	client *Client
}

// NewAncillaryClient creates an authentication-free author evidence client
// that shares only transport, rate-limit, and clock configuration.
func NewAncillaryClient(client *Client) *AncillaryClient {
	return &AncillaryClient{client: client}
}

// PullRequest contains the facts collected in issue #2.
type PullRequest struct {
	Repository        string
	Number            int
	HeadSHA           string
	Title             string
	Body              string
	Author            string
	AuthorID          int64
	AuthorAssociation string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	URL               string
	Additions         int
	Deletions         int
	ChangedFiles      int
	Files             []PullRequestFile
	ReviewState       string
	Readiness         PullRequestReadiness
	CollectionIDs     []string
	Context           PullRequestContext
}

// PullRequestReadiness contains GitHub facts used independently in the review checklist.
type PullRequestReadiness struct {
	RequestedReviewers []string
	RequestedTeams     []string
	Mergeable          *bool
	MergeableState     string
	Checks             CheckSummary
	CollectedAt        time.Time
}

// CheckSummary is the complete set of GitHub Check Runs for the current head.
type CheckSummary struct {
	Total int
	Runs  []CheckRun
}

// CheckRun is one GitHub Check Run relevant to pull-request readiness.
type CheckRun struct {
	ID         int64
	Name       string
	Status     string
	Conclusion string
	URL        string
}

// PullRequestFile is per-file provider churn.
type PullRequestFile struct {
	Path      string
	Additions int
	Deletions int
}

// PullRequestDiffSource is one bounded per-file patch hunk.
type PullRequestDiffSource struct {
	Path          string
	Patch         string
	OriginalBytes int
	SentBytes     int
	Truncated     bool
}

// PullRequestDiffEvidence is bounded current-head patch evidence.
type PullRequestDiffEvidence struct {
	Completeness  string
	OriginalFiles int
	SentFiles     int
	OmittedFiles  int
	// OriginalBytes counts patch bytes returned by GitHub for inspected files.
	// GitHub does not expose byte totals for omitted files or omitted patches.
	OriginalBytes int
	SentBytes     int
	Truncated     bool
	Sources       []PullRequestDiffSource
}

// PullRequestHeadChangedError reports that a diff collection crossed a
// force-push boundary and therefore cannot be associated with the requested head.
type PullRequestHeadChangedError struct {
	Expected string
	Observed string
}

func (*PullRequestHeadChangedError) Error() string {
	return "pull request head changed during diff collection"
}

// IsPullRequestHeadChanged reports superseded diff work.
func IsPullRequestHeadChanged(err error) bool {
	var target *PullRequestHeadChangedError
	return errors.As(err, &target)
}

// PullRequestContext is bounded one-hop context collected with a PR.
type PullRequestContext struct {
	Completeness  string
	Limits        []ContextLimit
	Relationships []Relationship
	ExternalURLs  []ExternalURL
	CollectedAt   time.Time
}

// ContextLimit records explicit provider truncation.
type ContextLimit struct {
	Scope     string
	Reason    string
	Collected int
	Total     int
}

// Relationship is one GitHub-native evidence edge.
type Relationship struct {
	Kind, SourceID, SourceRepository, Title, Text, State, URL string
	SourceNumber                                              int
	Timestamp                                                 time.Time
}

// ExternalURL is untrusted metadata extracted without fetching it.
type ExternalURL struct {
	URL, SourceKind, SourceID string
	DiscoveredAt              time.Time
}

type incompleteError struct {
	err error
}

type review struct {
	ID   int64 `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	State       string    `json:"state"`
	Body        string    `json:"body"`
	SubmittedAt time.Time `json:"submitted_at"`
	HTMLURL     string    `json:"html_url"`
}

type reviewComment struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	Path        string    `json:"path"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	HTMLURL     string    `json:"html_url"`
	InReplyToID int64     `json:"in_reply_to_id"`
}

type commit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Date time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}

func (e *incompleteError) Error() string  { return "incomplete GitHub refresh: " + e.err.Error() }
func (e *incompleteError) Unwrap() error  { return e.err }
func (*incompleteError) Incomplete() bool { return true }

// IsIncomplete reports whether collection started but could not produce a
// complete repository snapshot.
func IsIncomplete(err error) bool {
	var target *incompleteError
	return errors.As(err, &target)
}

// New creates a client and validates its local cryptographic configuration.
func New(options Options) (*Client, error) {
	if options.AppID <= 0 || options.InstallationID <= 0 {
		return nil, errors.New("GitHub App and installation IDs must be positive")
	}
	privateKey, err := parsePrivateKey(options.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	client, err := newClient(options)
	if err != nil {
		return nil, err
	}
	client.appID = options.AppID
	client.installationID = options.InstallationID
	client.privateKey = privateKey
	client.profileType = ProfileGitHubApp
	return client, nil
}

// NewPAT creates a collector using one static fine-grained PAT. The token is
// retained only as credential state and is redacted from provider errors.
func NewPAT(options PATOptions) (*Client, error) {
	if strings.TrimSpace(options.Token) == "" {
		return nil, errors.New("GitHub fine-grained PAT must not be empty")
	}
	client, err := newClient(Options{
		BaseURL: options.BaseURL, HTTPClient: options.HTTPClient, Now: options.Now,
		DiscoveryPlans: options.DiscoveryPlans, CollectContext: options.CollectContext,
		CollectContribution: options.CollectContribution,
		CollectProgress:     options.CollectProgress, RequireMembersRead: options.RequireMembersRead,
		ContributionWindowDays:       options.ContributionWindowDays,
		ContributionRefreshIntervals: options.ContributionRefreshIntervals,
		ContributionRepositories:     options.ContributionRepositories,
		RateLimitBudget:              options.RateLimitBudget,
		RateLimitMaxRetries:          options.RateLimitMaxRetries, Sleep: options.Sleep,
	})
	if err != nil {
		return nil, err
	}
	client.profileType = ProfileFineGrainedPAT
	client.staticToken = options.Token
	return client, nil
}

func newClient(options Options) (*Client, error) {
	base := options.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("invalid GitHub API base URL")
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	rateLimitBudget := options.RateLimitBudget
	if rateLimitBudget <= 0 {
		rateLimitBudget = 10 * time.Minute
	}
	rateLimitMaxRetries := options.RateLimitMaxRetries
	if rateLimitMaxRetries <= 0 {
		rateLimitMaxRetries = 3
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	contributionWindowDays := options.ContributionWindowDays
	if contributionWindowDays < 1 {
		contributionWindowDays = 14
	}
	return &Client{
		baseURL:                baseURL,
		httpClient:             httpClient,
		now:                    now,
		discoveryPlans:         options.DiscoveryPlans,
		collectContext:         options.CollectContext,
		collectProgress:        options.CollectProgress,
		requireMembersRead:     options.RequireMembersRead,
		contributionWindowDays: contributionWindowDays,
		rateLimitBudget:        rateLimitBudget,
		rateLimitMaxRetries:    rateLimitMaxRetries,
		sleep:                  sleep,
		tokens:                 make(map[string]cachedToken),
	}, nil
}

// SetupCheck validates app identity, installation ownership, permissions, and
// the ability to mint a read-only installation token.
func (c *Client) SetupCheck(ctx context.Context, repositories ...string) (Setup, error) {
	return c.SetupCheckOwner(ctx, "", repositories...)
}

// SetupCheckOwner additionally verifies that the installation account matches
// the canonical owner assigned to this profile.
func (c *Client) SetupCheckOwner(
	ctx context.Context, expectedOwner string, repositories ...string,
) (Setup, error) {
	jwt, err := c.signedJWT()
	if err != nil {
		return Setup{}, err
	}
	var app struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, "/app", jwt, nil, &app); err != nil {
		return Setup{}, fmt.Errorf("validate GitHub App identity: %w", err)
	}
	if app.ID != c.appID {
		return Setup{}, fmt.Errorf("GitHub App identity mismatch: API returned app ID %d", app.ID)
	}

	var installation struct {
		ID      int64 `json:"id"`
		AppID   int64 `json:"app_id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		Permissions map[string]string `json:"permissions"`
	}
	path := fmt.Sprintf("/app/installations/%d", c.installationID)
	if err := c.requestJSON(ctx, http.MethodGet, path, jwt, nil, &installation); err != nil {
		return Setup{}, fmt.Errorf("validate GitHub App installation: %w", err)
	}
	if installation.ID != c.installationID || installation.AppID != c.appID {
		return Setup{}, errors.New("GitHub App installation does not belong to configured app")
	}
	if expectedOwner != "" && !strings.EqualFold(installation.Account.Login, expectedOwner) {
		return Setup{}, fmt.Errorf(
			"GitHub App installation account %q does not match configured owner %q",
			installation.Account.Login, expectedOwner,
		)
	}
	if installation.Permissions["pull_requests"] != "read" {
		return Setup{}, errors.New("GitHub App installation requires pull_requests: read permission")
	}
	if installation.Permissions["checks"] != "read" {
		return Setup{}, errors.New("GitHub App installation requires checks: read permission")
	}
	if installation.Permissions["metadata"] != "read" {
		return Setup{}, errors.New("GitHub App installation requires metadata: read permission")
	}
	if c.requireMembersRead && installation.Permissions["members"] != "read" {
		return Setup{}, errors.New("GitHub App installation requires members: read permission for authorization")
	}
	for permission, level := range installation.Permissions {
		if level == "write" {
			return Setup{}, fmt.Errorf("GitHub App installation has non-minimal write permission %q", permission)
		}
		if permission != "pull_requests" && permission != "checks" && permission != "metadata" &&
			(permission != "members" || !c.requireMembersRead) {
			return Setup{}, fmt.Errorf("GitHub App installation has unnecessary permission %q", permission)
		}
	}
	if _, err := c.accessToken(ctx, repositories); err != nil {
		return Setup{}, fmt.Errorf("validate installation access token: %w", err)
	}
	return Setup{AppSlug: app.Slug, AccountLogin: installation.Account.Login}, nil
}

// SetupCheckPAT probes authenticated REST and GraphQL access to one target
// repository without collecting or publishing a snapshot.
func (c *Client) SetupCheckPAT(ctx context.Context, repository string) (SetupProbe, error) {
	if c.profileType != ProfileFineGrainedPAT {
		return SetupProbe{}, errors.New("GitHub profile is not a fine-grained PAT")
	}
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return SetupProbe{}, errors.New("repository must use owner/name")
	}
	var rest struct {
		FullName string `json:"full_name"`
	}
	requestPath := fmt.Sprintf(
		"/repos/%s/%s", url.PathEscape(parts[0]), url.PathEscape(parts[1]),
	)
	if err := c.requestJSON(
		ctx, http.MethodGet, requestPath, c.staticToken, nil, &rest,
	); err != nil {
		return SetupProbe{}, fmt.Errorf("probe repository REST access: %w", err)
	}
	var graph struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
			Repository *struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{
		"query":     `query($owner:String!,$name:String!){viewer{login}repository(owner:$owner,name:$name){nameWithOwner}}`,
		"variables": map[string]string{"owner": parts[0], "name": parts[1]},
	}
	if err := c.requestJSON(
		ctx, http.MethodPost, "/graphql", c.staticToken, body, &graph,
	); err != nil {
		return SetupProbe{}, fmt.Errorf("probe authenticated GraphQL access: %w", err)
	}
	if len(graph.Errors) > 0 || graph.Data.Viewer.Login == "" ||
		graph.Data.Repository == nil ||
		!strings.EqualFold(graph.Data.Repository.NameWithOwner, repository) {
		return SetupProbe{}, errors.New("probe authenticated GraphQL access returned incomplete capability")
	}
	return SetupProbe{AccountLogin: graph.Data.Viewer.Login}, nil
}

// Collect performs a full reconciliation that bypasses the cache.
func (c *Client) Collect(ctx context.Context, repository string) (Snapshot, error) {
	return c.CollectWithOptions(ctx, repository, CollectOptions{ForceFull: true})
}

// CollectWithOptions discovers the current open set every time and returns an
// expensive cached result only when the complete versioned input fingerprint
// matches.
func (c *Client) CollectWithOptions(
	ctx context.Context, repository string, options CollectOptions,
) (Snapshot, error) {
	for attempt := 0; ; attempt++ {
		snapshot, err := c.collectWithOptionsOnce(ctx, repository, options)
		if err == nil || c.profileType != ProfileGitHubApp ||
			attempt == 1 || !isCredentialFailure(err) {
			return snapshot, err
		}
		c.evictToken(repository)
	}
}

// Discover returns only the current bounded pull-request set and its
// collection membership. It does not hydrate pull requests.
func (c *Client) Discover(ctx context.Context, repository string) (DiscoveryResult, error) {
	for attempt := 0; ; attempt++ {
		result, err := c.discoverOnce(ctx, repository)
		if err == nil || c.profileType != ProfileGitHubApp ||
			attempt == 1 || !isCredentialFailure(err) {
			return result, err
		}
		c.evictToken(repository)
	}
}

func (c *Client) discoverOnce(
	ctx context.Context, repository string,
) (DiscoveryResult, error) {
	ctx = c.withRateLimitDeadline(ctx)
	owner, name, err := splitRepository(repository)
	if err != nil {
		return DiscoveryResult{}, err
	}
	token, err := c.accessToken(ctx, []string{repository})
	if err != nil {
		return DiscoveryResult{}, fmt.Errorf("authenticate GitHub App installation: %w", err)
	}
	return c.discoverWithToken(ctx, token, repository, owner, name)
}

func (c *Client) discoverWithToken(
	ctx context.Context, token, repository, owner, name string,
) (DiscoveryResult, error) {
	matches, limits, err := c.discover(ctx, token, repository, owner, name)
	if err != nil {
		return DiscoveryResult{}, err
	}
	numbers := make([]int, 0, len(matches))
	for number := range matches {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	result := DiscoveryResult{
		Repository:   repository,
		Limits:       append([]ContextLimit(nil), limits...),
		PullRequests: make([]DiscoveredPullRequest, 0, len(numbers)),
	}
	for _, number := range numbers {
		result.PullRequests = append(result.PullRequests, DiscoveredPullRequest{
			Number:        number,
			CollectionIDs: sortedSet(matches[number]),
			ContextLimits: append([]ContextLimit(nil), limits...),
		})
	}
	return result, nil
}

// HydratePullRequests hydrates exactly one supplied bounded pull-request list.
func (c *Client) HydratePullRequests(
	ctx context.Context,
	repository string,
	pullRequests []DiscoveredPullRequest,
	options HydrationOptions,
) (HydrationResult, error) {
	for attempt := 0; ; attempt++ {
		result, err := c.hydratePullRequestsOnce(ctx, repository, pullRequests, options)
		if err == nil || c.profileType != ProfileGitHubApp ||
			attempt == 1 || !isCredentialFailure(err) {
			return result, err
		}
		c.evictToken(repository)
	}
}

func (c *Client) hydratePullRequestsOnce(
	ctx context.Context,
	repository string,
	pullRequests []DiscoveredPullRequest,
	options HydrationOptions,
) (HydrationResult, error) {
	ctx = c.withRateLimitDeadline(ctx)
	owner, name, err := splitRepository(repository)
	if err != nil {
		return HydrationResult{}, err
	}
	if err := validateHydrationBatch(pullRequests); err != nil {
		return HydrationResult{}, err
	}
	token, err := c.accessToken(ctx, []string{repository})
	if err != nil {
		return HydrationResult{}, fmt.Errorf("authenticate GitHub App installation: %w", err)
	}
	return c.hydrateWithToken(
		ctx, token, repository, owner, name, pullRequests, options,
	)
}

func (c *Client) hydrateWithToken(
	ctx context.Context,
	token, repository, owner, name string,
	discovered []DiscoveredPullRequest,
	options HydrationOptions,
) (HydrationResult, error) {
	result := HydrationResult{
		Repository: repository, CollectedAt: c.now().UTC(),
		PullRequests: make([]PullRequest, 0, len(discovered)),
		Fingerprints: make(map[int]InputFingerprint, len(discovered)),
		ForceFull:    options.ForceFull,
	}
	for _, item := range discovered {
		pr, headSHA, err := c.collectPullRequest(
			ctx, token, repository, owner, name, item.Number,
		)
		if err != nil {
			return HydrationResult{}, &incompleteError{err: err}
		}
		pr.CollectionIDs = append([]string(nil), item.CollectionIDs...)
		sort.Strings(pr.CollectionIDs)
		pr.Context.Limits = append(pr.Context.Limits, item.ContextLimits...)
		if len(pr.Context.Limits) > 0 {
			pr.Context.Completeness = "partial"
		}
		fingerprint, err := fingerprintPullRequest(pr, headSHA)
		if err != nil {
			return HydrationResult{}, &incompleteError{err: err}
		}
		result.Fingerprints[item.Number] = fingerprint
		cached, reusable := options.Cached[item.Number]
		switch {
		case !options.ForceFull && reusable && cached.Fingerprint == fingerprint:
			readiness := pr.Readiness
			headSHA := pr.HeadSHA
			pr = cached.PullRequest
			pr.HeadSHA = headSHA
			pr.Readiness = readiness
			pr.CollectionIDs = append([]string(nil), item.CollectionIDs...)
			sort.Strings(pr.CollectionIDs)
			result.CacheHits++
		case pr.ChangedFiles > 0:
			pr.Files, err = c.collectPullRequestFiles(ctx, token, owner, name, item.Number)
			if err != nil {
				return HydrationResult{}, &incompleteError{err: err}
			}
			if options.ForceFull {
				result.CacheBypasses++
			} else {
				result.CacheMisses++
			}
		case options.ForceFull:
			result.CacheBypasses++
		default:
			result.CacheMisses++
		}
		result.PullRequests = append(result.PullRequests, pr)
	}
	return result, nil
}

// CollectProgress collects progress evidence independently from discovery and
// pull-request hydration.
func (c *Client) CollectProgress(
	ctx context.Context, repository string, options ProgressOptions,
) (ProgressResult, error) {
	for attempt := 0; ; attempt++ {
		result, err := c.collectProgressOnce(ctx, repository, options)
		if err == nil || c.profileType != ProfileGitHubApp ||
			attempt == 1 || !isCredentialFailure(err) {
			return result, err
		}
		c.evictToken(repository)
	}
}

func (c *Client) collectProgressOnce(
	ctx context.Context, repository string, options ProgressOptions,
) (ProgressResult, error) {
	ctx = c.withRateLimitDeadline(ctx)
	owner, name, err := splitRepository(repository)
	if err != nil {
		return ProgressResult{}, err
	}
	token, err := c.accessToken(ctx, []string{repository})
	if err != nil {
		return ProgressResult{}, fmt.Errorf("authenticate GitHub App installation: %w", err)
	}
	return c.collectProgressWithToken(
		ctx, token, repository, owner, name, options,
	)
}

func (c *Client) collectProgressWithToken(
	ctx context.Context,
	token, repository, owner, name string,
	options ProgressOptions,
) (ProgressResult, error) {
	events, threads, err := c.collectProgressEvents(
		ctx, token, repository, owner, name, options.ProgressSince,
	)
	if err != nil {
		return ProgressResult{}, &incompleteError{err: err}
	}
	return ProgressResult{
		Repository: repository, ProgressEvents: events, ReviewThreads: threads,
		CollectedAt: c.now().UTC(),
	}, nil
}

func (c *Client) collectWithOptionsOnce(
	ctx context.Context, repository string, options CollectOptions,
) (Snapshot, error) {
	ctx = c.withRateLimitDeadline(ctx)
	owner, name, err := splitRepository(repository)
	if err != nil {
		return Snapshot{}, err
	}
	token, err := c.accessToken(ctx, []string{repository})
	if err != nil {
		return Snapshot{}, fmt.Errorf("authenticate GitHub App installation: %w", err)
	}
	discovery, err := c.discoverWithToken(ctx, token, repository, owner, name)
	if err != nil {
		return Snapshot{}, err
	}
	hydration, err := c.hydrateWithToken(
		ctx, token, repository, owner, name, discovery.PullRequests,
		HydrationOptions{Cached: options.Cached, ForceFull: options.ForceFull},
	)
	if err != nil {
		return Snapshot{}, err
	}
	var progress ProgressResult
	if c.collectProgress {
		progress, err = c.collectProgressWithToken(
			ctx, token, repository, owner, name,
			ProgressOptions{ProgressSince: options.ProgressSince},
		)
		if err != nil {
			return Snapshot{}, err
		}
	}
	return Snapshot{
		Repository: repository, PullRequests: hydration.PullRequests,
		ProgressEvents: progress.ProgressEvents, ReviewThreads: progress.ReviewThreads,
		CollectedAt: c.now().UTC(), Limits: discovery.Limits,
		Fingerprints: hydration.Fingerprints,
		CacheHits:    hydration.CacheHits, CacheMisses: hydration.CacheMisses,
		CacheBypasses:      hydration.CacheBypasses,
		FullReconciliation: options.ForceFull,
	}, nil
}

func (c *Client) withRateLimitDeadline(ctx context.Context) context.Context {
	if _, exists := ctx.Value(rateLimitDeadlineKey{}).(time.Time); exists {
		return ctx
	}
	return context.WithValue(
		ctx, rateLimitDeadlineKey{}, c.now().UTC().Add(c.rateLimitBudget),
	)
}

func splitRepository(repository string) (string, string, error) {
	owner, name, valid := strings.Cut(repository, "/")
	if !valid || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", errors.New("repository must use owner/name")
	}
	return owner, name, nil
}

func validateHydrationBatch(pullRequests []DiscoveredPullRequest) error {
	if len(pullRequests) > MaxHydrationBatchSize {
		return fmt.Errorf(
			"pull request hydration batch exceeds %d entries", MaxHydrationBatchSize,
		)
	}
	seen := make(map[int]struct{}, len(pullRequests))
	for _, pullRequest := range pullRequests {
		if pullRequest.Number <= 0 {
			return errors.New("pull request number must be positive")
		}
		if _, exists := seen[pullRequest.Number]; exists {
			return fmt.Errorf("pull request hydration batch repeats number %d", pullRequest.Number)
		}
		seen[pullRequest.Number] = struct{}{}
	}
	return nil
}

func isCredentialFailure(err error) bool {
	var routeFailure *RouteFailure
	return errors.As(err, &routeFailure) &&
		routeFailure.Category == FailureCredentialInvalid &&
		routeFailure.refresh
}

func (c *Client) evictToken(repository string) {
	c.tokenMu.Lock()
	delete(c.tokens, tokenCacheKey([]string{repository}))
	c.tokenMu.Unlock()
}

// CollectRepositoryContribution collects only the facts for one author in one
// repository. Its installation token is narrowed to that repository.
func (c *Client) CollectRepositoryContribution(
	ctx context.Context, authorID int64, login, repository, association string,
) (RepositoryContributionEvidence, error) {
	for attempt := 0; ; attempt++ {
		result, err := c.collectRepositoryContributionOnce(
			ctx, authorID, login, repository, association,
		)
		if err == nil || c.profileType != ProfileGitHubApp ||
			attempt == 1 || !isCredentialFailure(err) {
			return result, err
		}
		c.evictToken(repository)
	}
}

func (c *Client) collectRepositoryContributionOnce(
	ctx context.Context, authorID int64, login, repository, association string,
) (RepositoryContributionEvidence, error) {
	if authorID <= 0 || login == "" {
		return RepositoryContributionEvidence{}, errors.New("author identity is required")
	}
	if _, _, ok := strings.Cut(repository, "/"); !ok {
		return RepositoryContributionEvidence{}, errors.New("repository must use owner/name")
	}
	token, err := c.accessToken(ctx, []string{repository})
	if err != nil {
		return RepositoryContributionEvidence{}, fmt.Errorf("authenticate contribution evidence: %w", err)
	}
	items, complete, err := c.searchAuthorPullRequests(
		ctx, token, fmt.Sprintf("repo:%s is:pr author:%s", repository, login),
	)
	if err != nil {
		return RepositoryContributionEvidence{}, fmt.Errorf(
			"collect contribution evidence for %s in %s: %w",
			login, repository, err,
		)
	}
	if !complete {
		return RepositoryContributionEvidence{}, &incompleteError{
			err: fmt.Errorf("collect contribution evidence for %s in %s", login, repository),
		}
	}
	history := contribution.RepositoryHistory{Association: association}
	for _, item := range items {
		if associationRank(item.AuthorAssociation) > associationRank(history.Association) {
			history.Association = item.AuthorAssociation
		}
		switch {
		case strings.EqualFold(item.State, "open"):
			history.Open++
		case item.PullRequest.MergedAt != nil:
			history.Merged++
		default:
			history.ClosedUnmerged++
		}
	}
	return RepositoryContributionEvidence{
		AuthorID: authorID, Login: login, Repository: repository,
		History: history, Complete: true, CollectedAt: c.now().UTC(),
	}, nil
}

// CollectAncillaryAuthorEvidence uses only unauthenticated public endpoints.
// It never obtains or falls back to a configured profile token.
func (a *AncillaryClient) CollectAncillaryAuthorEvidence(
	ctx context.Context, authorID int64, login string,
) (AncillaryAuthorEvidence, error) {
	c := a.client
	result := AncillaryAuthorEvidence{
		AuthorID: authorID, Login: login, Complete: true, CollectedAt: c.now().UTC(),
	}
	var user struct {
		ID        int64     `json:"id"`
		Login     string    `json:"login"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := c.requestJSON(
		ctx, http.MethodGet, "/users/"+url.PathEscape(login), "", nil, &user,
	); err != nil {
		return AncillaryAuthorEvidence{}, err
	}
	if user.ID != authorID || user.Login == "" || user.CreatedAt.IsZero() {
		return AncillaryAuthorEvidence{}, &incompleteError{err: errors.New("incomplete author profile")}
	}
	result.AccountCreatedAt = user.CreatedAt
	cutoff := c.now().UTC().AddDate(0, 0, -c.contributionWindowDays).Format("2006-01-02")
	items, complete, err := c.searchAuthorPullRequests(
		ctx, "", fmt.Sprintf("is:pr author:%s created:>=%s", login, cutoff),
	)
	if err != nil {
		return AncillaryAuthorEvidence{}, fmt.Errorf("collect recent author activity: %w", err)
	}
	if !complete {
		return AncillaryAuthorEvidence{}, &incompleteError{err: errors.New("incomplete recent author activity")}
	}
	for _, item := range items {
		repository, ok := repositoryFromAPIURL(item.RepositoryURL)
		if !ok || item.CreatedAt.IsZero() {
			return AncillaryAuthorEvidence{}, &incompleteError{err: errors.New("invalid recent author activity")}
		}
		result.RecentActivity = append(result.RecentActivity, contribution.RepositoryActivity{
			Repository: repository, OccurredAt: item.CreatedAt,
		})
	}
	return result, nil
}

func (c *Client) discover(
	ctx context.Context,
	token, repository, owner, name string,
) (map[int]map[string]struct{}, []ContextLimit, error) {
	plans := c.discoveryPlans[repository]
	if len(plans) == 0 {
		plans = []DiscoveryTarget{{Mode: "all_open"}}
	}
	strategies := make(map[string][]string)
	for _, plan := range plans {
		mode := plan.Mode
		if mode == "" {
			mode = "all_open"
		}
		key := mode + "\x00" + plan.Query
		strategies[key] = append(strategies[key], plan.CollectionID)
	}
	matches := make(map[int]map[string]struct{})
	var limits []ContextLimit
	for strategy, collectionIDs := range strategies {
		mode, query, _ := strings.Cut(strategy, "\x00")
		var numbers []int
		var limit *ContextLimit
		var err error
		switch mode {
		case "all_open":
			numbers, err = c.discoverAllOpen(ctx, token, repository, owner, name)
		case "search":
			numbers, limit, err = c.discoverSearch(ctx, token, repository, query)
		default:
			return nil, nil, fmt.Errorf("unsupported discovery mode %q", mode)
		}
		if err != nil {
			return nil, nil, err
		}
		if limit != nil {
			limits = append(limits, *limit)
		}
		for _, number := range numbers {
			if matches[number] == nil {
				matches[number] = make(map[string]struct{})
			}
			for _, collectionID := range collectionIDs {
				if collectionID != "" {
					matches[number][collectionID] = struct{}{}
				}
			}
		}
	}
	return matches, limits, nil
}

func (c *Client) discoverAllOpen(
	ctx context.Context,
	token, repository, owner, name string,
) ([]int, error) {
	var numbers []int
	for page := 1; ; page++ {
		var batch []struct {
			Number int `json:"number"`
		}
		listPath := fmt.Sprintf(
			"/repos/%s/%s/pulls?state=open&per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(name), page,
		)
		if err := c.requestJSON(ctx, http.MethodGet, listPath, token, nil, &batch); err != nil {
			wrapped := fmt.Errorf("list open pull requests for %s page %d: %w", repository, page, err)
			if page > 1 {
				return nil, &incompleteError{err: wrapped}
			}
			return nil, wrapped
		}
		for _, item := range batch {
			numbers = append(numbers, item.Number)
		}
		if len(batch) < 100 {
			return numbers, nil
		}
	}
}

func (c *Client) discoverSearch(
	ctx context.Context,
	token, repository, configuredQuery string,
) ([]int, *ContextLimit, error) {
	query := fmt.Sprintf("repo:%s is:pr is:open %s", repository, configuredQuery)
	var numbers []int
	total := 0
	incomplete := false
	for page := 1; page <= 10; page++ {
		var result struct {
			TotalCount        int  `json:"total_count"`
			IncompleteResults bool `json:"incomplete_results"`
			Items             []struct {
				Number int `json:"number"`
			} `json:"items"`
		}
		values := url.Values{
			"q": {query}, "per_page": {"100"}, "page": {strconv.Itoa(page)},
		}
		searchPath := "/search/issues?" + values.Encode()
		if err := c.requestJSON(ctx, http.MethodGet, searchPath, token, nil, &result); err != nil {
			wrapped := fmt.Errorf("search pull requests for %s page %d: %w", repository, page, err)
			if page > 1 {
				return nil, nil, &incompleteError{err: wrapped}
			}
			return nil, nil, wrapped
		}
		total = result.TotalCount
		incomplete = incomplete || result.IncompleteResults
		for _, item := range result.Items {
			numbers = append(numbers, item.Number)
		}
		if len(result.Items) < 100 {
			break
		}
	}
	numbers = deduplicateInts(numbers)
	if incomplete || total > len(numbers) {
		return numbers, &ContextLimit{
			Scope: "discovery_search", Reason: "provider_limit",
			Collected: len(numbers), Total: total,
		}, nil
	}
	return numbers, nil, nil
}

type progressCandidate struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

type timelineEvent struct {
	ID    int64  `json:"id"`
	Event string `json:"event"`
	Actor struct {
		ID int64 `json:"id"`
	} `json:"actor"`
	User struct {
		ID int64 `json:"id"`
	} `json:"user"`
	CreatedAt   time.Time `json:"created_at"`
	SubmittedAt time.Time `json:"submitted_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	HTMLURL     string    `json:"html_url"`
}

func (c *Client) collectProgressEvents(
	ctx context.Context,
	token, repository, owner, name string,
	progressSince time.Time,
) ([]ProgressEvent, []ReviewThreadState, error) {
	cutoff := c.now().UTC().AddDate(0, -3, 0)
	if incrementalCutoff := progressSince.UTC().Add(-5 * time.Minute); incrementalCutoff.After(cutoff) {
		cutoff = incrementalCutoff
	}
	candidates, err := c.listProgressCandidates(ctx, token, owner, name, cutoff)
	if err != nil {
		return nil, nil, err
	}
	memberships, err := c.progressMemberships(ctx, token, repository, candidates, cutoff)
	if err != nil {
		return nil, nil, err
	}
	var result []ProgressEvent
	var reviewThreads []ReviewThreadState
	for _, candidate := range candidates {
		collectionIDs := memberships[candidate.Number]
		if len(collectionIDs) == 0 {
			continue
		}
		timeline, err := c.collectTimelineEvents(ctx, token, owner, name, candidate.Number)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range timeline {
			activityType := map[string]string{
				"reviewed":  QualifyingActivityReview,
				"commented": QualifyingActivityComment,
				"merged":    QualifyingActivityMerge,
				"closed":    QualifyingActivityClose,
			}[item.Event]
			actorID := item.Actor.ID
			if actorID == 0 {
				actorID = item.User.ID
			}
			occurredAt := item.CreatedAt
			if occurredAt.IsZero() {
				occurredAt = item.SubmittedAt
			}
			if occurredAt.IsZero() {
				occurredAt = item.UpdatedAt
			}
			if activityType == "" || item.ID == 0 || actorID == 0 ||
				occurredAt.Before(cutoff) || occurredAt.IsZero() {
				continue
			}
			activityURL := item.HTMLURL
			if activityURL == "" {
				activityURL = candidate.HTMLURL
			}
			result = append(result, ProgressEvent{
				ID: strconv.FormatInt(item.ID, 10), ActivityType: activityType,
				ActorID: actorID, Repository: repository, Number: candidate.Number,
				Title: candidate.Title, PullRequestURL: candidate.HTMLURL,
				ActivityURL: activityURL, OccurredAt: occurredAt,
				CollectionIDs: append([]string(nil), collectionIDs...),
			})
		}
		threads, err := c.collectReviewThreads(
			ctx, token, repository, owner, name, candidate, collectionIDs,
		)
		if err != nil {
			return nil, nil, err
		}
		reviewThreads = append(reviewThreads, threads...)
	}
	seen := make(map[string]struct{}, len(result))
	deduplicated := result[:0]
	for _, event := range result {
		key := strings.Join([]string{
			event.Repository, strconv.Itoa(event.Number), event.ActivityType, event.ID,
		}, "\x00")
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		deduplicated = append(deduplicated, event)
	}
	sort.Slice(deduplicated, func(i, j int) bool {
		if !deduplicated[i].OccurredAt.Equal(deduplicated[j].OccurredAt) {
			return deduplicated[i].OccurredAt.Before(deduplicated[j].OccurredAt)
		}
		return deduplicated[i].ID < deduplicated[j].ID
	})
	return deduplicated, reviewThreads, nil
}

const (
	QualifyingActivityReview         = "review"
	QualifyingActivityComment        = "comment"
	QualifyingActivityThreadResolved = "thread_resolved"
	QualifyingActivityMerge          = "merge"
	QualifyingActivityClose          = "close"
)

func (c *Client) listProgressCandidates(
	ctx context.Context,
	token, owner, name string,
	cutoff time.Time,
) ([]progressCandidate, error) {
	var result []progressCandidate
	for page := 1; page <= 30; page++ {
		var batch []progressCandidate
		requestPath := fmt.Sprintf(
			"/repos/%s/%s/pulls?state=all&sort=updated&direction=desc&per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(name), page,
		)
		if err := c.requestJSON(ctx, http.MethodGet, requestPath, token, nil, &batch); err != nil {
			return nil, fmt.Errorf("list recent pull requests for progress page %d: %w", page, err)
		}
		reachedCutoff := false
		for _, candidate := range batch {
			if candidate.UpdatedAt.Before(cutoff) {
				reachedCutoff = true
				continue
			}
			if candidate.Number > 0 && candidate.Title != "" && candidate.HTMLURL != "" {
				result = append(result, candidate)
			}
		}
		if len(batch) < 100 || reachedCutoff {
			return result, nil
		}
	}
	return nil, errors.New("recent pull request activity exceeded the local 3000 PR limit")
}

func (c *Client) progressMemberships(
	ctx context.Context,
	token, repository string,
	candidates []progressCandidate,
	cutoff time.Time,
) (map[int][]string, error) {
	sets := make(map[int]map[string]struct{}, len(candidates))
	plans := c.discoveryPlans[repository]
	for _, plan := range plans {
		if plan.CollectionID == "" {
			continue
		}
		switch plan.Mode {
		case "", "all_open":
			for _, candidate := range candidates {
				if sets[candidate.Number] == nil {
					sets[candidate.Number] = make(map[string]struct{})
				}
				sets[candidate.Number][plan.CollectionID] = struct{}{}
			}
		case "search":
			numbers, err := c.searchProgressCandidates(
				ctx, token, repository, plan.Query, cutoff,
			)
			if err != nil {
				return nil, err
			}
			for _, number := range numbers {
				if sets[number] == nil {
					sets[number] = make(map[string]struct{})
				}
				sets[number][plan.CollectionID] = struct{}{}
			}
		default:
			return nil, fmt.Errorf("unsupported discovery mode %q", plan.Mode)
		}
	}
	result := make(map[int][]string, len(sets))
	for number, collectionIDs := range sets {
		result[number] = sortedSet(collectionIDs)
	}
	return result, nil
}

func (c *Client) searchProgressCandidates(
	ctx context.Context,
	token, repository, configuredQuery string,
	cutoff time.Time,
) ([]int, error) {
	query := fmt.Sprintf(
		"repo:%s is:pr updated:>=%s %s",
		repository, cutoff.Format("2006-01-02"), configuredQuery,
	)
	var result []int
	for page := 1; page <= 10; page++ {
		var response struct {
			TotalCount        int  `json:"total_count"`
			IncompleteResults bool `json:"incomplete_results"`
			Items             []struct {
				Number int `json:"number"`
			} `json:"items"`
		}
		values := url.Values{
			"q": {query}, "per_page": {"100"}, "page": {strconv.Itoa(page)},
		}
		if err := c.requestJSON(
			ctx, http.MethodGet, "/search/issues?"+values.Encode(), token, nil, &response,
		); err != nil {
			return nil, fmt.Errorf("search recent progress pull requests: %w", err)
		}
		for _, item := range response.Items {
			result = append(result, item.Number)
		}
		if response.IncompleteResults {
			return nil, errors.New("recent progress search returned incomplete results")
		}
		if len(response.Items) < 100 {
			if len(result) < response.TotalCount {
				return nil, errors.New("recent progress search was truncated")
			}
			return deduplicateInts(result), nil
		}
	}
	return nil, errors.New("recent progress search exceeded the provider limit")
}

func (c *Client) collectTimelineEvents(
	ctx context.Context,
	token, owner, name string,
	number int,
) ([]timelineEvent, error) {
	var result []timelineEvent
	for page := 1; ; page++ {
		var batch []timelineEvent
		requestPath := fmt.Sprintf(
			"/repos/%s/%s/issues/%d/timeline?per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(name), number, page,
		)
		if err := c.requestJSON(ctx, http.MethodGet, requestPath, token, nil, &batch); err != nil {
			return nil, fmt.Errorf("list progress timeline for PR %d page %d: %w", number, page, err)
		}
		result = append(result, batch...)
		if len(batch) < 100 {
			return result, nil
		}
	}
}

func (c *Client) collectReviewThreads(
	ctx context.Context,
	token, repository, owner, name string,
	candidate progressCandidate,
	collectionIDs []string,
) ([]ReviewThreadState, error) {
	const query = `query($owner:String!,$name:String!,$number:Int!,$cursor:String){
		repository(owner:$owner,name:$name){
			pullRequest(number:$number){
				reviewThreads(first:100,after:$cursor){
					nodes{id isResolved resolvedBy{databaseId} comments(last:1){nodes{updatedAt url}}}
					pageInfo{hasNextPage endCursor}
				}
			}
		}
	}`
	var result []ReviewThreadState
	var cursor any
	for {
		var response struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewThreads struct {
							Nodes []struct {
								ID         string `json:"id"`
								IsResolved bool   `json:"isResolved"`
								ResolvedBy *struct {
									DatabaseID int64 `json:"databaseId"`
								} `json:"resolvedBy"`
								Comments struct {
									Nodes []struct {
										UpdatedAt time.Time `json:"updatedAt"`
										URL       string    `json:"url"`
									} `json:"nodes"`
								} `json:"comments"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		body := map[string]any{
			"query": query,
			"variables": map[string]any{
				"owner": owner, "name": name, "number": candidate.Number, "cursor": cursor,
			},
		}
		if err := c.requestJSON(ctx, http.MethodPost, "/graphql", token, body, &response); err != nil {
			return nil, fmt.Errorf("collect resolved review threads for %s#%d: %w", repository, candidate.Number, err)
		}
		if len(response.Errors) > 0 {
			return nil, fmt.Errorf("collect resolved review threads for %s#%d: %s", repository, candidate.Number, response.Errors[0].Message)
		}
		threads := response.Data.Repository.PullRequest.ReviewThreads
		for _, thread := range threads.Nodes {
			if thread.ID == "" {
				continue
			}
			activityURL := candidate.HTMLURL
			if len(thread.Comments.Nodes) > 0 {
				comment := thread.Comments.Nodes[0]
				if comment.URL != "" {
					activityURL = comment.URL
				}
			}
			var resolvedBy int64
			if thread.ResolvedBy != nil {
				resolvedBy = thread.ResolvedBy.DatabaseID
			}
			result = append(result, ReviewThreadState{
				ID: thread.ID, Resolved: thread.IsResolved, ResolvedBy: resolvedBy,
				Repository: repository, Number: candidate.Number, Title: candidate.Title,
				PullRequestURL: candidate.HTMLURL, ActivityURL: activityURL,
				CollectionIDs: append([]string(nil), collectionIDs...),
			})
		}
		if !threads.PageInfo.HasNextPage {
			return result, nil
		}
		if threads.PageInfo.EndCursor == "" {
			return nil, errors.New("resolved review thread pagination omitted a cursor")
		}
		cursor = threads.PageInfo.EndCursor
	}
}

func (c *Client) collectPullRequest(
	ctx context.Context,
	token, repository, owner, name string,
	number int,
) (PullRequest, string, error) {
	pullPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(name), number)
	var detail struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		User   struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"user"`
		AuthorAssociation  string    `json:"author_association"`
		CreatedAt          time.Time `json:"created_at"`
		UpdatedAt          time.Time `json:"updated_at"`
		HTMLURL            string    `json:"html_url"`
		Additions          int       `json:"additions"`
		Deletions          int       `json:"deletions"`
		ChangedFiles       int       `json:"changed_files"`
		Draft              bool      `json:"draft"`
		Mergeable          *bool     `json:"mergeable"`
		MergeableState     string    `json:"mergeable_state"`
		RequestedReviewers []struct {
			Login string `json:"login"`
		} `json:"requested_reviewers"`
		RequestedTeams []struct {
			Slug string `json:"slug"`
		} `json:"requested_teams"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, pullPath, token, nil, &detail); err != nil {
		return PullRequest{}, "", fmt.Errorf("get %s#%d: %w", repository, number, err)
	}

	var reviews []review
	for page := 1; ; page++ {
		var batch []review
		reviewsPath := fmt.Sprintf("%s/reviews?per_page=100&page=%d", pullPath, page)
		if err := c.requestJSON(ctx, http.MethodGet, reviewsPath, token, nil, &batch); err != nil {
			return PullRequest{}, "", fmt.Errorf("list reviews for %s#%d page %d: %w", repository, number, page, err)
		}
		reviews = append(reviews, batch...)
		if len(batch) < 100 {
			break
		}
	}
	checks, err := c.collectCheckRuns(ctx, token, owner, name, detail.Head.SHA)
	if err != nil {
		return PullRequest{}, "", err
	}
	context := PullRequestContext{Completeness: "complete", CollectedAt: c.now().UTC()}
	if c.collectContext {
		comments, err := c.collectReviewComments(ctx, token, repository, pullPath)
		if err != nil {
			return PullRequest{}, "", err
		}
		commits, err := c.collectCommits(ctx, token, repository, pullPath)
		if err != nil {
			return PullRequest{}, "", err
		}
		context = c.buildContext(
			repository, number, detail.Title+"\n"+detail.Body, detail.UpdatedAt,
			reviews, comments, commits,
		)
		if err := c.collectAssociatedPullRequests(ctx, token, owner, name, number, commits, &context); err != nil {
			return PullRequest{}, "", err
		}
		context = deduplicateContext(context)
	}
	requestedReviewers := make([]string, 0, len(detail.RequestedReviewers))
	for _, reviewer := range detail.RequestedReviewers {
		if reviewer.Login != "" {
			requestedReviewers = append(requestedReviewers, reviewer.Login)
		}
	}
	requestedTeams := make([]string, 0, len(detail.RequestedTeams))
	for _, team := range detail.RequestedTeams {
		if team.Slug != "" {
			requestedTeams = append(requestedTeams, team.Slug)
		}
	}
	return PullRequest{
		Repository:        repository,
		Number:            detail.Number,
		HeadSHA:           detail.Head.SHA,
		Title:             detail.Title,
		Body:              truncateBytes(detail.Body, maxBodyBytes),
		Author:            detail.User.Login,
		AuthorID:          detail.User.ID,
		AuthorAssociation: detail.AuthorAssociation,
		CreatedAt:         detail.CreatedAt,
		UpdatedAt:         detail.UpdatedAt,
		URL:               detail.HTMLURL,
		Additions:         detail.Additions,
		Deletions:         detail.Deletions,
		ChangedFiles:      detail.ChangedFiles,
		ReviewState: reviewState(
			detail.Draft,
			len(detail.RequestedReviewers) > 0 || len(detail.RequestedTeams) > 0,
			reviews,
		),
		Readiness: PullRequestReadiness{
			RequestedReviewers: requestedReviewers,
			RequestedTeams:     requestedTeams,
			Mergeable:          detail.Mergeable,
			MergeableState:     detail.MergeableState,
			Checks:             checks,
			CollectedAt:        c.now().UTC(),
		},
		Context: context,
	}, detail.Head.SHA, nil
}

// CollectPullRequestDiff fetches authenticated per-file patches and applies
// deterministic local limits suitable for durable caching.
func (c *Client) CollectPullRequestDiff(
	ctx context.Context, repository string, number int, headSHA string, changedFiles int,
) (PullRequestDiffEvidence, error) {
	for attempt := 0; ; attempt++ {
		result, err := c.collectPullRequestDiffOnce(
			ctx, repository, number, headSHA, changedFiles,
		)
		if err == nil || c.profileType != ProfileGitHubApp ||
			attempt == 1 || !isCredentialFailure(err) {
			return result, err
		}
		c.evictToken(repository)
	}
}

func (c *Client) collectPullRequestDiffOnce(
	ctx context.Context, repository string, number int, headSHA string, changedFiles int,
) (PullRequestDiffEvidence, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || number <= 0 || headSHA == "" {
		return PullRequestDiffEvidence{}, errors.New("pull request diff identity is required")
	}
	token, err := c.accessToken(ctx, []string{repository})
	if err != nil {
		return PullRequestDiffEvidence{}, fmt.Errorf("authenticate pull request diff: %w", err)
	}
	pullPath := fmt.Sprintf(
		"/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(name), number,
	)
	if err := c.verifyPullRequestHead(ctx, pullPath, token, headSHA); err != nil {
		return PullRequestDiffEvidence{}, err
	}
	var files []struct {
		Filename string `json:"filename"`
		Patch    string `json:"patch"`
	}
	const maxFiles = 100
	for page := 1; len(files) < maxFiles; page++ {
		var batch []struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		}
		perPage := min(100, maxFiles-len(files))
		requestPath := fmt.Sprintf("%s/files?per_page=%d&page=%d", pullPath, perPage, page)
		if err := c.requestJSON(ctx, http.MethodGet, requestPath, token, nil, &batch); err != nil {
			return PullRequestDiffEvidence{}, fmt.Errorf(
				"collect pull request diff for %s#%d page %d: %w",
				repository, number, page, err,
			)
		}
		files = append(files, batch[:min(len(batch), maxFiles-len(files))]...)
		if len(batch) < perPage {
			break
		}
	}
	if err := c.verifyPullRequestHead(ctx, pullPath, token, headSHA); err != nil {
		return PullRequestDiffEvidence{}, err
	}
	if changedFiles < len(files) {
		changedFiles = len(files)
	}
	result := PullRequestDiffEvidence{OriginalFiles: changedFiles}
	const maxFileBytes, maxTotalBytes = 8 << 10, 128 << 10
	for _, file := range files {
		result.OriginalBytes += len(file.Patch)
	}
	remaining := maxTotalBytes
	for _, file := range files {
		if file.Filename == "" || file.Patch == "" || remaining == 0 {
			continue
		}
		originalBytes := len(file.Patch)
		limit := min(maxFileBytes, remaining)
		patch := truncateBytes(file.Patch, limit)
		if patch == "" {
			result.Truncated = true
			continue
		}
		source := PullRequestDiffSource{
			Path: file.Filename, Patch: patch, OriginalBytes: originalBytes,
			SentBytes: len(patch), Truncated: len(patch) != originalBytes,
		}
		result.Sources = append(result.Sources, source)
		result.SentBytes += len(patch)
		remaining -= len(patch)
		result.Truncated = result.Truncated || source.Truncated
	}
	result.SentFiles = len(result.Sources)
	result.OmittedFiles = max(result.OriginalFiles-result.SentFiles, 0)
	result.Truncated = result.Truncated || result.OmittedFiles > 0
	result.Completeness = "complete"
	if result.Truncated {
		result.Completeness = "partial"
	}
	return result, nil
}

func (c *Client) verifyPullRequestHead(
	ctx context.Context, pullPath, token, expected string,
) error {
	var detail struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, pullPath, token, nil, &detail); err != nil {
		return fmt.Errorf("verify pull request head: %w", err)
	}
	if detail.Head.SHA != expected {
		return &PullRequestHeadChangedError{Expected: expected, Observed: detail.Head.SHA}
	}
	return nil
}

func (c *Client) collectCheckRuns(
	ctx context.Context, token, owner, name, headSHA string,
) (CheckSummary, error) {
	summary := CheckSummary{Runs: make([]CheckRun, 0)}
	if headSHA == "" {
		return summary, nil
	}
	for page := 1; ; page++ {
		var response struct {
			Total int `json:"total_count"`
			Runs  []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				URL        string `json:"html_url"`
			} `json:"check_runs"`
		}
		path := fmt.Sprintf(
			"/repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(name), url.PathEscape(headSHA), page,
		)
		if err := c.requestJSON(ctx, http.MethodGet, path, token, nil, &response); err != nil {
			return CheckSummary{}, fmt.Errorf(
				"list check runs for %s/%s@%s page %d: %w",
				owner, name, headSHA, page, err,
			)
		}
		summary.Total = response.Total
		for _, run := range response.Runs {
			summary.Runs = append(summary.Runs, CheckRun{
				ID: run.ID, Name: run.Name, Status: run.Status,
				Conclusion: run.Conclusion, URL: run.URL,
			})
		}
		if len(response.Runs) < 100 {
			break
		}
	}
	if summary.Total < len(summary.Runs) {
		summary.Total = len(summary.Runs)
	}
	return summary, nil
}

type authorSearchItem struct {
	State             string    `json:"state"`
	CreatedAt         time.Time `json:"created_at"`
	RepositoryURL     string    `json:"repository_url"`
	AuthorAssociation string    `json:"author_association"`
	PullRequest       struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

func (c *Client) searchAuthorPullRequests(
	ctx context.Context,
	token, query string,
) ([]authorSearchItem, bool, error) {
	var items []authorSearchItem
	total := 0
	complete := true
	for page := 1; page <= 10; page++ {
		var result struct {
			TotalCount        int                `json:"total_count"`
			IncompleteResults bool               `json:"incomplete_results"`
			Items             []authorSearchItem `json:"items"`
		}
		values := url.Values{
			"q": {query}, "per_page": {"100"}, "page": {strconv.Itoa(page)},
		}
		if err := c.requestJSON(
			ctx, http.MethodGet, "/search/issues?"+values.Encode(), token, nil, &result,
		); err != nil {
			return items, false, err
		}
		total = result.TotalCount
		complete = complete && !result.IncompleteResults
		items = append(items, result.Items...)
		if len(result.Items) < 100 {
			break
		}
	}
	return items, complete && len(items) >= total, nil
}

func repositoryFromAPIURL(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "repos" || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return parts[1] + "/" + parts[2], true
}

func associationRank(value string) int {
	switch strings.ToUpper(value) {
	case "OWNER":
		return 5
	case "MEMBER":
		return 4
	case "COLLABORATOR":
		return 3
	case "CONTRIBUTOR":
		return 2
	case "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER":
		return 1
	default:
		return 0
	}
}

type changedFile struct {
	Filename  string `json:"filename"`
	SHA       string `json:"sha"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

func fingerprintPullRequest(pr PullRequest, headSHA string) (InputFingerprint, error) {
	// Observation times do not describe GitHub input and must not invalidate
	// an otherwise valid cache entry.
	pr.Context.CollectedAt = time.Time{}
	pr.Readiness = PullRequestReadiness{}
	pr.Context.ExternalURLs = append([]ExternalURL(nil), pr.Context.ExternalURLs...)
	for index := range pr.Context.ExternalURLs {
		pr.Context.ExternalURLs[index].DiscoveredAt = time.Time{}
	}
	body, err := json.Marshal(struct {
		Version string      `json:"version"`
		HeadSHA string      `json:"headSHA"`
		Input   PullRequest `json:"input"`
	}{
		Version: InputFingerprintVersion,
		HeadSHA: headSHA,
		Input:   pr,
	})
	if err != nil {
		return InputFingerprint{}, fmt.Errorf("encode pull request input fingerprint: %w", err)
	}
	sum := sha256.Sum256(body)
	return InputFingerprint{
		Version: InputFingerprintVersion,
		Value:   hex.EncodeToString(sum[:]),
	}, nil
}

func (c *Client) collectPullRequestFiles(
	ctx context.Context, token, owner, name string, number int,
) ([]PullRequestFile, error) {
	var changed []changedFile
	for page := 1; page <= 30; page++ {
		var batch []changedFile
		filesPath := fmt.Sprintf(
			"/repos/%s/%s/pulls/%d/files?per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(name), number, page,
		)
		if err := c.requestJSON(ctx, http.MethodGet, filesPath, token, nil, &batch); err != nil {
			return nil, err
		}
		changed = append(changed, batch...)
		if len(batch) < 100 {
			break
		}
		if page == 30 {
			return nil, errors.New("pull request file list exceeds 3000 files")
		}
	}
	return pullRequestFiles(changed), nil
}

func pullRequestFiles(changed []changedFile) []PullRequestFile {
	files := make([]PullRequestFile, 0, len(changed))
	for _, file := range changed {
		files = append(files, PullRequestFile{
			Path: file.Filename, Additions: file.Additions, Deletions: file.Deletions,
		})
	}
	return files
}

func (c *Client) collectReviewComments(
	ctx context.Context, token, repository, pullPath string,
) ([]reviewComment, error) {
	var comments []reviewComment
	for page := 1; ; page++ {
		var batch []reviewComment
		path := fmt.Sprintf("%s/comments?per_page=100&page=%d", pullPath, page)
		if err := c.requestJSON(ctx, http.MethodGet, path, token, nil, &batch); err != nil {
			return nil, fmt.Errorf("list review threads for %s page %d: %w", repository, page, err)
		}
		comments = append(comments, batch...)
		if len(batch) < 100 {
			return comments, nil
		}
	}
}

func (c *Client) collectCommits(
	ctx context.Context, token, repository, pullPath string,
) ([]commit, error) {
	var commits []commit
	for page := 1; ; page++ {
		var batch []commit
		path := fmt.Sprintf("%s/commits?per_page=100&page=%d", pullPath, page)
		if err := c.requestJSON(ctx, http.MethodGet, path, token, nil, &batch); err != nil {
			return nil, fmt.Errorf("list commits for %s page %d: %w", repository, page, err)
		}
		commits = append(commits, batch...)
		if len(batch) < 100 {
			return commits, nil
		}
	}
}

func (c *Client) collectAssociatedPullRequests(
	ctx context.Context,
	token, owner, name string,
	focalNumber int,
	commits []commit,
	context *PullRequestContext,
) error {
	const maxAssociatedPullRequests = 100
	seen := make(map[string]struct{})
	for _, relationship := range context.Relationships {
		seen[relationship.Kind+"\x00"+relationship.URL] = struct{}{}
	}
	for _, item := range commits[:min(len(commits), 100)] {
		for page := 1; ; page++ {
			var pulls []struct {
				Number    int       `json:"number"`
				Title     string    `json:"title"`
				State     string    `json:"state"`
				HTMLURL   string    `json:"html_url"`
				UpdatedAt time.Time `json:"updated_at"`
				Base      struct {
					Repo struct {
						FullName string `json:"full_name"`
					} `json:"repo"`
				} `json:"base"`
			}
			path := fmt.Sprintf(
				"/repos/%s/%s/commits/%s/pulls?per_page=100&page=%d",
				url.PathEscape(owner), url.PathEscape(name), url.PathEscape(item.SHA), page,
			)
			if err := c.requestJSON(ctx, http.MethodGet, path, token, nil, &pulls); err != nil {
				return fmt.Errorf(
					"list associated pull requests for commit %s page %d: %w", item.SHA, page, err,
				)
			}
			for _, associated := range pulls {
				associatedRepository := associated.Base.Repo.FullName
				if associatedRepository == "" {
					associatedRepository = owner + "/" + name
				}
				if associated.Number == focalNumber &&
					strings.EqualFold(associatedRepository, owner+"/"+name) {
					continue
				}
				key := "associated_pull_request\x00" + associated.HTMLURL
				if _, exists := seen[key]; exists {
					continue
				}
				if countRelationships(context.Relationships, "associated_pull_request") >= maxAssociatedPullRequests {
					addContextLimit(context, ContextLimit{
						Scope: "associated_pull_requests", Reason: "local_limit",
						Collected: maxAssociatedPullRequests,
					})
					return nil
				}
				seen[key] = struct{}{}
				context.Relationships = append(context.Relationships, Relationship{
					Kind: "associated_pull_request",
					SourceID: fmt.Sprintf(
						"commit:%s:%s#%d", item.SHA, associatedRepository, associated.Number,
					),
					SourceRepository: associatedRepository, SourceNumber: associated.Number,
					Title: associated.Title, State: associated.State, URL: associated.HTMLURL,
					Timestamp: associated.UpdatedAt,
				})
			}
			if len(pulls) < 100 {
				break
			}
			if countRelationships(context.Relationships, "associated_pull_request") >= maxAssociatedPullRequests {
				addContextLimit(context, ContextLimit{
					Scope: "associated_pull_requests", Reason: "local_limit",
					Collected: maxAssociatedPullRequests,
				})
				return nil
			}
		}
	}
	return nil
}

func (c *Client) buildContext(
	repository string,
	number int,
	body string,
	updatedAt time.Time,
	reviews []review,
	comments []reviewComment,
	commits []commit,
) PullRequestContext {
	const (
		maxReviews       = 200
		maxReviewThreads = 200
		maxCommits       = 100
	)
	context := PullRequestContext{Completeness: "complete", CollectedAt: c.now().UTC()}
	c.extractTextContext(
		&context, repository, "pull_request", fmt.Sprintf("%s#%d", repository, number),
		body, updatedAt,
	)
	for _, item := range reviews[:min(len(reviews), maxReviews)] {
		sourceID := strconv.FormatInt(item.ID, 10)
		timestamp := item.SubmittedAt
		if timestamp.IsZero() {
			timestamp = updatedAt
		}
		reviewURL := item.HTMLURL
		if reviewURL == "" {
			reviewURL = fmt.Sprintf(
				"https://github.com/%s/pull/%d#pullrequestreview-%d", repository, number, item.ID,
			)
		}
		context.Relationships = append(context.Relationships, Relationship{
			Kind: "review", SourceID: sourceID, SourceRepository: repository,
			SourceNumber: number, Title: item.State, Text: item.Body,
			URL: reviewURL, Timestamp: timestamp,
		})
		c.extractTextContext(&context, repository, "review", sourceID, item.Body, timestamp)
	}
	if len(reviews) > maxReviews {
		addContextLimit(&context, ContextLimit{
			Scope: "reviews", Reason: "local_limit", Collected: maxReviews, Total: len(reviews),
		})
	}
	for _, item := range comments[:min(len(comments), maxReviewThreads)] {
		sourceID := strconv.FormatInt(item.ID, 10)
		timestamp := item.UpdatedAt
		if timestamp.IsZero() {
			timestamp = item.CreatedAt
		}
		if timestamp.IsZero() {
			timestamp = updatedAt
		}
		commentURL := item.HTMLURL
		if commentURL == "" {
			commentURL = fmt.Sprintf(
				"https://github.com/%s/pull/%d#discussion_r%d", repository, number, item.ID,
			)
		}
		threadTitle := item.Path
		if summary := strings.TrimSpace(strings.SplitN(item.Body, "\n", 2)[0]); summary != "" {
			summary = truncateRunes(summary, 160)
			if threadTitle != "" {
				threadTitle += ": "
			}
			threadTitle += summary
		}
		context.Relationships = append(context.Relationships, Relationship{
			Kind: "review_thread", SourceID: sourceID, SourceRepository: repository,
			SourceNumber: number, Title: threadTitle, Text: item.Body,
			URL: commentURL, Timestamp: timestamp,
		})
		c.extractTextContext(&context, repository, "review_thread", sourceID, item.Body, timestamp)
	}
	if len(comments) > maxReviewThreads {
		addContextLimit(&context, ContextLimit{
			Scope: "review_threads", Reason: "local_limit",
			Collected: maxReviewThreads, Total: len(comments),
		})
	}
	for _, item := range commits[:min(len(commits), maxCommits)] {
		title, _, _ := strings.Cut(item.Commit.Message, "\n")
		timestamp := item.Commit.Author.Date
		if timestamp.IsZero() {
			timestamp = updatedAt
		}
		commitURL := item.HTMLURL
		if commitURL == "" {
			commitURL = fmt.Sprintf("https://github.com/%s/commit/%s", repository, item.SHA)
		}
		context.Relationships = append(context.Relationships, Relationship{
			Kind: "commit", SourceID: item.SHA, SourceRepository: repository,
			SourceNumber: number, Title: title, Text: item.Commit.Message,
			URL: commitURL, Timestamp: timestamp,
		})
		c.extractTextContext(
			&context, repository, "commit", item.SHA, item.Commit.Message, timestamp,
		)
	}
	if len(commits) > maxCommits {
		addContextLimit(&context, ContextLimit{
			Scope: "commits", Reason: "local_limit", Collected: maxCommits, Total: len(commits),
		})
	}
	return context
}

var (
	referencePattern = regexp.MustCompile(
		`(?i)(?:(close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+)?(?:([a-z0-9_.-]+/[a-z0-9_.-]+))?#([0-9]+)`,
	)
	urlPattern = regexp.MustCompile(`https?://[^\s<>"']+`)
)

func (c *Client) extractTextContext(
	context *PullRequestContext,
	defaultRepository, sourceKind, sourceID, text string,
	timestamp time.Time,
) {
	for index, match := range referencePattern.FindAllStringSubmatch(text, -1) {
		repository := match[2]
		if repository == "" {
			repository = defaultRepository
		}
		number, _ := strconv.Atoi(match[3])
		kind := "cross_reference"
		if match[1] != "" {
			kind = "closing_issue"
		}
		context.Relationships = append(context.Relationships, Relationship{
			Kind: kind, SourceID: fmt.Sprintf("%s:reference:%d", sourceID, index),
			SourceRepository: repository, SourceNumber: number,
			URL:       fmt.Sprintf("https://github.com/%s/issues/%d", repository, number),
			Timestamp: timestamp,
		})
	}
	for _, rawURL := range urlPattern.FindAllString(text, -1) {
		rawURL = strings.TrimRight(rawURL, ".,;:!?)]}")
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Host == "" {
			continue
		}
		if strings.EqualFold(parsed.Hostname(), "github.com") {
			c.extractGitHubURL(context, sourceID, parsed, timestamp)
			continue
		}
		context.ExternalURLs = append(context.ExternalURLs, ExternalURL{
			URL: rawURL, SourceKind: sourceKind, SourceID: sourceID,
			DiscoveredAt: c.now().UTC(),
		})
	}
}

func (*Client) extractGitHubURL(
	context *PullRequestContext, sourceID string, parsed *url.URL, timestamp time.Time,
) {
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number < 1 {
		return
	}
	kind := "cross_reference"
	if parts[2] == "pull" {
		kind = "associated_pull_request"
	}
	context.Relationships = append(context.Relationships, Relationship{
		Kind: kind, SourceID: sourceID + ":url:" + strings.Join(parts, "/"),
		SourceRepository: parts[0] + "/" + parts[1], SourceNumber: number,
		URL: parsed.String(), Timestamp: timestamp,
	})
}

func addContextLimit(context *PullRequestContext, limit ContextLimit) {
	context.Completeness = "partial"
	context.Limits = append(context.Limits, limit)
}

func countRelationships(relationships []Relationship, kind string) int {
	count := 0
	for _, relationship := range relationships {
		if relationship.Kind == kind {
			count++
		}
	}
	return count
}

func deduplicateContext(context PullRequestContext) PullRequestContext {
	seenRelationships := make(map[string]struct{}, len(context.Relationships))
	relationships := make([]Relationship, 0, len(context.Relationships))
	for _, relationship := range context.Relationships {
		key := relationship.Kind + "\x00" + relationship.SourceID
		if _, exists := seenRelationships[key]; exists {
			continue
		}
		seenRelationships[key] = struct{}{}
		relationships = append(relationships, relationship)
	}
	context.Relationships = relationships
	seenURLs := make(map[string]struct{}, len(context.ExternalURLs))
	externalURLs := make([]ExternalURL, 0, len(context.ExternalURLs))
	for _, external := range context.ExternalURLs {
		key := strings.Join([]string{external.URL, external.SourceKind, external.SourceID}, "\x00")
		if _, exists := seenURLs[key]; exists {
			continue
		}
		seenURLs[key] = struct{}{}
		externalURLs = append(externalURLs, external)
	}
	context.ExternalURLs = externalURLs
	return context
}

// maxBodyBytes bounds the retained PR description used by deterministic
// quality rules and disclosure metadata.
const maxBodyBytes = 64 << 10

func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	truncated := value[:limit]
	for truncated != "" && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-3]) + "..."
}

func reviewState(draft, requested bool, reviews []review) string {
	if draft {
		return "draft"
	}
	latest := make(map[string]string)
	for _, review := range reviews {
		if review.User.Login == "" {
			continue
		}
		state := strings.ToUpper(review.State)
		switch state {
		case "APPROVED", "CHANGES_REQUESTED":
			latest[review.User.Login] = state
		case "DISMISSED":
			delete(latest, review.User.Login)
		}
	}
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return "changes_requested"
		}
	}
	if requested {
		return "review_requested"
	}
	for _, state := range latest {
		if state == "APPROVED" {
			return "approved"
		}
	}
	if len(latest) > 0 {
		return "reviewed"
	}
	return "none"
}

func (c *Client) accessToken(ctx context.Context, repositories []string) (string, error) {
	if c.profileType == ProfileFineGrainedPAT {
		return c.staticToken, nil
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	sortedRepositories := append([]string(nil), repositories...)
	sort.Strings(sortedRepositories)
	cacheKey := tokenCacheKey(sortedRepositories)
	cached := c.tokens[cacheKey]
	if cached.value != "" && c.now().Add(5*time.Minute).Before(cached.expiresAt) {
		return cached.value, nil
	}
	jwt, err := c.signedJWT()
	if err != nil {
		return "", err
	}
	var response struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	permissions := map[string]string{"pull_requests": "read", "checks": "read"}
	body := map[string]any{"permissions": permissions}
	if len(sortedRepositories) > 0 {
		names := make([]string, 0, len(sortedRepositories))
		for _, repository := range sortedRepositories {
			parts := strings.Split(repository, "/")
			if len(parts) != 2 {
				return "", errors.New("repository must use owner/name")
			}
			names = append(names, parts[1])
		}
		body["repositories"] = names
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", c.installationID)
	if err := c.requestJSON(ctx, http.MethodPost, path, jwt, body, &response); err != nil {
		return "", err
	}
	if response.Token == "" || response.ExpiresAt.IsZero() {
		return "", errors.New("GitHub returned an invalid installation token")
	}
	if level := response.Permissions["pull_requests"]; level != "" && level != "read" {
		return "", fmt.Errorf("GitHub returned unexpected pull_requests permission %q", level)
	}
	if level := response.Permissions["checks"]; level != "" && level != "read" {
		return "", fmt.Errorf("GitHub returned unexpected checks permission %q", level)
	}
	c.tokens[cacheKey] = cachedToken{value: response.Token, expiresAt: response.ExpiresAt}
	return response.Token, nil
}

func tokenCacheKey(repositories []string) string {
	sortedRepositories := append([]string(nil), repositories...)
	sort.Strings(sortedRepositories)
	return strings.Join(sortedRepositories, "\x00")
}

func (c *Client) requestJSON(
	ctx context.Context,
	method, requestPath, credential string,
	requestBody, responseBody any,
) error {
	var encoded []byte
	if requestBody != nil {
		var err error
		encoded, err = json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode GitHub request: %w", err)
		}
	}
	status, body, metadata, err := c.doRequest(ctx, method, requestPath, credential, encoded)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		err := apiError(status, metadata)
		switch status {
		case http.StatusUnauthorized:
			if c.profileType == ProfileGitHubApp &&
				credential != "" && !strings.HasPrefix(requestPath, "/app") {
				return newRefreshableRouteFailure(FailureCredentialInvalid, err)
			}
			return NewRouteFailure(FailureCredentialInvalid, err)
		case http.StatusForbidden:
			return NewRouteFailure(FailurePermissionMissing, err)
		case http.StatusNotFound:
			if strings.HasPrefix(requestPath, "/app/installations/") {
				return NewRouteFailure(FailureInstallationMissing, err)
			}
			if isRepositoryPullList(requestPath) {
				return NewRouteFailure(FailureRepositoryNotGranted, err)
			}
			return err
		default:
			return err
		}
	}
	if responseBody == nil {
		return nil
	}
	if err := json.Unmarshal(body, responseBody); err != nil {
		return fmt.Errorf("decode GitHub API response: %w", err)
	}
	return nil
}

func isRepositoryPullList(requestPath string) bool {
	requestPath = strings.SplitN(requestPath, "?", 2)[0]
	parts := strings.Split(strings.Trim(requestPath, "/"), "/")
	return len(parts) == 4 && parts[0] == "repos" && parts[3] == "pulls"
}

type rateLimitDeadlineKey struct{}

func apiError(status int, metadata requestFailureMetadata) *APIError {
	return &APIError{
		StatusCode: status, Method: metadata.method, Endpoint: metadata.endpoint,
		RequestID: metadata.requestID, RetryAfter: metadata.retryAfter,
		RateLimitResetUnix: metadata.rateLimitResetUnix,
	}
}

func rateLimitError(
	reason string, retries int, budgetExceeded bool, metadata requestFailureMetadata,
) *RateLimitError {
	return &RateLimitError{
		Reason: reason, Retries: retries, BudgetExceeded: budgetExceeded,
		Method: metadata.method, Endpoint: metadata.endpoint, RequestID: metadata.requestID,
		RetryAfter: metadata.retryAfter, RateLimitResetUnix: metadata.rateLimitResetUnix,
		StatusCode: metadata.statusCode,
	}
}

func isTimeoutError(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &timeout) && timeout.Timeout()
}

func (c *Client) doRequest(
	ctx context.Context,
	method, requestPath, credential string,
	requestBody []byte,
) (int, []byte, requestFailureMetadata, error) {
	target, err := c.baseURL.Parse(requestPath)
	if err != nil {
		return 0, nil, requestFailureMetadata{}, fmt.Errorf("build GitHub API URL: %w", err)
	}
	baseMetadata := boundedRequestMetadata(method, requestPath, nil, c.now().UTC())
	deadline, exists := ctx.Value(rateLimitDeadlineKey{}).(time.Time)
	if !exists {
		deadline = c.now().UTC().Add(c.rateLimitBudget)
	}
	var lastRateLimitReason string
	for retry := 0; ; retry++ {
		request, err := http.NewRequestWithContext(
			ctx, method, target.String(), bytes.NewReader(requestBody),
		)
		if err != nil {
			return 0, nil, baseMetadata, fmt.Errorf("create GitHub request: %w", err)
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		if credential != "" {
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		request.Header.Set("X-GitHub-Api-Version", apiVersion)
		if requestBody != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := c.httpClient.Do(request)
		if err != nil {
			if ctxErr := contextError(ctx, err); ctxErr != nil {
				return 0, nil, baseMetadata, ctxErr
			}
			return 0, nil, baseMetadata, &TransportError{
				Method: method, Endpoint: baseMetadata.endpoint, TimedOut: isTimeoutError(err),
			}
		}
		metadata := boundedRequestMetadata(method, requestPath, response.Header, c.now().UTC())
		metadata.statusCode = response.StatusCode
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		closeErr := response.Body.Close()
		if readErr != nil {
			if ctxErr := contextError(ctx, readErr); ctxErr != nil {
				return 0, nil, metadata, ctxErr
			}
			return 0, nil, metadata, &TransportError{
				Method: method, Endpoint: metadata.endpoint, TimedOut: isTimeoutError(readErr),
			}
		}
		if closeErr != nil {
			return 0, nil, metadata, &TransportError{Method: method, Endpoint: metadata.endpoint}
		}
		c.observeRateLimit(response.Header)
		reason, wait, limited := c.rateLimitWait(
			response.StatusCode, response.Header, body, retry,
		)
		if !limited {
			if lastRateLimitReason != "" {
				c.observeRetry(RetryEvent{
					State: "recovered", Reason: lastRateLimitReason, Retry: retry,
					ObservedAt: c.now().UTC(),
				})
			}
			return response.StatusCode, body, metadata, nil
		}
		lastRateLimitReason = reason
		if retry >= c.rateLimitMaxRetries {
			err := rateLimitError(reason, retry, false, metadata)
			c.observeRetry(RetryEvent{
				State: "failed", Reason: reason, Retry: retry, ObservedAt: c.now().UTC(),
			})
			return 0, nil, metadata, err
		}
		if c.now().UTC().Add(wait).After(deadline) {
			err := rateLimitError(reason, retry, true, metadata)
			c.observeRetry(RetryEvent{
				State: "failed", Reason: reason, Wait: wait, Retry: retry,
				ObservedAt: c.now().UTC(),
			})
			return 0, nil, metadata, err
		}
		c.observeRetry(RetryEvent{
			State: "waiting", Reason: reason, Wait: wait, Retry: retry + 1,
			ObservedAt: c.now().UTC(),
		})
		if err := c.sleep(ctx, wait); err != nil {
			return 0, nil, metadata, err
		}
	}
}

func (c *Client) rateLimitWait(
	status int, header http.Header, body []byte, retry int,
) (string, time.Duration, bool) {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return "", 0, false
	}
	if remaining, err := strconv.Atoi(header.Get("X-RateLimit-Remaining")); err == nil &&
		remaining == 0 {
		if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			wait := max(time.Unix(reset, 0).Sub(c.now().UTC()), 0)
			return "primary", wait, true
		}
	}
	if wait, ok := parseRetryAfter(header.Get("Retry-After"), c.now().UTC()); ok {
		return "secondary", wait, true
	}
	message := strings.ToLower(string(body))
	if status == http.StatusTooManyRequests || strings.Contains(message, "secondary rate limit") {
		wait := time.Minute * time.Duration(1<<min(retry, 5))
		return "secondary", wait, true
	}
	return "", 0, false
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		wait := max(at.Sub(now), 0)
		return wait, true
	}
	return 0, false
}

func (c *Client) observeRetry(event RetryEvent) {
	if c.retryObserver != nil {
		c.retryObserver(event)
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) observeRateLimit(header http.Header) {
	if c.rateLimitObserver == nil {
		return
	}
	remaining, remainingErr := strconv.Atoi(header.Get("X-RateLimit-Remaining"))
	limit, limitErr := strconv.Atoi(header.Get("X-RateLimit-Limit"))
	resetUnix, resetErr := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64)
	if remainingErr != nil || limitErr != nil || resetErr != nil || remaining < 0 || limit <= 0 {
		return
	}
	c.rateLimitObserver(
		remaining, limit, time.Unix(resetUnix, 0).UTC(), c.now().UTC(),
	)
}

func (c *Client) signedJWT() (string, error) {
	now := c.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(c.appID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("encode GitHub App JWT: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := header + "." + encodedPayload
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parsePrivateKey(body []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("PEM block not found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("unsupported RSA private key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return key, nil
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func deduplicateInts(values []int) []int {
	seen := make(map[int]struct{}, len(values))
	result := make([]int, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
