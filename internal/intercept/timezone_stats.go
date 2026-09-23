package intercept

import (
	"sync/atomic"
)

// TimezoneStats counts what the timezone rewriter saw.
//
// Counters rather than log lines, for the same reason the pipeline counters
// exist: the rewrite edits a payload and leaves nothing else behind, so this is
// the only way to answer "is it firing at all". That question matters more here
// than elsewhere, because whether a given client puts a timezone in the request
// is not something the plugin can assume -- a feature that silently does nothing
// looks exactly like a feature that is broken.
type TimezoneStats struct {
	// seen counts every request that reached the stage.
	seen atomic.Int64
	// disabled counts requests passed over because conversion is off, or
	// because the configured target is empty or unloadable.
	disabled atomic.Int64
	// noMarker counts bodies that carried no recognisable timezone. A large
	// number here is the answer to "why does nothing happen": this traffic
	// does not declare a timezone to rewrite.
	noMarker atomic.Int64
	// matched counts bodies that carried one.
	matched atomic.Int64
	// changed counts bodies actually rewritten.
	changed atomic.Int64
	// same counts bodies whose timezone was already the target.
	same atomic.Int64
}

// RecordDisabled notes a request passed over.
func (s *TimezoneStats) RecordDisabled() { s.disabled.Add(1) }

// Record notes one rewrite attempt.
func (s *TimezoneStats) Record(seen, changed bool) {
	s.seen.Add(1)
	if seen {
		s.matched.Add(1)
	} else {
		s.noMarker.Add(1)
		return
	}
	if changed {
		s.changed.Add(1)
	} else {
		s.same.Add(1)
	}
}

// TimezoneSnapshot is the exportable view.
type TimezoneSnapshot struct {
	Seen     int64 `json:"seen"`
	Disabled int64 `json:"disabled"`
	NoMarker int64 `json:"noMarker"`
	Matched  int64 `json:"matched"`
	Changed  int64 `json:"changed"`
	Same     int64 `json:"same"`
}

// Snapshot reads the counters.
func (s *TimezoneStats) Snapshot() TimezoneSnapshot {
	if s == nil {
		return TimezoneSnapshot{}
	}
	return TimezoneSnapshot{
		Seen:     s.seen.Load(),
		Disabled: s.disabled.Load(),
		NoMarker: s.noMarker.Load(),
		Matched:  s.matched.Load(),
		Changed:  s.changed.Load(),
		Same:     s.same.Load(),
	}
}
