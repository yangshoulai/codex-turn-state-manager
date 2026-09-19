package probe

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// ---------------------------------------------------------------------------
// doubles

type schedSettingsStore struct {
	mu     sync.Mutex
	values map[string]string
}

func (s *schedSettingsStore) All(context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}

func (s *schedSettingsStore) PutMany(_ context.Context, values map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range values {
		s.values[k] = v
	}
	return nil
}

type schedStateStore struct {
	mu       sync.Mutex
	bindings map[states.Pair]states.Binding
}

func newSchedStateStore() *schedStateStore {
	return &schedStateStore{bindings: map[states.Pair]states.Binding{}}
}

func (s *schedStateStore) UpsertBinding(_ context.Context, b states.Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[b.Pair] = b
	return nil
}

func (s *schedStateStore) DeleteBinding(_ context.Context, p states.Pair) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bindings, p)
	return nil
}

func (s *schedStateStore) ListBindings(context.Context) ([]states.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]states.Binding, 0, len(s.bindings))
	for _, b := range s.bindings {
		out = append(out, b)
	}
	return out, nil
}

func (s *schedStateStore) AppendHistory(context.Context, states.HistoryEntry) error { return nil }
func (s *schedStateStore) ListHistory(context.Context, states.Pair, int, int) ([]states.HistoryEntry, error) {
	return nil, nil
}
func (s *schedStateStore) ClearHistory(context.Context, states.Pair) (int64, error) { return 0, nil }

type schedPolicy struct{ p states.Policy }

func (s schedPolicy) StatePolicy() states.Policy { return s.p }

type fakePairs struct {
	mu      sync.Mutex
	pairs   []states.Pair
	syncs   int
	syncErr error
	// syncBlocks, when set, holds Sync until the channel is closed or the
	// caller's context expires. It stands in for a host call that hangs.
	syncBlocks chan struct{}
}

func (f *fakePairs) EnabledPairs() []states.Pair {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]states.Pair, len(f.pairs))
	copy(out, f.pairs)
	return out
}

func (f *fakePairs) Sync(ctx context.Context) (int, error) {
	f.mu.Lock()
	blocks := f.syncBlocks
	f.syncs++
	n := len(f.pairs)
	f.mu.Unlock()

	if blocks != nil {
		select {
		case <-blocks:
		case <-ctx.Done():
			// A real host call is expected to honour the context; the test is
			// that the caller bounded it at all.
			return 0, ctx.Err()
		}
	}
	return n, f.syncErr
}

func (f *fakePairs) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncs
}

// schedSource implements ConfigSource for tests.
type schedSource struct {
	settings *settings.Manager
	pairs    *fakePairs
	windows  *WindowManager
	states   *states.Registry
	executor *Executor
}

func (s *schedSource) SettingsManager() *settings.Manager   { return s.settings }
func (s *schedSource) EnabledPairSource() EnabledPairSource { return s.pairs }
func (s *schedSource) WindowManager() *WindowManager        { return s.windows }
func (s *schedSource) StateRegistry() *states.Registry      { return s.states }
func (s *schedSource) ProbeExecutor() *Executor             { return s.executor }

// safeClock is a settable clock that probe goroutines can read while the test
// advances it. A bare *time.Time would be a data race: the scheduler calls
// now() from the probe goroutine, not from the test goroutine.
type safeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newSafeClock(t time.Time) *safeClock { return &safeClock{t: t} }

