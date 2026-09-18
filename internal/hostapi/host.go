// Package hostapi is the port through which the plugin talks to CPA.
//
// Nothing outside this package and internal/pluginabi may reference a CPA SDK
// type. The plugin is loaded as a C-ABI shared library and exchanges JSON-RPC
// envelopes with the host, so all host interaction is expressed as plain Go
// values here and translated at the ABI edge.
//
// The shapes below were reconciled against CLIProxyAPI v7.3.7
// (sdk/pluginapi/types.go, sdk/pluginabi/types.go). Where CPA has no field for
// something the plugin needs, that is called out explicitly rather than papered
// over -- several earlier assumptions did not survive that reconciliation.
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

// Metadata keys the host populates on the after-auth request payload and on the
// stream-chunk header-init payload.
//
// These are the plugin's only route to the selected account: neither
// RequestInterceptRequest nor StreamChunkInterceptRequest has an auth field.
// CPA documents Metadata as a "best-effort cloned context snapshot", so these
// keys are an internal convention rather than a contract -- treat a missing key
// as "account unknown" and degrade, never as a reason to fail the request.
const (
	MetadataSelectedAuthID    = "selected_auth_id"
	MetadataSelectedAuthIndex = "selected_auth_index"
)

// Account is one entry of CPA's auth pool.
//
// AuthIndex is the plugin's stable persistence key; AuthID is CPA's runtime
// handle used by the scheduler. AccountRegistry owns the mapping between them.
type Account struct {
	AuthIndex string
	AuthID    string
	Provider  string
	Label     string
	Status    string
	Priority  int
	Disabled  bool
}

// Credential is the credential material CPA holds for an account.
//
// CPA's host.auth.get returns the raw on-disk auth file as JSON, not a parsed
// credential object. AccessToken is therefore extracted from that document by
// the adapter using the provider's conventional top-level "access_token" key;
// an unexpected layout must surface as a missing token, never as a cached one.
type Credential struct {
	AccessToken string
	ExpiresAt   time.Time
	// Raw is the decoded credential document. Never log it and never persist
	// it (NF-06: credentials are fetched live on every probe).
	Raw map[string]any
}

// Candidate is one account CPA offers to the scheduler.
//
// ID is CPA's runtime auth identifier. CPA does not put the plugin's AuthIndex
// on candidates, so resolving ID to AuthIndex goes through AccountRegistry.
type Candidate struct {
	ID         string
	Provider   string
	Priority   int
	Status     string
	Attributes map[string]string
}

// SchedulerPickRequest is what CPA hands the plugin when it is about to choose
// an account for an outbound request.
type SchedulerPickRequest struct {
	RequestID string
	Provider  string
	// Providers lists every provider key accepted by the route.
	Providers []string
	Model     string
	Stream    bool
	Headers   map[string][]string
	Metadata  map[string]any
	// Candidates contains the accounts available for selection. CPA restricts
	// this to the highest available priority tier unless the plugin declares
	// SchedulerAcrossPriorities at registration.
	Candidates []Candidate
}

// SchedulerPickResponse is the plugin's answer.
//
// Handled must be true for the host to honour AuthID; when it is false the host
// falls back to the built-in scheduler, which is the "do not interfere" path.
type SchedulerPickResponse struct {
	// AuthID names the chosen account and must match a candidate ID exactly.
	AuthID string
	// Handled reports whether the plugin made a decision.
	Handled bool
	// DelegateBuiltin asks the host to use a named built-in scheduler
	// ("round-robin" or "fill-first") instead of a specific account.
	DelegateBuiltin string
	// Reason is informational and is not sent to the host.
	Reason string
}

// Delegated reports whether the plugin declined to make a decision.
func (r SchedulerPickResponse) Delegated() bool {
	return !r.Handled || r.AuthID == ""
}

// RequestStage identifies an interceptor phase.
type RequestStage int

const (
	StageBeforeAuth RequestStage = iota
	StageAfterAuth
)

// InterceptedRequest is the view of an outbound request the plugin gets at an
// interceptor phase.
//
// AuthID and AuthIndex are populated only at StageAfterAuth, and only from the
// host's Metadata. Both are empty when that metadata is absent.
type InterceptedRequest struct {
	Stage     RequestStage
	RequestID string
	TraceID   string
	Provider  string
	Model     string
	Stream    bool
	AuthID    string
	AuthIndex string

	// Headers are mutable: what the plugin returns replaces matching headers.
	Headers http.Header
	// ClearHeaders names headers to delete outright, before Headers is applied.
	// This is how a header is removed -- there is no nil-value convention.
	ClearHeaders []string
}

// Completion is the terminal state of an intercepted request.
//
// This is a better self-healing signal than sniffing response bodies: the host
// reports the outcome and status code directly.
type Completion struct {
	RequestID  string
	Model      string
	Stream     bool
	Outcome    CompletionOutcome
	StatusCode int
}

// CompletionOutcome mirrors the host's terminal states.
type CompletionOutcome string

const (
	CompletionSucceeded CompletionOutcome = "succeeded"
	CompletionFailed    CompletionOutcome = "failed"
	CompletionRejected  CompletionOutcome = "rejected"
	CompletionCanceled  CompletionOutcome = "canceled"
)

// StreamChunkHeaderInitIndex marks the header-only stream initialisation call,
// as opposed to a payload chunk whose ChunkIndex starts at 0.
const StreamChunkHeaderInitIndex = -1

// StreamChunk is the response-side observation payload.
type StreamChunk struct {
	RequestID string
	Model     string
	AuthID    string
	AuthIndex string
	// ChunkIndex is StreamChunkHeaderInitIndex on the header-only call.
	ChunkIndex int
	// ResponseHeaders carries the raw upstream response headers, before the
	// host filters them for downstream delivery.
	ResponseHeaders http.Header
	// StatusCode is available on the non-streaming response interceptor.
	StatusCode int
	// Body is the response body on the non-streaming path; empty on the
	// header-init stream call.
	Body []byte
}

// IsHeaderInit reports whether this is the header-only initialisation call.
func (c StreamChunk) IsHeaderInit() bool {
	return c.ChunkIndex == StreamChunkHeaderInitIndex
}

// Host is the CPA surface the plugin depends on.
type Host interface {
	// ListAccounts returns the current auth pool.
	ListAccounts(ctx context.Context) ([]Account, error)
	// GetCredential fetches live credential material. Callers must not cache
	// the result (NF-06).
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
