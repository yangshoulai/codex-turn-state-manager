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
	// accountSyncs counts the per-account refreshes the scan performed, so a
	// test can pin that it happens once per account per scan rather than once
	// per pair.
	accountSyncs int
	// removed names accounts CPA no longer lists.
	removed []string
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

// SyncAccount stands in for the per-probe account refresh. removed holds the
// accounts CPA no longer lists, so a test can drop one mid-run.
func (f *fakePairs) SyncAccount(_ context.Context, authIndex string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accountSyncs++
	if f.syncErr != nil {
		return false, f.syncErr
	}
	for _, gone := range f.removed {
		if gone == authIndex {
			return false, nil
		}
	}
	return true, nil
}

func (f *fakePairs) accountSyncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accountSyncs
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

// TestScheduler_NonTargetBackoffGrowsThroughTheScan is the wiring check: the
// pure delay function is only half of it, and the run length has to actually
// reach the schedule the scan writes.
//
// A pair that keeps answering with the wrong shape used to be re-probed every
// few minutes forever, and every round walks the proxy pool. The cost of
// learning the same thing repeatedly lands on the proxies, which is what the
// escalation exists to stop.
func TestScheduler_NonTargetBackoffGrowsThroughTheScan(t *testing.T) {
	ctx := context.Background()
	pair := states.Pair{AuthIndex: "codex-auth-1", Model: "gpt-5-codex"}
	h := newSchedHarness(t, []states.Pair{pair})

	// Every probe misses, which is the case being scheduled for.
	h.scheduler.SetProbeFunc(func(context.Context, string, string) Result {
		return Result{Outcome: OutcomeSuccessNonTarget, StateValue: "short", StateLength: 5}
	})

	// Scan starts probes in goroutines, so it returns before the pair has been
	// rescheduled. Wait for the schedule rather than assuming Scan's return
	// means the round is over.
	awaitScheduled := func(round int, after time.Time) time.Time {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			// Strictly after this round's start: the previous round's schedule
			// is still in the map until the probe finishes, so "is it set" would
			// read the stale one and return immediately.
			if next, ok := h.scheduler.NextProbeAt(pair); ok && next.After(after) {
				return next
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("round %d: no next probe scheduled", round)
		return time.Time{}
	}

	var delays []time.Duration
	for round := 0; round < 7; round++ {
		at := h.clock.Now()
		h.scheduler.Scan(ctx)
		next := awaitScheduled(round, at)
		delays = append(delays, next.Sub(at))

		// Let the scheduled time arrive so the next scan picks the pair up
		// again; without this the scan would find nothing due and the run would
		// never grow.
		h.clock.Add(next.Sub(at) + time.Second)
	}

	// The first three rounds stay at the base cadence; the interval then grows.
	if delays[0] != NonTargetMinDelay {
		t.Errorf("round 1 delay = %s, want the base %s", delays[0], NonTargetMinDelay)
	}
	if delays[3] <= delays[2] {
		t.Errorf("the run did not grow the interval: %v", delays)
	}
	if last := delays[len(delays)-1]; last > NonTargetDelayCapDefault {
		t.Errorf("delay %s exceeded the default cap %s", last, NonTargetDelayCapDefault)
	}

	// And a hit ends it: the pair goes back to the base cadence immediately.
	h.scheduler.SetProbeFunc(func(context.Context, string, string) Result {
		return Result{Outcome: OutcomeSuccessTarget, StateValue: "target", StateLength: 292}
	})
	at := h.clock.Now()
	h.scheduler.Scan(ctx)
	next := awaitScheduled(7, at)
	// The escalation no longer applies: the interval is the TTL-derived one,
	// which for the harness's one-hour TTL and 15% threshold is 51 minutes.
	if got := next.Sub(at); got != 51*time.Minute {
		t.Errorf("after a hit the interval is %s, want the TTL-derived 51m", got)
	}
}

// ---------------------------------------------------------------------------
// account freshness

// TestScan_SyncsAccountsOutsideTheProbeWindow is the regression test for the
// bug that left an account stuck at 已暂停探测.
//
// The account sync used to sit behind the master-switch and time-window gates,
// so with probing switched off -- or merely outside the configured hours -- CPA
// was never asked about its accounts again. An operator who fixed an account in
// CPA and came back to a panel still showing the old status had no way to tell
// a stale cache from a broken plugin.
func TestScan_SyncsAccountsOutsideTheProbeWindow(t *testing.T) {
	t.Run("probing switched off", func(t *testing.T) {
		h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})
		if _, err := h.settings.Update(context.Background(),
			settings.Patch{GlobalProbeEnabled: boolPtr(false)}); err != nil {
			t.Fatalf("disable probing: %v", err)
		}

		h.scheduler.Scan(context.Background())

		if got := h.pairs.syncCount(); got != 1 {
			t.Errorf("account syncs = %d, want 1 even with probing off", got)
		}
	})

	t.Run("outside the time window", func(t *testing.T) {
		h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})
		// 12:00 is outside 08:00-09:00.
		if err := h.source.windows.Upsert(context.Background(), TimeWindow{
			ID: "w1", Label: "早间", StartTime: "08:00", EndTime: "09:00", Enabled: true,
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		h.scheduler.Scan(context.Background())

		// ShouldProbeNow is the "may we probe" answer, so false is the case
		// this test is about.
		if h.source.windows.ShouldProbeNow(h.clock.Now()) {
			t.Fatal("the harness is not actually outside the window")
		}
		if got := h.pairs.syncCount(); got != 1 {
			t.Errorf("account syncs = %d, want 1 even outside the window", got)
		}
		if got := h.probeCount(); got != 0 {
			t.Errorf("probes = %d, want 0 outside the window", got)
		}
	})
}

