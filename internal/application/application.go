// Package application wires configuration, persistence, HTTP APIs, and the
// embedded public interface into one runnable Maintainer Cockpit process.
package application

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/analysis"
	"github.com/ArthurSens/maintainer-cockpit/internal/auth"
	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/correlation"
	"github.com/ArthurSens/maintainer-cockpit/internal/githubapp"
	"github.com/ArthurSens/maintainer-cockpit/internal/periodic"
	listquery "github.com/ArthurSens/maintainer-cockpit/internal/query"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
	"github.com/ArthurSens/maintainer-cockpit/internal/web"
)

var collectionIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Application is the long-running Maintainer Cockpit process.
type Application struct {
	configPath      string
	store           *storage.Store
	static          fs.FS
	mu              sync.RWMutex
	config          config.Config
	auth            auth.Provider
	authInjected    bool
	now             func() time.Time
	runtimeReload   func(context.Context) error
	telemetryStatus func() TelemetryStatus
	scheduleStatus  func() []periodic.Status
}

// Options supplies testable process dependencies.
type Options struct {
	AuthProvider    auth.Provider
	Now             func() time.Time
	Reload          func(context.Context) error
	TelemetryStatus func() TelemetryStatus
}

// TelemetryStatus is safe deployment-administrator exporter state.
type TelemetryStatus struct {
	Configured bool   `json:"configured"`
	State      string `json:"state"`
	Message    string `json:"message,omitempty"`
}

// New loads configuration, opens storage, and atomically persists configured
// collections before returning a runnable application.
func New(ctx context.Context, configPath, databasePath string) (*Application, error) {
	return NewWithOptions(ctx, configPath, databasePath, Options{})
}

// NewWithOptions creates an application with optional explicit dependencies.
func NewWithOptions(
	ctx context.Context, configPath, databasePath string, options Options,
) (*Application, error) {
	loadedConfig, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	authProvider := options.AuthProvider
	if authProvider == nil {
		authProvider, err = configuredAuthProvider(loadedConfig)
		if err != nil {
			return nil, err
		}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	store, err := storage.Open(databasePath)
	if err != nil {
		return nil, err
	}
	if err := store.SyncCollectionsAt(ctx, loadedConfig.Collections, now().UTC()); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("persist configured collections: %w", err)
	}
	return &Application{
		configPath:      configPath,
		store:           store,
		static:          web.Files(),
		config:          loadedConfig,
		auth:            authProvider,
		authInjected:    options.AuthProvider != nil,
		now:             now,
		runtimeReload:   options.Reload,
		telemetryStatus: options.TelemetryStatus,
	}, nil
}

func configuredAuthProvider(loaded config.Config) (auth.Provider, error) {
	if loaded.GitHub == nil || loaded.GitHub.UserAuth == nil {
		return nil, nil
	}
	secret, err := config.ResolveSecret(loaded.GitHub.UserAuth.ClientSecret)
	if err != nil {
		return nil, fmt.Errorf("resolve GitHub user authentication client secret: %w", err)
	}
	provider, err := auth.NewGitHubProvider(auth.GitHubOptions{
		ClientID: loaded.GitHub.UserAuth.ClientID, ClientSecret: string(secret),
	})
	if err != nil {
		return nil, err
	}
	return provider, nil
}

// Store exposes persisted data to collectors and integration tests.
func (a *Application) Store() *storage.Store {
	return a.store
}

// Close releases application resources.
func (a *Application) Close() error {
	return a.store.Close()
}

// SetRuntimeReload installs the callback that atomically reloads all runtime
// dependencies in addition to the application configuration.
func (a *Application) SetRuntimeReload(reload func(context.Context) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runtimeReload = reload
}

// SetTelemetryStatus installs a non-sensitive telemetry health callback.
func (a *Application) SetTelemetryStatus(status func() TelemetryStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.telemetryStatus = status
}

// SetScheduleStatus installs the live, non-sensitive cron status callback.
func (a *Application) SetScheduleStatus(status func() []periodic.Status) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.scheduleStatus = status
}

// Reload validates the entire configuration before atomically replacing the
// persisted and active collection configuration.
func (a *Application) Reload(ctx context.Context) error {
	next, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	return a.ApplyConfig(ctx, next)
}

// ApplyConfig atomically persists an already validated configuration.
func (a *Application) ApplyConfig(ctx context.Context, next config.Config) error {
	if err := next.Validate(); err != nil {
		return err
	}
	nextAuth := a.auth
	if !a.authInjected {
		var err error
		nextAuth, err = configuredAuthProvider(next)
		if err != nil {
			return err
		}
	}
	if err := a.store.SyncCollectionsAt(ctx, next.Collections, a.now().UTC()); err != nil {
		return fmt.Errorf("persist reloaded collections: %w", err)
	}
	a.mu.Lock()
	policyChanged := !reflect.DeepEqual(configurationPolicies(a.config), configurationPolicies(next))
	a.config = next
	a.auth = nextAuth
	a.mu.Unlock()
	if policyChanged {
		_ = a.store.RecordAudit(
			ctx, "policy_changed", nil, "configuration_policy", a.now().UTC(),
		)
	}
	return nil
}

func configurationPolicies(loaded config.Config) any {
	type collectionPolicy struct {
		ID           string
		Quality      *config.Quality
		Contribution *config.Contribution
	}
	result := make([]collectionPolicy, 0, len(loaded.Collections))
	for _, collection := range loaded.Collections {
		result = append(result, collectionPolicy{
			ID: collection.ID, Quality: collection.Quality, Contribution: collection.Contribution,
		})
	}
	return result
}

