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
//
// There is deliberately no cooldown here. Availability depends on which account
// is asking -- the same node returns the target state length for one account and
// a non-target length for another -- so it lives in the per-pair ledger in
// cooldown.go. The counts below are the node's own lifetime totals, kept for the
// operator's benefit, and they no longer decide anything.
type Node struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`

	SuccessCount int `json:"successCount"`
	FailureCount int `json:"failureCount"`

	LastLatencyMS *int `json:"lastLatencyMs,omitempty"`

	// LastUsedAt is stamped the moment a node is picked, before the request is
	// sent, so concurrent probe workers never pick the same node (design doc
	// 3.7.3 rule 4).
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	LastSuccess *time.Time `json:"lastSuccess,omitempty"`
	LastFailure *time.Time `json:"lastFailure,omitempty"`
}

// Healthy reports whether the node may be used at all.
//
// "At all" is the whole of it now: whether it is usable *for a given account*
// is answered by Pool.AvailableFor, because that is a different question with a
// different answer per account.
func (n Node) Healthy() bool { return n.Enabled }

// Status renders the node state for the panel.
func (n Node) Status() string {
	if !n.Enabled {
		return "disabled"
	}
	return "healthy"
}

// Store persists pool state. Implemented by storage.ProxyStore.
type Store interface {
	ListProxies(ctx context.Context) ([]Node, error)
	UpsertProxy(ctx context.Context, n Node) error
	DeleteProxy(ctx context.Context, id string) error

	ListCooldowns(ctx context.Context) ([]Cooldown, error)
	UpsertCooldown(ctx context.Context, c Cooldown) error
	DeleteCooldown(ctx context.Context, authIndex, proxyID string) error
	DeleteAllCooldowns(ctx context.Context) error
}

// Pool is the in-memory view of the proxy pool. It is small (a handful of
// nodes) and read on every probe, so it is kept fully resident and guarded by
// a plain mutex rather than an atomic snapshot.
type Pool struct {
	store Store

	mu    sync.Mutex
	nodes map[string]Node
	// coolers is the per-(account, proxy) failure ledger. Guarded by mu.
	coolers *cooldowns
}

// NewPool builds an empty pool. Call Load to populate it.
func NewPool(store Store) *Pool {
	return &Pool{store: store, nodes: map[string]Node{}, coolers: newCooldowns(store)}
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
	// Cooldowns are loaded with the nodes: both are needed before the first
	// probe, and the panel reads them without a second round trip.
	if err := p.coolers.load(ctx); err != nil {
		return err
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

// AvailableFor returns the nodes one account may probe through, ordered by the
// selection policy in design doc 3.7.2:
//
//  1. enabled, and not in cooldown *for this account*
//  2. ascending lastUsedAt -- least recently used first
//  3. never-used nodes (nil lastUsedAt) sort ahead of every used node
//  4. ties broken by ascending ID for a stable order
//
// The account is part of the filter rather than an afterthought: a node that
// keeps failing for one account stays available to every other, which is what
// the ledger exists for. An empty result means every usable node is benched for
// this account, and the caller ends the round for it rather than waiting.
func (p *Pool) AvailableFor(authIndex string, now time.Time) []Node {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]Node, 0, len(p.nodes))
	for _, n := range p.nodes {
		if !n.Enabled || !p.coolers.available(authIndex, n.ID, now) {
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

// MarkSuccess records a successful probe for one account.
//
// The node's own counters still move -- how often a node works overall is worth
// seeing -- but availability is governed by the pair's ladder, so a success here
// only clears this account's streak.
func (p *Pool) MarkSuccess(ctx context.Context, authIndex, id string, latency time.Duration, now time.Time) error {
	ms := int(latency.Milliseconds())
	n, err := p.mutate(id, func(n *Node) {
		n.SuccessCount++
		n.LastSuccess = &now
		n.LastLatencyMS = &ms
	})
	if err != nil {
		return err
	}
	if err := p.store.UpsertProxy(ctx, n); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	return p.coolers.recordSuccess(ctx, authIndex, id)
}

// MarkFailure advances one account's ladder against one node.
//
// resetAfterCooldown is set by the caller once the ladder has reached its cap:
// the counter then drops back to zero so the next failure starts at the base
// delay again. Without it a pair that has failed seven times would sit at the
// cap forever, and an outage outlasting the cap would never be retried.
func (p *Pool) MarkFailure(ctx context.Context, authIndex, id string, cooldown time.Duration, now time.Time, resetAfterCooldown bool) (Cooldown, error) {
	// The node's lifetime failure count is a property of the node, so it still
	// moves; only the cooldown decision moved to the pair.
	n, err := p.mutate(id, func(n *Node) {
		n.FailureCount++
		n.LastFailure = &now
	})
	if err != nil {
		return Cooldown{}, err
	}
	if err := p.store.UpsertProxy(ctx, n); err != nil {
		return Cooldown{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	return p.coolers.recordFailure(ctx, authIndex, id, cooldown, now, resetAfterCooldown)
}

// ResetCooldown clears one account's state against one node, counters included.
//
// The counters go too, not just the cooldown: an operator pressing reset after
// fixing something means "start over", and leaving the ladder where it was would
// put the pair straight back into a long cooldown on its next hiccup.
func (p *Pool) ResetCooldown(ctx context.Context, authIndex, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.coolers.reset(ctx, authIndex, id)
}

// ResetAllCooldowns clears every pair. This is the panel's global reset.
func (p *Pool) ResetAllCooldowns(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.coolers.resetAll(ctx)
}

// Cooldowns returns every recorded pair, sorted for display.
func (p *Pool) Cooldowns() []Cooldown {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.coolers.all()
}

// CooldownsForProxy returns the pairs recorded against one node.
func (p *Pool) CooldownsForProxy(proxyID string) []Cooldown {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.coolers.forProxy(proxyID)
}

// CooldownSummary counts, per node, how many accounts have it benched right now.
//
// The node list cannot show one health value any more -- that is the point of
// the change -- so it shows this and leaves the detail to the panel.
func (p *Pool) CooldownSummary(now time.Time) map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.coolers.summary(now)
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
