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
}

// InjectorConfig configures an Injector.
type InjectorConfig struct {
	Settings *settings.Manager
	States   *states.Registry
	Corr     *CorrelationManager
	Log      func(hostapi.LogLevel, string, map[string]any)
}

// NewInjector builds an injector.
func NewInjector(cfg InjectorConfig) *Injector {
	return &Injector{
		settings: cfg.Settings,
		states:   cfg.States,
		corr:     cfg.Corr,
		logf:     cfg.Log,
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

	if !caps.Inject {
		// Master switch off: no injection, no bookkeeping beyond the record.
		return Decision{Action: ActionPassthrough, AuthIndex: req.AuthIndex}
	}
	if req.AuthIndex == "" {
		// The host did not publish the selected account. Nothing to look up.
		return Decision{Action: ActionUnresolvedAuth}
	}

	binding, status := i.states.Lookup(req.AuthIndex, req.Model)
	if !status.Usable() {
		return Decision{Action: ActionNoBinding, AuthIndex: req.AuthIndex}
	}

	if req.Headers == nil {
		req.Headers = http.Header{}
	}
	req.Headers.Set(headers.TurnState, binding.StateValue)
	if req.RequestID != "" {
		i.corr.MarkInjected(req.RequestID, binding.StateValue)
	}

	return Decision{Action: ActionInjected, AuthIndex: req.AuthIndex, StateLen: binding.StateLength}
}

// Correlation exposes the manager, for the response side and diagnostics.
func (i *Injector) Correlation() *CorrelationManager { return i.corr }

func (i *Injector) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if i.logf != nil {
		i.logf(level, msg, fields)
	}
}