// Handler returns the Maintainer Cockpit HTTP handler.
func (a *Application) Handler() http.Handler {
	return http.HandlerFunc(a.serveHTTP)
}

func (a *Application) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https://avatars.githubusercontent.com; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

	switch {
	case r.URL.Path == "/-/healthy":
		a.serveHealthy(w, r)
	case r.URL.Path == "/-/ready":
		a.serveReady(w, r)
	case r.URL.Path == "/-/reload":
		a.serveReload(w, r)
	case r.URL.Path == "/auth/login":
		a.serveLogin(w, r)
	case r.URL.Path == "/auth/callback":
		a.serveAuthCallback(w, r)
	case r.URL.Path == "/auth/logout":
		a.serveLogout(w, r)
	case r.URL.Path == "/api/viewer":
		a.serveViewer(w, r)
	case r.URL.Path == "/api/me/data":
		a.serveDeletePersonalData(w, r)
	case r.URL.Path == "/api/collections":
		if !allowRead(w, r) {
			return
		}
		a.serveCollections(w, r)
	case r.URL.Path == "/api/analysis/status":
		if !allowRead(w, r) {
			return
		}
		a.serveAnalysisStatus(w, r)
	case r.URL.Path == "/api/admin/status":
		a.serveAdminStatus(w, r)
	case r.URL.Path == "/api/admin/refresh":
		a.serveAdminRefresh(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/admin/model-payloads/"):
		a.serveRetainedModelPayload(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/collections/"):
		a.serveCollectionResource(w, r)
	case r.URL.Path == "/" || r.URL.Path == "/settings" || r.URL.Path == "/admin" ||
		isCollectionPage(r.URL.Path) || isAuthorPage(r.URL.Path):
		if !allowRead(w, r) {
			return
		}
		a.serveStaticFile(w, r, "index.html")
	default:
		if !allowRead(w, r) {
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		a.serveStaticFile(w, r, name)
	}
}

func (*Application) serveHealthy(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *Application) serveReady(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	if err := a.store.Health(r.Context()); err != nil {
		sendJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *Application) serveReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.mu.RLock()
	current := a.config
	reload := a.runtimeReload
	a.mu.RUnlock()
	if current.Operations == nil {
		sendError(w, http.StatusNotFound, "reload is not configured")
		return
	}
	secret, err := config.ResolveSecret(current.Operations.ReloadSecret)
	if err != nil {
		_ = a.store.RecordAudit(r.Context(), "reload_failed", nil, "credential_unavailable", a.now().UTC())
		sendError(w, http.StatusServiceUnavailable, "reload is unavailable")
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == r.Header.Get("Authorization") || subtle.ConstantTimeCompare(
		[]byte(token), []byte(strings.TrimSpace(string(secret))),
	) != 1 {
		_ = a.store.RecordAudit(r.Context(), "authorization_denied", nil, "reload", a.now().UTC())
		w.Header().Set("WWW-Authenticate", "Bearer")
		sendError(w, http.StatusUnauthorized, "reload authorization required")
		return
	}
	if reload == nil {
		reload = a.Reload
	}
	if err := reload(r.Context()); err != nil {
		_ = a.store.RecordAudit(r.Context(), "reload_failed", nil, "invalid_configuration", a.now().UTC())
		sendError(w, http.StatusBadRequest, "configuration reload failed")
		return
	}
	_ = a.store.RecordAudit(r.Context(), "reload_succeeded", nil, "configuration_changed", a.now().UTC())
	w.WriteHeader(http.StatusNoContent)
}

const (
	sessionCookieName = "__Host-maintainer-cockpit"
	flowCookieName    = "__Host-maintainer-cockpit-flow"
)

func (a *Application) serveLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	config, provider := a.authConfig()
	if provider == nil || config.GitHub == nil || config.GitHub.UserAuth == nil {
		sendError(w, http.StatusNotFound, "login is not configured")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	flowID, err := randomToken()
	if err != nil {
		sendError(w, http.StatusInternalServerError, "start login")
		return
	}
	state, err := randomToken()
	if err != nil {
		sendError(w, http.StatusInternalServerError, "start login")
		return
	}
	verifier, err := randomToken()
	if err != nil {
		sendError(w, http.StatusInternalServerError, "start login")
		return
	}
	now := a.now().UTC()
	if err := a.store.CreateAuthFlow(r.Context(), storage.AuthFlow{
		ID: flowID, State: state, CodeVerifier: verifier, ExpiresAt: now.Add(10 * time.Minute),
	}); err != nil {
		sendError(w, http.StatusInternalServerError, "start login")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: flowCookieName, Value: flowID, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	http.Redirect(w, r, provider.AuthorizationURL(
		state, auth.PKCEChallenge(verifier), callbackURL(config.ExternalBaseURL),
	), http.StatusFound)
}

func (a *Application) serveAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	config, provider := a.authConfig()
	if provider == nil {
		sendError(w, http.StatusNotFound, "login is not configured")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	cookie, err := r.Cookie(flowCookieName)
	if err != nil || cookie.Value == "" {
		sendError(w, http.StatusBadRequest, "invalid login state")
		return
	}
	clearCookie(w, flowCookieName)
	flow, err := a.store.ConsumeAuthFlow(
		r.Context(), cookie.Value, r.URL.Query().Get("state"), a.now().UTC(),
	)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "complete login")
		return
	}
	if flow == nil || r.URL.Query().Get("code") == "" {
		sendError(w, http.StatusBadRequest, "invalid or expired login state")
		return
	}
	identity, evidence, err := provider.Authenticate(
		r.Context(), r.URL.Query().Get("code"), flow.CodeVerifier,
		callbackURL(config.ExternalBaseURL), authorizationRequirements(config),
	)
	if err != nil {
		_ = a.store.RecordAudit(r.Context(), "authentication_denied", nil, "verification_failed", a.now().UTC())
		sendError(w, http.StatusBadGateway, "GitHub identity or authorization could not be verified")
		return
	}
	sessionID, err := randomToken()
	if err != nil {
		sendError(w, http.StatusInternalServerError, "complete login")
		return
	}
	csrf, err := randomToken()
	if err != nil {
		sendError(w, http.StatusInternalServerError, "complete login")
		return
	}
	now := a.now().UTC()
	if err := a.store.CreateSession(r.Context(), storage.Session{
		ID: sessionID, UserID: identity.ID, Login: identity.Login, AvatarURL: identity.AvatarURL,
		Organizations: evidence.Organizations, Teams: evidence.Teams,
		CSRFToken: csrf, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}); err != nil {
		sendError(w, http.StatusInternalServerError, "complete login")
		return
	}
	userID := identity.ID
	if err := a.store.RecordAudit(r.Context(), "authentication_succeeded", &userID, "", now); err != nil {
		_ = a.store.DeleteSession(r.Context(), sessionID)
		sendError(w, http.StatusInternalServerError, "complete login")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: sessionID, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int((24 * time.Hour).Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *Application) serveViewer(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Add("Vary", "Cookie")
	session, err := a.session(r)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load session")
		return
	}
	if session == nil {
		sendJSON(w, http.StatusOK, map[string]any{
			"authenticated": false, "authorizedCollections": []string{},
		})
		return
	}
	config, _ := a.authConfig()
	sendJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"id":            session.UserID, "login": session.Login, "avatarURL": session.AvatarURL,
		"csrfToken":             session.CSRFToken,
		"expiresAt":             session.ExpiresAt,
		"deploymentAdmin":       isDeploymentAdmin(config, session.UserID),
		"authorizedCollections": authorizedCollections(config, session),
	})
}

func (a *Application) serveLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	session, ok := a.requireMutation(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteSession(r.Context(), session.ID); err != nil {
		sendError(w, http.StatusInternalServerError, "log out")
		return
	}
	userID := session.UserID
	_ = a.store.RecordAudit(r.Context(), "logout", &userID, "", a.now().UTC())
	clearCookie(w, sessionCookieName)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Application) serveDeletePersonalData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		sendError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	session, ok := a.requireMutation(w, r)
	if !ok {
		return
	}
	if err := a.store.DeletePersonalData(r.Context(), session.UserID, a.now().UTC()); err != nil {
		sendError(w, http.StatusInternalServerError, "delete personal data")
		return
	}
	clearCookie(w, sessionCookieName)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Application) requireMutation(
	w http.ResponseWriter, r *http.Request,
) (*storage.Session, bool) {
	session, err := a.session(r)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load session")
		return nil, false
	}
	if session == nil {
		_ = a.store.RecordAudit(r.Context(), "authorization_denied", nil, "mutation", a.now().UTC())
		sendError(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	config, _ := a.authConfig()
	if !validOrigin(r.Header.Get("Origin"), config.ExternalBaseURL) ||
		subtle.ConstantTimeCompare(
			[]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRFToken),
		) != 1 {
		userID := session.UserID
		_ = a.store.RecordAudit(r.Context(), "authorization_denied", &userID, "mutation", a.now().UTC())
		sendError(w, http.StatusForbidden, "invalid CSRF token or origin")
		return nil, false
	}
	return session, true
}

