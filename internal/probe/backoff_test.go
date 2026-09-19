package probe

import (
	"context"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
)

func testBackoff() Backoff {
	return Backoff{
		TTL:                 time.Hour,
		RefreshThresholdPct: 15,
		// Deterministic jitter: always the top of the range.
		Jitter: func(n time.Duration) time.Duration { return n },
	}
}

func TestBackoff_NextDelay(t *testing.T) {
	b := testBackoff()

	cases := []struct {
		name string
		out  Outcome
		want time.Duration
	}{
		// TTL 60m with a 15% threshold re-probes after 51m.
		{"target hit waits out the refresh window", OutcomeSuccessTarget, 51 * time.Minute},
		{"non-target length is retried in minutes, not seconds", OutcomeSuccessNonTarget, 5 * time.Minute},
		{"no proxy available", OutcomeNoProxyAvailable, 5 * time.Minute},
		{"traversal timed out", OutcomeTimeoutAllProxies, 5 * time.Minute},
		{"network error", OutcomeNetworkError, 2 * time.Minute},
		{"upstream 5xx", OutcomeUpstreamError, 2 * time.Minute},
		{"rate limited without Retry-After", OutcomeRateLimit, DefaultRetryAfter},
		{"auth error waits for CPA to refresh", OutcomeAuthError, 30 * time.Minute},
		{"model unsupported", OutcomeModelUnsupported, 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.NextDelay(tc.out); got != tc.want {
				t.Errorf("NextDelay(%s) = %s, want %s", tc.out, got, tc.want)
			}
		})
	}
}

// TestBackoff_NonTargetNeverRetriesEveryMinute guards the design's explicit
// warning that scanning every minute must not become probing every minute.
func TestBackoff_NonTargetNeverRetriesEveryMinute(t *testing.T) {
	// Worst case: jitter at its minimum.
	b := testBackoff()
	b.Jitter = func(time.Duration) time.Duration { return 0 }

	for _, out := range []Outcome{
		OutcomeSuccessNonTarget, OutcomeNoProxyAvailable, OutcomeTimeoutAllProxies,
		OutcomeNetworkError, OutcomeUpstreamError, OutcomeAuthError, OutcomeModelUnsupported,
	} {
		if got := b.NextDelay(out); got < NonTargetMinDelay {
			t.Errorf("NextDelay(%s) = %s, which is too aggressive", out, got)
		}
	}
}

func TestBackoff_NonTargetDelayIsJittered(t *testing.T) {
	b := testBackoff()
	b.Jitter = func(n time.Duration) time.Duration { return 0 }
	low := b.NextDelay(OutcomeSuccessNonTarget)

	b.Jitter = func(n time.Duration) time.Duration { return n }
	high := b.NextDelay(OutcomeSuccessNonTarget)

	if low != NonTargetMinDelay {
		t.Errorf("minimum delay = %s, want %s", low, NonTargetMinDelay)
	}
	if high != NonTargetMaxDelay {
		t.Errorf("maximum delay = %s, want %s", high, NonTargetMaxDelay)
	}
}

func TestBackoff_RateLimitHonoursRetryAfter(t *testing.T) {
	b := testBackoff()
	b.RetryAfter = 42 * time.Second
	if got := b.NextDelay(OutcomeRateLimit); got != 42*time.Second {
		t.Errorf("NextDelay(RATE_LIMIT) = %s, want 42s", got)
	}
}

func TestBackoff_TTLChangeMovesTheSuccessWindow(t *testing.T) {
	b := testBackoff()
	b.TTL = 20 * time.Minute
	b.RefreshThresholdPct = 25
	// 20m * (1 - 0.25) = 15m
	if got := b.NextDelay(OutcomeSuccessTarget); got != 15*time.Minute {
		t.Errorf("NextDelay(SUCCESS_TARGET) = %s, want 15m", got)
	}
}

