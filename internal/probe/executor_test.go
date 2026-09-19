package probe

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/models"
	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

const probeTargetLen = 32

func targetState() string { return strings.Repeat("t", probeTargetLen) }

// ---------------------------------------------------------------------------
// fake upstream reachable through a pool node
//
// The executor egresses through a pool node as an HTTP proxy, so a plain test
// server used *as* the node URL sees the exact request the plugin would send
// upstream. That makes the request shape from design doc 3.6 directly
// assertable instead of merely inspected by eye.

type capturedRequest struct {
	method string
	url    string
	header http.Header
	body   []byte
}

type fakeProxy struct {
	*httptest.Server

	mu       sync.Mutex
	received []capturedRequest
}

// RespondFunc decides what the fake upstream returns for one request. attempt
// is the zero-based index of the request that reached this node.
type RespondFunc func(attempt int, w http.ResponseWriter, r *http.Request)

func newFakeProxy(t *testing.T, respond RespondFunc) *fakeProxy {
	t.Helper()
	fp := &fakeProxy{}
	fp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			body = nil
		}
		fp.mu.Lock()
		attempt := len(fp.received)
		fp.received = append(fp.received, capturedRequest{
			method: r.Method,
			url:    r.URL.String(),
			header: r.Header.Clone(),
			body:   body,
		})
		fp.mu.Unlock()
		respond(attempt, w, r)
	}))
	t.Cleanup(fp.Close)
	return fp
}

func (f *fakeProxy) requests() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedRequest, len(f.received))
	copy(out, f.received)
	return out
}

func (f *fakeProxy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

// closedPortProxy returns a node URL nothing is listening on, so the transport
// fails at connect time.
func closedPortProxy(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// respondWith is the common shape: a status, an optional state header, and an
// optional body.
func respondWith(status int, state string, body string) RespondFunc {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		if state != "" {
			w.Header().Set(headers.TurnState, state)
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
	}
}

// ---------------------------------------------------------------------------
// harness

type memProxyStore struct {
	mu        sync.Mutex
	nodes     map[string]proxies.Node
	cooldowns map[string]proxies.Cooldown
}

func newMemProxyStore() *memProxyStore {
	return &memProxyStore{nodes: map[string]proxies.Node{}, cooldowns: map[string]proxies.Cooldown{}}
}

func (s *memProxyStore) ListProxies(context.Context) ([]proxies.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proxies.Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n)
	}
	return out, nil
}

func (s *memProxyStore) UpsertProxy(_ context.Context, n proxies.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n.ID] = n
	return nil
}

func (s *memProxyStore) DeleteProxy(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
	return nil
}

// The cooldown ledger, in memory. It is a separate map keyed by the pair so a
// test can assert that one account's failures leave another account's view of
// the same node alone -- which is the behaviour the ledger exists for.
func (s *memProxyStore) ListCooldowns(context.Context) ([]proxies.Cooldown, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proxies.Cooldown, 0, len(s.cooldowns))
	for _, c := range s.cooldowns {
		out = append(out, c)
	}
	return out, nil
}

func (s *memProxyStore) UpsertCooldown(_ context.Context, c proxies.Cooldown) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cooldowns[memCooldownKey(c.AuthIndex, c.ProxyID)] = c
	return nil
}

func (s *memProxyStore) DeleteCooldown(_ context.Context, authIndex, proxyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cooldowns, memCooldownKey(authIndex, proxyID))
	return nil
}

func (s *memProxyStore) DeleteAllCooldowns(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cooldowns = map[string]proxies.Cooldown{}
	return nil
}

type recordingHistory struct {
	mu      sync.Mutex
	entries []HistoryEntry
}

func (h *recordingHistory) AppendProbe(_ context.Context, e HistoryEntry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, e)
	return nil
}

func (h *recordingHistory) ListProbes(context.Context, ProbeQuery) ([]HistoryEntry, error) {
	return nil, nil
}

func (h *recordingHistory) CountProbes(context.Context, ProbeQuery) (int, error) { return 0, nil }

func (h *recordingHistory) PruneProbesBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func (h *recordingHistory) all() []HistoryEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]HistoryEntry, len(h.entries))
	copy(out, h.entries)
	return out
}

// fakePlans stands in for the account registry: the executor only needs to be
// told the tier, not how it was learned.
type fakePlans struct {
	mu   sync.Mutex
	plan map[string]string
}

func (f *fakePlans) PlanType(authIndex string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plan[authIndex]
}

func (f *fakePlans) set(authIndex, plan string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plan[authIndex] = plan
}

type execHarness struct {
	executor *Executor
	pool     *proxies.Pool
	host     *hostapi.MockHost
	history  *recordingHistory
	models   *models.Registry
	plans    *fakePlans
	policy   ExecutorPolicy
}

