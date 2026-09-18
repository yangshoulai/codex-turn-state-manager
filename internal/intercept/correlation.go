// Package intercept implements the outbound request pipeline: injecting bound
// state into a request and harvesting state from responses.
//
// Reconciled against CLIProxyAPI v7.3.7. The design document originally
// specified a correlation-header mechanism: inject a marker in BeforeAuth, read
// it in the Scheduler, recover it in AfterAuth. That mechanism is gone. The
// host populates Metadata["selected_auth_index"] before invoking the after-auth
// hook, so the account identity is simply read from there -- no header is
// injected, and nothing has to be stripped before the request leaves.
//
// The remaining CorrelationManager is not about header plumbing: it records, per
// request id, which pair was injected and whether injection happened at all,
// which is what the response stage and the self-healing rules need.
package intercept

import (
	"sync"
	"time"
)

// Correlation records what the plugin did to one request.
type Correlation struct {
	RequestID string
	Model     string
	AuthID    string
	AuthIndex string
	CreatedAt time.Time

	// Injected is true only when the injector wrote a state value into this
	// request's headers. Self-healing keys off this flag rather than inferring
	// it, so a request the plugin left alone is never blamed for a failure.
	Injected   bool
	StateValue string
}

// CorrelationManager holds the in-flight request map.
//
// Entries are short-lived by construction, but a request that never reaches a
// terminal state would leak one, so entries also expire on a TTL and are swept
// opportunistically.
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

// Record notes the account a request was served by.
func (c *CorrelationManager) Record(requestID, model, authID, authIndex string) *Correlation {
	rec := &Correlation{
		RequestID: requestID,
		Model:     model,
		AuthID:    authID,
		AuthIndex: authIndex,
		CreatedAt: c.now(),
	}
	c.mu.Lock()
	c.m[requestID] = rec
	c.mu.Unlock()
	return rec
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

// Sweep drops expired entries. It is called opportunistically so no background
// goroutine is needed.
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
