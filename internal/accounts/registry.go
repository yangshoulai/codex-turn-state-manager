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
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
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

// Model list sources.
const (
	// ModelSourceUpstream means the list was read from the account's own
	// catalog endpoint.
	ModelSourceUpstream = "upstream"
	// ModelSourceCatalog means it is the shared manifest, used until the
	// account's own list has been fetched and whenever that fetch fails.
	ModelSourceCatalog = "catalog"
)

// ErrUnknownAccount means the registry does not track the account.
//
// A sentinel rather than a formatted string so callers can answer 404 rather
// than a blanket 502: "this account does not exist" is the caller's mistake,
// and reporting it as a bad gateway sends an operator to look at CPA's health
// for what is actually a stale panel row.
var ErrUnknownAccount = errors.New("accounts: unknown account")

// ModelList is the model list held for one account.
type ModelList struct {
	AuthIndex string   `json:"authIndex"`
	Models    []string `json:"models"`
	// Source names where the list came from: "upstream" for the account's own
	// catalog, "catalog" for the shared manifest fallback.
	Source string `json:"source"`
	// SyncedAt is when the list was fetched. Zero for the fallback, which is
	// not fetched per account at all.
	SyncedAt time.Time `json:"syncedAt,omitempty"`
	// Error is the last fetch failure. Kept rather than discarded so the panel
	// can say the list is stale *because* the refresh failed, instead of
	// presenting yesterday's answer as today's.
	Error string `json:"error,omitempty"`
}

// ModelListStore persists per-account model lists.
type ModelListStore interface {
	ListModelLists(ctx context.Context) ([]ModelList, error)
	SaveModelList(ctx context.Context, l ModelList) error
	// DeleteModelList drops an account's list and its probe toggles.
	DeleteModelList(ctx context.Context, authIndex string) error
}

// ModelFetcher loads an account's model list from upstream.
//
// Declared here and implemented by the composition root, which owns the live
// credential lookup and the outbound call. A fetch failure is never fatal: the
// caller keeps whatever list it already had.
type ModelFetcher interface {
	FetchModels(ctx context.Context, authIndex string) ([]string, error)
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
	// modelStore persists the per-account model lists. Optional: without it the
	// registry falls back to the shared catalog for every account, which is the
	// previous behaviour.
	modelStore ModelListStore
	// catalogModels supplies the shared manifest, used until an account's own
	// list has been fetched.
	catalogModels func() []string
	// plans reads an account's subscription tier. Optional: without it the
	// panel simply shows no plan.
	plans PlanReader

	mu sync.RWMutex
	// targetLength supplies the configured fallback state length, used when an
	// account's plan is unknown. A function rather than a cached value because
	// settings are written through the management API, which does not tell this
	// package -- a cached copy would go stale the moment an operator changed it.
	targetLength func() int
	// byIndex is keyed by AuthIndex; byAuthID is the reverse mapping the
	// scheduler needs when CPA hands back an AuthID.
	byIndex  map[string]Account
	byAuthID map[string]string
	configs  map[modelKey]bool
	// modelLists holds each account's loaded catalog, keyed by AuthIndex.
	modelLists map[string]ModelList
	// fetcher loads model lists. Installed by the composition root.
	fetcher ModelFetcher
	// logger receives cleanup failures. Optional.
	logger func(string, ...any)
	// onRemoved is told when an account CPA no longer lists is dropped. The
	// registry cannot clear bindings itself -- it does not own them -- and
	// leaving them behind would keep injecting state for an account that will
	// never be probed again.
	onRemoved func(ctx context.Context, authIndex string)
}

type modelKey struct {
	authIndex string
	model     string
}

// PlanReader reads an account's subscription tier from its credential.
type PlanReader interface {
	ReadPlan(ctx context.Context, authIndex string) (Plan, error)
}

// RegistryConfig assembles a Registry.
//
// A struct rather than a parameter list: the registry has grown past the point
// where positional arguments of mostly-identical types are readable, and a
// mis-ordered call would compile.
type RegistryConfig struct {
	Host hostapi.Host
	// Store persists the per-pair probe toggles. Required.
	Store ConfigStore
	// ModelStore persists the per-account model lists. Optional; without it
	// every account falls back to the shared catalog.
	ModelStore ModelListStore
	// CatalogModels supplies the shared manifest fallback. Optional.
	CatalogModels func() []string
	// Plans reads an account's subscription tier. Optional.
	Plans PlanReader
	// TargetLength supplies the configured fallback state length. Optional.
	TargetLength func() int
}

