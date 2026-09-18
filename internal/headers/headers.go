// Package headers holds the wire names of the headers the plugin reads or
// writes.
//
// They live in their own package so that the probe engine and the request
// pipeline can share them without either depending on the other.
package headers

import "net/http"

// TurnState is the upstream sticky-routing token. Codex treats it as a
// per-turn token; the plugin's cross-turn reuse is the experimental part.
const TurnState = "X-Codex-Turn-State"

// Correlation links an intercepted request across the BeforeAuth, Scheduler
// and AfterAuth stages, because AfterAuth does not expose AuthID directly.
//
// It is an internal marker: it must always be stripped before the request
// leaves CPA. See intercept.RequestInjector.
const Correlation = "X-CPA-Turn-State-Correlation"

// Get reads a header case-insensitively.
func Get(h http.Header, name string) string {
	if h == nil {
		return ""
	}
	return h.Get(name)
}
