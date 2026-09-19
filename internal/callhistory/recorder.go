// Package callhistory records what happened to each intercepted request.
//
// The binding registry answers "what state does this pair hold". This package
// answers the next question an operator asks when that is not enough: what did
// the request actually carry on the wire, what did the plugin put into it, what
// came back, and how did it end. Those four facts live in three different
// places (the injector, the response interceptor, and the host's completion
// callback) and are stitched back together here by request id.
//
// The request path never touches SQLite. A record is assembled in memory and
// handed to a background writer through a bounded channel; a full channel drops
// the row rather than blocking the host's request goroutine (NF-01).
package callhistory

import (
	"context"
	"sync"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
)

// Record is one intercepted request.
type Record struct {
	ID        int64  `json:"id"`
	AuthIndex string `json:"authIndex"`
	Model     string `json:"model"`
	// RequestID is the host's identifier for the request. Kept because it is the
	// only way to correlate a row here with a host log line.
	RequestID string `json:"requestId,omitempty"`

	// CarriedState is the turn-state header the request already had when it
	// reached the plugin -- sent by the client, or left over from a previous
	// layer. Empty on a normal request.
	CarriedState string `json:"-"`
	// InjectedState is the value the plugin wrote. Empty when it injected
	// nothing, which is what makes "no binding" distinguishable from "binding
	// that happened to match".
	InjectedState string `json:"-"`
	// ResponseState is the turn-state header upstream answered with. Empty when
	// upstream sent none, which on this traffic path is the common case.
	ResponseState string `json:"-"`

	// StatusCode is the upstream HTTP status as the host reported it on
	// completion. Zero means the request never reached a terminal state.
	StatusCode int `json:"statusCode"`
	// Outcome is the host's terminal outcome: succeeded, failed, rejected or
	// canceled.
	Outcome   string    `json:"outcome,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// CarriedLength, InjectedLength and ResponseLength are derived for the panel so
// it can render a length column without the value.
func (r Record) CarriedLength() int  { return len(r.CarriedState) }
func (r Record) InjectedLength() int { return len(r.InjectedState) }
func (r Record) ResponseLength() int { return len(r.ResponseState) }

// Query filters and pages call history. Zero-valued fields mean "no
// constraint".
type Query struct {
	AuthIndex string
	Model     string
	Limit     int
	Offset    int
}

// Normalise applies the defaults a caller may leave unset.
func (q Query) Normalise() Query {
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

// Store persists call records. Implemented by storage.CallStore.
type Store interface {
	AppendCalls(ctx context.Context, rows []Record) error
	ListCalls(ctx context.Context, q Query) ([]Record, error)
	// CountCalls returns how many rows match the filters, ignoring paging, so
	// the panel can show a page count.
	CountCalls(ctx context.Context, q Query) (int, error)
	// PruneCallsBefore deletes rows created before the cutoff and reports how
	// many went.
	PruneCallsBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// DefaultPendingTTL bounds how long an unfinished request is remembered.
const DefaultPendingTTL = 15 * time.Minute

// queueSize caps how many finished records may wait for the writer. A burst
// larger than this loses rows rather than applying back-pressure to the host.
const queueSize = 2048

// Recorder assembles per-request records and writes them in the background.
type Recorder struct {
	settings *settings.Manager
	store    Store
	ttl      time.Duration

	queue chan Record
	now   func() time.Time

	mu      sync.Mutex
	pending map[string]*Record
	order   []string

	// dropped counts records discarded because the writer fell behind. Surfaced
	// so a gap in the panel is explainable rather than mysterious.
	dropped    int64
	writeError string
}

// Config configures a Recorder.
type Config struct {
	Settings *settings.Manager
	Store    Store
	// PendingTTL bounds how long a request that never completes is kept before
	// being written with whatever was observed.
	PendingTTL time.Duration
}

// New builds a recorder.
func New(cfg Config) *Recorder {
	ttl := cfg.PendingTTL
	if ttl <= 0 {
		ttl = DefaultPendingTTL
	}
	return &Recorder{
		settings: cfg.Settings,
		store:    cfg.Store,
		ttl:      ttl,
		queue:    make(chan Record, queueSize),
		now:      time.Now,
		pending:  map[string]*Record{},
	}
}

// SetClock overrides the time source. Tests only.
func (r *Recorder) SetClock(now func() time.Time) { r.now = now }

// enabled reads the master switch through one snapshot, like every other
// capability. Call history is diagnostic rather than an intervention, so it is
// gated by the master switch alone and not by the reverse-bind sub-switch:
// turning off capture is a statement about writing bindings, not about being
// able to see what happened.
func (r *Recorder) enabled() bool {
	if r == nil || r.settings == nil || r.store == nil {
		return false
	}
	return r.settings.Current().Capabilities().Enabled
}

// Begin notes a request that reached the injection stage.
//
// carried is the turn-state header as the request arrived; injected is what the
// plugin wrote into it (empty when it wrote nothing).
func (r *Recorder) Begin(requestID, authIndex, model, carried, injected string) {
	if !r.enabled() || requestID == "" || authIndex == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.pending[requestID]; !exists {
		r.order = append(r.order, requestID)
	}
	r.pending[requestID] = &Record{
		AuthIndex:     authIndex,
		Model:         model,
		RequestID:     requestID,
		CarriedState:  carried,
		InjectedState: injected,
		CreatedAt:     r.now(),
	}
}

// Response fills in what upstream answered.
//
// Called from the response interceptor, which can arrive before or after
// completion depending on the path: the streaming header-init callback fires
// while the turn is still running, so the row is not written here.
func (r *Recorder) Response(requestID, state string, status int) {
	if !r.enabled() || requestID == "" {
		return
	}
	r.mu.Lock()
	rec, ok := r.pending[requestID]
	if ok {
		rec.ResponseState = state
		if status != 0 {
			rec.StatusCode = status
		}
	}
	r.mu.Unlock()
}

// Complete writes the record. This is the only path that produces a row.
func (r *Recorder) Complete(requestID string, status int, outcome string) {
	if !r.enabled() || requestID == "" {
		return
	}
	r.mu.Lock()
	rec, ok := r.pending[requestID]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.pending, requestID)
	r.order = removeString(r.order, requestID)
	if status != 0 {
		rec.StatusCode = status
	}
	rec.Outcome = outcome
	row := *rec
	r.mu.Unlock()

	r.enqueue(row)
}

// Sweep writes out records whose request never reached a terminal state.
//
// A canceled request, a client that disconnected, or a host that stopped
// reporting would otherwise leave the row in memory forever. Writing it with
// StatusCode 0 is the honest answer: the request was seen and never finished.
func (r *Recorder) Sweep() int {
	if r == nil || r.store == nil {
		return 0
	}
	cutoff := r.now().Add(-r.ttl)

	r.mu.Lock()
	var stale []Record
	kept := r.order[:0]
	for _, id := range r.order {
		rec, ok := r.pending[id]
		if !ok {
			continue
		}
		if rec.CreatedAt.Before(cutoff) {
			stale = append(stale, *rec)
			delete(r.pending, id)
			continue
		}
		kept = append(kept, id)
	}
	r.order = kept
	r.mu.Unlock()

	for _, row := range stale {
		r.enqueue(row)
	}
	return len(stale)
}

// Pending reports how many requests are being tracked.
func (r *Recorder) Pending() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// enqueue hands a finished record to the writer without ever blocking.
//
// A dropped row is a gap in the history, not a stalled proxy: this runs on the
// goroutine that is serving a user's request.
func (r *Recorder) enqueue(row Record) {
	select {
	case r.queue <- row:
	default:
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
	}
}

// Dropped reports how many records the writer never saw.
func (r *Recorder) Dropped() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// WriteError reports the last persistence failure, or "" when the writer is
// healthy.
func (r *Recorder) WriteError() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeError
}

// Run drains the queue until ctx ends, writing rows in batches.
//
// Batched because the alternative is one transaction per request, and a busy
// proxy produces far more requests than the disk wants transactions. Batching
// is bounded by the queue going quiet rather than only by the ticker: an
// operator who has just sent one request should see it in the panel
// immediately, not up to a second later.
func (r *Recorder) Run(ctx context.Context) {
	if r == nil || r.store == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	batch := make([]Record, 0, 64)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.store.AppendCalls(ctx, batch); err != nil {
			r.mu.Lock()
			r.writeError = err.Error()
			r.mu.Unlock()
		} else {
			r.mu.Lock()
			r.writeError = ""
			r.mu.Unlock()
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// The batch is dropped deliberately: the context is already
			// cancelled, so a final write would fail and be reported as an
			// error the shutdown caused itself.
			return
		case row := <-r.queue:
			batch = append(batch, row)
			// Take whatever else is already waiting before touching the disk,
			// so a burst still costs one transaction.
		collect:
			for len(batch) < cap(batch) {
				select {
				case more := <-r.queue:
					batch = append(batch, more)
				default:
					break collect
				}
			}
			flush()
		case <-ticker.C:
			flush()
		}
	}
}

func removeString(list []string, target string) []string {
	for i, v := range list {
		if v == target {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}