func (a *Application) session(r *http.Request) (*storage.Session, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if errors.Is(err, http.ErrNoCookie) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a.store.GetSession(r.Context(), cookie.Value, a.now().UTC())
}

func (a *Application) authConfig() (config.Config, auth.Provider) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.config, a.auth
}

func authorizationRequirements(config config.Config) auth.Requirements {
	var result auth.Requirements
	for _, collection := range config.Collections {
		if collection.Authorization == nil {
			continue
		}
		result.Organizations = append(result.Organizations, collection.Authorization.Organizations...)
		result.Teams = append(result.Teams, collection.Authorization.Teams...)
	}
	return result
}

func authorizedCollections(config config.Config, session *storage.Session) []string {
	result := make([]string, 0)
	for _, collection := range config.Collections {
		if collectionAuthorized(collection, session) {
			result = append(result, collection.ID)
		}
	}
	return result
}

func collectionAuthorized(collection config.Collection, session *storage.Session) bool {
	if session == nil || collection.Authorization == nil {
		return false
	}
	if slices.Contains(collection.Authorization.Users, session.UserID) {
		return true
	}
	for _, configured := range collection.Authorization.Organizations {
		for _, verified := range session.Organizations {
			if strings.EqualFold(configured, verified) {
				return true
			}
		}
	}
	for _, configured := range collection.Authorization.Teams {
		for _, verified := range session.Teams {
			if strings.EqualFold(configured, verified) {
				return true
			}
		}
	}
	return false
}

