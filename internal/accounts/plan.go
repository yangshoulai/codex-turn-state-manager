package accounts

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Plan is the subscription tier attached to a Codex account.
//
// The value comes from the id_token's chatgpt_plan_type claim, which is the
// same source CLIProxyAPI reads. It is not always present: an account whose
// credentials predate the claim, or whose provider omits it, simply has no
// plan, and the panel shows nothing rather than guessing.
type Plan struct {
	// Type is the raw claim value, e.g. "plus", "pro", "team".
	Type string `json:"type,omitempty"`
	// ActiveUntil is the subscription renewal timestamp when the token carries
	// one.
	//
	// A pointer rather than a time.Time: encoding/json's omitempty does not
	// recognise time.Time's zero value, so a plain field serialised as
	// "0001-01-01T00:00:00Z" and the panel would have shown a renewal date in
	// year 1.
	ActiveUntil *time.Time `json:"activeUntil,omitempty"`
}

// Known reports whether a plan value was found.
func (p Plan) Known() bool { return strings.TrimSpace(p.Type) != "" }

// Label renders the plan for display.
//
// Unknown values are shown verbatim rather than mapped to something tidier:
// inventing a taxonomy for values this code has not seen would be a guess
// presented as a fact, which is the mistake the model list already made once.
func (p Plan) Label() string {
	if !p.Known() {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(p.Type)) {
	case "free":
		return "Free"
	case "plus":
		return "Plus"
	case "pro":
		return "Pro"
	case "team":
		return "Team"
	case "business":
		return "Business"
	case "enterprise":
		return "Enterprise"
	case "k12":
		return "K12"
	default:
		return p.Type
	}
}

// openAIAuthClaim is the namespaced claim holding ChatGPT account metadata.
const openAIAuthClaim = "https://api.openai.com/auth"

// ParsePlanFromCredential extracts the plan from a Codex auth document.
//
// The document is the raw auth file. Only the id_token's claims are read; the
// token itself is never retained, logged, or persisted, and the caller must not
// cache the document it came from (NF-06).
func ParsePlanFromCredential(raw map[string]any) Plan {
	if raw == nil {
		return Plan{}
	}
	token, _ := raw["id_token"].(string)
	if token == "" {
		return Plan{}
	}
	claims, err := decodeJWTPayload(token)
	if err != nil {
		return Plan{}
	}

	info, ok := claims[openAIAuthClaim].(map[string]any)
	if !ok {
		return Plan{}
	}

	plan := Plan{}
	if v, ok := info["chatgpt_plan_type"].(string); ok {
		plan.Type = strings.TrimSpace(v)
	}
	if v, ok := info["chatgpt_subscription_active_until"].(string); ok {
		if at, err := time.Parse(time.RFC3339, v); err == nil {
			plan.ActiveUntil = &at
		}
	}
	return plan
}

// decodeJWTPayload returns a JWT's claims without verifying the signature.
//
// Verification is deliberately skipped and is not this code's job: the token
// was issued to CPA and already validated when it was stored. We are reading a
// display attribute, not making a trust decision on it.
func decodeJWTPayload(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errNotJWT
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Tolerate padded encodings some issuers emit.
		payload, err = base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4))
		if err != nil {
			return nil, err
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

type jwtError string

func (e jwtError) Error() string { return string(e) }

const errNotJWT = jwtError("accounts: token is not a JWT")

// ParseAccountIDFromCredential extracts the ChatGPT account id a Codex request
// has to name.
//
// The upstream model catalog is account-scoped and expects it in a
// Chatgpt-Account-Id header. The auth document carries it as a top-level
// "account_id"; the id_token's claim is the fallback, because the two are
// written by different code paths in CPA and only one of them is always
// present. An account whose id cannot be found still works -- the header is
// optional -- so this returns "" rather than an error.
func ParseAccountIDFromCredential(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	if id, ok := raw["account_id"].(string); ok {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			return trimmed
		}
	}
	token, _ := raw["id_token"].(string)
	if token == "" {
		return ""
	}
	claims, err := decodeJWTPayload(token)
	if err != nil {
		return ""
	}
	info, ok := claims[openAIAuthClaim].(map[string]any)
	if !ok {
		return ""
	}
	if id, ok := info["chatgpt_account_id"].(string); ok {
		return strings.TrimSpace(id)
	}
	return ""
}
