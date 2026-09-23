package pluginabi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/management"
	"github.com/yangshoulai/codex-turn-state-manager/internal/version"
	"github.com/yangshoulai/codex-turn-state-manager/web"
)

// fakeCaller stands in for the C ABI so the translation layer is testable
// without loading a shared library.
type fakeCaller struct {
	mu       sync.Mutex
	handlers map[string]func(json.RawMessage) (any, error)
	calls    []string
}

func newFakeCaller() *fakeCaller {
	return &fakeCaller{handlers: map[string]func(json.RawMessage) (any, error){}}
}

func (f *fakeCaller) on(method string, fn func(json.RawMessage) (any, error)) {
	f.handlers[method] = fn
}

func (f *fakeCaller) Call(method string, payload []byte) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	fn, ok := f.handlers[method]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fakeCaller: no handler for %s", method)
	}
	result, err := fn(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

// authListCaller answers host.auth.list with one Codex account.
func authListCaller() *fakeCaller {
	c := newFakeCaller()
	c.on(pluginabi.MethodHostAuthList, func(json.RawMessage) (any, error) {
		return map[string]any{
			"files": []pluginapi.HostAuthFileEntry{{
				ID:        "auth-id-1",
				AuthIndex: "codex-auth-1",
				Provider:  "codex",
				Label:     "user1@example.com",
				Status:    hostapi.AccountStatusActive,
				Priority:  10,
			}},
		}, nil
	})
	c.on(pluginabi.MethodHostAuthGet, func(json.RawMessage) (any, error) {
		authFile, _ := json.Marshal(map[string]any{
			"access_token": "token-abc",
			"expiry":       "2026-09-18T13:00:00Z",
		})
		return pluginapi.HostAuthGetResponse{
			AuthIndex: "codex-auth-1",
			JSON:      authFile,
		}, nil
	})
	return c
}

// newTestPlugin registers a plugin against a temp data directory.
func newTestPlugin(t *testing.T, caller *fakeCaller) *Plugin {
	t.Helper()
	p := New(caller)

	cfg, err := json.Marshal(map[string]any{
		"data_dir": filepath.Join(t.TempDir(), "data"),
	})
	// The host sends the config as raw YAML bytes inside the JSON envelope.
	cfgYAML, _ := json.Marshal(filepath.Join(t.TempDir(), "data"))
	_ = cfg

	raw, err := json.Marshal(map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"config_yaml":    []byte("data_dir: " + string(mustUnquote(cfgYAML)) + "\n"),
	})
	if err != nil {
		t.Fatalf("marshal register request: %v", err)
	}

	resp, err := p.Handle(pluginabi.MethodPluginRegister, raw)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !envelopeOK(t, resp) {
		t.Fatalf("register returned a failure envelope: %s", resp)
	}
	t.Cleanup(p.Stop)
	return p
}

func mustUnquote(raw []byte) []byte {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return raw
	}
	return []byte(s)
}

