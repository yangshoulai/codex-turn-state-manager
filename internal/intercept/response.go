package intercept

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// Collector harvests turn-state values from responses and runs the
// self-healing rules.
//
// Normal traffic is a second, free source of state: every successful response
// carrying a target-length value refreshes the binding for the account that
// served it, which is what keeps active probing rare.
// PlanSource reports an account's subscription tier, which decides how many
// ciphertext blocks its state values carry.
type PlanSource interface {
	PlanType(authIndex string) string
}

type Collector struct {
	settings *settings.Manager
	states   *states.Registry
	plans    PlanSource
	corr     *CorrelationManager
	logf     func(hostapi.LogLevel, string, map[string]any)

	// serverErrors counts consecutive 5xx per pair. The threshold is a per-pair
	// run, not a per-request one, because a retry arrives with a fresh request
	// id.
	stats   *Stats
	signals func(authIndex string, signals headers.Signals)
	onStale func(pair states.Pair)
	// calls receives the response half of the call history. Optional.
	calls CallRecorder

	failMu       sync.Mutex
	serverErrors map[states.Pair]int
}

// CollectorConfig configures a Collector.
type CollectorConfig struct {
	Settings *settings.Manager
	States   *states.Registry
	// Plans supplies an account's subscription tier, which decides the shape
	// its state values must have. Optional: without it every account is judged
	// by the configured target length, which is the previous behaviour.
	Plans PlanSource
	Corr  *CorrelationManager
	Log   func(hostapi.LogLevel, string, map[string]any)
	// Stats receives the pipeline counters.
	Stats *Stats
	// Signals receives the account state the upstream reports. The probe path
	// reads the same headers, but its minimal request does not elicit them --
	// only real turns do, which is where this runs.
	Signals func(authIndex string, signals headers.Signals)
	// OnStale is told when a binding was discarded as stale, so the pair can be
	// probed again promptly.
	OnStale func(pair states.Pair)
	// Calls receives what upstream answered and how the request ended.
	Calls CallRecorder
}

