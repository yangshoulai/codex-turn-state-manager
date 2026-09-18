package settings

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu     sync.Mutex
	values map[string]string
	failOn map[string]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{values: map[string]string{}, failOn: map[string]bool{}}
}

func (s *fakeStore) All(context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}

func (s *fakeStore) PutMany(_ context.Context, values map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range values {
		s.values[k] = v
	}
	return nil
}

func (s *fakeStore) get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[key]
}

func newManager(t *testing.T) (*Manager, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	m, err := NewManager(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, store
}

// TestCapabilities_SwitchMatrix pins the five combinations in design doc 3.3.
// This is the single definition of the switch semantics; every runtime call
// site derives its behaviour from here.
func TestCapabilities_SwitchMatrix(t *testing.T) {
	cases := []struct {
		name    string
		master  bool
		probe   bool
		reverse bool

		wantProbe   bool
		wantInject  bool
		wantCapture bool
		wantRoute   bool
	}{
		{
			name:   "all on (default): both channels active",
			master: true, probe: true, reverse: true,
			wantProbe: true, wantInject: true, wantCapture: true, wantRoute: true,
		},
		{
			name:   "probe only: traffic capture forbidden",
			master: true, probe: true, reverse: false,
			wantProbe: true, wantInject: true, wantCapture: false, wantRoute: true,
		},
		{
			name:   "reverse bind only: state accumulates from traffic",
			master: true, probe: false, reverse: true,
			wantProbe: false, wantInject: true, wantCapture: true, wantRoute: true,
		},
		{
			name:   "both sub-switches off: held state is still served",
			master: true, probe: false, reverse: false,
			wantProbe: false, wantInject: true, wantCapture: false, wantRoute: true,
		},
		{
			name:   "master off: all four capabilities bypassed",
			master: false, probe: true, reverse: true,
			wantProbe: false, wantInject: false, wantCapture: false, wantRoute: false,
		},
		{
			name:   "master off wins over every combination",
			master: false, probe: false, reverse: false,
			wantProbe: false, wantInject: false, wantCapture: false, wantRoute: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Values{
				GlobalEnabled:            tc.master,
				GlobalProbeEnabled:       tc.probe,
				GlobalReverseBindEnabled: tc.reverse,
			}
			got := v.Capabilities()

			if got.Enabled != tc.master {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tc.master)
			}
			if got.Probe != tc.wantProbe {
				t.Errorf("Probe = %v, want %v", got.Probe, tc.wantProbe)
			}
			if got.Inject != tc.wantInject {
				t.Errorf("Inject = %v, want %v", got.Inject, tc.wantInject)
			}
			if got.Capture != tc.wantCapture {
				t.Errorf("Capture = %v, want %v", got.Capture, tc.wantCapture)
			}
			if got.Route != tc.wantRoute {
				t.Errorf("Route = %v, want %v", got.Route, tc.wantRoute)
			}
		})
	}
}

func TestDefaults_MatchDocumentedValues(t *testing.T) {
	d := Defaults()

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"global_enabled", d.GlobalEnabled, true},
		{"global_probe_enabled", d.GlobalProbeEnabled, true},
		{"global_reverse_bind_enabled", d.GlobalReverseBindEnabled, true},
		{"scan_interval", d.ScanInterval, 60 * time.Second},
		{"probe_concurrency", d.ProbeConcurrency, 2},
		{"state_ttl", d.StateTTL, 60 * time.Minute},
		{"refresh_threshold_pct", d.RefreshThresholdPct, 15},
		{"target_state_length", d.TargetStateLength, 292},
		{"max_probe_duration", d.MaxProbeDuration, 90 * time.Second},
		{"routing_strategy", d.RoutingStrategy, StrategyRespectCPAPriority},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("default %s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if err := d.Validate(); err != nil {
		t.Errorf("defaults must be valid: %v", err)
	}
}