// TestScan_RefreshesEachAccountOncePerScan pins the cost of the per-probe
// freshness check.
//
// Several models share an account, and asking CPA the same question once per
// model would multiply the host calls by the size of the model list for an
// answer that is identical.
func TestScan_RefreshesEachAccountOncePerScan(t *testing.T) {
	h := newSchedHarness(t, []states.Pair{
		{AuthIndex: "a", Model: "m1"},
		{AuthIndex: "a", Model: "m2"},
		{AuthIndex: "a", Model: "m3"},
		{AuthIndex: "b", Model: "m1"},
	})
	// Room for all four at once. The default concurrency is 2, and a scan that
	// runs out of slots stops rather than queueing.
	if _, err := h.settings.Update(context.Background(),
		settings.Patch{ProbeConcurrency: intPtr(8)}); err != nil {
		t.Fatalf("raise concurrency: %v", err)
	}

	h.scheduler.Scan(context.Background())
	if !waitFor(t, func() bool { return h.probeCount() == 4 }) {
		t.Fatalf("probes = %d, want 4", h.probeCount())
	}

	if got := h.pairs.accountSyncCount(); got != 2 {
		t.Errorf("per-account refreshes = %d, want 2 (one per account, not per pair)", got)
	}
}

// TestScan_DropsPairsWhoseAccountIsGone covers the removal path: CPA answers,
// and the account is not in the answer.
func TestScan_DropsPairsWhoseAccountIsGone(t *testing.T) {
	h := newSchedHarness(t, []states.Pair{
		{AuthIndex: "a", Model: "m"},
		{AuthIndex: "gone", Model: "m"},
	})
	h.pairs.removed = []string{"gone"}

	h.scheduler.Scan(context.Background())
	if !waitFor(t, func() bool { return h.probeCount() == 1 }) {
		t.Fatalf("probes = %d, want 1", h.probeCount())
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.probes {
		if p.AuthIndex == "gone" {
			t.Error("probed an account CPA no longer lists")
		}
	}
}

// TestScan_KeepsProbingWhenTheRefreshFails pins the distinction the whole
// removal path rests on: "could not ask" is not "gone".
//
// A transient host error must not tear down an account's bindings, because
// doing so is unrecoverable from the plugin's side -- the bindings are deleted,
// and the next probe that would rebuild them is the one that was just skipped.
func TestScan_KeepsProbingWhenTheRefreshFails(t *testing.T) {
	h := newSchedHarness(t, []states.Pair{{AuthIndex: "a", Model: "m"}})
	h.pairs.syncErr = context.DeadlineExceeded

	h.scheduler.Scan(context.Background())
	if !waitFor(t, func() bool { return h.probeCount() == 1 }) {
		t.Fatalf("probes = %d, want 1: a failed refresh must not stop probing", h.probeCount())
	}
}

func boolPtr(b bool) *bool { return &b }

func intPtr(n int) *int { return &n }
