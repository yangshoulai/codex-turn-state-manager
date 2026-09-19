// Package states owns turn-state bindings: the in-memory hot-path snapshot,
// the binding lifecycle, and the history trail the panel renders.
package states

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Source records where a state value came from.
type Source string

const (
	SourceProbe   Source = "probe"
	SourceTraffic Source = "traffic"
	SourceManual  Source = "manual"
)

// Action describes what a bind operation did. It is the value persisted in
// state_binding_history.action.
type Action string

const (
	// ActionBound means a pair went from unbound to bound.
	ActionBound Action = "bound"
	// ActionReplaced means an existing binding was overwritten with a
	// different value.
	ActionReplaced Action = "replaced"
	// ActionRefreshed means the same value was seen again and only its TTL was
	// extended. Refreshes are not written to history -- traffic would
	// otherwise flood the table (design doc 3.9).
	ActionRefreshed Action = "refreshed"
	// ActionDeleted means the binding was removed.
	ActionDeleted Action = "deleted"
)

// Pair identifies a binding. AuthIndex is the plugin's persistence key; see
// the naming convention in AGENTS.md.
type Pair struct {
	AuthIndex string `json:"authIndex"`
	Model     string `json:"model"`
}

// ErrStateExpired means the value's own issue time puts it beyond its TTL, so
// binding it would only displace something still usable.
var ErrStateExpired = errors.New("states: state value is already past its ttl")