func isDeploymentAdmin(config config.Config, userID int64) bool {
	return slices.Contains(config.DeploymentAdmins, userID)
}

func callbackURL(base string) string {
	return strings.TrimRight(base, "/") + "/auth/callback"
}

func validOrigin(origin, configured string) bool {
	actual, err := url.Parse(origin)
	if err != nil || actual.Scheme == "" || actual.Host == "" ||
		actual.User != nil || actual.Path != "" || actual.RawQuery != "" || actual.Fragment != "" {
		return false
	}
	expected, err := url.Parse(configured)
	return err == nil && strings.EqualFold(actual.Scheme, expected.Scheme) &&
		strings.EqualFold(actual.Host, expected.Host)
}

func randomToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func allowRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	sendError(w, http.StatusMethodNotAllowed, "method not allowed")
	return false
}

func (a *Application) serveAnalysisStatus(w http.ResponseWriter, r *http.Request) {
	status, err := a.store.AnalysisStatus(r.Context())
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load analysis status")
		return
	}
	sendJSON(w, http.StatusOK, status)
}

func (a *Application) requireDeploymentAdmin(
	w http.ResponseWriter, r *http.Request,
) *storage.Session {
	session, err := a.session(r)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load session")
		return nil
	}
	if session == nil {
		_ = a.store.RecordAudit(r.Context(), "authorization_denied", nil, "admin_status", a.now().UTC())
		sendError(w, http.StatusUnauthorized, "authentication required")
		return nil
	}
	current, _ := a.authConfig()
	if !isDeploymentAdmin(current, session.UserID) {
		userID := session.UserID
		_ = a.store.RecordAudit(r.Context(), "authorization_denied", &userID, "admin_status", a.now().UTC())
		sendError(w, http.StatusForbidden, "deployment administrator access required")
		return nil
	}
	return session
}

func (a *Application) serveAdminStatus(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	if a.requireDeploymentAdmin(w, r) == nil {
		return
	}
	refreshStatus, err := a.store.RefreshStatus(r.Context())
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load refresh status")
		return
	}
	analysisStatus, err := a.store.AnalysisStatus(r.Context())
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load analysis status")
		return
	}
	correlationStatus, err := a.store.CorrelationQueueStatus(r.Context())
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load correlation status")
		return
	}
	rateLimit, err := a.store.GitHubRateLimit(r.Context())
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load GitHub status")
		return
	}
	retryStatus, err := a.store.GitHubRetry(r.Context())
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load GitHub retry status")
		return
	}
	githubRoutes, err := a.store.ListRouteAttempts(r.Context())
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load GitHub route status")
		return
	}
	modelProviders, err := a.store.ModelProviderStatuses(r.Context())
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load model status")
		return
	}
	failures, err := a.store.RecentFailures(r.Context(), 20)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load recent failures")
		return
	}
	auditEvents, err := a.store.RecentAuditEvents(r.Context(), 20)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load audit events")
		return
	}
	current, _ := a.authConfig()
	enabledAnalysisCollections := 0
	enabledCorrelationCollections := 0
	for _, collection := range current.Collections {
		if collection.ModelProvider != "" {
			enabledAnalysisCollections++
		}
		if collection.FeatureCorrelation != nil {
			enabledCorrelationCollections++
		}
	}
	knownProviders := make(map[string]bool, len(modelProviders))
	for _, provider := range modelProviders {
		knownProviders[provider.Name] = true
	}
	for name := range current.ModelProviders {
		if !knownProviders[name] {
			modelProviders = append(modelProviders, storage.ModelProviderStatus{
				Name: name, State: "unknown",
			})
		}
	}
	sort.Slice(modelProviders, func(i, j int) bool {
		return modelProviders[i].Name < modelProviders[j].Name
	})
	telemetry := TelemetryStatus{State: "disabled"}
	a.mu.RLock()
	telemetryStatus := a.telemetryStatus
	scheduleStatus := a.scheduleStatus
	a.mu.RUnlock()
	if telemetryStatus != nil {
		telemetry = telemetryStatus()
	}
	var schedules []periodic.Status
	if scheduleStatus != nil {
		schedules = scheduleStatus()
	}
	storageState := "healthy"
	if err := a.store.Health(r.Context()); err != nil {
		storageState = "failed"
	}
	w.Header().Set("Cache-Control", "private, no-store")
	sendJSON(w, http.StatusOK, map[string]any{
		"collectionJobs":  refreshStatus,
		"analysisJobs":    analysisStatus,
		"correlationJobs": correlationStatus,
		"analysisConfiguration": map[string]int{
			"configuredProviders": len(current.ModelProviders),
			"enabledCollections":  enabledAnalysisCollections,
		},
		"correlationConfiguration": map[string]int{
			"enabledCollections": enabledCorrelationCollections,
		},
		"githubRateLimit": rateLimit,
		"githubRetry":     retryStatus,
		"githubRoutes":    githubRoutes,
		"modelProviders":  modelProviders,
		"storage": map[string]any{
			"state": storageState, "databaseBytes": a.store.DatabaseSize(),
		},
		"policyVersions": map[string]any{
			"analysisSchema":    analysis.SchemaVersion,
			"analysisPrompt":    analysis.PromptVersion,
			"correlationSchema": correlation.SchemaVersion,
			"correlationPrompt": correlation.PromptVersion,
			"inputFingerprint":  githubapp.InputFingerprintVersion,
		},
		"telemetry":      telemetry,
		"schedules":      schedules,
		"recentFailures": failures,
		"auditEvents":    auditEvents,
	})
}

