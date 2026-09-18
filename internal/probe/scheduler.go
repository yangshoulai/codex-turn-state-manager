package probe

import (
	"context"
	"sync"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// Pair is the unit of probing.
type Pair = states.Pair

// ConfigSource is everything the scheduler reads from the app.
//
// The accessor names are deliberately specific rather than the obvious
// Settings()/Accounts()/... because the composition root already exposes those
// names with different return types for the Management API.
type ConfigSource interface {
	SettingsManager() *settings.Manager
	EnabledPairSource() EnabledPairSource
	WindowManager() *WindowManager
	StateRegistry() *states.Registry
	ProbeExecutor() *Executor
}

// EnabledPairSource yields the pairs an operator has switched probing on.
//
// It returns states.Pair rather than accounts.Config so this package does not
// depend on the accounts package; the app adapts between them.
type EnabledPairSource interface {
	EnabledPairs() []states.Pair
	Sync(ctx context.Context) (int, error)
}

// pairState is the scheduler's per-pair bookkeeping.
type pairState struct {
	nextProbeAt time.Time
	inFlight    bool

	// nonTargetStreak counts consecutive probes that returned a state of the
	// wrong length. Such a pair is re-probed every few minutes by design, but a
	// long streak means the model simply does not yield a usable token, and
	// polling it forever spends quota to learn nothing. Surfaced so an operator
	// can turn it off; never acted on automatically, because the design
	// document only permits auto-disabling on MODEL_UNSUPPORTED.
	nonTargetStreak int
}

// Scheduler runs the periodic scan and owns the probe concurrency limit.
//
// Scanning is not probing (design doc 3.5): a scan visits every enabled pair
// once per tick and only enqueues the ones that are actually due.
type Scheduler struct {
	src    ConfigSource
	logf   func(hostapi.LogLevel, string, map[string]any)
	now    func() time.Time
	jitter func(time.Duration) time.Duration

	mu    sync.Mutex
	pairs map[states.Pair]*pairState
	// lastSync is when accounts were last pulled from CPA; zero means never.
	lastSync time.Time

	// sem is the probe concurrency limit. It is re-sized when the configured
	// value changes, which is why it is rebuilt rather than fixed at start.
	semMu   sync.Mutex
	sem     chan struct{}
	semSize int

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup

	// probeFn is injectable so tests can drive the state machine without
	// network access.
	probeFn func(ctx context.Context, authIndex, model string) Result
	// bindFn is injectable for the same reason.
	bindFn func(ctx context.Context, authIndex, model, value string, proxyID string) error
}

// SchedulerConfig configures a new Scheduler.
type SchedulerConfig struct {
	Source ConfigSource
	Log    func(hostapi.LogLevel, string, map[string]any)
}

// NewScheduler builds a scheduler.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	return &Scheduler{
		src:    cfg.Source,
		logf:   cfg.Log,
		now:    time.Now,
		pairs:  map[states.Pair]*pairState{},
		stop:   make(chan struct{}),
		jitter: func(n time.Duration) time.Duration { return time.Duration(time.Now().UnixNano() % int64(max(n, 1))) }}
}

// SetClock overrides the time source. Tests only.
func (s *Scheduler) SetClock(now func() time.Time) { s.now = now }

// SetJitter overrides the jitter source. Tests only.
func (s *Scheduler) SetJitter(f func(time.Duration) time.Duration) { s.jitter = f }

// SetProbeFunc overrides the probe implementation. Tests only.
func (s *Scheduler) SetProbeFunc(f func(ctx context.Context, authIndex, model string) Result) {
	s.probeFn = f
}

// SetBindFunc overrides the binding write. Tests only.
func (s *Scheduler) SetBindFunc(f func(ctx context.Context, authIndex, model, value, proxyID string) error) {
	s.bindFn = f
}

// Start launches the scan loop. It returns immediately.
func (s *Scheduler) Start(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(ctx)
	}()
}

// Stop ends the scan loop and waits for it to unwind. In-flight probes finish
// naturally; their results are discarded by the settings re-check in runProbe.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

func (s *Scheduler) loop(ctx context.Context) {
	// Spread the first probes of a fresh install so 50 unbound pairs do not
	// fire at once (NF-02).
	s.seedJitter()

	for {
		interval := s.src.SettingsManager().Current().ScanInterval
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.stop:
			timer.Stop()
			return
		case <-timer.C:
			s.Scan(ctx)
		}
	}
}

