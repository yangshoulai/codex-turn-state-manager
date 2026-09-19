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
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
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

	// StateBlocks is the ciphertext block count read from the value, and
	// StateIssued when the upstream minted it. Zero when the value had no
	// readable envelope, which is also when the length comparison decided the
	// outcome.
	StateBlocks int
	StateIssued time.Time
}

// PlanSource reports an account's subscription tier, which decides how many
// ciphertext blocks its state values carry and therefore how long they are.
type PlanSource interface {
	PlanType(authIndex string) string
}

// Succeeded reports whether a state of the account's shape was harvested.
func (r Result) Succeeded() bool { return r.Outcome == OutcomeSuccessTarget }

// expectation resolves the state shape this account's values must have.
func (e *Executor) expectation(authIndex, planFromResponse string, policy ExecutorPolicy) states.Expectation {
	plan := strings.TrimSpace(planFromResponse)
	if plan == "" && e.plans != nil {
		plan = e.plans.PlanType(authIndex)
	}
	return states.ExpectationFor(plan, policy.TargetStateLength)
}

// ExecutorPolicy is the per-run configuration, resolved from settings at call
// time so a configuration change applies without rebuilding the executor.
type ExecutorPolicy struct {
	TargetStateLength int
	// TTL is how long a state value is good for, counted from when the upstream
	// minted it rather than from when we stored it.
	TTL              time.Duration
	MaxProbeDuration time.Duration
	// MaxProxies caps one traversal. Zero means no cap, which is what the
	// tests that predate the setting rely on.
	MaxProxies int
}

// Executor probes one (authIndex, model) pair, walking the proxy pool until it
// harvests a target-length state or exhausts every usable node.
type Executor struct {
	host    hostapi.Host
	pool    *proxies.Pool
	models  *models.Registry
	history HistoryStore
	plans   PlanSource
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
	// Plans supplies an account's subscription tier, which decides the shape its
	// state values must have. Optional: without it every account is judged by
	// the configured target length, which is the previous behaviour.
	Plans   PlanSource
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
			return ExecutorPolicy{
				TargetStateLength: 292,
				TTL:               time.Hour,
				MaxProbeDuration:  90 * time.Second,
			}
		}
	}
	return &Executor{
		host:    cfg.Host,
		pool:    cfg.Pool,
		models:  cfg.Models,
		history: cfg.History,
		plans:   cfg.Plans,
		policy:  policy,
		baseURL: strings.TrimRight(baseURL, "/"),
		now:     time.Now,
	}
}

// SetClock overrides the time source. Tests only.
func (e *Executor) SetClock(now func() time.Time) { e.now = now }

// ProbeOnce runs a single-attempt probe: one node, then stop whatever the
// outcome.
//
// This is the on-demand probe behind the panel's button. Its purpose is to
// answer "what does this pair do right now", so it deliberately does not walk
// the pool -- walking it would take minutes and report on the pool rather than
// on the pair.
func (e *Executor) ProbeOnce(ctx context.Context, authIndex, model string) Result {
	return e.probe(ctx, authIndex, model, 1)
}

// Probe runs the full traversal for one pair.
//
// Structure follows design doc 3.7.3: proxies are visited least-recently-used
// first, each is stamped as used *before* its request is sent, and the walk
// stops early on a target hit or on an error no other proxy could fix.
func (e *Executor) Probe(ctx context.Context, authIndex, model string) Result {
	return e.probe(ctx, authIndex, model, e.policy().MaxProxies)
}

