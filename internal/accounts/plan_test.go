package accounts

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
)

// jwtWithClaims builds a token shaped like a Codex id_token. The signature is
// never checked, so a placeholder is enough.
func jwtWithClaims(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + "." + enc([]byte("signature"))
}

func TestParsePlanFromCredential(t *testing.T) {
	until := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		raw       map[string]any
		wantType  string
		wantUntil *time.Time
		wantLabel string
	}{
		{
			name: "plus plan with renewal",
			raw: map[string]any{"id_token": jwtWithClaims(t, map[string]any{
				openAIAuthClaim: map[string]any{
					"chatgpt_plan_type":                 "plus",
					"chatgpt_subscription_active_until": until.Format(time.RFC3339),
				},
			})},
			wantType: "plus", wantUntil: &until, wantLabel: "Plus",
		},
		{
			name: "plan claim present but empty",
			raw: map[string]any{"id_token": jwtWithClaims(t, map[string]any{
				openAIAuthClaim: map[string]any{"chatgpt_plan_type": "  "},
			})},
			wantLabel: "",
		},
		{
			name: "no auth claim at all",
			raw: map[string]any{"id_token": jwtWithClaims(t, map[string]any{
				"sub": "user-1", "email": "a@example.com",
			})},
			wantLabel: "",
		},
		{
			// The real account that prompted this work has no plan claim, so
			// this is the path that actually ran first.
			name:      "credential without an id_token",
			raw:       map[string]any{"access_token": "not-a-jwt"},
			wantLabel: "",
		},
		{
			name:      "nil credential",
			raw:       nil,
			wantLabel: "",
		},
		{
			name:      "malformed token",
			raw:       map[string]any{"id_token": "only.two"},
			wantLabel: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParsePlanFromCredential(tc.raw)
			if got.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tc.wantType)
			}
			switch {
			case tc.wantUntil == nil && got.ActiveUntil != nil:
				t.Errorf("ActiveUntil = %v, want nil", *got.ActiveUntil)
			case tc.wantUntil != nil && got.ActiveUntil == nil:
				t.Errorf("ActiveUntil = nil, want %v", *tc.wantUntil)
			case tc.wantUntil != nil && !got.ActiveUntil.Equal(*tc.wantUntil):
				t.Errorf("ActiveUntil = %v, want %v", *got.ActiveUntil, *tc.wantUntil)
			}
			if label := got.Label(); label != tc.wantLabel {
				t.Errorf("Label() = %q, want %q", label, tc.wantLabel)
			}
		})
	}
}

