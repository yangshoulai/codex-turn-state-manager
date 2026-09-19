package probe

import (
	"math/rand"
	"time"
)

// Outcome classifies the result of one probe attempt (one proxy, one pair).
type Outcome string

const (
	// OutcomeSuccessTarget means a state of exactly the target length came
	// back; the traversal stops here.
	OutcomeSuccessTarget Outcome = "SUCCESS_TARGET"
	// OutcomeSuccessNonTarget means a state came back but its length is not
	// target. Recorded, never bound.
	OutcomeSuccessNonTarget Outcome = "SUCCESS_NON_TARGET"
	// OutcomeSuccessStale is a value of the right shape that the upstream minted
	// too long ago to be worth binding. It is not a proxy fault and not a shape
	// problem: the node did its job and the upstream will mint a fresh value in
	// time.
	OutcomeSuccessStale Outcome = "SUCCESS_STALE"
	// OutcomeNoProxyAvailable means the pool had no usable node.
	OutcomeNoProxyAvailable Outcome = "PROBE_NO_PROXY_AVAILABLE"
	// OutcomeTimeoutAllProxies means the traversal hit maxProbeDuration.
	OutcomeTimeoutAllProxies Outcome = "PROBE_TIMEOUT_ALL_PROXIES"
	// OutcomeNetworkError means a genuine proxy fault.
	OutcomeNetworkError Outcome = "NETWORK_ERROR"
	// OutcomeRateLimit means upstream returned 429.
	OutcomeRateLimit Outcome = "RATE_LIMIT"
	// OutcomeAuthError means upstream rejected the credential.
	OutcomeAuthError Outcome = "AUTH_ERROR"
	// OutcomeModelUnsupported means the account cannot serve the model.
	OutcomeModelUnsupported Outcome = "MODEL_UNSUPPORTED"
	// OutcomeUpstreamError is the catch-all for an upstream rejection that is
	// neither a proxy fault nor a rate limit -- notably 5xx, and a 400 that is
	// not model-related. The design document's taxonomy does not name this
	// case, but it must be distinguished from NETWORK_ERROR, because cooling
	// down a healthy node for an upstream 500 would drain the pool.
	OutcomeUpstreamError Outcome = "UPSTREAM_ERROR"
	// OutcomeAborted means the master switch flipped mid-probe; the result is
	// discarded and nothing is bound.
	OutcomeAborted Outcome = "ABORTED"
)

// Terminal reports whether the outcome ends the proxy traversal early.
//
// AUTH_ERROR and MODEL_UNSUPPORTED are account- or model-level facts that no
// other proxy can change, so retrying them across the pool just burns quota
// (design doc 3.7.3, rule 2).
func (o Outcome) Terminal() bool {
	switch o {
	case OutcomeAuthError, OutcomeModelUnsupported, OutcomeAborted:
		return true
	default:
		return false
	}
}

// ProxyFault reports whether the outcome justifies cooling the node down.
// Upstream 400/401/403/429 are explicitly excluded (design doc 3.7.4).
func (o Outcome) ProxyFault() bool { return o == OutcomeNetworkError }

// Backoff decides how long to wait before probing a pair again.
type Backoff struct {
	// TTL is the current state TTL; the success path derives from it.
	TTL time.Duration
	// RefreshThresholdPct mirrors the settings value.
	RefreshThresholdPct int
	// RetryAfter is the upstream Retry-After hint for rate limiting.
	RetryAfter time.Duration
	// NonTargetStreak is how many consecutive wrong-shaped values this pair has
	// produced, including the one being scheduled for. The retry interval grows
	// with it, so a pair that will never yield the shape stops being asked
	// every few minutes.
	NonTargetStreak int
	// NonTargetCap bounds that growth. Zero means the default.
	NonTargetCap time.Duration
	// Jitter returns a duration in [0, n). Injectable for deterministic tests.
	Jitter func(n time.Duration) time.Duration
}

// Durations from the design doc, section 3.10.
const (
	NonTargetMinDelay = 2 * time.Minute
	NonTargetMaxDelay = 5 * time.Minute

	// NonTargetEscalateAfter is how many consecutive wrong-shaped answers a pair
	// may give at the base cadence before the interval starts growing.
	//
	// A few in a row is a transient -- the upstream has not settled, or is
	// answering a slightly different question than we asked -- and deserves the
	// same prompt retry as any other miss. Past that, the pair is telling us
	// this model does not yield the shape we want, and retrying every few
	// minutes only spends proxy reputation to learn the same thing again.
	NonTargetEscalateAfter = 3

	// NonTargetDelayCapDefault bounds the grown interval when nothing is
	// configured. Half an hour still notices a model that starts working again
	// within the hour, while cutting the request rate by an order of magnitude
	// for one that never does.
	NonTargetDelayCapDefault = 30 * time.Minute

	NoProxyDelay    = 5 * time.Minute
	TimeoutAllDelay = 5 * time.Minute
	NetworkErrDelay = 2 * time.Minute
	AuthErrDelay    = 30 * time.Minute
	ModelErrDelay   = 30 * time.Minute

	// DefaultRetryAfter is used when upstream sends 429 without a usable
	// Retry-After header.
	DefaultRetryAfter = 5 * time.Minute
)

