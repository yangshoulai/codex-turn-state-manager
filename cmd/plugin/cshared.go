//go:build cshared

// C-ABI entry points for the CPA plugin shared library.
//
// Build with:
//
//	make build-shared   # or: go build -buildmode=c-shared -tags cshared ./cmd/plugin
//
// # Status
//
// This file deliberately exports almost nothing. The plugin's internals are
// complete and exercised by the development harness, but the exact CPA plugin
// ABI -- the registration symbol, the callbacks it expects, and how the host
// passes interceptor and scheduler payloads across the boundary -- has not been
// confirmed against a CPA instance or the plugin SDK. See AGENTS.md section 7
// and the design document's "关键技术验证清单".
//
// Inventing a plausible-looking registration surface here would be worse than
// leaving it empty: it would compile, load, and silently do nothing, and the
// failure would look like a CPA integration bug rather than a missing adapter.
//
// # What to add
//
// The adapter belongs in internal/pluginabi as an implementation of
// hostapi.Host, plus the exported callbacks CPA invokes. Each callback should
// translate the host payload into the corresponding internal type and call the
// matching app.App method:
//
//	InterceptRequestBeforeAuth -> app.BeforeAuth
//	InterceptRequestAfterAuth  -> app.AfterAuth
//	InterceptResponseHeaders   -> app.ObserveResponse
//	SchedulerPick              -> app.PickCredential
//
// Unverified assumptions to settle first (design document 8.1-8.3):
//
//  1. whether a custom header set in BeforeAuth survives to Scheduler/AfterAuth
//  2. whether SchedulerPickResponse can name a specific AuthID
//  3. the credential JSON layout behind host.auth.get
//  4. whether StreamChunkInterceptor at HeaderInitIndex = -1 carries
//     X-Codex-Turn-State
package main

/*
#include <stdlib.h>
*/
import "C"

// ABIVersion identifies the adapter surface this build expects. It exists so
// the shared library can be smoke-tested for loadability -- design document
// verification item 5 -- before the real registration surface exists.
const ABIVersion = 1

//export CodexTurnStateManagerABIVersion
func CodexTurnStateManagerABIVersion() C.int {
	return C.int(ABIVersion)
}

func main() {}
