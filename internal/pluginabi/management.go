package pluginabi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"

	"github.com/yangshoulai/codex-turn-state-manager/internal/version"
)

// parseConfig decodes the plugin's configuration section.
//
// The host hands it over as YAML bytes, but accepts anything from a full
// document down to a single key. A malformed document falls back to defaults
// with a warning rather than refusing to load: the plugin is an optimisation
// layer, and taking CPA down over a typo in its config would be a bad trade.
func parseConfig(raw []byte) (Config, error) {
	cfg := Config{DataDir: DefaultDataDir}
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse plugin config: %w", err)
	}
	cfg.DataDir = strings.TrimSpace(cfg.DataDir)
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir
	}
	cfg.UpstreamBaseURL = strings.TrimSpace(cfg.UpstreamBaseURL)
	return cfg, nil
}

// ---------------------------------------------------------------------------
// management API
//
// CPA dispatches management calls by exact path: routes are a map keyed by
// "METHOD /path", and paths containing ":" or "*" are rejected at registration.
// So the plugin registers a fixed set of paths and does its own sub-path work
// inside the handler -- there is no wildcard option to lean on.

// managementRoutes is the exact set of Management API routes registered with
// the host. Every entry must be a path the plugin's own mux can serve verbatim.
var managementRoutes = []struct {
	Method string
	Path   string
}{
	{http.MethodGet, "/status"},
	{http.MethodGet, "/settings"},
	{http.MethodPut, "/settings"},
	{http.MethodGet, "/time-windows"},
	{http.MethodPost, "/time-windows"},
	{http.MethodGet, "/accounts"},
	{http.MethodPost, "/accounts/sync"},
	{http.MethodGet, "/bindings"},
	{http.MethodGet, "/proxy-nodes"},
	{http.MethodPut, "/proxy-nodes"},
	{http.MethodGet, "/probe-history"},
}

// resourceRoutes are the panel's static assets. Each file needs its own route:
// the host has no static-directory or wildcard resource support.
var resourceRoutes = []string{"/index.html", "/app.js", "/style.css"}

func (p *Plugin) handleManagementRegister(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRegistrationRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management register: %w", err)
		}
	}

	// Paths are relative; the host resolves them under the base paths it
	// supplied, which match version.ManagementBasePath / ResourceBasePath.
	routes := make([]pluginapi.ManagementRoute, 0, len(managementRoutes))
	for _, r := range managementRoutes {
		routes = append(routes, pluginapi.ManagementRoute{
			Method: r.Method,
			Path:   r.Path,
		})
	}

	resources := make([]pluginapi.ResourceRoute, 0, len(resourceRoutes))
	for _, path := range resourceRoutes {
		resources = append(resources, pluginapi.ResourceRoute{
			Path: path,
			Menu: resourceMenuLabel(path),
		})
	}

	return okEnvelope(pluginapi.ManagementRegistrationResponse{
		Routes:    routes,
		Resources: resources,
	})
}

// resourceMenuLabel labels the panel entry point; asset routes get no menu
// entry so the management UI does not list app.js as a page.
func resourceMenuLabel(path string) string {
	if path == "/index.html" {
		return version.PluginName
	}
	return ""
}

// handleManagement dispatches one Management API request.
//
// The plugin's existing handlers are ordinary net/http handlers, so the request
// is replayed through them and the recorded response is handed back. That keeps
// one implementation of every endpoint rather than a second, ABI-specific copy.
func (p *Plugin) handleManagement(request []byte) ([]byte, error) {
	a := p.current()
	if a == nil {
		return unavailable()
	}

	var req pluginapi.ManagementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
	}

	// The plugin's mux is mounted at the management base path, and the host
	// sends the full path it dispatched on -- so it is passed through as-is.
	// A relative path (which the host documents but does not currently send) is
	// resolved against the base rather than silently 404ing.
	path := req.Path
	if !strings.HasPrefix(path, version.ManagementBasePath) {
		path = version.ManagementBasePath + "/" + strings.TrimPrefix(path, "/")
	}
	if len(req.Query) > 0 {
		path += "?" + req.Query.Encode()
	}

	target := req.Method
	if target == "" {
		target = http.MethodGet
	}

	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(target, path, bytes.NewReader(req.Body))
	httpReq.Header = cloneHeader(req.Headers)
	a.Handler(nil).ServeHTTP(rec, httpReq)

	result := rec.Result()
	defer result.Body.Close()

	body := rec.Body.Bytes()
	resp := pluginapi.ManagementResponse{
		StatusCode: result.StatusCode,
		Headers:    result.Header,
		Body:       body,
	}
	return okEnvelope(resp)
}

func cloneHeader(in http.Header) http.Header {
	if in == nil {
		return http.Header{}
	}
	return in.Clone()
}
