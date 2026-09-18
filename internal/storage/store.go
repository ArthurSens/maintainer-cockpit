// Package storage persists Maintainer Cockpit data in SQLite.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	// Register the SQLite database driver.
	_ "modernc.org/sqlite"

	"github.com/ArthurSens/maintainer-cockpit/internal/analysis"
	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/contribution"
	"github.com/ArthurSens/maintainer-cockpit/internal/quality"
	listquery "github.com/ArthurSens/maintainer-cockpit/internal/query"
)

var (
	// ErrCollectionNotFound reports a request for an unknown collection.
	ErrCollectionNotFound = errors.New("collection not found")
	// ErrRepositoryNotConfigured reports work canceled by configuration removal.
	ErrRepositoryNotConfigured = errors.New("repository not configured")
	// ErrAnalysisSuperseded reports work for a pull-request head that is no longer current.
	ErrAnalysisSuperseded = errors.New("analysis job superseded")
)

const (
	RefreshNever      = "never"
	RefreshFresh      = "fresh"
	RefreshStale      = "stale"
	RefreshRunning    = "running"
	RefreshFailed     = "failed"
	RefreshIncomplete = "incomplete"
	refreshStaleAfter = 2 * time.Hour
)

const (
	RouteOperationDiscovery    = "discovery"
	RouteOperationHydration    = "hydration"
	RouteOperationProgress     = "progress"
	RouteOperationContribution = "contribution_evidence"
	RouteOperationDiff         = "pull_request_diff"

	RouteProfileGitHubApp      = "github_app"
	RouteProfileFineGrainedPAT = "fine_grained_pat"

	RouteOutcomeSelected = "selected"
	RouteOutcomeAdvanced = "advanced"
	RouteOutcomeFailed   = "failed"

	RouteFailureRouteMissing            = "route_missing"
	RouteFailureRouteAmbiguous          = "route_ambiguous"
	RouteFailureInstallationMissing     = "installation_missing"
	RouteFailureInstallationSuspended   = "installation_suspended"
	RouteFailureRepositoryNotGranted    = "repository_not_granted"
	RouteFailurePermissionMissing       = "permission_missing"
	RouteFailureCredentialInvalid       = "credential_invalid"
	RouteFailurePublicCapabilityMissing = "public_capability_missing"
	RouteFailurePrimaryRateLimited      = "primary_rate_limited"
	RouteFailureSecondaryRateLimited    = "secondary_rate_limited"
	RouteFailureGitHubUnavailable       = "github_unavailable"
	RouteFailureUnknown                 = "unknown"

	RouteWarningFallback = "fallback"
	RouteWarningPending  = "pending"
	RouteWarningFailed   = "failed"
)

// Store owns the Maintainer Cockpit SQLite database.
type Store struct {
	db *sql.DB
}

// CollectionSummary is the persisted public collection metadata.
type CollectionSummary struct {
	ID                    string         `json:"id"`
	Name                  string         `json:"name"`
	Description           string         `json:"description,omitempty"`
	Repositories          []string       `json:"repositories"`
	OpenPRs               int            `json:"openPRs"`
	RefreshState          string         `json:"refreshState"`
	LastSuccessfulRefresh *time.Time     `json:"lastSuccessfulRefresh,omitempty"`
	LastRefreshAttempt    *time.Time     `json:"lastRefreshAttempt,omitempty"`
	RefreshError          string         `json:"refreshError,omitempty"`
	Completeness          []ContextLimit `json:"completeness,omitempty"`
	RouteWarnings         []RouteWarning `json:"routeWarnings,omitempty"`
}

// RouteWarning is the deliberately sanitized public routing projection.
type RouteWarning struct {
	Repository            string     `json:"repository"`
	Operation             string     `json:"operation"`
	Status                string     `json:"status"`
	Freshness             string     `json:"freshness"`
	LastAttempt           *time.Time `json:"lastAttempt,omitempty"`
	LastSuccessfulRefresh *time.Time `json:"lastSuccessfulRefresh,omitempty"`
}

// RouteAttempt is one bounded administrator-visible current route attempt.
type RouteAttempt struct {
	Profile         string    `json:"profile"`
	Priority        int       `json:"priority"`
	ProfileType     string    `json:"profileType"`
	Outcome         string    `json:"outcome"`
	FailureCategory string    `json:"failureCategory,omitempty"`
	Detail          string    `json:"detail,omitempty"`
	Remediation     string    `json:"remediation,omitempty"`
	AttemptedAt     time.Time `json:"attemptedAt"`
	Selected        bool      `json:"selected"`
}

// RouteEntry is the complete current state for one repository operation.
type RouteEntry struct {
	Repository      string         `json:"repository"`
	Operation       string         `json:"operation"`
	Pending         bool           `json:"pending"`
	SelectedProfile string         `json:"selectedProfile,omitempty"`
	LastAttempt     *time.Time     `json:"lastAttempt,omitempty"`
	Attempts        []RouteAttempt `json:"attempts"`
}

// PullRequest is the baseline public PR read model.
type PullRequest struct {
	Repository              string                    `json:"repository"`
	Number                  int                       `json:"number"`
	HeadSHA                 string                    `json:"-"`
	Title                   string                    `json:"title"`
	Author                  string                    `json:"author"`
	AuthorID                int64                     `json:"authorID,omitempty"`
	Contribution            *contribution.Evaluation  `json:"contribution,omitempty"`
	Additions               int                       `json:"additions"`
	Deletions               int                       `json:"deletions"`
	ChangedFiles            int                       `json:"changedFiles"`
	Quality                 QualitySummary            `json:"quality"`
	Analysis                AnalysisSummary           `json:"analysis"`
	CreatedAt               time.Time                 `json:"createdAt"`
	UpdatedAt               time.Time                 `json:"updatedAt"`
	URL                     string                    `json:"url"`
	ReviewState             string                    `json:"reviewState"`
	Readiness               PullRequestReadiness      `json:"-"`
	Personal                *PersonalPullRequestState `json:"personal,omitempty"`
	Body                    string                    `json:"-"`
	CollectionIDs           []string                  `json:"-"`
	Context                 *PullRequestContext       `json:"-"`
	Files                   []PullRequestFile         `json:"-"`
	AuthorHistory           *AuthorHistory            `json:"-"`
	InputFingerprintVersion string                    `json:"-"`
	InputFingerprint        string                    `json:"-"`
	CachePayload            []byte                    `json:"-"`
}

// PullRequestReadiness contains independently presented review-readiness facts.
type PullRequestReadiness struct {
	RequestedReviewers []string     `json:"requestedReviewers"`
	RequestedTeams     []string     `json:"requestedTeams"`
	Mergeable          *bool        `json:"mergeable"`
	MergeableState     string       `json:"mergeableState"`
	Checks             CheckSummary `json:"checks"`
	CollectedAt        time.Time    `json:"collectedAt"`
}

type CheckSummary struct {
	Total int        `json:"total"`
	Runs  []CheckRun `json:"runs"`
}

type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
}

// QualitySummary is the compact collection-specific quality read model.
type QualitySummary struct {
	Level    string `json:"level"`
	Findings int    `json:"findings"`
}

// AnalysisSummary is the compact public model-analysis state. Generated
// summary prose remains omitted until collection authorization exists.
type AnalysisSummary struct {
	Status              string                        `json:"status"`
	AnalysisStatus      string                        `json:"analysisStatus"`
	ReviewCognitiveLoad *analysis.ReviewCognitiveLoad `json:"reviewCognitiveLoad,omitempty"`
	WaitingOn           []analysis.WaitingState       `json:"waitingOn"`
	Provider            string                        `json:"provider,omitempty"`
	FallbackFrom        string                        `json:"fallbackFrom,omitempty"`
	Model               string                        `json:"model,omitempty"`
	AnalyzedAt          *time.Time                    `json:"analyzedAt,omitempty"`
	InputRevision       string                        `json:"inputRevision,omitempty"`
	PayloadID           string                        `json:"retainedPayloadID,omitempty"`
	Error               string                        `json:"error,omitempty"`
	DiffCompleteness    string                        `json:"diffCompleteness,omitempty"`
	DiffTruncated       bool                          `json:"diffTruncated,omitempty"`
}

// RepositoryContribution is factual history in one configured repository.
type RepositoryContribution = contribution.RepositoryHistory

// AuthorHistory is collected factual author evidence attached to a snapshot.
type AuthorHistory struct {
	ID               int64
	Login            string
	Complete         bool
	AccountCreatedAt time.Time
	Repositories     map[string]RepositoryContribution
	RecentActivity   []contribution.RepositoryActivity
	CollectedAt      time.Time
}

// AuthorContext is the public collection-specific author read model.
type AuthorContext struct {
	ID               int64                   `json:"id"`
	Login            string                  `json:"login"`
	AccountCreatedAt *time.Time              `json:"accountCreatedAt,omitempty"`
	CollectedAt      time.Time               `json:"collectedAt"`
	Contribution     contribution.Evaluation `json:"contribution"`
}

// PullRequestFile persists provider churn.
type PullRequestFile struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// PullRequestContext is bounded, one-hop evidence collected for a PR.
type PullRequestContext struct {
	Completeness  string         `json:"completeness"`
	Limits        []ContextLimit `json:"limits"`
	CollectedAt   time.Time      `json:"collectedAt"`
	Relationships []Relationship `json:"relationships"`
	ExternalURLs  []ExternalURL  `json:"externalURLs"`
}

// ContextLimit explains why a relationship section is incomplete.
type ContextLimit struct {
	Repository string `json:"repository,omitempty"`
	Scope      string `json:"scope"`
	Reason     string `json:"reason"`
	Collected  int    `json:"collected"`
	Total      int    `json:"total,omitempty"`
}

// Relationship is one GitHub-native one-hop relationship.
type Relationship struct {
	Kind             string    `json:"kind"`
	SourceID         string    `json:"sourceID"`
	SourceRepository string    `json:"sourceRepository,omitempty"`
	SourceNumber     int       `json:"sourceNumber,omitempty"`
	Title            string    `json:"title,omitempty"`
	Text             string    `json:"text,omitempty"`
	State            string    `json:"state,omitempty"`
	URL              string    `json:"url"`
	Timestamp        time.Time `json:"timestamp"`
}

// ExternalURL is untrusted metadata extracted from GitHub-controlled text.
type ExternalURL struct {
	URL          string    `json:"url"`
	SourceKind   string    `json:"sourceKind"`
	SourceID     string    `json:"sourceID"`
	DiscoveredAt time.Time `json:"discoveredAt"`
}

// PullRequestDetail combines a public PR with its collected one-hop context.
type PullRequestDetail struct {
	PullRequest `json:"pullRequest"`
	Context     PullRequestContext        `json:"context"`
	Files       []PullRequestFile         `json:"files"`
	Quality     quality.Assessment        `json:"quality"`
	Analysis    AnalysisDetail            `json:"analysis"`
	Groups      []CorrelationGroupSummary `json:"groups"`
	Readiness   PullRequestReadiness      `json:"readiness"`
}

// AnalysisDetail exposes validated attributes and provenance.
type AnalysisDetail struct {
	AnalysisSummary
	QualityEvaluations []analysis.QualityEvaluation `json:"qualityEvaluations"`
	SchemaVersion      string                       `json:"schemaVersion,omitempty"`
	PromptVersion      string                       `json:"promptVersion,omitempty"`
}

// PullRequestPage is one filtered, sorted cursor page.
type PullRequestPage struct {
	PullRequests []PullRequest
	Total        int
	Matched      int
	NextCursor   string
}

// RefreshJob is one durable, coalesced repository refresh request.
type RefreshJob struct {
	ID           int64
	Repository   string
	ScheduledFor time.Time
	Forced       bool
}

// ContributionEvidenceJob is durable work for one author and repository.
type ContributionEvidenceJob struct {
	ID           int64
	AuthorID     int64
	Login        string
	Repository   string
	Association  string
	ScheduledFor time.Time
}

// AncillaryAuthorJob is durable unauthenticated public author work.
type AncillaryAuthorJob struct {
	ID           int64
	AuthorID     int64
	Login        string
	ScheduledFor time.Time
}

// EvidenceAuthor identifies one open-PR author after a core snapshot commit.
type EvidenceAuthor struct {
	ID          int64
	Login       string
	Association string
}

// RepositoryRefresh is durable scheduling and health state for one repository.
type RepositoryRefresh struct {
	Repository             string     `json:"repository"`
	State                  string     `json:"state"`
	LastAttempt            *time.Time `json:"lastAttemptAt,omitempty"`
	LastSuccessful         *time.Time `json:"lastSuccessfulAt,omitempty"`
	LastFullReconciliation *time.Time `json:"lastFullReconciliationAt,omitempty"`
	LastCacheHits          int        `json:"lastCacheHits"`
	LastCacheMisses        int        `json:"lastCacheMisses"`
	LastCacheBypasses      int        `json:"lastCacheBypasses"`
	LastForced             bool       `json:"lastForced"`
}

// CachedPullRequest is opaque persisted cache state consumed by the refresh
// coordinator.
type CachedPullRequest struct {
	Number             int
	FingerprintVersion string
	Fingerprint        string
	Payload            []byte
}

// RepositorySnapshotMetadata describes how one complete snapshot was built.
type RepositorySnapshotMetadata struct {
	FullReconciliation bool
	Forced             bool
	CacheHits          int
	CacheMisses        int
	CacheBypasses      int
}

