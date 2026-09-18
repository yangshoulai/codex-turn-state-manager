package app

import (
	"context"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/intercept"
)

// This file is the plugin's request-path surface. The CPA ABI adapter
// (internal/pluginabi) translates host payloads into these calls and nothing
// else.
//
// Each entry point resolves the current settings snapshot once, so a switch
// flipped mid-request cannot produce a half-applied decision.

// BeforeAuth runs the first request interceptor stage.
//
// It opens the correlation that lets the later stages recover the account
// identity, because the AfterAuth payload does not carry AuthID.
func (a *App) BeforeAuth(req *hostapi.InterceptedRequest) intercept.Decision {
	return a.injector.BeforeAuth(req)
}

// AfterAuth runs the second request interceptor stage: it resolves the account
// and injects bound state.
func (a *App) AfterAuth(req *hostapi.InterceptedRequest) intercept.Decision {
	return a.injector.AfterAuth(req)
}

// ObserveResponse handles the streaming header-init callback.
//
// A nil response header set is treated as "no state present" rather than an
// error: the upstream response is still valid, it simply carries nothing the
// plugin wants.
func (a *App) ObserveResponse(ctx context.Context, resp hostapi.ResponseHeaders) intercept.CaptureResult {
	return a.collector.Observe(ctx, resp)
}

// ObserveFailure applies the self-healing rules to a failed response.
func (a *App) ObserveFailure(ctx context.Context, requestID string, status int, body []byte) intercept.FailureSignal {
	return a.collector.ObserveFailure(ctx, requestID, status, body)
}

// PickCredential asks the plugin which account to use for a request.
//
// The returned response either names an AuthID or requests delegation; the
// adapter must honour Delegate by falling through to CPA's built-in policy.
func (a *App) PickCredential(ctx context.Context, req hostapi.SchedulerPickRequest) hostapi.SchedulerPickResponse {
	decision := a.router.Decide(ctx, req)
	if !decision.Delegate && decision.AuthID != "" {
		// Record the choice so AfterAuth can resolve the persistence key even
		// if the correlation header was lost in transit.
		if authIndex, ok := a.accounts.ResolveAuthID(decision.AuthID); ok {
			a.injector.NoteAuthChoice(req.RequestID, decision.AuthID, authIndex)
		}
	}
	return decision
}

// Correlation exposes the correlation manager for diagnostics and tests.
func (a *App) Correlation() *intercept.CorrelationManager { return a.corr }