// TestProxyCooldown_DoublesToTheCap pins the ladder an operator asked for:
// 1, 2, 4, 8, 16, 32, 64 minutes, holding at the top.
func TestProxyCooldown_DoublesToTheCap(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, time.Minute}, // never failed: the base delay, not zero
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 16 * time.Minute},
		{6, 32 * time.Minute},
		{7, 64 * time.Minute},
		{8, 64 * time.Minute}, // holds at the cap rather than growing
		{50, 64 * time.Minute},
		{-3, time.Minute},
	}
	for _, tc := range cases {
		if got := ProxyCooldown(tc.failures); got != tc.want {
			t.Errorf("ProxyCooldown(%d) = %s, want %s", tc.failures, got, tc.want)
		}
	}
}

// TestProxyCooldownReachedCap marks the rung at which the caller resets the
// counter, which is what keeps a long outage from pinning a node out of the
// pool forever.
func TestProxyCooldownReachedCap(t *testing.T) {
	for failures := 1; failures <= 6; failures++ {
		if ProxyCooldownReachedCap(failures) {
			t.Errorf("ProxyCooldownReachedCap(%d) = true, want false below the cap", failures)
		}
	}
	for _, failures := range []int{7, 8, 100} {
		if !ProxyCooldownReachedCap(failures) {
			t.Errorf("ProxyCooldownReachedCap(%d) = false, want true at or above the cap", failures)
		}
	}
}

// TestPoolMarkFailure_ResetsTheCounterAtTheCap is the pairing of the two: the
// ladder reaching its top is what clears the count, so the next failure starts
// at one minute again.
func TestPoolMarkFailure_ResetsTheCounterAtTheCap(t *testing.T) {
	ctx := context.Background()
	store := newMemProxyStore()
	pool := proxies.NewPool(store)
	if err := pool.Upsert(ctx, proxies.Node{ID: "a", URL: "http://127.0.0.1:1", Enabled: true}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	now := time.Now()

	// Walk the ladder. Each failure is recorded with the delay that count earns.
	for failures := 1; failures <= 6; failures++ {
		cooldown := ProxyCooldown(failures)
		if _, err := pool.MarkFailure(ctx, "acct", "a", cooldown, now, ProxyCooldownReachedCap(failures)); err != nil {
			t.Fatalf("MarkFailure(%d): %v", failures, err)
		}
	}
	if got := cooldownFor(pool, "acct", "a").ConsecutiveFailures; got != 6 {
		t.Fatalf("ConsecutiveFailures = %d, want 6 before the cap", got)
	}

	// The seventh reaches the cap and clears the count.
	if _, err := pool.MarkFailure(ctx, "acct", "a", ProxyCooldown(7), now, ProxyCooldownReachedCap(7)); err != nil {
		t.Fatalf("MarkFailure(7): %v", err)
	}
	row := cooldownFor(pool, "acct", "a")
	if row.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d after the cap, want 0", row.ConsecutiveFailures)
	}
	if row.FailureCount != 7 {
		t.Errorf("FailureCount = %d, want 7 (the lifetime counter must not be cleared)", row.FailureCount)
	}
	if ProxyCooldown(row.ConsecutiveFailures) != time.Minute {
		t.Errorf("the next failure would wait %s, want the base 1m", ProxyCooldown(row.ConsecutiveFailures))
	}
}