// envelopeOK reports whether a response envelope carries ok:true.
func envelopeOK(t *testing.T, raw []byte) bool {
	t.Helper()
	var env struct {
		OK    bool            `json:"ok"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope %s: %v", raw, err)
	}
	if !env.OK {
		t.Logf("failure envelope: %s", env.Error)
	}
	return env.OK
}

// resultOf unwraps an ok envelope's result.
func resultOf(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got %s", raw)
	}
	return env.Result
}

// ---------------------------------------------------------------------------
// dispatch

func TestHandle_UnknownMethodIsReported(t *testing.T) {
	p := New(newFakeCaller())

	raw, err := p.Handle("nonsense.method", nil)
	if err != nil {
		t.Fatalf("Handle returned a Go error for an unknown method: %v", err)
	}
	if envelopeOK(t, raw) {
		t.Error("an unknown method should produce a failure envelope")
	}
}

func TestHandle_BeforeRegistrationIsNotConfigured(t *testing.T) {
	p := New(newFakeCaller())

	for _, method := range []string{
		pluginabi.MethodRequestInterceptAfter,
		pluginabi.MethodResponseInterceptStreamChunk,
		pluginabi.MethodSchedulerPick,
		pluginabi.MethodManagementHandle,
	} {
		raw, err := p.Handle(method, []byte(`{}`))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if envelopeOK(t, raw) {
			t.Errorf("%s before registration should fail, not silently succeed", method)
		}
	}
}

func TestRegister_DeclaresTheCapabilitiesThePluginActuallyImplements(t *testing.T) {
	p := newTestPlugin(t, authListCaller())

	raw, err := p.Handle(pluginabi.MethodPluginRegister, []byte(`{"schema_version":6}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	var reg struct {
		SchemaVersion uint32 `json:"schema_version"`
		Metadata      struct {
			Name string `json:"Name"`
		} `json:"metadata"`
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(resultOf(t, raw), &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}

	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}

	// Every capability claimed here must have a handler in Handle; claiming one
	// without implementing it means the host calls a method that comes back
	// unknown_method.
	required := []string{
		"request_interceptor",
		"request_lifecycle_plugin",
		"response_interceptor",
		"response_stream_interceptor",
		"scheduler",
		"scheduler_across_priorities",
		"management_api",
	}
	for _, name := range required {
		if reg.Capabilities[name] != true {
			t.Errorf("capability %s = %v, want true", name, reg.Capabilities[name])
		}
	}
}

// TestRegistrationSatisfiesHostValidity pins the host's acceptance rules.
//
// CPA's validPlugin rejects a registration whose Name, Version, Author or
// GitHubRepository is blank, and it rejects it by discarding every declared
// capability -- the plugin loads, logs nothing wrong from its own side, and is
// simply never called. That failure mode is invisible without a real instance,
// so it is asserted here instead.
func TestRegistrationSatisfiesHostValidity(t *testing.T) {
	p := newTestPlugin(t, authListCaller())

	raw, err := p.Handle(pluginabi.MethodPluginRegister, []byte(`{"schema_version":6}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	var reg struct {
		Metadata struct {
			Name             string `json:"Name"`
			Version          string `json:"Version"`
			Author           string `json:"Author"`
			GitHubRepository string `json:"GitHubRepository"`
		} `json:"metadata"`
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(resultOf(t, raw), &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}

	for name, value := range map[string]string{
		"Name":             reg.Metadata.Name,
		"Version":          reg.Metadata.Version,
		"Author":           reg.Metadata.Author,
		"GitHubRepository": reg.Metadata.GitHubRepository,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("metadata %s is blank; the host will discard every capability", name)
		}
	}

	if reg.Metadata.Name != version.PluginName {
		t.Errorf("Name = %q, want %q -- it must match the library name and the plugins.configs key",
			reg.Metadata.Name, version.PluginName)
	}

	// At least one capability must be declared or the host rejects the plugin
	// outright, regardless of metadata.
	var declared int
	for _, v := range reg.Capabilities {
		if v == true {
			declared++
		}
	}
	if declared == 0 {
		t.Error("no capability declared; the host will reject the registration")
	}
}

// TestRequestInterceptBeforeIsAnswered covers a defect that only a real instance
// revealed: RequestInterceptor declares both stages, so the host calls both, and
// leaving the pre-credential stage unimplemented made it log
//
//	pluginhost: request interceptor ... failed: unsupported method: request.intercept_before
//
// on every single request. The stage does nothing by design -- no account has
// been chosen yet -- but it has to say so.
func TestRequestInterceptBeforeIsAnswered(t *testing.T) {
	p := newTestPlugin(t, authListCaller())

	raw, err := p.Handle(pluginabi.MethodRequestInterceptBefore, []byte(`{"RequestID":"req-1"}`))
	if err != nil {
		t.Fatalf("request.intercept_before: %v", err)
	}
	if !envelopeOK(t, raw) {
		t.Fatal("the pre-credential stage must be answered, not rejected")
	}

	// And it must not modify anything: there is no binding to apply yet.
	var resp pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(resultOf(t, raw), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Headers) != 0 || len(resp.ClearHeaders) != 0 || resp.Terminate {
		t.Errorf("the pre-credential stage should be a pass-through, got %+v", resp)
	}
}

func TestShutdown_IsIdempotent(t *testing.T) {
	p := newTestPlugin(t, authListCaller())
	if _, err := p.Handle(pluginabi.MethodPluginShutdown, nil); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := p.Handle(pluginabi.MethodPluginShutdown, nil); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
	if p.current() != nil {
		t.Error("the app should be released after shutdown")
	}
}

// ---------------------------------------------------------------------------
// config

func TestParseConfig(t *testing.T) {
	t.Run("empty uses the default data dir", func(t *testing.T) {
		cfg, err := parseConfig(nil)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if cfg.DataDir != DefaultDataDir {
			t.Errorf("DataDir = %q, want %q", cfg.DataDir, DefaultDataDir)
		}
	})

	t.Run("values override the defaults", func(t *testing.T) {
		cfg, err := parseConfig([]byte("data_dir: /var/lib/cts\ndata_dir_typo: x\nupstream_base_url: https://example.test\n"))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if cfg.DataDir != "/var/lib/cts" {
			t.Errorf("DataDir = %q", cfg.DataDir)
		}
		if cfg.UpstreamBaseURL != "https://example.test" {
			t.Errorf("UpstreamBaseURL = %q", cfg.UpstreamBaseURL)
		}
	})

	t.Run("blank data dir falls back", func(t *testing.T) {
		cfg, err := parseConfig([]byte("data_dir: \"   \"\n"))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if cfg.DataDir != DefaultDataDir {
			t.Errorf("DataDir = %q, want the default", cfg.DataDir)
		}
	})

	t.Run("malformed yaml is rejected", func(t *testing.T) {
		if _, err := parseConfig([]byte("data_dir: [unclosed\n")); err == nil {
			t.Error("expected a parse error")
		}
	})
}

// ---------------------------------------------------------------------------
// metadata

func TestSelectedAuth(t *testing.T) {
	cases := []struct {
		name      string
		metadata  map[string]any
		wantID    string
		wantIndex string
	}{
		{"both keys", map[string]any{
			hostapi.MetadataSelectedAuthID:    "auth-id-1",
			hostapi.MetadataSelectedAuthIndex: "codex-auth-1",
		}, "auth-id-1", "codex-auth-1"},
		{"missing entirely", nil, "", ""},
		{"wrong types", map[string]any{
			hostapi.MetadataSelectedAuthID: 42,
		}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, index := selectedAuth(tc.metadata)
			if id != tc.wantID || index != tc.wantIndex {
				t.Errorf("selectedAuth = (%q, %q), want (%q, %q)", id, index, tc.wantID, tc.wantIndex)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// host client

func TestHostClient_ListAccountsMapsTheAuthPool(t *testing.T) {
	client := NewHostClient(authListCaller())

	accounts, err := client.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}

	got := accounts[0]
	// The runtime id and the persistence key are different things and both must
	// survive the translation.
	if got.AuthID != "auth-id-1" || got.AuthIndex != "codex-auth-1" {
		t.Errorf("ids = %q/%q, want auth-id-1/codex-auth-1", got.AuthID, got.AuthIndex)
	}
	if got.Provider != hostapi.ProviderCodex {
		t.Errorf("Provider = %q, want codex", got.Provider)
	}
	if got.Priority != 10 {
		t.Errorf("Priority = %d, want 10", got.Priority)
	}
}

func TestHostClient_GetCredentialExtractsTheToken(t *testing.T) {
	client := NewHostClient(authListCaller())

	cred, err := client.GetCredential(context.Background(), "codex-auth-1")
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	if cred.AccessToken != "token-abc" {
		t.Errorf("AccessToken = %q, want token-abc", cred.AccessToken)
	}
	if cred.ExpiresAt.IsZero() {
		t.Error("ExpiresAt was not read from the credential document")
	}
	if cred.Raw == nil {
		t.Error("Raw should carry the decoded document")
	}
}

// TestHostClient_UnknownCredentialLayoutYieldsNoToken covers the documented
// risk that the access_token key is a provider convention rather than a
// contract: an unfamiliar layout must surface as a missing token, which the
// probe treats as an auth error, rather than as a request with no credential.
func TestHostClient_UnknownCredentialLayoutYieldsNoToken(t *testing.T) {
	caller := newFakeCaller()
	caller.on(pluginabi.MethodHostAuthGet, func(json.RawMessage) (any, error) {
		// Nested rather than top-level, and named differently.
		nested, _ := json.Marshal(map[string]any{
			"tokens": map[string]any{"access": "token-xyz"},
		})
		return pluginapi.HostAuthGetResponse{AuthIndex: "codex-auth-1", JSON: nested}, nil
	})

	cred, err := NewHostClient(caller).GetCredential(context.Background(), "codex-auth-1")
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	if cred.AccessToken != "" {
		t.Errorf("AccessToken = %q, want empty for an unrecognised layout", cred.AccessToken)
	}
}

func TestHostClient_LogNeverFailsTheCaller(t *testing.T) {
	// No host.log handler registered: the call errors, and logging must absorb
	// that rather than propagate it.
	NewHostClient(newFakeCaller()).Log(hostapi.LogInfo, "hello", map[string]any{"k": "v"})
}

// ---------------------------------------------------------------------------
// management bridge

// TestManagementRoutesAreExactPaths pins the host constraint that made the
// management API query-parameter based: routes are matched by exact path, and
// wildcards are rejected at registration.
func TestManagementRoutesAreExactPaths(t *testing.T) {
	declared := management.Routes()
	if len(declared) == 0 {
		t.Fatal("the route table is empty")
	}

	seen := map[string]bool{}
	for _, r := range declared {
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("duplicate route %s", key)
		}
		seen[key] = true

		if strings.ContainsAny(r.Path, ":*") {
			t.Errorf("route %s contains a wildcard, which the host rejects", key)
		}
		if !strings.HasPrefix(r.Path, "/") {
			t.Errorf("route %s must be absolute", key)
		}
	}
}

// TestWebAssetsExist guards the resource list against a rename: a path declared
// to the host that cannot be served would hand the panel a 404.
//
// It asserts resolvability through ServedAsset rather than an embedded file of
// the same name, because the two deliberately differ now: asset routes carry a
// content hash, so /app.<hash>.js is served from app.js. Checking the file name
// would pass for a route the panel cannot actually load.
func TestWebAssetsExist(t *testing.T) {
	for _, name := range web.Assets {
		body, err := web.ServedAsset(name)
		if err != nil {
			t.Errorf("declared asset %s cannot be served: %v", name, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("declared asset %s served an empty body", name)
		}
	}

	// The hashed routes must not be guessable into serving a different file:
	// the bare names are no longer routes at all.
	if _, err := web.ServedAsset("/app.js"); err == nil {
		t.Error("the bare /app.js still resolves as a route")
	}
}

func TestManagementRegister_ReturnsRoutesAndResources(t *testing.T) {
	p := newTestPlugin(t, authListCaller())

	raw, err := p.Handle(pluginabi.MethodManagementRegister,
		[]byte(`{"base_path":"/v0/management","resource_base_path":"/v0/resource/plugins/codex-turn-state-manager"}`))
	if err != nil {
		t.Fatalf("management.register: %v", err)
	}

	var resp pluginapi.ManagementRegistrationResponse
	if err := json.Unmarshal(resultOf(t, raw), &resp); err != nil {
		t.Fatalf("decode management registration: %v", err)
	}

	// The registration must declare exactly the routes the plugin serves, each
	// carrying the plugin path segment. The host resolves a management route as
	// <BasePath> + <Path> and BasePath is only "/v0/management", so a route
	// without the segment lands somewhere unreachable -- or worse, somewhere
	// that belongs to another owner -- and is dropped without a warning.
	//
	// The segment is deliberately not "plugins/<id>": see version.ManagementBasePath.
	declared := management.Routes()
	if len(resp.Routes) != len(declared) {
		t.Fatalf("routes declared = %d, want %d", len(resp.Routes), len(declared))
	}

	const wantPrefix = "/" + version.PluginName
	if strings.Contains(wantPrefix, "/plugins/") {
		t.Fatal("the management prefix must not be nested under CPA's own /plugins namespace")
	}
	byKey := map[string]bool{}
	for _, r := range declared {
		byKey[r.Method+" "+wantPrefix+r.Path] = true
	}
	for _, r := range resp.Routes {
		if !byKey[r.Method+" "+r.Path] {
			t.Errorf("registered route %s %s has no handler", r.Method, r.Path)
		}
		if !strings.HasPrefix(r.Path, wantPrefix+"/") {
			t.Errorf("route %s %s is missing the %s prefix and would be unreachable",
				r.Method, r.Path, wantPrefix)
		}
	}
	if len(resp.Resources) != len(web.Assets) {
		t.Errorf("resources = %d, want %d", len(resp.Resources), len(web.Assets))
	}

	// The panel's entry point is labelled so it appears in the management UI;
	// the asset routes must not be, or app.js shows up as a page.
	var menus int
	for _, r := range resp.Resources {
		if r.Menu != "" {
			menus++
		}
	}
	if menus != 1 {
		t.Errorf("%d resource routes carry a menu label, want exactly 1", menus)
	}
}

func TestManagementHandle_ReplaysThroughThePluginHandlers(t *testing.T) {
	p := newTestPlugin(t, authListCaller())

	body, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   version.ManagementBasePath + "/status",
	})
	raw, err := p.Handle(pluginabi.MethodManagementHandle, body)
	if err != nil {
		t.Fatalf("management.handle: %v", err)
	}

	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(resultOf(t, raw), &resp); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", resp.StatusCode, resp.Body)
	}

	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatalf("status body is not JSON: %v", err)
	}
	if payload["version"] == nil {
		t.Error("the status payload should carry a version")
	}
}

func TestManagementHandle_UnknownPathIsANotFound(t *testing.T) {
	p := newTestPlugin(t, authListCaller())

	body, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   version.ManagementBasePath + "/does-not-exist",
	})
	raw, err := p.Handle(pluginabi.MethodManagementHandle, body)
	if err != nil {
		t.Fatalf("management.handle: %v", err)
	}

	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(resultOf(t, raw), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestManagementHandle_QueryParametersSurvive(t *testing.T) {
	p := newTestPlugin(t, authListCaller())

	body, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   version.ManagementBasePath + "/probe-history",
		Query:  map[string][]string{"limit": {"5"}},
	})
	raw, err := p.Handle(pluginabi.MethodManagementHandle, body)
	if err != nil {
		t.Fatalf("management.handle: %v", err)
	}

	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(resultOf(t, raw), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", resp.StatusCode, resp.Body)
	}

	var payload struct {
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Limit != 5 {
		t.Errorf("limit = %d, want 5; query parameters were dropped in the bridge", payload.Limit)
	}
}

// TestManagementHandleServesResourcePaths covers the path that made the panel
// unreachable in a real instance.
//
// The host uses management.handle for both a plugin's management routes and its
// browser-navigable resources. Replaying a resource path through the management
// mux, which is mounted at the management base path, yields a 404 -- and the
// host reports the dispatch as successful, so the failure is invisible from the
// plugin side.
func TestManagementHandleServesResourcePaths(t *testing.T) {
	p := newTestPlugin(t, authListCaller())

	// Registration tells the plugin where its resources live.
	regBody, _ := json.Marshal(pluginapi.ManagementRegistrationRequest{
		BasePath:         version.ManagementBasePath[:len("/v0/management")],
		ResourceBasePath: version.ResourceBasePath,
	})
	if _, err := p.Handle(pluginabi.MethodManagementRegister, regBody); err != nil {
		t.Fatalf("management.register: %v", err)
	}

	for _, asset := range web.Assets {
		t.Run(asset, func(t *testing.T) {
			body, _ := json.Marshal(pluginapi.ManagementRequest{
				Method: http.MethodGet,
				Path:   version.ResourceBasePath + asset,
			})
			raw, err := p.Handle(pluginabi.MethodManagementHandle, body)
			if err != nil {
				t.Fatalf("management.handle: %v", err)
			}

			var resp pluginapi.ManagementResponse
			if err := json.Unmarshal(resultOf(t, raw), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 for resource %s", resp.StatusCode, asset)
			}
			if len(resp.Body) == 0 {
				t.Fatalf("resource %s served an empty body", asset)
			}
			if got := resp.Headers.Get("Content-Type"); got != web.ContentType(asset) {
				t.Errorf("Content-Type = %q, want %q", got, web.ContentType(asset))
			}
		})
	}

	// A path under the resource base that is not an asset is a 404, not a
	// management lookup.
	body, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   version.ResourceBasePath + "/nope.js",
	})
	raw, err := p.Handle(pluginabi.MethodManagementHandle, body)
	if err != nil {
		t.Fatalf("management.handle: %v", err)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(resultOf(t, raw), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown asset", resp.StatusCode)
	}
}

// TestServeResourceStampsThePanelVersion covers the production resource path.
//
// It had no test, which is how its first cache-busting change came to be wired
// into the development harness only: the released binary served the raw asset
// while the harness served the stamped one, and everything stayed green.
func TestServeResourceStampsThePanelVersion(t *testing.T) {
	base := "/v0/resource/plugins/" + version.PluginName

	raw, err := serveResource(base+"/index.html", base, http.MethodGet)
	if err != nil {
		t.Fatalf("serveResource: %v", err)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(resultOf(t, raw), &resp); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	served := string(resp.Body)

	// The served HTML must point at the hashed routes, not at the bare file
	// names, or a CDN will happily serve the previous release's assets again.
	for _, bare := range []string{`"app.js"`, `"style.css"`} {
		if strings.Contains(served, bare) {
			t.Errorf("index.html is served still referring to %s", bare)
		}
	}
	// Relative, because the panel is served from a subpath: a leading slash
	// would resolve to the site root.
	if !strings.Contains(served, `"app.`) || !strings.Contains(served, `"style.`) {
		t.Error("index.html does not refer to hashed asset routes")
	}
	if strings.Contains(served, `"/app.`) || strings.Contains(served, `"/style.`) {
		t.Error("index.html refers to asset routes absolutely; they would resolve outside the plugin's path")
	}

	// Every declared route must resolve, and the hashed ones must be cacheable
	// forever -- their address changes when their contents do.
	for _, route := range web.Assets {
		assetRaw, err := serveResource(base+route, base, http.MethodGet)
		if err != nil {
			t.Fatalf("serveResource(%s): %v", route, err)
		}
		var asset pluginapi.ManagementResponse
		if err := json.Unmarshal(resultOf(t, assetRaw), &asset); err != nil {
			t.Fatalf("decode %s: %v", route, err)
		}
		if asset.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", route, asset.StatusCode)
			continue
		}
		cacheControl := asset.Headers.Get("Cache-Control")
		if route == "/index.html" {
			if cacheControl != "no-cache" {
				t.Errorf("index.html Cache-Control = %q, want no-cache", cacheControl)
			}
			continue
		}
		if !strings.Contains(cacheControl, "immutable") {
			t.Errorf("%s Cache-Control = %q, want an immutable directive", route, cacheControl)
		}
	}

	// ReadAsset stays raw: the rewriting belongs to serving, not to reading.
	direct, err := web.ReadAsset("index.html")
	if err != nil {
		t.Fatalf("ReadAsset: %v", err)
	}
	if strings.Contains(string(direct), "?v=") {
		t.Error("ReadAsset returned rewritten bytes; the rewriting must live in ServedAsset")
	}
}

// TestRequestInterceptCarriesTheBodyBack is the ABI-level check for the one
// thing the timezone feature cannot work without: the host sends the payload
// and honours a replacement.
//
// The adapter is the only place CPA's types are translated, so a body dropped
// here is a body the rest of the plugin never sees -- and the failure is silent,
// because a request with no rewrite is indistinguishable from a request with
// nothing to rewrite.
func TestRequestInterceptCarriesTheBodyBack(t *testing.T) {
	p := newTestPlugin(t, authListCaller())

	// Turn timezone conversion on through the plugin's own settings surface, so
	// this test exercises the whole path rather than a shortcut.
	settingsRaw, err := json.Marshal(map[string]any{
		"timezoneConversionEnabled": true,
		"timezoneTarget":            "Asia/Tokyo",
	})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	envelope, err := json.Marshal(map[string]any{
		"Method": http.MethodPut,
		"Path":   version.ManagementBasePath + "/settings",
		"Body":   settingsRaw,
	})
	if err != nil {
		t.Fatalf("marshal management request: %v", err)
	}
	mgmtResp, err := p.Handle(pluginabi.MethodManagementHandle, envelope)
	if err != nil {
		t.Fatalf("put settings: %v", err)
	}
	var putResult struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	if err := json.Unmarshal(resultOf(t, mgmtResp), &putResult); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	if putResult.StatusCode != http.StatusOK {
		t.Fatalf("PUT /settings = %d: %s", putResult.StatusCode, putResult.Body)
	}

	body := `{"input":[{"type":"message","role":"user","content":[{"type":"input_text",` +
		`"text":"<environment_context>\n  <timezone>Asia/Shanghai</timezone>\n</environment_context>"}]}]}`
	raw, err := json.Marshal(map[string]any{
		"RequestID": "req-body",
		"Model":     "gpt-5.5",
		"Headers":   map[string][]string{},
		"Body":      []byte(body),
		"Metadata":  map[string]any{"selected_auth_index": "codex-auth-1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp, err := p.Handle(pluginabi.MethodRequestInterceptAfter, raw)
	if err != nil {
		t.Fatalf("request.intercept_after: %v", err)
	}
	var out pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(resultOf(t, resp), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(out.Body) == 0 {
		t.Fatal("no replacement body was sent back")
	}
	got := string(out.Body)
	if !strings.Contains(got, "<timezone>Asia/Tokyo</timezone>") {
		t.Errorf("zone not rewritten:\n%s", got)
	}
	if strings.Contains(got, "Asia/Shanghai") {
		t.Errorf("the original zone survived:\n%s", got)
	}
}

// TestRequestInterceptSendsNoBodyWhenNothingChanged: re-sending an identical
// payload would make the host re-read and re-translate a whole conversation on
// every request.
func TestRequestInterceptSendsNoBodyWhenNothingChanged(t *testing.T) {
	p := newTestPlugin(t, authListCaller())

	body := `{"input":[{"type":"input_text","text":"hello"}]}`
	raw, _ := json.Marshal(map[string]any{
		"RequestID": "req-plain",
		"Model":     "gpt-5.5",
		"Headers":   map[string][]string{},
		"Body":      []byte(body),
	})

	resp, err := p.Handle(pluginabi.MethodRequestInterceptAfter, raw)
	if err != nil {
		t.Fatalf("request.intercept_after: %v", err)
	}
	var out pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(resultOf(t, resp), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Body) != 0 {
		t.Errorf("sent back a body of %d bytes when nothing changed", len(out.Body))
	}
}