// Open opens or creates a SQLite database and initializes its schema.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path must not be empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	if err := s.validatePreInitializeCompatibility(ctx); err != nil {
		return err
	}
	const schema = `
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS collections (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	description TEXT NOT NULL,
	quality_policy_json TEXT NOT NULL DEFAULT '',
	contribution_policy_json TEXT NOT NULL DEFAULT '',
	contribution_refresh_seconds INTEGER NOT NULL DEFAULT 604800,
	model_provider TEXT NOT NULL DEFAULT '',
	model_fallback_provider TEXT NOT NULL DEFAULT '',
	correlation_enabled INTEGER NOT NULL DEFAULT 0 CHECK (correlation_enabled IN (0, 1)),
	active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
	removed_at TEXT
);

CREATE TABLE IF NOT EXISTS collection_repositories (
	collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
	repository TEXT NOT NULL,
	discovery_mode TEXT NOT NULL DEFAULT 'all_open',
	search_query TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (collection_id, repository)
);

CREATE TABLE IF NOT EXISTS pull_requests (
	repository TEXT NOT NULL,
	number INTEGER NOT NULL CHECK (number > 0),
	head_sha TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL,
	additions INTEGER NOT NULL CHECK (additions >= 0),
	deletions INTEGER NOT NULL CHECK (deletions >= 0),
	updated_at TEXT NOT NULL,
	url TEXT NOT NULL,
	author TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z',
	review_state TEXT NOT NULL DEFAULT 'none',
	body TEXT NOT NULL DEFAULT '',
	changed_files INTEGER NOT NULL DEFAULT 0 CHECK (changed_files >= 0),
	author_id INTEGER NOT NULL DEFAULT 0,
	readiness_json TEXT NOT NULL DEFAULT '{}',
	input_fingerprint_version TEXT NOT NULL DEFAULT '',
	input_fingerprint TEXT NOT NULL DEFAULT '',
	reuse_payload BLOB NOT NULL DEFAULT X'',
	PRIMARY KEY (repository, number)
);

CREATE TABLE IF NOT EXISTS collection_pull_requests (
	collection_id TEXT NOT NULL,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	PRIMARY KEY (collection_id, repository, number),
	FOREIGN KEY (collection_id, repository)
		REFERENCES collection_repositories(collection_id, repository)
		ON DELETE CASCADE,
	FOREIGN KEY (repository, number)
		REFERENCES pull_requests(repository, number)
		ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS repository_refreshes (
	repository TEXT PRIMARY KEY,
	state TEXT NOT NULL DEFAULT 'never',
	last_attempt_at TEXT,
	last_success_at TEXT,
	error TEXT NOT NULL DEFAULT '',
	last_full_reconciliation_at TEXT,
	last_reused INTEGER NOT NULL DEFAULT 0 CHECK (last_reused >= 0),
	last_hydrated INTEGER NOT NULL DEFAULT 0 CHECK (last_hydrated >= 0),
	last_forced INTEGER NOT NULL DEFAULT 0 CHECK (last_forced IN (0, 1))
);

CREATE TABLE IF NOT EXISTS refresh_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repository TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed')),
	scheduled_for TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT,
	forced INTEGER NOT NULL DEFAULT 0 CHECK (forced IN (0, 1))
);

CREATE UNIQUE INDEX IF NOT EXISTS refresh_jobs_one_active
	ON refresh_jobs(repository)
	WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS refresh_generations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	refresh_job_id INTEGER NOT NULL UNIQUE REFERENCES refresh_jobs(id) ON DELETE CASCADE,
	repository TEXT NOT NULL,
	phase TEXT NOT NULL CHECK (phase IN ('hydrating', 'progress', 'publishing')),
	forced INTEGER NOT NULL CHECK (forced IN (0, 1)),
	discovered_at TEXT NOT NULL,
	progress_since TEXT,
	limits_json TEXT NOT NULL DEFAULT '[]',
	progress_payload BLOB NOT NULL DEFAULT X'',
	rediscovery_requested INTEGER NOT NULL DEFAULT 0 CHECK (rediscovery_requested IN (0, 1)),
	retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
	failure_category TEXT NOT NULL DEFAULT '',
	failure_detail TEXT NOT NULL DEFAULT '',
	failure_remediation TEXT NOT NULL DEFAULT '',
	failure_phase TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS refresh_generation_pull_requests (
	generation_id INTEGER NOT NULL REFERENCES refresh_generations(id) ON DELETE CASCADE,
	number INTEGER NOT NULL CHECK (number > 0),
	collection_ids_json TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('pending', 'completed')),
	payload BLOB NOT NULL DEFAULT X'',
	fingerprint_version TEXT NOT NULL DEFAULT '',
	fingerprint TEXT NOT NULL DEFAULT '',
	cache_outcome TEXT NOT NULL DEFAULT '' CHECK (cache_outcome IN ('', 'hit', 'miss', 'bypass')),
	PRIMARY KEY (generation_id, number)
);

CREATE TABLE IF NOT EXISTS github_route_operations (
	repository TEXT NOT NULL REFERENCES repository_refreshes(repository) ON DELETE CASCADE,
	operation TEXT NOT NULL CHECK (operation IN (
		'discovery', 'hydration', 'progress',
		'contribution_evidence', 'pull_request_diff'
	)),
	pending INTEGER NOT NULL DEFAULT 0 CHECK (pending IN (0, 1)),
	last_attempt_at TEXT,
	PRIMARY KEY (repository, operation)
);

CREATE TABLE IF NOT EXISTS github_route_attempts (
	repository TEXT NOT NULL,
	operation TEXT NOT NULL,
	priority INTEGER NOT NULL CHECK (priority >= 0),
	profile_name TEXT NOT NULL,
	profile_type TEXT NOT NULL CHECK (profile_type IN ('github_app', 'fine_grained_pat')),
	outcome TEXT NOT NULL CHECK (outcome IN ('selected', 'advanced', 'failed')),
	failure_category TEXT NOT NULL CHECK (failure_category IN (
		'', 'route_missing', 'route_ambiguous', 'installation_missing',
		'installation_suspended', 'repository_not_granted', 'permission_missing',
		'credential_invalid', 'public_capability_missing', 'primary_rate_limited',
		'secondary_rate_limited', 'github_unavailable', 'unknown'
	)),
	detail TEXT NOT NULL,
	remediation TEXT NOT NULL,
	attempted_at TEXT NOT NULL,
	selected INTEGER NOT NULL CHECK (selected IN (0, 1)),
	PRIMARY KEY (repository, operation, priority),
	FOREIGN KEY (repository, operation)
		REFERENCES github_route_operations(repository, operation) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS pull_request_contexts (
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	completeness TEXT NOT NULL,
	limits_json TEXT NOT NULL,
	collected_at TEXT NOT NULL,
	PRIMARY KEY (repository, number),
	FOREIGN KEY (repository, number) REFERENCES pull_requests(repository, number) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS pull_request_diff_evidence (
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	head_sha TEXT NOT NULL,
	completeness TEXT NOT NULL CHECK (completeness IN ('complete', 'partial', 'unavailable')),
	original_files INTEGER NOT NULL CHECK (original_files >= 0),
	sent_files INTEGER NOT NULL CHECK (sent_files >= 0),
	omitted_files INTEGER NOT NULL CHECK (omitted_files >= 0),
	original_bytes INTEGER NOT NULL CHECK (original_bytes >= 0),
	sent_bytes INTEGER NOT NULL CHECK (sent_bytes >= 0),
	truncated INTEGER NOT NULL CHECK (truncated IN (0, 1)),
	collected_at TEXT NOT NULL,
	PRIMARY KEY (repository, number, head_sha),
	FOREIGN KEY (repository, number) REFERENCES pull_requests(repository, number) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS pull_request_diff_sources (
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	head_sha TEXT NOT NULL,
	path TEXT NOT NULL,
	source_id TEXT NOT NULL,
	patch TEXT NOT NULL,
	original_bytes INTEGER NOT NULL CHECK (original_bytes >= 0),
	sent_bytes INTEGER NOT NULL CHECK (sent_bytes >= 0),
	truncated INTEGER NOT NULL CHECK (truncated IN (0, 1)),
	PRIMARY KEY (repository, number, head_sha, path),
	FOREIGN KEY (repository, number, head_sha)
		REFERENCES pull_request_diff_evidence(repository, number, head_sha) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS pull_request_diff_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	head_sha TEXT NOT NULL,
	changed_files INTEGER NOT NULL CHECK (changed_files >= 0),
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed')),
	scheduled_for TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT,
	FOREIGN KEY (repository, number) REFERENCES pull_requests(repository, number) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS pull_request_diff_jobs_active
	ON pull_request_diff_jobs(repository, number, head_sha)
	WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS pull_request_relationships (
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	kind TEXT NOT NULL,
	source_id TEXT NOT NULL,
	source_repository TEXT NOT NULL,
	source_number INTEGER NOT NULL,
	title TEXT NOT NULL,
	state TEXT NOT NULL,
	url TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	text TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (repository, number, kind, source_id),
	FOREIGN KEY (repository, number) REFERENCES pull_requests(repository, number) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS pull_request_external_urls (
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	url TEXT NOT NULL,
	source_kind TEXT NOT NULL,
	source_id TEXT NOT NULL,
	discovered_at TEXT NOT NULL,
	PRIMARY KEY (repository, number, url, source_kind, source_id),
	FOREIGN KEY (repository, number) REFERENCES pull_requests(repository, number) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS repository_context_limits (
	repository TEXT PRIMARY KEY,
	limits_json TEXT NOT NULL,
	observed_at TEXT NOT NULL,
	FOREIGN KEY (repository) REFERENCES repository_refreshes(repository) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS pull_request_files (
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	path TEXT NOT NULL,
	additions INTEGER NOT NULL CHECK (additions >= 0),
	deletions INTEGER NOT NULL CHECK (deletions >= 0),
	PRIMARY KEY (repository, number, path),
	FOREIGN KEY (repository, number) REFERENCES pull_requests(repository, number) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS pull_request_quality (
	collection_id TEXT NOT NULL,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	level TEXT NOT NULL CHECK (level IN ('no_concerns', 'review_suggested', 'strong_concerns')),
	findings INTEGER NOT NULL CHECK (findings >= 0),
	assessment_json TEXT NOT NULL,
	assessed_at TEXT NOT NULL,
	PRIMARY KEY (collection_id, repository, number),
	FOREIGN KEY (collection_id, repository, number)
		REFERENCES collection_pull_requests(collection_id, repository, number)
		ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS authors (
	id INTEGER PRIMARY KEY CHECK (id > 0),
	login TEXT NOT NULL COLLATE NOCASE UNIQUE,
	account_created_at TEXT NOT NULL,
	history_complete INTEGER NOT NULL CHECK (history_complete IN (0, 1)),
	recent_activity_json TEXT NOT NULL,
	collected_at TEXT NOT NULL,
	ancillary_status TEXT NOT NULL DEFAULT 'pending'
		CHECK (ancillary_status IN ('pending', 'complete', 'failed')),
	ancillary_collected_at TEXT
);

CREATE TABLE IF NOT EXISTS author_repository_contributions (
	author_id INTEGER NOT NULL REFERENCES authors(id) ON DELETE CASCADE,
	repository TEXT NOT NULL,
	association TEXT NOT NULL,
	merged INTEGER NOT NULL CHECK (merged >= 0),
	closed_unmerged INTEGER NOT NULL CHECK (closed_unmerged >= 0),
	open INTEGER NOT NULL CHECK (open >= 0),
	status TEXT NOT NULL DEFAULT 'complete'
		CHECK (status IN ('pending', 'complete', 'failed')),
	collected_at TEXT,
	PRIMARY KEY (author_id, repository)
);

CREATE TABLE IF NOT EXISTS contribution_evidence_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	author_id INTEGER NOT NULL,
	login TEXT NOT NULL,
	repository TEXT NOT NULL,
	association TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed')),
	scheduled_for TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS contribution_evidence_jobs_active
	ON contribution_evidence_jobs(author_id, repository)
	WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS ancillary_author_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	author_id INTEGER NOT NULL,
	login TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed')),
	scheduled_for TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS ancillary_author_jobs_active
	ON ancillary_author_jobs(author_id)
	WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS analysis_results (
	collection_id TEXT NOT NULL,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('complete', 'unknown', 'failed')),
	input_revision TEXT NOT NULL,
	provider TEXT NOT NULL,
	fallback_from TEXT NOT NULL,
	model TEXT NOT NULL,
	analyzed_at TEXT,
	payload_id TEXT NOT NULL,
	result_json TEXT NOT NULL,
	quality_level TEXT NOT NULL CHECK (quality_level IN ('no_concerns', 'review_suggested', 'strong_concerns')),
	error TEXT NOT NULL,
	schema_version TEXT NOT NULL,
	prompt_version TEXT NOT NULL,
	PRIMARY KEY (collection_id, repository, number),
	FOREIGN KEY (collection_id, repository, number)
		REFERENCES collection_pull_requests(collection_id, repository, number)
		ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS analysis_attempt_state (
	collection_id TEXT NOT NULL,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('complete', 'partial', 'unknown', 'failed')),
	input_revision TEXT NOT NULL,
	provider TEXT NOT NULL,
	fallback_from TEXT NOT NULL,
	model TEXT NOT NULL,
	attempted_at TEXT NOT NULL,
	payload_id TEXT NOT NULL,
	error TEXT NOT NULL,
	schema_version TEXT NOT NULL,
	prompt_version TEXT NOT NULL,
	PRIMARY KEY (collection_id, repository, number),
	FOREIGN KEY (collection_id, repository, number)
		REFERENCES collection_pull_requests(collection_id, repository, number)
		ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS retained_model_payloads (
	id TEXT PRIMARY KEY,
	request_json TEXT NOT NULL,
	response_json TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS analysis_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	collection_id TEXT NOT NULL,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	head_sha TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed')),
	forced INTEGER NOT NULL CHECK (forced IN (0, 1)),
	scheduled_for TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT,
	FOREIGN KEY (collection_id, repository, number)
		REFERENCES collection_pull_requests(collection_id, repository, number)
		ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS analysis_jobs_one_active
	ON analysis_jobs(collection_id, repository, number)
	WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS correlation_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed')),
	forced INTEGER NOT NULL CHECK (forced IN (0, 1)),
	scheduled_for TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS correlation_jobs_one_active
	ON correlation_jobs(collection_id)
	WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS schedule_dispatches (
	operation TEXT NOT NULL,
	target TEXT NOT NULL,
	last_dispatched_at TEXT NOT NULL,
	PRIMARY KEY (operation, target)
);

CREATE TABLE IF NOT EXISTS correlation_state (
	collection_id TEXT PRIMARY KEY REFERENCES collections(id) ON DELETE CASCADE,
	status TEXT NOT NULL CHECK (status IN ('never', 'complete', 'failed', 'unknown')),
	last_attempt_at TEXT,
	last_success_at TEXT,
	error TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	input_revision TEXT NOT NULL DEFAULT '',
	schema_version TEXT NOT NULL DEFAULT '',
	prompt_version TEXT NOT NULL DEFAULT '',
	payload_id TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS correlation_groups (
	collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
	id TEXT NOT NULL,
	name TEXT NOT NULL,
	description TEXT NOT NULL,
	confidence TEXT NOT NULL CHECK (confidence IN ('medium', 'high')),
	source_ids_json TEXT NOT NULL,
	group_json TEXT NOT NULL,
	text_graph TEXT NOT NULL,
	provider TEXT NOT NULL,
	fallback_from TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL,
	input_revision TEXT NOT NULL,
	schema_version TEXT NOT NULL,
	prompt_version TEXT NOT NULL,
	correlated_at TEXT NOT NULL,
	payload_id TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (collection_id, id)
);

CREATE TABLE IF NOT EXISTS correlation_group_members (
	collection_id TEXT NOT NULL,
	group_id TEXT NOT NULL,
	source_id TEXT NOT NULL,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL CHECK (number > 0),
	state TEXT NOT NULL,
	PRIMARY KEY (collection_id, group_id, source_id),
	FOREIGN KEY (collection_id, group_id)
		REFERENCES correlation_groups(collection_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS correlation_members_by_pull_request
	ON correlation_group_members(collection_id, repository, number);

CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL CHECK (user_id > 0),
	login TEXT NOT NULL,
	avatar_url TEXT NOT NULL DEFAULT '',
	organizations_json TEXT NOT NULL,
	teams_json TEXT NOT NULL,
	csrf_token TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS sessions_by_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS sessions_by_expiry ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS auth_flows (
	id TEXT PRIMARY KEY,
	state TEXT NOT NULL,
	code_verifier TEXT NOT NULL,
	expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	occurred_at TEXT NOT NULL,
	event TEXT NOT NULL,
	user_id INTEGER,
	detail TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS important_pull_requests (
	user_id INTEGER NOT NULL CHECK (user_id > 0),
	collection_id TEXT NOT NULL,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	PRIMARY KEY (user_id, collection_id, repository, number),
	FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE,
	FOREIGN KEY (repository, number)
		REFERENCES pull_requests(repository, number) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS important_pull_requests_by_owner
	ON important_pull_requests(user_id, collection_id);

CREATE TABLE IF NOT EXISTS goal_preferences (
	user_id INTEGER NOT NULL CHECK (user_id > 0),
	collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
	target INTEGER NOT NULL CHECK (target BETWEEN 1 AND 1000),
	enabled_activities_json TEXT NOT NULL,
	timezone TEXT NOT NULL,
	PRIMARY KEY (user_id, collection_id)
);

CREATE TABLE IF NOT EXISTS progress_events (
	collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL CHECK (number > 0),
	event_id TEXT NOT NULL,
	actor_id INTEGER NOT NULL CHECK (actor_id > 0),
	activity_type TEXT NOT NULL CHECK (activity_type IN ('review', 'comment', 'thread_resolved', 'merge', 'close')),
	title TEXT NOT NULL,
	pull_request_url TEXT NOT NULL,
	activity_url TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	collected_at TEXT NOT NULL,
	PRIMARY KEY (collection_id, repository, number, event_id, activity_type)
);

CREATE INDEX IF NOT EXISTS progress_events_by_user_day
	ON progress_events(actor_id, collection_id, occurred_at);

CREATE TABLE IF NOT EXISTS progress_review_threads (
	collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL CHECK (number > 0),
	thread_id TEXT NOT NULL,
	resolved INTEGER NOT NULL CHECK (resolved IN (0, 1)),
	resolved_by INTEGER NOT NULL,
	title TEXT NOT NULL,
	pull_request_url TEXT NOT NULL,
	activity_url TEXT NOT NULL,
	observed_at TEXT NOT NULL,
	PRIMARY KEY (collection_id, repository, number, thread_id)
);

CREATE TABLE IF NOT EXISTS repository_progress_refreshes (
	repository TEXT PRIMARY KEY REFERENCES repository_refreshes(repository) ON DELETE CASCADE,
	last_collected_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS hidden_pull_requests (
	user_id INTEGER NOT NULL CHECK (user_id > 0),
	collection_id TEXT NOT NULL,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL CHECK (number > 0),
	kind TEXT NOT NULL CHECK (kind IN ('snoozed', 'ignored')),
	reason TEXT NOT NULL,
	snoozed_until TEXT,
	wake_on_activity INTEGER NOT NULL CHECK (wake_on_activity IN (0, 1)),
	hidden_at_updated_at TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (user_id, collection_id, repository, number),
	FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE,
	FOREIGN KEY (repository, number)
		REFERENCES pull_requests(repository, number) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS hidden_pull_requests_by_owner
	ON hidden_pull_requests(user_id, collection_id, kind);

CREATE INDEX IF NOT EXISTS collections_by_active ON collections(active, id);

CREATE TABLE IF NOT EXISTS github_rate_limit (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	remaining INTEGER NOT NULL CHECK (remaining >= 0),
	limit_value INTEGER NOT NULL CHECK (limit_value >= 0),
	reset_at TEXT NOT NULL,
	observed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS github_retry_status (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	state TEXT NOT NULL CHECK (state IN ('waiting', 'recovered', 'failed')),
	reason TEXT NOT NULL CHECK (reason IN ('primary', 'secondary')),
	wait_seconds INTEGER NOT NULL CHECK (wait_seconds >= 0),
	retry INTEGER NOT NULL CHECK (retry >= 0),
	observed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS retained_analysis_results (
	collection_id TEXT NOT NULL,
	repository TEXT NOT NULL,
	number INTEGER NOT NULL,
	result_json TEXT NOT NULL,
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	analyzed_at TEXT,
	payload_id TEXT NOT NULL,
	closed_at TEXT NOT NULL,
	PRIMARY KEY (collection_id, repository, number)
);

CREATE INDEX IF NOT EXISTS retained_analysis_results_by_expiry
	ON retained_analysis_results(closed_at);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize SQLite database: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "pull_requests", "head_sha", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "analysis_jobs", "head_sha", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "refresh_generations", "rediscovery_requested", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureRouteOperations(ctx, s.db); err != nil {
		return err
	}
	return s.validateCurrentSchema(ctx)
}

func (s *Store) validatePreInitializeCompatibility(ctx context.Context) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'pull_requests'
	`).Scan(&exists); err != nil {
		return fmt.Errorf("inspect SQLite compatibility: %w", err)
	}
	if exists == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(pull_requests)`)
	if err != nil {
		return fmt.Errorf("inspect pull_requests compatibility: %w", err)
	}
	defer rows.Close()
	hasReadiness := false
	removedColumn := ""
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		hasReadiness = hasReadiness || name == "readiness_json"
		switch name {
		case "adjusted_additions", "adjusted_deletions", "size_classification_version",
			"size_classification_completeness", "size_classification_warning":
			removedColumn = name
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasReadiness {
		return errors.New(
			"SQLite database has an incompatible pull_requests table missing readiness_json; " +
				"recreate the database",
		)
	}
	if removedColumn != "" {
		return fmt.Errorf(
			"SQLite database has an incompatible pull_requests table containing removed column %s; "+
				"back up and recreate the database",
			removedColumn,
		)
	}
	return nil
}

func ensureColumn(
	ctx context.Context, db *sql.DB, table, column, definition string,
) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		found = found || name == column
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE `+table+` ADD COLUMN `+column+` `+definition,
	); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func ensureRouteOperations(ctx context.Context, db *sql.DB) error {
	var definition string
	if err := db.QueryRowContext(ctx, `
		SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'github_route_operations'
	`).Scan(&definition); err != nil {
		return fmt.Errorf("inspect GitHub route schema: %w", err)
	}
	if strings.Contains(definition, RouteOperationDiscovery) &&
		!strings.Contains(definition, "core_collection") {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin GitHub route schema upgrade: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE github_route_operations_new (
			repository TEXT NOT NULL REFERENCES repository_refreshes(repository) ON DELETE CASCADE,
			operation TEXT NOT NULL CHECK (operation IN (
				'discovery', 'hydration', 'progress',
				'contribution_evidence', 'pull_request_diff'
			)),
			pending INTEGER NOT NULL DEFAULT 0 CHECK (pending IN (0, 1)),
			last_attempt_at TEXT,
			PRIMARY KEY (repository, operation)
		);
		INSERT INTO github_route_operations_new
			SELECT * FROM github_route_operations WHERE operation != 'core_collection';
		CREATE TABLE github_route_attempts_new (
			repository TEXT NOT NULL,
			operation TEXT NOT NULL,
			priority INTEGER NOT NULL CHECK (priority >= 0),
			profile_name TEXT NOT NULL,
			profile_type TEXT NOT NULL CHECK (profile_type IN ('github_app', 'fine_grained_pat')),
			outcome TEXT NOT NULL CHECK (outcome IN ('selected', 'advanced', 'failed')),
			failure_category TEXT NOT NULL CHECK (failure_category IN (
				'', 'route_missing', 'route_ambiguous', 'installation_missing',
				'installation_suspended', 'repository_not_granted', 'permission_missing',
				'credential_invalid', 'public_capability_missing', 'primary_rate_limited',
				'secondary_rate_limited', 'github_unavailable', 'unknown'
			)),
			detail TEXT NOT NULL,
			remediation TEXT NOT NULL,
			attempted_at TEXT NOT NULL,
			selected INTEGER NOT NULL CHECK (selected IN (0, 1)),
			PRIMARY KEY (repository, operation, priority),
			FOREIGN KEY (repository, operation)
				REFERENCES github_route_operations_new(repository, operation) ON DELETE CASCADE
		);
		INSERT INTO github_route_attempts_new
			SELECT * FROM github_route_attempts WHERE operation != 'core_collection';
		DROP TABLE github_route_attempts;
		DROP TABLE github_route_operations;
		ALTER TABLE github_route_operations_new RENAME TO github_route_operations;
		ALTER TABLE github_route_attempts_new RENAME TO github_route_attempts;
	`); err != nil {
		return fmt.Errorf("upgrade GitHub route schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit GitHub route schema upgrade: %w", err)
	}
	return nil
}

func (s *Store) validateCurrentSchema(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(pull_requests)`)
	if err != nil {
		return fmt.Errorf("inspect pull_requests schema: %w", err)
	}
	defer rows.Close()

	hasReadiness := false
	removedColumn := ""
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("inspect pull_requests columns: %w", err)
		}
		if name == "readiness_json" {
			hasReadiness = true
		}
		switch name {
		case "adjusted_additions", "adjusted_deletions", "size_classification_version",
			"size_classification_completeness", "size_classification_warning":
			removedColumn = name
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect pull_requests columns: %w", err)
	}
	if !hasReadiness {
		return errors.New(
			"SQLite database has an incompatible pull_requests table missing readiness_json; " +
				"recreate the database",
		)
	}
	if removedColumn != "" {
		return fmt.Errorf(
			"SQLite database has an incompatible pull_requests table containing removed column %s; recreate the database",
			removedColumn,
		)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// SyncCollections atomically makes persisted collection configuration match
// the supplied validated configuration.
func (s *Store) SyncCollections(ctx context.Context, collections []config.Collection) error {
	return s.SyncCollectionsAt(ctx, collections, time.Now().UTC())
}

// SyncCollectionsAt is SyncCollections with an explicit clock for retention
// and recovery-boundary tests.
func (s *Store) SyncCollectionsAt(
	ctx context.Context, collections []config.Collection, now time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin collection sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	active := make(map[string]struct{}, len(collections))
	for _, collection := range collections {
		if _, exists := active[collection.ID]; exists {
			return fmt.Errorf("duplicate collection ID %q", collection.ID)
		}
		active[collection.ID] = struct{}{}
		policy := quality.PolicyFromConfig(collection.Quality)
		policyJSON, err := json.Marshal(policy)
		if err != nil {
			return fmt.Errorf("encode quality policy for %q: %w", collection.ID, err)
		}
		contributionPolicyJSON, err := json.Marshal(
			contribution.PolicyFromConfig(collection.Contribution),
		)
		if err != nil {
			return fmt.Errorf("encode contribution policy for %q: %w", collection.ID, err)
		}
		var previousPolicyJSON string
		if err := tx.QueryRowContext(ctx,
			"SELECT quality_policy_json FROM collections WHERE id = ?", collection.ID,
		).Scan(&previousPolicyJSON); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load quality policy for %q: %w", collection.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO collections (
				id, name, description, quality_policy_json, contribution_policy_json,
				contribution_refresh_seconds,
				model_provider, model_fallback_provider,
				correlation_enabled, active, removed_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, NULL)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				description = excluded.description,
				quality_policy_json = excluded.quality_policy_json,
				contribution_policy_json = excluded.contribution_policy_json,
				contribution_refresh_seconds = excluded.contribution_refresh_seconds,
				model_provider = excluded.model_provider,
				model_fallback_provider = excluded.model_fallback_provider,
				correlation_enabled = excluded.correlation_enabled,
				active = 1,
				removed_at = NULL
		`, collection.ID, collection.Name, collection.Description,
			string(policyJSON), string(contributionPolicyJSON),
			contributionRefreshInterval(),
			collection.ModelProvider, collection.ModelFallbackProvider,
			collection.FeatureCorrelation != nil); err != nil {
			return fmt.Errorf("upsert collection %q: %w", collection.ID, err)
		}
		if previousPolicyJSON != string(policyJSON) {
			if err := recomputeCollectionQuality(ctx, tx, collection.ID, policy); err != nil {
				return err
			}
		}
		seenRepositories := make(map[string]struct{}, len(collection.Repositories))
		for _, repository := range collection.Repositories {
			if _, exists := seenRepositories[repository]; exists {
				return fmt.Errorf("collection %q has duplicate repository %q", collection.ID, repository)
			}
			seenRepositories[repository] = struct{}{}
			discovery := discoveryFor(collection, repository)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO collection_repositories (
					collection_id, repository, discovery_mode, search_query
				)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(collection_id, repository) DO UPDATE SET
					discovery_mode = excluded.discovery_mode,
					search_query = excluded.search_query
			`, collection.ID, repository, discovery.Mode, discovery.Query); err != nil {
				return fmt.Errorf("insert repository %q for %q: %w", repository, collection.ID, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO repository_refreshes (repository)
				VALUES (?)
				ON CONFLICT DO NOTHING
			`, repository); err != nil {
				return fmt.Errorf("initialize refresh state for %q: %w", repository, err)
			}
		}
		repositoryRows, err := tx.QueryContext(ctx, `
			SELECT repository
			FROM collection_repositories
			WHERE collection_id = ?
		`, collection.ID)
		if err != nil {
			return fmt.Errorf("list existing repositories for %q: %w", collection.ID, err)
		}
		var removedRepositories []string
		for repositoryRows.Next() {
			var repository string
			if err := repositoryRows.Scan(&repository); err != nil {
				_ = repositoryRows.Close()
				return fmt.Errorf("scan existing repository for %q: %w", collection.ID, err)
			}
			if _, exists := seenRepositories[repository]; !exists {
				removedRepositories = append(removedRepositories, repository)
			}
		}
		if err := repositoryRows.Close(); err != nil {
			return fmt.Errorf("close repository rows for %q: %w", collection.ID, err)
		}
		if err := repositoryRows.Err(); err != nil {
			return fmt.Errorf("iterate existing repositories for %q: %w", collection.ID, err)
		}
		for _, repository := range removedRepositories {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM collection_repositories
				WHERE collection_id = ? AND repository = ?
			`, collection.ID, repository); err != nil {
				return fmt.Errorf("delete repository %q from %q: %w", repository, collection.ID, err)
			}
		}
		if collection.ModelProvider != "" {
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO analysis_jobs (
					collection_id, repository, number, head_sha, status, forced, scheduled_for
				)
				SELECT cpr.collection_id, cpr.repository, cpr.number, pr.head_sha, 'queued', 0, ?
				FROM collection_pull_requests cpr
				JOIN pull_requests pr
				  ON pr.repository = cpr.repository AND pr.number = cpr.number
				LEFT JOIN pull_request_diff_evidence diff
				  ON diff.repository = pr.repository AND diff.number = pr.number
				 AND diff.head_sha = pr.head_sha
				WHERE cpr.collection_id = ?
				  AND (
					pr.head_sha = '' OR
					diff.completeness IN ('complete', 'partial')
				  )
			`, formatTime(now), collection.ID); err != nil {
				return fmt.Errorf("enqueue analyses for %q: %w", collection.ID, err)
			}
		} else if _, err := tx.ExecContext(ctx, `
			UPDATE analysis_jobs
			SET status = 'completed', finished_at = ?
			WHERE collection_id = ? AND status = 'queued'
		`, formatTime(now), collection.ID); err != nil {
			return fmt.Errorf("cancel analyses for disabled collection %q: %w", collection.ID, err)
		}
		if collection.FeatureCorrelation != nil {
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO correlation_state (collection_id, status)
				VALUES (?, 'never')
			`, collection.ID); err != nil {
				return fmt.Errorf("initialize correlation state for %q: %w", collection.ID, err)
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE correlation_jobs SET status = 'completed', finished_at = ?
				WHERE collection_id = ? AND status = 'queued'
			`, formatTime(now), collection.ID); err != nil {
				return fmt.Errorf("cancel correlation for disabled collection %q: %w", collection.ID, err)
			}
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM correlation_groups WHERE collection_id = ?", collection.ID,
			); err != nil {
				return fmt.Errorf("remove disabled correlation groups for %q: %w", collection.ID, err)
			}
		}
	}

	rows, err := tx.QueryContext(ctx, "SELECT id FROM collections WHERE active = 1")
	if err != nil {
		return fmt.Errorf("list persisted collections: %w", err)
	}
	var removed []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan persisted collection: %w", err)
		}
		if _, exists := active[id]; !exists {
			removed = append(removed, id)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close persisted collection rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate persisted collections: %w", err)
	}
	for _, id := range removed {
		if _, err := tx.ExecContext(ctx, `
			UPDATE collections SET active = 0, removed_at = ? WHERE id = ?
		`, formatTime(now), id); err != nil {
			return fmt.Errorf("retain removed collection %q: %w", id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE analysis_jobs
		SET status = 'completed', finished_at = ?
		WHERE status = 'queued'
		  AND collection_id IN (SELECT id FROM collections WHERE active = 0)
	`, formatTime(now)); err != nil {
		return fmt.Errorf("cancel analysis jobs for removed collections: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE correlation_jobs
		SET status = 'completed', finished_at = ?
		WHERE status = 'queued'
		  AND collection_id IN (SELECT id FROM collections WHERE active = 0)
	`, formatTime(now)); err != nil {
		return fmt.Errorf("cancel correlation jobs for removed collections: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE refresh_jobs
		SET status = 'completed', finished_at = ?
		WHERE status = 'queued'
		  AND repository NOT IN (
			SELECT DISTINCT cr.repository
			FROM collection_repositories cr
			JOIN collections c ON c.id = cr.collection_id
			WHERE c.active = 1
		  )
	`, formatTime(now)); err != nil {
		return fmt.Errorf("cancel refresh jobs for removed repositories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM refresh_generations
		WHERE repository NOT IN (
			SELECT DISTINCT cr.repository
			FROM collection_repositories cr
			JOIN collections c ON c.id = cr.collection_id
			WHERE c.active = 1
		)
	`); err != nil {
		return fmt.Errorf("cancel refresh generations for removed repositories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM repository_refreshes
		WHERE repository NOT IN (
			SELECT DISTINCT cr.repository
			FROM collection_repositories cr
			JOIN collections c ON c.id = cr.collection_id
			WHERE c.active = 1
		)
	`); err != nil {
		return fmt.Errorf("delete refresh state for removed repositories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM important_pull_requests
		WHERE NOT EXISTS (
			SELECT 1 FROM collection_pull_requests cpr
			WHERE cpr.collection_id = important_pull_requests.collection_id
			  AND cpr.repository = important_pull_requests.repository
			  AND cpr.number = important_pull_requests.number
		)
	`); err != nil {
		return fmt.Errorf("delete Important state outside configured collections: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM hidden_pull_requests
		WHERE NOT EXISTS (
			SELECT 1 FROM collection_pull_requests cpr
			WHERE cpr.collection_id = hidden_pull_requests.collection_id
			  AND cpr.repository = hidden_pull_requests.repository
			  AND cpr.number = hidden_pull_requests.number
		)
	`); err != nil {
		return fmt.Errorf("delete hidden state outside configured collections: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit collection sync: %w", err)
	}
	return nil
}

// UpsertPullRequest persists a PR and associates it with one collection.
func (s *Store) UpsertPullRequest(ctx context.Context, collectionID string, pr PullRequest) error {
	if err := normalizePullRequest(&pr); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin PR upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := upsertPullRequestRow(ctx, tx, pr); err != nil {
		return err
	}
	if err := replacePullRequestFiles(ctx, tx, pr); err != nil {
		return err
	}
	if pr.AuthorHistory != nil {
		if err := writeAuthorHistory(ctx, tx, *pr.AuthorHistory); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO collection_pull_requests (collection_id, repository, number)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING
	`, collectionID, pr.Repository, pr.Number); err != nil {
		return fmt.Errorf("associate pull request with collection %q: %w", collectionID, err)
	}
	policy, err := loadQualityPolicy(ctx, tx, collectionID)
	if err != nil {
		return err
	}
	if err := writeQualityAssessment(ctx, tx, collectionID, pr, policy, time.Now()); err != nil {
		return err
	}
	if err := enqueueAnalysisForPRTx(ctx, tx, collectionID, pr.Repository, pr.Number, false, time.Now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit PR upsert: %w", err)
	}
	return nil
}

func upsertPullRequestRow(ctx context.Context, tx *sql.Tx, pr PullRequest) error {
	cachePayload := pr.CachePayload
	if cachePayload == nil {
		cachePayload = []byte{}
	}
	readinessJSON, err := json.Marshal(normalizeReadiness(pr.Readiness))
	if err != nil {
		return fmt.Errorf("encode readiness for %s#%d: %w", pr.Repository, pr.Number, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pull_requests (
			repository, number, head_sha, title, author, author_id, additions, deletions, changed_files, body,
			created_at, updated_at, url, review_state,
			readiness_json, input_fingerprint_version, input_fingerprint, reuse_payload
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repository, number) DO UPDATE SET
			head_sha = excluded.head_sha,
			title = excluded.title,
			author = excluded.author,
			author_id = excluded.author_id,
			additions = excluded.additions,
			deletions = excluded.deletions,
			changed_files = excluded.changed_files,
			body = excluded.body,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at,
			url = excluded.url,
			review_state = excluded.review_state,
			readiness_json = excluded.readiness_json,
			input_fingerprint_version = excluded.input_fingerprint_version,
			input_fingerprint = excluded.input_fingerprint,
			reuse_payload = excluded.reuse_payload
	`, pr.Repository, pr.Number, pr.HeadSHA, pr.Title, pr.Author, pr.AuthorID, pr.Additions, pr.Deletions,
		pr.ChangedFiles, pr.Body,
		pr.CreatedAt.UTC().Format(time.RFC3339Nano), pr.UpdatedAt.UTC().Format(time.RFC3339Nano),
		pr.URL, pr.ReviewState, string(readinessJSON), pr.InputFingerprintVersion, pr.InputFingerprint,
		cachePayload); err != nil {
		return fmt.Errorf("upsert pull request %s#%d: %w", pr.Repository, pr.Number, err)
	}
	return nil
}

// ListCollections returns configured collections and their current PR counts.
func (s *Store) ListCollections(ctx context.Context) ([]CollectionSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.name, c.description, COUNT(cpr.number)
		FROM collections c
		LEFT JOIN collection_pull_requests cpr ON cpr.collection_id = c.id
		WHERE c.active = 1
		GROUP BY c.id, c.name, c.description
		ORDER BY c.name, c.id
	`)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer rows.Close()

	var collections []CollectionSummary
	for rows.Next() {
		var collection CollectionSummary
		if err := rows.Scan(&collection.ID, &collection.Name, &collection.Description, &collection.OpenPRs); err != nil {
			return nil, fmt.Errorf("scan collection: %w", err)
		}
		collections = append(collections, collection)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collections: %w", err)
	}
	for index := range collections {
		repositories, err := s.listRepositories(ctx, collections[index].ID)
		if err != nil {
			return nil, err
		}
		collections[index].Repositories = repositories
		if err := s.loadCollectionRefresh(ctx, &collections[index]); err != nil {
			return nil, err
		}
		if err := s.loadCollectionCompleteness(ctx, &collections[index]); err != nil {
			return nil, err
		}
		if err := s.loadCollectionRouteWarnings(ctx, &collections[index]); err != nil {
			return nil, err
		}
	}
	return collections, nil
}

func (s *Store) listRepositories(ctx context.Context, collectionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repository
		FROM collection_repositories
		WHERE collection_id = ?
		ORDER BY repository
	`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("list repositories for %q: %w", collectionID, err)
	}
	defer rows.Close()

	var repositories []string
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			return nil, fmt.Errorf("scan repository for %q: %w", collectionID, err)
		}
		repositories = append(repositories, repository)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repositories for %q: %w", collectionID, err)
	}
	return repositories, nil
}

