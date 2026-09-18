package routing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// ---------------------------------------------------------------------------
// doubles

type memSettingsStore struct {
	mu     sync.Mutex
	values map[string]string
}

func (s *memSettingsStore) All(context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}

func (s *memSettingsStore) PutMany(_ context.Context, values map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range values {
		s.values[k] = v
	}
	return nil
}

type memStateStore struct {
	mu       sync.Mutex
	bindings map[states.Pair]states.Binding
}

func newMemStateStore() *memStateStore {
	return &memStateStore{bindings: map[states.Pair]states.Binding{}}
}

func (s *memStateStore) UpsertBinding(_ context.Context, b states.Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[b.Pair] = b
	return nil
}

func (s *memStateStore) DeleteBinding(_ context.Context, p states.Pair) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bindings, p)
	return nil
}

func (s *memStateStore) ListBindings(context.Context) ([]states.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]states.Binding, 0, len(s.bindings))
	for _, b := range s.bindings {
		out = append(out, b)
	}
	return out, nil
}

func (s *memStateStore) ListHistory(context.Context, states.Pair, int, int) ([]states.HistoryEntry, error) {
	return nil, nil
}

func (s *memStateStore) ClearHistory(context.Context, states.Pair) (int64, error) { return 0, nil }

func (s *memStateStore) AppendHistory(context.Context, states.HistoryEntry) error { return nil }

type memCursors struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemCursors() *memCursors { return &memCursors{m: map[string]string{}} }

func (c *memCursors) LastUsed(_ context.Context, authIndex string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[authIndex]
	return v, ok, nil
}

func (c *memCursors) SetLastUsed(_ context.Context, authIndex, lastUsed string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[authIndex] = lastUsed
	return nil
}

type fixedPolicy struct{ p states.Policy }

func (f fixedPolicy) StatePolicy() states.Policy { return f.p }

type fixedModels struct{ enabled map[string]bool }

func (m fixedModels) ModelTurnStateEnabled(model string) bool { return m.enabled[model] }

// authMap mimics AccountRegistry: the host hands the plugin a runtime auth id,
// and the plugin resolves it to its own persistence key. They are deliberately
// different strings here so the resolution is actually exercised.
type authMap map[string]string

func (m authMap) ResolveAuthID(authID string) (string, bool) {
	idx, ok := m[authID]
	return idx, ok
}

// ---------------------------------------------------------------------------
// harness

type harness struct {
	scheduler *Scheduler
	settings  *settings.Manager
	states    *states.Registry
	cursors   *memCursors
	clock     *time.Time
}

func newHarness(t *testing.T, strategy settings.RoutingStrategy) *harness {
	t.Helper()
	ctx := context.Background()

	store := &memSettingsStore{values: map[string]string{
		settings.KeyAccountRoutingStrategy: string(strategy),
	}}
	manager, err := settings.NewManager(ctx, store, nil)
	if err != nil {
		t.Fatalf("settings.NewManager: %v", err)
	}

	stateStore := newMemStateStore()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	clock := &now
	registry := states.NewRegistry(stateStore, stateStore, fixedPolicy{
		p: states.Policy{TTL: time.Hour, RefreshThresholdPct: 15, TargetStateLength: 292},
	})
	registry.SetClock(func() time.Time { return *clock })

	cursors := newMemCursors()
	s := NewScheduler(SchedulerConfig{
		Settings: manager,
		States:   registry,
		Cursors:  cursors,
		Models:   fixedModels{enabled: map[string]bool{"gpt-5-codex": true}},
		// "auth-id-a" resolves to persistence key "a", and so on.
		Auth: authMap{"auth-id-a": "a", "auth-id-b": "b", "auth-id-c": "c"},
	})
	s.SetClock(func() time.Time { return *clock })

	return &harness{scheduler: s, settings: manager, states: registry, cursors: cursors, clock: clock}
}

// bind attaches a usable binding to a persistence key.
func (h *harness) bind(t *testing.T, authIndex, model string) {
	t.Helper()
	if _, err := h.states.Bind(context.Background(), states.Binding{
		Pair: states.Pair{AuthIndex: authIndex, Model: model}, StateValue: "v",
	}); err != nil {
		t.Fatalf("Bind(%s,%s): %v", authIndex, model, err)
	}
}

// candidate builds a host candidate. The id is the host runtime identifier,
// not the plugin's persistence key.
func candidate(letter string, priority int) hostapi.Candidate {
	return hostapi.Candidate{
		ID:       "auth-id-" + letter,
		Provider: hostapi.ProviderCodex,
		Priority: priority,
		Status:   hostapi.AccountStatusActive,
	}
}

func codexRequest(model string, candidates ...hostapi.Candidate) hostapi.SchedulerPickRequest {
	return hostapi.SchedulerPickRequest{
		RequestID:  "req-1",
		Provider:   hostapi.ProviderCodex,
		Model:      model,
		Candidates: candidates,
	}
}

// ---------------------------------------------------------------------------
// declining to interfere

