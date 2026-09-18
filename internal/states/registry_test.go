package states

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory Store + HistoryStore.
type fakeStore struct {
	mu       sync.Mutex
	bindings map[Pair]Binding
	history  []HistoryEntry
}

func newFakeStore() *fakeStore {
	return &fakeStore{bindings: map[Pair]Binding{}}
}

func (s *fakeStore) UpsertBinding(_ context.Context, b Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[b.Pair] = b
	return nil
}

func (s *fakeStore) DeleteBinding(_ context.Context, p Pair) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bindings, p)
	return nil
}

func (s *fakeStore) ListBindings(context.Context) ([]Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Binding, 0, len(s.bindings))
	for _, b := range s.bindings {
		out = append(out, b)
	}
	return out, nil
}

func (s *fakeStore) AppendHistory(_ context.Context, e HistoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, e)
	return nil
}

func (s *fakeStore) ListHistory(_ context.Context, p Pair, limit, offset int) ([]HistoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []HistoryEntry
	for _, e := range s.history {
		if e.Pair == p {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) ClearHistory(context.Context, Pair) (int64, error) { return 0, nil }

func (s *fakeStore) historyFor(p Pair) []HistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []HistoryEntry
	for _, e := range s.history {
		if e.Pair == p {
			out = append(out, e)
		}
	}
	return out
}

type fixedPolicy struct{ p Policy }

func (f fixedPolicy) StatePolicy() Policy { return f.p }

func newTestRegistry(t *testing.T, ttl time.Duration, thresholdPct int) (*Registry, *fakeStore, *time.Time) {
	t.Helper()
	store := newFakeStore()
	policy := fixedPolicy{p: Policy{TTL: ttl, RefreshThresholdPct: thresholdPct, TargetStateLength: 292}}

	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	clock := &now

	r := NewRegistry(store, store, policy)
	r.SetClock(func() time.Time { return *clock })
	return r, store, clock
}

// TestRegistry_StatusTransitions walks the lifecycle across the exact
// FRESH/REFRESH_DUE/EXPIRED boundaries (design doc 3.2).
func TestRegistry_StatusTransitions(t *testing.T) {
	ttl := 60 * time.Minute
	const threshold = 15

	r, _, clock := newTestRegistry(t, ttl, threshold)

	pair := Pair{AuthIndex: "codex-auth-1", Model: "gpt-5-codex"}

	// MISSING before anything is bound.
	if _, status := r.Lookup(pair.AuthIndex, pair.Model); status != StatusMissing {
		t.Fatalf("initial status = %s, want missing", status)
	}

	if _, err := r.Bind(context.Background(), Binding{
		Pair: pair, StateValue: "abc", Source: SourceProbe, ProxyID: "p1",
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// refresh_after = bound_at + 51m, expires_at = bound_at + 60m.
	cases := []struct {
		name string
		at   time.Time
		want Status
	}{
		{"immediately after binding", clock.Add(0), StatusFresh},
		{"just inside the refresh window", clock.Add(50*time.Minute + 59*time.Second), StatusFresh},
		{"exactly at refresh_after", clock.Add(51 * time.Minute), StatusRefreshDue},
		{"well inside the refresh window", clock.Add(55 * time.Minute), StatusRefreshDue},
		{"one second before expiry", clock.Add(60*time.Minute - time.Second), StatusRefreshDue},
		{"exactly at expiry", clock.Add(60 * time.Minute), StatusExpired},
		{"long after expiry", clock.Add(3 * time.Hour), StatusExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			*clock = tc.at
			_, status := r.Lookup(pair.AuthIndex, pair.Model)
			if status != tc.want {
				t.Errorf("status at %s = %s, want %s", tc.at, status, tc.want)
			}
		})
	}
}

func TestStatus_UsableAndNeedsRefresh(t *testing.T) {
	cases := []struct {
		status       Status
		usable       bool
		needsRefresh bool
	}{
		{StatusMissing, false, false},
		{StatusFresh, true, false},
		{StatusRefreshDue, true, true},
		{StatusExpired, false, false},
	}
	for _, tc := range cases {
		if got := tc.status.Usable(); got != tc.usable {
			t.Errorf("%s.Usable() = %v, want %v", tc.status, got, tc.usable)
		}
		if got := tc.status.NeedsRefresh(); got != tc.needsRefresh {
			t.Errorf("%s.NeedsRefresh() = %v, want %v", tc.status, got, tc.needsRefresh)
		}
	}
}

func TestRegistry_BindActions(t *testing.T) {
	ctx := context.Background()
	r, store, _ := newTestRegistry(t, time.Hour, 15)
	pair := Pair{AuthIndex: "a", Model: "m"}

	action, err := r.Bind(ctx, Binding{Pair: pair, StateValue: "first", Source: SourceProbe})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if action != ActionBound {
		t.Errorf("first bind action = %s, want bound", action)
	}

	// A different value replaces the binding.
	action, err = r.Bind(ctx, Binding{Pair: pair, StateValue: "second", Source: SourceTraffic})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if action != ActionReplaced {
		t.Errorf("second bind action = %s, want replaced", action)
	}

	// The same value only extends the TTL.
	action, err = r.Bind(ctx, Binding{Pair: pair, StateValue: "second", Source: SourceTraffic})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if action != ActionRefreshed {
		t.Errorf("repeat bind action = %s, want refreshed", action)
	}

	history := store.historyFor(pair)
	if len(history) != 2 {
		t.Fatalf("history has %d rows, want 2 (a refresh must not be recorded)", len(history))
	}
	if history[0].Action != ActionBound || history[1].Action != ActionReplaced {
		t.Errorf("history actions = %s, %s; want bound, replaced", history[0].Action, history[1].Action)
	}
}

func TestRegistry_BindComputesExpiry(t *testing.T) {
	ctx := context.Background()
	r, _, clock := newTestRegistry(t, 60*time.Minute, 15)

	if _, err := r.Bind(ctx, Binding{
		Pair: Pair{AuthIndex: "a", Model: "m"}, StateValue: "value",
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	binding, status := r.Lookup("a", "m")
	if status != StatusFresh {
		t.Fatalf("status = %s, want fresh", status)
	}
	if !binding.BoundAt.Equal(*clock) {
		t.Errorf("BoundAt = %v, want %v", binding.BoundAt, *clock)
	}
	if want := clock.Add(60 * time.Minute); !binding.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", binding.ExpiresAt, want)
	}
	if want := clock.Add(51 * time.Minute); !binding.RefreshAfter.Equal(want) {
		t.Errorf("RefreshAfter = %v, want %v", binding.RefreshAfter, want)
	}
	if binding.StateLength != 5 {
		t.Errorf("StateLength = %d, want 5", binding.StateLength)
	}
}

func TestRegistry_DeleteRecordsHistory(t *testing.T) {
	ctx := context.Background()
	r, store, _ := newTestRegistry(t, time.Hour, 15)
	pair := Pair{AuthIndex: "a", Model: "m"}

	if _, err := r.Bind(ctx, Binding{Pair: pair, StateValue: "v", Source: SourceProbe, ProxyID: "p1"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := r.Delete(ctx, pair, SourceManual); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, status := r.Lookup(pair.AuthIndex, pair.Model); status != StatusMissing {
		t.Errorf("status after delete = %s, want missing", status)
	}

	history := store.historyFor(pair)
	if len(history) != 2 {
		t.Fatalf("history has %d rows, want 2", len(history))
	}
	last := history[len(history)-1]
	if last.Action != ActionDeleted {
		t.Errorf("last action = %s, want deleted", last.Action)
	}
	if last.Source != SourceManual {
		t.Errorf("last source = %s, want manual", last.Source)
	}
	// The deleted value is retained in history so the panel can show what was
	// removed.
	if last.StateValue != "v" {
		t.Errorf("history state = %q, want v", last.StateValue)
	}
}

func TestRegistry_DeleteMissingPairIsHarmless(t *testing.T) {
	ctx := context.Background()
	r, store, _ := newTestRegistry(t, time.Hour, 15)
	pair := Pair{AuthIndex: "ghost", Model: "m"}

	if err := r.Delete(ctx, pair, SourceManual); err != nil {
		t.Fatalf("Delete on a missing pair returned an error: %v", err)
	}
	if got := len(store.historyFor(pair)); got != 0 {
		t.Errorf("history rows = %d, want 0 for a pair that was never bound", got)
	}
}

func TestRegistry_LoadRebuildsSnapshot(t *testing.T) {
	ctx := context.Background()
	r, store, clock := newTestRegistry(t, time.Hour, 15)

	if _, err := r.Bind(ctx, Binding{
		Pair: Pair{AuthIndex: "a", Model: "m"}, StateValue: "persisted", Source: SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// A fresh registry over the same store must see the binding, which is what
	// makes a CPA restart resume cleanly (NF-04).
	policy := fixedPolicy{p: Policy{TTL: time.Hour, RefreshThresholdPct: 15}}
	restored := NewRegistry(store, store, policy)
	restored.SetClock(func() time.Time { return *clock })
	if err := restored.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	b, status := restored.Lookup("a", "m")
	if status != StatusFresh {
		t.Fatalf("restored status = %s, want fresh", status)
	}
	if b.StateValue != "persisted" {
		t.Errorf("restored value = %q, want persisted", b.StateValue)
	}
}

func TestRegistry_Expired(t *testing.T) {
	ctx := context.Background()
	r, _, clock := newTestRegistry(t, time.Hour, 15)

	for _, m := range []string{"m1", "m2"} {
		if _, err := r.Bind(ctx, Binding{
			Pair: Pair{AuthIndex: "a", Model: m}, StateValue: "v",
		}); err != nil {
			t.Fatalf("Bind: %v", err)
		}
	}

	if got := len(r.Expired()); got != 0 {
		t.Errorf("Expired() = %d fresh bindings, want 0", got)
	}

	*clock = clock.Add(61 * time.Minute)
	if got := len(r.Expired()); got != 2 {
		t.Errorf("Expired() = %d, want 2", got)
	}
}

func TestRegistry_InvalidateDropsBinding(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRegistry(t, time.Hour, 15)
	pair := Pair{AuthIndex: "a", Model: "m"}

	if _, err := r.Bind(ctx, Binding{Pair: pair, StateValue: "bad", Source: SourceProbe}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := r.Invalidate(ctx, pair); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, status := r.Lookup(pair.AuthIndex, pair.Model); status != StatusMissing {
		t.Errorf("status after invalidate = %s, want missing", status)
	}
}

// TestRegistry_LookupSurvivesConcurrentWrites exercises the copy-on-write
// snapshot under the race detector: readers must never observe a torn map.
func TestRegistry_LookupSurvivesConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	r, _, _ := newTestRegistry(t, time.Hour, 15)

	// Readers are bounded by a stop channel that the writers close, not by
	// wg.Wait -- otherwise the readers and the wait would deadlock each other.
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			pair := Pair{AuthIndex: "a", Model: string(rune('a' + id))}
			for n := 0; n < 200; n++ {
				if _, err := r.Bind(ctx, Binding{Pair: pair, StateValue: "v", Source: SourceProbe}); err != nil {
					t.Errorf("Bind: %v", err)
					return
				}
			}
		}(i)
	}

	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r.Lookup("a", "a")
					r.All()
				}
			}
		}()
	}

	wg.Wait()
	close(stop)
	readers.Wait()
}

func TestPolicy_Expiry(t *testing.T) {
	p := Policy{TTL: 40 * time.Minute, RefreshThresholdPct: 25}
	bound := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	expires, refresh := p.Expiry(bound)
	if want := bound.Add(40 * time.Minute); !expires.Equal(want) {
		t.Errorf("Expiry expires = %v, want %v", expires, want)
	}
	if want := bound.Add(30 * time.Minute); !refresh.Equal(want) {
		t.Errorf("Expiry refresh = %v, want %v", refresh, want)
	}
}

func TestPolicy_AcceptsLength(t *testing.T) {
	p := Policy{TargetStateLength: 292}
	if !p.AcceptsLength(292) {
		t.Error("292 should be accepted")
	}
	for _, n := range []int{0, 291, 293} {
		if p.AcceptsLength(n) {
			t.Errorf("%d should be rejected", n)
		}
	}
}