func (a *Application) serveAdminRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	session, ok := a.requireMutation(w, r)
	if !ok {
		return
	}
	current, _ := a.authConfig()
	if !isDeploymentAdmin(current, session.UserID) {
		userID := session.UserID
		_ = a.store.RecordAudit(
			r.Context(), "authorization_denied", &userID, "admin_refresh", a.now().UTC(),
		)
		sendError(w, http.StatusForbidden, "deployment administrator access required")
		return
	}

	seen := make(map[string]struct{})
	repositories := make([]string, 0)
	for _, collection := range current.Collections {
		for _, repository := range collection.Repositories {
			if _, exists := seen[repository]; exists {
				continue
			}
			seen[repository] = struct{}{}
			repositories = append(repositories, repository)
		}
	}
	enqueued := 0
	scheduledFor := a.now().UTC()
	for _, repository := range repositories {
		added, err := a.store.EnqueueRefreshWithForce(
			r.Context(), repository, scheduledFor, true,
		)
		if err != nil {
			sendError(w, http.StatusInternalServerError, "request repository refresh")
			return
		}
		if added {
			enqueued++
		}
	}
	userID := session.UserID
	_ = a.store.RecordAudit(
		r.Context(), "admin_refresh_requested", &userID, "all_repositories", scheduledFor,
	)
	sendJSON(w, http.StatusOK, map[string]int{
		"repositories": len(repositories),
		"enqueued":     enqueued,
		"coalesced":    len(repositories) - enqueued,
	})
}

func (a *Application) serveRetainedModelPayload(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	session := a.requireDeploymentAdmin(w, r)
	if session == nil {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/model-payloads/")
	if len(id) != 32 {
		sendError(w, http.StatusBadRequest, "invalid retained payload ID")
		return
	}
	for _, character := range id {
		if !strings.ContainsRune("0123456789abcdef", character) {
			sendError(w, http.StatusBadRequest, "invalid retained payload ID")
			return
		}
	}
	payload, err := a.store.RetainedModelPayload(r.Context(), id)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load retained model payload")
		return
	}
	if payload == nil {
		sendError(w, http.StatusNotFound, "retained model payload not found")
		return
	}
	userID := session.UserID
	if err := a.store.RecordAudit(
		r.Context(), "model_payload_accessed", &userID, "retained_payload", a.now().UTC(),
	); err != nil {
		sendError(w, http.StatusInternalServerError, "record payload access")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	sendJSON(w, http.StatusOK, payload)
}

func (a *Application) serveCollectionResource(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/collections/"
	relative := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(relative, "/")
	if len(parts) >= 2 && parts[1] == "groups" {
		if !allowRead(w, r) {
			return
		}
		collectionID, err := url.PathUnescape(parts[0])
		if err != nil || !collectionIDRE.MatchString(collectionID) {
			sendError(w, http.StatusBadRequest, "invalid collection ID")
			return
		}
		if len(parts) == 2 {
			groups, err := a.store.ListCorrelationGroups(r.Context(), collectionID)
			if errors.Is(err, storage.ErrCollectionNotFound) {
				sendError(w, http.StatusNotFound, "collection not found")
				return
			}
			if err != nil {
				sendError(w, http.StatusInternalServerError, "load correlation groups")
				return
			}
			summaries := make([]storage.CorrelationGroupSummary, 0, len(groups))
			for _, group := range groups {
				summaries = append(summaries, storage.CorrelationGroupSummary{
					ID: group.ID, Name: group.Name, Description: group.Description,
					OpenMemberCount: group.OpenMemberCount,
					Confidence:      group.Confidence, Provider: group.Provenance.Provider,
					Model: group.Provenance.Model, CorrelatedAt: group.Provenance.CorrelatedAt,
				})
			}
			status, err := a.store.CorrelationStatus(r.Context(), collectionID)
			if err != nil {
				sendError(w, http.StatusInternalServerError, "load correlation status")
				return
			}
			sendJSON(w, http.StatusOK, map[string]any{"groups": summaries, "status": status})
			return
		}
		if len(parts) == 3 {
			groupID, err := url.PathUnescape(parts[2])
			if err != nil || groupID == "" || len(groupID) > 200 {
				sendError(w, http.StatusBadRequest, "invalid group ID")
				return
			}
			group, err := a.store.GetCorrelationGroup(r.Context(), collectionID, groupID)
			if errors.Is(err, storage.ErrCollectionNotFound) {
				sendError(w, http.StatusNotFound, "collection not found")
				return
			}
			if err != nil {
				sendError(w, http.StatusInternalServerError, "load correlation group")
				return
			}
			if group == nil {
				sendError(w, http.StatusNotFound, "correlation group not found")
				return
			}
			sendJSON(w, http.StatusOK, group)
			return
		}
		sendError(w, http.StatusNotFound, "not found")
		return
	}
	if len(parts) == 2 && parts[1] == "progress" {
		collectionID, err := url.PathUnescape(parts[0])
		if err != nil || !collectionIDRE.MatchString(collectionID) {
			sendError(w, http.StatusBadRequest, "invalid collection ID")
			return
		}
		a.serveProgress(w, r, collectionID)
		return
	}
	if len(parts) == 3 && parts[1] == "authors" {
		if !allowRead(w, r) {
			return
		}
		collectionID, collectionErr := url.PathUnescape(parts[0])
		login, loginErr := url.PathUnescape(parts[2])
		if collectionErr != nil || !collectionIDRE.MatchString(collectionID) ||
			loginErr != nil || login == "" || len(login) > 100 || strings.Contains(login, "/") {
			sendError(w, http.StatusBadRequest, "invalid author identity")
			return
		}
		author, err := a.store.GetAuthorContext(
			r.Context(), collectionID, login, time.Now().UTC(),
		)
		if errors.Is(err, storage.ErrCollectionNotFound) {
			sendError(w, http.StatusNotFound, "author not found")
			return
		}
		if err != nil {
			sendError(w, http.StatusInternalServerError, "load author context")
			return
		}
		sendJSON(w, http.StatusOK, author)
		return
	}
	a.serveCollectionPullRequests(w, r)
}

func (a *Application) serveProgress(
	w http.ResponseWriter,
	r *http.Request,
	collectionID string,
) {
	var session *storage.Session
	switch r.Method {
	case http.MethodGet:
		session = a.requireCollectionRead(w, r, collectionID)
	case http.MethodPut:
		session = a.requireCollectionMutation(w, r, collectionID)
		if session != nil {
			var preferences storage.GoalPreferences
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&preferences); err != nil {
				sendError(w, http.StatusBadRequest, "invalid goal preferences")
				return
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				sendError(w, http.StatusBadRequest, "invalid goal preferences")
				return
			}
			if err := a.store.SetGoalPreferences(
				r.Context(), session.UserID, collectionID, preferences,
			); err != nil {
				if errors.Is(err, storage.ErrCollectionNotFound) {
					sendError(w, http.StatusNotFound, "collection not found")
				} else {
					sendError(w, http.StatusBadRequest, err.Error())
				}
				return
			}
		}
	default:
		sendError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if session == nil {
		return
	}
	progress, err := a.store.GetDailyProgress(
		r.Context(), session.UserID, collectionID, a.now().UTC(),
	)
	if errors.Is(err, storage.ErrCollectionNotFound) {
		sendError(w, http.StatusNotFound, "collection not found")
		return
	}
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load daily progress")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Add("Vary", "Cookie")
	sendJSON(w, http.StatusOK, progress)
}

func (a *Application) serveCollections(w http.ResponseWriter, r *http.Request) {
	collections, err := a.store.ListCollections(r.Context())
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load collections")
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"collections": collections})
}