// Scan runs one scheduling pass.
func (s *Scheduler) Scan(ctx context.Context) {
	caps := s.src.SettingsManager().Current().Capabilities()
	if !caps.Probe {
		// Master switch off, or the probe sub-switch off. Nothing to do.
		return
	}
	if !s.src.WindowManager().ShouldProbeNow(s.now()) {
		return
	}

	now := s.now()
	settingsNow := s.src.SettingsManager().Current()

	// Account sync runs on its own, slower cadence than the scan: it hits the
	// CPA host API, and the account list changes far less often than the probe
	// schedule does.
	if now.Sub(s.lastSyncAt()) >= settingsNow.AccountSyncInterval {
		if n, err := s.src.EnabledPairSource().Sync(ctx); err != nil {
			s.log(hostapi.LogWarn, "account sync failed", map[string]any{"error": err.Error()})
		} else {
			s.log(hostapi.LogDebug, "account sync complete", map[string]any{"accounts": n})
			s.markSynced(now)
		}
	}

	for _, p := range s.src.EnabledPairSource().EnabledPairs() {
		if !s.due(p, now) {
			continue
		}

		// A pair inside its refresh window is due; a fresh binding is not.
		if _, status := s.src.StateRegistry().Lookup(p.AuthIndex, p.Model); status == states.StatusFresh {
			continue
		}

		if !s.acquire(settingsNow.ProbeConcurrency) {
			// Every slot is busy; the next tick will pick this pair up.
			return
		}
		s.markInFlight(p, true)
		s.wg.Add(1)
		go func(p states.Pair) {
			defer s.wg.Done()
			defer s.release()
			defer s.markInFlight(p, false)
			s.runProbe(ctx, p)
		}(p)
	}
}

// due reports whether a pair's next_probe_at has arrived. A pair with no
// bookkeeping yet is due immediately.
func (s *Scheduler) due(p states.Pair, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.pairs[p]
	if !ok {
		return true
	}
	if st.inFlight {
		return false
	}
	return !now.Before(st.nextProbeAt)
}

// lastSyncAt returns when accounts were last synced from CPA.
func (s *Scheduler) lastSyncAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSync
}

// markSynced records a successful account sync.
func (s *Scheduler) markSynced(at time.Time) {
	s.mu.Lock()
	s.lastSync = at
	s.mu.Unlock()
}

func (s *Scheduler) markInFlight(p states.Pair, inFlight bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.pairs[p]
	if !ok {
		st = &pairState{}
		s.pairs[p] = st
	}
	st.inFlight = inFlight
}

// schedule sets the next probe time for a pair.
func (s *Scheduler) schedule(p states.Pair, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.pairs[p]
	if !ok {
		st = &pairState{}
		s.pairs[p] = st
	}
	st.nextProbeAt = at
}

// ProbeNow clears a pair's backoff so the next scan picks it up. Used after a
// manual binding delete (design doc 5.4).
func (s *Scheduler) ProbeNow(p states.Pair) {
	s.schedule(p, time.Time{})
}

// runProbe executes one probe and applies its result.
func (s *Scheduler) runProbe(ctx context.Context, p states.Pair) {
	result := s.doProbe(ctx, p)

	// Re-check the switch after the probe returns: a probe that started before
	// the master switch was turned off must not write a binding (design doc
	// 3.3).
	if !s.src.SettingsManager().Current().Capabilities().Probe {
		s.log(hostapi.LogInfo, "discarding probe result: probing disabled", map[string]any{
			"authIndex": p.AuthIndex, "model": p.Model, "outcome": string(result.Outcome),
		})
		return
	}

	now := s.now()
	values := s.src.SettingsManager().Current()
	s.recordStreak(p, result.Outcome)

	if result.Succeeded() {
		policy := values.Capabilities()
		if !policy.Inject {
			// Master switch off: never bind.
			return
		}
		if err := s.doBind(ctx, p, result.StateValue, result.ProxyID); err != nil {
			s.log(hostapi.LogError, "could not bind state", map[string]any{
				"authIndex": p.AuthIndex, "model": p.Model, "error": err.Error(),
			})
		} else {
			s.log(hostapi.LogInfo, "state bound", map[string]any{
				"authIndex": p.AuthIndex, "model": p.Model,
				"proxyId": result.ProxyID, "length": result.StateLength,
			})
		}
	}

	backoff := Backoff{
		TTL:                 values.StateTTL,
		RefreshThresholdPct: values.RefreshThresholdPct,
		Jitter:              s.jitter,
	}
	if result.Err != nil {
		s.log(hostapi.LogDebug, "probe finished", map[string]any{
			"authIndex": p.AuthIndex, "model": p.Model,
			"outcome": string(result.Outcome), "error": result.Err.Error(),
			"proxiesTried": result.ProxiesTried,
		})
	}
	s.schedule(p, now.Add(backoff.NextDelay(result.Outcome)))
}