func newExecHarness(t *testing.T) *execHarness {
	t.Helper()
	host := hostapi.NewMockHost(hostapi.Account{
		AuthIndex: "codex-auth-1",
		AuthID:    "auth-id-1",
		Provider:  hostapi.ProviderCodex,
		Label:     "user1",
		Status:    hostapi.AccountStatusActive,
	})
	pool := proxies.NewPool(newMemProxyStore())
	history := &recordingHistory{}
	h := &execHarness{
		pool:    pool,
		host:    host,
		history: history,
		plans:   &fakePlans{plan: map[string]string{}},
		policy: ExecutorPolicy{
			TargetStateLength: probeTargetLen,
			TTL:               time.Hour,
			MaxProbeDuration:  5 * time.Second,
		},
	}
	h.models = models.NewRegistry()
	h.executor = NewExecutor(ExecutorConfig{
		Host:    host,
		Pool:    pool,
		Models:  h.models,
		History: history,
		Plans:   h.plans,
		Policy:  func() ExecutorPolicy { return h.policy },
		// Plain HTTP so the node receives a normal proxied request rather than
		// a CONNECT tunnel, which is what makes the request assertable.
		BaseURL: "http://codex-upstream.invalid",
	})
	return h
}

// addProxy registers a pool node pointing at a server and returns the server.
func (h *execHarness) addProxy(t *testing.T, id string, respond RespondFunc) *fakeProxy {
	t.Helper()
	fp := newFakeProxy(t, respond)
	node := proxies.Node{ID: id, URL: fp.URL, Enabled: true}
	if err := h.pool.Upsert(context.Background(), node); err != nil {
		t.Fatalf("add proxy %s: %v", id, err)
	}
	return fp
}

// addDeadProxy registers a node whose address refuses connections.
func (h *execHarness) addDeadProxy(t *testing.T, id string) {
	t.Helper()
	node := proxies.Node{ID: id, URL: closedPortProxy(t), Enabled: true}
	if err := h.pool.Upsert(context.Background(), node); err != nil {
		t.Fatalf("add dead proxy %s: %v", id, err)
	}
}

func (h *execHarness) probe(t *testing.T) Result {
	t.Helper()
	return h.executor.Probe(context.Background(), "codex-auth-1", "gpt-5-codex")
}

// ---------------------------------------------------------------------------
// request shape (design doc 3.6, F-14)

func TestBuildProbeBody_MatchesDesignDoc(t *testing.T) {
	raw, err := buildProbeBody("gpt-5-codex", models.EffortLow)
	if err != nil {
		t.Fatalf("buildProbeBody: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("probe body is not valid JSON: %v", err)
	}

	if body["model"] != "gpt-5-codex" {
		t.Errorf("model = %v, want gpt-5-codex", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	if body["store"] != false {
		t.Errorf("store = %v, want false", body["store"])
	}

	// An empty tool list must be present, not omitted: a missing key makes some
	// upstream versions fall back to a default toolset.
	tools, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %#v, want a present empty array", body["tools"])
	}
	if len(tools) != 0 {
		t.Errorf("tools = %v, want empty", tools)
	}

	// One-character input keeps the probe as cheap as possible (F-14).
	input, ok := body["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %#v, want exactly one entry", body["input"])
	}
	msg, _ := input[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("input[0].role = %v, want user", msg["role"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %#v, want one entry", msg["content"])
	}
	part, _ := content[0].(map[string]any)
	if part["type"] != "input_text" {
		t.Errorf("content[0].type = %v, want input_text", part["type"])
	}
	if part["text"] != "." {
		t.Errorf("content[0].text = %v, want a single character", part["text"])
	}

	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Errorf("reasoning.effort = %v, want low", reasoning["effort"])
	}
}

// TestExecutor_UsesModelReasoningFloor pins that the effort comes from the
// capability table rather than a hardcoded value.
//
// Every model in the current manifest happens to accept "low", so asserting on
// real slugs would prove nothing. The table is instead given a model whose floor
// is deliberately different, which is the property that actually matters: an
// operator-added model with a different floor must not be probed at the wrong
// effort.
func TestExecutor_UsesModelReasoningFloor(t *testing.T) {
	h := newExecHarness(t)
	fp := h.addProxy(t, "p1", respondWith(http.StatusOK, targetState(), ""))

	h.models.Upsert(models.Capability{Model: "cheap-model", MinReasoning: models.EffortNone, Probeable: true})
	h.models.Upsert(models.Capability{Model: "dear-model", MinReasoning: models.EffortHigh, Probeable: true})

	cases := []struct {
		model string
		want  string
	}{
		{"cheap-model", "none"},
		{"dear-model", "high"},
		// A model the table has never heard of still has to produce a valid probe.
		{"gpt-5.6-luna", "low"},
	}
	for _, tc := range cases {
		before := fp.count()
		if got := h.executor.Probe(context.Background(), "codex-auth-1", tc.model); !got.Succeeded() {
			t.Fatalf("%s: outcome = %s, want success", tc.model, got.Outcome)
		}

		reqs := fp.requests()
		if len(reqs) != before+1 {
			t.Fatalf("%s: upstream saw %d requests, want %d", tc.model, len(reqs), before+1)
		}
		var body map[string]any
		if err := json.Unmarshal(reqs[before].body, &body); err != nil {
			t.Fatalf("%s: decode body: %v", tc.model, err)
		}
		if body["model"] != tc.model {
			t.Errorf("body model = %v, want %s", body["model"], tc.model)
		}
		reasoning, _ := body["reasoning"].(map[string]any)
		if reasoning["effort"] != tc.want {
			t.Errorf("%s: reasoning.effort = %v, want %s", tc.model, reasoning["effort"], tc.want)
		}
	}
}

func TestExecutor_SendsBearerCredentialAndHeaders(t *testing.T) {
	h := newExecHarness(t)
	h.host.SetCredential("codex-auth-1", hostapi.Credential{AccessToken: "token-abc"})
	fp := h.addProxy(t, "p1", respondWith(http.StatusOK, targetState(), ""))

	if got := h.probe(t); !got.Succeeded() {
		t.Fatalf("probe outcome = %s, want success", got.Outcome)
	}

	req := fp.requests()[0]
	if req.method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.method)
	}
	if !strings.HasSuffix(req.url, ProbePath) {
		t.Errorf("url = %s, want it to end with %s", req.url, ProbePath)
	}
	if got := req.header.Get("Authorization"); got != "Bearer token-abc" {
		t.Errorf("Authorization = %q, want Bearer token-abc", got)
	}
	if got := req.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := req.header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", got)
	}
	if got := req.header.Get(headers.Correlation); got != "" {
		t.Errorf("the internal correlation header leaked upstream: %q", got)
	}
}