// TestPlanJSONOmitsUnknownFields guards the wire shape the panel consumes: an
// account with no plan must serialise as an empty object, not as a renewal date
// in year one.
func TestPlanJSONOmitsUnknownFields(t *testing.T) {
	raw, err := json.Marshal(Plan{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != "{}" {
		t.Errorf("empty plan marshalled to %s, want {}", raw)
	}

	until := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	raw, err = json.Marshal(Plan{Type: "pro", ActiveUntil: &until})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["type"] != "pro" || back["activeUntil"] == nil {
		t.Errorf("populated plan marshalled to %s", raw)
	}
}

// TestPlanLabelKeepsUnknownValues pins that an unrecognised tier is shown as-is.
// Mapping it to something tidier would be a guess presented as a fact, which is
// the mistake the model list already made once.
func TestPlanLabelKeepsUnknownValues(t *testing.T) {
	if got := (Plan{Type: "pro-20x"}).Label(); got != "pro-20x" {
		t.Errorf("Label() = %q, want the raw value", got)
	}
	if got := (Plan{Type: "Team"}).Label(); got != "Team" {
		t.Errorf("Label() = %q, want Team", got)
	}
	if got := (Plan{}).Label(); got != "" {
		t.Errorf("an absent plan should render empty, got %q", got)
	}
	if (Plan{}).Known() {
		t.Error("an empty plan should not report as known")
	}
}

// TestParsePlanAcceptsPaddedPayload covers issuers that emit standard base64
// rather than the unpadded URL-safe form.
func TestParsePlanAcceptsPaddedPayload(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		openAIAuthClaim: map[string]any{"chatgpt_plan_type": "pro"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	enc := func(b []byte) string { return base64.URLEncoding.EncodeToString(b) }
	token := enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))

	got := ParsePlanFromCredential(map[string]any{"id_token": token})
	if got.Type != "pro" {
		t.Errorf("Type = %q, want pro", got.Type)
	}
}

// TestJudge covers which accounts the probe scheduler refuses to probe.
//
// The list of blocking states is deliberately short. A probe is one cheap
// direct request whose whole purpose is to find out what the account does right
// now, so the only states worth refusing are the ones it cannot possibly
// succeed from. Treating CPA's *temporary* conditions as blocking is what left
// an account the operator had already refreshed sitting at 已暂停探测.
func TestJudge(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	later := now.Add(30 * time.Minute)
	past := now.Add(-time.Minute)

	cases := []struct {
		name    string
		account Account
		known   bool
		blocked bool
		kind    VerdictKind
	}{
		{"active account is probeable", Account{Status: hostapi.AccountStatusActive}, true, false, VerdictOK},
		{"unknown account is gone", Account{Status: hostapi.AccountStatusActive}, false, true, VerdictDeleted},

		{"disabled flag", Account{Status: hostapi.AccountStatusActive, Disabled: true}, true, true, VerdictDisabled},
		{"disabled status", Account{Status: hostapi.AccountStatusDisabled}, true, true, VerdictDisabled},
		{"awaiting mfa", Account{Status: hostapi.AccountStatusPending}, true, true, VerdictPending},
		{"refreshing", Account{Status: hostapi.AccountStatusRefreshing}, true, true, VerdictRefreshing},

		// A rejected credential is an account-level fact no retry fixes.
		{"401 in the message", Account{Status: hostapi.AccountStatusError,
			StatusMessage: "upstream returned 401"}, true, true, VerdictAuth},
		{"403 in the message", Account{Status: hostapi.AccountStatusActive,
			StatusMessage: "403 Forbidden"}, true, true, VerdictAuth},
		{"revoked token", Account{Status: hostapi.AccountStatusError,
			StatusMessage: "token has been revoked"}, true, true, VerdictAuth},

		// A status code embedded in a longer number is not a 401.
		{"a longer number is not a 401", Account{Status: hostapi.AccountStatusError,
			StatusMessage: "request 14012 failed"}, true, false, VerdictCooldown},

		// Everything below is "come back later", and probing is how the plugin
		// finds out whether later has arrived. This is the change that unsticks
		// a rate-limited account.
		{"quota exhausted", Account{Status: hostapi.AccountStatusActive, Unavailable: true,
			StatusMessage: "account quota exhausted"}, true, false, VerdictCooldown},
		{"cooling down", Account{Status: hostapi.AccountStatusActive, Unavailable: true,
			NextRetryAfter: &later}, true, false, VerdictCooldown},
		// CPA's own selector ignores NextRetryAfter when neither flag is set
		// (availabilityBlock returns "not blocked" for !unavailable &&
		// !quotaExceeded before it looks at any timestamp), so this mirrors it
		// rather than inventing a stricter rule.
		{"a recovery time with no flag is not a cooldown", Account{
			Status: hostapi.AccountStatusActive, NextRetryAfter: &later,
		}, true, false, VerdictOK},
		{"error status with no auth marker", Account{Status: hostapi.AccountStatusError}, true, false, VerdictCooldown},
		{"429 in the message", Account{Status: hostapi.AccountStatusError,
			StatusMessage: "upstream returned 429"}, true, false, VerdictCooldown},
		{"503 in the message", Account{Status: hostapi.AccountStatusError,
			StatusMessage: "upstream returned 503"}, true, false, VerdictCooldown},

		// The flag is set and its recovery time has passed. CPA's selector
		// already treats the account as available and simply has not cleared
		// the marker, so the panel must not present it as a live fault.
		{"a cooldown whose recovery time has passed is stale, not live", Account{
			Status: hostapi.AccountStatusError, Unavailable: true,
			StatusMessage: "upstream returned 503", NextRetryAfter: &past,
		}, true, false, VerdictStale},
		{"an error status whose recovery time has passed is stale too", Account{
			Status: hostapi.AccountStatusError, NextRetryAfter: &past,
		}, true, false, VerdictStale},

		{"unknown status", Account{Status: hostapi.AccountStatusUnknown}, true, false, VerdictOK},
		{"empty status", Account{}, true, false, VerdictOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Judge(tc.account, tc.known, now)
			if got.Blocked != tc.blocked {
				t.Errorf("Blocked = %v (%q), want %v", got.Blocked, got.Reason, tc.blocked)
			}
			if got.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.kind)
			}
			// A non-OK verdict always carries a reason: the panel shows it, and
			// an empty one would render as a blank pill.
			if tc.kind != VerdictOK && got.Reason == "" {
				t.Error("reason is empty; the panel has nothing to show")
			}
		})
	}
}