// Binding is one turn-state value bound to a (authIndex, model) pair.
type Binding struct {
	Pair
	StateValue  string `json:"stateValue"`
	StateLength int    `json:"stateLength"`
	// IssuedAt is when the upstream minted the value, read from its envelope.
	// Zero when the envelope was unreadable. It is the anchor the expiry is
	// computed from and is not persisted: the derived timestamps are.
	IssuedAt     time.Time `json:"issuedAt,omitzero"`
	BoundAt      time.Time `json:"boundAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
	RefreshAfter time.Time `json:"refreshAfter"`
	Source       Source    `json:"source"`
	ProxyID      string    `json:"proxyId,omitempty"`
}

// Status is the binding lifecycle state.
type Status int

const (
	// StatusMissing means no binding exists for the pair.
	StatusMissing Status = iota
	// StatusFresh means more than the refresh threshold of the TTL remains.
	StatusFresh
	// StatusRefreshDue means still valid but inside the refresh threshold.
	StatusRefreshDue
	// StatusExpired means the TTL has elapsed.
	StatusExpired
)

// String renders the status for the API and the panel.
func (s Status) String() string {
	switch s {
	case StatusFresh:
		return "fresh"
	case StatusRefreshDue:
		return "refresh_due"
	case StatusExpired:
		return "expired"
	default:
		return "missing"
	}
}

// Usable reports whether the state may still be injected. RefreshDue counts as
// usable: it is a hint to probe early, not an instruction to stop using it.
func (s Status) Usable() bool { return s == StatusFresh || s == StatusRefreshDue }

// NeedsRefresh reports whether a background probe should be scheduled.
func (s Status) NeedsRefresh() bool { return s == StatusRefreshDue }

// HistoryEntry is one row of state_binding_history.
type HistoryEntry struct {
	ID int64 `json:"id"`
	Pair
	StateValue  string    `json:"stateValue"`
	StateLength int       `json:"stateLength"`
	Source      Source    `json:"source"`
	Action      Action    `json:"action"`
	BoundAt     time.Time `json:"boundAt"`
	ExpiresAt   time.Time `json:"expiresAt,omitempty"`
	ProxyID     string    `json:"proxyId,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Store persists current bindings. Implemented by storage.BindingStore.
type Store interface {
	UpsertBinding(ctx context.Context, b Binding) error
	DeleteBinding(ctx context.Context, p Pair) error
	ListBindings(ctx context.Context) ([]Binding, error)
}

// HistoryStore persists the binding history trail.
type HistoryStore interface {
	AppendHistory(ctx context.Context, e HistoryEntry) error
	ListHistory(ctx context.Context, p Pair, limit, offset int) ([]HistoryEntry, error)
	ClearHistory(ctx context.Context, p Pair) (int64, error)
}

// Policy carries the tunables the registry needs, resolved from settings at
// call time so a TTL change applies to the next binding without a restart.
type Policy struct {
	TTL                 time.Duration
	RefreshThresholdPct int
	TargetStateLength   int
}

// Expiry derives the absolute timestamps a binding gets.
//
// The clock starts when the upstream minted the value, not when we got round to
// storing it, whenever that is known. A value that arrives already part-way
// through its life would otherwise be handed a full TTL and kept alive past the
// point the upstream stopped honouring it -- which reads as "it worked, then it
// stopped working" with nothing in between.
//
// issued may be zero, which is the case for a value whose envelope we cannot
// read; the binding is then anchored on boundAt as it was before.
func (p Policy) Expiry(issued, boundAt time.Time) (expiresAt, refreshAfter time.Time) {
	anchor := boundAt
	if !issued.IsZero() && issued.Before(boundAt) {
		anchor = issued
	}
	ttl := p.TTL
	expiresAt = anchor.Add(ttl)
	refreshAfter = anchor.Add(time.Duration(float64(ttl) * (1 - float64(p.RefreshThresholdPct)/100)))
	return expiresAt, refreshAfter
}

// AcceptsLength reports whether a state value is the configured target length.
// Non-target lengths are recorded in probe history but never bound (F-15).
func (p Policy) AcceptsLength(n int) bool { return n == p.TargetStateLength }

// PolicyProvider yields the current policy. Implemented by the app, backed by
// the settings snapshot.
type PolicyProvider interface {
	StatePolicy() Policy
}

// Registry is the binding store plus its hot-path snapshot.
type Registry struct {
	store   Store
	history HistoryStore
	policy  PolicyProvider

	// now is injectable so tests can drive the lifecycle deterministically.
	now func() time.Time

	mu   sync.Mutex
	snap atomic.Pointer[map[Pair]Binding]
}

// NewRegistry builds an empty registry. Call Load to populate the snapshot.
func NewRegistry(store Store, history HistoryStore, policy PolicyProvider) *Registry {
	r := &Registry{store: store, history: history, policy: policy, now: time.Now}
	empty := map[Pair]Binding{}
	r.snap.Store(&empty)
	return r
}

// SetClock overrides the time source. Tests only.
func (r *Registry) SetClock(now func() time.Time) { r.now = now }

// Load rebuilds the in-memory snapshot from the store. Called once at startup
// so a CPA restart resumes with every binding intact (NF-04).
func (r *Registry) Load(ctx context.Context) error {
	bindings, err := r.store.ListBindings(ctx)
	if err != nil {
		return err
	}
	r.replaceSnapshot(bindings)
	return nil
}

// Lookup returns the binding for a pair and its status as of now. This is the
// hot path: one atomic load plus a map lookup, no locks and no database
// access (NF-01).
func (r *Registry) Lookup(authIndex, model string) (Binding, Status) {
	snap := r.snap.Load()
	b, ok := (*snap)[Pair{AuthIndex: authIndex, Model: model}]
	if !ok {
		return Binding{}, StatusMissing
	}
	return b, r.StatusOf(b, r.now())
}

// StatusOf classifies a binding at a given instant.
func (r *Registry) StatusOf(b Binding, at time.Time) Status {
	switch {
	case b.StateValue == "":
		return StatusMissing
	case !at.Before(b.ExpiresAt):
		return StatusExpired
	case !at.Before(b.RefreshAfter):
		return StatusRefreshDue
	default:
		return StatusFresh
	}
}

// All returns every binding with its current status.
func (r *Registry) All() map[Pair]Binding {
	snap := r.snap.Load()
	out := make(map[Pair]Binding, len(*snap))
	for k, v := range *snap {
		out[k] = v
	}
	return out
}

// Bind records a state value for a pair, extending its TTL.
//
// The action is derived from the previous binding: none -> bound, different
// value -> replaced, identical value -> refreshed. Only bound and replaced
// append a history row.
func (r *Registry) Bind(ctx context.Context, b Binding) (Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	policy := r.policy.StatePolicy()
	now := r.now()

	if b.BoundAt.IsZero() {
		b.BoundAt = now
	}
	b.StateLength = len(b.StateValue)
	b.ExpiresAt, b.RefreshAfter = policy.Expiry(b.IssuedAt, b.BoundAt)

	// A value whose life has already run out must not replace one that still
	// has some. The caller is expected to have noticed before getting here, but
	// this is the rule's home and a binding that is born expired is never what
	// anyone meant.
	if !b.ExpiresAt.After(now) {
		return "", ErrStateExpired
	}

	prev, existed := (*r.snap.Load())[b.Pair]
	action := ActionBound
	switch {
	case existed && prev.StateValue == b.StateValue:
		action = ActionRefreshed
	case existed:
		action = ActionReplaced
	}

	if err := r.store.UpsertBinding(ctx, b); err != nil {
		return "", err
	}
	if action != ActionRefreshed {
		if err := r.history.AppendHistory(ctx, HistoryEntry{
			Pair:        b.Pair,
			StateValue:  b.StateValue,
			StateLength: b.StateLength,
			Source:      b.Source,
			Action:      action,
			BoundAt:     b.BoundAt,
			ExpiresAt:   b.ExpiresAt,
			ProxyID:     b.ProxyID,
			CreatedAt:   now,
		}); err != nil {
			return action, err
		}
	}
	r.putSnapshot(b)
	return action, nil
}

// Delete removes a binding and records the deletion (design doc 5.4).
func (r *Registry) Delete(ctx context.Context, p Pair, source Source) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	prev, existed := (*r.snap.Load())[p]

	if err := r.store.DeleteBinding(ctx, p); err != nil {
		return err
	}
	if existed {
		if err := r.history.AppendHistory(ctx, HistoryEntry{
			Pair:        p,
			StateValue:  prev.StateValue,
			StateLength: prev.StateLength,
			Source:      source,
			Action:      ActionDeleted,
			BoundAt:     prev.BoundAt,
			ExpiresAt:   prev.ExpiresAt,
			ProxyID:     prev.ProxyID,
			CreatedAt:   r.now(),
		}); err != nil {
			return err
		}
	}
	r.deleteSnapshot(p)
	return nil
}

