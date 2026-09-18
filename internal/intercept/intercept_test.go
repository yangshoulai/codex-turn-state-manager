package intercept

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

const targetLength = 292

// ---------------------------------------------------------------------------
// doubles

type memSettingsStore struct {
	mu     sync.Mutex
	values map[string]string
}

func (s *memSettingsStore) All(context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}

func (s *memSettingsStore) PutMany(_ context.Context, values map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range values {
		s.values[k] = v
	}
	return nil
}

type memStateStore struct {
	mu       sync.Mutex
	bindings map[states.Pair]states.Binding
	history  []states.HistoryEntry
}

func newMemStateStore() *memStateStore {
	return &memStateStore{bindings: map[states.Pair]states.Binding{}}
}

func (s *memStateStore) UpsertBinding(_ context.Context, b states.Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[b.Pair] = b
	return nil
}

func (s *memStateStore) DeleteBinding(_ context.Context, p states.Pair) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bindings, p)
	return nil
}

func (s *memStateStore) ListBindings(context.Context) ([]states.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]states.Binding, 0, len(s.bindings))
	for _, b := range s.bindings {
		out = append(out, b)
	}
	return out, nil
}

func (s *memStateStore) AppendHistory(_ context.Context, e states.HistoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, e)
	return nil
}

func (s *memStateStore) ListHistory(context.Context, states.Pair, int, int) ([]states.HistoryEntry, error) {
	return nil, nil
}

func (s *memStateStore) ClearHistory(context.Context, states.Pair) (int64, error) { return 0, nil }

func (s *memStateStore) historyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.history)
}

type fixedPolicy struct{ p states.Policy }

func (f fixedPolicy) StatePolicy() states.Policy { return f.p }

type harness struct {
	settings  *settings.Manager
	states    *states.Registry
	store     *memStateStore
	corr      *CorrelationManager
	injector  *Injector
	collector *Collector
	clock     *time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	manager, err := settings.NewManager(ctx, &memSettingsStore{values: map[string]string{}}, nil)
	if err != nil {
		t.Fatalf("settings.NewManager: %v", err)
	}

	store := newMemStateStore()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	clock := &now

	registry := states.NewRegistry(store, store, fixedPolicy{p: states.Policy{
		TTL: time.Hour, RefreshThresholdPct: 15, TargetStateLength: targetLength,
	}})
	registry.SetClock(func() time.Time { return *clock })

	corr := NewCorrelationManager(time.Hour)
	corr.SetClock(func() time.Time { return *clock })

	stats := &Stats{}
	return &harness{
		settings: manager,
		states:   registry,
		store:    store,
		corr:     corr,
		injector: NewInjector(InjectorConfig{
			Settings: manager, States: registry, Corr: corr, Stats: stats,
		}),
		collector: NewCollector(CollectorConfig{
			Settings: manager, States: registry, Corr: corr, Stats: stats,
		}),
		clock: clock,
	}
}

func (h *harness) bind(t *testing.T, authIndex, model, value string) {
	t.Helper()
	if _, err := h.states.Bind(context.Background(), states.Binding{
		Pair:       states.Pair{AuthIndex: authIndex, Model: model},
		StateValue: value, Source: states.SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
}

func stateOf(n int) string { return strings.Repeat("s", n) }

// request builds an after-auth request as the ABI adapter would: the selected
// account is already resolved from the host's Metadata.
func request(model, authID, authIndex string) *hostapi.InterceptedRequest {
	return &hostapi.InterceptedRequest{
		Stage:     hostapi.StageAfterAuth,
		RequestID: "req-1",
		Provider:  hostapi.ProviderCodex,
		Model:     model,
		AuthID:    authID,
		AuthIndex: authIndex,
		Headers:   http.Header{},
	}
}

// headerInit builds the header-only stream initialisation payload.
func headerInit(model, authIndex, value string) hostapi.StreamChunk {
	h := http.Header{}
	if value != "" {
		h.Set(headers.TurnState, value)
	}
	return hostapi.StreamChunk{
		RequestID:       "req-1",
		Model:           model,
		AuthIndex:       authIndex,
		ChunkIndex:      hostapi.StreamChunkHeaderInitIndex,
		ResponseHeaders: h,
	}
}

// ---------------------------------------------------------------------------
// injection

func TestInject_WritesStateAndRecordsIt(t *testing.T) {
	h := newHarness(t)
	h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))

	req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
	got := h.injector.Inject(req)
	if got.Action != ActionInjected {
		t.Fatalf("action = %s, want injected", got.Action)
	}
	if len(req.Headers.Get(headers.TurnState)) != targetLength {
		t.Errorf("injected length = %d, want %d", len(req.Headers.Get(headers.TurnState)), targetLength)
	}
	// Nothing internal is ever added to the request, so there is nothing to
	// strip afterwards.
	if len(req.ClearHeaders) != 0 {
		t.Errorf("ClearHeaders = %v, want empty", req.ClearHeaders)
	}

	rec, ok := h.corr.Get("req-1")
	if !ok || !rec.Injected {
		t.Error("the injection must be recorded, since self-healing depends on it")
	}
}