// ReplaceRouteAttempts atomically replaces the entire current attempt set for
// one repository operation. It deliberately stores no attempt history.
func (s *Store) ReplaceRouteAttempts(
	ctx context.Context, repository, operation string, attempts []RouteAttempt, attemptedAt time.Time,
) error {
	if !validRouteOperation(operation) {
		return fmt.Errorf("invalid route operation %q", operation)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin route attempt replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO github_route_operations (repository, operation, pending, last_attempt_at)
		VALUES (?, ?, 0, ?)
		ON CONFLICT(repository, operation) DO UPDATE SET
			pending = 0, last_attempt_at = excluded.last_attempt_at
	`, repository, operation, formatTime(attemptedAt)); err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return ErrRepositoryNotConfigured
		}
		return fmt.Errorf("upsert route operation: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM github_route_attempts WHERE repository = ? AND operation = ?",
		repository, operation,
	); err != nil {
		return fmt.Errorf("clear current route attempts: %w", err)
	}
	for index, attempt := range attempts {
		if attempt.Priority != index || !validRouteAttempt(attempt) {
			return fmt.Errorf("invalid route attempt at priority %d", index)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO github_route_attempts (
				repository, operation, priority, profile_name, profile_type, outcome,
				failure_category, detail, remediation, attempted_at, selected
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, repository, operation, attempt.Priority, attempt.Profile, attempt.ProfileType,
			attempt.Outcome, attempt.FailureCategory, attempt.Detail, attempt.Remediation,
			formatTime(attemptedAt), attempt.Selected); err != nil {
			return fmt.Errorf("insert route attempt: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit route attempt replacement: %w", err)
	}
	return nil
}

// MarkRouteOperationsPending marks bounded operations as awaiting
// revalidation while preserving snapshots and prior attempts.
func (s *Store) MarkRouteOperationsPending(
	ctx context.Context, repositories []string, at time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin route pending update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, repository := range repositories {
		for _, operation := range []string{
			RouteOperationDiscovery, RouteOperationHydration,
			RouteOperationProgress, RouteOperationContribution, RouteOperationDiff,
		} {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO github_route_operations (repository, operation, pending)
				VALUES (?, ?, 1)
				ON CONFLICT(repository, operation) DO UPDATE SET pending = 1
			`, repository, operation); err != nil {
				if strings.Contains(err.Error(), "FOREIGN KEY") {
					continue
				}
				return fmt.Errorf("mark route pending for %q: %w", repository, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit route pending update: %w", err)
	}
	_ = at // reserved for generation transition diagnostics; attempts retain their own timestamp.
	return nil
}

// ListRouteAttempts returns administrator-only current route diagnostics.
func (s *Store) ListRouteAttempts(ctx context.Context) ([]RouteEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.repository, o.operation, o.pending, o.last_attempt_at,
			a.priority, a.profile_name, a.profile_type, a.outcome,
			a.failure_category, a.detail, a.remediation, a.attempted_at, a.selected
		FROM github_route_operations o
		LEFT JOIN github_route_attempts a
		  ON a.repository = o.repository AND a.operation = o.operation
		ORDER BY o.repository, o.operation, a.priority
	`)
	if err != nil {
		return nil, fmt.Errorf("list route attempts: %w", err)
	}
	defer rows.Close()
	var result []RouteEntry
	for rows.Next() {
		var repository, operation string
		var pending bool
		var lastAttempt, profile, profileType, outcome, category, detail, remediation, attempted sql.NullString
		var priority, selected sql.NullInt64
		if err := rows.Scan(
			&repository, &operation, &pending, &lastAttempt, &priority, &profile,
			&profileType, &outcome, &category, &detail, &remediation, &attempted, &selected,
		); err != nil {
			return nil, fmt.Errorf("scan route attempt: %w", err)
		}
		if len(result) == 0 || result[len(result)-1].Repository != repository ||
			result[len(result)-1].Operation != operation {
			entry := RouteEntry{Repository: repository, Operation: operation, Pending: pending}
			if lastAttempt.Valid {
				value, err := time.Parse(time.RFC3339Nano, lastAttempt.String)
				if err != nil {
					return nil, err
				}
				entry.LastAttempt = &value
			}
			result = append(result, entry)
		}
		if !priority.Valid {
			continue
		}
		attemptTime, err := time.Parse(time.RFC3339Nano, attempted.String)
		if err != nil {
			return nil, err
		}
		result[len(result)-1].Attempts = append(result[len(result)-1].Attempts, RouteAttempt{
			Profile: profile.String, Priority: int(priority.Int64), ProfileType: profileType.String,
			Outcome: outcome.String, FailureCategory: category.String, Detail: detail.String,
			Remediation: remediation.String, AttemptedAt: attemptTime, Selected: selected.Int64 == 1,
		})
		if selected.Int64 == 1 {
			result[len(result)-1].SelectedProfile = profile.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate route attempts: %w", err)
	}
	return result, nil
}

func validRouteOperation(operation string) bool {
	return operation == RouteOperationDiscovery ||
		operation == RouteOperationHydration || operation == RouteOperationProgress ||
		operation == RouteOperationContribution || operation == RouteOperationDiff
}

func validRouteAttempt(attempt RouteAttempt) bool {
	if attempt.Profile == "" || len(attempt.Profile) > 100 ||
		(attempt.ProfileType != RouteProfileGitHubApp && attempt.ProfileType != RouteProfileFineGrainedPAT) ||
		(attempt.Outcome != RouteOutcomeSelected && attempt.Outcome != RouteOutcomeAdvanced &&
			attempt.Outcome != RouteOutcomeFailed) ||
		!validRouteFailure(attempt.FailureCategory) || len(attempt.Detail) > 240 ||
		len(attempt.Remediation) > 240 {
		return false
	}
	return !strings.ContainsAny(attempt.Profile+attempt.Detail+attempt.Remediation, "\r\n\x00")
}

func validRouteFailure(category string) bool {
	switch category {
	case "", RouteFailureRouteMissing, RouteFailureRouteAmbiguous,
		RouteFailureInstallationMissing, RouteFailureInstallationSuspended,
		RouteFailureRepositoryNotGranted, RouteFailurePermissionMissing,
		RouteFailureCredentialInvalid, RouteFailurePublicCapabilityMissing,
		RouteFailurePrimaryRateLimited, RouteFailureSecondaryRateLimited,
		RouteFailureGitHubUnavailable, RouteFailureUnknown:
		return true
	default:
		return false
	}
}

func (s *Store) loadCollectionRouteWarnings(
	ctx context.Context, collection *CollectionSummary,
) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.repository, o.operation, o.pending, o.last_attempt_at,
			rr.state, rr.last_success_at,
			COALESCE(MAX(CASE WHEN a.selected = 1 THEN a.priority END), -1),
			COUNT(a.priority)
		FROM github_route_operations o
		JOIN collection_repositories cr ON cr.repository = o.repository
		JOIN repository_refreshes rr ON rr.repository = o.repository
		LEFT JOIN github_route_attempts a
		  ON a.repository = o.repository AND a.operation = o.operation
		WHERE cr.collection_id = ?
		GROUP BY o.repository, o.operation, o.pending, o.last_attempt_at,
			rr.state, rr.last_success_at
		ORDER BY o.repository, o.operation
	`, collection.ID)
	if err != nil {
		return fmt.Errorf("load collection route warnings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var repository, operation, freshness string
		var pending bool
		var attempted, succeeded sql.NullString
		var selectedPriority, attempts int
		if err := rows.Scan(
			&repository, &operation, &pending, &attempted, &freshness, &succeeded,
			&selectedPriority, &attempts,
		); err != nil {
			return fmt.Errorf("scan collection route warning: %w", err)
		}
		status := ""
		switch {
		case pending:
			status = RouteWarningPending
		case selectedPriority > 0:
			status = RouteWarningFallback
		case attempts > 0 && selectedPriority < 0:
			status = RouteWarningFailed
		}
		if status == "" {
			continue
		}
		warning := RouteWarning{
			Repository: repository, Operation: operation, Status: status, Freshness: freshness,
		}
		if attempted.Valid {
			value, err := time.Parse(time.RFC3339Nano, attempted.String)
			if err != nil {
				return err
			}
			warning.LastAttempt = &value
		}
		if succeeded.Valid {
			value, err := time.Parse(time.RFC3339Nano, succeeded.String)
			if err != nil {
				return err
			}
			warning.LastSuccessfulRefresh = &value
		}
		collection.RouteWarnings = append(collection.RouteWarnings, warning)
	}
	return rows.Err()
}

func (s *Store) loadCollectionRefresh(ctx context.Context, collection *CollectionSummary) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rr.state, rr.last_attempt_at, rr.last_success_at, rr.error
		FROM repository_refreshes rr
		JOIN collection_repositories cr ON cr.repository = rr.repository
		WHERE cr.collection_id = ?
	`, collection.ID)
	if err != nil {
		return fmt.Errorf("load refresh state for %q: %w", collection.ID, err)
	}
	defer rows.Close()

	collection.RefreshState = RefreshNever
	priority := map[string]int{
		RefreshFresh: 0, RefreshNever: 1, RefreshStale: 2, RefreshRunning: 3,
		RefreshIncomplete: 4, RefreshFailed: 5,
	}
	sawNever := false
	sawComplete := false
	hasState := false
	for rows.Next() {
		var state, errorMessage string
		var attempted, succeeded sql.NullString
		if err := rows.Scan(&state, &attempted, &succeeded, &errorMessage); err != nil {
			return fmt.Errorf("scan refresh state for %q: %w", collection.ID, err)
		}
		if !hasState || priority[state] > priority[collection.RefreshState] {
			collection.RefreshState = state
			collection.RefreshError = errorMessage
		}
		hasState = true
		if state == RefreshNever {
			sawNever = true
		} else {
			sawComplete = true
		}
		if attempted.Valid {
			value, err := time.Parse(time.RFC3339Nano, attempted.String)
			if err != nil {
				return fmt.Errorf("parse refresh attempt time: %w", err)
			}
			if collection.LastRefreshAttempt == nil || value.After(*collection.LastRefreshAttempt) {
				collection.LastRefreshAttempt = &value
			}
		}
		if succeeded.Valid {
			value, err := time.Parse(time.RFC3339Nano, succeeded.String)
			if err != nil {
				return fmt.Errorf("parse successful refresh time: %w", err)
			}
			if collection.LastSuccessfulRefresh == nil || value.Before(*collection.LastSuccessfulRefresh) {
				collection.LastSuccessfulRefresh = &value
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if sawNever && sawComplete && priority[collection.RefreshState] < priority[RefreshIncomplete] {
		collection.RefreshState = RefreshIncomplete
		collection.RefreshError = "Some repositories have not completed their first refresh."
	}
	if collection.RefreshState == RefreshFresh &&
		collection.LastSuccessfulRefresh != nil &&
		time.Now().After(collection.LastSuccessfulRefresh.Add(refreshStaleAfter)) {
		collection.RefreshState = RefreshStale
	}
	return nil
}

func (s *Store) loadCollectionCompleteness(
	ctx context.Context, collection *CollectionSummary,
) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT limits.repository, limits.limits_json
		FROM repository_context_limits limits
		JOIN collection_repositories configured ON configured.repository = limits.repository
		WHERE configured.collection_id = ?
		ORDER BY limits.repository
	`, collection.ID)
	if err != nil {
		return fmt.Errorf("load completeness for %q: %w", collection.ID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var repository, encoded string
		if err := rows.Scan(&repository, &encoded); err != nil {
			return fmt.Errorf("scan completeness for %q: %w", collection.ID, err)
		}
		var limits []ContextLimit
		if err := json.Unmarshal([]byte(encoded), &limits); err != nil {
			return fmt.Errorf("decode completeness for %q: %w", collection.ID, err)
		}
		for _, limit := range limits {
			limit.Repository = repository
			collection.Completeness = append(collection.Completeness, limit)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate completeness for %q: %w", collection.ID, err)
	}
	return nil
}

// ListPullRequests returns the current PRs in a collection.
func (s *Store) ListPullRequests(ctx context.Context, collectionID string) ([]PullRequest, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT 1 FROM collections WHERE id = ? AND active = 1", collectionID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCollectionNotFound
		}
		return nil, fmt.Errorf("find collection %q: %w", collectionID, err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+pullRequestColumns+`
		FROM pull_requests pr
		JOIN collection_pull_requests cpr
			ON cpr.repository = pr.repository AND cpr.number = pr.number
		LEFT JOIN pull_request_quality q
			ON q.collection_id = cpr.collection_id AND q.repository = pr.repository AND q.number = pr.number
		LEFT JOIN analysis_results ar
			ON ar.collection_id = cpr.collection_id AND ar.repository = pr.repository AND ar.number = pr.number
		LEFT JOIN analysis_attempt_state aa
			ON aa.collection_id = cpr.collection_id AND aa.repository = pr.repository AND aa.number = pr.number
		WHERE cpr.collection_id = ?
		ORDER BY pr.updated_at DESC, pr.repository, pr.number
	`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("list pull requests for %q: %w", collectionID, err)
	}
	defer rows.Close()

	pullRequests := make([]PullRequest, 0)
	for rows.Next() {
		pr, err := scanPullRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pull request for %q: %w", collectionID, err)
		}
		pullRequests = append(pullRequests, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pull requests for %q: %w", collectionID, err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range pullRequests {
		if err := s.attachContribution(ctx, collectionID, &pullRequests[index]); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(pullRequests, func(i, j int) bool {
		return pullRequests[i].UpdatedAt.After(pullRequests[j].UpdatedAt)
	})
	return pullRequests, nil
}

// ListPullRequestsPage applies public table filters and sorting in SQLite.
func (s *Store) ListPullRequestsPage(
	ctx context.Context,
	collectionID string,
	options listquery.Options,
) (PullRequestPage, error) {
	return s.listPullRequestsPage(ctx, collectionID, options, nil)
}

type personalListQuery struct {
	userID int64
	view   string
}

func (s *Store) listPullRequestsPage(
	ctx context.Context,
	collectionID string,
	options listquery.Options,
	personal *personalListQuery,
) (PullRequestPage, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT 1 FROM collections WHERE id = ? AND active = 1", collectionID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PullRequestPage{}, ErrCollectionNotFound
		}
		return PullRequestPage{}, fmt.Errorf("find collection %q: %w", collectionID, err)
	}

	var personalJoins string
	var joinArgs []any
	var visibilitySQL string
	var visibilityArgs []any
	if personal != nil {
		personalJoins = `
		LEFT JOIN hidden_pull_requests hidden
			ON hidden.user_id = ? AND hidden.collection_id = cpr.collection_id
			AND hidden.repository = pr.repository AND hidden.number = pr.number
		LEFT JOIN important_pull_requests important
			ON important.user_id = ? AND important.collection_id = cpr.collection_id
			AND important.repository = pr.repository AND important.number = pr.number`
		joinArgs = []any{personal.userID, personal.userID}
		switch personal.view {
		case "mine":
			visibilitySQL = "hidden.repository IS NULL AND (pr.author_id = ? OR important.repository IS NOT NULL)"
			visibilityArgs = []any{personal.userID}
		case "hidden":
			visibilitySQL = "hidden.repository IS NOT NULL"
		default:
			visibilitySQL = "hidden.repository IS NULL"
		}
	}

	totalWhere := "cpr.collection_id = ?"
	totalArgs := append(append([]any(nil), joinArgs...), collectionID)
	if visibilitySQL != "" {
		totalWhere += " AND " + visibilitySQL
		totalArgs = append(totalArgs, visibilityArgs...)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM pull_requests pr
		JOIN collection_pull_requests cpr
			ON cpr.repository = pr.repository AND cpr.number = pr.number
		`+personalJoins+`
		WHERE `+totalWhere, totalArgs...).Scan(&total); err != nil {
		return PullRequestPage{}, fmt.Errorf("count pull requests for %q: %w", collectionID, err)
	}

	where := []string{"cpr.collection_id = ?"}
	args := []any{collectionID}
	if visibilitySQL != "" {
		where = append(where, visibilitySQL)
		args = append(args, visibilityArgs...)
	}
	if options.Repository != "" {
		where = append(where, "pr.repository = ?")
		args = append(args, options.Repository)
	}
	if options.Review != "" {
		where = append(where, "pr.review_state = ?")
		args = append(args, options.Review)
	}
	if options.Quality != "" {
		where = append(where, combinedQualitySQL+" = ?")
		args = append(args, options.Quality)
	}
	if options.ReviewLoad != "" {
		where = append(where, reviewLoadSQL+" = ?")
		args = append(args, options.ReviewLoad)
	}
	if options.AnalysisStatus != "" {
		where = append(where, analysisStatusSQL+" = ?")
		args = append(args, options.AnalysisStatus)
	}
	if options.WaitingMode == "any" && len(options.WaitingOn) > 0 {
		placeholders := make([]string, len(options.WaitingOn))
		for index, party := range options.WaitingOn {
			placeholders[index] = "?"
			args = append(args, party)
		}
		where = append(where, `EXISTS (
			SELECT 1 FROM json_each(COALESCE(ar.result_json, '{}'), '$.waitingOn') waiting
			WHERE json_extract(waiting.value, '$.party') IN (`+strings.Join(placeholders, ",")+`)
		)`)
	} else {
		for _, party := range options.WaitingOn {
			where = append(where, `EXISTS (
				SELECT 1 FROM json_each(COALESCE(ar.result_json, '{}'), '$.waitingOn') waiting
				WHERE json_extract(waiting.value, '$.party') = ?
			)`)
			args = append(args, party)
		}
	}
	if options.Query != "" {
		where = append(where, `(
			LOWER(pr.title) LIKE ? OR LOWER(pr.author) LIKE ? OR
			LOWER(pr.repository) LIKE ? OR CAST(pr.number AS TEXT) = ?
		)`)
		term := "%" + strings.ToLower(options.Query) + "%"
		number := strings.TrimPrefix(options.Query, "#")
		args = append(args, term, term, term, number)
	}
	whereSQL := strings.Join(where, " AND ")
	countQuery := `
		SELECT COUNT(*)
		FROM pull_requests pr
		JOIN collection_pull_requests cpr
			ON cpr.repository = pr.repository AND cpr.number = pr.number
		LEFT JOIN pull_request_quality q
			ON q.collection_id = cpr.collection_id AND q.repository = pr.repository AND q.number = pr.number
		LEFT JOIN analysis_results ar
			ON ar.collection_id = cpr.collection_id AND ar.repository = pr.repository AND ar.number = pr.number
		LEFT JOIN analysis_attempt_state aa
			ON aa.collection_id = cpr.collection_id AND aa.repository = pr.repository AND aa.number = pr.number
		` + personalJoins + `
		WHERE ` + whereSQL
	var matched int
	queryArgs := append(append([]any(nil), joinArgs...), args...)
	if err := s.db.QueryRowContext(ctx, countQuery, queryArgs...).Scan(&matched); err != nil {
		return PullRequestPage{}, fmt.Errorf("count matching pull requests: %w", err)
	}

	orderExpressions := map[string]string{
		"updated":    "pr.updated_at",
		"number":     "pr.number",
		"repository": "pr.repository",
		"title":      "LOWER(pr.title)",
		"author":     "LOWER(pr.author)",
		"churn":      "pr.additions + pr.deletions",
		"review":     "pr.review_state",
		"quality": `CASE (` + combinedQualitySQL + `)
			WHEN 'strong_concerns' THEN 2
			WHEN 'review_suggested' THEN 1
			ELSE 0 END`,
		"review_load": `CASE ` + reviewLoadSQL + `
			WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END`,
	}
	var orderBy []string
	if options.Sort != "" {
		sorts := strings.Split(options.Sort, ",")
		orders := strings.Split(options.Order, ",")
		if len(sorts) != len(orders) {
			return PullRequestPage{}, errors.New("sort and order lengths differ")
		}
		for index, sort := range sorts {
			expression := orderExpressions[sort]
			if expression == "" {
				return PullRequestPage{}, fmt.Errorf("unsupported sort %q", sort)
			}
			order := strings.ToUpper(orders[index])
			if order != "ASC" && order != "DESC" {
				return PullRequestPage{}, fmt.Errorf("unsupported order %q", orders[index])
			}
			if sort == "review_load" {
				orderBy = append(orderBy,
					"CASE WHEN "+reviewLoadSQL+" IS NULL THEN 1 ELSE 0 END ASC",
				)
			}
			orderBy = append(orderBy, expression+" "+order)
		}
	}
	orderBy = append(orderBy, "pr.repository ASC", "pr.number ASC")
	query := `
		SELECT ` + pullRequestColumns + `
		FROM pull_requests pr
		JOIN collection_pull_requests cpr
			ON cpr.repository = pr.repository AND cpr.number = pr.number
		LEFT JOIN pull_request_quality q
			ON q.collection_id = cpr.collection_id AND q.repository = pr.repository AND q.number = pr.number
		LEFT JOIN analysis_results ar
			ON ar.collection_id = cpr.collection_id AND ar.repository = pr.repository AND ar.number = pr.number
		LEFT JOIN analysis_attempt_state aa
			ON aa.collection_id = cpr.collection_id AND aa.repository = pr.repository AND aa.number = pr.number
		` + personalJoins + `
		WHERE ` + whereSQL + `
		ORDER BY ` + strings.Join(orderBy, ", ") + `
		LIMIT ? OFFSET ?`
	pageArgs := append(append([]any(nil), queryArgs...), options.Limit+1, options.Offset)
	rows, err := s.db.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return PullRequestPage{}, fmt.Errorf("list pull request page: %w", err)
	}
	defer rows.Close()
	pullRequests := make([]PullRequest, 0, options.Limit+1)
	for rows.Next() {
		pr, err := scanPullRequest(rows)
		if err != nil {
			return PullRequestPage{}, err
		}
		pullRequests = append(pullRequests, pr)
	}
	if err := rows.Err(); err != nil {
		return PullRequestPage{}, fmt.Errorf("iterate pull request page: %w", err)
	}
	if err := rows.Close(); err != nil {
		return PullRequestPage{}, err
	}
	for index := range pullRequests {
		if err := s.attachContribution(ctx, collectionID, &pullRequests[index]); err != nil {
			return PullRequestPage{}, err
		}
	}
	var nextCursor string
	if len(pullRequests) > options.Limit {
		pullRequests = pullRequests[:options.Limit]
		nextCursor, err = options.NextCursor(options.Offset + len(pullRequests))
		if err != nil {
			return PullRequestPage{}, err
		}
	}
	return PullRequestPage{
		PullRequests: pullRequests, Total: total, Matched: matched, NextCursor: nextCursor,
	}, nil
}

type rowScanner interface {
	Scan(...any) error
}

// pullRequestColumns must match the scanPullRequest field order and requires
// pull_requests pr joined with LEFT JOIN pull_request_quality q.
const pullRequestColumns = `
	pr.repository, pr.number, pr.title, pr.author, pr.author_id, pr.additions, pr.deletions,
	pr.changed_files,
	pr.created_at, pr.updated_at, pr.url, pr.review_state,
	pr.readiness_json,
	` + combinedQualitySQL + `,
	COALESCE(q.findings, 0) + COALESCE((
		SELECT COUNT(*)
		FROM json_each(ar.result_json, '$.qualityEvaluations') AS evaluation
		WHERE json_extract(evaluation.value, '$.status') = 'finding'
	), 0),
	COALESCE(aa.status, 'unknown'), COALESCE(aa.input_revision, ''),
	COALESCE(aa.provider, ''), COALESCE(aa.fallback_from, ''), COALESCE(aa.model, ''), aa.attempted_at,
	COALESCE(aa.payload_id, ''), COALESCE(aa.error, ''), COALESCE(ar.result_json, ''),
	` + analysisStatusSQL + `,
	COALESCE((
		SELECT evidence.completeness
		FROM pull_request_diff_evidence evidence
		WHERE evidence.repository = pr.repository AND evidence.number = pr.number
		  AND evidence.head_sha = pr.head_sha
	), ''),
	COALESCE((
		SELECT evidence.truncated
		FROM pull_request_diff_evidence evidence
		WHERE evidence.repository = pr.repository AND evidence.number = pr.number
		  AND evidence.head_sha = pr.head_sha
	), 0)`

const combinedQualitySQL = `CASE
	WHEN COALESCE(q.level, 'no_concerns') = 'strong_concerns'
		OR COALESCE(ar.quality_level, 'no_concerns') = 'strong_concerns'
		THEN 'strong_concerns'
	WHEN COALESCE(q.level, 'no_concerns') = 'review_suggested'
		OR COALESCE(ar.quality_level, 'no_concerns') = 'review_suggested'
		THEN 'review_suggested'
	ELSE 'no_concerns'
END`

const reviewLoadSQL = `CASE
	WHEN json_extract(ar.result_json, '$.reviewCognitiveLoad.overall') IN ('low', 'medium', 'high')
	THEN json_extract(ar.result_json, '$.reviewCognitiveLoad.overall')
	ELSE NULL
END`

const analysisStatusSQL = `CASE
	WHEN EXISTS (
		SELECT 1 FROM pull_request_diff_jobs diff_job
		WHERE diff_job.repository = pr.repository AND diff_job.number = pr.number
		  AND diff_job.head_sha = pr.head_sha AND diff_job.status IN ('queued', 'running')
	) OR EXISTS (
		SELECT 1 FROM analysis_jobs analysis_job
		WHERE analysis_job.collection_id = cpr.collection_id
		  AND analysis_job.repository = pr.repository AND analysis_job.number = pr.number
		  AND analysis_job.status IN ('queued', 'running')
	) THEN CASE WHEN ar.collection_id IS NULL THEN 'pending' ELSE 'stale' END
	WHEN aa.status = 'partial' THEN 'partial'
	WHEN aa.status = 'failed' THEN 'failed'
	WHEN aa.status = 'unknown' THEN 'invalid'
	WHEN ` + reviewLoadSQL + ` IS NOT NULL THEN 'available'
	ELSE 'pending'
END`

func scanPullRequest(row rowScanner) (PullRequest, error) {
	var pr PullRequest
	var createdAt, updatedAt string
	var analyzedAt sql.NullString
	var analysisJSON, readinessJSON string
	if err := row.Scan(
		&pr.Repository, &pr.Number, &pr.Title, &pr.Author, &pr.AuthorID,
		&pr.Additions, &pr.Deletions,
		&pr.ChangedFiles,
		&createdAt, &updatedAt, &pr.URL, &pr.ReviewState,
		&readinessJSON,
		&pr.Quality.Level, &pr.Quality.Findings,
		&pr.Analysis.Status, &pr.Analysis.InputRevision,
		&pr.Analysis.Provider, &pr.Analysis.FallbackFrom, &pr.Analysis.Model, &analyzedAt,
		&pr.Analysis.PayloadID, &pr.Analysis.Error, &analysisJSON,
		&pr.Analysis.AnalysisStatus, &pr.Analysis.DiffCompleteness,
		&pr.Analysis.DiffTruncated,
	); err != nil {
		return PullRequest{}, fmt.Errorf("scan pull request: %w", err)
	}
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return PullRequest{}, fmt.Errorf("parse pull request creation time %q: %w", createdAt, err)
	}
	updated, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return PullRequest{}, fmt.Errorf("parse pull request update time %q: %w", updatedAt, err)
	}
	pr.CreatedAt = created
	pr.UpdatedAt = updated
	if err := json.Unmarshal([]byte(readinessJSON), &pr.Readiness); err != nil {
		return PullRequest{}, fmt.Errorf("decode pull request readiness: %w", err)
	}
	pr.Readiness = normalizeReadiness(pr.Readiness)
	if analyzedAt.Valid {
		value, err := time.Parse(time.RFC3339Nano, analyzedAt.String)
		if err != nil {
			return PullRequest{}, fmt.Errorf("parse analysis time %q: %w", analyzedAt.String, err)
		}
		pr.Analysis.AnalyzedAt = &value
	}
	pr.Analysis.WaitingOn = make([]analysis.WaitingState, 0)
	if analysisJSON != "" {
		var result analysis.Result
		if err := json.Unmarshal([]byte(analysisJSON), &result); err != nil {
			return PullRequest{}, fmt.Errorf("decode analysis result: %w", err)
		}
		pr.Analysis.ReviewCognitiveLoad = result.ReviewCognitiveLoad
		pr.Analysis.WaitingOn = result.WaitingOn
	}
	return pr, nil
}

func normalizeReadiness(readiness PullRequestReadiness) PullRequestReadiness {
	if readiness.RequestedReviewers == nil {
		readiness.RequestedReviewers = make([]string, 0)
	}
	if readiness.RequestedTeams == nil {
		readiness.RequestedTeams = make([]string, 0)
	}
	if readiness.Checks.Runs == nil {
		readiness.Checks.Runs = make([]CheckRun, 0)
	}
	return readiness
}

// ReplaceRepositorySnapshot atomically publishes one complete all-open result.
func (s *Store) ReplaceRepositorySnapshot(
	ctx context.Context,
	repository string,
	pullRequests []PullRequest,
	completedAt time.Time,
) error {
	return s.replaceRepositorySnapshot(
		ctx, repository, pullRequests, completedAt, nil, RepositorySnapshotMetadata{},
	)
}

// ReplaceRepositorySnapshotWithCompleteness also records explicit provider limits.
func (s *Store) ReplaceRepositorySnapshotWithCompleteness(
	ctx context.Context,
	repository string,
	pullRequests []PullRequest,
	completedAt time.Time,
	limits []ContextLimit,
) error {
	return s.replaceRepositorySnapshot(
		ctx, repository, pullRequests, completedAt, limits, RepositorySnapshotMetadata{},
	)
}

// ReplaceRepositorySnapshotWithMetadata atomically publishes a complete
// snapshot and its cache/reconciliation operational status.
func (s *Store) ReplaceRepositorySnapshotWithMetadata(
	ctx context.Context,
	repository string,
	pullRequests []PullRequest,
	completedAt time.Time,
	limits []ContextLimit,
	metadata RepositorySnapshotMetadata,
) error {
	return s.replaceRepositorySnapshot(
		ctx, repository, pullRequests, completedAt, limits, metadata,
	)
}

func (s *Store) replaceRepositorySnapshot(
	ctx context.Context,
	repository string,
	pullRequests []PullRequest,
	completedAt time.Time,
	limits []ContextLimit,
	metadata RepositorySnapshotMetadata,
) error {
	for index := range pullRequests {
		if pullRequests[index].Repository != repository {
			return fmt.Errorf("pull request %d belongs to %q, want %q", pullRequests[index].Number, pullRequests[index].Repository, repository)
		}
		if err := normalizePullRequest(&pullRequests[index]); err != nil {
			return fmt.Errorf("pull request %d: %w", pullRequests[index].Number, err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin repository snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var configured int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM collection_repositories cr
		 JOIN collections c ON c.id = cr.collection_id
		 WHERE cr.repository = ? AND c.active = 1`, repository,
	).Scan(&configured); err != nil {
		return fmt.Errorf("find configured repository %q: %w", repository, err)
	}
	if configured == 0 {
		return fmt.Errorf("%w: %q", ErrRepositoryNotConfigured, repository)
	}
	for _, pr := range pullRequests {
		if err := upsertPullRequestRow(ctx, tx, pr); err != nil {
			return err
		}
		if err := replacePullRequestFiles(ctx, tx, pr); err != nil {
			return err
		}
		if err := replacePullRequestContext(ctx, tx, pr); err != nil {
			return err
		}
		if pr.AuthorHistory != nil {
			if err := writeAuthorHistory(ctx, tx, *pr.AuthorHistory); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE IF NOT EXISTS desired_collection_pull_requests (
			collection_id TEXT NOT NULL,
			repository TEXT NOT NULL,
			number INTEGER NOT NULL,
			PRIMARY KEY (collection_id, repository, number)
		);
		DELETE FROM desired_collection_pull_requests;
	`); err != nil {
		return fmt.Errorf("prepare desired snapshot for %q: %w", repository, err)
	}
	for _, pr := range pullRequests {
		if len(pr.CollectionIDs) == 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO desired_collection_pull_requests (collection_id, repository, number)
				SELECT cr.collection_id, cr.repository, ?
				FROM collection_repositories cr
				JOIN collections c ON c.id = cr.collection_id
				WHERE cr.repository = ? AND c.active = 1
			`, pr.Number, repository); err != nil {
				return fmt.Errorf("publish %s#%d: %w", repository, pr.Number, err)
			}
			continue
		}
		for _, collectionID := range pr.CollectionIDs {
			result, err := tx.ExecContext(ctx, `
				INSERT INTO desired_collection_pull_requests (collection_id, repository, number)
				SELECT cr.collection_id, cr.repository, ?
				FROM collection_repositories cr
				JOIN collections c ON c.id = cr.collection_id
				WHERE cr.collection_id = ? AND cr.repository = ? AND c.active = 1
			`, pr.Number, collectionID, repository)
			if err != nil {
				return fmt.Errorf("publish %s#%d to %q: %w", repository, pr.Number, collectionID, err)
			}
			affected, err := result.RowsAffected()
			if err != nil || affected != 1 {
				return fmt.Errorf("collection %q does not configure repository %q", collectionID, repository)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO retained_analysis_results (
			collection_id, repository, number, result_json,
			provider, model, analyzed_at, payload_id, closed_at
		)
		SELECT ar.collection_id, ar.repository, ar.number, ar.result_json,
			ar.provider, ar.model, ar.analyzed_at, ar.payload_id, ?
		FROM analysis_results ar
		WHERE ar.repository = ?
		  AND NOT EXISTS (
			SELECT 1 FROM desired_collection_pull_requests desired
			WHERE desired.collection_id = ar.collection_id
			  AND desired.repository = ar.repository
			  AND desired.number = ar.number
		  )
		ON CONFLICT(collection_id, repository, number) DO UPDATE SET
			result_json = excluded.result_json,
			provider = excluded.provider,
			model = excluded.model,
			analyzed_at = excluded.analyzed_at,
			payload_id = excluded.payload_id,
			closed_at = excluded.closed_at
	`, formatTime(completedAt), repository); err != nil {
		return fmt.Errorf("retain closed analyses for %q: %w", repository, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM collection_pull_requests
		WHERE repository = ?
		  AND NOT EXISTS (
			SELECT 1 FROM desired_collection_pull_requests desired
			WHERE desired.collection_id = collection_pull_requests.collection_id
			  AND desired.repository = collection_pull_requests.repository
			  AND desired.number = collection_pull_requests.number
		  )
	`, repository); err != nil {
		return fmt.Errorf("remove closed collection pull requests for %q: %w", repository, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO collection_pull_requests (collection_id, repository, number)
		SELECT collection_id, repository, number FROM desired_collection_pull_requests
	`); err != nil {
		return fmt.Errorf("publish desired snapshot for %q: %w", repository, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM desired_collection_pull_requests"); err != nil {
		return fmt.Errorf("clear desired snapshot for %q: %w", repository, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM pull_requests
		WHERE repository = ?
		  AND NOT EXISTS (
			SELECT 1 FROM collection_pull_requests cpr
			WHERE cpr.repository = pull_requests.repository AND cpr.number = pull_requests.number
		  )
	`, repository); err != nil {
		return fmt.Errorf("remove closed pull requests for %q: %w", repository, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM important_pull_requests
		WHERE repository = ?
		  AND NOT EXISTS (
			SELECT 1 FROM collection_pull_requests cpr
			WHERE cpr.collection_id = important_pull_requests.collection_id
			  AND cpr.repository = important_pull_requests.repository
			  AND cpr.number = important_pull_requests.number
		  )
	`, repository); err != nil {
		return fmt.Errorf("remove Important state for closed pull requests in %q: %w", repository, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM hidden_pull_requests
		WHERE repository = ?
		  AND NOT EXISTS (
			SELECT 1 FROM collection_pull_requests cpr
			WHERE cpr.collection_id = hidden_pull_requests.collection_id
			  AND cpr.repository = hidden_pull_requests.repository
			  AND cpr.number = hidden_pull_requests.number
		  )
	`, repository); err != nil {
		return fmt.Errorf("remove hidden state for closed pull requests in %q: %w", repository, err)
	}
	policies, err := loadQualityPolicies(ctx, tx)
	if err != nil {
		return err
	}
	for _, pr := range pullRequests {
		publishedTo, err := publishedCollections(ctx, tx, pr.Repository, pr.Number)
		if err != nil {
			return err
		}
		for _, collectionID := range publishedTo {
			policy, exists := policies[collectionID]
			if !exists {
				policy = quality.DefaultPolicy()
			}
			if err := writeQualityAssessment(ctx, tx, collectionID, pr, policy, completedAt); err != nil {
				return err
			}
		}
		pr.CollectionIDs = publishedTo
		if err := enqueuePullRequestDiffTx(ctx, tx, pr, completedAt); err != nil {
			return err
		}
	}
	var fullReconciliationAt any
	if metadata.FullReconciliation {
		fullReconciliationAt = formatTime(completedAt)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO repository_refreshes (
			repository, state, last_attempt_at, last_success_at, error,
			last_full_reconciliation_at, last_reused, last_hydrated, last_forced
		)
		VALUES (?, ?, ?, ?, '', ?, ?, ?, ?)
		ON CONFLICT(repository) DO UPDATE SET
			state = excluded.state,
			last_attempt_at = excluded.last_attempt_at,
			last_success_at = excluded.last_success_at,
			error = '',
			last_full_reconciliation_at = COALESCE(
				excluded.last_full_reconciliation_at,
				repository_refreshes.last_full_reconciliation_at
			),
			last_reused = excluded.last_reused,
			last_hydrated = excluded.last_hydrated,
			last_forced = excluded.last_forced
	`, repository, RefreshFresh, formatTime(completedAt), formatTime(completedAt),
		fullReconciliationAt, metadata.CacheHits,
		metadata.CacheMisses+metadata.CacheBypasses, metadata.Forced); err != nil {
		return fmt.Errorf("record successful refresh for %q: %w", repository, err)
	}
	if len(limits) == 0 {
		if _, err := tx.ExecContext(
			ctx, "DELETE FROM repository_context_limits WHERE repository = ?", repository,
		); err != nil {
			return fmt.Errorf("clear completeness for %q: %w", repository, err)
		}
	} else {
		encoded, err := json.Marshal(limits)
		if err != nil {
			return fmt.Errorf("encode completeness for %q: %w", repository, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO repository_context_limits (repository, limits_json, observed_at)
			VALUES (?, ?, ?)
			ON CONFLICT(repository) DO UPDATE SET
				limits_json = excluded.limits_json,
				observed_at = excluded.observed_at
		`, repository, string(encoded), formatTime(completedAt)); err != nil {
			return fmt.Errorf("record completeness for %q: %w", repository, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit repository snapshot: %w", err)
	}
	return nil
}

// RecordRefreshResult updates health without changing the last complete PR snapshot.
func (s *Store) RecordRefreshResult(
	ctx context.Context,
	repository, state string,
	attemptedAt time.Time,
	message string,
) error {
	switch state {
	case RefreshStale, RefreshRunning, RefreshFailed, RefreshIncomplete:
	default:
		return fmt.Errorf("invalid refresh state %q", state)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE repository_refreshes
		SET state = ?, last_attempt_at = ?, error = ?
		WHERE repository = ?
	`, state, formatTime(attemptedAt), message, repository); err != nil {
		return fmt.Errorf("record refresh result for %q: %w", repository, err)
	}
	return nil
}

// ListRepositoryRefreshes returns durable scheduler state.
func (s *Store) ListRepositoryRefreshes(ctx context.Context) ([]RepositoryRefresh, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repository, state, last_attempt_at, last_success_at,
			last_full_reconciliation_at, last_reused, last_hydrated, last_forced
		FROM repository_refreshes
		ORDER BY repository
	`)
	if err != nil {
		return nil, fmt.Errorf("list repository refreshes: %w", err)
	}
	defer rows.Close()
	var result []RepositoryRefresh
	for rows.Next() {
		var item RepositoryRefresh
		var attempted, successful, full sql.NullString
		var refreshed int
		if err := rows.Scan(
			&item.Repository, &item.State, &attempted, &successful, &full,
			&item.LastCacheHits, &refreshed, &item.LastForced,
		); err != nil {
			return nil, fmt.Errorf("scan repository refresh: %w", err)
		}
		if item.LastForced {
			item.LastCacheBypasses = refreshed
		} else {
			item.LastCacheMisses = refreshed
		}
		var err error
		if item.LastAttempt, err = parseNullTime(attempted); err != nil {
			return nil, err
		}
		if item.LastSuccessful, err = parseNullTime(successful); err != nil {
			return nil, err
		}
		if item.LastFullReconciliation, err = parseNullTime(full); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// LoadCachedPullRequests returns only complete persisted cache payloads for one
// repository.
func (s *Store) LoadCachedPullRequests(
	ctx context.Context, repository string,
) (map[int]CachedPullRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT number, input_fingerprint_version, input_fingerprint, reuse_payload
		FROM pull_requests
		WHERE repository = ?
		  AND input_fingerprint_version != ''
		  AND input_fingerprint != ''
		  AND length(reuse_payload) > 0
	`, repository)
	if err != nil {
		return nil, fmt.Errorf("load cached pull requests for %q: %w", repository, err)
	}
	defer rows.Close()
	result := make(map[int]CachedPullRequest)
	for rows.Next() {
		var item CachedPullRequest
		if err := rows.Scan(
			&item.Number, &item.FingerprintVersion, &item.Fingerprint, &item.Payload,
		); err != nil {
			return nil, fmt.Errorf("scan cached pull request for %q: %w", repository, err)
		}
		result[item.Number] = item
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cached pull requests for %q: %w", repository, err)
	}
	return result, nil
}

// EnqueueRefresh durably requests a refresh and coalesces active duplicates.
func (s *Store) EnqueueRefresh(ctx context.Context, repository string, scheduledFor time.Time) (bool, error) {
	return s.EnqueueRefreshWithForce(ctx, repository, scheduledFor, false)
}

// EnqueueRefreshWithForce requests refresh work and upgrades a coalesced job
// to a forced full reconciliation.
func (s *Store) EnqueueRefreshWithForce(
	ctx context.Context, repository string, scheduledFor time.Time, forced bool,
) (bool, error) {
	var configured int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM collection_repositories cr
		 JOIN collections c ON c.id = cr.collection_id
		 WHERE cr.repository = ? AND c.active = 1`, repository,
	).Scan(&configured); err != nil {
		return false, fmt.Errorf("find configured repository %q: %w", repository, err)
	}
	if configured == 0 {
		return false, fmt.Errorf("%w: %q", ErrRepositoryNotConfigured, repository)
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO refresh_jobs (repository, status, scheduled_for, forced)
		VALUES (?, 'queued', ?, ?)
	`, repository, formatTime(scheduledFor), forced)
	if err != nil {
		return false, fmt.Errorf("enqueue refresh for %q: %w", repository, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect refresh enqueue: %w", err)
	}
	if forced {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE refresh_jobs SET forced = 1
			WHERE repository = ? AND status IN ('queued', 'running')
		`, repository); err != nil {
			return false, fmt.Errorf("force coalesced refresh for %q: %w", repository, err)
		}
	}
	return affected == 1, nil
}

// ClaimRefresh atomically claims the next due refresh job.
func (s *Store) ClaimRefresh(ctx context.Context, now time.Time) (*RefreshJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin refresh claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var job RefreshJob
	var scheduledFor string
	err = tx.QueryRowContext(ctx, `
		SELECT id, repository, scheduled_for, forced
		FROM refresh_jobs
		WHERE status = 'queued' AND scheduled_for <= ?
		ORDER BY scheduled_for, id
		LIMIT 1
	`, formatTime(now)).Scan(&job.ID, &job.Repository, &scheduledFor, &job.Forced)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select refresh job: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE refresh_jobs
		SET status = 'running', started_at = ?
		WHERE id = ? AND status = 'queued'
	`, formatTime(now), job.ID)
	if err != nil {
		return nil, fmt.Errorf("claim refresh job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return nil, fmt.Errorf("refresh job %d was claimed concurrently", job.ID)
	}
	job.ScheduledFor, err = time.Parse(time.RFC3339Nano, scheduledFor)
	if err != nil {
		return nil, fmt.Errorf("parse scheduled refresh time: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit refresh claim: %w", err)
	}
	return &job, nil
}

// CompleteRefreshJob removes an active job from coalescing while retaining history.
func (s *Store) CompleteRefreshJob(ctx context.Context, id int64, completedAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE refresh_jobs
		SET status = 'completed', finished_at = ?
		WHERE id = ?
	`, formatTime(completedAt), id); err != nil {
		return fmt.Errorf("complete refresh job %d: %w", id, err)
	}
	return nil
}

// EnqueueAuthorEvidenceAfterSnapshot creates bounded, deduplicated evidence
// work for every collection containing sourceRepository.
func (s *Store) EnqueueAuthorEvidenceAfterSnapshot(
	ctx context.Context, sourceRepository string, authors []EvidenceAuthor, now time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin evidence enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT cr.repository, MIN(c.contribution_refresh_seconds)
		FROM collection_repositories source
		JOIN collections c ON c.id = source.collection_id AND c.active = 1
		JOIN collection_repositories cr ON cr.collection_id = source.collection_id
		WHERE source.repository = ?
		GROUP BY cr.repository
		ORDER BY cr.repository
	`, sourceRepository)
	if err != nil {
		return fmt.Errorf("list contribution evidence repositories: %w", err)
	}
	intervals := make(map[string]time.Duration)
	for rows.Next() {
		var repository string
		var seconds int64
		if err := rows.Scan(&repository, &seconds); err != nil {
			_ = rows.Close()
			return err
		}
		intervals[repository] = time.Duration(seconds) * time.Second
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, author := range authors {
		if author.ID <= 0 || author.Login == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO authors (
				id, login, account_created_at, history_complete, recent_activity_json,
				collected_at, ancillary_status
			) VALUES (?, ?, '', 0, '[]', ?, 'pending')
			ON CONFLICT(id) DO UPDATE SET login = excluded.login
		`, author.ID, author.Login, formatTime(now)); err != nil {
			return fmt.Errorf("upsert evidence author %q: %w", author.Login, err)
		}
		for repository, interval := range intervals {
			association := ""
			if repository == sourceRepository {
				association = author.Association
			}
			var existingAssociation string
			if err := tx.QueryRowContext(ctx, `
				SELECT association FROM author_repository_contributions
				WHERE author_id = ? AND repository = ?
			`, author.ID, repository).Scan(&existingAssociation); err == nil &&
				contributionAssociationRank(existingAssociation) >
					contributionAssociationRank(association) {
				association = existingAssociation
			}
			var collected sql.NullString
			_ = tx.QueryRowContext(ctx, `
				SELECT collected_at FROM author_repository_contributions
				WHERE author_id = ? AND repository = ? AND status = 'complete'
			`, author.ID, repository).Scan(&collected)
			if collected.Valid {
				at, parseErr := time.Parse(time.RFC3339Nano, collected.String)
				if parseErr == nil && now.Before(at.Add(interval)) {
					continue
				}
			}
			result, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO contribution_evidence_jobs (
					author_id, login, repository, association, status, scheduled_for
				) VALUES (?, ?, ?, ?, 'queued', ?)
			`, author.ID, author.Login, repository, association, formatTime(now))
			if err != nil {
				return fmt.Errorf("enqueue repository contribution evidence: %w", err)
			}
			if affected, _ := result.RowsAffected(); affected == 1 {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO author_repository_contributions (
						author_id, repository, association, merged, closed_unmerged, open,
						status, collected_at
					) VALUES (?, ?, ?, 0, 0, 0, 'pending', NULL)
					ON CONFLICT(author_id, repository) DO UPDATE SET
						association = excluded.association, status = 'pending'
				`, author.ID, repository, association); err != nil {
					return err
				}
			}
		}
		var ancillaryCollected sql.NullString
		_ = tx.QueryRowContext(ctx,
			`SELECT ancillary_collected_at FROM authors
			 WHERE id = ? AND ancillary_status = 'complete'`, author.ID,
		).Scan(&ancillaryCollected)
		ancillaryFresh := false
		if ancillaryCollected.Valid {
			at, parseErr := time.Parse(time.RFC3339Nano, ancillaryCollected.String)
			ancillaryFresh = parseErr == nil && now.Before(at.Add(minimumInterval(intervals)))
		}
		if !ancillaryFresh {
			result, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO ancillary_author_jobs (
					author_id, login, status, scheduled_for
				) VALUES (?, ?, 'queued', ?)
			`, author.ID, author.Login, formatTime(now))
			if err != nil {
				return err
			}
			if affected, _ := result.RowsAffected(); affected == 1 {
				if _, err := tx.ExecContext(ctx,
					`UPDATE authors SET ancillary_status = 'pending' WHERE id = ?`, author.ID,
				); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

func minimumInterval(values map[string]time.Duration) time.Duration {
	result := 7 * 24 * time.Hour
	for _, value := range values {
		if value > 0 && value < result {
			result = value
		}
	}
	return result
}

func contributionAssociationRank(value string) int {
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

// ClaimContributionEvidence atomically claims one due repository evidence job.
func (s *Store) ClaimContributionEvidence(
	ctx context.Context, now time.Time,
) (*ContributionEvidenceJob, error) {
	var job ContributionEvidenceJob
	var scheduled string
	err := s.claimEvidenceJob(ctx, "contribution_evidence_jobs", now,
		`id, author_id, login, repository, association, scheduled_for`, &job.ID,
		func(row *sql.Row) error {
			return row.Scan(&job.ID, &job.AuthorID, &job.Login, &job.Repository, &job.Association, &scheduled)
		})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job.ScheduledFor, err = time.Parse(time.RFC3339Nano, scheduled)
	return &job, err
}

// ClaimAncillaryAuthorEvidence atomically claims one due ancillary job.
func (s *Store) ClaimAncillaryAuthorEvidence(
	ctx context.Context, now time.Time,
) (*AncillaryAuthorJob, error) {
	var job AncillaryAuthorJob
	var scheduled string
	err := s.claimEvidenceJob(ctx, "ancillary_author_jobs", now,
		`id, author_id, login, scheduled_for`, &job.ID, func(row *sql.Row) error {
			return row.Scan(&job.ID, &job.AuthorID, &job.Login, &scheduled)
		})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job.ScheduledFor, err = time.Parse(time.RFC3339Nano, scheduled)
	return &job, err
}

func (s *Store) claimEvidenceJob(
	ctx context.Context, table string, now time.Time, columns string,
	id *int64, scan func(*sql.Row) error,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, `SELECT `+columns+` FROM `+table+
		` WHERE status = 'queued' AND scheduled_for <= ? ORDER BY scheduled_for, id LIMIT 1`,
		formatTime(now))
	if err := scan(row); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+table+
		` SET status = 'running', started_at = ? WHERE id = ? AND status = 'queued'`,
		formatTime(now), *id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect evidence job claim: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("evidence job %d was claimed concurrently", *id)
	}
	return tx.Commit()
}

// CompleteContributionEvidence persists successful facts or marks only their
// completeness failed while retaining prior counts.
func (s *Store) CompleteContributionEvidence(
	ctx context.Context, job *ContributionEvidenceJob,
	history RepositoryContribution, collectedAt time.Time, collectErr error,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if collectErr == nil {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO author_repository_contributions (
				author_id, repository, association, merged, closed_unmerged, open,
				status, collected_at
			) VALUES (?, ?, ?, ?, ?, ?, 'complete', ?)
			ON CONFLICT(author_id, repository) DO UPDATE SET
				association = excluded.association, merged = excluded.merged,
				closed_unmerged = excluded.closed_unmerged, open = excluded.open,
				status = 'complete', collected_at = excluded.collected_at
		`, job.AuthorID, job.Repository, history.Association, history.Merged,
			history.ClosedUnmerged, history.Open, formatTime(collectedAt))
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE author_repository_contributions SET status = 'failed'
			WHERE author_id = ? AND repository = ?
		`, job.AuthorID, job.Repository)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE contribution_evidence_jobs
		SET status = 'completed', finished_at = ? WHERE id = ?
	`, formatTime(collectedAt), job.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteAncillaryAuthorEvidence persists successful public facts or marks
