package pluginabi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
)

// Caller invokes a host RPC method and returns the unwrapped result.
//
// The real implementation crosses the C ABI (see abi.go); tests supply an
// in-process fake, which is what keeps every translation below testable without
// loading a shared library.
type Caller interface {
	Call(method string, payload []byte) (json.RawMessage, error)
}

// HostClient implements hostapi.Host on top of the host callbacks.
//
// It deliberately holds no state: credentials must be read live on every probe
// (NF-06), so there is nowhere here for a token to hide.
type HostClient struct {
	caller Caller
}

// NewHostClient builds a host client.
func NewHostClient(caller Caller) *HostClient {
	return &HostClient{caller: caller}
}

// ListAccounts implements hostapi.Host.
//
// CPA returns the auth pool as file entries carrying both the runtime id and
// the plugin's persistence key, which is exactly the mapping AccountRegistry
// needs. Priority is read from the entry because the host's scheduler
// candidates do not carry it consistently across provider kinds.
func (c *HostClient) ListAccounts(ctx context.Context) ([]hostapi.Account, error) {
	raw, err := c.caller.Call(pluginabi.MethodHostAuthList, []byte(`{}`))
	if err != nil {
		return nil, fmt.Errorf("host.auth.list: %w", err)
	}

	var payload struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("host.auth.list: decode: %w", err)
	}

	out := make([]hostapi.Account, 0, len(payload.Files))
	for _, f := range payload.Files {
		out = append(out, hostapi.Account{
			AuthIndex:      f.AuthIndex,
			AuthID:         f.ID,
			Provider:       strings.ToLower(strings.TrimSpace(f.Provider)),
			Label:          firstNonEmpty(f.Label, f.Email, f.Name),
			Status:         f.Status,
			StatusMessage:  f.StatusMessage,
			Priority:       f.Priority,
			Disabled:       f.Disabled,
			Unavailable:    f.Unavailable,
			NextRetryAfter: f.NextRetryAfter,
		})
	}
	return out, nil
}

// GetCredential implements hostapi.Host.
//
// The host returns the raw on-disk auth file, not a parsed credential, so the
// access token is pulled out of that document. The "access_token" key is a
// provider convention rather than a documented contract, so an unfamiliar
// layout yields an empty token and the probe fails as AUTH_ERROR rather than
// sending a request with no credential.
//
// Nothing is cached: CPA owns the token lifecycle (NF-06/NF-07).
func (c *HostClient) GetCredential(ctx context.Context, authIndex string) (hostapi.Credential, error) {
	raw, err := c.caller.Call(pluginabi.MethodHostAuthGet,
		mustJSON(pluginapi.HostAuthGetRequest{AuthIndex: authIndex}))
	if err != nil {
		return hostapi.Credential{}, fmt.Errorf("host.auth.get: %w", err)
	}

	var payload pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return hostapi.Credential{}, fmt.Errorf("host.auth.get: decode: %w", err)
	}

	var doc map[string]any
	if len(payload.JSON) > 0 {
		if err := json.Unmarshal(payload.JSON, &doc); err != nil {
			return hostapi.Credential{}, fmt.Errorf("host.auth.get: decode credential json: %w", err)
		}
	}

	cred := hostapi.Credential{Raw: doc}
	if token, ok := doc["access_token"].(string); ok {
		cred.AccessToken = token
	}
	if exp, ok := doc["expiry"].(string); ok {
		if at, err := time.Parse(time.RFC3339, exp); err == nil {
			cred.ExpiresAt = at
		}
	}
	return cred, nil
}

// Log implements hostapi.Host.
func (c *HostClient) Log(level hostapi.LogLevel, msg string, fields map[string]any) {
	payload := map[string]any{"level": string(level), "message": msg}
	if len(fields) > 0 {
		payload["fields"] = fields
	}
	// Logging must never take the plugin down, so the result is discarded.
	_, _ = c.caller.Call(pluginabi.MethodHostLog, mustJSON(payload))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// mustJSON marshals a value that is known to be marshalable.
func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		// The values here are plugin-defined structs of plain fields; a failure
		// means a programming error, and an empty object is the safe fallback.
		return []byte(`{}`)
	}
	return raw
}
