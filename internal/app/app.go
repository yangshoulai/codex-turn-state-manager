// Package app is the composition root: it owns the plugin's lifecycle, wires
// the stores to the domain services, and exposes the hot-path entry points the
// CPA ABI adapter calls into.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/accounts"
	"github.com/yangshoulai/codex-turn-state-manager/internal/callhistory"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/intercept"
	"github.com/yangshoulai/codex-turn-state-manager/internal/management"
	"github.com/yangshoulai/codex-turn-state-manager/internal/models"
	"github.com/yangshoulai/codex-turn-state-manager/internal/probe"
	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
	"github.com/yangshoulai/codex-turn-state-manager/internal/routing"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
	"github.com/yangshoulai/codex-turn-state-manager/internal/storage"
	"github.com/yangshoulai/codex-turn-state-manager/internal/version"
)

// Config configures the plugin.
type Config struct {
	// DataDir holds state.db and the backups/ directory.
	DataDir string
	// UpstreamBaseURL overrides the Codex backend host. Empty uses the default.
	UpstreamBaseURL string
	// CatalogURL overrides the model manifest. Empty uses the shared default.
	CatalogURL string
	// Host is the CPA adapter. Required.
	Host hostapi.Host
	// Log receives plugin log lines. Optional.
	Log func(hostapi.LogLevel, string, map[string]any)
}

// App is the assembled plugin.
type App struct {
	cfg Config

	db        *storage.DB
	settings  *settings.Manager
	accounts  *accounts.Registry
	models    *models.Registry
	catalog   *models.Catalog
	states    *states.Registry
	proxies   *proxies.Pool
	windows   *probe.WindowManager
	executor  *probe.Executor
	scheduler *probe.Scheduler

	// modelFetcher reads an account's own model catalog from upstream.
	modelFetcher *models.AccountFetcher
	// calls records one row per intercepted request.
	calls *callhistory.Recorder

	corr      *intercept.CorrelationManager
	stats     *intercept.Stats
	injector  *intercept.Injector
	collector *intercept.Collector
	router    *routing.Scheduler
	api       *management.API

	// pairs adapts accounts.EnabledPairs to the probe scheduler's view.
	pairs enabledPairs

	cancel context.CancelFunc
	wg     sync.WaitGroup

	startOnce sync.Once
	stopOnce  sync.Once
}

