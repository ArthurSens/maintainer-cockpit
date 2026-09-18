package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ArthurSens/maintainer-cockpit/internal/analysis"
	"github.com/ArthurSens/maintainer-cockpit/internal/analysisworker"
	"github.com/ArthurSens/maintainer-cockpit/internal/application"
	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/correlationworker"
	"github.com/ArthurSens/maintainer-cockpit/internal/evidenceworker"
	"github.com/ArthurSens/maintainer-cockpit/internal/githubapp"
	"github.com/ArthurSens/maintainer-cockpit/internal/periodic"
	"github.com/ArthurSens/maintainer-cockpit/internal/refresh"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
	"github.com/ArthurSens/maintainer-cockpit/internal/telemetry"
)

func newServeCommand(stdout, stderr io.Writer) *cobra.Command {
	var configPath string
	var databasePath string
	var listenAddress string
	var fixturePath string
	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the web application",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return serve(command.Context(), stdout, stderr, configPath, databasePath, listenAddress, fixturePath)
		},
	}
	command.Flags().StringVar(&configPath, "config", "config/maintainer-cockpit.yaml", "path to configuration")
	command.Flags().StringVar(&databasePath, "database", "data/maintainer-cockpit.db", "path to SQLite database")
	command.Flags().StringVar(&listenAddress, "listen", "127.0.0.1:8765", "HTTP listen address")
	command.Flags().StringVar(&fixturePath, "fixture", "", "optional development fixture to seed into SQLite")
	if err := command.Flags().MarkHidden("fixture"); err != nil {
		panic(fmt.Sprintf("hide fixture flag: %v", err))
	}
	return command
}

type schedulerProcess struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type schedulerManager struct {
	mu            sync.Mutex
	parent        context.Context
	store         *storage.Store
	runtimeErrors chan<- error
	process       *schedulerProcess
}

type periodicManager struct {
	mu            sync.RWMutex
	parent        context.Context
	store         *storage.Store
	runtimeErrors chan<- error
	process       *schedulerProcess
	coordinator   *periodic.Coordinator
}

type analysisProcess struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type evidenceManager struct {
	mu            sync.Mutex
	parent        context.Context
	store         *storage.Store
	runtimeErrors chan<- error
	process       *analysisProcess
}

func (manager *evidenceManager) replace(router *githubapp.Router) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
	if router == nil {
		return
	}
	profiles := router.Profiles()
	if len(profiles) == 0 {
		return
	}
	workerCtx, cancel := context.WithCancel(manager.parent)
	process := &analysisProcess{cancel: cancel, done: make(chan struct{})}
	manager.process = process
	worker := evidenceworker.New(
		manager.store, router, githubapp.NewAncillaryClient(profiles[0].Client),
		evidenceworker.Options{Diff: router},
	)
	go func() {
		defer close(process.done)
		if err := worker.Run(workerCtx); err != nil {
			select {
			case manager.runtimeErrors <- fmt.Errorf("contribution evidence worker: %w", err):
			case <-workerCtx.Done():
			}
		}
	}()
}

func (manager *evidenceManager) stop() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
}

func (manager *evidenceManager) stopLocked() {
	if manager.process == nil {
		return
	}
	manager.process.cancel()
	<-manager.process.done
	manager.process = nil
}

type analysisManager struct {
	mu            sync.Mutex
	parent        context.Context
	store         *storage.Store
	runtimeErrors chan<- error
	process       *analysisProcess
}

type correlationManager struct {
	mu            sync.Mutex
	parent        context.Context
	store         *storage.Store
	runtimeErrors chan<- error
	process       *analysisProcess
}

func (manager *correlationManager) replace(providers map[string]correlationworker.Provider) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
	if len(providers) == 0 {
		return
	}
	workerCtx, cancel := context.WithCancel(manager.parent)
	process := &analysisProcess{cancel: cancel, done: make(chan struct{})}
	manager.process = process
	worker := correlationworker.New(manager.store, providers, correlationworker.Options{})
	go func() {
		defer close(process.done)
		if err := worker.Run(workerCtx); err != nil {
			select {
			case manager.runtimeErrors <- fmt.Errorf("correlation worker: %w", err):
			case <-workerCtx.Done():
			}
		}
	}()
}

func (manager *correlationManager) stop() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
}