// NewRegistry builds an empty registry. Call Load then Sync.
func NewRegistry(cfg RegistryConfig) *Registry {
	catalogModels := cfg.CatalogModels
	if catalogModels == nil {
		catalogModels = func() []string { return nil }
	}
	return &Registry{
		host:          cfg.Host,
		store:         cfg.Store,
		modelStore:    cfg.ModelStore,
		catalogModels: catalogModels,
		plans:         cfg.Plans,
		targetLength:  cfg.TargetLength,
		byIndex:       map[string]Account{},
		byAuthID:      map[string]string{},
		configs:       map[modelKey]bool{},
		modelLists:    map[string]ModelList{},
	}
}

// SetModelFetcher installs the upstream model-list loader.
func (r *Registry) SetModelFetcher(f ModelFetcher) {
	r.mu.Lock()
	r.fetcher = f
	r.mu.Unlock()
}

// Load restores persisted probe toggles and model lists.
func (r *Registry) Load(ctx context.Context) error {
	cfgs, err := r.store.ListAccountModels(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.configs = make(map[modelKey]bool, len(cfgs))
	for _, c := range cfgs {
		r.configs[modelKey{c.AuthIndex, c.Model}] = c.ProbeEnabled
	}
	r.mu.Unlock()

	if r.modelStore == nil {
		return nil
	}
	lists, err := r.modelStore.ListModelLists(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.modelLists = make(map[string]ModelList, len(lists))
	for _, l := range lists {
		r.modelLists[l.AuthIndex] = l
	}
	r.mu.Unlock()
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
	count, missing := r.adopt(list, time.Now())
	r.resolvePlans(ctx, missing)
	return count, nil
}

// SyncAccount refreshes one account from CPA and reports whether it still
// exists.
//
// This is the per-probe freshness check the design requires: an account's state
// is CPA's to know and can change at any moment -- the operator re-authorises
// it, it runs out of quota, it is removed -- and a snapshot taken minutes ago is
// not evidence about right now.
//
// The second return value is false only when CPA answered and the account was
// not in the answer. A failed call leaves the cached view alone and reports the
// error: "the host did not answer" is not "the account is gone", and acting on
// that confusion would delete a healthy account's bindings.
func (r *Registry) SyncAccount(ctx context.Context, authIndex string) (Account, bool, error) {
	list, err := r.host.ListAccounts(ctx)
	if err != nil {
		acc, known := r.Get(authIndex)
		return acc, known, err
	}
	r.adopt(list, time.Now())
	acc, known := r.Get(authIndex)
	return acc, known, nil
}

// Forget removes an account the caller has established CPA no longer lists.
//
// Idempotent: forgetting an account the registry does not track does nothing,
// so a caller that repeats the call cannot fire the removal hook twice.
func (r *Registry) Forget(ctx context.Context, authIndex string) {
	r.mu.RLock()
	_, known := r.byIndex[authIndex]
	r.mu.RUnlock()
	if !known {
		return
	}
	r.dropAccount(ctx, authIndex)
}

// dropAccount removes an account and everything keyed by its auth index: the
// cached entry, the model list, the probe toggles, and -- through the removal
// hook -- its bindings. Leaving a binding behind would keep injecting a state
// value for an account that cannot serve the request, and nothing would ever
// replace it, because the account is never probed again.
//
// Unguarded on purpose. The sync path calls this from inside its own critical
// section, after the account map has already been replaced, so a "is it still
// tracked" test there would always answer no and silently skip the cleanup.
func (r *Registry) dropAccount(ctx context.Context, authIndex string) {
	r.mu.Lock()
	delete(r.byIndex, authIndex)
	for authID, idx := range r.byAuthID {
		if idx == authIndex {
			delete(r.byAuthID, authID)
		}
	}
	for key := range r.configs {
		if key.authIndex == authIndex {
			delete(r.configs, key)
		}
	}
	delete(r.modelLists, authIndex)
	hook := r.onRemoved
	r.mu.Unlock()

	if r.modelStore != nil {
		if err := r.modelStore.DeleteModelList(ctx, authIndex); err != nil {
			r.logf("could not drop model list for removed account %s: %v", authIndex, err)
		}
	}
	if hook != nil {
		hook(ctx, authIndex)
	}
}

// adopt replaces the cached account list from a host snapshot and returns how
// many Codex accounts it contained, plus the accounts whose plan is still
// unknown.
func (r *Registry) adopt(list []hostapi.Account, now time.Time) (int, []string) {
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
	var gone []string
	for idx := range r.byIndex {
		if _, still := next[idx]; !still {
			gone = append(gone, idx)
		}
	}
	// Preserve what this build learned from upstream response headers rather
	// than from the host's account list, which carries neither. The plan is
	// looked up from a credential and read once per process; the quota is
	// refreshed by traffic. Both would otherwise be blanked on every sync.
	//
	// SyncedAt is the first-seen time, kept stable so the panel can show a
	// "known since" that does not move.
	for idx, acc := range next {
		if prev, ok := r.byIndex[idx]; ok {
			acc.SyncedAt = prev.SyncedAt
			acc.Plan = prev.Plan
			acc.Quota = prev.Quota
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

	// An account CPA no longer lists is not merely idle: its bindings and
	// probe toggles are dead weight that would be resurrected if the same auth
	// index ever came back with different credentials.
	for _, idx := range gone {
		r.dropAccount(context.Background(), idx)
	}

	return count, missing
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

// TargetStateLength returns the configured fallback length for accounts whose
// plan the upstream has not stated.
func (r *Registry) TargetStateLength() int {
	if r.targetLength == nil {
		return states.EncodedLength(states.PlanBlocks(""))
	}
	if n := r.targetLength(); n > 0 {
		return n
	}
	return states.EncodedLength(states.PlanBlocks(""))
}

// PlanType reports an account's subscription tier as the upstream described it,
// or "" when it has not said.
//
// Read through the two interfaces that judge a state value's shape, so neither
// has to know how a plan is learned -- from traffic headers first, from the
// credential's claim as a fallback.
func (r *Registry) PlanType(authIndex string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	acc, ok := r.byIndex[authIndex]
	if !ok {
		return ""
	}
	return acc.Plan.Type
}

// AllWithVerdict returns tracked accounts paired with the judged reason probing
// is or is not happening, so the panel can say why an account is idle without
// re-deriving the rule.
func (r *Registry) AllWithVerdict(now time.Time) []AccountView {
	accounts := r.All()
	fallback := r.TargetStateLength()
	out := make([]AccountView, 0, len(accounts))
	for _, acc := range accounts {
		expect := states.ExpectationFor(acc.Plan.Type, fallback)
		v := Judge(acc, true, now)
		list, hasList := r.ModelList(acc.AuthIndex)
		out = append(out, AccountView{
			Account:             acc,
			Verdict:             v,
			ExpectedStateBlocks: expect.Blocks,
			ExpectedStateLength: expect.Length,
			ModelSource:         list.Source,
			ModelsSyncedAt:      list.SyncedAt,
			ModelsError:         list.Error,
			ModelCount:          len(list.Models),
			ModelsLoaded:        hasList && list.Source == ModelSourceUpstream,
		})
	}
	return out
}

// AccountView is an account plus what the plugin derived about it.
type AccountView struct {
	Account
	// Verdict is the judged answer to "may this be probed, and why". The panel
	// renders it rather than recomputing the rule from the raw host fields.
	Verdict Verdict `json:"verdict"`
	// ExpectedStateBlocks and ExpectedStateLength are the shape this account's
	// values must have, derived from its plan. Surfaced because the shape varies
	// by tier: a Team account's 332 is a Plus account's wrong length, and an
	// operator comparing the panel against a probe log needs to see which rule
	// is being applied rather than assume 292 for everyone.
	ExpectedStateBlocks int `json:"expectedStateBlocks"`
	ExpectedStateLength int `json:"expectedStateLength"`

	// ModelSource, ModelCount, ModelsSyncedAt and ModelsError describe where
	// this account's model list came from. The panel needs all four: a list
	// that fell back to the shared catalog looks identical to one that was
	// fetched, and "synced 3 days ago with an error since" is a different
	// situation from "synced an hour ago".
	ModelSource    string    `json:"modelSource,omitempty"`
	ModelCount     int       `json:"modelCount"`
	ModelsSyncedAt time.Time `json:"modelsSyncedAt,omitempty"`
	ModelsError    string    `json:"modelsError,omitempty"`
	ModelsLoaded   bool      `json:"modelsLoaded"`
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
// Accounts the verdict refuses are filtered out here, which is the single gate
// the probe scheduler draws from. Note how narrow that gate is: a disabled,
// removed, expired-credential, refreshing or MFA-pending account is skipped,
// while one that is merely rate-limited or in CPA's temporary cooldown is not.
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
		if !ok || Judge(acc, true, now).Blocked {
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
// The list is the account's own, fetched from the catalog endpoint the Codex
// client uses. Until that first fetch lands -- and permanently on a host with
// no egress -- it falls back to the shared manifest, which is the union of
// every tier and therefore an over-approximation for any one account. Which of
// the listed models an account can actually serve is answered by probing, not
// by guessing.
func (r *Registry) Models(authIndex string) ([]ModelState, bool) {
	if _, ok := r.Get(authIndex); !ok {
		return nil, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	models, _ := r.modelsLocked(authIndex)
	out := make([]ModelState, 0, len(models))
	for _, m := range models {
		out = append(out, ModelState{
			Model:        m,
			ProbeEnabled: r.configs[modelKey{authIndex, m}],
		})
	}
	return out, true
}

// modelsLocked resolves an account's list, preferring its own. Callers must
// hold at least a read lock.
func (r *Registry) modelsLocked(authIndex string) ([]string, bool) {
	if list, ok := r.modelLists[authIndex]; ok && len(list.Models) > 0 {
		return list.Models, true
	}
	return r.catalogModels(), false
}

// ModelList returns the stored list for an account and whether it is the
// account's own rather than the shared fallback.
func (r *Registry) ModelList(authIndex string) (ModelList, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list, ok := r.modelLists[authIndex]
	if !ok {
		return ModelList{AuthIndex: authIndex, Models: r.catalogModels(), Source: ModelSourceCatalog}, false
	}
	return list, true
}

// MissingModelLists returns the accounts whose own list has not been fetched.
func (r *Registry) MissingModelLists() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byIndex))
	for idx := range r.byIndex {
		if _, ok := r.modelLists[idx]; !ok {
			out = append(out, idx)
		}
	}
	sort.Strings(out)
	return out
}

// LoadModels fetches and stores one account's model list.
//
// With force false an account that already has a list is skipped: the list is
// read from upstream against the account's own quota and changes rarely, so it
// is loaded once and thereafter only when an operator asks. A failed fetch is
// recorded on the existing entry rather than replacing the list with nothing --
// a stale list is worth more than an empty one.
func (r *Registry) LoadModels(ctx context.Context, authIndex string, force bool) (ModelList, error) {
	r.mu.RLock()
	fetcher := r.fetcher
	existing, has := r.modelLists[authIndex]
	_, known := r.byIndex[authIndex]
	r.mu.RUnlock()

	if !known {
		return ModelList{}, fmt.Errorf("%w %q", ErrUnknownAccount, authIndex)
	}
	if has && !force {
		return existing, nil
	}
	if fetcher == nil {
		return existing, errors.New("accounts: no model fetcher is installed")
	}

	models, err := fetcher.FetchModels(ctx, authIndex)
	if err != nil {
		// Keep the list, record why it is old. The panel shows the error
		// alongside the sync time so "this may be out of date" is visible
		// rather than inferred.
		stale := existing
		stale.AuthIndex = authIndex
		if stale.Models == nil {
			stale.Models = r.catalogModels()
			stale.Source = ModelSourceCatalog
		}
		stale.Error = err.Error()
		stale.SyncedAt = time.Now()
		if r.modelStore != nil {
			// Best effort: failing to record the failure must not turn into a
			// second failure the caller has to handle.
			_ = r.modelStore.SaveModelList(ctx, stale)
		}
		r.mu.Lock()
		r.modelLists[authIndex] = stale
		r.mu.Unlock()
		return stale, err
	}

	list := ModelList{
		AuthIndex: authIndex,
		Models:    models,
		Source:    ModelSourceUpstream,
		SyncedAt:  time.Now(),
	}
	if r.modelStore != nil {
		if err := r.modelStore.SaveModelList(ctx, list); err != nil {
			return list, err
		}
	}
	r.mu.Lock()
	r.modelLists[authIndex] = list
	r.mu.Unlock()
	return list, nil
}

// ModelState is one row of the panel's per-account model table.
type ModelState struct {
	Model        string `json:"model"`
	ProbeEnabled bool   `json:"probeEnabled"`
}

// logf writes a diagnostic line if the composition root installed a logger.
//
// The registry has no logger of its own because the only thing it has to report
// is a persistence failure during cleanup, and dropping a removed account's
// bookkeeping is not worth aborting a sync over.
func (r *Registry) logf(format string, args ...any) {
	if r.logger != nil {
		r.logger(format, args...)
	}
}

// SetAccountRemovalHook installs the callback run when an account is dropped.
func (r *Registry) SetAccountRemovalHook(fn func(ctx context.Context, authIndex string)) {
	r.mu.Lock()
	r.onRemoved = fn
	r.mu.Unlock()
}

// SetLogger installs the diagnostic sink.
func (r *Registry) SetLogger(logf func(string, ...any)) {
	r.mu.Lock()
	r.logger = logf
	r.mu.Unlock()
}
