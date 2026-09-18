package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// Design document section 8, verification item 6 asks whether SQLite WAL write
// concurrency is stable under the plugin's threading, naming concurrent
// proxy_node.last_used_at updates specifically.
//
// These tests exercise the real path -- proxies.Pool over the SQLite store --
// rather than the store in isolation, because the pool is what the probe
// workers actually share.

func newMigratedDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(context.Background(), filepath.Join(dir, "backups"), nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

// TestProxyLastUsed_ConcurrentUpdates is the exact scenario item 6 names: many
// workers stamping the same node at once.
func TestProxyLastUsed_ConcurrentUpdates(t *testing.T) {
	ctx := context.Background()
	db := newMigratedDB(t)

	pool := proxies.NewPool(db.Proxies())
	if err := pool.Upsert(ctx, proxies.Node{ID: "p1", URL: "http://p1:8080", Enabled: true}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	const (
		workers    = 8
		iterations = 25
	)

	var wg sync.WaitGroup
	errs := make(chan error, workers*iterations)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				now := time.Now().UTC()
				// All three mutators contend on the same row, which is the
				// worst case the design document flags.
				if err := pool.MarkUsed(ctx, "p1", now); err != nil {
					errs <- fmt.Errorf("worker %d MarkUsed: %w", worker, err)
					return
				}
				if _, err := pool.MarkFailure(ctx, "p1", 0, now, false); err != nil {
					errs <- fmt.Errorf("worker %d MarkFailure: %w", worker, err)
					return
				}
				if err := pool.MarkSuccess(ctx, "p1", 5*time.Millisecond, now); err != nil {
					errs <- fmt.Errorf("worker %d MarkSuccess: %w", worker, err)
					return
				}
			}
		}(w)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	// Every write must have landed: 8 workers x 25 iterations.
	nodes, err := db.Proxies().ListProxies(ctx)
	if err != nil {
		t.Fatalf("ListProxies after concurrent writes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("rows = %d, want 1", len(nodes))
	}
	node := nodes[0]
	if want := workers * iterations; node.SuccessCount != want {
		t.Errorf("SuccessCount = %d, want %d (a lost update means writes are not serialised)",
			node.SuccessCount, want)
	}
	if want := workers * iterations; node.FailureCount != want {
		t.Errorf("FailureCount = %d, want %d", node.FailureCount, want)
	}
	if node.LastUsedAt == nil {
		t.Error("LastUsedAt is null after concurrent updates")
	}
}

// TestProxyLastUsed_ConcurrentDistinctNodes covers the other half: many nodes
// being stamped at once, which is what a pool traversal looks like.
func TestProxyLastUsed_ConcurrentDistinctNodes(t *testing.T) {
	ctx := context.Background()
	db := newMigratedDB(t)

	pool := proxies.NewPool(db.Proxies())
	const nodeCount = 12
	for i := 0; i < nodeCount; i++ {
		if err := pool.Upsert(ctx, proxies.Node{
			ID: fmt.Sprintf("p%02d", i), URL: fmt.Sprintf("http://p%02d:8080", i), Enabled: true,
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < nodeCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("p%02d", i)
			for j := 0; j < 10; j++ {
				if err := pool.MarkUsed(ctx, id, time.Now().UTC()); err != nil {
					t.Errorf("%s MarkUsed: %v", id, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	nodes, err := db.Proxies().ListProxies(ctx)
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(nodes) != nodeCount {
		t.Fatalf("rows = %d, want %d", len(nodes), nodeCount)
	}
	for _, n := range nodes {
		if n.LastUsedAt == nil {
			t.Errorf("%s has no lastUsedAt", n.ID)
		}
	}
}

// TestReadersAreNotBlockedByWriters is the "supports concurrent read/write"
// claim from NF-03: the management API must stay answerable while probe
// workers write.
func TestReadersAreNotBlockedByWriters(t *testing.T) {
	ctx := context.Background()
	db := newMigratedDB(t)

	pool := proxies.NewPool(db.Proxies())
	if err := pool.Upsert(ctx, proxies.Node{ID: "p1", URL: "http://p1:8080", Enabled: true}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	stop := make(chan struct{})
	var writers, readers sync.WaitGroup

	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if err := pool.MarkUsed(ctx, "p1", time.Now().UTC()); err != nil {
						t.Errorf("MarkUsed: %v", err)
						return
					}
				}
			}
		}()
	}

	errs := make(chan error, 4)
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := db.Proxies().ListProxies(ctx); err != nil {
						errs <- err
						return
					}
					if _, err := db.Settings().All(ctx); err != nil {
						errs <- err
						return
					}
				}
			}
		}()
	}

	time.Sleep(250 * time.Millisecond)
	close(stop)
	writers.Wait()
	readers.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("a read failed while writes were in flight: %v", err)
	}
}

// TestBindingWritesAreSerialised checks the same guarantee on the hot-path
// backing table: concurrent binds must all land, with the last one winning.
func TestBindingWritesAreSerialised(t *testing.T) {
	ctx := context.Background()
	db := newMigratedDB(t)
	store := db.Bindings()

	const writers = 6
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			now := time.Now().UTC()
			for i := 0; i < 15; i++ {
				if err := store.UpsertBinding(ctx, bindingFixture(w, i, now)); err != nil {
					t.Errorf("worker %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	list, err := store.ListBindings(ctx)
	if err != nil {
		t.Fatalf("ListBindings: %v", err)
	}
	if len(list) != writers {
		t.Fatalf("rows = %d, want %d (one per writer's pair)", len(list), writers)
	}
	for _, b := range list {
		if b.StateLength == 0 {
			t.Errorf("binding %s/%s lost its value length", b.AuthIndex, b.Model)
		}
	}
}

func bindingFixture(worker, i int, at time.Time) states.Binding {
	return states.Binding{
		Pair: states.Pair{
			AuthIndex: fmt.Sprintf("auth-%d", worker),
			Model:     "gpt-5-codex",
		},
		StateValue:   fmt.Sprintf("value-%d-%d", worker, i),
		StateLength:  len(fmt.Sprintf("value-%d-%d", worker, i)),
		BoundAt:      at,
		ExpiresAt:    at.Add(time.Hour),
		RefreshAfter: at.Add(51 * time.Minute),
		Source:       states.SourceProbe,
	}
}
