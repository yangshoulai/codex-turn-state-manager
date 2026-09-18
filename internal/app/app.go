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
	"sync"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/accounts"
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
	states    *states.Registry
	proxies   *proxies.Pool
	windows   *probe.WindowManager
	executor  *probe.Executor
	scheduler *probe.Scheduler

	corr      *intercept.CorrelationManager
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

	a.accounts = accounts.NewRegistry(cfg.Host, db.AccountModels(), func() []string {
		entries := a.models.All()
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.Probeable {
				out = append(out, e.Model)
			}
		}
		return out
	})
	if err := a.accounts.Load(ctx); err != nil {
		return nil, a.fail(err)
	}

	a.states = states.NewRegistry(db.Bindings(), db.Bindings(), a)
	if err := a.states.Load(ctx); err != nil {
		return nil, a.fail(err)
	}

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
		History: db.Probes(),
		Policy: func() probe.ExecutorPolicy {
			v := a.settings.Current()
			return probe.ExecutorPolicy{
				TargetStateLength: v.TargetStateLength,
				MaxProbeDuration:  v.MaxProbeDuration,
			}
		},
		BaseURL: cfg.UpstreamBaseURL,
	})

	a.pairs = enabledPairs{registry: a.accounts}
	a.scheduler = probe.NewScheduler(probe.SchedulerConfig{
		Source: a,
		Log:    logf,
	})

	a.corr = intercept.NewCorrelationManager(intercept.DefaultCorrelationTTL)
	a.injector = intercept.NewInjector(intercept.InjectorConfig{
		Settings: a.settings, States: a.states, Auth: a.accounts, Corr: a.corr, Log: logf,
	})
	a.collector = intercept.NewCollector(intercept.CollectorConfig{
		Settings: a.settings, States: a.states, Corr: a.corr, Log: logf,
	})
	a.router = routing.NewScheduler(routing.SchedulerConfig{
		Settings: a.settings, States: a.states, Cursors: db.Cursors(), Models: a, Log: logf,
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
func (a *App) Start(ctx context.Context) {
	a.startOnce.Do(func() {
		ctx, a.cancel = context.WithCancel(ctx)

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
		}()

		a.scheduler.Start(ctx)

		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.pruneLoop(ctx)
		}()
	})
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

// pruneLoop keeps probe_history bounded.
func (a *App) pruneLoop(ctx context.Context) {
	const (
		interval = time.Hour
		keepRows = 20000
	)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := a.db.Probes().PruneProbes(ctx, keepRows); err != nil {
				a.log(hostapi.LogWarn, "probe history prune failed", map[string]any{"error": err.Error()})
			} else if n > 0 {
				a.log(hostapi.LogDebug, "probe history pruned", map[string]any{"rows": n})
			}
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

// SyncAccounts pulls the account list from CPA.
func (a *App) SyncAccounts(ctx context.Context) (int, error) { return a.accounts.Sync(ctx) }

// TriggerProbe runs an on-demand probe for one pair.
func (a *App) TriggerProbe(ctx context.Context, authIndex, model string) error {
	v := a.settings.Current()
	if !v.Capabilities().Probe {
		return errors.New("app: probing is disabled")
	}
	if !a.accounts.ProbeEnabled(authIndex, model) {
		return fmt.Errorf("app: probing is not enabled for %s/%s", authIndex, model)
	}
	result := a.executor.Probe(ctx, authIndex, model)
	if result.Err != nil {
		return fmt.Errorf("app: probe %s/%s: %w (%s)", authIndex, model, result.Err, result.Outcome)
	}
	if !result.Succeeded() {
		return fmt.Errorf("app: probe %s/%s finished with %s", authIndex, model, result.Outcome)
	}
	return nil
}

// ManagementAPI returns the Management API handler.
func (a *App) ManagementAPI() *management.API { return a.api }

// Handler mounts the Management API and the embedded panel on one mux.
func (a *App) Handler(assets http.Handler) http.Handler {
	mux := http.NewServeMux()
	a.api.Register(mux)
	if assets != nil {
		mux.Handle(version.ResourceBasePath+"/", http.StripPrefix(version.ResourceBasePath+"/", assets))
	}
	return mux
}

func (a *App) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if a.cfg.Log != nil {
		a.cfg.Log(level, msg, fields)
	}
}
