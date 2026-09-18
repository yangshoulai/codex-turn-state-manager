package probe

import (
	"testing"
	"time"
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

func TestProxyCooldown_LadderSaturates(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, time.Minute},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 5 * time.Minute},
		{4, 10 * time.Minute},
		{9, 10 * time.Minute}, // saturates rather than growing unbounded
		{-3, time.Minute},
	}
	for _, tc := range cases {
		if got := ProxyCooldown(tc.failures); got != tc.want {
			t.Errorf("ProxyCooldown(%d) = %s, want %s", tc.failures, got, tc.want)
		}
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
