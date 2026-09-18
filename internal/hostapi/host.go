// Package hostapi is the port through which the plugin talks to CPA.
//
// Nothing outside this package may import a CPA SDK type. The plugin is loaded
// as a C-ABI shared library and CPA's Go types are not available to us at
// compile time, so all host interaction is expressed as plain Go interfaces
// here and adapted at the ABI edge (see internal/pluginabi).
//
// This keeps the entire domain testable against MockHost, and confines the
// still-unverified parts of the CPA integration (see the design doc, section
// 8 "关键技术验证清单") to a single adapter package.
package hostapi

import (
	"context"
	"net/http"
	"time"
)

// Provider identifiers.
const (
	ProviderCodex = "codex"
)

// Account lifecycle states as reported by CPA.
const (
	AccountStatusAvailable   = "available"
	AccountStatusUnavailable = "unavailable"
	AccountStatusDisabled    = "disabled"
)

// Account is one entry of CPA's auth pool.
//
// AuthIndex is the plugin's stable persistence key; AuthID is CPA's runtime
// identifier. The two are distinct and AccountRegistry owns the mapping
// between them -- see the naming convention in AGENTS.md.
type Account struct {
	AuthIndex string
	AuthID    string
	Provider  string
	Label     string
	Status    string
	Priority  int
	Disabled  bool
}

// Credential is the credential JSON CPA holds for an account.
type Credential struct {
	AccessToken string
	ExpiresAt   time.Time
	// Raw is the decoded credential document. Never log it and never persist
	// it (NF-06: credentials are fetched live on every probe).
	Raw map[string]any
}

// Candidate is one account CPA offers to the scheduler for a request.
type Candidate struct {
	AuthID    string
	AuthIndex string
	Provider  string
	Model     string
	Priority  int
}

// SchedulerPickRequest is what CPA hands the plugin when it is about to choose
// an account for an outbound request.
type SchedulerPickRequest struct {
	RequestID  string
	Provider   string
	Model      string
	Headers    http.Header
	Candidates []Candidate
}

// SchedulerPickResponse is the plugin's answer. Exactly one of Delegate or
// AuthID should be set: Delegate means "fall back to CPA's built-in policy",
// AuthID means "use this account".
type SchedulerPickResponse struct {
	Delegate bool
	AuthID   string
	Reason   string
}

// RequestStage identifies an interceptor phase.
type RequestStage int

const (
	StageBeforeAuth RequestStage = iota
	StageAfterAuth
)

// InterceptedRequest is the mutable view of an outbound request the plugin
// gets at an interceptor phase.
//
// Headers are mutable: mutations made here are what gets sent upstream. Set
// RemoveHeaders to delete header names outright, which is how the correlation
// header is stripped before the request leaves CPA.
type InterceptedRequest struct {
	Stage     RequestStage
	RequestID string
	Provider  string
	Model     string
	// AuthID is only populated at StageAfterAuth. BeforeAuth runs before CPA
	// has picked an account, which is why the correlation header exists.
	AuthID  string
	Headers http.Header
}

// ResponseHeaders carries the response metadata available at the streaming
// header-init callback (StreamChunkHeaderInitIndex == -1).
type ResponseHeaders struct {
	RequestID string
	Model     string
	Status    int
	Header    http.Header
}

// Host is the CPA surface the plugin depends on.
type Host interface {
	// ListAccounts returns the current Codex auth pool.
	ListAccounts(ctx context.Context) ([]Account, error)
	// GetCredential fetches a live credential. Callers must not cache the
	// result (NF-06).
	GetCredential(ctx context.Context, authIndex string) (Credential, error)
	// Log emits a structured host log line.
	Log(level LogLevel, msg string, fields map[string]any)
}

// LogLevel mirrors the levels CPA accepts from plugins.
type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)