// New assembles the plugin: opens the database, migrates it, restores every
// persisted subsystem, and wires the services. It does not start background
// work -- call Start for that.
func New(ctx context.Context, cfg Config) (*App, error) {
	if cfg.Host == nil {
		return nil, errors.New("app: Config.Host is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("app: Config.DataDir is required")
	}

	dbPath := filepath.Join(cfg.DataDir, "state.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		return nil, err
	}

	logf := cfg.Log
	if logf == nil {
		logf = func(hostapi.LogLevel, string, map[string]any) {}
	}

	backupDir := filepath.Join(cfg.DataDir, "backups")
	from, err := db.Migrate(ctx, backupDir, func(format string, args ...any) {
		logf(hostapi.LogInfo, fmt.Sprintf(format, args...), nil)
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	logf(hostapi.LogInfo, "database ready", map[string]any{
		"path": dbPath, "fromVersion": from, "schemaVersion": storage.CurrentSchemaVersion,
	})

	a := &App{cfg: cfg, db: db}

	// Settings first: every other subsystem resolves its behaviour from the
	// snapshot this produces.
	a.settings, err = settings.NewManager(ctx, db.Settings(), func(format string, args ...any) {
		logf(hostapi.LogWarn, fmt.Sprintf(format, args...), nil)
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	a.models = models.NewRegistry()
	a.catalog = models.NewCatalog(cfg.CatalogURL, func(format string, args ...any) {
		logf(hostapi.LogInfo, fmt.Sprintf(format, args...), nil)
	})

	// The registry judges state shape for the panel, and the executor and
	// collector do it on the request path. All three read the same fallback, so
	// a change to the setting cannot make the panel disagree with what binds.
	a.accounts = accounts.NewRegistry(accounts.RegistryConfig{
		Host:          cfg.Host,
		Store:         db.AccountModels(),
		ModelStore:    db.ModelLists(),
		CatalogModels: a.catalog.Models,
		Plans:         a,
		TargetLength:  func() int { return a.settings.Current().TargetStateLength },
	})
	a.accounts.SetLogger(func(format string, args ...any) {
		logf(hostapi.LogWarn, fmt.Sprintf(format, args...), nil)
	})
	if err := a.accounts.Load(ctx); err != nil {
		return nil, a.fail(err)
	}
	// The model list comes from upstream rather than from CPA, which exposes no
	// such callback; the composition root owns the credential lookup because
	// only it is allowed to reach the host (NF-06).
	a.modelFetcher = models.NewAccountFetcher(cfg.UpstreamBaseURL)
	a.accounts.SetModelFetcher(a)

	a.states = states.NewRegistry(db.Bindings(), db.Bindings(), a)
	if err := a.states.Load(ctx); err != nil {
		return nil, a.fail(err)
	}
	// An account CPA no longer lists must not keep a binding: the plugin would
	// go on injecting a state value for an account that can never be probed
	// again, so nothing would ever replace it.
	a.accounts.SetAccountRemovalHook(func(ctx context.Context, authIndex string) {
		n, err := a.states.DeleteAccount(ctx, authIndex, states.SourceProbe)
		if err != nil {
			logf(hostapi.LogWarn, "could not drop bindings for a removed account", map[string]any{
				"authIndex": authIndex, "error": err.Error(),
			})
			return
		}
		if n > 0 {
			logf(hostapi.LogInfo, "dropped bindings for an account CPA no longer lists", map[string]any{
				"authIndex": authIndex, "bindings": n,
			})
		}
	})

	a.proxies = proxies.NewPool(db.Proxies())
	if err := a.proxies.Load(ctx); err != nil {
		return nil, a.fail(err)
	}

	a.windows = probe.NewWindowManager(db.Windows())
	if err := a.windows.Load(ctx); err != nil {
		return nil, a.fail(err)
	}

	a.executor = probe.NewExecutor(probe.ExecutorConfig{
		Host:    cfg.Host,
		Pool:    a.proxies,
		Models:  a.models,
		Plans:   a.accounts,
		History: db.Probes(),
		Policy: func() probe.ExecutorPolicy {
			v := a.settings.Current()
			return probe.ExecutorPolicy{
				TargetStateLength: v.TargetStateLength,
				TTL:               v.StateTTL,
				MaxProbeDuration:  v.MaxProbeDuration,
				MaxProxies:        v.MaxProxiesPerProbe,
			}
		},
		BaseURL: cfg.UpstreamBaseURL,
	})

	a.pairs = enabledPairs{registry: a.accounts}
	a.scheduler = probe.NewScheduler(probe.SchedulerConfig{
		Source: a,
		Log:    logf,
	})
	// The upstream reports plan and rate-limit state on the same response the
	// probe reads for the state token, so the account view refreshes itself
	// without anyone asking CPA for data it does not expose.
	a.scheduler.SetSignalFunc(a.accounts.RecordSignals)

	a.corr = intercept.NewCorrelationManager(intercept.DefaultCorrelationTTL)
	a.stats = &intercept.Stats{}
	a.calls = callhistory.New(callhistory.Config{Settings: a.settings, Store: db.Calls()})
	a.injector = intercept.NewInjector(intercept.InjectorConfig{
		Settings: a.settings, States: a.states, Corr: a.corr, Log: logf, Stats: a.stats,
		Calls: a.calls,
	})
	a.collector = intercept.NewCollector(intercept.CollectorConfig{
		Settings: a.settings, States: a.states, Plans: a.accounts,
		Corr: a.corr, Log: logf, Stats: a.stats,
		Calls:   a.calls,
		Signals: a.accounts.RecordSignals,
		// A binding dropped as stale should be refilled by the next scan, not
		// by a probe whose backoff predates the discovery.
		OnStale: a.scheduler.ProbeNow,
	})
	a.router = routing.NewScheduler(routing.SchedulerConfig{
		Settings: a.settings, States: a.states, Cursors: db.Cursors(),
		Models: a, Auth: a.accounts, Log: logf,
	})

	a.api = management.New(a)

	// Adopt any binding that was already past its TTL at startup, so expired
	// rows are not silently injected on the first requests after a restart.
	for _, pair := range a.states.Expired() {
		logf(hostapi.LogInfo, "binding expired while the plugin was stopped", map[string]any{
			"authIndex": pair.AuthIndex, "model": pair.Model,
		})
	}

	return a, nil
}

func (a *App) fail(err error) error {
	_ = a.db.Close()
	return err
}

// Start launches the background subsystems.
//
// The lifetime is deliberately the plugin's own, not the caller's. An earlier
// version accepted a context, and the adapter passed the registration timeout
// into it -- so `defer cancel()` in the registration handler killed the scan
// loop the instant registration returned. Everything looked healthy because the
// management API is request-driven and kept answering; nothing background ever
// ran. Taking no context makes that mistake impossible. Use Stop to end it.
func (a *App) Start() {
	a.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		a.cancel = cancel

		// A first sync populates the account list the panel renders, without
		// waiting for the first scan tick.
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if n, err := a.accounts.Sync(syncCtx); err != nil {
				a.log(hostapi.LogWarn, "initial account sync failed", map[string]any{"error": err.Error()})
			} else {
				a.log(hostapi.LogInfo, "initial account sync complete", map[string]any{"accounts": n})
			}
			a.loadMissingModels(syncCtx)
		}()

		a.scheduler.Start(ctx)
		a.catalog.Start(ctx)

		// The call-history writer owns the only goroutine that touches the
		// table, so the request path never waits on a disk.
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.calls.Run(ctx)
		}()

		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.modelsLoop(ctx)
		}()

		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.pruneLoop(ctx)
		}()
	})
}

