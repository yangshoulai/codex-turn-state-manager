package proxies

import (
	"context"
	"testing"
	"time"
)

type fakeStore struct {
	nodes map[string]Node
}

func newFakeStore() *fakeStore { return &fakeStore{nodes: map[string]Node{}} }

func (s *fakeStore) ListProxies(context.Context) ([]Node, error) {
	out := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n)
	}
	return out, nil
}

func (s *fakeStore) UpsertProxy(_ context.Context, n Node) error {
	s.nodes[n.ID] = n
	return nil
}

func (s *fakeStore) DeleteProxy(_ context.Context, id string) error {
	delete(s.nodes, id)
	return nil
}

func mustPool(t *testing.T, nodes ...Node) *Pool {
	t.Helper()
	p := NewPool(newFakeStore())
	for _, n := range nodes {
		if err := p.Upsert(context.Background(), n); err != nil {
			t.Fatalf("Upsert(%s): %v", n.ID, err)
		}
	}
	return p
}

func ids(nodes []Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPool_AvailableOrdersLeastRecentlyUsedFirst covers the selection policy
// in design doc 3.7.2.
func TestPool_AvailableOrdersLeastRecentlyUsedFirst(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time {
		at := now.Add(-d)
		return &at
	}

	pool := mustPool(t,
		Node{ID: "used-recently", URL: "http://a", Enabled: true, LastUsedAt: ago(time.Minute)},
		Node{ID: "never-used", URL: "http://b", Enabled: true},
		Node{ID: "used-long-ago", URL: "http://c", Enabled: true, LastUsedAt: ago(time.Hour)},
	)

	got := ids(pool.Available(now))
	want := []string{"never-used", "used-long-ago", "used-recently"}
	if !equal(got, want) {
		t.Errorf("Available() = %v, want %v", got, want)
	}
}

func TestPool_AvailableExcludesDisabledAndCooling(t *testing.T) {
	now := time.Now()
	until := now.Add(5 * time.Minute)
	past := now.Add(-time.Minute)

	pool := mustPool(t,
		Node{ID: "healthy", URL: "http://a", Enabled: true},
		Node{ID: "disabled", URL: "http://b", Enabled: false},
		Node{ID: "cooling", URL: "http://c", Enabled: true, CooldownUntil: &until},
		Node{ID: "cooled-off", URL: "http://d", Enabled: true, CooldownUntil: &past},
	)

	got := ids(pool.Available(now))
	want := []string{"cooled-off", "healthy"}
	if !equal(got, want) {
		t.Errorf("Available() = %v, want %v", got, want)
	}
}

func TestPool_AvailableTieBreaksByID(t *testing.T) {
	now := time.Now()
	same := now.Add(-time.Minute)

	pool := mustPool(t,
		Node{ID: "zulu", URL: "http://z", Enabled: true, LastUsedAt: &same},
		Node{ID: "alpha", URL: "http://a", Enabled: true, LastUsedAt: &same},
		Node{ID: "mike", URL: "http://m", Enabled: true, LastUsedAt: &same},
	)

	got := ids(pool.Available(now))
	want := []string{"alpha", "mike", "zulu"}
	if !equal(got, want) {
		t.Errorf("Available() = %v, want %v (ties must break by ascending id)", got, want)
	}
}

func TestPool_MarkUsedRotatesSelection(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	pool := mustPool(t,
		Node{ID: "a", URL: "http://a", Enabled: true},
		Node{ID: "b", URL: "http://b", Enabled: true},
	)

	// The least-recently-used node is picked, and stamping it sends it to the
	// back of the queue.
	if got := ids(pool.Available(now))[0]; got != "a" {
		t.Fatalf("first pick = %q, want a", got)
	}
	if err := pool.MarkUsed(ctx, "a", now); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if got := ids(pool.Available(now.Add(time.Second)))[0]; got != "b" {
		t.Errorf("second pick = %q, want b", got)
	}
	if err := pool.MarkUsed(ctx, "b", now.Add(time.Second)); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if got := ids(pool.Available(now.Add(2 * time.Second)))[0]; got != "a" {
		t.Errorf("third pick = %q, want a (rotation should repeat)", got)
	}
}

func TestPool_MarkFailureCoolsDown(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	pool := mustPool(t, Node{ID: "a", URL: "http://a", Enabled: true})

	n, err := pool.MarkFailure(ctx, "a", 2*time.Minute, now)
	if err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}
	if n.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", n.ConsecutiveFailures)
	}
	if n.CooldownUntil == nil || !n.CooldownUntil.Equal(now.Add(2*time.Minute)) {
		t.Errorf("CooldownUntil = %v, want %v", n.CooldownUntil, now.Add(2*time.Minute))
	}
	if got := len(pool.Available(now)); got != 0 {
		t.Errorf("a cooling node must not be available, got %d available", got)
	}

	// Past the cooldown it returns to service.
	if got := len(pool.Available(now.Add(3 * time.Minute))); got != 1 {
		t.Errorf("node should be available again after cooldown, got %d", got)
	}
}

