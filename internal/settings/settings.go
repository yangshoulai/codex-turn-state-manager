// Package settings owns the plugin's global configuration and exposes it as
// an immutable runtime snapshot.
//
// Every runtime capability reads one atomic snapshot per request (NF-10), so
// flipping a switch in the panel takes effect on the very next request with no
// locks on the hot path.
package settings

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Setting keys as persisted in the settings table.
const (
	KeyGlobalEnabled          = "global_enabled"
	KeyGlobalProbeEnabled     = "global_probe_enabled"
	KeyGlobalReverseBind      = "global_reverse_bind_enabled"
	KeyStatePriorityEnabled   = "state_priority_enabled"
	KeyScanIntervalSec        = "scan_interval_sec"
	KeyProbeConcurrency       = "probe_concurrency"
	KeyStateTTLMin            = "state_ttl_min"
	KeyRefreshThresholdPct    = "refresh_threshold_pct"
	KeyTargetStateLength      = "target_state_length"
	KeyMaxProbeDurationSec    = "max_probe_duration_sec"
	KeyAccountRoutingStrategy = "account_routing_strategy"
	KeyAccountSyncSec         = "account_sync_interval_sec"
	KeyProbeRetentionHours    = "probe_history_retention_hours"
	KeyMaxProxiesPerProbe     = "max_proxies_per_probe"
	KeyNonTargetBackoffCapMin = "non_target_backoff_cap_min"
	KeyCallHistoryRetentionH  = "call_history_retention_hours"
)

// RoutingStrategy selects how the credential scheduler picks among candidates.
type RoutingStrategy string

const (
	// StrategyRespectCPAPriority takes the highest CPA priority bucket first,
	// then round-robins inside it. This is the default.
	StrategyRespectCPAPriority RoutingStrategy = "respect_cpa_priority"
	// StrategyStateFirst prefers accounts that hold a valid state over the
	// host's priority ordering. It only has an effect while
	// state_priority_enabled is on, and it requires the plugin to have
	// declared SchedulerAcrossPriorities at registration -- otherwise the host
	// only offers the top priority tier and there is nothing to reach across.
	StrategyStateFirst RoutingStrategy = "state_first"
)

// ParseRoutingStrategy validates a strategy name.
func ParseRoutingStrategy(s string) (RoutingStrategy, error) {
	switch RoutingStrategy(s) {
	case StrategyRespectCPAPriority, StrategyStateFirst:
		return RoutingStrategy(s), nil
	case "":
		return StrategyRespectCPAPriority, nil
	default:
		return "", fmt.Errorf("settings: unknown routing strategy %q", s)
	}
}

// Values is the immutable snapshot the hot path reads.
type Values struct {
	GlobalEnabled            bool
	GlobalProbeEnabled       bool
	GlobalReverseBindEnabled bool
	// StatePriorityEnabled gates account-selection interference. Steering the
	// routing decision is the most invasive thing the plugin does -- it
	// overrides the host's own load balancing -- so it carries its own switch
	// in addition to the master switch. Off means always defer to the host.
	StatePriorityEnabled bool
	ScanInterval         time.Duration
	ProbeConcurrency     int
	StateTTL             time.Duration
	RefreshThresholdPct  int
	TargetStateLength    int
	MaxProbeDuration     time.Duration
	RoutingStrategy      RoutingStrategy
	AccountSyncInterval  time.Duration
	// ProbeRetention bounds how long a probe_history row is kept. Probe
	// history is operational telemetry, not an audit log: the panel reads the
	// last day and older rows are cost without benefit.
	ProbeRetention time.Duration

	// MaxProxiesPerProbe caps how many nodes one probe may try before giving
	// up for this round. A pool deep enough to walk in full can spend a long
	// time failing; the cap bounds the cost of a round without changing which
	// nodes are eligible.
	MaxProxiesPerProbe int

	// NonTargetBackoffCap bounds how long a pair that keeps answering with the
	// wrong shape waits between rounds. The interval grows from the base delay
	// towards this; without a cap the pair would eventually stop being probed
	// at all, and the upstream may start yielding the right shape at any time.
	NonTargetBackoffCap time.Duration

	// CallHistoryRetention bounds how long one intercepted request is kept.
	//
	// Capped at a day on purpose. A call row carries three full turn-state
	// values, so the table grows with traffic rather than with time, and the
	// question it answers -- "what did this request carry and what came back" --
	// is about the last few minutes. A week of rows would be a slower panel and
	// a bigger secret surface for nobody's benefit.
	CallHistoryRetention time.Duration
}