// loadMissingModels fetches the model list for every account that has none.
//
// Run right after the first account sync so the panel has real per-account
// lists rather than the shared manifest from the first render onwards.
func (a *App) loadMissingModels(ctx context.Context) {
	for _, authIndex := range a.accounts.MissingModelLists() {
		if ctx.Err() != nil {
			return
		}
		if _, err := a.accounts.LoadModels(ctx, authIndex, false); err != nil {
			a.log(hostapi.LogInfo, "could not load the account model list", map[string]any{
				"authIndex": authIndex, "error": err.Error(),
			})
		}
	}
}

// modelsLoop retries the first-time model load for accounts that did not get
// one, and never re-reads a list it already has.
//
// A separate loop rather than work inside the scan: the first load is a burst of
// outbound requests against each account's own quota, and folding it into the
// scan would put it behind the probe switch and the time window -- so an
// operator who keeps probing switched off would never see a model list at all.
func (a *App) modelsLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.loadMissingModels(ctx)
		}
	}
}

// Stop shuts the plugin down, leaving the database in a clean state.
func (a *App) Stop() {
	a.stopOnce.Do(func() {
		if a.cancel != nil {
			a.cancel()
		}
		a.scheduler.Stop()
		a.wg.Wait()
		if err := a.db.Close(); err != nil {
			a.log(hostapi.LogWarn, "error closing database", map[string]any{"error": err.Error()})
		}
	})
}

// pruneLoop drops history past its retention.
//
// It wakes more often than the retention window so a day-long setting is
// enforced within the hour rather than up to two days late.
func (a *App) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			values := a.settings.Current()
			cutoff := time.Now().Add(-values.ProbeRetention)
			n, err := a.db.Probes().PruneProbesBefore(ctx, cutoff)
			if err != nil {
				a.log(hostapi.LogWarn, "probe history prune failed", map[string]any{"error": err.Error()})
			} else if n > 0 {
				a.log(hostapi.LogInfo, "probe history pruned", map[string]any{
					"rows": n, "olderThan": cutoff.UTC().Format(time.RFC3339),
				})
			}

			callCutoff := time.Now().Add(-values.CallHistoryRetention)
			m, err := a.db.Calls().PruneCallsBefore(ctx, callCutoff)
			if err != nil {
				a.log(hostapi.LogWarn, "call history prune failed", map[string]any{"error": err.Error()})
			} else if m > 0 {
				a.log(hostapi.LogInfo, "call history pruned", map[string]any{
					"rows": m, "olderThan": callCutoff.UTC().Format(time.RFC3339),
				})
			}
			// Requests that never reached a terminal state would otherwise sit
			// in the recorder's map forever.
			a.calls.Sweep()
		}
	}
}

// ---------------------------------------------------------------------------
// Accessors

// Settings returns the settings manager.
func (a *App) Settings() *settings.Manager { return a.settings }

// Accounts returns the account registry.
func (a *App) Accounts() *accounts.Registry { return a.accounts }

// Models returns the model capability registry.
func (a *App) Models() *models.Registry { return a.models }

// Catalog returns the account model list source.
func (a *App) Catalog() *models.Catalog { return a.catalog }