func (s *Scheduler) doProbe(ctx context.Context, p states.Pair) Result {
	if s.probeFn != nil {
		return s.probeFn(ctx, p.AuthIndex, p.Model)
	}
	return s.src.ProbeExecutor().Probe(ctx, p.AuthIndex, p.Model)
}

func (s *Scheduler) doBind(ctx context.Context, p states.Pair, value, proxyID string) error {
	if s.bindFn != nil {
		return s.bindFn(ctx, p.AuthIndex, p.Model, value, proxyID)
	}
	_, err := s.src.StateRegistry().Bind(ctx, states.Binding{
		Pair:       p,
		StateValue: value,
		Source:     states.SourceProbe,
		ProxyID:    proxyID,
	})
	return err
}

// MaxInitialSpread caps how long the first probes of a fresh install are
// staggered over.
const MaxInitialSpread = 10 * time.Minute

// seedJitter staggers the initial next_probe_at of every known pair.
//
// The window is deliberately much shorter than the state TTL: spreading the
// first probes across a full TTL would leave a fresh install with no bound
// state for up to an hour. One scan interval per pair, capped, avoids a
// thundering herd while still acquiring state promptly (NF-02).
func (s *Scheduler) seedJitter() {
	pairs := s.src.EnabledPairSource().EnabledPairs()
	if len(pairs) == 0 {
		return
	}

	scanInterval := s.src.SettingsManager().Current().ScanInterval
	if scanInterval <= 0 {
		scanInterval = time.Minute
	}
	spread := scanInterval * time.Duration(len(pairs))
	if spread > MaxInitialSpread {
		spread = MaxInitialSpread
	}

	step := spread / time.Duration(len(pairs))
	now := s.now()
	for i, p := range pairs {
		offset := time.Duration(i)*step + s.jitter(step)
		s.schedule(p, now.Add(offset))
	}
}

// acquire takes a probe slot, resizing the semaphore if the configured
// concurrency changed. It returns false when no slot is free.
func (s *Scheduler) acquire(concurrency int) bool {
	if concurrency < settings.MinProbeConcurrency {
		concurrency = settings.MinProbeConcurrency
	}
	s.semMu.Lock()
	if s.sem == nil || s.semSize != concurrency {
		// Resize without dropping in-flight accounting: carry the number of
		// currently-held slots into the new channel.
		held := 0
		if s.sem != nil {
			held = len(s.sem)
		}
		next := make(chan struct{}, concurrency)
		for i := 0; i < held && i < concurrency; i++ {
			next <- struct{}{}
		}
		s.sem = next
		s.semSize = concurrency
	}
	sem := s.sem
	s.semMu.Unlock()

	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Scheduler) release() {
	s.semMu.Lock()
	sem := s.sem
	s.semMu.Unlock()
	if sem == nil {
		return
	}
	select {
	case <-sem:
	default:
	}
}

// recordStreak updates the consecutive non-target run for a pair.
func (s *Scheduler) recordStreak(p states.Pair, outcome Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.pairs[p]
	if !ok {
		st = &pairState{}
		s.pairs[p] = st
	}
	if outcome == OutcomeSuccessNonTarget {
		st.nonTargetStreak++
		return
	}
	// Anything else -- a hit, a model that cannot serve the request, a network
	// fault -- breaks the run; only an unbroken stretch is evidence.
	st.nonTargetStreak = 0
}

// NonTargetStreak reports how many consecutive probes returned a non-target
// length for a pair.
func (s *Scheduler) NonTargetStreak(p states.Pair) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.pairs[p]; ok {
		return st.nonTargetStreak
	}
	return 0
}

// NextProbeAt reports the scheduled time for a pair, for the panel.
func (s *Scheduler) NextProbeAt(p states.Pair) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.pairs[p]
	if !ok {
		return time.Time{}, false
	}
	return st.nextProbeAt, true
}

// InFlight reports whether a pair is currently being probed.
func (s *Scheduler) InFlight(p states.Pair) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.pairs[p]
	return ok && st.inFlight
}

func (s *Scheduler) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if s.logf != nil {
		s.logf(level, msg, fields)
	}
}
