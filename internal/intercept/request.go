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
}

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
}

// NewInjector builds an injector.
func NewInjector(cfg InjectorConfig) *Injector {
	if cfg.Stats == nil {
		cfg.Stats = &Stats{}
	}
	return &Injector{
		settings: cfg.Settings,
		states:   cfg.States,
		corr:     cfg.Corr,
		logf:     cfg.Log,
		stats:    cfg.Stats,
		calls:    cfg.Calls,
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

// Inject applies bound state to a request at the after-auth stage.
//
// The returned ClearHeaders is always empty: the plugin never has an internal
// marker to strip, because it never injected one.
func (i *Injector) Inject(req *hostapi.InterceptedRequest) Decision {
	caps := i.settings.Current().Capabilities()

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
