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

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
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
	// StatusMessage is CPA's own explanation when it has one.
	StatusMessage string `json:"statusMessage,omitempty"`
	Priority      int    `json:"priority"`
	Disabled      bool   `json:"disabled"`
	// Unavailable marks transient provider unavailability, quota exhaustion
	// being the case that matters here.
	Unavailable bool `json:"unavailable,omitempty"`
	// NextRetryAfter is when CPA considers another attempt worthwhile.
	NextRetryAfter *time.Time `json:"nextRetryAfter,omitempty"`

	// Plan is the subscription tier. It comes from the upstream's response
	// headers when traffic has been seen, falling back to the credential's
	// id_token claim, which is frequently absent.
	Plan Plan `json:"plan"`

	// Quota is the rate-limit state the upstream last reported.
	Quota *headers.Signals `json:"quota,omitempty"`

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
	// plans reads an account's subscription tier. Optional: without it the
	// panel simply shows no plan.
	plans PlanReader

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

// PlanReader reads an account's subscription tier from its credential.
type PlanReader interface {
	ReadPlan(ctx context.Context, authIndex string) (Plan, error)
}

// NewRegistry builds an empty registry. Call Load then Sync.
func NewRegistry(host hostapi.Host, store ConfigStore, catalogModels func() []string, plans PlanReader) *Registry {
	if catalogModels == nil {
		catalogModels = func() []string { return nil }
	}
	return &Registry{
		host:          host,
		store:         store,
		catalogModels: catalogModels,
		plans:         plans,
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
			AuthIndex:     a.AuthIndex,
			AuthID:        a.AuthID,
			Label:         a.Label,
			Status:        a.Status,
			StatusMessage: a.StatusMessage,
			Priority:      a.Priority,
			Disabled:      a.Disabled,
			Unavailable:   a.Unavailable,
			SyncedAt:      now,
		}
		if !a.NextRetryAfter.IsZero() {
			retryAfter := a.NextRetryAfter
			synced.NextRetryAfter = &retryAfter
		}
		next[a.AuthIndex] = synced
		if a.AuthID != "" {
			nextAuthID[a.AuthID] = a.AuthIndex
		}
	}

	r.mu.Lock()
	// Preserve the first-seen time for accounts that are still present, so the
	// panel can show a stable "known since", and carry over a plan already
	// looked up so it is not re-read on every sync.
	for idx, acc := range next {
		if prev, ok := r.byIndex[idx]; ok {
			acc.SyncedAt = prev.SyncedAt
			acc.Plan = prev.Plan
			next[idx] = acc
		}
	}
	r.byIndex = next
	r.byAuthID = nextAuthID

	missing := make([]string, 0, len(next))
	for idx, acc := range next {
		if !acc.Plan.Known() {
			missing = append(missing, idx)
		}
	}
	r.mu.Unlock()

	r.resolvePlans(ctx, missing)
	return count, nil
}

// resolvePlans fills in plans for accounts that do not have one yet.
//
// The lookup reads a credential, so it runs once per account per process rather
// than on every sync: a plan changes rarely, and a restart re-reads it. Only the
// derived tier is kept -- the credential document is discarded immediately and
// is never cached, logged, or persisted (NF-06).
func (r *Registry) resolvePlans(ctx context.Context, authIndexes []string) {
	if r.plans == nil || len(authIndexes) == 0 {
		return
	}
	for _, authIndex := range authIndexes {
		if ctx.Err() != nil {
			return
		}
		plan, err := r.plans.ReadPlan(ctx, authIndex)
		if err != nil || !plan.Known() {
			continue
		}
		r.mu.Lock()
		if acc, ok := r.byIndex[authIndex]; ok {
			acc.Plan = plan
			r.byIndex[authIndex] = acc
		}
		r.mu.Unlock()
	}
}

