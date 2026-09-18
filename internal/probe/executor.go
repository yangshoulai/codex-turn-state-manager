package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/models"
	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
)

// HistoryEntry is one row of probe_history -- one proxy attempt.
type HistoryEntry struct {
	ID          int64     `json:"id"`
	AuthIndex   string    `json:"authIndex"`
	Model       string    `json:"model"`
	ProxyID     string    `json:"proxyId,omitempty"`
	Result      Outcome   `json:"result"`
	StateLength int       `json:"stateLength"`
	LatencyMS   int       `json:"latencyMs"`
	ProbedAt    time.Time `json:"probedAt"`
}

// ProbeQuery filters and pages probe history.
//
// Zero-valued fields mean "no constraint", so the default query is the newest
// page of everything.
type ProbeQuery struct {
	AuthIndex string
	Model     string
	Limit     int
	Offset    int
}

// Normalise applies the defaults a caller may leave unset.
func (q ProbeQuery) Normalise() ProbeQuery {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 500 {
		q.Limit = 500
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	return q
}

// HistoryStore persists probe history. Implemented by storage.ProbeStore.
type HistoryStore interface {
	AppendProbe(ctx context.Context, e HistoryEntry) error
	ListProbes(ctx context.Context, q ProbeQuery) ([]HistoryEntry, error)
	// CountProbes returns how many rows match the query's filters, ignoring
	// paging, so the panel can show a page count.
	CountProbes(ctx context.Context, q ProbeQuery) (int, error)
	// PruneProbesBefore deletes rows probed before the cutoff and reports how
	// many went.
	PruneProbesBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// Result is the aggregate outcome of probing one pair across the pool.
//
// Outcome reflects the last attempt made; ProxyID is where that attempt went.
// ProxiesTried counts how many nodes the traversal actually contacted, which
// is what the panel shows as "遍历 N 节点".
type Result struct {
	Outcome      Outcome
	StateValue   string
	StateLength  int
	ProxyID      string
	ProxiesTried int
	Latency      time.Duration
	Err          error
	// Signals carries what the upstream said about the account -- plan and
	// rate-limit windows -- which arrives on the same response as the state
	// token. Reading it here is free; the panel would otherwise have no source
	// for the plan at all, since the credential's own claim is often absent.
	Signals headers.Signals
}

// Succeeded reports whether a target-length state was harvested.
func (r Result) Succeeded() bool { return r.Outcome == OutcomeSuccessTarget }

// ExecutorPolicy is the per-run configuration, resolved from settings at call
// time so a configuration change applies without rebuilding the executor.
type ExecutorPolicy struct {
	TargetStateLength int
	MaxProbeDuration  time.Duration
}

// Executor probes one (authIndex, model) pair, walking the proxy pool until it
// harvests a target-length state or exhausts every usable node.
type Executor struct {
	host    hostapi.Host
	pool    *proxies.Pool
	models  *models.Registry
	history HistoryStore
	policy  func() ExecutorPolicy

	// baseURL is overridable so tests can point at an httptest server.
	baseURL string

	now func() time.Time
}

// ExecutorConfig configures a new Executor.
type ExecutorConfig struct {
	Host    hostapi.Host
	Pool    *proxies.Pool
	Models  *models.Registry
	History HistoryStore
	Policy  func() ExecutorPolicy
	BaseURL string
}

// NewExecutor builds an executor.
func NewExecutor(cfg ExecutorConfig) *Executor {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultUpstreamBaseURL
	}
	policy := cfg.Policy
	if policy == nil {
		policy = func() ExecutorPolicy {
			return ExecutorPolicy{TargetStateLength: 292, MaxProbeDuration: 90 * time.Second}
		}
	}
	return &Executor{
		host:    cfg.Host,
		pool:    cfg.Pool,
		models:  cfg.Models,
		history: cfg.History,
		policy:  policy,
		baseURL: strings.TrimRight(baseURL, "/"),
		now:     time.Now,
	}
}

// SetClock overrides the time source. Tests only.
func (e *Executor) SetClock(now func() time.Time) { e.now = now }

// Probe runs the full traversal for one pair.
//
// Structure follows design doc 3.7.3: proxies are visited least-recently-used
// first, each is stamped as used *before* its request is sent, and the walk
// stops early on a target hit or on an error no other proxy could fix.
func (e *Executor) Probe(ctx context.Context, authIndex, model string) Result {
	started := e.now()
	policy := e.policy()

	available := e.pool.Available(started)
	if len(available) == 0 {
		r := Result{Outcome: OutcomeNoProxyAvailable}
		e.record(ctx, authIndex, model, r, started)
		r.Latency = e.now().Sub(started)
		return r
	}

	// One deadline for the whole traversal, so a pair cannot occupy a probe
	// concurrency slot indefinitely (NF-09).
	deadline := started.Add(policy.MaxProbeDuration)
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	result := Result{Outcome: OutcomeNoProxyAvailable}

	for _, node := range available {
		if !e.now().Before(deadline) {
			result.Outcome = OutcomeTimeoutAllProxies
			result.ProxyID = ""
			break
		}
		if runCtx.Err() != nil {
			result.Outcome = OutcomeTimeoutAllProxies
			result.ProxyID = ""
			break
		}

		// Stamp before sending: a concurrent worker must observe this node as
		// recently used and prefer a different one.
		if err := e.pool.MarkUsed(runCtx, node.ID, e.now()); err != nil {
			e.log(hostapi.LogWarn, "could not stamp proxy last_used_at", map[string]any{
				"proxyId": node.ID, "error": err.Error(),
			})
		}

		attempt := e.attempt(runCtx, node, authIndex, model, policy)
		attempt.ProxyID = node.ID

		result.Outcome = attempt.Outcome
		result.StateValue = attempt.StateValue
		result.StateLength = attempt.StateLength
		result.ProxyID = node.ID
		result.Err = attempt.Err
		result.ProxiesTried++

		e.record(runCtx, authIndex, model, attempt, started)

		switch {
		case attempt.Outcome == OutcomeSuccessTarget:
			// Target hit: bind and stop (design doc 3.7.3, rule 1).
			_ = e.pool.MarkSuccess(runCtx, node.ID, attempt.Latency, e.now())
			result.Latency = e.now().Sub(started)
			return result

		case attempt.Outcome.Terminal():
			// Account- or model-level: another proxy cannot help.
			result.Latency = e.now().Sub(started)
			return result

		case attempt.Outcome.ProxyFault():
			cooldown := ProxyCooldown(e.consecutiveFailures(node.ID))
			if _, err := e.pool.MarkFailure(runCtx, node.ID, cooldown, e.now()); err != nil {
				e.log(hostapi.LogWarn, "could not cool down proxy", map[string]any{
					"proxyId": node.ID, "error": err.Error(),
				})
			}
		}
	}

	result.Latency = e.now().Sub(started)
	return result
}

// record appends one probe_history row. History is best-effort: losing a row
// must never abort a probe.
func (e *Executor) record(ctx context.Context, authIndex, model string, r Result, started time.Time) {
	if e.history == nil {
		return
	}
	latency := r.Latency
	if latency == 0 {
		latency = e.now().Sub(started)
	}
	entry := HistoryEntry{
		AuthIndex:   authIndex,
		Model:       model,
		ProxyID:     r.ProxyID,
		Result:      r.Outcome,
		StateLength: r.StateLength,
		LatencyMS:   int(latency.Milliseconds()),
		ProbedAt:    e.now(),
	}
	if err := e.history.AppendProbe(ctx, entry); err != nil {
		e.log(hostapi.LogWarn, "could not append probe history", map[string]any{
			"authIndex": authIndex, "model": model, "error": err.Error(),
		})
	}
}

// attempt sends one probe through one proxy.
func (e *Executor) attempt(ctx context.Context, node proxies.Node, authIndex, model string, policy ExecutorPolicy) Result {
	started := e.now()

	// Fetched live, never cached (NF-06).
	cred, err := e.host.GetCredential(ctx, authIndex)
	if err != nil {
		return Result{Outcome: OutcomeAuthError, Err: err, Latency: e.now().Sub(started)}
	}
	if cred.AccessToken == "" {
		return Result{
			Outcome: OutcomeAuthError,
			Err:     errors.New("probe: credential has no access_token"),
			Latency: e.now().Sub(started),
		}
	}

	effort := e.models.MinReasoning(model)
	req, err := newProbeHTTPRequest(ctx, e.baseURL, cred.AccessToken, model, effort)
	if err != nil {
		return Result{Outcome: OutcomeNetworkError, Err: err, Latency: e.now().Sub(started)}
	}

	client, err := newProxyClient(node.URL, policy.MaxProbeDuration)
	if err != nil {
		// A malformed proxy URL is a configuration fault; a cooldown will not
		// fix it, but the node is useless until corrected, so it is treated as
		// a node-level failure.
		return Result{Outcome: OutcomeNetworkError, Err: err, Latency: e.now().Sub(started)}
	}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return Result{Outcome: OutcomeNetworkError, Err: err, Latency: e.now().Sub(started)}
	}
	// Headers are all we need; the SSE body is abandoned immediately rather
	// than awaited (design doc 3.6).
	defer closeBody(resp)

	latency := e.now().Sub(started)

	switch resp.StatusCode {
	case http.StatusOK:
		value := headers.Get(resp.Header, headers.TurnState)
		out := Result{
			StateValue:  value,
			StateLength: len(value),
			Latency:     latency,
			Signals:     headers.ParseSignals(resp.Header),
		}
		if value != "" && len(value) == policy.TargetStateLength {
			out.Outcome = OutcomeSuccessTarget
		} else {
			// Recorded, never bound (F-15).
			out.Outcome = OutcomeSuccessNonTarget
		}
		return out

	case http.StatusUnauthorized, http.StatusForbidden:
		return Result{
			Outcome: OutcomeAuthError,
			Latency: latency,
			Err:     fmt.Errorf("probe: upstream returned %d", resp.StatusCode),
		}

	case http.StatusTooManyRequests:
		return Result{
			Outcome: OutcomeRateLimit,
			Latency: latency,
			Err:     fmt.Errorf("probe: upstream returned %d", resp.StatusCode),
		}

	case http.StatusBadRequest, http.StatusNotFound:
		if isModelUnsupported(resp) {
			return Result{
				Outcome: OutcomeModelUnsupported,
				Latency: latency,
				Err:     fmt.Errorf("probe: model %s is not usable by this account", model),
			}
		}
		return Result{
			Outcome: OutcomeUpstreamError,
			Latency: latency,
			Err:     fmt.Errorf("probe: upstream returned %d", resp.StatusCode),
		}

	default:
		// 5xx and anything unmapped. Not the proxy's fault, so no cooldown,
		// but the traversal continues to the next node.
		return Result{
			Outcome: OutcomeUpstreamError,
			Latency: latency,
			Err:     fmt.Errorf("probe: upstream returned %d", resp.StatusCode),
		}
	}
}

// isModelUnsupported sniffs the error body for a model-capability failure.
//
// Upstream has no distinct status code for this, so the body is the only
// signal. The read is bounded and any parse failure degrades to a generic
// upstream error.
func isModelUnsupported(resp *http.Response) bool {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return false
	}
	text := strings.ToLower(string(raw))
	if !strings.Contains(text, "model") {
		return false
	}
	for _, marker := range []string{
		"not supported", "unsupported", "does not exist", "unknown model",
		"not available", "model_not_found",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (e *Executor) consecutiveFailures(id string) int {
	if n, ok := e.pool.Get(id); ok {
		return n.ConsecutiveFailures
	}
	return 1
}

func (e *Executor) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if e.host != nil {
		e.host.Log(level, msg, fields)
	}
}
