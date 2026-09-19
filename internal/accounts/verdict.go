package accounts

import (
	"strings"
	"time"

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
)

// Verdict is the account-level answer to "may this be probed, and why".
type Verdict struct {
	Kind   VerdictKind `json:"kind"`
	Reason string      `json:"reason,omitempty"`
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
// the account is gone, switched off, or its credentials are rejected.
var blockedKinds = map[VerdictKind]bool{
	VerdictDeleted:    true,
	VerdictDisabled:   true,
	VerdictAuth:       true,
	VerdictPending:    true,
	VerdictRefreshing: true,

	// Explicit, and load-bearing: these are the cases the plugin deliberately
	// keeps probing.
	VerdictOK:       false,
	VerdictCooldown: false,
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

	// Everything left is a "come back later" condition, and probing is how the
	// plugin finds out whether later has arrived. Say which one it is, so the
	// panel explains the state instead of hiding it.
	reason := ""
	switch {
	case a.Unavailable:
		reason = strings.TrimSpace(a.StatusMessage)
		if reason == "" {
			reason = "上游暂时不可用（如已达额度上限）"
		}
	case a.NextRetryAfter != nil && now.Before(*a.NextRetryAfter):
		reason = "CPA 冷却中，可重试于 " + a.NextRetryAfter.Local().Format("15:04:05")
	case a.Status == hostapi.AccountStatusError:
		reason = strings.TrimSpace(a.StatusMessage)
		if reason == "" {
			reason = "账号处于临时错误状态"
		}
	}
	if reason != "" {
		return verdict(VerdictCooldown, reason)
	}
	return verdict(VerdictOK, "")
}

func verdict(kind VerdictKind, reason string) Verdict {
	return Verdict{Kind: kind, Reason: reason, Blocked: blockedKinds[kind]}
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