// PipelineStats exposes the request-pipeline counters.
//
// Counters rather than log lines: a header rewrite leaves no other trace, so
// this is how the panel can answer "has the plugin ever actually injected?" --
// and unlike per-request logging it stays affordable in production.
func (a *App) PipelineStats() intercept.StatsSnapshot { return a.stats.Snapshot() }

// ReadPlan implements accounts.PlanReader.
//
// It reads the credential to reach the id_token's plan claim, which is the same
// source CLIProxyAPI uses. The document is used and discarded: only the derived
// tier is kept, never the token (NF-06).
func (a *App) ReadPlan(ctx context.Context, authIndex string) (accounts.Plan, error) {
	cred, err := a.cfg.Host.GetCredential(ctx, authIndex)
	if err != nil {
		return accounts.Plan{}, err
	}
	return accounts.ParsePlanFromCredential(cred.Raw), nil
}

// States returns the binding registry.
func (a *App) States() *states.Registry { return a.states }

// Proxies returns the proxy pool.
func (a *App) Proxies() *proxies.Pool { return a.proxies }

// Windows returns the time-window manager.
func (a *App) Windows() *probe.WindowManager { return a.windows }

// ProbeHistory returns the probe history store.
func (a *App) ProbeHistory() probe.HistoryStore { return a.db.Probes() }

// ProbeScheduler returns the probe scheduler.
func (a *App) ProbeScheduler() *probe.Scheduler { return a.scheduler }

// DB exposes the database for diagnostics and the backup endpoints.
func (a *App) DB() *storage.DB { return a.db }

// ---------------------------------------------------------------------------
// Interfaces satisfied for other packages

// StatePolicy implements states.PolicyProvider.
func (a *App) StatePolicy() states.Policy {
	v := a.settings.Current()
	return states.Policy{
		TTL:                 v.StateTTL,
		RefreshThresholdPct: v.RefreshThresholdPct,
		TargetStateLength:   v.TargetStateLength,
	}
}