func TestNewProxyClient_RejectsMalformedURL(t *testing.T) {
	for _, bad := range []string{"127.0.0.1:8080", "://nope", "just-a-string"} {
		if _, err := newProxyClient(bad, time.Second); err == nil {
			t.Errorf("newProxyClient(%q) accepted a malformed URL", bad)
		}
	}
}

func TestNewProxyClient_EmptyURLMeansDirect(t *testing.T) {
	client, err := newProxyClient("", time.Second)
	if err != nil {
		t.Fatalf("newProxyClient(\"\") = %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Error("an empty proxy URL should leave the transport direct")
	}
}

// ---------------------------------------------------------------------------
// traversal

func TestExecutor_NoProxiesAvailable(t *testing.T) {
	h := newExecHarness(t)

	got := h.probe(t)
	if got.Outcome != OutcomeNoProxyAvailable {
		t.Errorf("outcome = %s, want %s", got.Outcome, OutcomeNoProxyAvailable)
	}
	if got.ProxiesTried != 0 {
		t.Errorf("ProxiesTried = %d, want 0", got.ProxiesTried)
	}
}

// TestExecutor_TargetHitStopsTraversal is rule 1 of design doc 3.7.3.
func TestExecutor_TargetHitStopsTraversal(t *testing.T) {
	h := newExecHarness(t)
	first := h.addProxy(t, "p1", respondWith(http.StatusOK, targetState(), ""))
	second := h.addProxy(t, "p2", respondWith(http.StatusOK, targetState(), ""))

	got := h.probe(t)
	if !got.Succeeded() {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeSuccessTarget)
	}
	if got.ProxiesTried != 1 {
		t.Errorf("ProxiesTried = %d, want 1 (a target hit must end the walk)", got.ProxiesTried)
	}
	if got.StateLength != probeTargetLen {
		t.Errorf("StateLength = %d, want %d", got.StateLength, probeTargetLen)
	}

	// Exactly one of the two nodes was contacted.
	contacted := 0
	for _, fp := range []*fakeProxy{first, second} {
		contacted += fp.count()
	}
	if contacted != 1 {
		t.Errorf("%d nodes were contacted, want 1", contacted)
	}
}

// TestExecutor_NonTargetThenTarget covers the "keep walking" path for a
// harvest that is the wrong length.
func TestExecutor_NonTargetThenTarget(t *testing.T) {
	h := newExecHarness(t)
	h.addProxy(t, "p1", respondWith(http.StatusOK, strings.Repeat("x", probeTargetLen-1), ""))
	h.addProxy(t, "p2", respondWith(http.StatusOK, targetState(), ""))

	got := h.probe(t)
	if !got.Succeeded() {
		t.Fatalf("outcome = %s, want success from the second node", got.Outcome)
	}
	if got.ProxiesTried != 2 {
		t.Errorf("ProxiesTried = %d, want 2", got.ProxiesTried)
	}
	if got.ProxyID != "p2" {
		t.Errorf("ProxyID = %q, want p2", got.ProxyID)
	}
}

// TestExecutor_NonTargetOnlyNeverBinds is F-15: the walk ends with a non-target
// result and nothing may be treated as a hit.
func TestExecutor_NonTargetOnlyNeverBinds(t *testing.T) {
	h := newExecHarness(t)
	h.addProxy(t, "p1", respondWith(http.StatusOK, strings.Repeat("x", probeTargetLen+5), ""))
	h.addProxy(t, "p2", respondWith(http.StatusOK, "", ""))

	got := h.probe(t)
	if got.Outcome != OutcomeSuccessNonTarget {
		t.Errorf("outcome = %s, want %s", got.Outcome, OutcomeSuccessNonTarget)
	}
	if got.Succeeded() {
		t.Error("a non-target length must never count as a success")
	}
	if got.ProxiesTried != 2 {
		t.Errorf("ProxiesTried = %d, want the whole pool to be walked", got.ProxiesTried)
	}
}