func TestInject_NoBindingLeavesTheRequestAlone(t *testing.T) {
	h := newHarness(t)

	req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
	got := h.injector.Inject(req)
	if got.Action != ActionNoBinding {
		t.Fatalf("action = %s, want no_binding", got.Action)
	}
	if req.Headers.Get(headers.TurnState) != "" {
		t.Error("no state should be injected without a binding")
	}
}

// TestInject_UnknownAccountIsNotAnError covers the metadata caveat: the host's
// selected-auth metadata is a best-effort snapshot, so a missing key must
// degrade to "no injection", never to a failed request.
func TestInject_UnknownAccountIsNotAnError(t *testing.T) {
	h := newHarness(t)
	h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))

	req := request("gpt-5-codex", "", "")
	got := h.injector.Inject(req)
	if got.Action != ActionUnresolvedAuth {
		t.Fatalf("action = %s, want unresolved_auth", got.Action)
	}
	if req.Headers.Get(headers.TurnState) != "" {
		t.Error("state must not be injected without a known account")
	}
}

func TestInject_PassesThroughWhenMasterSwitchOff(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))

	off := false
	if _, err := h.settings.Update(ctx, settings.Patch{GlobalEnabled: &off}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
	if got := h.injector.Inject(req); got.Action != ActionPassthrough {
		t.Errorf("action = %s, want passthrough", got.Action)
	}
	if req.Headers.Get(headers.TurnState) != "" {
		t.Error("state must not be injected while the master switch is off")
	}
}

func TestInject_IgnoresExpiredBinding(t *testing.T) {
	h := newHarness(t)
	h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))
	*h.clock = h.clock.Add(2 * time.Hour)

	req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
	if got := h.injector.Inject(req); got.Action != ActionNoBinding {
		t.Errorf("action = %s, want no_binding for an expired state", got.Action)
	}
}

// ---------------------------------------------------------------------------
// correlation

func TestCorrelation_RecordsAndCompletes(t *testing.T) {
	c := NewCorrelationManager(time.Minute)
	c.Record("req-1", "gpt-5-codex", "auth-id-1", "codex-auth-1")
	c.MarkInjected("req-1", "value")

	rec, ok := c.Get("req-1")
	if !ok {
		t.Fatal("record missing")
	}
	if rec.AuthIndex != "codex-auth-1" || rec.AuthID != "auth-id-1" {
		t.Errorf("auth = %s/%s, want codex-auth-1/auth-id-1", rec.AuthIndex, rec.AuthID)
	}
	if !rec.Injected || rec.StateValue != "value" {
		t.Errorf("Injected = %v, state = %q", rec.Injected, rec.StateValue)
	}

	if _, ok := c.Complete("req-1"); !ok {
		t.Error("Complete should return the record")
	}
	if _, ok := c.Get("req-1"); ok {
		t.Error("the record should be gone after Complete")
	}
}

func TestCorrelation_SweepDropsExpiredEntries(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	clock := &now
	c := NewCorrelationManager(10 * time.Minute)
	c.SetClock(func() time.Time { return *clock })

	c.Record("old", "m", "", "")
	*clock = clock.Add(11 * time.Minute)
	c.Record("fresh", "m", "", "")

	if dropped := c.Sweep(); dropped != 1 {
		t.Errorf("Sweep dropped %d, want 1", dropped)
	}
	if _, ok := c.Get("old"); ok {
		t.Error("the expired record should be gone")
	}
	if _, ok := c.Get("fresh"); !ok {
		t.Error("the fresh record should survive")
	}
}

