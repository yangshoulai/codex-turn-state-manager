// Package routing implements the CredentialScheduler: the plugin's
// interference in CPA's account selection.
//
// The plugin only overrides the host when doing so is likely to help, i.e. when
// it holds a usable state for at least one candidate. In every other case it
// declines, so a misconfiguration degrades to the host's own behaviour rather
// than to no routing at all.
//
// Reconciled against CLIProxyAPI v7.3.7:
//
//   - Candidates carry the host's runtime auth id (`SchedulerAuthCandidate.ID`)
//     and NOT the plugin's AuthIndex, so the persistence key is resolved
//     through AccountRegistry.
//   - The host pre-filters candidates to the highest available priority tier
//     unless the plugin declares SchedulerAcrossPriorities at registration. The
//     plugin declares it, which means it must apply the tier policy itself --
//     otherwise "respect CPA priority" would silently stop respecting anything.
//   - The response is honoured only when Handled is true.
package routing

import (
	"context"
	"sort"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
)

// CursorStore persists the round-robin position per account so a restart does
// not pin traffic to whichever account happened to be first.
type CursorStore interface {
	LastUsed(ctx context.Context, authIndex string) (string, bool, error)
	SetLastUsed(ctx context.Context, authIndex, lastUsed string) error
}

// ModelPolicy answers whether a model participates in turn-state handling.
type ModelPolicy interface {
	ModelTurnStateEnabled(model string) bool
}

// AuthResolver maps a host runtime auth id onto the plugin's persistence key.
type AuthResolver interface {
	ResolveAuthID(authID string) (string, bool)
}

// Scheduler picks accounts among the host's candidates.
type Scheduler struct {
	settings *settings.Manager
	states   *states.Registry
	cursors  CursorStore
	models   ModelPolicy
	auth     AuthResolver
	logf     func(hostapi.LogLevel, string, map[string]any)

	now func() time.Time
}

// SchedulerConfig configures a Scheduler.
type SchedulerConfig struct {
	Settings *settings.Manager
	States   *states.Registry
	Cursors  CursorStore
	Models   ModelPolicy
	Auth     AuthResolver
	Log      func(hostapi.LogLevel, string, map[string]any)
}

// NewScheduler builds a scheduler.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	return &Scheduler{
		settings: cfg.Settings,
		states:   cfg.States,
		cursors:  cfg.Cursors,
		models:   cfg.Models,
		auth:     cfg.Auth,
		logf:     cfg.Log,
		now:      time.Now,
	}
}

// SetClock overrides the time source. Tests only.
func (s *Scheduler) SetClock(now func() time.Time) { s.now = now }

// Decide returns the plugin's account choice, or declines to make one.
func (s *Scheduler) Decide(ctx context.Context, req hostapi.SchedulerPickRequest) hostapi.SchedulerPickResponse {
	caps := s.settings.Current().Capabilities()
	if !caps.Route {
		// Either the master switch or the state-priority switch is off. The
		// latter exists precisely so an operator can take the plugin out of
		// the routing path without disabling anything else.
		return decline("routing interference is disabled")
	}
	if !providerIsCodex(req) {
		return decline("provider is not codex")
	}
	if s.models == nil || !s.models.ModelTurnStateEnabled(req.Model) {
		return decline("model does not participate in turn state")
	}
	if len(req.Candidates) == 0 {
		return decline("no candidates")
	}

	// Attach each candidate's persistence key once, so the rest of the
	// decision works in terms of AuthIndex.
	all := make([]scoredCandidate, 0, len(req.Candidates))
	withState := make([]scoredCandidate, 0, len(req.Candidates))

	for _, c := range req.Candidates {
		authIndex, ok := s.resolve(ctx, c)
		if !ok {
			// Without a persistence key there is no binding to look up. The
			// candidate is not disqualified, just not preferred.
			all = append(all, scoredCandidate{candidate: c})
			continue
		}
		item := scoredCandidate{candidate: c, authIndex: authIndex}
		if _, status := s.states.Lookup(authIndex, req.Model); status.Usable() {
			item.hasState = true
			withState = append(withState, item)
		}
		all = append(all, item)
	}

	if len(withState) == 0 {
		// Nothing to gain from interfering.
		return decline("no candidate holds usable state")
	}

	// Both strategies choose inside a single priority tier, so round-robin can
	// never let a low-priority account outrank a high-priority one. They differ
	// in which set the tier is computed over:
	//
	//   state_first          -- top tier among the state-holding accounts, which
	//                           may be lower than the host's overall top tier
	//                           (state wins over host priority)
	//   respect_cpa_priority -- top tier among all candidates, so a
	//                           state-holding account outside it is not reached
	//                           for; the plugin declines instead
	var pool []scoredCandidate
	switch s.settings.Current().RoutingStrategy {
	case settings.StrategyStateFirst:
		pool = topTier(withState)

	default: // settings.StrategyRespectCPAPriority
		top := topPriority(all)
		for _, item := range withState {
			if item.candidate.Priority == top {
				pool = append(pool, item)
			}
		}
		if len(pool) == 0 {
			return decline("top priority tier holds no usable state")
		}
	}
	if len(pool) == 0 {
		return decline("no candidate holds usable state in the top tier")
	}

	pick := s.roundRobin(ctx, pool)
	return hostapi.SchedulerPickResponse{
		AuthID:  pick.candidate.ID,
		Handled: true,
		Reason:  "turn-state bound",
	}
}

