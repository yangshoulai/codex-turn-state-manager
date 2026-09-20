package headers

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signal headers the Codex upstream returns alongside every response.
//
// These carry the account's plan and its rate-limit windows -- the same facts
// the panel wants for "why is this account idle", except the upstream states
// them directly instead of the plugin inferring them. The id_token's
// chatgpt_plan_type claim is the other source and is frequently absent; this
// one arrives on every response.
const (
	SignalPlanType = "X-Codex-Plan-Type"

	// Primary is the short window (X-Codex-Primary-Window-Minutes, 300 for the
	// 5-hour limit); Secondary is the long one (weekly).
	SignalPrimaryUsedPercent   = "X-Codex-Primary-Used-Percent"
	SignalPrimaryResetAt       = "X-Codex-Primary-Reset-At"
	SignalPrimaryWindowMinutes = "X-Codex-Primary-Window-Minutes"

	SignalSecondaryUsedPercent   = "X-Codex-Secondary-Used-Percent"
	SignalSecondaryResetAt       = "X-Codex-Secondary-Reset-At"
	SignalSecondaryWindowMinutes = "X-Codex-Secondary-Window-Minutes"

	SignalActiveLimit    = "X-Codex-Active-Limit"
	SignalCreditsBalance = "X-Codex-Credits-Balance"
	SignalCreditsUnlimit = "X-Codex-Credits-Unlimited"
	SignalCreditsHasAny  = "X-Codex-Credits-Has-Credits"

	SignalModelsEtag   = "X-Models-Etag"
	SignalOAIRequestID = "X-Oai-Request-Id"
)

// Signals is the account state the upstream reports on a response.
//
// Every field is optional: the headers are undocumented and may appear, change
// shape, or vanish without notice, so a missing or unparseable value must leave
// the field at its zero value rather than fail the request that carried it.
type Signals struct {
	PlanType string `json:"planType,omitempty"`

	PrimaryUsedPercent   *int       `json:"primaryUsedPercent,omitempty"`
	PrimaryResetAt       *time.Time `json:"primaryResetAt,omitempty"`
	PrimaryWindowMinutes *int       `json:"primaryWindowMinutes,omitempty"`

	SecondaryUsedPercent   *int       `json:"secondaryUsedPercent,omitempty"`
	SecondaryResetAt       *time.Time `json:"secondaryResetAt,omitempty"`
	SecondaryWindowMinutes *int       `json:"secondaryWindowMinutes,omitempty"`

	ActiveLimit    string `json:"activeLimit,omitempty"`
	CreditsBalance string `json:"creditsBalance,omitempty"`
	CreditsOnly    bool   `json:"creditsOnly,omitempty"`
}

// Empty reports whether the response carried no signal headers at all.
func (s Signals) Empty() bool {
	return s.PlanType == "" && s.PrimaryUsedPercent == nil && s.SecondaryUsedPercent == nil &&
		s.ActiveLimit == "" && s.CreditsBalance == ""
}

// ParseSignals reads the signal headers from a response.
func ParseSignals(h http.Header) Signals {
	if h == nil {
		return Signals{}
	}
	var s Signals
	s.PlanType = strings.TrimSpace(h.Get(SignalPlanType))

	s.PrimaryUsedPercent = percent(h.Get(SignalPrimaryUsedPercent))
	s.PrimaryResetAt = timestamp(h.Get(SignalPrimaryResetAt))
	s.PrimaryWindowMinutes = integer(h.Get(SignalPrimaryWindowMinutes))

	s.SecondaryUsedPercent = percent(h.Get(SignalSecondaryUsedPercent))
	s.SecondaryResetAt = timestamp(h.Get(SignalSecondaryResetAt))
	s.SecondaryWindowMinutes = integer(h.Get(SignalSecondaryWindowMinutes))

	s.ActiveLimit = strings.TrimSpace(h.Get(SignalActiveLimit))
	s.CreditsBalance = strings.TrimSpace(h.Get(SignalCreditsBalance))
	s.CreditsOnly = strings.EqualFold(strings.TrimSpace(h.Get(SignalCreditsUnlimit)), "false")
	return s
}

// percent parses a 0-100 float or integer. Upstream sends "42" or "42.5".
func percent(raw string) *int {
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "%"))
	if raw == "" {
		return nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	v := int(f + 0.5)
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return &v
}

func integer(raw string) *int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	return &v
}

// timestamp parses the several shapes an epoch or RFC3339 reset time can take.
func timestamp(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		// Seconds and milliseconds are both plausible; anything this large is
		// milliseconds.
		if n > 1e12 {
			n /= 1000
		}
		at := time.Unix(n, 0).UTC()
		return &at
	}
	if at, err := time.Parse(time.RFC3339, raw); err == nil {
		return &at
	}
	return nil
}

// ExhaustedUntil reports the moment the account's spent budget is known to come
// back, or the zero time when no window is known to be spent until then.
//
// A window at 100% is the upstream saying "not until this resets", which is
// exactly the fact the probe scheduler needs and cannot obtain from a probe: an
// over-quota probe still answers 200, so nothing in its own result says the
// account is out of budget. The latest reset among the spent windows is the one
// that matters -- being under the weekly limit is no help while the 5-hour
// window is spent.
//
// Only a window with a stated *future* reset counts. One whose reset has
// already passed has recovered, and one with no reset at all gives no better
// estimate than probing does -- blocking on that would park the account
// indefinitely on the strength of a number nobody supplied.
func (s Signals) ExhaustedUntil(now time.Time) time.Time {
	var latest time.Time
	for _, w := range []struct {
		used  *int
		reset *time.Time
	}{
		{s.PrimaryUsedPercent, s.PrimaryResetAt},
		{s.SecondaryUsedPercent, s.SecondaryResetAt},
	} {
		if w.used == nil || *w.used < 100 {
			continue
		}
		if w.reset == nil || !w.reset.After(now) {
			continue
		}
		if w.reset.After(latest) {
			latest = *w.reset
		}
	}
	return latest
}