func (a *Application) serveCollectionPullRequests(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/collections/"
	relative := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(relative, "/")
	if relative == r.URL.Path || len(parts) < 2 || parts[1] != "pull-requests" {
		sendError(w, http.StatusNotFound, "not found")
		return
	}
	collectionID, err := url.PathUnescape(parts[0])
	if err != nil || !collectionIDRE.MatchString(collectionID) {
		sendError(w, http.StatusBadRequest, "invalid collection ID")
		return
	}
	if len(parts) == 5 {
		if !allowRead(w, r) {
			return
		}
		a.servePullRequestDetail(w, r, collectionID, parts[2], parts[3], parts[4])
		return
	}
	if len(parts) == 6 && parts[5] == "important" {
		a.serveImportantPullRequest(
			w, r, collectionID, parts[2], parts[3], parts[4],
		)
		return
	}
	if len(parts) == 6 && parts[5] == "hidden" {
		a.serveHiddenPullRequest(
			w, r, collectionID, parts[2], parts[3], parts[4],
		)
		return
	}
	if len(parts) != 2 {
		sendError(w, http.StatusNotFound, "not found")
		return
	}
	if !allowRead(w, r) {
		return
	}
	collections, err := a.store.ListCollections(r.Context())
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load collection")
		return
	}
	var collection *storage.CollectionSummary
	for index := range collections {
		if collections[index].ID == collectionID {
			collection = &collections[index]
			break
		}
	}
	if collection == nil {
		sendError(w, http.StatusNotFound, "collection not found")
		return
	}
	options, err := listquery.Parse(r.URL.Query())
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	view := options.View
	var authorizedSession *storage.Session
	if view == "mine" || view == "hidden" {
		authorizedSession = a.requireCollectionRead(w, r, collectionID)
		if authorizedSession == nil {
			return
		}
	} else {
		authorizedSession, _ = a.authorizedSessionForCollection(r, collectionID)
	}
	var page storage.PullRequestPage
	if authorizedSession != nil {
		page, err = a.store.ListPersonalPullRequestsPage(
			r.Context(), collectionID, authorizedSession.UserID, options, a.now().UTC(),
		)
	} else {
		page, err = a.store.ListPullRequestsPage(r.Context(), collectionID, options)
	}
	if err != nil {
		if errors.Is(err, storage.ErrCollectionNotFound) {
			sendError(w, http.StatusNotFound, "collection not found")
			return
		}
		logPullRequestLoadFailure(
			r.Context(), slog.Default(), collectionID, options,
			authorizedSession != nil, err,
		)
		sendError(w, http.StatusInternalServerError, "load pull requests")
		return
	}
	if authorizedSession != nil {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Add("Vary", "Cookie")
	}
	var nextCursor any
	if page.NextCursor != "" {
		nextCursor = page.NextCursor
	}
	sendJSON(w, http.StatusOK, map[string]any{
		"collection": collection,
		"counts": map[string]int{
			"total": page.Total, "matched": page.Matched,
		},
		"page": map[string]any{
			"limit": options.Limit, "nextCursor": nextCursor,
		},
		"pullRequests": page.PullRequests,
	})
}