// resolve maps a candidate onto the plugin's persistence key.
func (s *Scheduler) resolve(ctx context.Context, c hostapi.Candidate) (string, bool) {
	if s.auth == nil {
		return "", false
	}
	return s.auth.ResolveAuthID(c.ID)
}

func providerIsCodex(req hostapi.SchedulerPickRequest) bool {
	if req.Provider == hostapi.ProviderCodex {
		return true
	}
	// A mixed route reports an empty Provider and lists the accepted keys
	// instead, so a codex-only pool still qualifies.
	if req.Provider == "" && len(req.Providers) > 0 {
		for _, p := range req.Providers {
			if p != hostapi.ProviderCodex {
				return false
			}
		}
		return true
	}
	return false
}

// scoredCandidate pairs a host candidate with the plugin's persistence key.
type scoredCandidate struct {
	candidate hostapi.Candidate
	authIndex string
	hasState  bool
}

// topPriority returns the highest priority present in a set.
func topPriority(items []scoredCandidate) int {
	top := items[0].candidate.Priority
	for _, item := range items[1:] {
		if item.candidate.Priority > top {
			top = item.candidate.Priority
		}
	}
	return top
}

// topTier returns the candidates sharing the highest priority.
//
// The plugin has to do this itself because it declares
// SchedulerAcrossPriorities, which stops the host from pre-filtering the list.
func topTier(items []scoredCandidate) []scoredCandidate {
	if len(items) == 0 {
		return nil
	}
	top := topPriority(items)
	out := make([]scoredCandidate, 0, len(items))
	for _, item := range items {
		if item.candidate.Priority == top {
			out = append(out, item)
		}
	}
	return out
}

// roundRobin picks the least recently chosen candidate, persisting the choice.
//
// Least-recently-used is used rather than "longest remaining TTL" so traffic
// spreads across accounts instead of pinning to whichever one was refreshed
// last.
func (s *Scheduler) roundRobin(ctx context.Context, pool []scoredCandidate) scoredCandidate {
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].candidate.Priority != pool[j].candidate.Priority {
			return pool[i].candidate.Priority > pool[j].candidate.Priority
		}
		return pool[i].candidate.ID < pool[j].candidate.ID
	})

	best := pool[0]
	bestAt := s.lastUsed(ctx, best.authIndex)

	for _, item := range pool[1:] {
		at := s.lastUsed(ctx, item.authIndex)
		switch {
		case at == "" && bestAt != "":
			// A never-used account joins the rotation immediately.
			best, bestAt = item, ""
		case at == "" && bestAt == "":
			// keep the current best
		case at < bestAt:
			best, bestAt = item, at
		}
	}

	if s.cursors != nil && best.authIndex != "" {
		if err := s.cursors.SetLastUsed(ctx, best.authIndex, s.now().UTC().Format(time.RFC3339Nano)); err != nil {
			// Cursor persistence is an optimisation; a failure degrades to
			// less-even rotation, not to a failed request.
			s.log(hostapi.LogWarn, "could not persist scheduler cursor", map[string]any{
				"authIndex": best.authIndex, "error": err.Error(),
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

func decline(reason string) hostapi.SchedulerPickResponse {
	return hostapi.SchedulerPickResponse{Handled: false, Reason: reason}
}

func (s *Scheduler) log(level hostapi.LogLevel, msg string, fields map[string]any) {
	if s.logf != nil {
		s.logf(level, msg, fields)
	}
}