// NewCollector builds a collector.
func NewCollector(cfg CollectorConfig) *Collector {
	if cfg.Stats == nil {
		cfg.Stats = &Stats{}
	}
	return &Collector{
		settings:     cfg.Settings,
		states:       cfg.States,
		plans:        cfg.Plans,
		corr:         cfg.Corr,
		logf:         cfg.Log,
		stats:        cfg.Stats,
		signals:      cfg.Signals,
		onStale:      cfg.OnStale,
		calls:        cfg.Calls,
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
	CaptureSkipped       = "skipped"
	CaptureIgnored       = "ignored"
	CaptureNonTarget     = "non_target"
	CaptureBound         = "bound"
	CaptureRefreshed     = "refreshed"
	CaptureNoAuth        = "no_auth"
	CaptureNotHeaderInit = "not_header_init"
	// CaptureStale means the response carried a different, unusable value, so
	// the binding was discarded and the pair queued for re-probing.
	CaptureStale = "stale"
)

// Observe handles the stream header-init callback
// (ChunkIndex == StreamChunkHeaderInitIndex).
//
// With the master switch off the response is ignored outright -- no capture, no
// TTL refresh, no rebind.
func (c *Collector) Observe(ctx context.Context, chunk hostapi.StreamChunk) CaptureResult {
	c.stats.streamChunks.Add(1)
	if !chunk.IsHeaderInit() {
		// Only the header-only call carries the initial upstream headers.
		return CaptureResult{Action: CaptureNotHeaderInit}
	}
	c.stats.streamHeaders.Add(1)

	// Resolve the account here rather than leaving it to capture().
	//
	// The streaming payload does not carry selected_auth_index -- measured, not
	// assumed -- so the only reliable source is the correlation record the
	// injector wrote. capture() consumes that record, so this has to run first.
	authIndex := chunk.AuthIndex
	rec, correlated := c.corr.Get(chunk.RequestID)
	if authIndex == "" && correlated {
		authIndex = rec.AuthIndex
	}

	// Snapshot before capture, so the record reflects what arrived rather than
	// what happened to it.
	c.stats.recordHeaderInit(HeaderInitSnapshot{
		RequestID:   chunk.RequestID,
		Model:       chunk.Model,
		AuthIndex:   authIndex,
		Correlated:  correlated,
		HeaderCount: len(chunk.ResponseHeaders),
		HeaderNames: headerNames(chunk.ResponseHeaders),
		HasState:    chunk.ResponseHeaders.Get(headers.TurnState) != "",
		StateLength: len(chunk.ResponseHeaders.Get(headers.TurnState)),
	})

	return c.capture(ctx, chunk.RequestID, chunk.Model, authIndex, chunk.ResponseHeaders, chunk.StatusCode)
}

// ObserveResponse handles the non-streaming response interceptor, which is the
// simpler of the two paths for observing a state value.
func (c *Collector) ObserveResponse(ctx context.Context, chunk hostapi.StreamChunk) CaptureResult {
	c.stats.nonStream.Add(1)
	return c.capture(ctx, chunk.RequestID, chunk.Model, chunk.AuthIndex, chunk.ResponseHeaders, chunk.StatusCode)
}

func (c *Collector) capture(
	ctx context.Context,
	requestID, model, authIndex string,
	header http.Header,
	statusCode int,
) CaptureResult {
	// Recorded first, before any of this function's own decisions. The call log
	// describes what happened on the wire; whether the plugin chose to bind the
	// value is a separate question from whether upstream sent one, and an
	// operator debugging a binding that never appears needs the second answer
	// even when the first is "we ignored it".
	if c.calls != nil {
		c.calls.Response(requestID, headers.Get(header, headers.TurnState), statusCode)
	}

	caps := c.settings.Current().Capabilities()

	rec, hasRec := c.corr.Complete(requestID)

	if !caps.Capture {
		c.stats.skipSwitchOff.Add(1)
		return CaptureResult{Action: CaptureSkipped, AuthIndex: authIndex}
	}

	// Prefer the account the host published on this payload; fall back to what
	// the injection stage recorded.
	if authIndex == "" && hasRec {
		authIndex = rec.AuthIndex
	}
	if authIndex == "" {
		c.stats.skipNoAuth.Add(1)
		return CaptureResult{Action: CaptureNoAuth}
	}

	// The plan and rate-limit windows ride along on every real response, and
	// CPA exposes neither to plugins. It is the only place to learn them, and
	// it costs nothing -- but it is still a response-header read, so it sits
	// behind the same switch as the rest and is skipped with it.
	signals := headers.ParseSignals(header)
	if c.signals != nil && !signals.Empty() {
		c.signals(authIndex, signals)
	}

	if model == "" && hasRec {
		model = rec.Model
	}
	if model == "" {
		c.stats.skipNoAuth.Add(1)
		return CaptureResult{Action: CaptureNoAuth, AuthIndex: authIndex}
	}

	value := headers.Get(header, headers.TurnState)
	if value == "" {
		c.stats.skipNoState.Add(1)
		// What arrived is recorded in the last-header-init snapshot rather than
		// logged: the log path dropped these fields regardless of their type.
		return CaptureResult{Action: CaptureIgnored, AuthIndex: authIndex}
	}

	pair := states.Pair{AuthIndex: authIndex, Model: model}
	policy := c.settings.Current()

	// The response is compared against what this request carried.
	//
	// A different value means the token the plugin is holding is no longer the
	// one upstream issues, and the binding has to be reconciled rather than
	// trusted until its TTL lapses.
	injected := ""
	if hasRec && rec.Injected {
		injected = rec.StateValue
	}
	unchanged := injected != "" && injected == value

	// Shape, not length: the account's plan decides how many ciphertext blocks
	// its values carry, and a personal account's correct shape is a team
	// account's wrong one. The plan on this response wins over the stored one,
	// being newer.
	plan := signals.PlanType
	if plan == "" && c.plans != nil {
		plan = c.plans.PlanType(authIndex)
	}
	env, shapeOK := states.AcceptState(value, states.ExpectationFor(plan, policy.TargetStateLength))
	if !shapeOK {
		// Wrong shape: bound never (F-15).
		c.stats.skipNonTarget.Add(1)
		if unchanged {
			// We sent this value and got it back; it is simply not a shape
			// this plugin binds. Nothing to reconcile.
			return CaptureResult{Action: CaptureNonTarget, AuthIndex: authIndex, StateLen: len(value)}
		}
		// A value of the wrong shape that differs from what we hold says the
		// binding describes a token upstream no longer issues. Drop it -- TTL
		// and all -- and let the next scan probe, rather than keep injecting a
		// value the upstream has already moved past.
		return c.discardStale(ctx, pair, value, injected)
	}

	prev, status := c.states.Lookup(authIndex, model)
	sameValue := unchanged || (status.Usable() && prev.StateValue == value)
	if _, err := c.states.Bind(ctx, states.Binding{
		Pair:       pair,
		StateValue: value,
		// A value that reaches us already past its life is recorded and
		// dropped: binding it would replace a working binding with a dead one.
		IssuedAt: env.Issued,
		Source:   states.SourceTraffic,
	}); err != nil {
		if errors.Is(err, states.ErrStateExpired) {
			c.stats.skipStale.Add(1)
			return CaptureResult{Action: CaptureStale, AuthIndex: authIndex, StateLen: len(value)}
		}
		c.log(hostapi.LogError, "could not bind state from traffic", map[string]any{
			"authIndex": authIndex, "model": model, "error": err.Error(),
		})
		return CaptureResult{Action: CaptureIgnored, AuthIndex: authIndex, StateLen: len(value)}
	}
	c.resetServerErrors(pair)

	if sameValue {
		// Same value seen again: TTL extended, no history row.
		c.stats.capturedReused.Add(1)
		return CaptureResult{
			Action: CaptureRefreshed, AuthIndex: authIndex,
			StateLen: len(value), Refreshed: true,
		}
	}
	c.stats.captured.Add(1)
	c.log(hostapi.LogInfo, "state captured from traffic", map[string]any{
		"authIndex": authIndex, "model": model, "length": len(value),
		"replaced": injected != "" && injected != value,
	})
	return CaptureResult{
		Action: CaptureBound, AuthIndex: authIndex,
		StateLen: len(value), Bound: true,
	}
}

// discardStale drops a binding whose value upstream has moved past, and clears
// the pair's probe backoff so the next scan refills it.
//
// Without this the plugin keeps injecting a token the upstream no longer
// issues, for as long as the old TTL lasts, while the probe that would fix it
// waits out a backoff scheduled before the problem existed.
func (c *Collector) discardStale(ctx context.Context, pair states.Pair, value, injected string) CaptureResult {
	_, status := c.states.Lookup(pair.AuthIndex, pair.Model)
	if status == states.StatusMissing {
		// Nothing was held, so nothing is stale. The pair is already eligible
		// for probing, and clearing its backoff here would undo a deliberate
		// one -- a MODEL_UNSUPPORTED cooldown, say -- and turn every response
		// into a reason to probe again.
		return CaptureResult{Action: CaptureNonTarget, AuthIndex: pair.AuthIndex, StateLen: len(value)}
	}

	if err := c.states.Invalidate(ctx, pair); err != nil {
		c.log(hostapi.LogError, "could not discard stale binding", map[string]any{
			"authIndex": pair.AuthIndex, "model": pair.Model, "error": err.Error(),
		})
		return CaptureResult{Action: CaptureIgnored, AuthIndex: pair.AuthIndex, StateLen: len(value)}
	}
	c.stats.staleDiscarded.Add(1)
	c.log(hostapi.LogWarn, "discarded stale binding: upstream returned a non-target length", map[string]any{
		"authIndex":   pair.AuthIndex,
		"model":       pair.Model,
		"injectedLen": len(injected),
		"returnedLen": len(value),
	})

	if c.onStale != nil {
		// Let the next scan probe this pair instead of waiting out a backoff
		// that was scheduled before the binding was known to be stale.
		c.onStale(pair)
	}
	return CaptureResult{Action: CaptureStale, AuthIndex: pair.AuthIndex, StateLen: len(value)}
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

// ObserveCompletion applies the self-healing rules to a finished request.
//
// The host reports the outcome and status directly, which is a firmer signal
// than sniffing response bodies; body may be nil. Preconditions are strict on
// purpose: the plugin only invalidates state that *this* request actually
// carried, so an unrelated failure never throws away good state.
func (c *Collector) ObserveCompletion(ctx context.Context, comp hostapi.Completion, body []byte) FailureSignal {
	// The call log is written before the self-healing rules run, because every
	// request belongs in it -- including the ones the rules below decide to do
	// nothing about, which are the majority.
	if c.calls != nil {
		c.calls.Complete(comp.RequestID, comp.StatusCode, string(comp.Outcome))
	}

	if !c.settings.Current().Capabilities().Enabled {
		return FailureSignal{}
	}

	rec, ok := c.corr.Get(comp.RequestID)
	if !ok || !rec.Injected {
		return FailureSignal{}
	}

	// Rejected means one of our own interceptors stopped the request, and
	// canceled means the client went away. Neither implicates the state.
	if comp.Outcome != hostapi.CompletionFailed {
		c.corr.Forget(comp.RequestID)
		return FailureSignal{}
	}

	reason := ""
	switch {
	case comp.StatusCode == 400 && bodyMentions(body, reasonResponseNotFound):
		reason = "upstream reported previous_response_not_found"
	case comp.StatusCode == 400 && bodyMentions(body, reasonRoutingError):
		reason = "upstream reported a routing error"
	case comp.StatusCode >= 500:
		reason = "upstream returned a server error"
	}
	if reason == "" {
		return FailureSignal{}
	}

	pair := states.Pair{AuthIndex: rec.AuthIndex, Model: rec.Model}

	if comp.StatusCode >= 500 {
		if c.bumpServerErrors(pair) < consecutive5xxLimit {
			// One 5xx is noise; a run of them suggests the injected state is
			// the common factor.
			return FailureSignal{}
		}
	}

	if err := c.states.Invalidate(ctx, pair); err != nil {
		c.log(hostapi.LogError, "could not invalidate binding after failure", map[string]any{
			"authIndex": rec.AuthIndex, "model": rec.Model, "error": err.Error(),
		})
		return FailureSignal{}
	}
	c.stats.invalidated.Add(1)
	c.resetServerErrors(pair)
	c.corr.Forget(comp.RequestID)
	c.log(hostapi.LogWarn, "binding invalidated after request failure; will re-probe", map[string]any{
		"authIndex": rec.AuthIndex, "model": rec.Model,
		"status": comp.StatusCode, "reason": reason,
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
