package intercept

import (
	"context"
	"strings"
	"sync"

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// Collector harvests turn-state values from response headers and runs the
// self-healing rules.
//
// Normal traffic becomes a second, free source of state: every successful
// response that carries a target-length value refreshes the binding for the
// account that served it, which is what keeps active probing rare
// (design doc 3.9).
type Collector struct {
	settings *settings.Manager
	states   *states.Registry
	corr     *CorrelationManager
	logf     func(hostapi.LogLevel, string, map[string]any)

	// serverErrors counts consecutive 5xx per pair. The threshold is a per-pair
	// run, not a per-request one, because a retry arrives with a fresh
	// request id (design doc 3.12).
	failMu       sync.Mutex
	serverErrors map[states.Pair]int
}

// CollectorConfig configures a Collector.
type CollectorConfig struct {
	Settings *settings.Manager
	States   *states.Registry
	Corr     *CorrelationManager
	Log      func(hostapi.LogLevel, string, map[string]any)
}

// NewCollector builds a collector.
func NewCollector(cfg CollectorConfig) *Collector {
	return &Collector{
		settings:     cfg.Settings,
		states:       cfg.States,
		corr:         cfg.Corr,
		logf:         cfg.Log,
		serverErrors: map[states.Pair]int{},
	}
}

// CaptureResult describes what a response yielded.
type CaptureResult struct {
	Action    string
	AuthIndex string
	StateLen  int
	Bound     bool
	Refreshed bool
}

// Capture actions.
const (
	CaptureSkipped     = "skipped"
	CaptureIgnored     = "ignored"
	CaptureNonTarget   = "non_target"
	CaptureBound       = "bound"
	CaptureRefreshed   = "refreshed"
	CaptureNoAuth      = "no_auth"
	CaptureCollectOnly = "collect_only"
)

// Observe handles the streaming header-init callback
// (StreamChunkHeaderInitIndex == -1).
//
// With the master switch off the response is ignored outright -- no capture, no
// TTL refresh, no rebind (design doc 3.3).
func (c *Collector) Observe(ctx context.Context, resp hostapi.ResponseHeaders) CaptureResult {
	caps := c.settings.Current().Capabilities()

	rec, hasRec := c.corr.Complete(resp.RequestID)

	if !caps.Capture {
		return CaptureResult{Action: CaptureSkipped, AuthIndex: rec.AuthIndex}
	}
	if !hasRec {
		return CaptureResult{Action: CaptureNoAuth}
	}

	authIndex := rec.AuthIndex
	if authIndex == "" {
		return CaptureResult{Action: CaptureNoAuth}
	}

	model := resp.Model
	if model == "" {
		model = rec.Model
	}
	if model == "" {
		return CaptureResult{Action: CaptureNoAuth, AuthIndex: authIndex}
	}

	value := headers.Get(resp.Header, headers.TurnState)
	if value == "" {
		return CaptureResult{Action: CaptureIgnored, AuthIndex: authIndex}
	}

	policy := c.settings.Current()
	if len(value) != policy.TargetStateLength {
		// Wrong length: recorded nowhere, bound never (F-15).
		return CaptureResult{Action: CaptureNonTarget, AuthIndex: authIndex, StateLen: len(value)}
	}

	prev, status := c.states.Lookup(authIndex, model)
	sameValue := status.Usable() && prev.StateValue == value

	if _, err := c.states.Bind(ctx, states.Binding{
		Pair:       states.Pair{AuthIndex: authIndex, Model: model},
		StateValue: value,
		Source:     states.SourceTraffic,
	}); err != nil {
		c.log(hostapi.LogError, "could not bind state from traffic", map[string]any{
			"authIndex": authIndex, "model": model, "error": err.Error(),
		})
		return CaptureResult{Action: CaptureIgnored, AuthIndex: authIndex, StateLen: len(value)}
	}
	c.resetServerErrors(states.Pair{AuthIndex: authIndex, Model: model})

	if sameValue {
		// Same value seen again: TTL extended, no history row.
		return CaptureResult{
			Action: CaptureRefreshed, AuthIndex: authIndex,
			StateLen: len(value), Refreshed: true,
		}
	}
	c.log(hostapi.LogInfo, "state captured from traffic", map[string]any{
		"authIndex": authIndex, "model": model, "length": len(value),
	})
	return CaptureResult{
		Action: CaptureBound, AuthIndex: authIndex,
		StateLen: len(value), Bound: true,
	}
}

// FailureSignal is what the collector decided to do about a failed request.
type FailureSignal struct {
	Invalidated bool
	AuthIndex   string
	Reason      string
}

// Self-heal triggers from design doc 3.12.
const (
	reasonResponseNotFound = "previous_response_not_found"
	reasonRoutingError     = "routing"
	consecutive5xxLimit    = 3
)

// ObserveFailure applies the self-healing rules to a failed request.
//
// Preconditions are strict on purpose: we only invalidate state that *this*
// request actually carried. Invalidating on unrelated failures would throw
// away good state and trigger pointless re-probing.
func (c *Collector) ObserveFailure(ctx context.Context, requestID string, status int, body []byte) FailureSignal {
	if !c.settings.Current().Capabilities().Enabled {
		return FailureSignal{}
	}

	rec, ok := c.corr.Get(requestID)
	if !ok || !rec.Injected {
		return FailureSignal{}
	}

	reason := ""
	switch {
	case status == 400 && bodyMentions(body, reasonResponseNotFound):
		reason = "upstream reported previous_response_not_found"
	case status == 400 && bodyMentions(body, reasonRoutingError):
		reason = "upstream reported a routing error"
	case status >= 500:
		reason = "upstream returned a server error"
	}
	if reason == "" {
		return FailureSignal{}
	}

	if status >= 500 && c.bumpServerErrors(states.Pair{AuthIndex: rec.AuthIndex, Model: rec.Model}) < consecutive5xxLimit {
		// One 5xx is noise; a run of them suggests the injected state is the
		// common factor.
		return FailureSignal{}
	}

	if err := c.states.Invalidate(ctx, states.Pair{AuthIndex: rec.AuthIndex, Model: rec.Model}); err != nil {
		c.log(hostapi.LogError, "could not invalidate binding after failure", map[string]any{
			"authIndex": rec.AuthIndex, "model": rec.Model, "error": err.Error(),
		})
		return FailureSignal{}
	}
	c.resetServerErrors(states.Pair{AuthIndex: rec.AuthIndex, Model: rec.Model})
	c.corr.Forget(requestID)
	c.log(hostapi.LogWarn, "binding invalidated after request failure; will re-probe", map[string]any{
		"authIndex": rec.AuthIndex, "model": rec.Model, "status": status, "reason": reason,
	})
	return FailureSignal{Invalidated: true, AuthIndex: rec.AuthIndex, Reason: reason}
}

func bodyMentions(body []byte, marker string) bool {
	if len(body) == 0 {
		return false
	}
	// Bounded scan: error bodies are small, and a truncated body must not turn
	// a genuine signal into a false negative.
	limit := len(body)
	if limit > 64<<10 {
		limit = 64 << 10
	}
	return strings.Contains(strings.ToLower(string(body[:limit])), marker)
}

// bumpServerErrors increments and returns the consecutive-5xx run for a pair.
func (c *Collector) bumpServerErrors(p states.Pair) int {
	c.failMu.Lock()
	defer c.failMu.Unlock()
	c.serverErrors[p]++
	return c.serverErrors[p]
}

func (c *Collector) resetServerErrors(p states.Pair) {
	c.failMu.Lock()
	delete(c.serverErrors, p)
	c.failMu.Unlock()
}

func (c *Collector) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if c.logf != nil {
		c.logf(level, msg, fields)
	}
}