// AllWithBlockReason returns tracked accounts paired with the reason probing is
// being skipped, so the panel can show why an account is idle.
func (r *Registry) AllWithBlockReason(now time.Time) []AccountView {
	accounts := r.All()
	out := make([]AccountView, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, AccountView{Account: acc, BlockedReason: BlockedReason(acc, now)})
	}
	return out
}

// AccountView is an account plus the derived reason it is not being probed.
type AccountView struct {
	Account
	BlockedReason string `json:"blockedReason,omitempty"`
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

// RecordSignals stores the account state the upstream reported.
//
// Called from the probe path, so it runs on whatever goroutine finished a
// probe; the write is guarded like every other registry mutation.
func (r *Registry) RecordSignals(authIndex string, signals headers.Signals) {
	if signals.Empty() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	acc, ok := r.byIndex[authIndex]
	if !ok {
		return
	}
	stored := signals
	acc.Quota = &stored
	if signals.PlanType != "" {
		acc.Plan = Plan{Type: signals.PlanType, ActiveUntil: acc.Plan.ActiveUntil}
	}
	r.byIndex[authIndex] = acc
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
//
// Accounts CPA reports as unhealthy are filtered out here, which is the single
// gate the probe scheduler draws from: an account that is disabled, refreshing,
// awaiting MFA, or cooling down after a quota rejection is not worth a request.
func (r *Registry) EnabledPairs() []Config {
	now := time.Now()

	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Config, 0, len(r.configs))
	for k, enabled := range r.configs {
		if !enabled {
			continue
		}
		acc, ok := r.byIndex[k.authIndex]
		if !ok || BlockedReason(acc, now) != "" {
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

// BlockedReason explains why an account should not be probed, or returns an
// empty string when it should.
//
// CPA already knows far more about an account's health than the plugin can
// infer, and it reports it through host.auth.list: whether the account is
// disabled, mid-refresh, waiting on an external step, or temporarily
// unavailable because the provider said so -- quota exhaustion being the case
// that matters here. Probing an account in any of those states spends a request
// to learn what CPA already knew, and in the quota case it spends part of the
// very budget that is exhausted.
//
// A blocked account is never dropped from the panel: the operator needs to see
// that it exists and why it is idle.
func BlockedReason(a Account, now time.Time) string {
	switch {
	case a.Disabled:
		return "账号已禁用"
	case a.Status == hostapi.AccountStatusDisabled:
		return "账号已禁用"
	case a.Unavailable:
		if a.StatusMessage != "" {
			return "账号暂时不可用：" + a.StatusMessage
		}
		return "账号暂时不可用（可能已达额度上限）"
	case a.NextRetryAfter != nil && now.Before(*a.NextRetryAfter):
		return "账号冷却中，可重试于 " + a.NextRetryAfter.Local().Format("15:04:05")
	case a.Status == hostapi.AccountStatusError:
		if a.StatusMessage != "" {
			return "账号处于错误状态：" + a.StatusMessage
		}
		return "账号处于错误状态"
	case a.Status == hostapi.AccountStatusPending:
		return "账号等待外部操作（如 MFA）"
	case a.Status == hostapi.AccountStatusRefreshing:
		return "账号正在刷新凭证"
	case a.Status == hostapi.AccountStatusUnknown || a.Status == "":
		// An unknown state is not evidence of a fault. Probing costs one cheap
		// request and is how the state becomes known.
		return ""
	}
	return ""
}

// ProbeBlocked reports whether probing should be skipped for an account.
func (r *Registry) ProbeBlocked(authIndex string, now time.Time) (bool, string) {
	acc, ok := r.Get(authIndex)
	if !ok {
		return true, "账号不在 CPA 的账号池中"
	}
	reason := BlockedReason(acc, now)
	return reason != "", reason
}

// ModelState is one row of the panel's per-account model table.
type ModelState struct {
	Model        string `json:"model"`
	ProbeEnabled bool   `json:"probeEnabled"`
}