func TestManager_UpdatePersistsAndPublishes(t *testing.T) {
	ctx := context.Background()
	m, store := newManager(t)

	off := false
	concurrency := 4
	maxDuration := 120 * time.Second

	updated, err := m.Update(ctx, Patch{
		GlobalProbeEnabled: &off,
		ProbeConcurrency:   &concurrency,
		MaxProbeDuration:   &maxDuration,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.ProbeConcurrency != 4 {
		t.Errorf("returned concurrency = %d, want 4", updated.ProbeConcurrency)
	}
	if m.Current().ProbeConcurrency != 4 {
		t.Error("snapshot was not updated")
	}

	// Persisted, so a restart restores it (NF-04).
	if got := store.get(KeyProbeConcurrency); got != "4" {
		t.Errorf("persisted probe_concurrency = %q, want 4", got)
	}
	if got := store.get(KeyGlobalProbeEnabled); got != "false" {
		t.Errorf("persisted global_probe_enabled = %q, want false", got)
	}
	if got := store.get(KeyMaxProbeDurationSec); got != "120" {
		t.Errorf("persisted max_probe_duration_sec = %q, want 120", got)
	}
}

func TestManager_UpdateLeavesOmittedFieldsAlone(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)

	before := *m.Current()
	strategy := StrategyStateFirst
	if _, err := m.Update(ctx, Patch{RoutingStrategy: &strategy}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after := *m.Current()

	if after.RoutingStrategy != StrategyStateFirst {
		t.Errorf("RoutingStrategy = %s, want state_first", after.RoutingStrategy)
	}
	if after.ProbeConcurrency != before.ProbeConcurrency ||
		after.StateTTL != before.StateTTL ||
		after.TargetStateLength != before.TargetStateLength {
		t.Error("a partial patch must not reset unrelated fields")
	}
}

func TestManager_RejectsInvalidUpdates(t *testing.T) {
	ctx := context.Background()
	m, store := newManager(t)

	cases := []struct {
		name  string
		patch Patch
	}{
		{"concurrency below the floor", Patch{ProbeConcurrency: intPtr(0)}},
		{"concurrency above the ceiling", Patch{ProbeConcurrency: intPtr(99)}},
		{"scan interval too short", Patch{ScanInterval: durPtr(time.Second)}},
		{"ttl too short", Patch{StateTTL: durPtr(time.Second)}},
		{"threshold above 90", Patch{RefreshThresholdPct: intPtr(95)}},
		{"threshold negative", Patch{RefreshThresholdPct: intPtr(-1)}},
		{"target length zero", Patch{TargetStateLength: intPtr(0)}},
		{"probe duration too short", Patch{MaxProbeDuration: durPtr(time.Second)}},
		{"unknown routing strategy", Patch{RoutingStrategy: strategyPtr("nonsense")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.Update(ctx, tc.patch); err == nil {
				t.Fatal("expected the update to be rejected")
			}
		})
	}

	// A rejected patch must leave both the live snapshot and the store alone.
	merged := 0
	for _, v := range store.values {
		_ = v
		merged++
	}
	if merged != 0 {
		t.Errorf("store has %d keys after only rejected updates, want 0", merged)
	}
	if m.Current().ProbeConcurrency != Defaults().ProbeConcurrency {
		t.Error("a rejected update changed the live snapshot")
	}
}

func TestNewManager_LayersPersistedOverDefaults(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.values[KeyProbeConcurrency] = "6"
	store.values[KeyTargetStateLength] = "512"
	store.values[KeyGlobalEnabled] = "false"

	m, err := NewManager(ctx, store, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	got := m.Current()

	if got.ProbeConcurrency != 6 {
		t.Errorf("ProbeConcurrency = %d, want 6", got.ProbeConcurrency)
	}
	if got.TargetStateLength != 512 {
		t.Errorf("TargetStateLength = %d, want 512", got.TargetStateLength)
	}
	if got.GlobalEnabled {
		t.Error("GlobalEnabled = true, want false")
	}
	// Untouched keys keep their defaults.
	if got.StateTTL != Defaults().StateTTL {
		t.Errorf("StateTTL = %s, want the default %s", got.StateTTL, Defaults().StateTTL)
	}
}

// TestNewManager_DegradesOnBadValues covers the "hand-edited database" case:
// one unreadable row must not stop the plugin from booting.
func TestNewManager_DegradesOnBadValues(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.values[KeyProbeConcurrency] = "not-a-number"
	store.values[KeyTargetStateLength] = "also-bad"
	store.values[KeyStateTTLMin] = "-5"

	var warnings []string
	m, err := NewManager(ctx, store, func(format string, args ...any) {
		warnings = append(warnings, format)
	})
	if err != nil {
		t.Fatalf("NewManager should degrade rather than fail: %v", err)
	}

	got := m.Current()
	if got.ProbeConcurrency != Defaults().ProbeConcurrency {
		t.Errorf("ProbeConcurrency = %d, want the default %d", got.ProbeConcurrency, Defaults().ProbeConcurrency)
	}
	if got.TargetStateLength != Defaults().TargetStateLength {
		t.Errorf("TargetStateLength = %d, want the default", got.TargetStateLength)
	}
	if len(warnings) == 0 {
		t.Error("unreadable values should be reported")
	}
}

func TestParseRoutingStrategy(t *testing.T) {
	for _, in := range []string{"respect_cpa_priority", "state_first"} {
		if _, err := ParseRoutingStrategy(in); err != nil {
			t.Errorf("ParseRoutingStrategy(%q) returned %v", in, err)
		}
	}
	// Empty means "use the default" rather than an error.
	if got, err := ParseRoutingStrategy(""); err != nil || got != StrategyRespectCPAPriority {
		t.Errorf("ParseRoutingStrategy(\"\") = %q, %v; want the default", got, err)
	}
	if _, err := ParseRoutingStrategy("state-first"); err == nil {
		t.Error("a misspelled strategy must be rejected, not silently defaulted")
	}
}

func TestManager_SnapshotIsAtomicUnderConcurrentReads(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					v := m.Current()
					// A torn read would show an impossible combination: the
					// master switch off while injection is still enabled.
					if !v.GlobalEnabled && v.Capabilities().Inject {
						t.Error("observed a torn settings snapshot")
						return
					}
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		on := i%2 == 0
		if _, err := m.Update(ctx, Patch{GlobalEnabled: &on}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	close(stop)
	readers.Wait()
}

func intPtr(v int) *int                     { return &v }
func durPtr(v time.Duration) *time.Duration { return &v }
func strategyPtr(v string) *RoutingStrategy {
	s := RoutingStrategy(v)
	return &s
}
