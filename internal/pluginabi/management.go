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

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/management"
	"github.com/yangshoulai/codex-turn-state-manager/internal/version"
	"github.com/yangshoulai/codex-turn-state-manager/web"
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
// CPA dispatches management calls by exact path, so the route table lives with
// the handlers in internal/management and is declared verbatim here. Deriving
// both from one table means a new endpoint cannot be added without also being
// reachable.

func (p *Plugin) handleManagementRegister(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRegistrationRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management register: %w", err)
		}
	}

	// Management route paths are NOT given the plugin prefix automatically:
	// the host resolves them as <BasePath> + <Path>, so the plugin segment has
	// to be part of what we send. Resource routes are the opposite -- the host
	// inserts /plugins/<id> for those itself. Returning "/status" here would
	// therefore register /v0/management/status, which is unreachable and is
	// dropped without any warning on the host side.
	prefix := strings.TrimSuffix(version.ManagementBasePath, req.BasePath)
	if prefix == version.ManagementBasePath {
		// BasePath did not match what this build expects; fall back to the
		// documented layout rather than registering unreachable routes.
		prefix = "/plugins/" + version.PluginName
	}

	declared := management.Routes()
	routes := make([]pluginapi.ManagementRoute, 0, len(declared))
	for _, r := range declared {
		routes = append(routes, pluginapi.ManagementRoute{
			Method: r.Method,
			Path:   prefix + r.Path,
		})
	}

	// One route per file: the host has no static-directory or wildcard resource
	// support, and resource requests are forced to GET.
	assets := web.Assets
	resources := make([]pluginapi.ResourceRoute, 0, len(assets))
	for _, path := range assets {
		resources = append(resources, pluginapi.ResourceRoute{
			Path: path,
			Menu: resourceMenuLabel(path),
		})
	}

	p.logf(hostapi.LogInfo, "management routes registered", map[string]any{
		"basePath": req.BasePath, "prefix": prefix,
		"routes": len(routes), "resources": len(resources),
	})

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