// Defaults returns the documented default configuration (design doc 3.3/3.5).
func Defaults() Values {
	return Values{
		GlobalEnabled:            true,
		GlobalProbeEnabled:       true,
		GlobalReverseBindEnabled: true,
		StatePriorityEnabled:     true,
		ScanInterval:             60 * time.Second,
		ProbeConcurrency:         2,
		StateTTL:                 60 * time.Minute,
		RefreshThresholdPct:      15,
		TargetStateLength:        292,
		MaxProbeDuration:         90 * time.Second,
		RoutingStrategy:          StrategyRespectCPAPriority,
		AccountSyncInterval:      5 * time.Minute,
		ProbeRetention:           24 * time.Hour,
		MaxProxiesPerProbe:       10,
		NonTargetBackoffCap:      30 * time.Minute,
		CallHistoryRetention:     24 * time.Hour,
	}
}

// Bounds enforced on every write so a bad panel value cannot wedge the plugin.
const (
	MinProbeConcurrency  = 1
	MaxProbeConcurrency  = 32
	MinScanInterval      = 5 * time.Second
	MinStateTTL          = time.Minute
	MinTargetStateLength = 1
	MaxTargetStateLength = 8192
	MinMaxProbeDuration  = 5 * time.Second
	MinProbeRetention    = time.Hour
	MaxProbeRetention    = 365 * 24 * time.Hour
	MinProxiesPerProbe   = 1
	MaxProxiesPerProbe   = 100
	// Bounded so an operator cannot effectively disable probing for a pair, nor
	// reintroduce the every-few-minutes retry the escalation exists to stop.
	MinNonTargetBackoffCap = 5 * time.Minute
	MaxNonTargetBackoffCap = 24 * time.Hour
	// Call history is bounded by design: see Values.CallHistoryRetention.
	MinCallHistoryRetention = time.Hour
	MaxCallHistoryRetention = 24 * time.Hour
)

// Store persists settings. Implemented by storage.SettingsStore.
type Store interface {
	All(ctx context.Context) (map[string]string, error)
	PutMany(ctx context.Context, values map[string]string) error
}

// Manager owns the live configuration.
type Manager struct {
	store Store

	// snap is read on every intercepted request; writes build a fresh Values
	// and swap the pointer so readers never see a torn struct.
	snap atomic.Pointer[Values]

	mu sync.Mutex // serialises writers
}

// NewManager loads persisted settings, layering them over Defaults, and
// publishes the first snapshot.
//
// Only a store failure is fatal. Unreadable or out-of-range persisted values
// degrade to defaults and are reported through warn, because refusing to boot
// over one bad row would take the whole plugin down (NF-04).
func NewManager(ctx context.Context, store Store, warn func(string, ...any)) (*Manager, error) {
	m := &Manager{store: store}
	defaults := Defaults()
	m.snap.Store(&defaults)

	raw, err := store.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("settings: load: %w", err)
	}
	v, warnings := decode(raw, defaults)
	for _, w := range warnings {
		if warn != nil {
			warn("settings: %s", w)
		}
	}
	m.snap.Store(&v)
	return m, nil
}

// Current returns the live snapshot. Callers must treat it as read-only.
func (m *Manager) Current() *Values { return m.snap.Load() }

// Capabilities is the resolved view of which runtime behaviours are active.
// It exists so the four call sites share one interpretation of the switches
// rather than each re-deriving them.
type Capabilities struct {
	// Enabled is the master switch. When false every other field is false.
	Enabled bool
	// Probe: the scheduler may run active probes.
	Probe bool
	// Inject: outbound requests may have X-Codex-Turn-State rewritten.
	Inject bool
	// Capture: response headers may be harvested for new state values.
	Capture bool
	// Route: the scheduler may override CPA's account choice. This requires the
	// state-priority switch on top of the master switch.
	Route bool
}

