package proxies

import (
	"context"
	"testing"
	"time"
)

type fakeStore struct {
	nodes     map[string]Node
	cooldowns map[cooldownKey]Cooldown
}

func newFakeStore() *fakeStore {
	return &fakeStore{nodes: map[string]Node{}, cooldowns: map[cooldownKey]Cooldown{}}
}

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

// The cooldown ledger, in memory. It is a separate map keyed by the pair so a
// test can assert that one account's failures leave another account's view of
// the same node alone -- which is the behaviour the ledger exists for.
func (s *fakeStore) ListCooldowns(context.Context) ([]Cooldown, error) {

	out := make([]Cooldown, 0, len(s.cooldowns))
	for _, c := range s.cooldowns {
		out = append(out, c)
	}
	return out, nil
}

func (s *fakeStore) UpsertCooldown(_ context.Context, c Cooldown) error {

	s.cooldowns[cooldownKey{c.AuthIndex, c.ProxyID}] = c
	return nil
}

func (s *fakeStore) DeleteCooldown(_ context.Context, authIndex, proxyID string) error {

	delete(s.cooldowns, cooldownKey{authIndex, proxyID})
	return nil
}

func (s *fakeStore) DeleteAllCooldowns(context.Context) error {

	s.cooldowns = map[cooldownKey]Cooldown{}
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

	got := ids(pool.AvailableFor("codex-auth-1", now))
	want := []string{"never-used", "used-long-ago", "used-recently"}
	if !equal(got, want) {
		t.Errorf("Available() = %v, want %v", got, want)
	}
}

// TestPool_AvailableExcludesDisabledAndCooling covers the selection filter. A
// disabled node is out for everyone; a cooling one is out only for the account
// that earned the cooldown.
func TestPool_AvailableExcludesDisabledAndCooling(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	pool := mustPool(t,
		Node{ID: "healthy", URL: "http://a", Enabled: true},
		Node{ID: "disabled", URL: "http://b", Enabled: false},
		Node{ID: "cooling", URL: "http://c", Enabled: true},
	)

	if _, err := pool.MarkFailure(ctx, "codex-auth-1", "cooling", 5*time.Minute, now, false); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}

	got := ids(pool.AvailableFor("codex-auth-1", now))
	want := []string{"healthy"}
	if !equal(got, want) {
		t.Errorf("AvailableFor(the cooling account) = %v, want %v", got, want)
	}
}

// TestPool_CooldownIsScopedToTheAccount is the reason the ledger exists.
//
// The same node returns the target state length for one account and a
// non-target length for another, so a node benched by one account's failures
// must stay available to every other. A node-level cooldown -- what this
// replaced -- took a working node away from accounts that had no problem with
// it.
func TestPool_CooldownIsScopedToTheAccount(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	pool := mustPool(t,
		Node{ID: "shared", URL: "http://a", Enabled: true},
	)

	if _, err := pool.MarkFailure(ctx, "account-a", "shared", 5*time.Minute, now, false); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}

	if got := ids(pool.AvailableFor("account-a", now)); len(got) != 0 {
		t.Errorf("account-a can still use the node it just failed against: %v", got)
	}
	if got := ids(pool.AvailableFor("account-b", now)); !equal(got, []string{"shared"}) {
		t.Errorf("account-b lost a node it never failed against: %v, want [shared]", got)
	}

	// A success for one account clears only that account's streak.
	if err := pool.MarkSuccess(ctx, "account-a", "shared", time.Second, now); err != nil {
		t.Fatalf("MarkSuccess: %v", err)
	}
	if got := ids(pool.AvailableFor("account-a", now)); !equal(got, []string{"shared"}) {
		t.Errorf("account-a did not get the node back after a success: %v", got)
	}
}

