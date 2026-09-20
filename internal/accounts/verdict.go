package accounts

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
)

// VerdictKind classifies the reason an account may or may not be probed.
//
// The vocabulary is deliberately about the *cause*, not about whether probing
// is allowed: the panel shows the kind and the operator decides what to do
// about it, and a rule change here must not silently change what the panel
// says.
type VerdictKind string

const (
	// VerdictOK means the account is a legitimate probe target.
	VerdictOK VerdictKind = "ok"
	// VerdictDeleted means CPA no longer lists the account.
	VerdictDeleted VerdictKind = "deleted"
	// VerdictDisabled means the account is switched off, either by the
	// operator or by CPA.
	VerdictDisabled VerdictKind = "disabled"
	// VerdictAuth means upstream rejected the account's credentials.
	VerdictAuth VerdictKind = "auth"
	// VerdictPending means the account is waiting on an external step.
	VerdictPending VerdictKind = "pending"
	// VerdictRefreshing means CPA is mid-refresh on the credential.
	VerdictRefreshing VerdictKind = "refreshing"
	// VerdictCooldown means CPA has the account in a temporary cooldown --
	// quota exhaustion or a rate limit. Probing continues.
	VerdictCooldown VerdictKind = "cooldown"
	// VerdictQuota means a usage window is exhausted with a known reset time,
	// so a probe is guaranteed a 429 until then. Unlike a generic cooldown,
	// this one blocks: there is nothing to discover by asking early.
	VerdictQuota VerdictKind = "quota"
	// VerdictStale means CPA is still carrying markers that no longer describe
	// the account -- a cooldown whose recovery time has passed, or an error
	// status with nothing behind it. Probing continues, and the panel says the
	// marker is leftover rather than presenting it as a live fault.
	VerdictStale VerdictKind = "stale"
)

// Verdict is the account-level answer to "may this be probed, and why".
type Verdict struct {
	Kind VerdictKind `json:"kind"`
	// Reason is the human-readable explanation. When CPA's own message is a
	// JSON error body it is summarised here rather than pasted: the envelope
	// buries the one line that matters.
	Reason string `json:"reason,omitempty"`
	// Detail is CPA's message verbatim, for a tooltip. Empty when there was
	// none, and identical to Reason when there was nothing to summarise.
	Detail string `json:"detail,omitempty"`
	// Blocked reports whether probing must stop. It is derived from Kind by
	// BlockedKinds and is carried explicitly so no caller re-derives the rule.
	Blocked bool `json:"blocked"`
}

// blockedKinds is the whole rule, in one place.
//
// The list is short on purpose. CPA reports far more account states than the
// plugin can act on, and most of them are transient conditions that a probe
// answers cheaply: a temporary cooldown, a quota window, a 5xx from upstream.
// Treating those as "do not probe" is what made an account that the operator
// had already refreshed sit at 已暂停探测 for hours.
//
// A probe is not a user request. It is one cheap, direct call whose entire
// purpose is to find out what the account does right now -- so the only states
// worth refusing to probe are the ones a probe cannot possibly succeed from:
// the account is gone, switched off, its credentials are rejected, or a usage
// window is exhausted until a moment CPA can name.
var blockedKinds = map[VerdictKind]bool{
	VerdictDeleted:    true,
	VerdictDisabled:   true,
	VerdictAuth:       true,
	VerdictPending:    true,
	VerdictRefreshing: true,
	// See the quota branch in Judge.
	VerdictQuota: true,

	// Explicit, and load-bearing: these are the cases the plugin deliberately
	// keeps probing.
	VerdictOK:       false,
	VerdictCooldown: false,
	VerdictStale:    false,
}

// quotaMarkers are the substrings in CPA's own status message that identify a
// usage-window rejection, as opposed to some other transient error. Matched
// case-insensitively.
var quotaMarkers = []string{
	"usage limit",
	"usage_limit",
	"quota",
	"rate limit",
	"limit reached",
}

// authFailureMarkers are the substrings in CPA's own status message that mean
// the credentials were rejected rather than the account being busy.
//
// Matched case-insensitively. The status codes are checked as whole tokens so a
// stray "403" inside a longer number cannot match.
var authFailureMarkers = []string{
	"unauthorized",
	"forbidden",
	"invalid_token",
	"invalid token",
	"invalid_grant",
	"revoked",
	"deactivated",
	"account deleted",
	"token has expired",
	"expired token",
}

// authFailureCodes are the HTTP statuses that mean "these credentials are not
// accepted", as opposed to "come back later".
var authFailureCodes = []string{"401", "403"}

