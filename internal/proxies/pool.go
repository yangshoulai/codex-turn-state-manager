// Package proxies owns the probe proxy pool: node health, cooldown and the
// least-recently-used selection order.
package proxies

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Node is one proxy in the pool.
type Node struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`

	SuccessCount        int `json:"successCount"`
	FailureCount        int `json:"failureCount"`
	ConsecutiveFailures int `json:"consecutiveFailures"`

	CooldownUntil *time.Time `json:"cooldownUntil,omitempty"`
	LastLatencyMS *int       `json:"lastLatencyMs,omitempty"`

	// LastUsedAt is stamped the moment a node is picked, before the request is
	// sent, so concurrent probe workers never pick the same node (design doc
	// 3.7.3 rule 4).
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	LastSuccess *time.Time `json:"lastSuccess,omitempty"`
	LastFailure *time.Time `json:"lastFailure,omitempty"`
}

// Healthy reports whether the node is enabled and out of cooldown at `now`.
func (n Node) Healthy(now time.Time) bool {
	return n.Enabled && !n.CoolingDown(now)
}

// CoolingDown reports whether the node is inside a cooldown window.
func (n Node) CoolingDown(now time.Time) bool {
	return n.CooldownUntil != nil && now.Before(*n.CooldownUntil)
}

// Status renders the node state for the panel.
func (n Node) Status(now time.Time) string {
	switch {
	case !n.Enabled:
		return "disabled"
	case n.CoolingDown(now):
		return "cooldown"
	default:
		return "healthy"
	}
}

// Store persists pool state. Implemented by storage.ProxyStore.
type Store interface {
	ListProxies(ctx context.Context) ([]Node, error)
	UpsertProxy(ctx context.Context, n Node) error
	DeleteProxy(ctx context.Context, id string) error
}

// Pool is the in-memory view of the proxy pool. It is small (a handful of
// nodes) and read on every probe, so it is kept fully resident and guarded by
// a plain mutex rather than an atomic snapshot.
type Pool struct {
	store Store

	mu    sync.Mutex
	nodes map[string]Node
}

// NewPool builds an empty pool. Call Load to populate it.
func NewPool(store Store) *Pool {
	return &Pool{store: store, nodes: map[string]Node{}}
}

// Load rebuilds the pool from the store, restoring health counters and
// last_used_at ordering (NF-04).
func (p *Pool) Load(ctx context.Context) error {
	nodes, err := p.store.ListProxies(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodes = make(map[string]Node, len(nodes))
	for _, n := range nodes {
		p.nodes[n.ID] = n
	}
	return nil
}

// All returns every node sorted by ID.
func (p *Pool) All() []Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Node, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Available returns enabled nodes that are out of cooldown, ordered by the
// selection policy in design doc 3.7.2:
//
//  1. enabled and cooldownUntil <= now
//  2. ascending lastUsedAt -- least recently used first
//  3. never-used nodes (nil lastUsedAt) sort ahead of every used node
//  4. ties broken by ascending ID for a stable order
func (p *Pool) Available(now time.Time) []Node {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]Node, 0, len(p.nodes))
	for _, n := range p.nodes {
		if !n.Enabled || n.CoolingDown(now) {
			continue
		}
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.LastUsedAt == nil && b.LastUsedAt == nil:
			return a.ID < b.ID
		case a.LastUsedAt == nil:
			return true
		case b.LastUsedAt == nil:
			return false
		case a.LastUsedAt.Equal(*b.LastUsedAt):
			return a.ID < b.ID
		default:
			return a.LastUsedAt.Before(*b.LastUsedAt)
		}
	})
	return out
}

// Get returns a node by ID.
func (p *Pool) Get(id string) (Node, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	return n, ok
}

// Upsert adds or replaces a node and persists it.
func (p *Pool) Upsert(ctx context.Context, n Node) error {
	if n.ID == "" {
		return fmt.Errorf("proxies: node id is required")
	}
	if n.URL == "" {
		return fmt.Errorf("proxies: node %s has no url", n.ID)
	}
	p.mu.Lock()
	p.nodes[n.ID] = n
	p.mu.Unlock()
	return p.store.UpsertProxy(ctx, n)
}

// ReplaceAll swaps the whole pool for the supplied set, persisting each node
// and deleting the ones that are gone. Used by the "save proxy list" action.
func (p *Pool) ReplaceAll(ctx context.Context, nodes []Node) error {
	seen := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.ID == "" {
			return fmt.Errorf("proxies: node id is required")
		}
		if n.URL == "" {
			return fmt.Errorf("proxies: node %s has no url", n.ID)
		}
		seen[n.ID] = true
		if err := p.Upsert(ctx, n); err != nil {
			return err
		}
	}
	for _, existing := range p.All() {
		if seen[existing.ID] {
			continue
		}
		if err := p.Delete(ctx, existing.ID); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes a node.
func (p *Pool) Delete(ctx context.Context, id string) error {
	p.mu.Lock()
	delete(p.nodes, id)
	p.mu.Unlock()
	return p.store.DeleteProxy(ctx, id)
}

// MarkUsed stamps lastUsedAt and persists immediately. Persisting here (rather
// than batching) is what lets a second probe worker observe the stamp and skip
// this node (design doc 3.7.3 rule 4).
func (p *Pool) MarkUsed(ctx context.Context, id string, now time.Time) error {
	n, err := p.mutate(id, func(n *Node) { n.LastUsedAt = &now })
	if err != nil {
		return err
	}
	return p.store.UpsertProxy(ctx, n)
}

// MarkSuccess records a successful probe.
func (p *Pool) MarkSuccess(ctx context.Context, id string, latency time.Duration, now time.Time) error {
	ms := int(latency.Milliseconds())
	n, err := p.mutate(id, func(n *Node) {
		n.SuccessCount++
		n.ConsecutiveFailures = 0
		n.CooldownUntil = nil
		n.LastSuccess = &now
		n.LastLatencyMS = &ms
	})
	if err != nil {
		return err
	}
	return p.store.UpsertProxy(ctx, n)
}

// MarkFailure records a failure and applies the cooldown.
//
// resetAfterCooldown is set by the caller once the ladder has reached its cap:
// the counter then drops back to zero so the next failure starts at one minute
// again. Without it a node that has failed seven times would sit at the cap
// forever, and a proxy whose outage outlasts an hour would never be retried.
func (p *Pool) MarkFailure(ctx context.Context, id string, cooldown time.Duration, now time.Time, resetAfterCooldown bool) (Node, error) {
	n, err := p.mutate(id, func(n *Node) {
		n.FailureCount++
		n.ConsecutiveFailures++
		n.LastFailure = &now
		if cooldown > 0 {
			until := now.Add(cooldown)
			n.CooldownUntil = &until
		}
		if resetAfterCooldown {
			n.ConsecutiveFailures = 0
		}
	})
	if err != nil {
		return Node{}, err
	}
	return n, p.store.UpsertProxy(ctx, n)
}

// ResetFailure clears a node's cooldown and both failure counters, returning it
// to the pool immediately.
//
// The counters are cleared too, not just the cooldown: an operator pressing
// reset after fixing a proxy means "start over", and leaving the ladder where it
// was would put the node straight back into a long cooldown on its next hiccup.
func (p *Pool) ResetFailure(ctx context.Context, id string, now time.Time) (Node, error) {
	n, err := p.mutate(id, func(n *Node) {
		n.ConsecutiveFailures = 0
		n.FailureCount = 0
		n.CooldownUntil = nil
		n.LastFailure = nil
	})
	if err != nil {
		return Node{}, err
	}
	return n, p.store.UpsertProxy(ctx, n)
}

func (p *Pool) mutate(id string, fn func(*Node)) (Node, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	if !ok {
		return Node{}, fmt.Errorf("proxies: unknown node %q", id)
	}
	fn(&n)
	p.nodes[id] = n
	return n, nil
}
