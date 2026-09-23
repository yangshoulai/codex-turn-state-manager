package intercept

import (
	"net/http"

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// Injector rewrites outbound requests that have a usable binding.
//
// There is only one stage. The host runs request.intercept_before before
// credential selection, so no account is known yet and there is nothing to look
// up; the state is injected at request.intercept_after, by which point the host
// has published the chosen account in Metadata.
type Injector struct {
	settings *settings.Manager
	states   *states.Registry
	corr     *CorrelationManager
	logf     func(hostapi.LogLevel, string, map[string]any)
	stats    *Stats

	// calls records one row per request. Optional.
	calls CallRecorder
	// clock rewrites the timezone a request declares about its caller. Optional:
	// without it no request is touched for that reason.
	clock ClockRewriter
	// tzStats receives the timezone counters. Never nil after construction.
	tzStats *TimezoneStats
}

// ClockResult is what one timezone rewrite attempt did.
type ClockResult struct {
	// Body is what to send. Identical to the input when nothing changed.
	Body []byte
	// From is the zone the request declared, empty when it declared none.
	From string
	// Ran reports whether the attempt was made at all. False means the
	// configuration named no usable target, which is a different fact from
	// "this request carried no timezone" and has to be counted separately: an
	// operator told "no marker" would go looking at their client when the
	// problem is the field they left empty.
	Ran bool
}

// ClockRewriter replaces the timezone markers in a request payload.
//
// A function rather than an interface, matching the other optional hooks here.
// It must return the input slice untouched when it changes nothing, so the
// caller can tell whether to send a replacement body.
type ClockRewriter func(body []byte) ClockResult

// CallRecorder receives the request-side half of the call history.
//
// Declared as an interface rather than taking *callhistory.Recorder so this
// package stays independent of the store, and so a nil recorder is a working
// configuration rather than a panic.
type CallRecorder interface {
	Begin(requestID, authIndex, model, carried, injected string)
	Response(requestID, state string, status int)
	Complete(requestID string, status int, outcome string)
}

// InjectorConfig configures an Injector.
type InjectorConfig struct {
	Settings *settings.Manager
	States   *states.Registry
	Corr     *CorrelationManager
	Log      func(hostapi.LogLevel, string, map[string]any)
	Stats    *Stats
	// Calls receives what each request carried and what was injected into it.
	Calls CallRecorder
	// Clock rewrites the timezone a request declares. Optional.
	Clock ClockRewriter
	// TimezoneStats receives the timezone counters. Optional.
	TimezoneStats *TimezoneStats
}

// NewInjector builds an injector.
func NewInjector(cfg InjectorConfig) *Injector {
	if cfg.Stats == nil {
		cfg.Stats = &Stats{}
	}
	if cfg.TimezoneStats == nil {
		cfg.TimezoneStats = &TimezoneStats{}
	}
	return &Injector{
		settings: cfg.Settings,
		states:   cfg.States,
		corr:     cfg.Corr,
		logf:     cfg.Log,
		stats:    cfg.Stats,
		calls:    cfg.Calls,
		clock:    cfg.Clock,
		tzStats:  cfg.TimezoneStats,
	}
}

// Decision explains what the injector did, for the panel and for tests.
type Decision struct {
	Action    string
	AuthIndex string
	StateLen  int
}

// Decision actions.
const (
	ActionPassthrough    = "passthrough"
	ActionInjected       = "injected"
	ActionNoBinding      = "no_binding"
	ActionUnresolvedAuth = "unresolved_auth"
)

// Inject applies bound state to a request at the after-auth stage, and rewrites
// the timezone it declares.
//
// The returned ClearHeaders is always empty: the plugin never has an internal
// marker to strip, because it never injected one.
func (i *Injector) Inject(req *hostapi.InterceptedRequest) Decision {
	caps := i.settings.Current().Capabilities()

	// Before anything that can return early. Turn-state injection is conditional
	// on a binding existing; the timezone rewrite is not conditional on anything
	// this function decides, and a request with no binding is still a request
	// whose timezone the operator asked to convert.
	i.rewriteTimezone(req)

	if req.RequestID != "" {
		i.corr.Sweep()
		// Record unconditionally so the response stage can find the pair even
		// when we inject nothing, and so self-healing can tell "we left this
		// request alone" from "we do not know what happened".
		i.corr.Record(req.RequestID, req.Model, req.AuthID, req.AuthIndex)
	}

	i.stats.requestsSeen.Add(1)

	if !caps.Inject {
		// Master switch off: no injection, no bookkeeping beyond the record.
		return Decision{Action: ActionPassthrough, AuthIndex: req.AuthIndex}
	}
	if req.AuthIndex == "" {
		// The host did not publish the selected account. Nothing to look up.
		i.stats.unresolvedAuth.Add(1)
		return Decision{Action: ActionUnresolvedAuth}
	}

	// Read before anything is written: this is what the request already carried,
	// which is the only way to tell "we injected this and upstream echoed it"
	// from "the client sent it and upstream kept it".
	carried := ""
	if req.Headers != nil {
		carried = headers.Get(req.Headers, headers.TurnState)
	}

	binding, status := i.states.Lookup(req.AuthIndex, req.Model)
	if !status.Usable() {
		i.stats.noBinding.Add(1)
		i.beginCall(req, carried, "")
		return Decision{Action: ActionNoBinding, AuthIndex: req.AuthIndex}
	}

	if req.Headers == nil {
		req.Headers = http.Header{}
	}
	req.Headers.Set(headers.TurnState, binding.StateValue)
	if req.RequestID != "" {
		i.corr.MarkInjected(req.RequestID, binding.StateValue)
	}
	i.beginCall(req, carried, binding.StateValue)

	// Logged at debug: this is the one place the plugin changes a user's
	// outbound request, and "did injection actually happen" is otherwise
	// unanswerable from outside the process.
	i.stats.injected.Add(1)
	i.log(hostapi.LogDebug, "injected turn state into request", map[string]any{
		"requestId": req.RequestID,
		"authIndex": req.AuthIndex,
		"model":     req.Model,
		"length":    binding.StateLength,
	})

	return Decision{Action: ActionInjected, AuthIndex: req.AuthIndex, StateLen: binding.StateLength}
}

// rewriteTimezone replaces the timezone the request declares about its caller.
//
// Behind the master switch and its own setting, and it does nothing at all when
// the configured target is empty or cannot be loaded -- an enabled switch with
// no usable target must not convert anything, and must not fail the request
// either.
func (i *Injector) rewriteTimezone(req *hostapi.InterceptedRequest) {
	if i.clock == nil || len(req.Body) == 0 {
		return
	}
	// One snapshot for the whole decision, like every other capability.
	values := i.settings.Current()
	if !values.Capabilities().Enabled || !values.TimezoneConversionEnabled {
		i.tzStats.RecordDisabled()
		return
	}
	result := i.clock(req.Body)
	if !result.Ran {
		// Enabled, but no usable target. Counted apart from "no marker",
		// because the two point at different things to fix.
		i.tzStats.RecordDisabled()
		return
	}
	changed := len(result.Body) > 0 && &result.Body[0] != &req.Body[0]
	if changed {
		req.Body = result.Body
	}
	i.tzStats.Record(result.From != "", changed)
	if !changed {
		return
	}
	i.log(hostapi.LogDebug, "rewrote the timezone a request declared", map[string]any{
		"requestId": req.RequestID,
		"model":     req.Model,
		"from":      result.From,
		"to":        values.TimezoneTarget,
	})
}

// Correlation exposes the manager, for the response side and diagnostics.
func (i *Injector) Correlation() *CorrelationManager { return i.corr }

// beginCall records the request-side half of the call history.
//
// Failures here are impossible by construction -- the recorder buffers in
// memory -- but it is still called last and guarded, because a diagnostic must
// never be able to break the request it is describing.
func (i *Injector) beginCall(req *hostapi.InterceptedRequest, carried, injected string) {
	if i.calls == nil {
		return
	}
	i.calls.Begin(req.RequestID, req.AuthIndex, req.Model, carried, injected)
}

func (i *Injector) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if i.logf != nil {
		i.logf(level, msg, fields)
	}
}
