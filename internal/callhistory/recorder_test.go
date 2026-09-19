package callhistory

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
)

// ---------------------------------------------------------------------------
// doubles

type stubSettingsStore struct {
	mu     sync.Mutex
	values map[string]string
}

func (s *stubSettingsStore) All(context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}

func (s *stubSettingsStore) PutMany(_ context.Context, values map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range values {
		s.values[k] = v
	}
	return nil
}

type stubStore struct {
	mu    sync.Mutex
	rows  []Record
	failN int
}

func (s *stubStore) AppendCalls(_ context.Context, rows []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failN > 0 {
		s.failN--
		return context.DeadlineExceeded
	}
	s.rows = append(s.rows, rows...)
	return nil
}

func (s *stubStore) ListCalls(context.Context, Query) ([]Record, error) { return nil, nil }
func (s *stubStore) CountCalls(context.Context, Query) (int, error)     { return 0, nil }
func (s *stubStore) PruneCallsBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func (s *stubStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

func (s *stubStore) last() (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) == 0 {
		return Record{}, false
	}
	return s.rows[len(s.rows)-1], true
}

func newTestRecorder(t *testing.T, master bool) (*Recorder, *stubStore, *settings.Manager) {
	t.Helper()
	ctx := context.Background()
	store := &stubStore{}
	manager, err := settings.NewManager(ctx, &stubSettingsStore{values: map[string]string{}}, nil)
	if err != nil {
		t.Fatalf("settings.NewManager: %v", err)
	}
	if !master {
		if _, err := manager.Update(ctx, settings.Patch{GlobalEnabled: boolPtr(false)}); err != nil {
			t.Fatalf("disable master switch: %v", err)
		}
	}
	return New(Config{Settings: manager, Store: store}), store, manager
}

func boolPtr(b bool) *bool { return &b }

// drain runs the writer and waits for it to have flushed at least want rows.
//
// The negative cases pass want 1 and never see it, so the deadline is what
// bounds them; it is kept short because it is paid in full by every test that
// asserts nothing was written.
func drain(t *testing.T, r *Recorder, want int, store *stubStore) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if store.count() >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// tests

// TestRecorder_JoinsTheThreeObservations is the whole point of the package: the
// carried value, the injected value, the response, and the outcome arrive from
// three different callbacks, and none of them sees the whole request.
func TestRecorder_JoinsTheThreeObservations(t *testing.T) {
	r, store, _ := newTestRecorder(t, true)
	at := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	r.SetClock(func() time.Time { return at })

	r.Begin("req-1", "auth-a", "gpt-5.6-sol", "", "INJECTED")
	r.Response("req-1", "RETURNED", 0)
	r.Complete("req-1", 200, "succeeded")

	drain(t, r, 1, store)
	row, ok := store.last()
	if !ok {
		t.Fatal("no row was written")
	}
	if row.AuthIndex != "auth-a" || row.Model != "gpt-5.6-sol" {
		t.Errorf("pair = %s/%s", row.AuthIndex, row.Model)
	}
	if row.CarriedState != "" || row.InjectedState != "INJECTED" || row.ResponseState != "RETURNED" {
		t.Errorf("states = %q / %q / %q", row.CarriedState, row.InjectedState, row.ResponseState)
	}
	if row.StatusCode != 200 || row.Outcome != "succeeded" {
		t.Errorf("status = %d, outcome = %q", row.StatusCode, row.Outcome)
	}
	if !row.CreatedAt.Equal(at) {
		t.Errorf("CreatedAt = %v, want %v", row.CreatedAt, at)
	}
}

// TestRecorder_CompletionWinsOverTheResponseStatus: the streaming header-init
// call carries no status code, and the completion callback is the authoritative
// one. A status recorded earlier must not shadow it.
func TestRecorder_CompletionWinsOverTheResponseStatus(t *testing.T) {
	r, store, _ := newTestRecorder(t, true)

	r.Begin("req-1", "auth-a", "m", "", "S")
	r.Response("req-1", "R", 0)
	r.Complete("req-1", 429, "failed")

	drain(t, r, 1, store)
	row, _ := store.last()
	if row.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", row.StatusCode)
	}
}

// TestRecorder_CompletionDoesNotEraseARecordedStatus covers the non-streaming
// path, where the response interceptor already knows the status and completion
// may report zero.
func TestRecorder_CompletionDoesNotEraseARecordedStatus(t *testing.T) {
	r, store, _ := newTestRecorder(t, true)

	r.Begin("req-1", "auth-a", "m", "", "S")
	r.Response("req-1", "R", 201)
	r.Complete("req-1", 0, "succeeded")

	drain(t, r, 1, store)
	row, _ := store.last()
	if row.StatusCode != 201 {
		t.Errorf("StatusCode = %d, want the recorded 201", row.StatusCode)
	}
}

