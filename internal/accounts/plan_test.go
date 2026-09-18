package accounts

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// TestBlockedReason covers which accounts the probe scheduler skips.
//
// CPA already knows more about an account's health than the plugin can infer,
// and probing one it has already written off spends a request to relearn it --
// and in the quota case spends part of the very budget that is exhausted.
func TestBlockedReason(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	later := now.Add(30 * time.Minute)
	past := now.Add(-time.Minute)

	cases := []struct {
		name    string
		account Account
		blocked bool
	}{
		{"active account is probeable", Account{Status: hostapi.AccountStatusActive}, false},
		{"disabled flag", Account{Status: hostapi.AccountStatusActive, Disabled: true}, true},
		{"disabled status", Account{Status: hostapi.AccountStatusDisabled}, true},
		{"quota exhausted", Account{Status: hostapi.AccountStatusActive, Unavailable: true,
			StatusMessage: "account quota exhausted"}, true},
		{"cooling down", Account{Status: hostapi.AccountStatusActive, NextRetryAfter: &later}, true},
		{"cooldown elapsed", Account{Status: hostapi.AccountStatusActive, NextRetryAfter: &past}, false},
		{"error status", Account{Status: hostapi.AccountStatusError}, true},
		{"awaiting mfa", Account{Status: hostapi.AccountStatusPending}, true},
		{"refreshing", Account{Status: hostapi.AccountStatusRefreshing}, true},
		// An unknown state is not evidence of a fault; one cheap request is how
		// it becomes known.
		{"unknown status", Account{Status: hostapi.AccountStatusUnknown}, false},
		{"empty status", Account{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := BlockedReason(tc.account, now)
			if got := reason != ""; got != tc.blocked {
				t.Errorf("BlockedReason = %q, blocked = %v, want %v", reason, got, tc.blocked)
			}
		})
	}
}

// TestBlockedReasonSurfacesTheProviderMessage pins that CPA's own explanation
// reaches the operator rather than being flattened into a generic "unavailable".
func TestBlockedReasonSurfacesTheProviderMessage(t *testing.T) {
	acc := Account{
		Status:        hostapi.AccountStatusActive,
		Unavailable:   true,
		StatusMessage: "5h limit reached",
	}
	if got := BlockedReason(acc, time.Now()); !strings.Contains(got, "5h limit reached") {
		t.Errorf("BlockedReason = %q, want it to carry the provider's message", got)
	}
}