// Judge decides whether an account may be probed.
//
// `known` is false when CPA no longer lists the account at all. `now` is passed
// in rather than read so the decision is testable at a boundary.
func Judge(a Account, known bool, now time.Time) Verdict {
	if !known {
		return verdict(VerdictDeleted, "CPA 的账号池中已没有该账号")
	}
	if a.Disabled {
		return verdict(VerdictDisabled, "账号已禁用")
	}
	switch a.Status {
	case hostapi.AccountStatusDisabled:
		return verdict(VerdictDisabled, "账号已禁用")
	case hostapi.AccountStatusPending:
		return verdict(VerdictPending, "账号等待外部操作（如 MFA）")
	case hostapi.AccountStatusRefreshing:
		return verdict(VerdictRefreshing, "账号正在刷新凭证")
	}

	// A rejected credential is an account-level fact, not a transient one: no
	// amount of retrying fixes it, and every probe spends a request to learn
	// what CPA already knows.
	if code := authFailureCode(a.StatusMessage); code != "" {
		msg := strings.TrimSpace(a.StatusMessage)
		if msg == "" {
			msg = "上游返回 " + code
		}
		return verdict(VerdictAuth, "账号凭证被上游拒绝："+msg)
	}

	// A usage window that is exhausted until a known moment is the one
	// "come back later" that is not worth probing.
	//
	// This is the case that cost an operator a whole 5h window: a probe returns
	// 200 with a response header set even when the account is over its limit,
	// so nothing in the probe's own result says "stop asking". Two sources name
	// the fact, and both are free. CPA reports it as a cooldown with a stated
	// retry time; ordinary traffic reports it as a rate-limit window at 100%,
	// which CPA never exposes to plugins.
	//
	// A cooldown with NO stated retry time is left to the cooldown branch
	// below, because then there is no better estimate than probing.
	if a.NextRetryAfter != nil && now.Before(*a.NextRetryAfter) &&
		matchesAny(a.StatusMessage, quotaMarkers) {
		return verdict(VerdictQuota,
			"额度已用尽："+strings.TrimSpace(a.StatusMessage)+
				"，将在 "+a.NextRetryAfter.Local().Format("15:04:05")+" 恢复")
	}
	if a.Quota != nil {
		if until := a.Quota.ExhaustedUntil(now); until.After(now) {
			return verdict(VerdictQuota,
				"上游报告的额度窗口已用尽，恢复于 "+until.Local().Format("15:04:05"))
		}
	}

	// Everything left is a "come back later" condition, and probing is how the
	// plugin finds out whether later has arrived.
	//
	// The distinction that matters here is between a cooldown that is still
	// running and a marker CPA simply has not cleared. CPA never resets
	// Unavailable/Status by itself -- the flags are set once and stay set --
	// and it never consults them alone: its own selector decides availability
	// with availabilityBlock(unavailable, quotaExceeded, nextRetryAfter,
	// nextRecoverAt, now), a pure function of the flags and the clock, where a
	// flag with all recovery times in the past means AVAILABLE
	// (sdk/cliproxy/auth/selector.go).
	//
	// That is why CPA's own panel shows nothing for an account whose cooldown
	// expired while the plugin showed "error": the plugin was reading the raw
	// flag, and CPA was deriving. The rule below is CPA's, mirrored so the two
	// agree.
	//
	// One deliberate divergence: CPA treats "flagged with no recovery time at
	// all" as blocked, and this does not. CPA's rule answers "should a user's
	// request be routed here"; this one answers "should we probe". A probe is
	// one cheap request and is the only thing that makes the state known, so
	// refusing it on a marker with no deadline would park the account on the
	// strength of a number nobody supplied.
	recovery := a.NextRetryAfter
	flagged := a.Unavailable || a.Status == hostapi.AccountStatusError

	switch {
	case flagged && recovery != nil && now.Before(*recovery):
		return verdictWithDetail(VerdictCooldown,
			"CPA 冷却中，可重试于 "+recovery.Local().Format("15:04:05"),
			strings.TrimSpace(a.StatusMessage))

	case flagged && recovery == nil:
		msg := strings.TrimSpace(a.StatusMessage)
		if msg == "" {
			msg = "CPA 未给出恢复时间"
		}
		return verdictWithDetail(VerdictCooldown,
			"账号暂时不可用（CPA 未给出恢复时间）："+summariseStatusMessage(msg),
			msg)

	case flagged:
		// The flag is set and its recovery time has passed. CPA's selector
		// already treats this account as available -- it just has not cleared
		// the marker, and will not until a successful token refresh or an
		// explicit quota reset.
		//
		// The reason names both halves on purpose: what the account was cooled
		// down for (otherwise the operator has to hover to find out it was a
		// 503) and the fact that the marker is now a leftover.
		detail := strings.TrimSpace(a.StatusMessage)
		at := recovery.Local().Format("15:04:05")
		cause := ""
		if summary := summariseStatusMessage(detail); summary != "" {
			cause = "（" + summary + "，恢复时间 " + at + " 已过）"
		} else {
			cause = "（恢复时间 " + at + " 已过）"
		}
		return verdictWithDetail(VerdictStale,
			"CPA 的冷却标记已过期"+cause+
				"：CPA 的选择器已把这个账号视为可用，插件仍在探测；"+
				"该标记要等一次成功的令牌刷新，或在 CPA 里手动重置额度，才会被清掉",
			detail)
	}
	return verdict(VerdictOK, "")
}