// TestRecorder_OnlyCompletionWrites: a response that arrives while the turn is
// still running must not produce a row, or every streamed request would be
// recorded twice.
func TestRecorder_OnlyCompletionWrites(t *testing.T) {
	r, store, _ := newTestRecorder(t, true)

	r.Begin("req-1", "auth-a", "m", "", "S")
	r.Response("req-1", "R", 200)
	if got := store.count(); got != 0 {
		t.Fatalf("rows after Response = %d, want 0", got)
	}
	if got := r.Pending(); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
}

// TestRecorder_SweepWritesAbandonedRequests: a canceled request, or a host that
// stops reporting, would otherwise leave the record in memory forever.
func TestRecorder_SweepWritesAbandonedRequests(t *testing.T) {
	r, store, _ := newTestRecorder(t, true)
	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	r.SetClock(func() time.Time { return now })

	r.Begin("req-old", "auth-a", "m", "", "S")
	now = now.Add(DefaultPendingTTL + time.Minute)

	if n := r.Sweep(); n != 1 {
		t.Fatalf("Sweep wrote %d, want 1", n)
	}
	if got := r.Pending(); got != 0 {
		t.Errorf("pending = %d, want 0", got)
	}

	drain(t, r, 1, store)
	row, _ := store.last()
	if row.StatusCode != 0 || row.Outcome != "" {
		t.Errorf("abandoned row = status %d outcome %q, want zeroes", row.StatusCode, row.Outcome)
	}
}

// TestRecorder_SweepLeavesFreshRequestsAlone.
func TestRecorder_SweepLeavesFreshRequestsAlone(t *testing.T) {
	r, _, _ := newTestRecorder(t, true)
	r.Begin("req-new", "auth-a", "m", "", "S")
	if n := r.Sweep(); n != 0 {
		t.Fatalf("Sweep wrote %d, want 0", n)
	}
	if got := r.Pending(); got != 1 {
		t.Errorf("pending = %d, want 1", got)
	}
}

// TestRecorder_MasterSwitchOffRecordsNothing: the master switch gates every
// capability, and the call log is one of them -- with it off the plugin should
// look like it was never loaded.
func TestRecorder_MasterSwitchOffRecordsNothing(t *testing.T) {
	r, store, _ := newTestRecorder(t, false)

	r.Begin("req-1", "auth-a", "m", "", "S")
	r.Response("req-1", "R", 200)
	r.Complete("req-1", 200, "succeeded")

	if got := store.count(); got != 0 {
		t.Errorf("rows = %d, want 0 with the master switch off", got)
	}
	if got := r.Pending(); got != 0 {
		t.Errorf("pending = %d, want 0 with the master switch off", got)
	}
}

// TestRecorder_ASubSwitchDoesNotGateTheLog pins the deliberate asymmetry: the
// call log answers "what did the plugin do to my request", which is the
// question an operator has precisely when they have turned a capability off.
func TestRecorder_ASubSwitchDoesNotGateTheLog(t *testing.T) {
	r, store, manager := newTestRecorder(t, true)
	if _, err := manager.Update(context.Background(), settings.Patch{
		GlobalProbeEnabled:       boolPtr(false),
		GlobalReverseBindEnabled: boolPtr(false),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	r.Begin("req-1", "auth-a", "m", "", "S")
	r.Complete("req-1", 200, "succeeded")

	drain(t, r, 1, store)
	if got := store.count(); got != 1 {
		t.Errorf("rows = %d, want 1 with a sub-switch off", got)
	}
}

// TestRecorder_DropsRatherThanBlocks: this runs on the goroutine serving a
// user's request, so a full queue must lose the row rather than apply
// back-pressure to the proxy.
func TestRecorder_DropsRatherThanBlocks(t *testing.T) {
	r, _, _ := newTestRecorder(t, true)

	// Nothing is draining the queue, so it fills and then drops.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < queueSize+64; i++ {
			id := "req-" + time.Duration(i).String()
			r.Begin(id, "auth-a", "m", "", "S")
			r.Complete(id, 200, "succeeded")
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Complete blocked on a full queue")
	}
	if got := r.Dropped(); got != 64 {
		t.Errorf("dropped = %d, want 64", got)
	}
}

// TestRecorder_UnknownRequestIsIgnored: a completion for a request this
// recorder never saw must not create a row with no account on it.
func TestRecorder_UnknownRequestIsIgnored(t *testing.T) {
	r, store, _ := newTestRecorder(t, true)
	r.Complete("never-seen", 200, "succeeded")

	drain(t, r, 1, store)
	if got := store.count(); got != 0 {
		t.Errorf("rows = %d, want 0", got)
	}
}

// TestRecorder_ReportsAWriteFailure: a gap in the history has to be explicable
// from the panel, or it reads as "no traffic".
func TestRecorder_ReportsAWriteFailure(t *testing.T) {
	r, store, _ := newTestRecorder(t, true)
	store.failN = 1

	r.Begin("req-1", "auth-a", "m", "", "S")
	r.Complete("req-1", 200, "succeeded")

	drain(t, r, 1, store)
	if r.WriteError() == "" {
		t.Fatal("WriteError is empty after a failed write")
	}
}