// Now reads the current time.
func (c *safeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Set replaces the current time.
func (c *safeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// Add advances the clock and returns the new time.
func (c *safeClock) Add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

type schedHarness struct {
	scheduler *Scheduler
	source    *schedSource
	pairs     *fakePairs
	settings  *settings.Manager
	states    *states.Registry
	clock     *safeClock
	mu        sync.Mutex
	probes    []states.Pair
	binds     []string
}

func newSchedHarness(t *testing.T, pairs []states.Pair) *schedHarness {
	t.Helper()
	ctx := context.Background()

	manager, err := settings.NewManager(ctx, &schedSettingsStore{values: map[string]string{}}, nil)
	if err != nil {
		t.Fatalf("settings.NewManager: %v", err)
	}

	store := newSchedStateStore()
	clock := newSafeClock(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
	registry := states.NewRegistry(store, store, schedPolicy{p: states.Policy{
		TTL: time.Hour, RefreshThresholdPct: 15, TargetStateLength: 292,
	}})
	registry.SetClock(clock.Now)

	windows := NewWindowManager(newFakeWindowStore())
	if err := windows.Load(ctx); err != nil {
		t.Fatalf("windows.Load: %v", err)
	}

	fp := &fakePairs{pairs: pairs}
	h := &schedHarness{
		pairs:    fp,
		settings: manager,
		states:   registry,
		clock:    clock,
	}
	h.source = &schedSource{
		settings: manager, pairs: fp, windows: windows, states: registry,
	}
	h.scheduler = NewScheduler(SchedulerConfig{Source: h.source})
	h.scheduler.SetClock(clock.Now)
	h.scheduler.SetJitter(func(time.Duration) time.Duration { return 0 })

	h.scheduler.SetProbeFunc(func(ctx context.Context, authIndex, model string) Result {
		h.mu.Lock()
		h.probes = append(h.probes, states.Pair{AuthIndex: authIndex, Model: model})
		h.mu.Unlock()
		return Result{Outcome: OutcomeSuccessTarget, StateValue: "target", StateLength: 292}
	})
	h.scheduler.SetBindFunc(func(_ context.Context, authIndex, model, value, proxyID string, issued time.Time) error {
		h.mu.Lock()
		h.binds = append(h.binds, authIndex+"/"+model)
		h.mu.Unlock()
		_, err := registry.Bind(context.Background(), states.Binding{
			Pair:       states.Pair{AuthIndex: authIndex, Model: model},
			StateValue: value,
			Source:     states.SourceProbe,
		})
		return err
	})
	return h
}

func (h *schedHarness) probeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.probes)
}

// waitFor polls until cond holds or the deadline passes. The scheduler runs
// probes in goroutines, so the tests must wait rather than assert immediately.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// tests

func TestScan_ProbesDuePairsOnce(t *testing.T) {
	h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})

	h.scheduler.Scan(context.Background())
	if !waitFor(t, func() bool { return h.probeCount() == 1 }) {
		t.Fatalf("probes = %d, want 1", h.probeCount())
	}

	// A second scan immediately afterwards must not re-probe: the success path
	// schedules the next attempt one refresh window out.
	h.scheduler.Scan(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := h.probeCount(); got != 1 {
		t.Errorf("probes after a second scan = %d, want 1", got)
	}
}

func TestScan_SkipsFreshBindings(t *testing.T) {
	h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})

	if _, err := h.states.Bind(context.Background(), states.Binding{
		Pair: states.Pair{AuthIndex: "a", Model: "m"}, StateValue: "target", Source: states.SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	h.scheduler.Scan(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := h.probeCount(); got != 0 {
		t.Errorf("probes = %d, want 0 for a fresh binding", got)
	}
}

func TestScan_ProbesRefreshDueBindings(t *testing.T) {
	h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})

	if _, err := h.states.Bind(context.Background(), states.Binding{
		Pair: states.Pair{AuthIndex: "a", Model: "m"}, StateValue: "target", Source: states.SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// Into the refresh window: still valid, but worth renewing.
	h.clock.Add(55 * time.Minute)
	h.scheduler.Scan(context.Background())

	if !waitFor(t, func() bool { return h.probeCount() == 1 }) {
		t.Errorf("probes = %d, want 1 for a refresh_due binding", h.probeCount())
	}
}

func TestScan_SkipsWhenProbeSwitchOff(t *testing.T) {
	ctx := context.Background()
	h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})

	off := false
	if _, err := h.settings.Update(ctx, settings.Patch{GlobalProbeEnabled: &off}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	h.scheduler.Scan(ctx)
	time.Sleep(50 * time.Millisecond)
	if got := h.probeCount(); got != 0 {
		t.Errorf("probes = %d, want 0 while the probe switch is off", got)
	}
}

func TestScan_SkipsWhenMasterSwitchOff(t *testing.T) {
	ctx := context.Background()
	h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})

	off := false
	if _, err := h.settings.Update(ctx, settings.Patch{GlobalEnabled: &off}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	h.scheduler.Scan(ctx)
	time.Sleep(50 * time.Millisecond)
	if got := h.probeCount(); got != 0 {
		t.Errorf("probes = %d, want 0 while the master switch is off", got)
	}
}