// ancillary completeness failed while retaining prior values.
func (s *Store) CompleteAncillaryAuthorEvidence(
	ctx context.Context, job *AncillaryAuthorJob, accountCreatedAt time.Time,
	recent []contribution.RepositoryActivity, collectedAt time.Time, collectErr error,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if collectErr == nil {
		encoded, err := json.Marshal(recent)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE authors SET account_created_at = ?, recent_activity_json = ?,
				history_complete = 1, collected_at = ?,
				ancillary_status = 'complete', ancillary_collected_at = ?
			WHERE id = ?
		`, formatTime(accountCreatedAt), string(encoded), formatTime(collectedAt),
			formatTime(collectedAt), job.AuthorID)
		if err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx,
		`UPDATE authors SET ancillary_status = 'failed' WHERE id = ?`, job.AuthorID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE ancillary_author_jobs SET status = 'completed', finished_at = ? WHERE id = ?
	`, formatTime(collectedAt), job.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetPullRequestDetail returns one collection-visible PR and its bounded context.
func (s *Store) GetPullRequestDetail(
	ctx context.Context,
	collectionID, repository string,
	number int,
) (PullRequestDetail, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+pullRequestColumns+`
		FROM pull_requests pr
		JOIN collection_pull_requests cpr
			ON cpr.repository = pr.repository AND cpr.number = pr.number
		JOIN collections active_collection
			ON active_collection.id = cpr.collection_id AND active_collection.active = 1
		LEFT JOIN pull_request_quality q
			ON q.collection_id = cpr.collection_id AND q.repository = pr.repository AND q.number = pr.number
		LEFT JOIN analysis_results ar
			ON ar.collection_id = cpr.collection_id AND ar.repository = pr.repository AND ar.number = pr.number
		LEFT JOIN analysis_attempt_state aa
			ON aa.collection_id = cpr.collection_id AND aa.repository = pr.repository AND aa.number = pr.number
		WHERE cpr.collection_id = ? AND pr.repository = ? AND pr.number = ?
	`, collectionID, repository, number)
	pr, err := scanPullRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRequestDetail{}, ErrCollectionNotFound
	}
	if err != nil {
		return PullRequestDetail{}, fmt.Errorf("load pull request detail: %w", err)
	}
	detail := PullRequestDetail{
		PullRequest: pr,
		Readiness:   pr.Readiness,
		Context: PullRequestContext{
			Completeness: "unknown", Limits: make([]ContextLimit, 0),
			Relationships: make([]Relationship, 0), ExternalURLs: make([]ExternalURL, 0),
		},
		Files: make([]PullRequestFile, 0),
		Quality: quality.Assessment{
			Level:         quality.LevelNoConcerns,
			Findings:      make([]quality.Finding, 0),
			Unevaluated:   make([]quality.UnevaluatedRule, 0),
			AIDisclosures: make([]quality.Disclosure, 0),
			Policy:        quality.DefaultPolicy(),
		},
		Analysis: AnalysisDetail{
			AnalysisSummary:    pr.Analysis,
			QualityEvaluations: make([]analysis.QualityEvaluation, 0),
		},
		Groups: make([]CorrelationGroupSummary, 0),
	}
	if err := s.attachContribution(ctx, collectionID, &detail.PullRequest); err != nil {
		return PullRequestDetail{}, err
	}
	var assessmentJSON string
	err = s.db.QueryRowContext(ctx, `
		SELECT assessment_json
		FROM pull_request_quality
		WHERE collection_id = ? AND repository = ? AND number = ?
	`, collectionID, repository, number).Scan(&assessmentJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PullRequestDetail{}, fmt.Errorf("load quality assessment: %w", err)
	}
	if err == nil {
		if unmarshalErr := json.Unmarshal([]byte(assessmentJSON), &detail.Quality); unmarshalErr != nil {
			return PullRequestDetail{}, fmt.Errorf("decode quality assessment: %w", unmarshalErr)
		}
	}
	var analysisJSON, schemaVersion, promptVersion string
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(result.result_json, ''), attempt.schema_version, attempt.prompt_version
		FROM analysis_attempt_state attempt
		LEFT JOIN analysis_results result
		  ON result.collection_id = attempt.collection_id
		 AND result.repository = attempt.repository
		 AND result.number = attempt.number
		WHERE attempt.collection_id = ? AND attempt.repository = ? AND attempt.number = ?
	`, collectionID, repository, number).Scan(&analysisJSON, &schemaVersion, &promptVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PullRequestDetail{}, fmt.Errorf("load analysis detail: %w", err)
	}
	if err == nil {
		detail.Analysis.SchemaVersion = schemaVersion
		detail.Analysis.PromptVersion = promptVersion
	}
	if err == nil && analysisJSON != "" {
		var result analysis.Result
		if unmarshalErr := json.Unmarshal([]byte(analysisJSON), &result); unmarshalErr != nil {
			return PullRequestDetail{}, fmt.Errorf("decode analysis detail: %w", unmarshalErr)
		}
		detail.Analysis.QualityEvaluations = result.QualityEvaluations
		for _, evaluation := range result.QualityEvaluations {
			switch evaluation.Status {
			case analysis.QualityStatusFinding:
				if evaluation.Finding == nil {
					continue
				}
				detail.Quality.Findings = append(detail.Quality.Findings, quality.Finding{
					Rule:         evaluation.Family,
					Severity:     evaluation.Finding.Severity,
					Provenance:   quality.ProvenanceModelInferred,
					Summary:      evaluation.Finding.Summary,
					Evidence:     make([]quality.Evidence, 0),
					Completeness: evaluation.Completeness,
					Confidence:   evaluation.Finding.Confidence,
					SourceIDs:    evaluation.Finding.SourceIDs,
				})
			case analysis.QualityStatusNotEvaluated:
				detail.Quality.Unevaluated = append(
					detail.Quality.Unevaluated,
					quality.UnevaluatedRule{
						Rule:         evaluation.Family,
						Provenance:   quality.ProvenanceModelInferred,
						Completeness: evaluation.Completeness,
						Reason:       evaluation.Reason,
					},
				)
			}
		}
		detail.Quality.Level = quality.Level(detail.Quality.Findings)
	}
	var limitsJSON, collectedAt string
	err = s.db.QueryRowContext(ctx, `
		SELECT completeness, limits_json, collected_at
		FROM pull_request_contexts
		WHERE repository = ? AND number = ?
	`, repository, number).Scan(&detail.Context.Completeness, &limitsJSON, &collectedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PullRequestDetail{}, fmt.Errorf("load context status: %w", err)
	}
	if err == nil {
		if unmarshalErr := json.Unmarshal([]byte(limitsJSON), &detail.Context.Limits); unmarshalErr != nil {
			return PullRequestDetail{}, fmt.Errorf("decode context limits: %w", unmarshalErr)
		}
		if detail.Context.Limits == nil {
			detail.Context.Limits = make([]ContextLimit, 0)
		}
		detail.Context.CollectedAt, err = time.Parse(time.RFC3339Nano, collectedAt)
		if err != nil {
			return PullRequestDetail{}, fmt.Errorf("parse context collection time: %w", err)
		}
	}
	relationshipRows, err := s.db.QueryContext(ctx, `
		SELECT kind, source_id, source_repository, source_number, title, text, state, url, occurred_at
		FROM pull_request_relationships
		WHERE repository = ? AND number = ?
		ORDER BY kind, occurred_at DESC, source_id
	`, repository, number)
	if err != nil {
		return PullRequestDetail{}, fmt.Errorf("list pull request relationships: %w", err)
	}
	for relationshipRows.Next() {
		var relationship Relationship
		var occurredAt string
		if err := relationshipRows.Scan(
			&relationship.Kind, &relationship.SourceID, &relationship.SourceRepository,
			&relationship.SourceNumber, &relationship.Title, &relationship.Text, &relationship.State,
			&relationship.URL, &occurredAt,
		); err != nil {
			_ = relationshipRows.Close()
			return PullRequestDetail{}, fmt.Errorf("scan pull request relationship: %w", err)
		}
		relationship.Timestamp, err = time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			_ = relationshipRows.Close()
			return PullRequestDetail{}, fmt.Errorf("parse relationship timestamp: %w", err)
		}
		detail.Context.Relationships = append(detail.Context.Relationships, relationship)
	}
	if err := relationshipRows.Close(); err != nil {
		return PullRequestDetail{}, err
	}
	externalRows, err := s.db.QueryContext(ctx, `
		SELECT url, source_kind, source_id, discovered_at
		FROM pull_request_external_urls
		WHERE repository = ? AND number = ?
		ORDER BY url, source_kind, source_id
	`, repository, number)
	if err != nil {
		return PullRequestDetail{}, fmt.Errorf("list external URLs: %w", err)
	}
	for externalRows.Next() {
		var external ExternalURL
		var discoveredAt string
		if err := externalRows.Scan(
			&external.URL, &external.SourceKind, &external.SourceID, &discoveredAt,
		); err != nil {
			_ = externalRows.Close()
			return PullRequestDetail{}, fmt.Errorf("scan external URL: %w", err)
		}
		external.DiscoveredAt, err = time.Parse(time.RFC3339Nano, discoveredAt)
		if err != nil {
			_ = externalRows.Close()
			return PullRequestDetail{}, fmt.Errorf("parse external URL timestamp: %w", err)
		}
		detail.Context.ExternalURLs = append(detail.Context.ExternalURLs, external)
	}
	if err := externalRows.Close(); err != nil {
		return PullRequestDetail{}, err
	}
	fileRows, err := s.db.QueryContext(ctx, `
		SELECT path, additions, deletions
		FROM pull_request_files
		WHERE repository = ? AND number = ?
		ORDER BY path
	`, repository, number)
	if err != nil {
		return PullRequestDetail{}, fmt.Errorf("list pull request files: %w", err)
	}
	for fileRows.Next() {
		var file PullRequestFile
		if err := fileRows.Scan(&file.Path, &file.Additions, &file.Deletions); err != nil {
			_ = fileRows.Close()
			return PullRequestDetail{}, fmt.Errorf("scan pull request file: %w", err)
		}
		detail.Files = append(detail.Files, file)
	}
	if err := fileRows.Close(); err != nil {
		return PullRequestDetail{}, err
	}
	detail.Groups, err = s.correlationGroupsForPullRequest(
		ctx, collectionID, repository, number,
	)
	if err != nil {
		return PullRequestDetail{}, fmt.Errorf("list pull request correlation groups: %w", err)
	}
	if detail.Groups == nil {
		detail.Groups = make([]CorrelationGroupSummary, 0)
	}
	return detail, nil
}

// RecoverRefreshJobs requeues work interrupted by a prior scheduler process.
// It must only be called by the process that owns scheduler execution.
func (s *Store) RecoverRefreshJobs(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE refresh_jobs SET status = 'queued', started_at = NULL WHERE status = 'running'
	`); err != nil {
		return fmt.Errorf("recover interrupted refresh jobs: %w", err)
	}
	return nil
}