// TestJudgeSurfacesTheProviderMessage pins that CPA's own explanation reaches
// the operator rather than being flattened into a generic "unavailable".
//
// It is also what makes a non-blocking cooldown legible: the account is still
// being probed, and the reason says why it may not answer.
func TestJudgeSurfacesTheProviderMessage(t *testing.T) {
	acc := Account{
		Status:        hostapi.AccountStatusActive,
		Unavailable:   true,
		StatusMessage: "5h limit reached",
	}
	got := Judge(acc, true, time.Now())
	if !strings.Contains(got.Reason, "5h limit reached") {
		t.Errorf("Reason = %q, want it to carry the provider's message", got.Reason)
	}
	if got.Blocked {
		t.Error("a quota cooldown must not stop probing")
	}
}

// TestJudge_QuotaWithAKnownResetStopsProbing is the fix for the case that cost
// an operator a whole 5h window.
//
// A probe of an over-quota account still answers 200 with a response header set,
// so nothing in the probe's own result says "stop asking" -- the plugin kept
// probing every few minutes and billed each attempt. The fact was available from
// two free sources; this pins both.
func TestJudge_QuotaWithAKnownResetStopsProbing(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(3 * time.Hour)
	pct := 100

	spent := &headers.Signals{PrimaryUsedPercent: &pct, PrimaryResetAt: &resetAt}

	cases := []struct {
		name     string
		account  Account
		wantKind VerdictKind
	}{
		{
			name: "CPA reports an exhausted window with a retry time",
			account: Account{
				Status:         hostapi.AccountStatusError,
				Unavailable:    true,
				StatusMessage:  "usage limit reached",
				NextRetryAfter: &resetAt,
			},
			wantKind: VerdictQuota,
		},
		{
			name:     "traffic reported a spent rate-limit window",
			account:  Account{Status: hostapi.AccountStatusActive, Quota: spent},
			wantKind: VerdictQuota,
		},
		{
			// A cooldown with no stated retry time is a different thing: there
			// is no better estimate than probing, so it must not block.
			name: "a quota cooldown with no retry time still probes",
			account: Account{
				Status: hostapi.AccountStatusError, Unavailable: true,
				StatusMessage: "usage limit reached",
			},
			wantKind: VerdictCooldown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Judge(tc.account, true, now)
			if got.Kind != tc.wantKind {
				t.Fatalf("Kind = %q (%q), want %q", got.Kind, got.Reason, tc.wantKind)
			}
			wantBlocked := blockedKinds[tc.wantKind]
			if got.Blocked != wantBlocked {
				t.Errorf("Blocked = %v, want %v", got.Blocked, wantBlocked)
			}
			if got.Reason == "" {
				t.Error("no reason; the panel has nothing to show")
			}
		})
	}
}

// TestJudge_AnExhaustedWindowDoesNotOutliveItsReset: the block has to clear by
// itself, or the account that a quota verdict parked would never be probed
// again.
func TestJudge_AnExhaustedWindowDoesNotOutliveItsReset(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(-time.Minute)
	pct := 100
	acc := Account{
		Status: hostapi.AccountStatusActive,
		Quota:  &headers.Signals{PrimaryUsedPercent: &pct, PrimaryResetAt: &resetAt},
	}

	if got := Judge(acc, true, now); got.Blocked {
		t.Fatalf("still blocked after the reset: %+v", got)
	}
}