// ---------------------------------------------------------------------------
// response capture

func TestObserve_BindsTargetLengthFromTraffic(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.corr.Record("req-1", "gpt-5-codex", "auth-id-1", "codex-auth-1")

	got := h.collector.Observe(ctx, headerInit("gpt-5-codex", "codex-auth-1", stateOf(targetLength)))
	if got.Action != CaptureBound || !got.Bound {
		t.Fatalf("action = %s (bound %v), want bound", got.Action, got.Bound)
	}

	binding, status := h.states.Lookup("codex-auth-1", "gpt-5-codex")
	if !status.Usable() {
		t.Fatalf("status = %s, want a usable binding", status)
	}
	if binding.Source != states.SourceTraffic {
		t.Errorf("source = %s, want traffic", binding.Source)
	}
	if h.store.historyCount() != 1 {
		t.Errorf("history rows = %d, want 1", h.store.historyCount())
	}
}

// TestObserve_IgnoresPayloadChunks pins that only the header-init call carries
// the initial upstream headers.
func TestObserve_IgnoresPayloadChunks(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.corr.Record("req-1", "gpt-5-codex", "auth-id-1", "codex-auth-1")

	chunk := headerInit("gpt-5-codex", "codex-auth-1", stateOf(targetLength))
	chunk.ChunkIndex = 0 // a payload chunk

	got := h.collector.Observe(ctx, chunk)
	if got.Action != CaptureNotHeaderInit {
		t.Errorf("action = %s, want not_header_init", got.Action)
	}
	if _, status := h.states.Lookup("codex-auth-1", "gpt-5-codex"); status != states.StatusMissing {
		t.Error("a payload chunk must not create a binding")
	}
}

func TestObserve_RefreshesWithoutRecordingHistory(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	value := stateOf(targetLength)
	h.bind(t, "codex-auth-1", "gpt-5-codex", value)

	req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
	if got := h.injector.Inject(req); got.Action != ActionInjected {
		t.Fatalf("setup: action = %s, want injected", got.Action)
	}

	before := h.store.historyCount()
	*h.clock = h.clock.Add(10 * time.Minute)

	got := h.collector.Observe(ctx, headerInit("gpt-5-codex", "codex-auth-1", value))
	if got.Action != CaptureRefreshed || !got.Refreshed {
		t.Fatalf("action = %s, want refreshed", got.Action)
	}
	if h.store.historyCount() != before {
		t.Error("a same-value refresh must not append a history row")
	}

	binding, _ := h.states.Lookup("codex-auth-1", "gpt-5-codex")
	if !binding.BoundAt.Equal(*h.clock) {
		t.Errorf("BoundAt = %v, want %v (the TTL should have been extended)", binding.BoundAt, *h.clock)
	}
}

// TestObserve_RejectsNonTargetLength is F-15: record it, never bind it.
func TestObserve_RejectsNonTargetLength(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	for _, n := range []int{0, 100, targetLength - 1, targetLength + 1} {
		h.corr.Record("req-1", "gpt-5-codex", "auth-id-1", "codex-auth-1")
		got := h.collector.Observe(ctx, headerInit("gpt-5-codex", "codex-auth-1", stateOf(n)))

		if n == 0 {
			if got.Action != CaptureIgnored {
				t.Errorf("length 0: action = %s, want ignored", got.Action)
			}
		} else if got.Action != CaptureNonTarget {
			t.Errorf("length %d: action = %s, want non_target", n, got.Action)
		}
	}

	if _, status := h.states.Lookup("codex-auth-1", "gpt-5-codex"); status != states.StatusMissing {
		t.Error("a non-target length must never create a binding")
	}
	if h.store.historyCount() != 0 {
		t.Errorf("history rows = %d, want 0", h.store.historyCount())
	}
}