// TestExecutor_WalksPoolInLRUOrder pins rule 4 of design doc 3.7.3: the
// least-recently-used node goes first, and its stamp is written before the
// request so concurrent workers see it.
func TestExecutor_WalksPoolInLRUOrder(t *testing.T) {
	ctx := context.Background()
	h := newExecHarness(t)

	recent := time.Now().Add(-time.Minute)
	old := time.Now().Add(-time.Hour)

	first := h.addProxy(t, "recently-used", respondWith(http.StatusOK, targetState(), ""))
	second := h.addProxy(t, "long-unused", respondWith(http.StatusOK, targetState(), ""))

	// Overwrite the nodes with lastUsedAt values so the ordering is decided by
	// the policy rather than by insertion.
	base := time.Now()
	_ = base
	if err := h.pool.Upsert(ctx, proxies.Node{
		ID: "recently-used", URL: first.URL, Enabled: true, LastUsedAt: &recent,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := h.pool.Upsert(ctx, proxies.Node{
		ID: "long-unused", URL: second.URL, Enabled: true, LastUsedAt: &old,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if got := h.probe(t); !got.Succeeded() {
		t.Fatalf("outcome = %s, want success", got.Outcome)
	}

	if second.count() != 1 {
		t.Fatalf("the long-unused node was contacted %d times, want 1", second.count())
	}
	if first.count() != 0 {
		t.Errorf("the recently-used node was contacted %d times, want 0", first.count())
	}
}

func TestExecutor_StampsLastUsedOnFailure(t *testing.T) {
	h := newExecHarness(t)
	h.addDeadProxy(t, "dead")

	if got := h.probe(t); got.Outcome != OutcomeNetworkError {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeNetworkError)
	}

	node, ok := h.pool.Get("dead")
	if !ok {
		t.Fatal("node missing")
	}
	// The stamp is written before the request precisely so a node that fails is
	// still marked as just-used.
	if node.LastUsedAt == nil {
		t.Error("lastUsedAt was not stamped for a node whose request failed")
	}
	if node.LastFailure == nil {
		t.Error("lastFailure was not recorded")
	}
}

// ---------------------------------------------------------------------------
// failure classification

// TestExecutor_AuthErrorAbortsTraversal is rule 2: another proxy cannot fix an
// account-level problem.
func TestExecutor_AuthErrorAbortsTraversal(t *testing.T) {
	h := newExecHarness(t)
	first := h.addProxy(t, "p1", respondWith(http.StatusUnauthorized, "", `{"error":"bad token"}`))
	second := h.addProxy(t, "p2", respondWith(http.StatusOK, targetState(), ""))

	got := h.probe(t)
	if got.Outcome != OutcomeAuthError {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeAuthError)
	}
	if got.ProxiesTried != 1 {
		t.Errorf("ProxiesTried = %d, want 1 (the walk must abort)", got.ProxiesTried)
	}
	if second.count() != 0 {
		t.Errorf("the second node was contacted %d times, want 0", second.count())
	}
	if first.count() != 1 {
		t.Errorf("the first node was contacted %d times, want 1", first.count())
	}
}

func TestExecutor_ModelUnsupportedAbortsTraversal(t *testing.T) {
	h := newExecHarness(t)
	h.addProxy(t, "p1", respondWith(http.StatusNotFound, "",
		`{"error":{"message":"The model does not exist"}}`))
	second := h.addProxy(t, "p2", respondWith(http.StatusOK, targetState(), ""))

	got := h.probe(t)
	if got.Outcome != OutcomeModelUnsupported {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeModelUnsupported)
	}
	if second.count() != 0 {
		t.Error("the walk should have aborted before the second node")
	}
}

func TestExecutor_RateLimitContinuesTraversal(t *testing.T) {
	h := newExecHarness(t)
	h.addProxy(t, "p1", respondWith(http.StatusTooManyRequests, "", `{"error":"slow down"}`))
	h.addProxy(t, "p2", respondWith(http.StatusOK, targetState(), ""))

	got := h.probe(t)
	if !got.Succeeded() {
		t.Fatalf("outcome = %s, want the walk to continue past a 429", got.Outcome)
	}
	if got.ProxiesTried != 2 {
		t.Errorf("ProxiesTried = %d, want 2", got.ProxiesTried)
	}
}

// TestExecutor_ServerErrorDoesNotCoolDownProxy is the rule from design doc
// 3.7.4: an upstream 5xx is not the node's fault.
func TestExecutor_ServerErrorDoesNotCoolDownProxy(t *testing.T) {
	h := newExecHarness(t)
	h.addProxy(t, "p1", respondWith(http.StatusInternalServerError, "", "boom"))
	h.addProxy(t, "p2", respondWith(http.StatusOK, targetState(), ""))

	got := h.probe(t)
	if !got.Succeeded() {
		t.Fatalf("outcome = %s, want the walk to continue past a 5xx", got.Outcome)
	}

	if rows := h.pool.CooldownsForProxy("p1"); len(rows) != 0 {
		t.Errorf("an upstream 5xx benched the node for account %s; it must not", rows[0].AuthIndex)
	}
	if node, _ := h.pool.Get("p1"); node.FailureCount != 0 {
		t.Errorf("FailureCount = %d, want 0 for an upstream fault", node.FailureCount)
	}
}

func TestExecutor_NetworkErrorCoolsDownProxy(t *testing.T) {
	h := newExecHarness(t)
	h.addDeadProxy(t, "dead")
	h.addProxy(t, "p2", respondWith(http.StatusOK, targetState(), ""))

	got := h.probe(t)
	if !got.Succeeded() {
		t.Fatalf("outcome = %s, want the walk to continue past a dead node", got.Outcome)
	}

	rows := h.pool.CooldownsForProxy("dead")
	if len(rows) != 1 {
		t.Fatalf("a connect failure left %d ledger rows for the node, want 1", len(rows))
	}
	row := rows[0]
	if row.CooldownUntil == nil {
		t.Fatal("a connect failure must cool the node down")
	}
	if row.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", row.ConsecutiveFailures)
	}
	if want := time.Now().Add(ProxyCooldown(1)); row.CooldownUntil.After(want.Add(time.Second)) {
		t.Errorf("cooldown until %v, want about %v", row.CooldownUntil, want)
	}
}