// RecoverEvidenceJobs requeues evidence claims interrupted by a prior process.
func (s *Store) RecoverEvidenceJobs(ctx context.Context) error {
	for _, table := range []string{
		"contribution_evidence_jobs", "ancillary_author_jobs", "pull_request_diff_jobs",
	} {
		if _, err := s.db.ExecContext(ctx, `UPDATE `+table+
			` SET status = 'queued', started_at = NULL WHERE status = 'running'`); err != nil {
			return fmt.Errorf("recover interrupted %s: %w", table, err)
		}
	}
	return nil
}

func replacePullRequestContext(ctx context.Context, tx *sql.Tx, pr PullRequest) error {
	for _, table := range []string{
		"pull_request_relationships", "pull_request_external_urls", "pull_request_contexts",
	} {
		if _, err := tx.ExecContext(
			ctx, "DELETE FROM "+table+" WHERE repository = ? AND number = ?", pr.Repository, pr.Number,
		); err != nil {
			return fmt.Errorf("clear %s for %s#%d: %w", table, pr.Repository, pr.Number, err)
		}
	}
	if pr.Context == nil {
		return nil
	}
	if pr.Context.Completeness != "complete" && pr.Context.Completeness != "partial" &&
		pr.Context.Completeness != "unknown" {
		return fmt.Errorf("invalid context completeness %q", pr.Context.Completeness)
	}
	if pr.Context.CollectedAt.IsZero() {
		return errors.New("context collection time is required")
	}
	limits := pr.Context.Limits
	if limits == nil {
		limits = make([]ContextLimit, 0)
	}
	limitsJSON, err := json.Marshal(limits)
	if err != nil {
		return fmt.Errorf("encode context limits: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pull_request_contexts (
			repository, number, completeness, limits_json, collected_at
		) VALUES (?, ?, ?, ?, ?)
	`, pr.Repository, pr.Number, pr.Context.Completeness, string(limitsJSON), formatTime(pr.Context.CollectedAt)); err != nil {
		return fmt.Errorf("insert context for %s#%d: %w", pr.Repository, pr.Number, err)
	}
	for _, relationship := range pr.Context.Relationships {
		if relationship.Kind == "" || relationship.SourceID == "" || relationship.URL == "" || relationship.Timestamp.IsZero() {
			return fmt.Errorf("relationship for %s#%d has missing required fields", pr.Repository, pr.Number)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pull_request_relationships (
				repository, number, kind, source_id, source_repository, source_number,
				title, text, state, url, occurred_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, pr.Repository, pr.Number, relationship.Kind, relationship.SourceID,
			relationship.SourceRepository, relationship.SourceNumber, relationship.Title,
			relationship.Text, relationship.State, relationship.URL,
			formatTime(relationship.Timestamp)); err != nil {
			return fmt.Errorf("insert relationship for %s#%d: %w", pr.Repository, pr.Number, err)
		}
	}
	for _, external := range pr.Context.ExternalURLs {
		if external.URL == "" || external.SourceKind == "" || external.SourceID == "" || external.DiscoveredAt.IsZero() {
			return fmt.Errorf("external URL for %s#%d has missing required fields", pr.Repository, pr.Number)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pull_request_external_urls (
				repository, number, url, source_kind, source_id, discovered_at
			) VALUES (?, ?, ?, ?, ?, ?)
		`, pr.Repository, pr.Number, external.URL, external.SourceKind,
			external.SourceID, formatTime(external.DiscoveredAt)); err != nil {
			return fmt.Errorf("insert external URL for %s#%d: %w", pr.Repository, pr.Number, err)
		}
	}
	return nil
}