func (e *Executor) probe(ctx context.Context, authIndex, model string, maxProxies int) Result {
	started := e.now()
	policy := e.policy()

	// Scoped to the account: a node benched for this account is still available
	// to every other, which is the point of keeping the ledger per pair.
	available := e.pool.AvailableFor(authIndex, started)
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
		if maxProxies > 0 && result.ProxiesTried >= maxProxies {
			// The round is over. Report the last attempt's outcome rather than
			// a special one: what the operator needs to know is how the last
			// node behaved, and that the pool was not exhausted is visible in
			// ProxiesTried.
			break
		}
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

		// Carried field by field rather than assigned wholesale, so that a
		// per-attempt field added without being listed here is silently lost
		// on the way out -- which is exactly what happened to the parsed shape
		// the first time it was added.
		result.Outcome = attempt.Outcome
		result.StateValue = attempt.StateValue
		result.StateLength = attempt.StateLength
		result.ProxyID = node.ID
		result.Err = attempt.Err
		result.StateBlocks = attempt.StateBlocks
		result.StateIssued = attempt.StateIssued
		result.ProxiesTried++

		e.record(runCtx, authIndex, model, attempt, started)

		switch {
		case attempt.Outcome == OutcomeSuccessTarget:
			// Target hit: bind and stop (design doc 3.7.3, rule 1).
			_ = e.pool.MarkSuccess(runCtx, authIndex, node.ID, attempt.Latency, e.now())
			result.Latency = e.now().Sub(started)
			return result

		case attempt.Outcome.Terminal():
			// Account- or model-level: another proxy cannot help.
			result.Latency = e.now().Sub(started)
			return result

		case attempt.Outcome.ProxyFault(), attempt.Outcome == OutcomeSuccessNonTarget:
			// Two different reasons to prefer another node next time. A proxy
			// fault is the node's problem. A non-target length is not a fault at
			// all -- the request succeeded -- but this node is not yielding what
			// the pair needs, so it steps aside for the rest of the ladder. The
			// upstream errors that must not evict a node (400/401/403/429) are
			// Terminal, and returned above.
			e.coolDown(runCtx, authIndex, node.ID)
		}
	}

	result.Latency = e.now().Sub(started)
	return result
}

// coolDown applies the next step of a node's failure ladder.
//
// The count is taken before the increment so the first failure gets the base
// delay, and it is cleared when the ladder reaches its cap so the next failure
// starts over rather than holding the node out indefinitely.
func (e *Executor) coolDown(ctx context.Context, authIndex, id string) {
	// The rung of the failure being recorded, not the one already on file.
	rung := e.recordedFailures(authIndex, id) + 1
	cooldown := ProxyCooldown(rung)
	if _, err := e.pool.MarkFailure(ctx, authIndex, id, cooldown, e.now(), ProxyCooldownReachedCap(rung)); err != nil {
		e.log(hostapi.LogWarn, "could not cool down proxy", map[string]any{
			"proxyId": id, "error": err.Error(),
		})
	}
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
		signals := headers.ParseSignals(resp.Header)
		out := Result{
			StateValue:  value,
			StateLength: len(value),
			Latency:     latency,
			Signals:     signals,
		}
		// The plan on this very response is the freshest evidence of what shape
		// this account's values should have, and it arrives on the same
		// response as the value being judged. Falling back to the stored plan
		// and then to the configured length is what keeps a first probe working
		// before anything has been learned.
		env, ok := states.AcceptState(value, e.expectation(authIndex, signals.PlanType, policy))
		if ok && !env.Issued.IsZero() && !e.now().Before(env.Issued.Add(policy.TTL)) {
			// The right shape, but minted longer ago than a value is good for.
			// Binding it would replace a working binding with a dead one, and
			// blaming the node would bench a proxy that behaved correctly.
			ok = false
			out.Outcome = OutcomeSuccessStale
			out.StateIssued = env.Issued
			out.StateBlocks = env.Blocks
			return out
		}
		if ok {
			out.Outcome = OutcomeSuccessTarget
			out.StateIssued = env.Issued
			out.StateBlocks = env.Blocks
		} else {
			// Recorded, never bound (F-15).
			out.Outcome = OutcomeSuccessNonTarget
			out.StateBlocks = env.Blocks
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

// recordedFailures is how many consecutive failures this account already has on
// file against this node; zero when there are none.
//
// It is deliberately the count *before* the failure being handled. The ladder
// position is that number plus one, and conflating the two is what put every
// rung one step behind: the second failure was charged the first failure's
// delay, and the cap arrived one failure late.
func (e *Executor) recordedFailures(authIndex, id string) int {
	for _, row := range e.pool.CooldownsForProxy(id) {
		if row.AuthIndex == authIndex {
			return row.ConsecutiveFailures
		}
	}
	return 0
}

func (e *Executor) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if e.host != nil {
		e.host.Log(level, msg, fields)
	}
}