func TestObserve_IgnoredWhenCaptureSwitchOff(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	off := false
	if _, err := h.settings.Update(ctx, settings.Patch{GlobalReverseBindEnabled: &off}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	h.corr.Record("req-1", "gpt-5-codex", "auth-id-1", "codex-auth-1")

	got := h.collector.Observe(ctx, headerInit("gpt-5-codex", "codex-auth-1", stateOf(targetLength)))
	if got.Action != CaptureSkipped {
		t.Errorf("action = %s, want skipped", got.Action)
	}
	if _, status := h.states.Lookup("codex-auth-1", "gpt-5-codex"); status != states.StatusMissing {
		t.Error("no binding may be created while reverse binding is off")
	}
}

// TestObserve_DoesNotRefreshExistingBindingWhenCaptureOff covers the subtler
// half of the switch: an existing binding must not have its TTL extended
// either, or "off" would still mean "partially on".
func TestObserve_DoesNotRefreshExistingBindingWhenCaptureOff(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	value := stateOf(targetLength)
	h.bind(t, "codex-auth-1", "gpt-5-codex", value)

	before, _ := h.states.Lookup("codex-auth-1", "gpt-5-codex")

	off := false
	if _, err := h.settings.Update(ctx, settings.Patch{GlobalReverseBindEnabled: &off}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	h.corr.Record("req-1", "gpt-5-codex", "auth-id-1", "codex-auth-1")

	*h.clock = h.clock.Add(20 * time.Minute)
	h.collector.Observe(ctx, headerInit("gpt-5-codex", "codex-auth-1", value))

	after, _ := h.states.Lookup("codex-auth-1", "gpt-5-codex")
	if !after.BoundAt.Equal(before.BoundAt) {
		t.Errorf("BoundAt moved from %v to %v; the TTL must not be refreshed while capture is off",
			before.BoundAt, after.BoundAt)
	}
}

func TestObserve_FallsBackToTheRecordedAccount(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// The host did not publish the account on this payload, but the injection
	// stage recorded it.
	h.corr.Record("req-1", "gpt-5-codex", "auth-id-1", "codex-auth-1")

	got := h.collector.Observe(ctx, headerInit("gpt-5-codex", "", stateOf(targetLength)))
	if got.Action != CaptureBound {
		t.Fatalf("action = %s, want bound via the recorded account", got.Action)
	}
	if _, status := h.states.Lookup("codex-auth-1", "gpt-5-codex"); !status.Usable() {
		t.Error("the binding should have been created for the recorded account")
	}
}

func TestObserve_WithoutAnyAccountIsIgnored(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	got := h.collector.Observe(ctx, headerInit("gpt-5-codex", "", stateOf(targetLength)))
	if got.Action != CaptureNoAuth {
		t.Errorf("action = %s, want no_auth", got.Action)
	}
}

// ---------------------------------------------------------------------------
// self-healing

func TestObserveCompletion_InvalidatesInjectedState(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))

	req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
	if got := h.injector.Inject(req); got.Action != ActionInjected {
		t.Fatalf("setup: action = %s, want injected", got.Action)
	}

	got := h.collector.ObserveCompletion(ctx, hostapi.Completion{
		RequestID: "req-1", Model: "gpt-5-codex",
		Outcome: hostapi.CompletionFailed, StatusCode: 400,
	}, []byte(`{"error":{"code":"previous_response_not_found"}}`))

	if !got.Invalidated {
		t.Fatal("expected the binding to be invalidated")
	}
	if _, status := h.states.Lookup("codex-auth-1", "gpt-5-codex"); status != states.StatusMissing {
		t.Error("the bad binding should be gone")
	}
}

// TestObserveCompletion_IgnoresRequestsThatCarriedNoInjectedState is the
// precondition in design doc 3.12: an unrelated failure must not throw away
// good state.
func TestObserveCompletion_IgnoresRequestsThatCarriedNoInjectedState(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))

	// The request was recorded but nothing was injected.
	h.corr.Record("req-1", "gpt-5-codex", "auth-id-1", "codex-auth-1")

	got := h.collector.ObserveCompletion(ctx, hostapi.Completion{
		RequestID: "req-1", Model: "gpt-5-codex",
		Outcome: hostapi.CompletionFailed, StatusCode: 400,
	}, []byte(`{"error":{"code":"previous_response_not_found"}}`))

	if got.Invalidated {
		t.Error("a request without injected state must not invalidate a binding")
	}
	if _, status := h.states.Lookup("codex-auth-1", "gpt-5-codex"); !status.Usable() {
		t.Error("the binding should have survived")
	}
}

