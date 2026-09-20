package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/models"
)

// DefaultUpstreamBaseURL is the Codex backend the probes talk to.
//
// Probes deliberately bypass CPA's /v1/responses path: CPA's host HTTP API
// cannot carry a per-request proxy, and the whole point of probing is to route
// through the pool (design doc 3.6).
const DefaultUpstreamBaseURL = "https://chatgpt.com"

// ProbePath is the upstream endpoint.
const ProbePath = "/backend-api/codex/responses"

// probeRequest is the minimal payload that yields response headers.
//
// Field choices are load-bearing (F-14): an empty tool list and a one-character
// input keep the turn as cheap as possible, and the reasoning effort is the
// cheapest level the model accepts.
type probeRequest struct {
	Model     string         `json:"model"`
	Stream    bool           `json:"stream"`
	Store     bool           `json:"store"`
	Tools     []any          `json:"tools"`
	Input     []probeInput   `json:"input"`
	Reasoning probeReasoning `json:"reasoning"`
	// MaxOutputTokens bounds what upstream generates for the turn. A probe's
	// only output is the response headers, read before the body is abandoned --
	// but upstream bills the generation whether anyone reads it, and a
	// reasoning model answering "." can spend more on thinking than the whole
	// rest of the request costs. Zero omits the field entirely, which is how a
	// deployment opts out if a model rejects the cap.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

type probeInput struct {
	Role    string         `json:"role"`
	Content []probeContent `json:"content"`
}

type probeContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type probeReasoning struct {
	Effort string `json:"effort"`
}

// buildProbeBody renders the probe payload for a model. maxOutputTokens <= 0
// leaves the cap out of the request.
func buildProbeBody(model string, effort models.ReasoningEffort, maxOutputTokens int) ([]byte, error) {
	body := probeRequest{
		Model:  model,
		Stream: true,
		Store:  false,
		// The Responses API documents 16 as the minimum it accepts.
		MaxOutputTokens: maxOutputTokens,
		// Never nil: an omitted "tools" key makes some upstream versions fall
		// back to a default toolset.
		Tools: []any{},
		Input: []probeInput{{
			Role: "user",
			Content: []probeContent{{
				Type: "input_text",
				Text: ".",
			}},
		}},
		Reasoning: probeReasoning{Effort: string(effort)},
	}
	return json.Marshal(body)
}

// newProbeHTTPRequest builds the outbound probe request.
func newProbeHTTPRequest(ctx context.Context, baseURL, accessToken, model string, effort models.ReasoningEffort, maxOutputTokens int) (*http.Request, error) {
	body, err := buildProbeBody(model, effort, maxOutputTokens)
	if err != nil {
		return nil, fmt.Errorf("probe: build body: %w", err)
	}

	endpoint := baseURL + ProbePath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("probe: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return req, nil
}

// newProxyClient builds an HTTP client whose transport egresses through
// proxyURL. Go's transport understands http, https and socks5 proxy schemes.
func newProxyClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("probe: parse proxy url %q: %w", proxyURL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("probe: proxy url %q must include a scheme and host", proxyURL)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{
		Transport: transport,
		// No client-level Timeout: the traversal owns the deadline via the
		// request context, so a slow-but-progressing probe is not cut short.
	}, nil
}

// closeBody abandons a response body without waiting for it.
//
// Deliberately no drain: the response is usually a live SSE stream, and
// reading even a bounded prefix would block until upstream sent those bytes or
// closed the stream -- holding a probe concurrency slot for the whole turn.
// Design doc 3.6 requires reading the headers and then closing immediately, so
// the connection is sacrificed rather than reused.
func closeBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_ = resp.Body.Close()
}