func TestPool_MarkFailureWithoutCooldown(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	pool := mustPool(t, Node{ID: "a", URL: "http://a", Enabled: true})

	// A zero cooldown is how a non-proxy fault (upstream 4xx) is recorded: the
	// counters move but the node stays in service.
	n, err := pool.MarkFailure(ctx, "a", 0, now)
	if err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}
	if n.FailureCount != 1 {
		t.Errorf("FailureCount = %d, want 1", n.FailureCount)
	}
	if n.CooldownUntil != nil {
		t.Errorf("CooldownUntil = %v, want nil", n.CooldownUntil)
	}
	if got := len(pool.Available(now)); got != 1 {
		t.Errorf("a zero-cooldown failure must leave the node available, got %d", got)
	}
}

func TestPool_MarkSuccessClearsCooldownAndStreak(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	pool := mustPool(t, Node{ID: "a", URL: "http://a", Enabled: true})
	if _, err := pool.MarkFailure(ctx, "a", time.Minute, now); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}
	if err := pool.MarkSuccess(ctx, "a", 120*time.Millisecond, now.Add(time.Second)); err != nil {
		t.Fatalf("MarkSuccess: %v", err)
	}

	n, ok := pool.Get("a")
	if !ok {
		t.Fatal("node missing after MarkSuccess")
	}
	if n.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", n.ConsecutiveFailures)
	}
	if n.CooldownUntil != nil {
		t.Errorf("CooldownUntil = %v, want nil after a success", n.CooldownUntil)
	}
	if n.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", n.SuccessCount)
	}
	if n.LastLatencyMS == nil || *n.LastLatencyMS != 120 {
		t.Errorf("LastLatencyMS = %v, want 120", n.LastLatencyMS)
	}
}

func TestPool_UpsertRejectsIncompleteNodes(t *testing.T) {
	pool := NewPool(newFakeStore())
	if err := pool.Upsert(context.Background(), Node{URL: "http://a"}); err == nil {
		t.Error("expected an error for a node without an id")
	}
	if err := pool.Upsert(context.Background(), Node{ID: "a"}); err == nil {
		t.Error("expected an error for a node without a url")
	}
}

func TestPool_ReplaceAllRemovesOmittedNodes(t *testing.T) {
	ctx := context.Background()
	pool := mustPool(t,
		Node{ID: "a", URL: "http://a", Enabled: true},
		Node{ID: "b", URL: "http://b", Enabled: true},
	)

	if err := pool.ReplaceAll(ctx, []Node{{ID: "b", URL: "http://b2", Enabled: true}}); err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	got := ids(pool.All())
	if !equal(got, []string{"b"}) {
		t.Errorf("All() = %v, want [b]", got)
	}
	if n, _ := pool.Get("b"); n.URL != "http://b2" {
		t.Errorf("URL = %q, want http://b2", n.URL)
	}
}

func TestPool_LoadRestoresPersistedState(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	now := time.Now()
	used := now.Add(-time.Hour)
	store.nodes["a"] = Node{
		ID: "a", URL: "http://a", Enabled: true,
		SuccessCount: 4, ConsecutiveFailures: 2, LastUsedAt: &used,
	}

	pool := NewPool(store)
	if err := pool.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	n, ok := pool.Get("a")
	if !ok {
		t.Fatal("node a was not restored")
	}
	if n.SuccessCount != 4 || n.ConsecutiveFailures != 2 {
		t.Errorf("restored counters = %d/%d, want 4/2", n.SuccessCount, n.ConsecutiveFailures)
	}
	if n.LastUsedAt == nil || !n.LastUsedAt.Equal(used) {
		t.Errorf("LastUsedAt = %v, want %v", n.LastUsedAt, used)
	}
}