// TestPoolResetFailureClearsCooldownAndCounters covers the panel's reset
// button: "start over" has to mean the counters too, or the node lands straight
// back in a long cooldown on its next hiccup.
func TestPoolResetCooldownClearsCooldownAndCounters(t *testing.T) {
	ctx := context.Background()
	store := newMemProxyStore()
	pool := proxies.NewPool(store)
	if err := pool.Upsert(ctx, proxies.Node{ID: "a", URL: "http://127.0.0.1:1", Enabled: true}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	now := time.Now()

	for failures := 1; failures <= 4; failures++ {
		if _, err := pool.MarkFailure(ctx, "acct", "a", ProxyCooldown(failures), now, false); err != nil {
			t.Fatalf("MarkFailure: %v", err)
		}
	}
	if got := cooldownFor(pool, "acct", "a").ConsecutiveFailures; got != 4 {
		t.Fatalf("setup: ConsecutiveFailures = %d, want 4", got)
	}

	if err := pool.ResetCooldown(ctx, "acct", "a"); err != nil {
		t.Fatalf("ResetCooldown: %v", err)
	}
	if rows := pool.Cooldowns(); len(rows) != 0 {
		t.Errorf("Cooldowns() = %d rows after a reset, want 0", len(rows))
	}
	if got := len(pool.AvailableFor("acct", now)); got != 1 {
		t.Errorf("the node is still unavailable to the reset account (%d available)", got)
	}
}

// cooldownFor reads one pair's ledger row.
func cooldownFor(pool *proxies.Pool, authIndex, proxyID string) proxies.Cooldown {
	for _, row := range pool.CooldownsForProxy(proxyID) {
		if row.AuthIndex == authIndex {
			return row
		}
	}
	return proxies.Cooldown{}
}

// TestNonTargetDelay_GrowsWithTheRun is the guard on proxy load.
//
// A pair that never yields the right shape used to be retried every two to five
// minutes forever, and each round walks the pool -- which spends proxy
// reputation to learn the same thing repeatedly. The interval now grows with
// the run, so the steady-state request rate for such a pair falls by an order
// of magnitude while a transient wrong shape still gets a prompt retry.
func TestNonTargetDelay_GrowsWithTheRun(t *testing.T) {
	// Deterministic jitter: the bottom of the base range, so the growth is what
	// is being measured rather than the jitter.
	b := func(streak int) Backoff {
		return Backoff{
			Jitter:          func(time.Duration) time.Duration { return 0 },
			NonTargetStreak: streak,
			NonTargetCap:    30 * time.Minute,
		}
	}

	// The first few stay at the base cadence: a wrong shape twice in a row is
	// not yet evidence of anything permanent.
	for _, streak := range []int{1, 2, 3} {
		if got := b(streak).NextDelay(OutcomeSuccessNonTarget); got != NonTargetMinDelay {
			t.Errorf("streak %d: delay = %s, want the base %s", streak, got, NonTargetMinDelay)
		}
	}

	// Then it doubles, and stops at the cap rather than growing without bound.
	cases := map[int]time.Duration{
		4:  4 * time.Minute,
		5:  8 * time.Minute,
		6:  16 * time.Minute,
		7:  30 * time.Minute, // 32m clamped to the cap
		20: 30 * time.Minute,
	}
	for streak, want := range cases {
		if got := b(streak).NextDelay(OutcomeSuccessNonTarget); got != want {
			t.Errorf("streak %d: delay = %s, want %s", streak, got, want)
		}
	}

	// A stale value rides the same escalation: it means "come back later" for
	// the same reason, and it is not a node fault either.
	if got := b(6).NextDelay(OutcomeSuccessStale); got != 16*time.Minute {
		t.Errorf("a stale value at streak 6 waited %s, want 16m", got)
	}

	// Without a configured cap the default applies, so the setting is a knob
	// rather than a requirement.
	noCap := Backoff{Jitter: func(time.Duration) time.Duration { return 0 }, NonTargetStreak: 40}
	if got := noCap.NextDelay(OutcomeSuccessNonTarget); got != NonTargetDelayCapDefault {
		t.Errorf("with no cap configured, delay = %s, want %s", got, NonTargetDelayCapDefault)
	}

	// A successful hit is unaffected by any of this: the run has ended, and the
	// interval comes from the TTL rather than from anything above.
	hit := Backoff{TTL: time.Hour, Jitter: func(time.Duration) time.Duration { return 0 }}
	if got := hit.NextDelay(OutcomeSuccessTarget); got != time.Hour {
		t.Errorf("a target hit waited %s, want the TTL-derived interval", got)
	}
}