func TestExecutor_TimeoutAllProxies(t *testing.T) {
	h := newExecHarness(t)
	// One deadline covers the whole traversal, and a slow node eats it.
	h.policy.MaxProbeDuration = 60 * time.Millisecond

	// Node ids decide the order between equally-unused nodes, so the slow one
	// is named to sort first.
	h.addProxy(t, "a-slow", func(_ int, w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set(headers.TurnState, targetState())
		w.WriteHeader(http.StatusOK)
	})
	second := h.addProxy(t, "b-fast", respondWith(http.StatusOK, targetState(), ""))

	got := h.probe(t)
	if got.Outcome != OutcomeTimeoutAllProxies {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeTimeoutAllProxies)
	}
	if got.ProxiesTried != 1 {
		t.Errorf("ProxiesTried = %d, want 1 (the deadline should stop the walk)", got.ProxiesTried)
	}
	if second.count() != 0 {
		t.Error("the walk should not have reached the second node")
	}
}

// ---------------------------------------------------------------------------
// credential handling

// TestExecutor_FetchesCredentialPerAttempt is NF-06: the token is read live
// before every attempt and never cached.
func TestExecutor_FetchesCredentialPerAttempt(t *testing.T) {
	h := newExecHarness(t)
	h.host.SetCredential("codex-auth-1", hostapi.Credential{AccessToken: "token-1"})

	first := h.addProxy(t, "p1", func(_ int, w http.ResponseWriter, _ *http.Request) {
		// Rotate the credential between the two attempts.
		h.host.SetCredential("codex-auth-1", hostapi.Credential{AccessToken: "token-2"})
		w.WriteHeader(http.StatusOK)
	})
	second := h.addProxy(t, "p2", respondWith(http.StatusOK, targetState(), ""))

	if got := h.probe(t); !got.Succeeded() {
		t.Fatalf("outcome = %s, want success", got.Outcome)
	}

	reqs := first.requests()
	if len(reqs) != 1 {
		t.Fatalf("first node saw %d requests, want 1", len(reqs))
	}
	if got := reqs[0].header.Get("Authorization"); got != "Bearer token-1" {
		t.Errorf("first attempt Authorization = %q, want the token live at that moment", got)
	}

	// The second attempt must pick up the rotated token: had the executor
	// cached the first one, this would still say token-1.
	secondReqs := second.requests()
	if len(secondReqs) != 1 {
		t.Fatalf("second node saw %d requests, want 1", len(secondReqs))
	}
	if got := secondReqs[0].header.Get("Authorization"); got != "Bearer token-2" {
		t.Errorf("second attempt Authorization = %q, want the rotated token-2", got)
	}
}

func TestExecutor_MissingAccessTokenIsAuthError(t *testing.T) {
	h := newExecHarness(t)
	fp := h.addProxy(t, "p1", respondWith(http.StatusOK, targetState(), ""))
	h.host.SetCredential("codex-auth-1", hostapi.Credential{AccessToken: ""})

	got := h.probe(t)
	if got.Outcome != OutcomeAuthError {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeAuthError)
	}
	if got.Err == nil {
		t.Error("expected an error explaining the missing token")
	}
	// Refuse before spending a request.
	if fp.count() != 0 {
		t.Errorf("a request was sent with no token (%d)", fp.count())
	}
}