// TestPool_ResetIsScopedToThePair covers the detail modal's per-record reset.
func TestPool_ResetIsScopedToThePair(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	pool := mustPool(t, Node{ID: "shared", URL: "http://a", Enabled: true})
	for _, account := range []string{"account-a", "account-b"} {
		if _, err := pool.MarkFailure(ctx, account, "shared", 5*time.Minute, now, false); err != nil {
			t.Fatalf("MarkFailure(%s): %v", account, err)
		}
	}

	if err := pool.ResetCooldown(ctx, "account-a", "shared"); err != nil {
		t.Fatalf("ResetCooldown: %v", err)
	}
	if got := ids(pool.AvailableFor("account-a", now)); !equal(got, []string{"shared"}) {
		t.Errorf("account-a was not restored: %v", got)
	}
	if got := ids(pool.AvailableFor("account-b", now)); len(got) != 0 {
		t.Errorf("resetting one pair cleared another: %v", got)
	}

	// The global reset clears everything.
	if err := pool.ResetAllCooldowns(ctx); err != nil {
		t.Fatalf("ResetAllCooldowns: %v", err)
	}
	if got := ids(pool.AvailableFor("account-b", now)); !equal(got, []string{"shared"}) {
		t.Errorf("the global reset did not restore account-b: %v", got)
	}
	if n := len(pool.Cooldowns()); n != 0 {
		t.Errorf("Cooldowns() = %d rows after a global reset, want 0", n)
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

	got := ids(pool.AvailableFor("codex-auth-1", now))
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
	if got := ids(pool.AvailableFor("codex-auth-1", now))[0]; got != "a" {
		t.Fatalf("first pick = %q, want a", got)
	}
	if err := pool.MarkUsed(ctx, "a", now); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if got := ids(pool.AvailableFor("codex-auth-1", now.Add(time.Second)))[0]; got != "b" {
		t.Errorf("second pick = %q, want b", got)
	}
	if err := pool.MarkUsed(ctx, "b", now.Add(time.Second)); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	if got := ids(pool.AvailableFor("codex-auth-1", now.Add(2*time.Second)))[0]; got != "a" {
		t.Errorf("third pick = %q, want a (rotation should repeat)", got)
	}
}

func TestPool_MarkFailureCoolsDown(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	pool := mustPool(t, Node{ID: "a", URL: "http://a", Enabled: true})

	row, err := pool.MarkFailure(ctx, "acct", "a", 2*time.Minute, now, false)
	if err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}
	if row.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", row.ConsecutiveFailures)
	}
	if row.CooldownUntil == nil || !row.CooldownUntil.Equal(now.Add(2*time.Minute)) {
		t.Errorf("CooldownUntil = %v, want %v", row.CooldownUntil, now.Add(2*time.Minute))
	}
	if got := len(pool.AvailableFor("acct", now)); got != 0 {
		t.Errorf("a cooling node must not be available, got %d available", got)
	}

	// Past the cooldown it returns to service.
	if got := len(pool.AvailableFor("acct", now.Add(3*time.Minute))); got != 1 {
		t.Errorf("node should be available again after cooldown, got %d", got)
	}
}

func TestPool_MarkFailureWithoutCooldown(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	pool := mustPool(t, Node{ID: "a", URL: "http://a", Enabled: true})

	// A zero cooldown records the failure without benching the pair. Nothing in
	// the probe path passes zero today -- upstream 4xx are terminal and never
	// reach here -- but the guard is what keeps a future caller from taking a
	// node out of service for a fault it did not cause.
	row, err := pool.MarkFailure(ctx, "acct", "a", 0, now, false)
	if err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}
	if row.FailureCount != 1 {
		t.Errorf("FailureCount = %d, want 1", row.FailureCount)
	}
	if row.CooldownUntil != nil {
		t.Errorf("CooldownUntil = %v, want nil", row.CooldownUntil)
	}
	if got := len(pool.AvailableFor("acct", now)); got != 1 {
		t.Errorf("the node left service for a zero cooldown (%d available)", got)
	}
	if got := len(pool.AvailableFor("codex-auth-1", now)); got != 1 {
		t.Errorf("a zero-cooldown failure must leave the node available, got %d", got)
	}
}

func TestPool_MarkSuccessClearsCooldownAndStreak(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	pool := mustPool(t, Node{ID: "a", URL: "http://a", Enabled: true})
	if _, err := pool.MarkFailure(ctx, "acct", "a", time.Minute, now, false); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}
	if err := pool.MarkSuccess(ctx, "acct", "a", 120*time.Millisecond, now.Add(time.Second)); err != nil {
		t.Fatalf("MarkSuccess: %v", err)
	}

	if row := cooldownFor(pool, "acct", "a"); row.ConsecutiveFailures != 0 || row.CooldownUntil != nil {
		t.Errorf("a success left the pair at %d failures until %v, want cleared",
			row.ConsecutiveFailures, row.CooldownUntil)
	}
	n, ok := pool.Get("a")
	if !ok {
		t.Fatal("node missing after MarkSuccess")
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
		SuccessCount: 4, FailureCount: 2, LastUsedAt: &used,
	}

	pool := NewPool(store)
	if err := pool.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	n, ok := pool.Get("a")
	if !ok {
		t.Fatal("node a was not restored")
	}
	if n.SuccessCount != 4 || n.FailureCount != 2 {
		t.Errorf("restored counters = %d/%d, want 4/2", n.SuccessCount, n.FailureCount)
	}
	if n.LastUsedAt == nil || !n.LastUsedAt.Equal(used) {
		t.Errorf("LastUsedAt = %v, want %v", n.LastUsedAt, used)
	}
}

// cooldownFor reads one pair's ledger row, or the zero value when the pair has
// never failed.
func cooldownFor(pool *Pool, authIndex, proxyID string) Cooldown {
	for _, row := range pool.CooldownsForProxy(proxyID) {
		if row.AuthIndex == authIndex {
			return row
		}
	}
	return Cooldown{}
}
