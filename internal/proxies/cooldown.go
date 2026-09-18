package proxies

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Cooldown is one (account, proxy) pair's failure state.
//
// Health belongs to the pair rather than to the proxy. The same node returns
// the target state length for one account and a non-target length for another,
// because the upstream decides per account; a node-level cooldown therefore
// lets one account's failures take a working node away from every other
// account. Everything here is keyed by both.
type Cooldown struct {
	AuthIndex           string     `json:"authIndex"`
	ProxyID             string     `json:"proxyId"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	FailureCount        int        `json:"failureCount"`
	CooldownUntil       *time.Time `json:"cooldownUntil,omitempty"`
	LastFailure         *time.Time `json:"lastFailure,omitempty"`
}

// CoolingDown reports whether the pair is still benched at `now`.
func (c Cooldown) CoolingDown(now time.Time) bool {
	return c.CooldownUntil != nil && now.Before(*c.CooldownUntil)
}

type cooldownKey struct {
	authIndex string
	proxyID   string
}

// cooldowns is the per-(account, proxy) failure ledger.
//
// Small like the pool itself -- accounts times nodes -- so it is kept resident
// and guarded by the pool's mutex rather than snapshotted.
type cooldowns struct {
	store Store
	byKey map[cooldownKey]Cooldown
}

func newCooldowns(store Store) *cooldowns {
	return &cooldowns{store: store, byKey: map[cooldownKey]Cooldown{}}
}

func (c *cooldowns) load(ctx context.Context) error {
	rows, err := c.store.ListCooldowns(ctx)
	if err != nil {
		return err
	}
	c.byKey = make(map[cooldownKey]Cooldown, len(rows))
	for _, row := range rows {
		c.byKey[cooldownKey{row.AuthIndex, row.ProxyID}] = row
	}
	return nil
}

// get returns the pair's state, or a zero value when it has never failed.
func (c *cooldowns) get(authIndex, proxyID string) Cooldown {
	row, ok := c.byKey[cooldownKey{authIndex, proxyID}]
	if !ok {
		return Cooldown{AuthIndex: authIndex, ProxyID: proxyID}
	}
	return row
}

// available reports whether the pair may be used at `now`.
func (c *cooldowns) available(authIndex, proxyID string, now time.Time) bool {
	return !c.get(authIndex, proxyID).CoolingDown(now)
}

// recordFailure advances the pair's ladder.
//
// resetAfterCooldown is set by the caller once the ladder has reached its cap:
// the counter drops to zero so the next failure starts at the base delay again,
// which is what keeps an outage longer than the cap from removing the pair
// permanently. The lifetime counter is never cleared by the ladder -- only by an
// explicit reset.
func (c *cooldowns) recordFailure(ctx context.Context, authIndex, proxyID string, cooldown time.Duration, now time.Time, resetAfterCooldown bool) (Cooldown, error) {
	row := c.get(authIndex, proxyID)
	row.ConsecutiveFailures++
	row.FailureCount++
	row.LastFailure = &now
	if cooldown > 0 {
		until := now.Add(cooldown)
		row.CooldownUntil = &until
	}
	if resetAfterCooldown {
		row.ConsecutiveFailures = 0
	}
	c.byKey[cooldownKey{authIndex, proxyID}] = row

	if err := c.store.UpsertCooldown(ctx, row); err != nil {
		return Cooldown{}, err
	}
	return row, nil
}

// recordSuccess clears the pair's streak but keeps the lifetime count, so the
// panel can still show how often this account has had trouble with this node.
func (c *cooldowns) recordSuccess(ctx context.Context, authIndex, proxyID string) error {
	row, ok := c.byKey[cooldownKey{authIndex, proxyID}]
	if !ok {
		return nil
	}
	if row.ConsecutiveFailures == 0 && row.CooldownUntil == nil {
		return nil
	}
	row.ConsecutiveFailures = 0
	row.CooldownUntil = nil
	c.byKey[cooldownKey{authIndex, proxyID}] = row
	return c.store.UpsertCooldown(ctx, row)
}

// reset clears one pair, counters included.
func (c *cooldowns) reset(ctx context.Context, authIndex, proxyID string) error {
	delete(c.byKey, cooldownKey{authIndex, proxyID})
	if err := c.store.DeleteCooldown(ctx, authIndex, proxyID); err != nil {
		return fmt.Errorf("proxies: reset cooldown: %w", err)
	}
	return nil
}

// resetAll clears every pair.
func (c *cooldowns) resetAll(ctx context.Context) error {
	c.byKey = map[cooldownKey]Cooldown{}
	if err := c.store.DeleteAllCooldowns(ctx); err != nil {
		return fmt.Errorf("proxies: reset cooldowns: %w", err)
	}
	return nil
}

// all returns every pair with a recorded failure, sorted for display.
func (c *cooldowns) all() []Cooldown {
	out := make([]Cooldown, 0, len(c.byKey))
	for _, row := range c.byKey {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AuthIndex != out[j].AuthIndex {
			return out[i].AuthIndex < out[j].AuthIndex
		}
		return out[i].ProxyID < out[j].ProxyID
	})
	return out
}

// forProxy returns the pairs recorded against one node.
func (c *cooldowns) forProxy(proxyID string) []Cooldown {
	out := make([]Cooldown, 0)
	for _, row := range c.byKey {
		if row.ProxyID == proxyID {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AuthIndex < out[j].AuthIndex })
	return out
}

// summary counts, per node, how many accounts currently have it benched.
//
// The node list cannot show a single health value any more -- that is the point
// of the change -- so it shows this instead and leaves the detail to the panel.
func (c *cooldowns) summary(now time.Time) map[string]int {
	out := map[string]int{}
	for _, row := range c.byKey {
		if row.CoolingDown(now) {
			out[row.ProxyID]++
		}
	}
	return out
}