func replacePullRequestFiles(ctx context.Context, tx *sql.Tx, pr PullRequest) error {
	if _, err := tx.ExecContext(
		ctx, "DELETE FROM pull_request_files WHERE repository = ? AND number = ?",
		pr.Repository, pr.Number,
	); err != nil {
		return fmt.Errorf("clear files for %s#%d: %w", pr.Repository, pr.Number, err)
	}
	seen := make(map[string]struct{}, len(pr.Files))
	for _, file := range pr.Files {
		if file.Path == "" || file.Additions < 0 || file.Deletions < 0 {
			return fmt.Errorf("file for %s#%d has invalid churn or path", pr.Repository, pr.Number)
		}
		if _, exists := seen[file.Path]; exists {
			return fmt.Errorf("file %q is duplicated for %s#%d", file.Path, pr.Repository, pr.Number)
		}
		seen[file.Path] = struct{}{}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pull_request_files (
				repository, number, path, additions, deletions
			) VALUES (?, ?, ?, ?, ?)
		`, pr.Repository, pr.Number, file.Path, file.Additions, file.Deletions); err != nil {
			return fmt.Errorf("insert file %q for %s#%d: %w", file.Path, pr.Repository, pr.Number, err)
		}
	}
	return nil
}

func writeAuthorHistory(ctx context.Context, tx *sql.Tx, history AuthorHistory) error {
	if history.ID <= 0 || history.Login == "" || history.CollectedAt.IsZero() {
		return errors.New("author history has missing identity or collection time")
	}
	recent, err := json.Marshal(history.RecentActivity)
	if err != nil {
		return fmt.Errorf("encode recent author activity: %w", err)
	}
	accountCreatedAt := ""
	if !history.AccountCreatedAt.IsZero() {
		accountCreatedAt = formatTime(history.AccountCreatedAt)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO authors (
			id, login, account_created_at, history_complete, recent_activity_json, collected_at,
			ancillary_status, ancillary_collected_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			login = excluded.login,
			account_created_at = excluded.account_created_at,
			history_complete = excluded.history_complete,
			recent_activity_json = excluded.recent_activity_json,
			collected_at = excluded.collected_at,
			ancillary_status = excluded.ancillary_status,
			ancillary_collected_at = excluded.ancillary_collected_at
	`, history.ID, history.Login, accountCreatedAt, history.Complete,
		string(recent), formatTime(history.CollectedAt),
		map[bool]string{true: "complete", false: "failed"}[history.Complete],
		formatTime(history.CollectedAt)); err != nil {
		return fmt.Errorf("upsert author history for %q: %w", history.Login, err)
	}
	for repository, item := range history.Repositories {
		if repository == "" || item.Merged < 0 || item.ClosedUnmerged < 0 || item.Open < 0 {
			return fmt.Errorf("author history for %q has invalid repository evidence", history.Login)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO author_repository_contributions (
				author_id, repository, association, merged, closed_unmerged, open,
				status, collected_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(author_id, repository) DO UPDATE SET
				association = excluded.association,
				merged = excluded.merged,
				closed_unmerged = excluded.closed_unmerged,
				open = excluded.open,
				status = excluded.status,
				collected_at = excluded.collected_at
		`, history.ID, repository, item.Association, item.Merged,
			item.ClosedUnmerged, item.Open,
			map[bool]string{true: "complete", false: "failed"}[history.Complete],
			formatTime(history.CollectedAt)); err != nil {
			return fmt.Errorf("upsert repository contribution for %q: %w", history.Login, err)
		}
	}
	return nil
}

// GetAuthorContext returns public factual history for an author visible in a collection.
func (s *Store) GetAuthorContext(
	ctx context.Context,
	collectionID, login string,
	now time.Time,
) (AuthorContext, error) {
	var result AuthorContext
	var accountCreatedAt, collectedAt, recentJSON, policyJSON, ancillaryStatus string
	var ancillaryCollectedAt sql.NullString
	var complete bool
	err := s.db.QueryRowContext(ctx, `
		SELECT a.id, a.login, a.account_created_at, a.history_complete,
			a.recent_activity_json, a.collected_at, c.contribution_policy_json,
			a.ancillary_status, a.ancillary_collected_at
		FROM authors a
		JOIN pull_requests pr ON pr.author_id = a.id
		JOIN collection_pull_requests cpr
			ON cpr.repository = pr.repository AND cpr.number = pr.number
		JOIN collections c ON c.id = cpr.collection_id
		WHERE cpr.collection_id = ? AND c.active = 1 AND a.login = ? COLLATE NOCASE
		LIMIT 1
	`, collectionID, login).Scan(
		&result.ID, &result.Login, &accountCreatedAt, &complete,
		&recentJSON, &collectedAt, &policyJSON, &ancillaryStatus,
		&ancillaryCollectedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthorContext{}, ErrCollectionNotFound
	}
	if err != nil {
		return AuthorContext{}, fmt.Errorf("load author %q in %q: %w", login, collectionID, err)
	}
	if accountCreatedAt != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, accountCreatedAt)
		err = parseErr
		if err != nil {
			return AuthorContext{}, fmt.Errorf("parse author account creation: %w", err)
		}
		result.AccountCreatedAt = &parsed
	}
	result.CollectedAt, err = time.Parse(time.RFC3339Nano, collectedAt)
	if err != nil {
		return AuthorContext{}, fmt.Errorf("parse author collection time: %w", err)
	}
	var recent []contribution.RepositoryActivity
	if err := json.Unmarshal([]byte(recentJSON), &recent); err != nil {
		return AuthorContext{}, fmt.Errorf("decode recent author activity: %w", err)
	}
	if ancillaryStatus != "complete" || !ancillaryCollectedAt.Valid {
		complete = false
	} else if _, parseErr := time.Parse(time.RFC3339Nano, ancillaryCollectedAt.String); parseErr != nil {
		complete = false
	}
	var policy contribution.Policy
	if policyJSON == "" {
		policy = contribution.DefaultPolicy()
	} else if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
		return AuthorContext{}, fmt.Errorf("decode contribution policy: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT configured.repository, history.association, history.merged,
			history.closed_unmerged, history.open, history.status, history.collected_at
		FROM collection_repositories configured
		LEFT JOIN author_repository_contributions history
			ON history.repository = configured.repository AND history.author_id = ?
		WHERE configured.collection_id = ?
		ORDER BY configured.repository
	`, result.ID, collectionID)
	if err != nil {
		return AuthorContext{}, fmt.Errorf("load repository contribution history: %w", err)
	}
	repositories := make(map[string]contribution.RepositoryHistory)
	for rows.Next() {
		var repository string
		var association sql.NullString
		var merged, closed, open sql.NullInt64
		var status, evidenceCollected sql.NullString
		if err := rows.Scan(
			&repository, &association, &merged, &closed, &open, &status, &evidenceCollected,
		); err != nil {
			_ = rows.Close()
			return AuthorContext{}, fmt.Errorf("scan repository contribution history: %w", err)
		}
		if !merged.Valid || !closed.Valid || !open.Valid {
			complete = false
			continue
		}
		if status.String != "complete" || !evidenceCollected.Valid {
			complete = false
		} else {
			if _, parseErr := time.Parse(time.RFC3339Nano, evidenceCollected.String); parseErr != nil {
				complete = false
			}
		}
		repositories[repository] = contribution.RepositoryHistory{
			Association: association.String,
			Merged:      int(merged.Int64), ClosedUnmerged: int(closed.Int64), Open: int(open.Int64),
		}
	}
	if err := rows.Close(); err != nil {
		return AuthorContext{}, err
	}
	if err := rows.Err(); err != nil {
		return AuthorContext{}, err
	}
	currentOpen := make(map[string]int)
	openRows, err := s.db.QueryContext(ctx, `
		SELECT pr.repository, COUNT(*)
		FROM pull_requests pr
		JOIN collection_pull_requests cpr
			ON cpr.repository = pr.repository AND cpr.number = pr.number
		WHERE cpr.collection_id = ? AND pr.author_id = ?
		GROUP BY pr.repository
	`, collectionID, result.ID)
	if err != nil {
		return AuthorContext{}, fmt.Errorf("count current author pull requests: %w", err)
	}
	for openRows.Next() {
		var repository string
		var count int
		if err := openRows.Scan(&repository, &count); err != nil {
			_ = openRows.Close()
			return AuthorContext{}, fmt.Errorf("scan current author pull request count: %w", err)
		}
		currentOpen[repository] = count
	}
	if err := openRows.Close(); err != nil {
		return AuthorContext{}, err
	}
	if err := openRows.Err(); err != nil {
		return AuthorContext{}, err
	}
	var createdAt time.Time
	if result.AccountCreatedAt != nil {
		createdAt = *result.AccountCreatedAt
	}
	evaluation := contribution.Evaluate(contribution.Input{
		Complete: complete, AccountCreatedAt: createdAt,
		Repositories: repositories, CurrentOpen: currentOpen, RecentActivity: recent,
	}, policy, now)
	result.Contribution = evaluation
	return result, nil
}

func (s *Store) attachContribution(
	ctx context.Context,
	collectionID string,
	pr *PullRequest,
) error {
	if pr.AuthorID <= 0 || pr.Author == "" {
		return nil
	}
	author, err := s.GetAuthorContext(ctx, collectionID, pr.Author, time.Now().UTC())
	if errors.Is(err, ErrCollectionNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	pr.Contribution = &author.Contribution
	return nil
}

func normalizePullRequest(pr *PullRequest) error {
	if pr.Repository == "" || pr.Number <= 0 || pr.Title == "" || pr.URL == "" || pr.UpdatedAt.IsZero() {
		return errors.New("pull request has missing required fields")
	}
	if pr.Additions < 0 || pr.Deletions < 0 {
		return errors.New("pull request churn must not be negative")
	}
	if pr.ChangedFiles < 0 {
		return errors.New("pull request changed-file count must not be negative")
	}
	if pr.ChangedFiles == 0 && len(pr.Files) > 0 {
		pr.ChangedFiles = len(pr.Files)
	}
	pr.Body = truncateBody(pr.Body, maxBodyBytes)
	if pr.Author == "" {
		pr.Author = "unknown"
	}
	if pr.CreatedAt.IsZero() {
		pr.CreatedAt = pr.UpdatedAt
	}
	if pr.ReviewState == "" {
		pr.ReviewState = "none"
	}
	return nil
}

func contributionRefreshInterval() int64 {
	return int64((7 * 24 * time.Hour) / time.Second)
}

// maxBodyBytes bounds the persisted PR description used by quality rules.
const maxBodyBytes = 64 << 10

func truncateBody(body string, limit int) string {
	if len(body) <= limit {
		return body
	}
	truncated := body[:limit]
	for truncated != "" && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

func loadQualityPolicy(ctx context.Context, tx *sql.Tx, collectionID string) (quality.Policy, error) {
	var encoded string
	err := tx.QueryRowContext(ctx,
		"SELECT quality_policy_json FROM collections WHERE id = ?", collectionID,
	).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return quality.Policy{}, fmt.Errorf("%w: %q", ErrCollectionNotFound, collectionID)
	}
	if err != nil {
		return quality.Policy{}, fmt.Errorf("load quality policy for %q: %w", collectionID, err)
	}
	return decodeQualityPolicy(collectionID, encoded)
}

func loadQualityPolicies(ctx context.Context, tx *sql.Tx) (map[string]quality.Policy, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id, quality_policy_json FROM collections")
	if err != nil {
		return nil, fmt.Errorf("load quality policies: %w", err)
	}
	defer rows.Close()
	policies := make(map[string]quality.Policy)
	for rows.Next() {
		var collectionID, encoded string
		if err := rows.Scan(&collectionID, &encoded); err != nil {
			return nil, fmt.Errorf("scan quality policy: %w", err)
		}
		policy, err := decodeQualityPolicy(collectionID, encoded)
		if err != nil {
			return nil, err
		}
		policies[collectionID] = policy
	}
	return policies, rows.Err()
}

func decodeQualityPolicy(collectionID, encoded string) (quality.Policy, error) {
	if encoded == "" {
		return quality.DefaultPolicy(), nil
	}
	var policy quality.Policy
	if err := json.Unmarshal([]byte(encoded), &policy); err != nil {
		return quality.Policy{}, fmt.Errorf("decode quality policy for %q: %w", collectionID, err)
	}
	return policy, nil
}

func publishedCollections(ctx context.Context, tx *sql.Tx, repository string, number int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT collection_id
		FROM collection_pull_requests
		WHERE repository = ? AND number = ?
	`, repository, number)
	if err != nil {
		return nil, fmt.Errorf("list published collections for %s#%d: %w", repository, number, err)
	}
	defer rows.Close()
	var collectionIDs []string
	for rows.Next() {
		var collectionID string
		if err := rows.Scan(&collectionID); err != nil {
			return nil, fmt.Errorf("scan published collection: %w", err)
		}
		collectionIDs = append(collectionIDs, collectionID)
	}
	return collectionIDs, rows.Err()
}

func qualityInput(pr PullRequest) quality.Input {
	input := quality.Input{
		Repository:       pr.Repository,
		Number:           pr.Number,
		Title:            pr.Title,
		Description:      pr.Body,
		URL:              pr.URL,
		Additions:        pr.Additions,
		Deletions:        pr.Deletions,
		ChangedFileCount: pr.ChangedFiles,
	}
	for _, file := range pr.Files {
		input.Files = append(input.Files, quality.File{
			Path: file.Path, Additions: file.Additions, Deletions: file.Deletions,
		})
	}
	return input
}

func writeQualityAssessment(
	ctx context.Context,
	tx *sql.Tx,
	collectionID string,
	pr PullRequest,
	policy quality.Policy,
	assessedAt time.Time,
) error {
	assessment := quality.Assess(qualityInput(pr), policy)
	encoded, err := json.Marshal(assessment)
	if err != nil {
		return fmt.Errorf("encode quality assessment for %s#%d: %w", pr.Repository, pr.Number, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pull_request_quality (
			collection_id, repository, number, level, findings, assessment_json, assessed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(collection_id, repository, number) DO UPDATE SET
			level = excluded.level,
			findings = excluded.findings,
			assessment_json = excluded.assessment_json,
			assessed_at = excluded.assessed_at
	`, collectionID, pr.Repository, pr.Number, assessment.Level, len(assessment.Findings),
		string(encoded), formatTime(assessedAt)); err != nil {
		return fmt.Errorf(
			"persist quality assessment for %s#%d in %q: %w",
			pr.Repository, pr.Number, collectionID, err,
		)
	}
	return nil
}

func recomputeCollectionQuality(
	ctx context.Context,
	tx *sql.Tx,
	collectionID string,
	policy quality.Policy,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT pr.repository, pr.number, pr.title, pr.body, pr.url,
			pr.additions, pr.deletions, pr.changed_files
		FROM pull_requests pr
		JOIN collection_pull_requests cpr
			ON cpr.repository = pr.repository AND cpr.number = pr.number
		WHERE cpr.collection_id = ?
	`, collectionID)
	if err != nil {
		return fmt.Errorf("list pull requests to reassess for %q: %w", collectionID, err)
	}
	var pullRequests []PullRequest
	for rows.Next() {
		var pr PullRequest
		if err := rows.Scan(
			&pr.Repository, &pr.Number, &pr.Title, &pr.Body, &pr.URL,
			&pr.Additions, &pr.Deletions, &pr.ChangedFiles,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan pull request to reassess: %w", err)
		}
		pullRequests = append(pullRequests, pr)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate pull requests to reassess for %q: %w", collectionID, err)
	}
	assessedAt := time.Now()
	for index := range pullRequests {
		pr := &pullRequests[index]
		fileRows, err := tx.QueryContext(ctx, `
			SELECT path, additions, deletions
			FROM pull_request_files
			WHERE repository = ? AND number = ?
			ORDER BY path
		`, pr.Repository, pr.Number)
		if err != nil {
			return fmt.Errorf("list files to reassess %s#%d: %w", pr.Repository, pr.Number, err)
		}
		for fileRows.Next() {
			var file PullRequestFile
			if err := fileRows.Scan(&file.Path, &file.Additions, &file.Deletions); err != nil {
				_ = fileRows.Close()
				return fmt.Errorf("scan file to reassess %s#%d: %w", pr.Repository, pr.Number, err)
			}
			pr.Files = append(pr.Files, file)
		}
		if err := fileRows.Close(); err != nil {
			return err
		}
		if err := fileRows.Err(); err != nil {
			return fmt.Errorf("iterate files to reassess %s#%d: %w", pr.Repository, pr.Number, err)
		}
		if err := writeQualityAssessment(ctx, tx, collectionID, *pr, policy, assessedAt); err != nil {
			return err
		}
	}
	return nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseNullTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, fmt.Errorf("parse stored time %q: %w", value.String, err)
	}
	return &parsed, nil
}

func discoveryFor(collection config.Collection, repository string) config.RepositoryDiscovery {
	for identity, discovery := range collection.Discovery {
		if strings.EqualFold(identity, repository) {
			return discovery
		}
	}
	return config.RepositoryDiscovery{Mode: "all_open"}
}