// Capabilities resolves the switch matrix described in design doc 3.3.
//
// The master switch gates all four. When it is off, bound state is neither
// injected nor refreshed, and the scheduler defers to CPA.
func (v *Values) Capabilities() Capabilities {
	if !v.GlobalEnabled {
		return Capabilities{}
	}
	return Capabilities{
		Enabled: true,
		// Probe, Capture and Route each additionally honour their own switch.
		Probe:   v.GlobalProbeEnabled,
		Capture: v.GlobalReverseBindEnabled,
		Route:   v.StatePriorityEnabled,
		// Injection is master-switch-only: with every sub-switch off the plugin
		// still serves state it already holds. Only the master switch stops it.
		Inject: true,
	}
}

// Patch is a partial settings update. Nil fields are left unchanged.
type Patch struct {
	GlobalEnabled            *bool
	GlobalProbeEnabled       *bool
	GlobalReverseBindEnabled *bool
	StatePriorityEnabled     *bool
	ScanInterval             *time.Duration
	ProbeConcurrency         *int
	StateTTL                 *time.Duration
	RefreshThresholdPct      *int
	TargetStateLength        *int
	MaxProbeDuration         *time.Duration
	RoutingStrategy          *RoutingStrategy
	AccountSyncInterval      *time.Duration
	ProbeRetention           *time.Duration
	MaxProxiesPerProbe       *int
	NonTargetBackoffCapMin   *int
	CallHistoryRetention     *time.Duration
}

// Update applies a patch, validates it, persists it and publishes a new
// snapshot. Validation happens before persistence so a rejected patch leaves
// no trace in the database.
func (m *Manager) Update(ctx context.Context, p Patch) (*Values, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := *m.snap.Load()
	if p.GlobalEnabled != nil {
		next.GlobalEnabled = *p.GlobalEnabled
	}
	if p.GlobalProbeEnabled != nil {
		next.GlobalProbeEnabled = *p.GlobalProbeEnabled
	}
	if p.GlobalReverseBindEnabled != nil {
		next.GlobalReverseBindEnabled = *p.GlobalReverseBindEnabled
	}
	if p.StatePriorityEnabled != nil {
		next.StatePriorityEnabled = *p.StatePriorityEnabled
	}
	if p.ScanInterval != nil {
		next.ScanInterval = *p.ScanInterval
	}
	if p.ProbeConcurrency != nil {
		next.ProbeConcurrency = *p.ProbeConcurrency
	}
	if p.StateTTL != nil {
		next.StateTTL = *p.StateTTL
	}
	if p.RefreshThresholdPct != nil {
		next.RefreshThresholdPct = *p.RefreshThresholdPct
	}
	if p.TargetStateLength != nil {
		next.TargetStateLength = *p.TargetStateLength
	}
	if p.MaxProbeDuration != nil {
		next.MaxProbeDuration = *p.MaxProbeDuration
	}
	if p.RoutingStrategy != nil {
		next.RoutingStrategy = *p.RoutingStrategy
	}
	if p.AccountSyncInterval != nil {
		next.AccountSyncInterval = *p.AccountSyncInterval
	}
	if p.ProbeRetention != nil {
		next.ProbeRetention = *p.ProbeRetention
	}
	if p.MaxProxiesPerProbe != nil {
		next.MaxProxiesPerProbe = *p.MaxProxiesPerProbe
	}
	if p.NonTargetBackoffCapMin != nil {
		next.NonTargetBackoffCap = time.Duration(*p.NonTargetBackoffCapMin) * time.Minute
	}
	if p.CallHistoryRetention != nil {
		next.CallHistoryRetention = *p.CallHistoryRetention
	}

	if err := next.Validate(); err != nil {
		return nil, err
	}

	encoded, err := encode(next)
	if err != nil {
		return nil, err
	}
	if err := m.store.PutMany(ctx, encoded); err != nil {
		return nil, fmt.Errorf("settings: persist: %w", err)
	}

	m.snap.Store(&next)
	out := next
	return &out, nil
}

