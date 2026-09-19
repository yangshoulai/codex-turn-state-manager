package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DefaultUpstreamBaseURL is the Codex backend the model list is read from.
//
// The plugin asks the same host the probes talk to, not CPA: CPA exposes no
// callback for an account's model catalog, and inferring one from the shared
// manifest is what made every account look identical -- a Team account and a
// Plus account have different catalogs, and the union of every tier was wrong
// for both.
const DefaultUpstreamBaseURL = "https://chatgpt.com"

// ModelsPath is the account-scoped catalog endpoint, the one the Codex client
// itself calls.
const ModelsPath = "/backend-api/codex/models"

// defaultClientVersion is sent as the client_version query parameter.
//
// Upstream uses it to decide which catalog generation to answer with. The value
// is a real Codex CLI release rather than something invented; an unrecognised
// version would get whatever the server falls back to.
const defaultClientVersion = "0.154.0"

// defaultUserAgent and defaultOriginator identify the caller the way the Codex
// client does. The endpoint is not part of a published API, so the request has
// to look like the client it is imitating.
const (
	defaultUserAgent  = "codex_cli_rs/0.154.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9"
	defaultOriginator = "codex_cli_rs"
)

// fetchTimeout bounds one model-list request.
const fetchTimeout = 20 * time.Second

// maxModelsBody bounds the response. The catalog is a few hundred kilobytes of
// model descriptors; an unexpectedly large body must not become a memory
// problem inside the host process.
const maxModelsBody = 8 << 20

// AccountFetcher reads the model list an individual account can serve.
//
// It talks directly to upstream rather than through a pool node. That is
// deliberate: this runs once per account and only when an operator asks for it,
// and routing it through the probe pool would stamp a node's last_used_at and
// consume one of its attempts for a request that is not a probe. A failure is
// not fatal -- the caller keeps the list it already has.
type AccountFetcher struct {
	baseURL string
	client  *http.Client
	version string
}

// NewAccountFetcher builds a fetcher. An empty baseURL uses the default.
func NewAccountFetcher(baseURL string) *AccountFetcher {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultUpstreamBaseURL
	}
	return &AccountFetcher{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: fetchTimeout},
		version: defaultClientVersion,
	}
}

// List returns the model ids the account offers.
//
// accountID may be empty: upstream accepts the request without it, and some
// credential layouts carry no account id at all.
func (f *AccountFetcher) List(ctx context.Context, accessToken, accountID string) ([]string, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("models: no access token for the account")
	}

	endpoint := f.baseURL + ModelsPath + "?client_version=" + f.version
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("models: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Originator", defaultOriginator)
	req.Header.Set("User-Agent", defaultUserAgent)
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models: fetch account catalog: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
	if err != nil {
		return nil, fmt.Errorf("models: read account catalog: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The status is the whole diagnosis here: 401/403 means the account
		// cannot read its own catalog, which is a fact about the account worth
		// reporting rather than retrying.
		return nil, fmt.Errorf("models: account catalog returned %d", resp.StatusCode)
	}

	list, err := ParseAccountCatalog(body)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("models: account catalog contained no models")
	}
	return list, nil
}

// ParseAccountCatalog extracts the model ids from an account catalog response.
//
// The identifier field is "slug" -- that is what the Codex client catalog uses
// and what upstream sends. "id" is accepted as a fallback because the manifest
// CPA publishes uses that spelling, and a rename in either place should degrade
// to a shorter list rather than to no list at all.
func ParseAccountCatalog(raw []byte) ([]string, error) {
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("models: decode account catalog: %w", err)
	}

	seen := map[string]bool{}
	var out []string
	for _, entry := range payload.Models {
		id := stringField(entry, "slug")
		if id == "" {
			id = stringField(entry, "id")
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