func TestExecutor_CredentialFetchErrorIsAuthError(t *testing.T) {
	h := newExecHarness(t)
	h.host.SetCredentialError("codex-auth-1", errors.New("host said no"))
	fp := h.addProxy(t, "p1", respondWith(http.StatusOK, targetState(), ""))

	got := h.probe(t)
	if got.Outcome != OutcomeAuthError {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeAuthError)
	}
	if fp.count() != 0 {
		t.Error("no request should be sent when the credential cannot be read")
	}
}

// ---------------------------------------------------------------------------
// bookkeeping

func TestExecutor_RecordsOneHistoryRowPerAttempt(t *testing.T) {
	h := newExecHarness(t)
	h.addProxy(t, "p1", respondWith(http.StatusOK, strings.Repeat("x", probeTargetLen-1), ""))
	h.addProxy(t, "p2", respondWith(http.StatusNotFound, "", `{"error":"model not supported"}`))

	got := h.probe(t)
	if got.Outcome != OutcomeModelUnsupported {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeModelUnsupported)
	}

	entries := h.history.all()
	if len(entries) != 2 {
		t.Fatalf("history rows = %d, want one per attempt (2)", len(entries))
	}
	if entries[0].Result != OutcomeSuccessNonTarget || entries[0].ProxyID != "p1" {
		t.Errorf("row 0 = %s/%s, want SUCCESS_NON_TARGET/p1", entries[0].Result, entries[0].ProxyID)
	}
	if entries[1].Result != OutcomeModelUnsupported || entries[1].ProxyID != "p2" {
		t.Errorf("row 1 = %s/%s, want MODEL_UNSUPPORTED/p2", entries[1].Result, entries[1].ProxyID)
	}
	for i, e := range entries {
		if e.AuthIndex != "codex-auth-1" || e.Model != "gpt-5-codex" {
			t.Errorf("row %d identifies %s/%s, want codex-auth-1/gpt-5-codex", i, e.AuthIndex, e.Model)
		}
		if e.ProbedAt.IsZero() {
			t.Errorf("row %d has no timestamp", i)
		}
	}
}

func TestExecutor_RecordsNonTargetLength(t *testing.T) {
	h := newExecHarness(t)
	h.addProxy(t, "p1", respondWith(http.StatusOK, strings.Repeat("x", 7), ""))

	// A state is present but the wrong length: recorded for diagnosis, never
	// bound (F-15).
	if got := h.probe(t); got.Outcome != OutcomeSuccessNonTarget {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeSuccessNonTarget)
	}
	entries := h.history.all()
	if len(entries) != 1 {
		t.Fatalf("history rows = %d, want 1", len(entries))
	}
	if entries[0].StateLength != 7 {
		t.Errorf("recorded length = %d, want 7", entries[0].StateLength)
	}
}

func TestIsModelUnsupported(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"explicit unsupported", `{"error":{"message":"model not supported"}}`, true},
		{"does not exist", `{"error":{"message":"The model gpt-x does not exist"}}`, true},
		{"unknown model", `{"error":"unknown model"}`, true},
		{"snake case code", `{"error":{"code":"model_not_found"}}`, true},
		{"a model mentioned for another reason", `{"error":{"message":"invalid input for this model"}}`, false},
		{"no model mentioned at all", `{"error":{"message":"bad request"}}`, false},
		{"a non-json body mentioning a model", `the model field is malformed`, false},
		{"empty body", ``, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
			if got := isModelUnsupported(resp); got != tc.want {
				t.Errorf("isModelUnsupported(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestExecutor_AbandonsTheStreamWithoutReadingIt(t *testing.T) {
	h := newExecHarness(t)

	// An endless SSE body: the executor must read the headers, keep the state,
	// and close the body rather than waiting for the stream to finish
	// (design doc 3.6).
	streamed := make(chan struct{})
	h.addProxy(t, "p1", func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headers.TurnState, targetState())
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-streamed:
		case <-time.After(2 * time.Second):
		}
	})

	start := time.Now()
	got := h.probe(t)
	elapsed := time.Since(start)
	close(streamed)

	if !got.Succeeded() {
		t.Fatalf("outcome = %s, want success", got.Outcome)
	}
	if elapsed > time.Second {
		t.Errorf("probe took %s; it waited for the stream instead of closing the body", elapsed)
	}
}

// TestExecutor_MaxProxiesCapsTheRound covers the setting an operator asked for:
// one round tries at most N nodes, then stops.
//
// Without the cap a deep pool is walked in full, which on a bad day means every
// probe spends the whole traversal budget before reporting anything. The cap
// bounds the cost of a round; it does not change which nodes are eligible,
// because the order is still least-recently-used.
func TestExecutor_MaxProxiesCapsTheRound(t *testing.T) {
	h := newExecHarness(t)

	// Four nodes that all answer with a non-target length, so the walk has no
	// reason to stop early on its own.
	var nodes []*fakeProxy
	for _, id := range []string{"p1", "p2", "p3", "p4"} {
		nodes = append(nodes, h.addProxy(t, id, respondWith(http.StatusOK, "short", "")))
	}

	h.policy.MaxProxies = 2

	got := h.probe(t)
	if got.ProxiesTried != 2 {
		t.Errorf("ProxiesTried = %d, want 2 with the cap set", got.ProxiesTried)
	}
	contacted := 0
	for _, n := range nodes {
		contacted += n.count()
	}
	if contacted != 2 {
		t.Errorf("%d nodes were contacted, want 2", contacted)
	}
}