// Validate checks the snapshot against the documented bounds.
func (v *Values) Validate() error {
	if v.ProbeConcurrency < MinProbeConcurrency || v.ProbeConcurrency > MaxProbeConcurrency {
		return fmt.Errorf("settings: probe_concurrency must be between %d and %d, got %d",
			MinProbeConcurrency, MaxProbeConcurrency, v.ProbeConcurrency)
	}
	if v.ScanInterval < MinScanInterval {
		return fmt.Errorf("settings: scan_interval must be at least %s, got %s", MinScanInterval, v.ScanInterval)
	}
	if v.StateTTL < MinStateTTL {
		return fmt.Errorf("settings: state_ttl must be at least %s, got %s", MinStateTTL, v.StateTTL)
	}
	if v.RefreshThresholdPct < 0 || v.RefreshThresholdPct > 90 {
		return fmt.Errorf("settings: refresh_threshold_pct must be between 0 and 90, got %d", v.RefreshThresholdPct)
	}
	if v.TargetStateLength < MinTargetStateLength || v.TargetStateLength > MaxTargetStateLength {
		return fmt.Errorf("settings: target_state_length must be between %d and %d, got %d",
			MinTargetStateLength, MaxTargetStateLength, v.TargetStateLength)
	}
	if v.ProbeRetention < MinProbeRetention || v.ProbeRetention > MaxProbeRetention {
		return fmt.Errorf("settings: probe_history_retention_hours must be between %s and %s, got %s",
			MinProbeRetention, MaxProbeRetention, v.ProbeRetention)
	}
	if v.MaxProxiesPerProbe < MinProxiesPerProbe || v.MaxProxiesPerProbe > MaxProxiesPerProbe {
		return fmt.Errorf("settings: max_proxies_per_probe must be between %d and %d, got %d",
			MinProxiesPerProbe, MaxProxiesPerProbe, v.MaxProxiesPerProbe)
	}
	if v.NonTargetBackoffCap < MinNonTargetBackoffCap || v.NonTargetBackoffCap > MaxNonTargetBackoffCap {
		return fmt.Errorf("settings: non_target_backoff_cap_min must be between %s and %s, got %s",
			MinNonTargetBackoffCap, MaxNonTargetBackoffCap, v.NonTargetBackoffCap)
	}
	if v.CallHistoryRetention < MinCallHistoryRetention || v.CallHistoryRetention > MaxCallHistoryRetention {
		return fmt.Errorf("settings: call_history_retention_hours must be between %s and %s, got %s",
			MinCallHistoryRetention, MaxCallHistoryRetention, v.CallHistoryRetention)
	}
	if v.MaxProbeDuration < MinMaxProbeDuration {
		return fmt.Errorf("settings: max_probe_duration must be at least %s, got %s", MinMaxProbeDuration, v.MaxProbeDuration)
	}
	switch v.RoutingStrategy {
	case StrategyRespectCPAPriority, StrategyStateFirst:
	default:
		return fmt.Errorf("settings: unknown routing strategy %q", v.RoutingStrategy)
	}
	return nil
}

func encode(v Values) (map[string]string, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return map[string]string{
		KeyGlobalEnabled:          strconv.FormatBool(v.GlobalEnabled),
		KeyGlobalProbeEnabled:     strconv.FormatBool(v.GlobalProbeEnabled),
		KeyGlobalReverseBind:      strconv.FormatBool(v.GlobalReverseBindEnabled),
		KeyStatePriorityEnabled:   strconv.FormatBool(v.StatePriorityEnabled),
		KeyScanIntervalSec:        strconv.Itoa(int(v.ScanInterval / time.Second)),
		KeyProbeConcurrency:       strconv.Itoa(v.ProbeConcurrency),
		KeyStateTTLMin:            strconv.Itoa(int(v.StateTTL / time.Minute)),
		KeyRefreshThresholdPct:    strconv.Itoa(v.RefreshThresholdPct),
		KeyTargetStateLength:      strconv.Itoa(v.TargetStateLength),
		KeyMaxProbeDurationSec:    strconv.Itoa(int(v.MaxProbeDuration / time.Second)),
		KeyAccountRoutingStrategy: string(v.RoutingStrategy),
		KeyAccountSyncSec:         strconv.Itoa(int(v.AccountSyncInterval / time.Second)),
		KeyProbeRetentionHours:    strconv.Itoa(int(v.ProbeRetention / time.Hour)),
		KeyMaxProxiesPerProbe:     strconv.Itoa(v.MaxProxiesPerProbe),
		KeyNonTargetBackoffCapMin: strconv.Itoa(int(v.NonTargetBackoffCap / time.Minute)),
		KeyCallHistoryRetentionH:  strconv.Itoa(int(v.CallHistoryRetention / time.Hour)),
	}, nil
}

