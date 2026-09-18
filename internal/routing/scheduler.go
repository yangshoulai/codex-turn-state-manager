// Package routing implements the CredentialScheduler: the plugin's
// interference in CPA's account selection.
//
// The plugin only overrides CPA when doing so is likely to help, i.e. when it
// holds a usable state for at least one candidate. In every other case it
// delegates, so a misconfiguration degrades to CPA's own behaviour rather than
// to no routing at all (design doc 3.11).
package routing

import (
	"context"
	"sort"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// CursorStore persists the round-robin position per account so a CPA restart
// does not pin traffic to whichever account happened to be first.
type CursorStore interface {
	LastUsed(ctx context.Context, authIndex string) (string, bool, error)
	SetLastUsed(ctx context.Context, authIndex, lastUsed string) error
}

// ModelPolicy answers whether a model participates in turn-state handling.
type ModelPolicy interface {
	ModelTurnStateEnabled(model string) bool
}

// Scheduler picks accounts among CPA's candidates.
type Scheduler struct {
	settings *settings.Manager
	states   *states.Registry
	cursors  CursorStore
	models   ModelPolicy
	logf     func(hostapi.LogLevel, string, map[string]any)

	now func() time.Time
}

// SchedulerConfig configures a Scheduler.
type SchedulerConfig struct {
	Settings *settings.Manager
	States   *states.Registry
	Cursors  CursorStore
	Models   ModelPolicy
	Log      func(hostapi.LogLevel, string, map[string]any)
}

// NewScheduler builds a scheduler.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	return &Scheduler{
		settings: cfg.Settings,
		states:   cfg.States,
		cursors:  cfg.Cursors,
		models:   cfg.Models,
		logf:     cfg.Log,
		now:      time.Now,
	}
}

// SetClock overrides the time source. Tests only.
func (s *Scheduler) SetClock(now func() time.Time) { s.now = now }

// Decide returns the plugin's account choice, or a delegation.
func (s *Scheduler) Decide(ctx context.Context, req hostapi.SchedulerPickRequest) hostapi.SchedulerPickResponse {
	caps := s.settings.Current().Capabilities()
	if !caps.Route {
		return delegate("master switch off")
	}
	if req.Provider != hostapi.ProviderCodex {
		return delegate("provider is not codex")
	}
	if s.models == nil || !s.models.ModelTurnStateEnabled(req.Model) {
		return delegate("model does not participate in turn state")
	}
	if len(req.Candidates) == 0 {
		return delegate("no candidates")
	}

	withState := s.withUsableState(req.Candidates, req.Model)
	if len(withState) == 0 {
		// Nothing to gain from interfering.
		return delegate("no candidate holds usable state")
	}

	// Both strategies end up choosing inside a single priority bucket, so that
	// round-robin never lets a low-priority account outrank a high-priority
	// one. They differ only in which set the bucket is computed over:
	//
	//   state_first          -- top bucket among the state-holding accounts,
	//                           which may be lower than CPA's overall top
	//                           bucket (state wins over CPA priority)
	//   respect_cpa_priority -- top bucket among all candidates, so a
	//                           state-holding account outside it is not
	//                           reached for; the plugin delegates instead
	var pool []hostapi.Candidate
	switch s.settings.Current().RoutingStrategy {
	case settings.StrategyStateFirst:
		pool = topPriorityBucket(withState)

	default: // settings.StrategyRespectCPAPriority
		top := topPriority(req.Candidates)
		for _, c := range withState {
			if c.Priority == top {
				pool = append(pool, c)
			}
		}
		if len(pool) == 0 {
			return delegate("top priority bucket holds no usable state")
		}
	}
	if len(pool) == 0 {
		return delegate("no candidate holds usable state in the top bucket")
	}

	pick := s.roundRobin(ctx, pool)
	return hostapi.SchedulerPickResponse{
		AuthID: pick.AuthID,
		Reason: "turn-state bound",
	}
}

// withUsableState filters candidates that hold a usable binding.
func (s *Scheduler) withUsableState(candidates []hostapi.Candidate, model string) []hostapi.Candidate {
	var out []hostapi.Candidate
	for _, c := range candidates {
		authIndex := c.AuthIndex
		if authIndex == "" {
			// Without a persistence key we cannot look up a binding; the
			// candidate is not disqualified, just not preferred.
			continue
		}
		if _, status := s.states.Lookup(authIndex, model); status.Usable() {
			out = append(out, c)
		}
	}
	return out
}

// roundRobin picks the least recently chosen candidate, persisting the choice.
//
// Least-recently-used is used rather than "longest remaining TTL" so traffic
// spreads across accounts instead of pinning to whichever one was refreshed
// last.
func (s *Scheduler) roundRobin(ctx context.Context, candidates []hostapi.Candidate) hostapi.Candidate {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		return candidates[i].AuthIndex < candidates[j].AuthIndex
	})

	best := candidates[0]
	bestAt := s.lastUsed(ctx, best.AuthIndex)

	for _, c := range candidates[1:] {
		at := s.lastUsed(ctx, c.AuthIndex)
		// A never-used account (empty cursor) always wins, so a new account
		// joins the rotation immediately.
		switch {
		case at == "" && bestAt != "":
			best, bestAt = c, ""
		case at != "" && bestAt == "":
			// keep current best
		case at < bestAt:
			best, bestAt = c, at
		}
	}

	if s.cursors != nil && best.AuthIndex != "" {
		if err := s.cursors.SetLastUsed(ctx, best.AuthIndex, s.now().UTC().Format(time.RFC3339Nano)); err != nil {
			// Cursor persistence is an optimisation; a failure degrades to
			// less-even rotation, not to a failed request.
			s.log(hostapi.LogWarn, "could not persist scheduler cursor", map[string]any{
				"authIndex": best.AuthIndex, "error": err.Error(),
			})
		}
	}
	return best
}

func (s *Scheduler) lastUsed(ctx context.Context, authIndex string) string {
	if s.cursors == nil || authIndex == "" {
		return ""
	}
	at, ok, err := s.cursors.LastUsed(ctx, authIndex)
	if err != nil || !ok {
		return ""
	}
	return at
}

func topPriority(candidates []hostapi.Candidate) int {
	top := candidates[0].Priority
	for _, c := range candidates[1:] {
		if c.Priority > top {
			top = c.Priority
		}
	}
	return top
}

// topPriorityBucket returns the candidates sharing the highest priority.
func topPriorityBucket(candidates []hostapi.Candidate) []hostapi.Candidate {
	if len(candidates) == 0 {
		return nil
	}
	top := topPriority(candidates)
	out := make([]hostapi.Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Priority == top {
			out = append(out, c)
		}
	}
	return out
}

func delegate(reason string) hostapi.SchedulerPickResponse {
	return hostapi.SchedulerPickResponse{Delegate: true, Reason: reason}
}

func (s *Scheduler) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if s.logf != nil {
		s.logf(level, msg, fields)
	}
}