// DeleteAccount drops every binding held for an account.
//
// Called when CPA no longer lists the account. Leaving the bindings behind
// would keep injecting a state value for an account that cannot serve the
// request -- and worse, one the plugin would never probe again, so the value
// would sit there until its TTL lapsed with nothing to replace it.
func (r *Registry) DeleteAccount(ctx context.Context, authIndex string, source Source) (int, error) {
	var pairs []Pair
	for p := range *r.snap.Load() {
		if p.AuthIndex == authIndex {
			pairs = append(pairs, p)
		}
	}
	var deleted int
	for _, p := range pairs {
		if err := r.Delete(ctx, p, source); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// Invalidate drops a binding after a request carrying it failed, so a bad
// value cannot keep poisoning traffic for the rest of its TTL (design doc
// 3.12). It is Delete with source=probe and no history-sourced blame.
func (r *Registry) Invalidate(ctx context.Context, p Pair) error {
	return r.Delete(ctx, p, SourceProbe)
}

// Expired returns the pairs whose TTL has elapsed but which are still present
// in the snapshot. The scheduler uses this to prioritise re-probing.
func (r *Registry) Expired() []Pair {
	now := r.now()
	var out []Pair
	for p, b := range *r.snap.Load() {
		if !now.Before(b.ExpiresAt) {
			out = append(out, p)
		}
	}
	return out
}

// HistoryStore exposes the history store so the Management API can page
// through it without the registry growing query methods of its own.
func (r *Registry) HistoryStore() HistoryStore { return r.history }

func (r *Registry) replaceSnapshot(bindings []Binding) {
	next := make(map[Pair]Binding, len(bindings))
	for _, b := range bindings {
		next[b.Pair] = b
	}
	// Copy-on-write: readers holding the old map keep a consistent view.
	r.snap.Store(&next)
}

func (r *Registry) putSnapshot(b Binding) {
	// Copy-on-write keeps the snapshot immutable for concurrent readers, which
	// is what makes the lock-free Lookup safe.
	prev := r.snap.Load()
	next := make(map[Pair]Binding, len(*prev)+1)
	for k, v := range *prev {
		next[k] = v
	}
	next[b.Pair] = b
	r.snap.Store(&next)
}

func (r *Registry) deleteSnapshot(p Pair) {
	prev := r.snap.Load()
	if _, ok := (*prev)[p]; !ok {
		return
	}
	next := make(map[Pair]Binding, len(*prev))
	for k, v := range *prev {
		if k != p {
			next[k] = v
		}
	}
	r.snap.Store(&next)
}
