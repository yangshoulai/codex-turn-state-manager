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
}

// Account is a tracked Codex account.
type Account struct {
	AuthIndex string `json:"authIndex"`
	AuthID    string `json:"authId"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Priority  int    `json:"priority"`
	Disabled  bool   `json:"disabled"`

	// Models is the model list observed for this account, unioned with any
	// model the operator has enabled explicitly.
	Models []string `json:"models"`

	SyncedAt time.Time `json:"syncedAt"`
}

// Registry caches CPA's account list and the per-pair probe toggles.
type Registry struct {
	host  hostapi.Host
	store ConfigStore
	// knownModels supplies the candidate model list per account.
	knownModels func() []string

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
func NewRegistry(host hostapi.Host, store ConfigStore, knownModels func() []string) *Registry {
	if knownModels == nil {
		knownModels = func() []string { return nil }
	}
	return &Registry{
		host:        host,
		store:       store,
		knownModels: knownModels,
		byIndex:     map[string]Account{},
		byAuthID:    map[string]string{},
		configs:     map[modelKey]bool{},
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

	known := r.knownModels()
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
		synced.Models = candidateModels(known)
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
			// Keep operator-enabled models that CPA has not reported yet.
			acc.Models = union(acc.Models, prev.Models)
			next[idx] = acc
		}
	}
	r.byIndex = next
	r.byAuthID = nextAuthID
	return count, nil
}

// candidateModels is the model set offered for each account.
//
// CPA's account record does not enumerate models, so the candidate set is the
// plugin's capability table. Which of them actually work is discovered by
// probing, not by asking CPA.
func candidateModels(known []string) []string {
	out := make([]string, 0, len(known))
	for _, m := range known {
		if m != "" {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
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

// Models returns the model list for an account together with its probe toggles.
func (r *Registry) Models(authIndex string) ([]ModelState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	acc, ok := r.byIndex[authIndex]
	if !ok {
		return nil, false
	}
	out := make([]ModelState, 0, len(acc.Models))
	for _, m := range acc.Models {
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

func union(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