func (manager *correlationManager) stopLocked() {
	if manager.process == nil {
		return
	}
	manager.process.cancel()
	<-manager.process.done
	manager.process = nil
}

func (manager *analysisManager) replace(providers map[string]analysis.Provider) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
	if len(providers) == 0 {
		return
	}
	workerCtx, cancel := context.WithCancel(manager.parent)
	process := &analysisProcess{cancel: cancel, done: make(chan struct{})}
	manager.process = process
	worker := analysisworker.New(manager.store, providers, analysisworker.Options{})
	go func() {
		defer close(process.done)
		if err := worker.Run(workerCtx); err != nil {
			select {
			case manager.runtimeErrors <- fmt.Errorf("analysis worker: %w", err):
			case <-workerCtx.Done():
			}
		}
	}()
}

func (manager *analysisManager) stop() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
}

func (manager *analysisManager) stopLocked() {
	if manager.process == nil {
		return
	}
	manager.process.cancel()
	<-manager.process.done
	manager.process = nil
}

func (manager *schedulerManager) replace(collector refresh.Collector) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
	if collector == nil {
		return
	}
	schedulerCtx, cancel := context.WithCancel(manager.parent)
	process := &schedulerProcess{cancel: cancel, done: make(chan struct{})}
	manager.process = process
	scheduler := refresh.NewScheduler(manager.store, collector)
	go func() {
		defer close(process.done)
		if err := scheduler.Run(schedulerCtx); err != nil {
			select {
			case manager.runtimeErrors <- fmt.Errorf("refresh scheduler: %w", err):
			case <-schedulerCtx.Done():
			}
		}
	}()
}

func (manager *schedulerManager) stop() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
}

func (manager *schedulerManager) stopLocked() {
	if manager.process == nil {
		return
	}
	manager.process.cancel()
	<-manager.process.done
	manager.process = nil
}

func (manager *periodicManager) replace(loaded config.Config) error {
	coordinator, err := periodic.New(periodicEntries(loaded, manager.store), manager.store)
	if err != nil {
		return err
	}
	if err := coordinator.Initialize(manager.parent, time.Now().UTC()); err != nil {
		return err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
	workerCtx, cancel := context.WithCancel(manager.parent)
	process := &schedulerProcess{cancel: cancel, done: make(chan struct{})}
	manager.process = process
	manager.coordinator = coordinator
	go func() {
		defer close(process.done)
		if err := coordinator.Run(workerCtx); err != nil {
			select {
			case manager.runtimeErrors <- fmt.Errorf("periodic scheduler: %w", err):
			case <-workerCtx.Done():
			}
		}
	}()
	return nil
}

func (manager *periodicManager) status() []periodic.Status {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.coordinator == nil {
		return nil
	}
	return manager.coordinator.Status()
}

func (manager *periodicManager) stop() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.stopLocked()
}

func (manager *periodicManager) stopLocked() {
	if manager.process == nil {
		manager.coordinator = nil
		return
	}
	manager.process.cancel()
	<-manager.process.done
	manager.process = nil
	manager.coordinator = nil
}

func periodicEntries(loaded config.Config, store *storage.Store) []periodic.Entry {
	var entries []periodic.Entry
	if loaded.GitHub != nil && loaded.GitHub.Authentication != nil {
		entries = append(entries, periodic.Entry{
			Operation: "github_refresh", Expression: loaded.GitHub.Schedule,
			Dispatch: func(ctx context.Context, now time.Time) error {
				return store.EnqueueAllRefreshes(ctx, now)
			},
		})
	}
	for _, collection := range loaded.Collections {
		collectionID := collection.ID
		if collection.Contribution != nil {
			entries = append(entries, periodic.Entry{
				Operation: "github_contribution", Target: collectionID,
				Expression: collection.Contribution.Schedule,
				Dispatch: func(ctx context.Context, now time.Time) error {
					return store.EnqueueCollectionEvidence(ctx, collectionID, now)
				},
			})
		}
		if collection.FeatureCorrelation != nil {
			entries = append(entries, periodic.Entry{
				Operation: "feature_correlation", Target: collectionID,
				Expression: collection.FeatureCorrelation.Schedule,
				Dispatch: func(ctx context.Context, now time.Time) error {
					_, _, err := store.EnqueueCollectionCorrelation(ctx, collectionID, false, now)
					return err
				},
			})
		}
	}
	return entries
}