func TestObserveCompletion_IgnoresNonFailures(t *testing.T) {
	ctx := context.Background()

	for _, outcome := range []hostapi.CompletionOutcome{
		hostapi.CompletionSucceeded,
		hostapi.CompletionRejected, // our own interceptor stopped it
		hostapi.CompletionCanceled, // the client went away
	} {
		t.Run(string(outcome), func(t *testing.T) {
			h := newHarness(t)
			h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))
			req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
			h.injector.Inject(req)

			got := h.collector.ObserveCompletion(ctx, hostapi.Completion{
				RequestID: "req-1", Model: "gpt-5-codex",
				Outcome: outcome, StatusCode: 400,
			}, []byte("previous_response_not_found"))

			if got.Invalidated {
				t.Errorf("outcome %s must not invalidate a binding", outcome)
			}
			if _, status := h.states.Lookup("codex-auth-1", "gpt-5-codex"); !status.Usable() {
				t.Error("the binding should have survived")
			}
		})
	}
}

func TestObserveCompletion_RequiresARepeatableSignal(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))

	for i := 1; i <= consecutive5xxLimit; i++ {
		id := "req-" + string(rune('0'+i))
		req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
		req.RequestID = id
		if got := h.injector.Inject(req); got.Action != ActionInjected {
			t.Fatalf("iteration %d: setup expected injection, got %s", i, got.Action)
		}

		got := h.collector.ObserveCompletion(ctx, hostapi.Completion{
			RequestID: id, Model: "gpt-5-codex",
			Outcome: hostapi.CompletionFailed, StatusCode: 503,
		}, nil)

		if i < consecutive5xxLimit {
			if got.Invalidated {
				t.Fatalf("failure %d of %d invalidated too early", i, consecutive5xxLimit)
			}
			continue
		}
		if !got.Invalidated {
			t.Fatalf("failure %d should have invalidated the binding", i)
		}
	}
}

// TestObserveCompletion_BareReasonlessFailureIsNotEnough pins that a failure we
// cannot attribute to the injected state does not evict it.
func TestObserveCompletion_BareReasonlessFailureIsNotEnough(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))

	req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
	h.injector.Inject(req)

	// A 4xx that is neither 400-with-a-known-marker nor a 5xx run.
	got := h.collector.ObserveCompletion(ctx, hostapi.Completion{
		RequestID: "req-1", Model: "gpt-5-codex",
		Outcome: hostapi.CompletionFailed, StatusCode: 422,
	}, []byte(`{"error":"unprocessable"}`))

	if got.Invalidated {
		t.Error("an unattributable failure must not invalidate the binding")
	}
	if _, status := h.states.Lookup("codex-auth-1", "gpt-5-codex"); !status.Usable() {
		t.Error("the binding should have survived")
	}
}

func TestObserveCompletion_SkippedWhenMasterSwitchOff(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))

	req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
	h.injector.Inject(req)

	off := false
	if _, err := h.settings.Update(ctx, settings.Patch{GlobalEnabled: &off}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := h.collector.ObserveCompletion(ctx, hostapi.Completion{
		RequestID: "req-1", Model: "gpt-5-codex",
		Outcome: hostapi.CompletionFailed, StatusCode: 400,
	}, []byte("previous_response_not_found")); got.Invalidated {
		t.Error("self-healing must not run while the master switch is off")
	}
}