func TestScan_SkipsOutsideTimeWindow(t *testing.T) {
	ctx := context.Background()
	h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})

	// The harness clock is 12:00 UTC, so a window of 22:00-23:00 excludes it.
	if err := h.source.windows.Upsert(ctx, TimeWindow{
		ID: "night", StartTime: "22:00", EndTime: "23:00", Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert window: %v", err)
	}

	h.scheduler.Scan(ctx)
	time.Sleep(50 * time.Millisecond)
	if got := h.probeCount(); got != 0 {
		t.Errorf("probes = %d, want 0 outside the configured window", got)
	}

	// Inside the window it runs.
	h.clock.Set(time.Date(2026, 9, 18, 22, 30, 0, 0, time.UTC))
	h.scheduler.Scan(ctx)
	if !waitFor(t, func() bool { return h.probeCount() == 1 }) {
		t.Errorf("probes = %d, want 1 inside the window", h.probeCount())
	}
}

// TestScan_DiscardsResultWhenSwitchFlipsMidProbe is the concurrency rule in
// design doc 3.3: a probe already in flight when the master switch goes off
// must not write a binding.
func TestScan_DiscardsResultWhenSwitchFlipsMidProbe(t *testing.T) {
	ctx := context.Background()
	h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})

	release := make(chan struct{})
	h.scheduler.SetProbeFunc(func(ctx context.Context, authIndex, model string) Result {
		<-release // hold the probe open
		return Result{Outcome: OutcomeSuccessTarget, StateValue: "target", StateLength: 292}
	})

	h.scheduler.Scan(ctx)
	if !waitFor(t, func() bool { return h.scheduler.InFlight(states.Pair{AuthIndex: "a", Model: "m"}) }) {
		t.Fatal("probe never started")
	}

	off := false
	if _, err := h.settings.Update(ctx, settings.Patch{GlobalEnabled: &off}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	close(release)

	// Give the probe time to return and be discarded.
	time.Sleep(100 * time.Millisecond)

	if _, status := h.states.Lookup("a", "m"); status != states.StatusMissing {
		t.Error("a probe that completed after the master switch went off must not bind")
	}
	h.mu.Lock()
	binds := len(h.binds)
	h.mu.Unlock()
	if binds != 0 {
		t.Errorf("binds = %d, want 0", binds)
	}
}

// TestScan_AccountSyncRespectsItsOwnCadence guards against the sync interval
// being a knob that does nothing.
func TestScan_AccountSyncRespectsItsOwnCadence(t *testing.T) {
	h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})

	// The default sync interval is 5 minutes; a scan tick is 60 seconds.
	h.scheduler.Scan(context.Background())
	h.scheduler.Scan(context.Background())
	h.scheduler.Scan(context.Background())
	if got := h.pairs.syncCount(); got != 1 {
		t.Errorf("syncs after three scans in the same minute = %d, want 1", got)
	}

	h.clock.Add(6 * time.Minute)
	h.scheduler.Scan(context.Background())
	if got := h.pairs.syncCount(); got != 2 {
		t.Errorf("syncs after the interval elapsed = %d, want 2", got)
	}
}