// nonTargetDelay grows the retry interval with the length of the run.
//
// The base delay carries the jitter, and each step past the threshold doubles
// what the jitter produced, so two pairs in the same state do not converge on
// the same schedule. The cap is a floor on the request rate, not a deadline:
// past it the pair is still probed, just rarely, because the upstream may
// change its mind about a model at any time.
func (b Backoff) nonTargetDelay() time.Duration {
	delay := b.jitter(NonTargetMaxDelay-NonTargetMinDelay) + NonTargetMinDelay
	if b.NonTargetStreak <= NonTargetEscalateAfter {
		return delay
	}
	cap := b.NonTargetCap
	if cap <= 0 {
		cap = NonTargetDelayCapDefault
	}
	for i := NonTargetEscalateAfter; i < b.NonTargetStreak; i++ {
		delay *= 2
		if delay >= cap {
			return cap
		}
	}
	return delay
}

// NextDelay returns how long to wait before the next probe of this pair.
//
// Scanning and probing are different cadences: a non-target result must never
// schedule a one-minute retry (design doc 3.10).
func (b Backoff) NextDelay(o Outcome) time.Duration {
	switch o {
	case OutcomeSuccessTarget:
		// Re-probe as the binding enters its refresh window.
		pct := b.RefreshThresholdPct
		if pct < 0 {
			pct = 0
		}
		return time.Duration(float64(b.TTL) * (1 - float64(pct)/100))

	case OutcomeSuccessNonTarget, OutcomeSuccessStale:
		// A stale value is retried on the same cadence as a wrong-shaped one:
		// both mean "come back later", neither means "this node is bad".
		return b.nonTargetDelay()

	case OutcomeNoProxyAvailable:
		return NoProxyDelay

	case OutcomeTimeoutAllProxies:
		return TimeoutAllDelay

	case OutcomeNetworkError:
		return NetworkErrDelay

	case OutcomeUpstreamError:
		// Short retry: upstream 5xx is usually transient and no node was at
		// fault.
		return NetworkErrDelay

	case OutcomeRateLimit:
		if b.RetryAfter > 0 {
			return b.RetryAfter
		}
		return DefaultRetryAfter

	case OutcomeAuthError:
		// The credential is likely being refreshed by CPA; come back later.
		return AuthErrDelay

	case OutcomeModelUnsupported:
		return ModelErrDelay

	case OutcomeAborted:
		// The master switch is off; the scheduler will not run anyway.
		return NoProxyDelay

	default:
		return NetworkErrDelay
	}
}

func (b Backoff) jitter(n time.Duration) time.Duration {
	if n <= 0 {
		return 0
	}
	if b.Jitter != nil {
		return b.Jitter(n)
	}
	return time.Duration(rand.Int63n(int64(n)))
}

// proxyCooldownBase and proxyCooldownCap define the per-node failure ladder:
// 1, 2, 4, 8, 16, 32, 64 minutes, doubling and then holding at the cap. The
// counter is reset by the caller once the cap is reached, so the ladder starts
// again rather than pinning a node out of the pool forever -- a node that keeps
// failing is still tried once an hour, which is how a transient outage that
// lasts longer than an hour gets noticed at all.
const (
	proxyCooldownBase = time.Minute
	proxyCooldownCap  = 64 * time.Minute
)

// ProxyCooldown returns the cooldown for a node that has failed
// consecutivelyFailed times in a row, saturating at the top of the ladder.
func ProxyCooldown(consecutivelyFailed int) time.Duration {
	if consecutivelyFailed <= 0 {
		return proxyCooldownBase
	}
	cooldown := proxyCooldownBase
	for i := 1; i < consecutivelyFailed && cooldown < proxyCooldownCap; i++ {
		cooldown *= 2
		if cooldown > proxyCooldownCap {
			cooldown = proxyCooldownCap
		}
	}
	return cooldown
}

// ProxyCooldownReachedCap reports whether a failure count has reached the top of
// the ladder, which is when the counter is reset rather than left to climb.
func ProxyCooldownReachedCap(consecutivelyFailed int) bool {
	return ProxyCooldown(consecutivelyFailed) >= proxyCooldownCap
}
