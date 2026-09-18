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

// InjectState runs the after-auth request interceptor stage: it looks up the
// account the host selected and rewrites the turn-state header when a usable
// binding exists.
//
// There is no before-auth stage. The design document originally specified one
// purely to plant a correlation marker, but the host publishes the selected
// account in Metadata, so the marker is unnecessary.
func (a *App) InjectState(req *hostapi.InterceptedRequest) intercept.Decision {
	return a.injector.Inject(req)
}

// ObserveStreamChunk handles the streaming response observation callback.
//
// Only the header-init call (ChunkIndex == StreamChunkHeaderInitIndex) carries
// the initial upstream headers, so payload chunks are ignored.
func (a *App) ObserveStreamChunk(ctx context.Context, chunk hostapi.StreamChunk) intercept.CaptureResult {
	return a.collector.Observe(ctx, chunk)
}

// ObserveResponse handles the non-streaming response interceptor.
func (a *App) ObserveResponse(ctx context.Context, chunk hostapi.StreamChunk) intercept.CaptureResult {
	return a.collector.ObserveResponse(ctx, chunk)
}

// ObserveCompletion applies the self-healing rules to a finished request. body
// may be nil; supplying it enables the body-based triggers.
func (a *App) ObserveCompletion(ctx context.Context, comp hostapi.Completion, body []byte) intercept.FailureSignal {
	return a.collector.ObserveCompletion(ctx, comp, body)
}

// PickCredential asks the plugin which account to use for a request.
//
// A response with Handled false means the plugin declined; the adapter must
// then let the host fall back to its own scheduler.
func (a *App) PickCredential(ctx context.Context, req hostapi.SchedulerPickRequest) hostapi.SchedulerPickResponse {
	return a.router.Decide(ctx, req)
}

// Correlation exposes the correlation manager for diagnostics and tests.
func (a *App) Correlation() *intercept.CorrelationManager { return a.corr }