// TestStatsCountPipelineActivity covers the counters the panel reports.
//
// They exist because a header rewrite leaves no trace: without them, "has the
// plugin ever injected anything?" can only be answered by logging every request,
// which a proxy should not do.
func TestStatsCountPipelineActivity(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.bind(t, "codex-auth-1", "gpt-5-codex", stateOf(targetLength))

	// An account with a usable binding.
	req := request("gpt-5-codex", "auth-id-1", "codex-auth-1")
	h.injector.Inject(req)

	// An account with none.
	req2 := request("gpt-5-codex", "auth-id-1", "codex-auth-unknown")
	h.injector.Inject(req2)

	// A request where the host published no account at all.
	req3 := request("gpt-5-codex", "", "")
	h.injector.Inject(req3)

	// A capture of a new value, then a repeat of it.
	h.corr.Record("cap-1", "gpt-5-codex", "auth-id-1", "codex-auth-2")
	h.collector.Observe(ctx, headerInit("gpt-5-codex", "codex-auth-2", stateOf(targetLength)))
	h.corr.Record("cap-2", "gpt-5-codex", "auth-id-1", "codex-auth-2")
	h.collector.Observe(ctx, headerInit("gpt-5-codex", "codex-auth-2", stateOf(targetLength)))

	got := h.injector.stats.Snapshot()
	if got.RequestsSeen != 3 {
		t.Errorf("RequestsSeen = %d, want 3", got.RequestsSeen)
	}
	if got.Injected != 1 {
		t.Errorf("Injected = %d, want 1", got.Injected)
	}
	if got.NoBinding != 1 {
		t.Errorf("NoBinding = %d, want 1", got.NoBinding)
	}
	if got.UnresolvedAuth != 1 {
		t.Errorf("UnresolvedAuth = %d, want 1", got.UnresolvedAuth)
	}
	if got.Captured != 1 {
		t.Errorf("Captured = %d, want 1 (a repeat is not a new capture)", got.Captured)
	}
	if got.CapturedReused != 1 {
		t.Errorf("CapturedReused = %d, want 1", got.CapturedReused)
	}
}

// TestObserveResolvesAccountFromCorrelation pins where the account identity
// comes from on the streaming path.
//
// The host's stream-chunk payload does not include selected_auth_index --
// measured against a live instance, not assumed -- so the correlation record the
// injector wrote is the only source. Both the capture and the account-state
// signals depend on resolving it before capture consumes the record.
func TestObserveResolvesAccountFromCorrelation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	var gotAuthIndex string
	var gotSignals headers.Signals
	h.collector.signals = func(authIndex string, signals headers.Signals) {
		gotAuthIndex, gotSignals = authIndex, signals
	}

	// A request whose payload carries no account, only a correlation record.
	h.corr.Record("req-1", "gpt-5-codex", "auth-id-1", "codex-auth-1")

	chunk := headerInit("gpt-5-codex", "", stateOf(targetLength))
	chunk.ResponseHeaders.Set(headers.SignalPlanType, "pro")
	chunk.ResponseHeaders.Set(headers.SignalPrimaryUsedPercent, "42")

	if got := h.collector.Observe(ctx, chunk); got.Action != CaptureBound {
		t.Fatalf("action = %s, want bound via the resolved account", got.Action)
	}
	if gotAuthIndex != "codex-auth-1" {
		t.Errorf("signals recorded against %q, want codex-auth-1", gotAuthIndex)
	}
	if gotSignals.PlanType != "pro" {
		t.Errorf("PlanType = %q, want pro", gotSignals.PlanType)
	}
	if gotSignals.PrimaryUsedPercent == nil || *gotSignals.PrimaryUsedPercent != 42 {
		t.Errorf("PrimaryUsedPercent = %v, want 42", gotSignals.PrimaryUsedPercent)
	}
}

// TestObserveSnapshotRecordsCorrelation pins that the diagnostic snapshot
// reports whether a correlation record was found. The field existed but was
// never populated, so it always read false -- a diagnostic that lies is worse
// than none.
func TestObserveSnapshotRecordsCorrelation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	h.corr.Record("req-1", "gpt-5-codex", "auth-id-1", "codex-auth-1")
	h.collector.Observe(ctx, headerInit("gpt-5-codex", "", ""))

	snap := h.collector.stats.LastHeaderInit()
	if snap == nil {
		t.Fatal("no snapshot recorded")
	}
	if !snap.Correlated {
		t.Error("Correlated = false with a record present")
	}
	if snap.AuthIndex != "codex-auth-1" {
		t.Errorf("AuthIndex = %q, want codex-auth-1", snap.AuthIndex)
	}
}