// TestSummariseStatusMessage unwraps the error body CPA copies into
// status_message.
//
// For a 503 the envelope holds one usable line: code server_is_overloaded,
// message "Our servers are currently overloaded...". Pasting the raw JSON into
// the panel's notice box buries it, and the operator reads punctuation instead
// of the reason.
func TestSummariseStatusMessage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"plain text is passed through", "usage limit reached", "usage limit reached"},
		{
			name: "an openai error envelope",
			raw: `{"error":{"type":"service_unavailable_error","code":"server_is_overloaded",` +
				`"message":"Our servers are currently overloaded. Please try again later.",` +
				`"param":null},"sequence_number":2}`,
			want: "server_is_overloaded：Our servers are currently overloaded. Please try again later.",
		},
		{
			name: "error as a bare string",
			raw:  `{"error":"rate limit exceeded"}`,
			want: "rate limit exceeded",
		},
		{
			name: "top-level code and message",
			raw:  `{"code":"insufficient_quota","message":"You exceeded your current quota"}`,
			want: "insufficient_quota：You exceeded your current quota",
		},
		{
			name: "code with no message",
			raw:  `{"error":{"code":"server_error"}}`,
			want: "server_error",
		},
		{
			name: "message with no code",
			raw:  `{"error":{"message":"something went wrong"}}`,
			want: "something went wrong",
		},
		{
			// Recognised as JSON but with nothing we model: better the raw text
			// than an empty string, which would render as no reason at all.
			name: "unrecognised json falls back to the raw text",
			raw:  `{"foo":"bar","baz":1}`,
			want: `{"foo":"bar","baz":1}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summariseStatusMessage(tc.raw); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSummariseStatusMessage_BoundsWhatReachesThePanel: the field is rendered
// in a notice box, and nothing enforces that an upstream error body is short.
func TestSummariseStatusMessage_BoundsWhatReachesThePanel(t *testing.T) {
	long := strings.Repeat("x", 2000)
	got := summariseStatusMessage(long)
	if len(got) > maxStatusMessage+len("…") {
		t.Errorf("length = %d, want at most %d", len(got), maxStatusMessage)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("no ellipsis on a truncated message")
	}

	// And it must not cut a multi-byte character in half, which would render
	// as a replacement character.
	cjk := summariseStatusMessage(strings.Repeat("错", 400))
	if !utf8.ValidString(cjk) {
		t.Error("truncation produced invalid UTF-8")
	}
}

// TestJudge_CarriesTheRawMessageForATooltip: the panel shows the summary and
// keeps the original one hover away.
func TestJudge_CarriesTheRawMessageForATooltip(t *testing.T) {
	now := time.Date(2026, 9, 20, 17, 0, 0, 0, time.UTC)
	raw := `{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded."}}`
	acc := Account{
		Status:        hostapi.AccountStatusError,
		Unavailable:   true,
		StatusMessage: raw,
	}

	got := Judge(acc, true, now)
	if strings.Contains(got.Reason, "{") {
		t.Errorf("Reason still carries JSON: %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "server_is_overloaded") {
		t.Errorf("Reason = %q, want the upstream code", got.Reason)
	}
	if got.Detail != raw {
		t.Errorf("Detail = %q, want the verbatim message", got.Detail)
	}
}

// TestJudge_ExpiredCooldownSaysWhyItIsStale: the operator has to be able to
// learn the rule from the panel, because the alternative is reading it as a
// broken account -- which is what happened.
func TestJudge_ExpiredCooldownSaysWhyItIsStale(t *testing.T) {
	now := time.Date(2026, 9, 20, 17, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	acc := Account{
		Status: hostapi.AccountStatusError, Unavailable: true,
		StatusMessage: "upstream returned 503", NextRetryAfter: &past,
	}

	got := Judge(acc, true, now)
	if got.Blocked {
		t.Fatal("an expired cooldown must not block probing")
	}
	// Both halves have to be there: that the marker is a leftover, and what the
	// account was originally cooled down for. Without the second the operator
	// has to hover to learn it was a 503.
	for _, want := range []string{"已过期", "令牌刷新", "重置额度", "upstream returned 503"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("Reason = %q, want it to mention %q", got.Reason, want)
		}
	}
}

// TestJudge_StaleReasonWithoutAStatusMessage covers the account CPA cooled down
// without leaving an explanation. The reason must not end up with an empty
// parenthesis.
func TestJudge_StaleReasonWithoutAStatusMessage(t *testing.T) {
	now := time.Date(2026, 9, 20, 17, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)

	got := Judge(Account{Status: hostapi.AccountStatusError, NextRetryAfter: &past}, true, now)
	if got.Kind != VerdictStale {
		t.Fatalf("Kind = %q, want %q", got.Kind, VerdictStale)
	}
	if strings.Contains(got.Reason, "（）") || strings.Contains(got.Reason, "（，") {
		t.Errorf("Reason has an empty cause: %q", got.Reason)
	}
	if got.Detail != "" {
		t.Errorf("Detail = %q, want empty when CPA sent no message", got.Detail)
	}
}