func reconcileDisabledSchedules(
	ctx context.Context,
	store *storage.Store,
	previous, next config.Config,
	now time.Time,
) error {
	nextContribution := make(map[string]struct{})
	nextCorrelation := make(map[string]struct{})
	for _, collection := range next.Collections {
		if collection.Contribution != nil {
			nextContribution[collection.ID] = struct{}{}
		}
		if collection.FeatureCorrelation != nil {
			nextCorrelation[collection.ID] = struct{}{}
		}
	}
	for _, collection := range previous.Collections {
		if collection.Contribution != nil {
			if _, exists := nextContribution[collection.ID]; !exists {
				if err := store.DeleteScheduleState(ctx, "github_contribution", collection.ID); err != nil {
					return err
				}
			}
		}
		if collection.FeatureCorrelation != nil {
			if _, exists := nextCorrelation[collection.ID]; !exists {
				if err := store.DeleteScheduleState(ctx, "feature_correlation", collection.ID); err != nil {
					return err
				}
			}
		}
	}
	enabled := make([]string, 0, len(nextContribution))
	for collectionID := range nextContribution {
		enabled = append(enabled, collectionID)
	}
	sort.Strings(enabled)
	return store.CancelDisabledContributionEvidence(ctx, enabled, now)
}

func serve(
	parent context.Context,
	stdout io.Writer,
	stderr io.Writer,
	configPath string,
	databasePath string,
	listenAddress string,
	fixturePath string,
) error {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	loadedConfig, err := config.Load(configPath)
	if err != nil {
		return err
	}
	app, err := application.New(ctx, configPath, databasePath)
	if err != nil {
		return err
	}
	defer app.Close()
	telemetryRuntime, err := telemetry.Initialize(ctx, app.Store())
	if err != nil {
		return fmt.Errorf("configure OpenTelemetry: %w", err)
	}
	previousLogger := slog.Default()
	slog.SetDefault(telemetryRuntime.Logger("github.com/ArthurSens/maintainer-cockpit"))
	defer slog.SetDefault(previousLogger)
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := telemetryRuntime.Shutdown(shutdownContext); err != nil {
			fmt.Fprintf(stderr, "OpenTelemetry shutdown failed: %v\n", err)
		}
	}()
	app.SetTelemetryStatus(func() application.TelemetryStatus {
		status := telemetryRuntime.Status()
		return application.TelemetryStatus{
			Configured: status.Configured, State: status.State, Message: status.Message,
		}
	})
	// "-" keeps fixture mode offline without reseeding a persisted database.
	if fixturePath != "" && fixturePath != "-" {
		if err := app.SeedFixture(ctx, fixturePath); err != nil {
			return err
		}
	}
	runtimeErrors := make(chan error, 5)
	startExternalWorkers := externalWorkersEnabled(fixturePath)
	var collector refresh.Collector
	var githubRouter *githubapp.Router
	if startExternalWorkers && hasGitHubCollectionAuthentication(loadedConfig) {
		router, err := newGitHubClients(loadedConfig)
		if err != nil {
			return err
		}
		configureGitHubObservers(router, app.Store())
		collector = router
		githubRouter = router
	}
	schedulers := &schedulerManager{
		parent: ctx, store: app.Store(), runtimeErrors: runtimeErrors,
	}
	schedulers.replace(collector)
	defer schedulers.stop()
	evidence := &evidenceManager{
		parent: ctx, store: app.Store(), runtimeErrors: runtimeErrors,
	}
	evidence.replace(githubRouter)
	defer evidence.stop()
	var providers map[string]analysis.Provider
	if startExternalWorkers {
		providers, err = newAnalysisProviders(loadedConfig)
		if err != nil {
			return err
		}
	}
	analyses := &analysisManager{
		parent: ctx, store: app.Store(), runtimeErrors: runtimeErrors,
	}
	analyses.replace(providers)
	defer analyses.stop()
	correlations := &correlationManager{
		parent: ctx, store: app.Store(), runtimeErrors: runtimeErrors,
	}
	correlations.replace(correlationProviders(providers))
	defer correlations.stop()
	periodics := &periodicManager{
		parent: ctx, store: app.Store(), runtimeErrors: runtimeErrors,
	}
	if startExternalWorkers {
		if err := periodics.replace(loadedConfig); err != nil {
			return err
		}
	}
	defer periodics.stop()
	app.SetScheduleStatus(periodics.status)
	if err := telemetry.RegisterScheduleInstruments(periodics.status); err != nil {
		return fmt.Errorf("configure schedule telemetry: %w", err)
	}

	reloadRuntime := func(reloadContext context.Context) error {
		next, err := config.Load(configPath)
		if err != nil {
			return err
		}
		var nextCollector refresh.Collector
		var nextRouter *githubapp.Router
		if startExternalWorkers && hasGitHubCollectionAuthentication(next) {
			router, err := newGitHubClients(next)
			if err != nil {
				return err
			}
			configureGitHubObservers(router, app.Store())
			nextCollector = router
			nextRouter = router
		}
		var nextProviders map[string]analysis.Provider
		if startExternalWorkers {
			nextProviders, err = newAnalysisProviders(next)
			if err != nil {
				return err
			}
		}
		// Secret values are deliberately not retained in Config, so equal
		// references cannot prove equal credential generations after reload.
		// Revalidate every authenticated repository on each successful reload.
		var affected []string
		if startExternalWorkers {
			affected = repositoriesRequiringCredentialGenerationValidation(next)
		}
		if err := app.ApplyConfig(reloadContext, next); err != nil {
			return err
		}
		periodics.stop()
		if startExternalWorkers {
			if err := reconcileDisabledSchedules(
				reloadContext, app.Store(), loadedConfig, next, time.Now().UTC(),
			); err != nil {
				return err
			}
		}
		if startExternalWorkers {
			if err := app.Store().MarkRouteOperationsPending(
				reloadContext, affected, time.Now().UTC(),
			); err != nil {
				fmt.Fprintf(
					stderr,
					"configuration reload warning: could not mark GitHub routes pending; continuing runtime replacement: %v\n",
					err,
				)
			}
		}
		schedulers.replace(nextCollector)
		evidence.replace(nextRouter)
		analyses.replace(nextProviders)
		correlations.replace(correlationProviders(nextProviders))
		if startExternalWorkers {
			if err := periodics.replace(next); err != nil {
				return err
			}
		}
		loadedConfig = next
		return nil
	}
	app.SetRuntimeReload(reloadRuntime)

	reloads := make(chan os.Signal, 1)
	signal.Notify(reloads, syscall.SIGHUP)
	defer signal.Stop(reloads)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloads:
				if err := reloadRuntime(ctx); err != nil {
					fmt.Fprintf(stderr, "configuration reload failed: %v\n", err)
					_ = app.Store().RecordAudit(
						ctx, "reload_failed", nil, "invalid_configuration", time.Now().UTC(),
					)
					continue
				}
				_ = app.Store().RecordAudit(
					ctx, "reload_succeeded", nil, "configuration_changed", time.Now().UTC(),
				)
				fmt.Fprintln(stdout, "configuration reloaded")
			}
		}
	}()

	if _, err := app.Store().RunRetention(ctx, time.Now().UTC()); err != nil {
		fmt.Fprintf(stderr, "retention failed: %v\n", err)
	}
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if _, err := app.Store().RunRetention(ctx, now.UTC()); err != nil {
					fmt.Fprintf(stderr, "retention failed: %v\n", err)
				}
			}
		}
	}()

	server := &http.Server{
		Addr:              listenAddress,
		Handler:           newServerHandler(app.Handler()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		fmt.Fprintf(stdout, "Maintainer Cockpit listening on http://%s/\n", listenAddress)
		runtimeErrors <- server.ListenAndServe()
	}()
	select {
	case err := <-runtimeErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func newServerHandler(applicationHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/", applicationHandler)
	return mux
}

func newAnalysisProviders(loaded config.Config) (map[string]analysis.Provider, error) {
	providers := make(map[string]analysis.Provider, len(loaded.ModelProviders))
	for name, configured := range loaded.ModelProviders {
		var apiKey string
		if configured.APIKey.Environment != "" || configured.APIKey.File != "" {
			value, err := config.ResolveSecret(configured.APIKey)
			if err != nil {
				return nil, fmt.Errorf("resolve API key for model provider %q: %w", name, err)
			}
			apiKey = string(value)
		}
		provider, err := analysis.NewHTTPProvider(analysis.HTTPProviderOptions{
			Kind: configured.Type, BaseURL: configured.BaseURL, Model: configured.Model,
			APIKey: apiKey, Client: modelProviderHTTPClient(),
		})
		if err != nil {
			return nil, fmt.Errorf("configure model provider %q: %w", name, err)
		}
		providers[name] = provider
	}
	return providers, nil
}

func modelProviderHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Minute}
}