func TestDecide_DeclinesWhenNothingToGain(t *testing.T) {
	ctx := context.Background()

	t.Run("master switch off", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")
		off := false
		if _, err := h.settings.Update(ctx, settings.Patch{GlobalEnabled: &off}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", candidate("a", 5))); !got.Delegated() {
			t.Errorf("expected a decline, got authID %q", got.AuthID)
		}
	})

	t.Run("state priority switch off", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")

		// Everything else is on: only the routing switch is off.
		off := false
		if _, err := h.settings.Update(ctx, settings.Patch{StatePriorityEnabled: &off}); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", candidate("a", 5)))
		if !got.Delegated() {
			t.Errorf("routing must not interfere while state_priority_enabled is off, got %q", got.AuthID)
		}
		if got.Handled {
			t.Error("Handled must be false when the plugin declines")
		}
	})

	t.Run("state priority off still allows injection", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")

		off := false
		if _, err := h.settings.Update(ctx, settings.Patch{StatePriorityEnabled: &off}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		caps := h.settings.Current().Capabilities()
		if !caps.Inject {
			t.Error("turning off routing must not stop state injection")
		}
		if caps.Route {
			t.Error("Route should be false with the switch off")
		}
	})

	t.Run("non-codex provider", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")

		req := codexRequest("gpt-5-codex", candidate("a", 5))
		req.Provider = "anthropic"
		req.Providers = []string{"anthropic"}
		if got := h.scheduler.Decide(ctx, req); !got.Delegated() {
			t.Error("expected a decline for a non-codex provider")
		}
	})

	t.Run("mixed route containing a non-codex provider", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")

		req := codexRequest("gpt-5-codex", candidate("a", 5))
		req.Provider = ""
		req.Providers = []string{"codex", "anthropic"}
		if got := h.scheduler.Decide(ctx, req); !got.Delegated() {
			t.Error("a mixed route must not be steered")
		}
	})

	t.Run("codex-only mixed route is fine", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")

		req := codexRequest("gpt-5-codex", candidate("a", 5))
		req.Provider = ""
		req.Providers = []string{"codex"}
		if got := h.scheduler.Decide(ctx, req); got.Delegated() {
			t.Error("a codex-only route should still be steerable")
		}
	})

	t.Run("model not participating", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "some-other-model")

		if got := h.scheduler.Decide(ctx, codexRequest("some-other-model", candidate("a", 5))); !got.Delegated() {
			t.Error("expected a decline for a model outside turn-state handling")
		}
	})

	t.Run("no candidates", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		if got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex")); !got.Delegated() {
			t.Error("expected a decline with no candidates")
		}
	})

	t.Run("no candidate holds state", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", candidate("a", 5), candidate("b", 5)))
		if !got.Delegated() {
			t.Errorf("expected a decline, got authID %q", got.AuthID)
		}
	})

	t.Run("expired state is not usable", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")
		*h.clock = h.clock.Add(2 * time.Hour)

		if got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", candidate("a", 5))); !got.Delegated() {
			t.Error("expected a decline once the only binding has expired")
		}
	})

	t.Run("unresolvable candidate id is not preferred", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")

		// The host offers an account the plugin cannot map to a persistence
		// key, so it has no binding it can trust.
		stranger := hostapi.Candidate{ID: "auth-id-unknown", Priority: 9}
		if got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", stranger)); !got.Delegated() {
			t.Error("expected a decline when no candidate resolves to a known account")
		}
	})
}

// ---------------------------------------------------------------------------
// respect_cpa_priority

// TestDecide_RespectPriorityRestrictsToTopTier pins that the plugin applies the
// tier policy itself. It declares SchedulerAcrossPriorities at registration, so
// the host no longer pre-filters the candidate list -- if the plugin did not do
// this, "respect CPA priority" would silently stop respecting anything.
func TestDecide_RespectPriorityRestrictsToTopTier(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyRespectCPAPriority)

	// Only the low-priority account holds state, but the candidates now span
	// every tier.
	h.bind(t, "low", "gpt-5-codex")
	h.scheduler = newSchedulerWithAuth(t, h, authMap{"auth-id-high": "high", "auth-id-low": "low"})

	req := codexRequest("gpt-5-codex",
		hostapi.Candidate{ID: "auth-id-high", Priority: 10},
		hostapi.Candidate{ID: "auth-id-low", Priority: 1},
	)
	if got := h.scheduler.Decide(ctx, req); !got.Delegated() {
		t.Errorf("expected a decline rather than reaching past the top tier, got %q", got.AuthID)
	}
}

func TestDecide_RespectPriorityPicksWithinTopTier(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyRespectCPAPriority)

	h.bind(t, "a", "gpt-5-codex")
	h.bind(t, "b", "gpt-5-codex")
	h.bind(t, "c", "gpt-5-codex")

	req := codexRequest("gpt-5-codex",
		candidate("a", 10), candidate("b", 10), candidate("c", 1))

	got := h.scheduler.Decide(ctx, req)
	if got.Delegated() {
		t.Fatal("expected the plugin to choose an account")
	}
	if got.AuthID != "auth-id-a" && got.AuthID != "auth-id-b" {
		t.Errorf("chose %q, want a state-holding account from the top tier", got.AuthID)
	}
}