func logPullRequestLoadFailure(
	ctx context.Context,
	logger *slog.Logger,
	collectionID string,
	options listquery.Options,
	authenticated bool,
	err error,
) {
	logger.LogAttrs(ctx, slog.LevelError, "pull request list failed",
		slog.String("event", "pull_request_list_failed"),
		slog.String("collection", collectionID),
		slog.String("view", options.View),
		slog.Int("limit", options.Limit),
		slog.Int("offset", options.Offset),
		slog.Bool("authenticated", authenticated),
		slog.String("category", "storage"),
		slog.String("detail", pullRequestLoadFailureDetail(err)),
	)
}

func pullRequestLoadFailureDetail(err error) string {
	message := err.Error()
	for prefix, detail := range map[string]string{
		"wake snoozed pull requests":           "wake_hidden_state",
		"count pull requests":                  "count_pull_requests",
		"count matching pull requests":         "count_matching_pull_requests",
		"list pull request page":               "list_pull_request_page",
		"scan pull request":                    "scan_pull_request",
		"decode pull request readiness":        "decode_pull_request_readiness",
		"decode analysis result":               "decode_analysis_result",
		"load author":                          "load_author",
		"parse author":                         "parse_author",
		"decode recent author activity":        "decode_author_activity",
		"decode contribution policy":           "decode_contribution_policy",
		"list Important pull requests":         "list_important_pull_requests",
		"list hidden pull requests":            "list_hidden_pull_requests",
		"scan hidden pull request":             "scan_hidden_pull_request",
		"parse hidden-state":                   "parse_hidden_state",
		"iterate hidden pull requests":         "iterate_hidden_pull_requests",
		"iterate Important pull requests":      "iterate_important_pull_requests",
		"scan Important pull request":          "scan_important_pull_request",
		"load repository contribution history": "load_contribution_history",
	} {
		if strings.HasPrefix(message, prefix) {
			return detail
		}
	}
	return "storage_failure"
}

func (a *Application) servePullRequestDetail(
	w http.ResponseWriter,
	r *http.Request,
	collectionID, rawOwner, rawRepository, rawNumber string,
) {
	owner, ownerErr := url.PathUnescape(rawOwner)
	repositoryName, repositoryErr := url.PathUnescape(rawRepository)
	number, numberErr := strconv.Atoi(rawNumber)
	if ownerErr != nil || repositoryErr != nil || owner == "" || repositoryName == "" ||
		strings.Contains(owner, "/") || strings.Contains(repositoryName, "/") ||
		numberErr != nil || number <= 0 {
		sendError(w, http.StatusBadRequest, "invalid pull request identity")
		return
	}
	detail, err := a.store.GetPullRequestDetail(
		r.Context(), collectionID, owner+"/"+repositoryName, number,
	)
	if err != nil {
		if errors.Is(err, storage.ErrCollectionNotFound) {
			sendError(w, http.StatusNotFound, "pull request not found")
			return
		}
		sendError(w, http.StatusInternalServerError, "load pull request detail")
		return
	}
	session, _ := a.authorizedSessionForCollection(r, collectionID)
	if session != nil {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Add("Vary", "Cookie")
		if err := a.store.AttachImportantPullRequest(
			r.Context(), session.UserID, collectionID, &detail,
		); err != nil {
			sendError(w, http.StatusInternalServerError, "load Important pull request")
			return
		}
		if err := a.store.AttachHiddenPullRequest(
			r.Context(), session.UserID, collectionID, &detail, a.now().UTC(),
		); err != nil {
			sendError(w, http.StatusInternalServerError, "load hidden pull request")
			return
		}
	}
	sendJSON(w, http.StatusOK, detail)
}

func (a *Application) authorizedSessionForCollection(
	r *http.Request, collectionID string,
) (*storage.Session, error) {
	session, err := a.session(r)
	if err != nil || session == nil {
		return nil, err
	}
	config, _ := a.authConfig()
	for _, collection := range config.Collections {
		if collection.ID == collectionID {
			if collectionAuthorized(collection, session) {
				return session, nil
			}
			return nil, nil
		}
	}
	return nil, nil
}

func (a *Application) requireCollectionRead(
	w http.ResponseWriter, r *http.Request, collectionID string,
) *storage.Session {
	session, err := a.authorizedSessionForCollection(r, collectionID)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "load session")
		return nil
	}
	if session == nil {
		if raw, sessionErr := a.session(r); sessionErr == nil && raw == nil {
			_ = a.store.RecordAudit(r.Context(), "authorization_denied", nil, "collection", a.now().UTC())
			sendError(w, http.StatusUnauthorized, "authentication required")
		} else {
			if raw != nil {
				userID := raw.UserID
				_ = a.store.RecordAudit(r.Context(), "authorization_denied", &userID, "collection", a.now().UTC())
			}
			sendError(w, http.StatusForbidden, "collection authorization required")
		}
		return nil
	}
	return session
}

func (a *Application) requireCollectionMutation(
	w http.ResponseWriter, r *http.Request, collectionID string,
) *storage.Session {
	session, ok := a.requireMutation(w, r)
	if !ok {
		return nil
	}
	config, _ := a.authConfig()
	for _, collection := range config.Collections {
		if collection.ID == collectionID && collectionAuthorized(collection, session) {
			return session
		}
	}
	userID := session.UserID
	_ = a.store.RecordAudit(r.Context(), "authorization_denied", &userID, "collection", a.now().UTC())
	sendError(w, http.StatusForbidden, "collection authorization required")
	return nil
}

