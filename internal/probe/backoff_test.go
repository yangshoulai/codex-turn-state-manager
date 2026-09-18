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
		if _, err := pool.MarkFailure(ctx, "a", cooldown, now, ProxyCooldownReachedCap(failures)); err != nil {
			t.Fatalf("MarkFailure(%d): %v", failures, err)
		}
	}
	n, ok := pool.Get("a")
	if !ok {
		t.Fatal("node disappeared")
	}
	if n.ConsecutiveFailures != 6 {
		t.Fatalf("ConsecutiveFailures = %d, want 6 before the cap", n.ConsecutiveFailures)
	}

	// The seventh reaches the cap and clears the count.
	if _, err := pool.MarkFailure(ctx, "a", ProxyCooldown(7), now, ProxyCooldownReachedCap(7)); err != nil {
		t.Fatalf("MarkFailure(7): %v", err)
	}
	n, _ = pool.Get("a")
	if n.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d after the cap, want 0", n.ConsecutiveFailures)
	}
	if n.FailureCount != 7 {
		t.Errorf("FailureCount = %d, want 7 (the lifetime counter must not be cleared)", n.FailureCount)
	}
	if ProxyCooldown(n.ConsecutiveFailures) != time.Minute {
		t.Errorf("the next failure would wait %s, want the base 1m", ProxyCooldown(n.ConsecutiveFailures))
	}
}

// TestPoolResetFailureClearsCooldownAndCounters covers the panel's reset
// button: "start over" has to mean the counters too, or the node lands straight
// back in a long cooldown on its next hiccup.
func TestPoolResetFailureClearsCooldownAndCounters(t *testing.T) {
	ctx := context.Background()
	store := newMemProxyStore()
	pool := proxies.NewPool(store)
	if err := pool.Upsert(ctx, proxies.Node{ID: "a", URL: "http://127.0.0.1:1", Enabled: true}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	now := time.Now()

	for failures := 1; failures <= 4; failures++ {
		if _, err := pool.MarkFailure(ctx, "a", ProxyCooldown(failures), now, false); err != nil {
			t.Fatalf("MarkFailure: %v", err)
		}
	}
	if n, _ := pool.Get("a"); n.ConsecutiveFailures != 4 {
		t.Fatalf("setup: ConsecutiveFailures = %d, want 4", n.ConsecutiveFailures)
	}

	n, err := pool.ResetFailure(ctx, "a", now)
	if err != nil {
		t.Fatalf("ResetFailure: %v", err)
	}
	if n.ConsecutiveFailures != 0 || n.FailureCount != 0 {
		t.Errorf("counters = %d/%d after reset, want 0/0", n.ConsecutiveFailures, n.FailureCount)
	}
	if n.CooldownUntil != nil {
		t.Errorf("CooldownUntil = %v after reset, want cleared", n.CooldownUntil)
	}
	if !n.Healthy(now) {
		t.Error("node is still unhealthy after a reset")
	}
}

// TestOutcome_ProxyFault pins the rule that upstream 4xx responses must not
// evict a healthy proxy node.
func TestOutcome_ProxyFault(t *testing.T) {
	shouldCool := []Outcome{OutcomeNetworkError}
	mustNotCool := []Outcome{
		OutcomeAuthError, OutcomeRateLimit, OutcomeModelUnsupported,
		OutcomeUpstreamError, OutcomeSuccessTarget, OutcomeSuccessNonTarget,
	}

	for _, o := range shouldCool {
		if !o.ProxyFault() {
			t.Errorf("%s should be classified as a proxy fault", o)
		}
	}
	for _, o := range mustNotCool {
		if o.ProxyFault() {
			t.Errorf("%s must not cool down the proxy node", o)
		}
	}
}

// TestOutcome_Terminal pins the rule that account- and model-level failures
// abort the traversal instead of walking the rest of the pool.
func TestOutcome_Terminal(t *testing.T) {
	terminal := []Outcome{OutcomeAuthError, OutcomeModelUnsupported, OutcomeAborted}
	continueWalking := []Outcome{
		OutcomeSuccessNonTarget, OutcomeNetworkError, OutcomeRateLimit,
		OutcomeUpstreamError, OutcomeNoProxyAvailable, OutcomeTimeoutAllProxies,
	}

	for _, o := range terminal {
		if !o.Terminal() {
			t.Errorf("%s should terminate the traversal", o)
		}
	}
	for _, o := range continueWalking {
		if o.Terminal() {
			t.Errorf("%s should not terminate the traversal", o)
		}
	}
}