func TestDecide_RoundRobinsAcrossEligibleAccounts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyRespectCPAPriority)

	h.bind(t, "a", "gpt-5-codex")
	h.bind(t, "b", "gpt-5-codex")

	req := codexRequest("gpt-5-codex", candidate("a", 5), candidate("b", 5))

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		got := h.scheduler.Decide(ctx, req)
		if got.Delegated() {
			t.Fatalf("iteration %d: unexpected decline", i)
		}
		seen[got.AuthID]++
		*h.clock = h.clock.Add(time.Second)
	}

	// Even rotation: traffic must not pin to a single account.
	if seen["auth-id-a"] != 2 || seen["auth-id-b"] != 2 {
		t.Errorf("distribution = %v, want each account chosen twice", seen)
	}
}

// ---------------------------------------------------------------------------
// state_first

func TestDecide_StateFirstReachesAcrossTiers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyStateFirst)

	// Only the low-priority account holds state, but under state_first it wins.
	h.bind(t, "low", "gpt-5-codex")
	h.scheduler = newSchedulerWithAuth(t, h, authMap{"auth-id-high": "high", "auth-id-low": "low"})

	req := codexRequest("gpt-5-codex",
		hostapi.Candidate{ID: "auth-id-high", Priority: 10},
		hostapi.Candidate{ID: "auth-id-low", Priority: 1},
	)
	got := h.scheduler.Decide(ctx, req)
	if got.Delegated() {
		t.Fatal("state_first should choose the state-holding account across tiers")
	}
	if got.AuthID != "auth-id-low" {
		t.Errorf("chose %q, want auth-id-low", got.AuthID)
	}
}

func TestDecide_StateFirstStillPrefersHigherPriority(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyStateFirst)

	h.bind(t, "a", "gpt-5-codex")
	h.bind(t, "b", "gpt-5-codex")

	req := codexRequest("gpt-5-codex", candidate("b", 9), candidate("a", 2))

	// Both hold state, so priority decides which tier is eligible; the
	// lower-priority account must not jump in just because it is unused.
	for i := 0; i < 3; i++ {
		got := h.scheduler.Decide(ctx, req)
		if got.Delegated() {
			t.Fatalf("iteration %d: unexpected decline", i)
		}
		if got.AuthID != "auth-id-b" {
			t.Errorf("chose %q, want auth-id-b (higher priority)", got.AuthID)
		}
		*h.clock = h.clock.Add(time.Second)
	}
}

func TestDecide_StateFirstRoundRobinsWithinTopTier(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyStateFirst)

	h.bind(t, "a", "gpt-5-codex")
	h.bind(t, "b", "gpt-5-codex")
	h.bind(t, "c", "gpt-5-codex")

	req := codexRequest("gpt-5-codex",
		candidate("a", 7), candidate("b", 7), candidate("c", 1))

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		got := h.scheduler.Decide(ctx, req)
		if got.Delegated() {
			t.Fatalf("iteration %d: unexpected decline", i)
		}
		seen[got.AuthID]++
		*h.clock = h.clock.Add(time.Second)
	}

	if seen["auth-id-c"] != 0 {
		t.Errorf("distribution = %v, want the lower-priority account left out", seen)
	}
	if seen["auth-id-a"] != 2 || seen["auth-id-b"] != 2 {
		t.Errorf("distribution = %v, want an even split across the top tier", seen)
	}
}

func TestDecide_RefreshDueStateIsStillUsable(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyRespectCPAPriority)
	h.bind(t, "a", "gpt-5-codex")

	// Into the refresh window: still valid, so still worth steering to.
	*h.clock = h.clock.Add(55 * time.Minute)

	if got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", candidate("a", 5))); got.Delegated() {
		t.Error("a refresh_due binding is still usable and should be preferred")
	}
}

func TestDecide_PersistsCursorForRestartStability(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyRespectCPAPriority)
	h.bind(t, "a", "gpt-5-codex")

	if got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", candidate("a", 5))); got.Delegated() {
		t.Fatal("unexpected decline")
	}
	if _, ok, _ := h.cursors.LastUsed(ctx, "a"); !ok {
		t.Error("the chosen account's cursor was not persisted")
	}
}

// newSchedulerWithAuth rebuilds a harness scheduler with a different auth map,
// so a test can control which host ids resolve.
func newSchedulerWithAuth(t *testing.T, h *harness, auth AuthResolver) *Scheduler {
	t.Helper()
	s := NewScheduler(SchedulerConfig{
		Settings: h.settings,
		States:   h.states,
		Cursors:  h.cursors,
		Models:   fixedModels{enabled: map[string]bool{"gpt-5-codex": true}},
		Auth:     auth,
	})
	s.SetClock(func() time.Time { return *h.clock })
	return s
}
