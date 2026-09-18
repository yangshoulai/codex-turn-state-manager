// Package accounts tracks CPA's Codex accounts and the per-(account, model)
// probe configuration.
//
// CPA exposes two identities for the same account: AuthIndex, which is stable
// and is what the plugin persists, and AuthID, which is a runtime handle used
// by the scheduler. This registry owns the mapping between them (design doc
// 9.1).
package accounts

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
)

// Config is the per-(account, model) probe toggle.
type Config struct {
	AuthIndex    string `json:"authIndex"`
	Model        string `json:"model"`
	ProbeEnabled bool   `json:"probeEnabled"`
}

// ConfigStore persists probe toggles.
type ConfigStore interface {
	ListAccountModels(ctx context.Context) ([]Config, error)
	UpsertAccountModel(ctx context.Context, c Config) error
	DeleteAccountModel(ctx context.Context, authIndex, model string) error
}

// Account is a tracked Codex account.
type Account struct {
	AuthIndex string `json:"authIndex"`
	AuthID    string `json:"authId"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Priority  int    `json:"priority"`
	Disabled  bool   `json:"disabled"`

	SyncedAt time.Time `json:"syncedAt"`
}

// Registry caches CPA's account list and the per-pair probe toggles.
type Registry struct {
	host  hostapi.Host
	store ConfigStore
	// catalogModels supplies the account model list, refreshed from the same
	// manifest CPA syncs. The plugin cannot read CPA's own registry, so this is
	// the nearest authoritative source it can obtain by itself.
	catalogModels func() []string

	mu sync.RWMutex
	// byIndex is keyed by AuthIndex; byAuthID is the reverse mapping the
	// scheduler needs when CPA hands back an AuthID.
	byIndex  map[string]Account
	byAuthID map[string]string
	configs  map[modelKey]bool
}

type modelKey struct {
	authIndex string
	model     string
}

// NewRegistry builds an empty registry. Call Load then Sync.
func NewRegistry(host hostapi.Host, store ConfigStore, catalogModels func() []string) *Registry {
	if catalogModels == nil {
		catalogModels = func() []string { return nil }
	}
	return &Registry{
		host:          host,
		store:         store,
		catalogModels: catalogModels,
		byIndex:       map[string]Account{},
		byAuthID:      map[string]string{},
		configs:       map[modelKey]bool{},
	}
}

// Load restores persisted probe toggles.
func (r *Registry) Load(ctx context.Context) error {
	cfgs, err := r.store.ListAccountModels(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configs = make(map[modelKey]bool, len(cfgs))
	for _, c := range cfgs {
		r.configs[modelKey{c.AuthIndex, c.Model}] = c.ProbeEnabled
	}
	return nil
}

// Sync pulls the current Codex account list from CPA.
//
// Accounts that disappeared from CPA are dropped; accounts that are new start
// with every pair disabled so nothing is probed until an operator opts in.
func (r *Registry) Sync(ctx context.Context) (int, error) {
	list, err := r.host.ListAccounts(ctx)
	if err != nil {
		return 0, err
	}

	now := time.Now()

	next := make(map[string]Account, len(list))
	nextAuthID := make(map[string]string, len(list))
	count := 0
	for _, a := range list {
		if a.Provider != hostapi.ProviderCodex {
			continue
		}
		count++
		synced := Account{
			AuthIndex: a.AuthIndex,
			AuthID:    a.AuthID,
			Label:     a.Label,
			Status:    a.Status,
			Priority:  a.Priority,
			Disabled:  a.Disabled,
			SyncedAt:  now,
		}
		next[a.AuthIndex] = synced
		if a.AuthID != "" {
			nextAuthID[a.AuthID] = a.AuthIndex
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Preserve the first-seen time for accounts that are still present, so the
	// panel can show a stable "known since".
	for idx, acc := range next {
		if prev, ok := r.byIndex[idx]; ok {
			acc.SyncedAt = prev.SyncedAt
			next[idx] = acc
		}
	}
	r.byIndex = next
	r.byAuthID = nextAuthID
	return count, nil
}

// All returns tracked accounts sorted by label then authIndex.
func (r *Registry) All() []Account {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Account, 0, len(r.byIndex))
	for _, a := range r.byIndex {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].AuthIndex < out[j].AuthIndex
	})
	return out
}

// Get returns a tracked account by AuthIndex.
func (r *Registry) Get(authIndex string) (Account, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byIndex[authIndex]
	return a, ok
}

// ResolveAuthID maps a CPA runtime AuthID back to the plugin's AuthIndex.
func (r *Registry) ResolveAuthID(authID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	idx, ok := r.byAuthID[authID]
	return idx, ok
}

// AuthID returns the runtime identifier for an AuthIndex.
func (r *Registry) AuthID(authIndex string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byIndex[authIndex]
	return a.AuthID, ok
}

// ProbeEnabled reports whether probing is on for a pair.
func (r *Registry) ProbeEnabled(authIndex, model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.configs[modelKey{authIndex, model}]
}

// CatalogHas reports whether the model list offers a name.
func (r *Registry) CatalogHas(model string) bool {
	for _, m := range r.catalogModels() {
		if m == model {
			return true
		}
	}
	return false
}

// ModelConfigured reports whether a model has an explicit configuration row for
// this account, as opposed to merely appearing in the seed list. Removal needs
// the distinction: dropping a seed entry has to be remembered, or the next sync
// would offer it again.
func (r *Registry) ModelConfigured(authIndex, model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.configs[modelKey{authIndex, model}]
	return ok
}

// SetProbeEnabled flips the probe toggle and persists it.
func (r *Registry) SetProbeEnabled(ctx context.Context, authIndex, model string, enabled bool) error {
	if err := r.store.UpsertAccountModel(ctx, Config{
		AuthIndex:    authIndex,
		Model:        model,
		ProbeEnabled: enabled,
	}); err != nil {
		return err
	}
	r.mu.Lock()
	r.configs[modelKey{authIndex, model}] = enabled
	r.mu.Unlock()
	return nil
}

// ForgetModel clears any stored state for a model, returning the pair to its
// unconfigured default. The model itself stays listed: the catalog is the
// authority on what exists, not the toggle table.
func (r *Registry) ForgetModel(ctx context.Context, authIndex, model string) error {
	if err := r.store.DeleteAccountModel(ctx, authIndex, model); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.configs, modelKey{authIndex, model})
	r.mu.Unlock()
	return nil
}

// EnabledPairs returns every (authIndex, model) with probing switched on.
// Pairs whose account is gone or disabled are skipped.
func (r *Registry) EnabledPairs() []Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Config, 0, len(r.configs))
	for k, enabled := range r.configs {
		if !enabled {
			continue
		}
		acc, ok := r.byIndex[k.authIndex]
		if !ok || acc.Disabled {
			continue
		}
		out = append(out, Config{AuthIndex: k.authIndex, Model: k.model, ProbeEnabled: true})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AuthIndex != out[j].AuthIndex {
			return out[i].AuthIndex < out[j].AuthIndex
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Models returns the model list for an account, with per-model probe toggles.
//
// The list comes from the shared manifest rather than from operator input or a
// hardcoded table. The plugin has no way to read CPA's own model registry, so
// this is the nearest authoritative source: the same file CPA syncs, parsed the
// same way. Which of the models an account can actually serve is answered by
// probing, not by guessing.
func (r *Registry) Models(authIndex string) ([]ModelState, bool) {
	if _, ok := r.Get(authIndex); !ok {
		return nil, false
	}
	models := r.catalogModels()

	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelState, 0, len(models))
	for _, m := range models {
		out = append(out, ModelState{
			Model:        m,
			ProbeEnabled: r.configs[modelKey{authIndex, m}],
		})
	}
	return out, true
}

// ModelState is one row of the panel's per-account model table.
type ModelState struct {
	Model        string `json:"model"`
	ProbeEnabled bool   `json:"probeEnabled"`
}
