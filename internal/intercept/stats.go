package intercept

import "sync/atomic"

// Stats counts what the request pipeline has actually done.
//
// This exists because "is the plugin injecting anything?" is otherwise
// unanswerable: injection changes a request header and leaves no trace anyone
// can query. Logging every injection would answer it, but at one line per
// outbound request that is not a trade a proxy should make. Counters answer it
// once, and stay useful in production rather than only during bring-up.
type Stats struct {
	requestsSeen   atomic.Int64
	injected       atomic.Int64
	unresolvedAuth atomic.Int64
	noBinding      atomic.Int64
	captured       atomic.Int64
	capturedReused atomic.Int64
	invalidated    atomic.Int64

	// Which response callback the host actually invokes. "Nothing was
	// captured" has two very different causes -- the callback never ran, or it
	// ran and the response carried no state -- and only these distinguish them.
	streamChunks  atomic.Int64
	streamHeaders atomic.Int64
	nonStream     atomic.Int64
}

// StatsSnapshot is a point-in-time copy for the panel.
type StatsSnapshot struct {
	// RequestsSeen counts requests that reached the after-auth stage.
	RequestsSeen int64 `json:"requestsSeen"`
	// Injected counts requests whose turn-state header the plugin rewrote.
	Injected int64 `json:"injected"`
	// UnresolvedAuth counts requests where no account could be determined,
	// which is the shape a broken correlation would take.
	UnresolvedAuth int64 `json:"unresolvedAuth"`
	// NoBinding counts requests served by an account with no usable state.
	NoBinding int64 `json:"noBinding"`
	// Captured counts state values harvested from ordinary traffic.
	Captured int64 `json:"captured"`
	// CapturedReused counts repeats of a value already held, which only extend
	// the TTL.
	CapturedReused int64 `json:"capturedReused"`
	// Invalidated counts bindings dropped by the self-healing rules.
	Invalidated int64 `json:"invalidated"`

	// StreamChunks counts streaming-response callbacks received.
	StreamChunks int64 `json:"streamChunks"`
	// StreamHeaders counts those that were the header-only initialisation call,
	// which is the only one carrying the upstream response headers.
	StreamHeaders int64 `json:"streamHeaders"`
	// NonStream counts non-streaming response callbacks received.
	NonStream int64 `json:"nonStream"`
}

// Snapshot returns the current counters.
func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		RequestsSeen:   s.requestsSeen.Load(),
		Injected:       s.injected.Load(),
		UnresolvedAuth: s.unresolvedAuth.Load(),
		NoBinding:      s.noBinding.Load(),
		Captured:       s.captured.Load(),
		CapturedReused: s.capturedReused.Load(),
		Invalidated:    s.invalidated.Load(),
		StreamChunks:   s.streamChunks.Load(),
		StreamHeaders:  s.streamHeaders.Load(),
		NonStream:      s.nonStream.Load(),
	}
}