// summariseStatusMessage renders CPA's status message for the panel.
//
// CPA copies the upstream error body into this field verbatim, so what arrives
// is often a single line of JSON -- for a 503, the envelope buries the one part
// that says anything:
//
//	{"error":{"type":"service_unavailable_error","code":"server_is_overloaded",
//	 "message":"Our servers are currently overloaded. Please try again later."}}
//
// Recognising that shape and pulling out the code and message is the difference
// between the operator reading "server_is_overloaded：Our servers are currently
// overloaded" and reading a wall of punctuation. Anything unrecognised is
// returned as-is, only bounded in length.
func summariseStatusMessage(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Only pay for parsing when it looks like JSON.
	if strings.HasPrefix(raw, "{") {
		if summary := summariseJSONStatus(raw); summary != "" {
			return summary
		}
	}
	return truncateMessage(raw)
}

type apiErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
	// Envelope fields some endpoints use instead of "error".
	Code string `json:"code"`
	Type string `json:"type"`
}

func summariseJSONStatus(raw string) string {
	var body apiErrorBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		// "error" as a bare string, or any shape we do not model. Fall back
		// rather than guessing at the structure.
		var loose map[string]any
		if err2 := json.Unmarshal([]byte(raw), &loose); err2 != nil {
			return ""
		}
		if s, ok := loose["error"].(string); ok && strings.TrimSpace(s) != "" {
			return truncateMessage(strings.TrimSpace(s))
		}
		return ""
	}

	label := firstNonEmpty(body.Error.Code, body.Error.Type, body.Code, body.Type)
	message := firstNonEmpty(body.Error.Message, body.Message, body.Detail)

	switch {
	case label != "" && message != "":
		return truncateMessage(label + "：" + message)
	case label != "":
		return truncateMessage(label)
	case message != "":
		return truncateMessage(message)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// maxStatusMessage bounds what reaches the panel. An upstream error body is
// usually one line, but nothing enforces that and the field is rendered in a
// notice box.
const maxStatusMessage = 300

func truncateMessage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxStatusMessage {
		return s
	}
	// Cut on a rune boundary: the message may be CJK, and half a rune renders
	// as a replacement character.
	cut := maxStatusMessage
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// matchesAny reports whether text contains any of the markers, ignoring case.
func matchesAny(text string, markers []string) bool {
	lower := strings.ToLower(text)
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func verdict(kind VerdictKind, reason string) Verdict {
	return Verdict{Kind: kind, Reason: reason, Blocked: blockedKinds[kind]}
}

// verdictWithDetail is verdict plus the untruncated source text.
func verdictWithDetail(kind VerdictKind, reason, detail string) Verdict {
	v := verdict(kind, reason)
	if detail != reason {
		v.Detail = detail
	}
	return v
}

// authFailureCode reports which rejected-credential signal a status message
// carries, or "" when it carries none.
func authFailureCode(message string) string {
	text := strings.ToLower(strings.TrimSpace(message))
	if text == "" {
		return ""
	}
	for _, code := range authFailureCodes {
		if containsToken(text, code) {
			return code
		}
	}
	for _, marker := range authFailureMarkers {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	return ""
}

// containsToken reports whether text contains word as a standalone token.
//
// A plain substring search would match "403" inside "1403" or "4032", and the
// status message is free-form text CPA assembled from an upstream error body.
func containsToken(text, word string) bool {
	for start := 0; ; {
		i := strings.Index(text[start:], word)
		if i < 0 {
			return false
		}
		i += start
		before := byte(' ')
		if i > 0 {
			before = text[i-1]
		}
		after := byte(' ')
		if end := i + len(word); end < len(text) {
			after = text[end]
		}
		if !isDigit(before) && !isDigit(after) {
			return true
		}
		start = i + len(word)
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