type githubProfileIdentity struct {
	Name            string
	Type            string
	AppID           int64
	InstallationID  int64
	SecretReference config.SecretReference
}

func githubAuthenticationIdentity(
	loaded config.Config,
) map[string][]githubProfileIdentity {
	result := make(map[string][]githubProfileIdentity)
	if loaded.GitHub == nil || loaded.GitHub.Authentication == nil {
		return result
	}
	authentication := loaded.GitHub.Authentication
	for owner, assignment := range authentication.Owners {
		for _, name := range assignment.Profiles {
			profile := authentication.Profiles[name]
			identity := githubProfileIdentity{Name: name, Type: profile.Type}
			if profile.AppID != nil {
				identity.AppID = *profile.AppID
			}
			if profile.InstallationID != nil {
				identity.InstallationID = *profile.InstallationID
			}
			switch {
			case profile.PrivateKey != nil:
				identity.SecretReference = *profile.PrivateKey
			case profile.Token != nil:
				identity.SecretReference = *profile.Token
			}
			result[strings.ToLower(owner)] = append(
				result[strings.ToLower(owner)], identity,
			)
		}
	}
	return result
}

type enqueueRefreshFunc func(
	context.Context, string, time.Time, bool,
) (bool, error)

func enqueueRoutingRefreshes(
	reloadContext context.Context,
	repositories []string,
	scheduledFor time.Time,
	enqueue enqueueRefreshFunc,
) []error {
	var warnings []error
	for _, repository := range repositories {
		if _, err := enqueue(reloadContext, repository, scheduledFor, true); err == nil {
			continue
		}
		fallbackContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := enqueue(fallbackContext, repository, scheduledFor, true)
		cancel()
		if err != nil {
			warnings = append(warnings, fmt.Errorf(
				"could not immediately schedule routing-changed repository %q; "+
					"the scheduler will retry it at its normal cadence: %w",
				repository, err,
			))
		}
	}
	return warnings
}