func TestScan_RespectsProbeConcurrency(t *testing.T) {
	ctx := context.Background()
	pairs := make([]states.Pair, 6)
	for i := range pairs {
		pairs[i] = states.Pair{AuthIndex: "a", Model: string(rune('a' + i))}
	}
	h := newSchedHarness(t, pairs)

	release := make(chan struct{})
	var concurrent, peak int
	var mu sync.Mutex

	h.scheduler.SetProbeFunc(func(ctx context.Context, authIndex, model string) Result {
		mu.Lock()
		concurrent++
		if concurrent > peak {
			peak = concurrent
		}
		mu.Unlock()

		<-release

		mu.Lock()
		concurrent--
		mu.Unlock()
		return Result{Outcome: OutcomeSuccessNonTarget}
	})

	h.scheduler.Scan(ctx)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	gotPeak := peak
	mu.Unlock()
	close(release)

	// Default concurrency is 2; a third slot must never be handed out.
	if gotPeak > 2 {
		t.Errorf("peak concurrency = %d, want at most 2", gotPeak)
	}
	if gotPeak == 0 {
		t.Error("no probes ran")
	}
}

func TestScheduler_StopIsIdempotent(t *testing.T) {
	h := newSchedHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.scheduler.Start(ctx)
	h.scheduler.Stop()
	h.scheduler.Stop() // must not panic or block
}

func TestLogLevelFields_AreSerialisable(t *testing.T) {
	// A smoke test that the logger contract accepts nil fields, which the
	// app's adapters rely on.
	var calls int
	s := NewScheduler(SchedulerConfig{
		Source: nil,
		Log:    func(hostapi.LogLevel, string, map[string]any) { calls++ },
	})
	s.log(hostapi.LogInfo, "hello", nil)
	if calls != 1 {
		t.Errorf("log calls = %d, want 1", calls)
	}
}

// TestSchedulerScanHealthStampsEveryTick guards the figure that tells a stalled
// scan loop apart from an idle one.
//
// The two are identical from every other signal the panel has: pairs overdue,
// no new probe rows, no error. Only the age of the last scan separates them, so
// the stamp has to happen before any gate -- including the ones that return
// without probing anything, which is exactly the state an operator is in when
// they go looking.
func TestSchedulerScanHealthStampsEveryTick(t *testing.T) {
	h := newSchedHarness(t, nil)

	if got := h.scheduler.ScanHealth().LastScanAt; !got.IsZero() {
		t.Fatalf("LastScanAt = %v before any scan, want the zero time", got)
	}

	// Probing off: the scan returns at its first gate without doing anything.
	// It still has to record that it ran.
	off := false
	if _, err := h.settings.Update(context.Background(), settings.Patch{
		GlobalProbeEnabled: &off,
	}); err != nil {
		t.Fatalf("settings.Update: %v", err)
	}

	h.scheduler.Scan(context.Background())

	stamped := h.scheduler.ScanHealth().LastScanAt
	if stamped.IsZero() {
		t.Fatal("a gated-off scan did not stamp LastScanAt; a stalled loop would be invisible")
	}
	if !stamped.Equal(h.clock.Now()) {
		t.Errorf("LastScanAt = %v, want the scheduler clock %v", stamped, h.clock.Now())
	}
}

// TestSchedulerScanSurvivesASlowSync is the guard for a scan loop that stops
// forever.
//
// The account sync runs on the scan goroutine and is the only thing in a scan
// that talks to the host. Unbounded, a call that never returns ends every future
// scan: no pair is scheduled again, nothing is logged, and the panel shows
// overdue pairs that simply never run. The initial sync carried a timeout and
// this one did not.
func TestSchedulerScanSurvivesASlowSync(t *testing.T) {
	h := newSchedHarness(t, nil)
	// Short enough to keep the test fast, long enough to be a real wait.
	h.scheduler.syncTimeout = 20 * time.Millisecond

	// Hold the sync far past the timeout, then release it.
	release := make(chan struct{})
	h.pairs.syncBlocks = release
	defer close(release)

	done := make(chan struct{})
	go func() {
		h.scheduler.Scan(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Scan did not return while the account sync was blocked; the scan loop would stop permanently")
	}
}