// ModelTurnStateEnabled implements routing.ModelPolicy.
//
// A model participates if any account has probing switched on for it, or if
// the plugin holds a binding for it. The second clause matters when probing is
// off and state was accumulated from traffic.
func (a *App) ModelTurnStateEnabled(model string) bool {
	for _, cfg := range a.accounts.EnabledPairs() {
		if cfg.Model == model {
			return true
		}
	}
	for pair := range a.states.All() {
		if pair.Model == model {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// probe.ConfigSource
//
// These accessors carry distinct names because the plugin's public accessors
// above already claim Settings/Accounts/States with different return types.

// SettingsManager implements probe.ConfigSource.
func (a *App) SettingsManager() *settings.Manager { return a.settings }

// EnabledPairSource implements probe.ConfigSource.
func (a *App) EnabledPairSource() probe.EnabledPairSource { return a.pairs }

// WindowManager implements probe.ConfigSource.
func (a *App) WindowManager() *probe.WindowManager { return a.windows }

// StateRegistry implements probe.ConfigSource.
func (a *App) StateRegistry() *states.Registry { return a.states }

// ProbeExecutor implements probe.ConfigSource.
func (a *App) ProbeExecutor() *probe.Executor { return a.executor }

// enabledPairs adapts accounts.Registry to probe.EnabledPairSource.
type enabledPairs struct{ registry *accounts.Registry }

// EnabledPairs implements probe.EnabledPairSource.
func (e enabledPairs) EnabledPairs() []states.Pair {
	cfgs := e.registry.EnabledPairs()
	out := make([]states.Pair, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, states.Pair{AuthIndex: c.AuthIndex, Model: c.Model})
	}
	return out
}

// Sync implements probe.EnabledPairSource.
func (e enabledPairs) Sync(ctx context.Context) (int, error) { return e.registry.Sync(ctx) }

// SyncAccount implements probe.EnabledPairSource.
func (e enabledPairs) SyncAccount(ctx context.Context, authIndex string) (bool, error) {
	_, listed, err := e.registry.SyncAccount(ctx, authIndex)
	return listed, err
}

// SyncAccounts pulls the account list from CPA.
func (a *App) SyncAccounts(ctx context.Context) (int, error) { return a.accounts.Sync(ctx) }

// SyncOneAccount refreshes a single account from CPA and, when models is true,
// re-reads its model list from upstream.
//
// The model reload is opt-in because it costs a request against the account's
// own quota; plain "refresh this account's state" is free. This is what the
// panel's per-account sync button calls, and the reason it exists at all: the
// operator can see CPA's current view of an account without waiting for the
// next scheduled sync, which is the only way to confirm that a fix they just
// made in CPA has been picked up.
func (a *App) SyncOneAccount(ctx context.Context, authIndex string, models bool) (accounts.AccountView, error) {
	acc, listed, err := a.accounts.SyncAccount(ctx, authIndex)
	if err != nil {
		return accounts.AccountView{}, err
	}
	if !listed {
		return accounts.AccountView{}, fmt.Errorf("%w: %s", accounts.ErrUnknownAccount, authIndex)
	}
	if models {
		if _, err := a.accounts.LoadModels(ctx, authIndex, true); err != nil {
			// The account refresh succeeded; only the model list did not. Report
			// it as one of the view's fields rather than as a failed sync, so the
			// panel still shows the account it just refreshed.
			a.log(hostapi.LogInfo, "account model reload failed", map[string]any{
				"authIndex": authIndex, "error": err.Error(),
			})
		}
	}

	views := a.accounts.AllWithVerdict(time.Now())
	for _, v := range views {
		if v.AuthIndex == authIndex {
			return v, nil
		}
	}
	return accounts.AccountView{Account: acc}, nil
}

// ModelLists reports the model list held for an account.
func (a *App) ModelList(authIndex string) (accounts.ModelList, bool) {
	return a.accounts.ModelList(authIndex)
}

// FetchModels implements accounts.ModelFetcher.
//
// The credential is read immediately before the call and discarded with it
// (NF-06); only the derived model ids are kept.
func (a *App) FetchModels(ctx context.Context, authIndex string) ([]string, error) {
	cred, err := a.cfg.Host.GetCredential(ctx, authIndex)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return nil, errors.New("app: credential has no access_token")
	}
	return a.modelFetcher.List(ctx, cred.AccessToken, accounts.ParseAccountIDFromCredential(cred.Raw))
}

// CallHistory returns the call-history store.
func (a *App) CallHistory() callhistory.Store { return a.db.Calls() }

// CallRecorder exposes the recorder for diagnostics.
func (a *App) CallRecorder() *callhistory.Recorder { return a.calls }

// TriggerProbe runs one on-demand probe for a pair, using a single node.
//
// Deliberately not a full traversal: the operator is asking what this pair does
// right now, and walking the pool would take minutes and answer a different
// question. The outcome is reported rather than treated as an error -- a
// non-target length is a legitimate answer to "what happens if I probe this".
//
// The probe toggle is not consulted. That switch decides what the *scheduler*
// may do unattended; an explicit click is a different kind of consent, and a
// pair whose scheduled probing is off is exactly the pair someone would want to
// test by hand.
func (a *App) TriggerProbe(ctx context.Context, authIndex, model string) (probe.Result, error) {
	v := a.settings.Current()
	if !v.Capabilities().Probe {
		return probe.Result{}, errors.New("app: probing is disabled")
	}
	modelStates, ok := a.accounts.Models(authIndex)
	if !ok {
		return probe.Result{}, fmt.Errorf("app: unknown account %q", authIndex)
	}
	known := false
	for _, ms := range modelStates {
		if ms.Model == model {
			known = true
			break
		}
	}
	if !known {
		return probe.Result{}, fmt.Errorf("app: model %q is not offered by account %q", model, authIndex)
	}
	return a.executor.ProbeOnce(ctx, authIndex, model), nil
}

// ManagementAPI returns the Management API handler.
func (a *App) ManagementAPI() *management.API { return a.api }

// Handler mounts the Management API and the embedded panel on one mux.
func (a *App) Handler(assets http.Handler) http.Handler {
	mux := http.NewServeMux()
	a.api.Register(mux)
	if assets != nil {
		// The harness is the only caller that mounts these. Issue the bare-base
		// redirect here rather than inside the asset handler: StripPrefix
		// rewrites r.URL.Path to "/", so a relative redirect computed at that
		// layer would resolve against the harness root instead of this mount.
		mux.HandleFunc("GET "+version.ResourceBasePath+"/{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, version.ResourceBasePath+"/index.html", http.StatusFound)
		})
		mux.Handle(version.ResourceBasePath+"/", http.StripPrefix(version.ResourceBasePath, assets))
	}
	return mux
}

func (a *App) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if a.cfg.Log != nil {
		a.cfg.Log(level, msg, fields)
	}
}