func githubAuthenticationEqual(first, second config.Config) bool {
	return reflect.DeepEqual(
		githubAuthenticationIdentity(first), githubAuthenticationIdentity(second),
	)
}

func hasGitHubCollectionAuthentication(loaded config.Config) bool {
	return loaded.GitHub != nil && loaded.GitHub.Authentication != nil
}

func externalWorkersEnabled(fixturePath string) bool {
	return fixturePath == ""
}

func repositoriesWithChangedGitHubRouting(
	previous, next config.Config,
) []string {
	before := githubAuthenticationIdentity(previous)
	after := githubAuthenticationIdentity(next)
	var result []string
	for _, repository := range configuredRepositories(next) {
		owner, _, valid := strings.Cut(repository, "/")
		if !valid {
			continue
		}
		key := strings.ToLower(owner)
		if !reflect.DeepEqual(before[key], after[key]) {
			result = append(result, repository)
		}
	}
	sort.Strings(result)
	return result
}

func repositoriesRequiringCredentialGenerationValidation(
	loaded config.Config,
) []string {
	if loaded.GitHub == nil || loaded.GitHub.Authentication == nil {
		return nil
	}
	result := configuredRepositories(loaded)
	sort.Strings(result)
	return result
}

func correlationProviders(
	providers map[string]analysis.Provider,
) map[string]correlationworker.Provider {
	result := make(map[string]correlationworker.Provider, len(providers))
	for name, provider := range providers {
		if correlationProvider, ok := provider.(correlationworker.Provider); ok {
			result[name] = correlationProvider
		}
	}
	return result
}

func configureGitHubObservers(router *githubapp.Router, store *storage.Store) {
	for _, profile := range router.Profiles() {
		profile.Client.SetRateLimitObserver(func(remaining, limit int, resetAt, observedAt time.Time) {
			_ = store.RecordGitHubRateLimit(
				context.Background(), remaining, limit, resetAt, observedAt,
			)
		})
		profile.Client.SetRetryObserver(func(event githubapp.RetryEvent) {
			_ = store.RecordGitHubRetry(
				context.Background(), event.State, event.Reason, event.Wait,
				event.Retry, event.ObservedAt,
			)
			telemetry.RecordGitHubRetry(event.State, event.Reason)
		})
	}
}