// TestExecutor_ProbeOnceStopsAfterOneNode is the panel's button: one node, then
// report, whatever happened.
func TestExecutor_ProbeOnceStopsAfterOneNode(t *testing.T) {
	h := newExecHarness(t)

	var nodes []*fakeProxy
	for _, id := range []string{"p1", "p2", "p3"} {
		nodes = append(nodes, h.addProxy(t, id, respondWith(http.StatusOK, "short", "")))
	}

	got := h.executor.ProbeOnce(context.Background(), "codex-auth-1", "gpt-5-codex")
	if got.ProxiesTried != 1 {
		t.Errorf("ProxiesTried = %d, want 1", got.ProxiesTried)
	}
	contacted := 0
	for _, n := range nodes {
		contacted += n.count()
	}
	if contacted != 1 {
		t.Errorf("%d nodes were contacted, want exactly 1", contacted)
	}
}

// TestExecutor_NonTargetCoolsTheProxy pins the second cooldown trigger an
// operator asked for: a node that returns the wrong state length steps aside so
// the next round prefers a different one.
func TestExecutor_NonTargetCoolsTheProxy(t *testing.T) {
	h := newExecHarness(t)
	h.addProxy(t, "only", respondWith(http.StatusOK, "short", ""))

	if got := h.probe(t); got.Outcome != OutcomeSuccessNonTarget {
		t.Fatalf("outcome = %s, want a non-target success", got.Outcome)
	}

	rows := h.pool.CooldownsForProxy("only")
	if len(rows) != 1 {
		t.Fatalf("a non-target result left %d ledger rows, want 1", len(rows))
	}
	row := rows[0]
	if row.CooldownUntil == nil {
		t.Fatal("a non-target result did not cool the node down")
	}
	if row.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", row.ConsecutiveFailures)
	}
	if node, _ := h.pool.Get("only"); node.FailureCount != 1 {
		t.Errorf("FailureCount = %d, want 1", node.FailureCount)
	}
	if got := len(h.pool.AvailableFor("codex-auth-1", time.Now())); got != 0 {
		t.Error("the node is still selectable immediately after a non-target result")
	}
}

// memCooldownKey stands in for the ledger's own key type, which is unexported.
func memCooldownKey(authIndex, proxyID string) string { return authIndex + "\x00" + proxyID }

// TestExecutor_CooldownLadderEscalates walks the real sequence rather than the
// pure function: repeated failures of one account against one node, with the
// clock advanced past each cooldown so the node comes back and fails again.
//
// The ladder position comes from the pair's stored count, and the count is
// incremented while recording the failure -- so the caller has to ask for the
// rung of the failure it is about to record, not the one already on file.
// Testing ProxyCooldown in isolation cannot see that; only the sequence can.
func TestExecutor_CooldownLadderEscalates(t *testing.T) {
	h := newExecHarness(t)
	h.addDeadProxy(t, "dead")

	// Seven rungs to the cap, then the counter resets and the ladder starts
	// over -- which is what keeps an outage longer than the cap from removing
	// the pair permanently.
	want := []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		32 * time.Minute,
		64 * time.Minute,
		1 * time.Minute,
		2 * time.Minute,
	}

	base := time.Now()
	now := base
	h.executor.SetClock(func() time.Time { return now })

	for i := range want {
		h.executor.Probe(context.Background(), "codex-auth-1", "gpt-5-codex")

		rows := h.pool.CooldownsForProxy("dead")
		if len(rows) != 1 {
			t.Fatalf("failure %d: %d ledger rows, want 1", i+1, len(rows))
		}
		row := rows[0]
		if row.CooldownUntil == nil {
			t.Fatalf("failure %d: no cooldown recorded", i+1)
		}
		got := row.CooldownUntil.Sub(now)
		if got != want[i] {
			t.Errorf("failure %d: cooldown %s, want %s", i+1, got, want[i])
		}

		// Past the cooldown so the node is selectable again for this account.
		now = now.Add(got + time.Second)
	}
}

// envelopeToken builds a value in the shape the upstream actually emits, so the
// shape checks can be exercised rather than the length fallback.
func envelopeToken(t *testing.T, blocks int) string {
	t.Helper()
	return envelopeTokenAt(t, blocks, time.Now())
}

