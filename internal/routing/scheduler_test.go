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

// harness bundles a scheduler with the pieces a test needs to steer it.
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
	})
	s.SetClock(func() time.Time { return *clock })

	return &harness{scheduler: s, settings: manager, states: registry, cursors: cursors, clock: clock}
}

func (h *harness) bind(t *testing.T, authIndex, model string) {
	t.Helper()
	if _, err := h.states.Bind(context.Background(), states.Binding{
		Pair: states.Pair{AuthIndex: authIndex, Model: model}, StateValue: "v",
	}); err != nil {
		t.Fatalf("Bind(%s,%s): %v", authIndex, model, err)
	}
}

func codexRequest(model string, candidates ...hostapi.Candidate) hostapi.SchedulerPickRequest {
	for i := range candidates {
		candidates[i].Provider = hostapi.ProviderCodex
		candidates[i].Model = model
	}
	return hostapi.SchedulerPickRequest{
		RequestID:  "req-1",
		Provider:   hostapi.ProviderCodex,
		Model:      model,
		Candidates: candidates,
	}
}

func candidate(authIndex string, priority int) hostapi.Candidate {
	return hostapi.Candidate{
		AuthID: "auth-id-" + authIndex, AuthIndex: authIndex, Priority: priority,
	}
}

// ---------------------------------------------------------------------------
// delegation

func TestDecide_DelegatesWhenNothingToGain(t *testing.T) {
	ctx := context.Background()

	t.Run("master switch off", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")
		off := false
		if _, err := h.settings.Update(ctx, settings.Patch{GlobalEnabled: &off}); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", candidate("a", 5)))
		if !got.Delegate {
			t.Errorf("Delegate = false (authID %q), want true when the master switch is off", got.AuthID)
		}
	})

	t.Run("non-codex provider", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")

		req := codexRequest("gpt-5-codex", candidate("a", 5))
		req.Provider = "anthropic"
		if got := h.scheduler.Decide(ctx, req); !got.Delegate {
			t.Error("expected delegation for a non-codex provider")
		}
	})

	t.Run("model not participating", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "some-other-model")

		if got := h.scheduler.Decide(ctx, codexRequest("some-other-model", candidate("a", 5))); !got.Delegate {
			t.Error("expected delegation for a model outside turn-state handling")
		}
	})

	t.Run("no candidates", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		if got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex")); !got.Delegate {
			t.Error("expected delegation with no candidates")
		}
	})

	t.Run("no candidate holds state", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex",
			candidate("a", 5), candidate("b", 5)))
		if !got.Delegate {
			t.Errorf("Delegate = false (authID %q), want true when no candidate has state", got.AuthID)
		}
	})

	t.Run("expired state is not usable", func(t *testing.T) {
		h := newHarness(t, settings.StrategyRespectCPAPriority)
		h.bind(t, "a", "gpt-5-codex")
		*h.clock = h.clock.Add(2 * time.Hour)

		if got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", candidate("a", 5))); !got.Delegate {
			t.Error("expected delegation once the only binding has expired")
		}
	})
}

// ---------------------------------------------------------------------------
// respect_cpa_priority

// TestDecide_RespectPriorityStaysInTheTopBucket pins the interpretation that a
// state-holding account outside CPA's top priority bucket is not reached for:
// honouring CPA's ordering is worth more than steering the request.
func TestDecide_RespectPriorityStaysInTheTopBucket(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyRespectCPAPriority)

	// Only the low-priority account holds state.
	h.bind(t, "low", "gpt-5-codex")

	req := codexRequest("gpt-5-codex", candidate("high", 10), candidate("low", 1))
	if got := h.scheduler.Decide(ctx, req); !got.Delegate {
		t.Errorf("Delegate = false (authID %q); want delegation rather than reaching past the top bucket", got.AuthID)
	}
}

func TestDecide_RespectPriorityPicksWithinTopBucket(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyRespectCPAPriority)

	h.bind(t, "high-a", "gpt-5-codex")
	h.bind(t, "high-b", "gpt-5-codex")
	h.bind(t, "low", "gpt-5-codex")

	req := codexRequest("gpt-5-codex",
		candidate("high-a", 10), candidate("high-b", 10), candidate("low", 1))

	got := h.scheduler.Decide(ctx, req)
	if got.Delegate {
		t.Fatal("expected the plugin to choose an account")
	}
	if got.AuthID != "auth-id-high-a" && got.AuthID != "auth-id-high-b" {
		t.Errorf("chose %q, want a state-holding account from the top bucket", got.AuthID)
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
		if got.Delegate {
			t.Fatalf("iteration %d: unexpected delegation", i)
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

func TestDecide_StateFirstReachesAcrossBuckets(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyStateFirst)

	// Only the low-priority account holds state, but under state_first it is
	// still preferred.
	h.bind(t, "low", "gpt-5-codex")

	req := codexRequest("gpt-5-codex", candidate("high", 10), candidate("low", 1))
	got := h.scheduler.Decide(ctx, req)
	if got.Delegate {
		t.Fatal("state_first should choose the state-holding account across buckets")
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

	// Both hold state, so priority decides which bucket is eligible; with a
	// single account in the top bucket, it should be chosen every time -- the
	// lower-priority account must not jump in just because it is unused.
	for i := 0; i < 3; i++ {
		got := h.scheduler.Decide(ctx, req)
		if got.Delegate {
			t.Fatalf("iteration %d: unexpected delegation", i)
		}
		if got.AuthID != "auth-id-b" {
			t.Errorf("chose %q, want auth-id-b (higher priority)", got.AuthID)
		}
		*h.clock = h.clock.Add(time.Second)
	}
}

func TestDecide_StateFirstRoundRobinsWithinTopBucket(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyStateFirst)

	h.bind(t, "a", "gpt-5-codex")
	h.bind(t, "b", "gpt-5-codex")
	h.bind(t, "c", "gpt-5-codex")

	// a and b share the top bucket; c is lower and must be excluded.
	req := codexRequest("gpt-5-codex",
		candidate("a", 7), candidate("b", 7), candidate("c", 1))

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		got := h.scheduler.Decide(ctx, req)
		if got.Delegate {
			t.Fatalf("iteration %d: unexpected delegation", i)
		}
		seen[got.AuthID]++
		*h.clock = h.clock.Add(time.Second)
	}

	if seen["auth-id-c"] != 0 {
		t.Errorf("distribution = %v, want the lower-priority account left out", seen)
	}
	if seen["auth-id-a"] != 2 || seen["auth-id-b"] != 2 {
		t.Errorf("distribution = %v, want an even split across the top bucket", seen)
	}
}

func TestDecide_RefreshDueStateIsStillUsable(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyRespectCPAPriority)
	h.bind(t, "a", "gpt-5-codex")

	// Move into the refresh window: still valid, so still worth steering to.
	*h.clock = h.clock.Add(55 * time.Minute)

	got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", candidate("a", 5)))
	if got.Delegate {
		t.Error("a refresh_due binding is still usable and should be preferred")
	}
}

func TestDecide_PersistsCursorForRestartStability(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, settings.StrategyRespectCPAPriority)
	h.bind(t, "a", "gpt-5-codex")

	if got := h.scheduler.Decide(ctx, codexRequest("gpt-5-codex", candidate("a", 5))); got.Delegate {
		t.Fatal("unexpected delegation")
	}
	if _, ok, _ := h.cursors.LastUsed(ctx, "a"); !ok {
		t.Error("the chosen account's cursor was not persisted")
	}
}
