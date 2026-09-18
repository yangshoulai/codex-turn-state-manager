package intercept

import (
	"net/http"

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// AuthResolver maps CPA's runtime AuthID back to the persistence key.
type AuthResolver interface {
	ResolveAuthID(authID string) (string, bool)
}

// Injector performs the two request-side interceptor stages.
type Injector struct {
	settings *settings.Manager
	states   *states.Registry
	auth     AuthResolver
	corr     *CorrelationManager
	logf     func(hostapi.LogLevel, string, map[string]any)
}

// InjectorConfig configures an Injector.
type InjectorConfig struct {
	Settings *settings.Manager
	States   *states.Registry
	Auth     AuthResolver
	Corr     *CorrelationManager
	Log      func(hostapi.LogLevel, string, map[string]any)
}

// NewInjector builds an injector.
func NewInjector(cfg InjectorConfig) *Injector {
	return &Injector{
		settings: cfg.Settings,
		states:   cfg.States,
		auth:     cfg.Auth,
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
	ActionCorrelated     = "correlated"
	ActionInjected       = "injected"
	ActionNoBinding      = "no_binding"
	ActionUnresolvedAuth = "unresolved_auth"
)

// BeforeAuth opens the correlation for a request.
//
// With the master switch off this is a pure pass-through: no marker is added
// and no state is recorded, so disabling the plugin leaves CPA's request
// handling byte-for-byte unchanged (design doc 3.3).
func (i *Injector) BeforeAuth(req *hostapi.InterceptedRequest) Decision {
	if !i.settings.Current().Capabilities().Enabled {
		return Decision{Action: ActionPassthrough}
	}
	if req.RequestID == "" {
		// Without a request id there is nothing to correlate on; skipping is
		// safer than injecting an ambiguous marker.
		return Decision{Action: ActionPassthrough}
	}

	i.corr.Sweep()
	i.corr.Begin(req.RequestID, req.Model, req.Provider)

	if req.Headers == nil {
		req.Headers = http.Header{}
	}
	req.Headers.Set(headers.Correlation, req.RequestID)
	return Decision{Action: ActionCorrelated}
}

// AfterAuth resolves the account chosen by the scheduler and injects state.
//
// The correlation header is always removed here, whether or not a binding was
// found: it is an internal marker and must never reach upstream.
func (i *Injector) AfterAuth(req *hostapi.InterceptedRequest) Decision {
	caps := i.settings.Current().Capabilities()

	// Resolve the account identity from whichever source is available. The
	// correlation is authoritative because it is what the scheduler recorded.
	authIndex := ""
	switch {
	case req.RequestID != "":
		if rec, ok := i.corr.Get(req.RequestID); ok {
			authIndex = rec.AuthIndex
		}
	}
	if authIndex == "" && req.AuthID != "" && i.auth != nil {
		if idx, ok := i.auth.ResolveAuthID(req.AuthID); ok {
			authIndex = idx
		}
	}
	if authIndex == "" && req.AuthID != "" {
		// Fall back to treating the runtime id as the key; this is wrong when
		// CPA ever makes the two diverge, so it is logged.
		authIndex = req.AuthID
	}

	if req.Headers != nil {
		// Always strip the marker, even when we inject nothing.
		req.Headers.Del(headers.Correlation)
	}
	if req.RequestID != "" {
		i.corr.AttachAuth(req.RequestID, req.AuthID, authIndex)
	}

	if !caps.Inject {
		return Decision{Action: ActionPassthrough, AuthIndex: authIndex}
	}
	if authIndex == "" {
		return Decision{Action: ActionUnresolvedAuth}
	}

	binding, status := i.states.Lookup(authIndex, req.Model)
	if !status.Usable() {
		return Decision{Action: ActionNoBinding, AuthIndex: authIndex}
	}

	if req.Headers == nil {
		req.Headers = http.Header{}
	}
	req.Headers.Set(headers.TurnState, binding.StateValue)
	if req.RequestID != "" {
		i.corr.MarkInjected(req.RequestID, binding.StateValue)
	}

	return Decision{Action: ActionInjected, AuthIndex: authIndex, StateLen: binding.StateLength}
}

// NoteAuthChoice records the scheduler's decision against a request. Called by
// the credential scheduler when it overrides CPA's choice.
func (i *Injector) NoteAuthChoice(requestID, authID, authIndex string) {
	if requestID == "" {
		return
	}
	i.corr.AttachAuth(requestID, authID, authIndex)
}

// Correlation exposes the manager, for the response side and diagnostics.
func (i *Injector) Correlation() *CorrelationManager { return i.corr }

func (i *Injector) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if i.logf != nil {
		i.logf(level, msg, fields)
	}
}