// envelopeTokenAt builds a value minted at a chosen moment, so age can be
// varied independently of shape.
func envelopeTokenAt(t *testing.T, blocks int, issued time.Time) string {
	t.Helper()
	raw := make([]byte, 57+16*blocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	for i := 9; i < len(raw); i++ {
		raw[i] = byte(i % 251)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

// TestExecutor_AcceptsTheAccountsShapeNotAFixedLength is the point of reading
// the envelope: a personal account's correct value is a team account's wrong
// one, and the same string cannot be both.
func TestExecutor_AcceptsTheAccountsShapeNotAFixedLength(t *testing.T) {
	cases := []struct {
		name   string
		plan   string
		blocks int
		want   Outcome
	}{
		{"personal account, personal value", "plus", 10, OutcomeSuccessTarget},
		{"personal account, team value", "plus", 12, OutcomeSuccessNonTarget},
		{"team account, team value", "team", 12, OutcomeSuccessTarget},
		{"team account, personal value", "team", 10, OutcomeSuccessNonTarget},
		{"business account, team value", "business", 12, OutcomeSuccessTarget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newExecHarness(t)
			h.plans.set("codex-auth-1", tc.plan)
			h.addProxy(t, "p1", respondWith(http.StatusOK, envelopeToken(t, tc.blocks), ""))

			got := h.probe(t)
			if got.Outcome != tc.want {
				t.Errorf("outcome = %s, want %s", got.Outcome, tc.want)
			}
			if got.StateBlocks != tc.blocks {
				t.Errorf("StateBlocks = %d, want %d", got.StateBlocks, tc.blocks)
			}
		})
	}
}

// TestExecutor_UnknownPlanUsesTheConfiguredLength keeps the setting meaningful
// for an account whose tier the upstream has not stated.
func TestExecutor_UnknownPlanUsesTheConfiguredLength(t *testing.T) {
	h := newExecHarness(t)
	// No plan recorded, and the fake upstream sends no plan header either.
	h.addProxy(t, "p1", respondWith(http.StatusOK, envelopeToken(t, 12), ""))

	// The harness's configured length is 32, which describes no block count, so
	// the personal rule applies and a team value is rejected.
	if got := h.probe(t); got.Outcome != OutcomeSuccessNonTarget {
		t.Errorf("outcome = %s, want a non-target value for an unknown tier", got.Outcome)
	}

	// A non-target result benches the node for this account, so a second probe
	// would find nothing to use. Clear that rather than letting it decide the
	// test's outcome.
	if err := h.pool.ResetAllCooldowns(context.Background()); err != nil {
		t.Fatalf("ResetAllCooldowns: %v", err)
	}

	// Point the configured length at the team shape and the same value binds.
	h.policy.TargetStateLength = states.EncodedLength(12)
	if got := h.probe(t); got.Outcome != OutcomeSuccessTarget {
		t.Errorf("outcome = %s, want acceptance once the configured length matches", got.Outcome)
	}
}

// TestExecutor_PlanOnTheResponseWins covers the first probe of an unseen
// account: the tier arrives on the same response as the value, and waiting for
// the next round would reject a perfectly good state.
func TestExecutor_PlanOnTheResponseWins(t *testing.T) {
	h := newExecHarness(t)
	h.addProxy(t, "p1", func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headers.TurnState, envelopeToken(t, 12))
		w.Header().Set("X-Codex-Plan-Type", "team")
		w.WriteHeader(http.StatusOK)
	})

	if got := h.probe(t); got.Outcome != OutcomeSuccessTarget {
		t.Errorf("outcome = %s, want the team shape accepted on the strength of this response", got.Outcome)
	}
}

// TestExecutor_StaleValueIsNotBound covers the value that arrives already old.
//
// Binding it would replace a working binding with a dead one, and the round
// would then report a target hit and wait a full TTL before trying again -- so
// the pair would go quiet exactly when it should be looking for a fresh value.
// The node is not at fault either: it answered correctly.
func TestExecutor_StaleValueIsNotBound(t *testing.T) {
	h := newExecHarness(t)

	// The upstream mints values with a fortnight-old timestamp.
	stale := envelopeTokenAt(t, 10, time.Now().Add(-14*24*time.Hour))
	h.addProxy(t, "p1", respondWith(http.StatusOK, stale, ""))

	got := h.probe(t)
	if got.Outcome != OutcomeSuccessStale {
		t.Fatalf("outcome = %s, want %s", got.Outcome, OutcomeSuccessStale)
	}
	if got.Succeeded() {
		t.Error("a stale value reported success")
	}
	if rows := h.pool.CooldownsForProxy("p1"); len(rows) != 0 {
		t.Error("a stale value benched the node; the node answered correctly")
	}
}

// TestExecutor_FreshValueIsNotStale is the other side of it: the check must not
// fire on a value that is simply new.
func TestExecutor_FreshValueIsNotStale(t *testing.T) {
	h := newExecHarness(t)
	h.plans.set("codex-auth-1", "plus")
	h.addProxy(t, "p1", respondWith(http.StatusOK, envelopeTokenAt(t, 10, time.Now().Add(-time.Minute)), ""))

	if got := h.probe(t); got.Outcome != OutcomeSuccessTarget {
		t.Errorf("outcome = %s, want a fresh value accepted", got.Outcome)
	}
}