// decode layers persisted values over defaults. Unparseable entries fall back
// to the default and are reported, so a hand-edited database degrades instead
// of refusing to boot.
func decode(raw map[string]string, base Values) (Values, []string) {
	v := base
	var problems []string

	boolAt := func(key string, dst *bool) {
		s, ok := raw[key]
		if !ok {
			return
		}
		b, err := strconv.ParseBool(strings.TrimSpace(s))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s=%q", key, s))
			return
		}
		*dst = b
	}
	intAt := func(key string, dst *int) {
		s, ok := raw[key]
		if !ok {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s=%q", key, s))
			return
		}
		*dst = n
	}
	secsAt := func(key string, dst *time.Duration) {
		s, ok := raw[key]
		if !ok {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			problems = append(problems, fmt.Sprintf("%s=%q", key, s))
			return
		}
		*dst = time.Duration(n) * time.Second
	}
	hoursAt := func(key string, dst *time.Duration) {
		s, ok := raw[key]
		if !ok {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			problems = append(problems, fmt.Sprintf("%s=%q", key, s))
			return
		}
		*dst = time.Duration(n) * time.Hour
	}
	minsAt := func(key string, dst *time.Duration) {
		s, ok := raw[key]
		if !ok {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			problems = append(problems, fmt.Sprintf("%s=%q", key, s))
			return
		}
		*dst = time.Duration(n) * time.Minute
	}

	boolAt(KeyGlobalEnabled, &v.GlobalEnabled)
	boolAt(KeyGlobalProbeEnabled, &v.GlobalProbeEnabled)
	boolAt(KeyGlobalReverseBind, &v.GlobalReverseBindEnabled)
	boolAt(KeyStatePriorityEnabled, &v.StatePriorityEnabled)
	secsAt(KeyScanIntervalSec, &v.ScanInterval)
	intAt(KeyProbeConcurrency, &v.ProbeConcurrency)
	minsAt(KeyStateTTLMin, &v.StateTTL)
	intAt(KeyRefreshThresholdPct, &v.RefreshThresholdPct)
	intAt(KeyTargetStateLength, &v.TargetStateLength)
	secsAt(KeyMaxProbeDurationSec, &v.MaxProbeDuration)
	secsAt(KeyAccountSyncSec, &v.AccountSyncInterval)
	hoursAt(KeyProbeRetentionHours, &v.ProbeRetention)
	intAt(KeyMaxProxiesPerProbe, &v.MaxProxiesPerProbe)
	minsAt(KeyNonTargetBackoffCapMin, &v.NonTargetBackoffCap)
	hoursAt(KeyCallHistoryRetentionH, &v.CallHistoryRetention)

	if s, ok := raw[KeyAccountRoutingStrategy]; ok && strings.TrimSpace(s) != "" {
		strategy, err := ParseRoutingStrategy(strings.TrimSpace(s))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s=%q", KeyAccountRoutingStrategy, s))
		} else {
			v.RoutingStrategy = strategy
		}
	}

	if err := v.Validate(); err != nil {
		// A persisted snapshot can only become invalid through manual edits or
		// a downgrade. Report it and keep running on defaults.
		return base, append(problems, fmt.Sprintf(
			"persisted configuration is invalid (%v); running on defaults", err))
	}
	return v, problems
}
