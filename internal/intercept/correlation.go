// Package intercept implements the outbound request pipeline: correlating a
// request across interceptor stages, injecting bound state, and harvesting
// state from responses.
package intercept

import (
	"sync"
	"time"
)

// Correlation links the three interceptor stages for one request.
//
// The plugin needs this because the AfterAuth request struct does not expose
// AuthID, so the account identity has to be carried from the Scheduler stage
// forward. A random per-request marker is injected as a temporary header in
// BeforeAuth, resolved in the Scheduler, and read back in AfterAuth
// (design doc 3.8).
//
// It also records whether this specific request actually carried
// plugin-injected state, which is the precondition for self-healing (3.12).
type Correlation struct {
	RequestID string
	Model     string
	Provider  string
	AuthID    string
	AuthIndex string
	CreatedAt time.Time

	// Injected is true only when the injector wrote a state value into this
	// request's headers.
	Injected   bool
	StateValue string
}

// CorrelationManager holds the in-flight request map.
//
// Entries are short-lived by construction, but a request that never reaches
// the response stage would leak one, so entries also expire on a TTL and are
// swept opportunistically.
type CorrelationManager struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[string]*Correlation
}

// DefaultCorrelationTTL bounds how long an unmatched request is remembered.
const DefaultCorrelationTTL = 10 * time.Minute

// NewCorrelationManager builds a manager.
func NewCorrelationManager(ttl time.Duration) *CorrelationManager {
	if ttl <= 0 {
		ttl = DefaultCorrelationTTL
	}
	return &CorrelationManager{
		ttl: ttl,
		now: time.Now,
		m:   map[string]*Correlation{},
	}
}

// SetClock overrides the time source. Tests only.
func (c *CorrelationManager) SetClock(now func() time.Time) { c.now = now }

// Begin records the start of a request. A repeated RequestID is treated as a
// new request, overwriting the stale entry.
func (c *CorrelationManager) Begin(requestID, model, provider string) *Correlation {
	rec := &Correlation{
		RequestID: requestID,
		Model:     model,
		Provider:  provider,
		CreatedAt: c.now(),
	}
	c.mu.Lock()
	c.m[requestID] = rec
	c.mu.Unlock()
	return rec
}

// AttachAuth records which account the scheduler chose.
func (c *CorrelationManager) AttachAuth(requestID, authID, authIndex string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rec, ok := c.m[requestID]; ok {
		rec.AuthID = authID
		rec.AuthIndex = authIndex
	}
}

// MarkInjected records that state was written into the request.
func (c *CorrelationManager) MarkInjected(requestID, stateValue string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rec, ok := c.m[requestID]; ok {
		rec.Injected = true
		rec.StateValue = stateValue
	}
}

// Get returns a copy of the record for a request.
func (c *CorrelationManager) Get(requestID string) (Correlation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.m[requestID]
	if !ok {
		return Correlation{}, false
	}
	return *rec, true
}

// Complete removes and returns a request's record.
func (c *CorrelationManager) Complete(requestID string) (Correlation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.m[requestID]
	if !ok {
		return Correlation{}, false
	}
	delete(c.m, requestID)
	return *rec, true
}

// Forget drops a request's record without returning it.
func (c *CorrelationManager) Forget(requestID string) {
	c.mu.Lock()
	delete(c.m, requestID)
	c.mu.Unlock()
}

// Len reports the number of tracked requests.
func (c *CorrelationManager) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// Sweep drops expired entries. It is called opportunistically from Begin so no
// background goroutine is needed.
func (c *CorrelationManager) Sweep() int {
	cutoff := c.now().Add(-c.ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	var dropped int
	for id, rec := range c.m {
		if rec.CreatedAt.Before(cutoff) {
			delete(c.m, id)
			dropped++
		}
	}
	return dropped
}