func (a *Application) serveImportantPullRequest(
	w http.ResponseWriter,
	r *http.Request,
	collectionID, rawOwner, rawRepository, rawNumber string,
) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		sendError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, ownerErr := url.PathUnescape(rawOwner)
	repositoryName, repositoryErr := url.PathUnescape(rawRepository)
	number, numberErr := strconv.Atoi(rawNumber)
	if ownerErr != nil || repositoryErr != nil || owner == "" || repositoryName == "" ||
		strings.Contains(owner, "/") || strings.Contains(repositoryName, "/") ||
		numberErr != nil || number <= 0 {
		sendError(w, http.StatusBadRequest, "invalid pull request identity")
		return
	}
	session := a.requireCollectionMutation(w, r, collectionID)
	if session == nil {
		return
	}
	repository := owner + "/" + repositoryName
	if r.Method == http.MethodDelete {
		err := a.store.UnmarkPullRequestImportant(
			r.Context(), session.UserID, collectionID, repository, number,
		)
		if errors.Is(err, storage.ErrImportantPullRequestNotFound) {
			sendError(w, http.StatusNotFound, "pull request is not marked Important")
			return
		}
		if err != nil {
			sendError(w, http.StatusInternalServerError, "unmark Important pull request")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	err := a.store.MarkPullRequestImportant(
		r.Context(), session.UserID, collectionID, repository, number,
	)
	if errors.Is(err, storage.ErrPullRequestNotFound) {
		sendError(w, http.StatusNotFound, "pull request not found")
		return
	}
	if err != nil {
		sendError(w, http.StatusInternalServerError, "mark Important pull request")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	sendJSON(w, http.StatusOK, storage.PersonalPullRequestState{Important: true})
}

func (a *Application) serveHiddenPullRequest(
	w http.ResponseWriter,
	r *http.Request,
	collectionID, rawOwner, rawRepository, rawNumber string,
) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		sendError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	owner, ownerErr := url.PathUnescape(rawOwner)
	repositoryName, repositoryErr := url.PathUnescape(rawRepository)
	number, numberErr := strconv.Atoi(rawNumber)
	if ownerErr != nil || repositoryErr != nil || owner == "" || repositoryName == "" ||
		strings.Contains(owner, "/") || strings.Contains(repositoryName, "/") ||
		numberErr != nil || number <= 0 {
		sendError(w, http.StatusBadRequest, "invalid pull request identity")
		return
	}
	session := a.requireCollectionMutation(w, r, collectionID)
	if session == nil {
		return
	}
	repository := owner + "/" + repositoryName
	if r.Method == http.MethodDelete {
		err := a.store.RestorePullRequest(
			r.Context(), session.UserID, collectionID, repository, number,
		)
		if errors.Is(err, storage.ErrHiddenPullRequestNotFound) {
			sendError(w, http.StatusNotFound, "pull request is not hidden")
			return
		}
		if err != nil {
			sendError(w, http.StatusInternalServerError, "restore pull request")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var input struct {
		Kind           string     `json:"kind"`
		Reason         string     `json:"reason"`
		SnoozedUntil   *time.Time `json:"snoozedUntil"`
		WakeOnActivity bool       `json:"wakeOnActivity"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		sendError(w, http.StatusBadRequest, "invalid hidden pull request state")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		sendError(w, http.StatusBadRequest, "invalid hidden pull request state")
		return
	}
	state, err := a.store.SetPullRequestHidden(
		r.Context(), session.UserID, collectionID, repository, number,
		storage.HiddenPullRequestState{
			Kind: input.Kind, Reason: input.Reason,
			SnoozedUntil: input.SnoozedUntil, WakeOnActivity: input.WakeOnActivity,
		},
		a.now().UTC(),
	)
	if errors.Is(err, storage.ErrPullRequestNotFound) {
		sendError(w, http.StatusNotFound, "pull request not found")
		return
	}
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Add("Vary", "Cookie")
	sendJSON(w, http.StatusOK, state)
}

func (a *Application) serveStaticFile(w http.ResponseWriter, r *http.Request, name string) {
	body, err := fs.ReadFile(a.static, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			sendError(w, http.StatusNotFound, "not found")
			return
		}
		sendError(w, http.StatusInternalServerError, "load static application")
		return
	}
	switch path.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if isAuthorPage(r.URL.Path) {
			w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		}
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func isCollectionPage(requestPath string) bool {
	const prefix = "/collections/"
	if !strings.HasPrefix(requestPath, prefix) {
		return false
	}
	id := strings.TrimPrefix(requestPath, prefix)
	return !strings.Contains(id, "/") && collectionIDRE.MatchString(id)
}

func isAuthorPage(requestPath string) bool {
	const prefix = "/collections/"
	if !strings.HasPrefix(requestPath, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(requestPath, prefix), "/")
	if len(parts) != 3 || parts[1] != "authors" || !collectionIDRE.MatchString(parts[0]) {
		return false
	}
	login, err := url.PathUnescape(parts[2])
	return err == nil && login != "" && len(login) <= 100 && !strings.Contains(login, "/")
}

func sendJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func sendError(w http.ResponseWriter, status int, message string) {
	sendJSON(w, status, map[string]string{"error": message})
}
