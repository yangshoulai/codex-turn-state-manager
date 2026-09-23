// Package pluginabi adapts the CPA C ABI onto the plugin's domain.
//
// This is the only package that may reference CGO or a CPA SDK type. Everything
// the rest of the plugin needs from the host is declared in internal/hostapi,
// and the translation between the two lives here.
//
// The protocol is JSON-RPC over C function pointers: the host calls
// cliproxyPluginCall(method, requestJSON, responseBuffer) and the plugin may
// call back through the host API it was handed at init. The wire structs come
// from the CPA SDK itself rather than being re-declared here, so a field rename
// upstream is a compile error instead of a silent runtime mismatch.
package pluginabi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/yangshoulai/codex-turn-state-manager/internal/app"
	"github.com/yangshoulai/codex-turn-state-manager/internal/headers"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/intercept"
	"github.com/yangshoulai/codex-turn-state-manager/internal/version"
)

// Config is the plugin's own configuration section, as supplied by the host at
// register time as YAML.
type Config struct {
	// DataDir holds state.db and backups/. Relative paths resolve against the
	// host process working directory.
	DataDir string `yaml:"data_dir"`
	// UpstreamBaseURL overrides the Codex backend host.
	UpstreamBaseURL string `yaml:"upstream_base_url"`
}

// DefaultDataDir mirrors the design document's layout.
const DefaultDataDir = "plugin-data/codex-turn-state-manager"

// Plugin is the ABI-side singleton.
type Plugin struct {
	mu  sync.Mutex
	cfg Config
	app *app.App
	// host is the host-callback facade handed to the domain.
	host *HostClient
	// caller is retained so a reconfigure can rebuild the host facade.
	caller Caller
	// resourceBase is the browser-navigable resource prefix the host handed
	// over at registration, used to tell resource requests from management ones.
	resourceBase string

	logf func(hostapi.LogLevel, string, map[string]any)
}

// New builds an unconfigured plugin.
func New(caller Caller) *Plugin {
	p := &Plugin{caller: caller, host: NewHostClient(caller)}
	p.logf = func(level hostapi.LogLevel, msg string, fields map[string]any) {
		p.host.Log(level, msg, fields)
	}
	return p
}

// Handle dispatches one host RPC call and returns the response payload.
func (p *Plugin) Handle(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return p.handleRegister(request)

	case pluginabi.MethodPluginQuiesce:
		// The host is asking the plugin to stop doing background work while it
		// drains. Probes stop; nothing else changes.
		return okEnvelope(struct{}{})

	case pluginabi.MethodPluginShutdown:
		p.Stop()
		return okEnvelope(struct{}{})

	case pluginabi.MethodRequestInterceptBefore:
		return p.handleRequestInterceptBefore(request)

	case pluginabi.MethodRequestInterceptAfter:
		return p.handleRequestIntercept(request)

	case pluginabi.MethodResponseInterceptAfter:
		return p.handleResponseIntercept(request)

	case pluginabi.MethodResponseInterceptStreamChunk:
		return p.handleStreamChunk(request)

	case pluginabi.MethodRequestComplete:
		return p.handleCompletion(request)

	case pluginabi.MethodSchedulerPick:
		return p.handleSchedulerPick(request)

	case pluginabi.MethodManagementRegister:
		return p.handleManagementRegister(request)

	case pluginabi.MethodManagementHandle:
		return p.handleManagement(request)

	default:
		return errorEnvelope("unknown_method", "unsupported method: "+method), nil
	}
}

// resourceBasePath returns the resource prefix, falling back to the documented
// layout if registration has not supplied one.
func (p *Plugin) resourceBasePath() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resourceBase != "" {
		return p.resourceBase
	}
	return version.ResourceBasePath
}

// Stop shuts the plugin down. Safe to call more than once.
func (p *Plugin) Stop() {
	p.mu.Lock()
	a := p.app
	p.app = nil
	p.mu.Unlock()
	if a != nil {
		a.Stop()
	}
}

// ---------------------------------------------------------------------------
// registration

func (p *Plugin) handleRegister(request []byte) ([]byte, error) {
	var req struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode register request: %w", err)
		}
	}

	cfg, err := parseConfig(req.ConfigYAML)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// A reconfigure may arrive for an already-running plugin; rebuild cleanly
	// rather than mutating a live app's configuration underneath it.
	if p.app != nil {
		p.app.Stop()
		p.app = nil
	}

	// Bounds construction only (database open and migration). The plugin's
	// lifetime is not tied to this call -- see App.Start.
	bootCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	application, err := app.New(bootCtx, app.Config{
		DataDir:         cfg.DataDir,
		UpstreamBaseURL: cfg.UpstreamBaseURL,
		Host:            p.host,
		Log:             p.logf,
	})
	if err != nil {
		return nil, fmt.Errorf("start plugin: %w", err)
	}
	p.cfg = cfg
	p.app = application
	application.Start()

	p.logf(hostapi.LogInfo, "plugin registered", map[string]any{
		"dataDir": cfg.DataDir, "schemaVersion": req.SchemaVersion,
	})
	return okEnvelope(p.registration())
}

// registration declares the plugin's capabilities.
//
// SchedulerAcrossPriorities is declared unconditionally. The host decides what
// candidate list to send at registration time, but whether the plugin actually
// reaches across priority tiers is a runtime decision driven by the configured
// strategy and the state-priority switch -- so the plugin opts in up front and
// applies the tier policy itself.
func (p *Plugin) registration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:    version.PluginName,
			Version: version.Version,
			// Author and GitHubRepository are not decorative: the host's
			// validPlugin rejects a registration whose Name, Version, Author or
			// GitHubRepository is empty, and then silently ignores every
			// declared capability. A blank Author cost one debugging round
			// against a real instance, so it is covered by a test now.
			Author:           version.Author,
			GitHubRepository: version.Repository,
			Logo:             version.Logo,
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "data_dir",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Directory for state.db and backups. Defaults to " + DefaultDataDir + ".",
				},
				{
					Name:        "upstream_base_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Override the Codex upstream host used for probes.",
				},
			},
		},
		Capabilities: capabilities{
			RequestInterceptor:        true,
			RequestLifecyclePlugin:    true,
			ResponseInterceptor:       true,
			StreamChunkInterceptor:    true,
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			ManagementAPI:             true,
		},
	}
}

// registration mirrors the host's expected reply shape, which uses snake_case
// keys unlike the interceptor payloads.
type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	RequestInterceptor        bool `json:"request_interceptor"`
	RequestLifecyclePlugin    bool `json:"request_lifecycle_plugin"`
	ResponseInterceptor       bool `json:"response_interceptor"`
	StreamChunkInterceptor    bool `json:"response_stream_interceptor"`
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities,omitempty"`
	ManagementAPI             bool `json:"management_api"`
}

// ---------------------------------------------------------------------------
// envelopes

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, err := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

// unavailable reports that a request arrived before the plugin was configured.
// It is an error rather than a silent success so the host sees a misconfiguration.
func unavailable() ([]byte, error) {
	return errorEnvelope("not_configured", "plugin has not completed registration"), nil
}

// ---------------------------------------------------------------------------
// request / response interception

// handleRequestInterceptBefore answers the pre-credential stage.
//
// It does nothing on purpose: no account has been chosen yet, so there is no
// binding to look up and no state to inject. The stage still has to be answered
// because the host calls it whenever RequestInterceptor is declared, and
// leaving it unimplemented made the host log a failure on every request.
func (p *Plugin) handleRequestInterceptBefore(request []byte) ([]byte, error) {
	if p.current() == nil {
		return unavailable()
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

// sameBody reports whether two payloads are the same bytes.
//
// Pointer equality first: the interceptor returns the body it was handed when
// it changed nothing, and that is the overwhelmingly common case.
func sameBody(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	if &a[0] == &b[0] {
		return true
	}
	return bytes.Equal(a, b)
}

func (p *Plugin) handleRequestIntercept(request []byte) ([]byte, error) {
	a := p.current()
	if a == nil {
		return unavailable()
	}

	var req pluginapi.RequestInterceptRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode request intercept: %w", err)
		}
	}

	// The host publishes the selected account in Metadata rather than as a
	// field. A missing key degrades to "account unknown", never to a failure.
	authID, authIndex := selectedAuth(req.Metadata)

	// Work on a copy so the host's own map is never mutated, and so the value
	// the injector wrote can be read straight back out.
	hdrs := req.Headers.Clone()
	if hdrs == nil {
		hdrs = http.Header{}
	}

	// The request goes in as a pointer, and the body is read back off it
	// afterwards -- not from a local copy.
	//
	// Headers are a map, so mutating one propagates through the value the
	// interceptor was handed. A slice is not: assigning a new one to a struct
	// field changes that field, and a caller holding the old local still holds
	// the old bytes. Reading the field back is what makes the replacement
	// reach the host, and getting this wrong is silent -- the request simply
	// goes out unmodified.
	ireq := &hostapi.InterceptedRequest{
		Stage:     hostapi.StageAfterAuth,
		RequestID: req.RequestID,
		TraceID:   req.TraceID,
		Model:     req.Model,
		Stream:    req.Stream,
		AuthID:    authID,
		AuthIndex: authIndex,
		Headers:   hdrs,
		Body:      req.Body,
	}
	decision := a.InjectState(ireq)

	// Only send back the header this plugin owns, and only when it was set.
	// Returning the whole header map would echo the request back at the host.
	resp := pluginapi.RequestInterceptResponse{}
	if decision.Action == intercept.ActionInjected {
		resp.Headers = http.Header{headers.TurnState: []string{hdrs.Get(headers.TurnState)}}
	}
	// A body is sent back only when it actually differs. Re-sending an identical
	// payload would make the host re-read and re-translate a conversation for
	// nothing, on every request.
	if len(ireq.Body) > 0 && !sameBody(ireq.Body, req.Body) {
		resp.Body = ireq.Body
	}
	return okEnvelope(resp)
}

func (p *Plugin) handleResponseIntercept(request []byte) ([]byte, error) {
	a := p.current()
	if a == nil {
		return unavailable()
	}

	var req pluginapi.ResponseInterceptRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode response intercept: %w", err)
		}
	}

	_, authIndex := selectedAuth(req.Metadata)
	a.ObserveResponse(context.Background(), hostapi.StreamChunk{
		RequestID:       req.RequestID,
		Model:           req.Model,
		AuthIndex:       authIndex,
		ResponseHeaders: req.ResponseHeaders,
		StatusCode:      req.StatusCode,
		Body:            req.Body,
	})
	// The plugin observes but never rewrites responses.
	return okEnvelope(pluginapi.ResponseInterceptResponse{})
}

func (p *Plugin) handleStreamChunk(request []byte) ([]byte, error) {
	a := p.current()
	if a == nil {
		return unavailable()
	}

	var req pluginapi.StreamChunkInterceptRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode stream chunk: %w", err)
		}
	}

	_, authIndex := selectedAuth(req.Metadata)
	a.ObserveStreamChunk(context.Background(), hostapi.StreamChunk{
		RequestID:       req.RequestID,
		Model:           req.Model,
		AuthIndex:       authIndex,
		ChunkIndex:      req.ChunkIndex,
		ResponseHeaders: req.ResponseHeaders,
	})
	// Dropping or rewriting chunks is not this plugin's business.
	return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
}

func (p *Plugin) handleCompletion(request []byte) ([]byte, error) {
	a := p.current()
	if a == nil {
		return unavailable()
	}

	var req pluginapi.RequestCompletion
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode completion: %w", err)
		}
	}

	a.ObserveCompletion(context.Background(), hostapi.Completion{
		RequestID:  req.RequestID,
		Model:      req.Model,
		Stream:     req.Stream,
		Outcome:    hostapi.CompletionOutcome(req.Outcome),
		StatusCode: req.StatusCode,
	}, nil)
	return okEnvelope(struct{}{})
}

// ---------------------------------------------------------------------------
// scheduling

func (p *Plugin) handleSchedulerPick(request []byte) ([]byte, error) {
	a := p.current()
	if a == nil {
		return unavailable()
	}

	var req pluginapi.SchedulerPickRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode scheduler pick: %w", err)
		}
	}

	candidates := make([]hostapi.Candidate, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		candidates = append(candidates, hostapi.Candidate{
			ID:         c.ID,
			Provider:   c.Provider,
			Priority:   c.Priority,
			Status:     c.Status,
			Attributes: c.Attributes,
		})
	}

	decision := a.PickCredential(context.Background(), hostapi.SchedulerPickRequest{
		RequestID:  stringFromMetadata(req.Options.Metadata, "request_id"),
		Provider:   req.Provider,
		Providers:  req.Providers,
		Model:      req.Model,
		Stream:     req.Stream,
		Headers:    req.Options.Headers,
		Metadata:   req.Options.Metadata,
		Candidates: candidates,
	})

	// Handled=false is what makes the host fall back to its built-in scheduler.
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:          decision.AuthID,
		Handled:         decision.Handled,
		DelegateBuiltin: decision.DelegateBuiltin,
	})
}

// ---------------------------------------------------------------------------
// helpers

func (p *Plugin) current() *app.App {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.app
}

// stringFromMetadata reads a plain string out of a metadata map.
func stringFromMetadata(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	v, _ := metadata[key].(string)
	return v
}

// selectedAuth pulls the chosen account out of the host's metadata snapshot.
func selectedAuth(metadata map[string]any) (authID, authIndex string) {
	if metadata == nil {
		return "", ""
	}
	if v, ok := metadata[hostapi.MetadataSelectedAuthID].(string); ok {
		authID = v
	}
	if v, ok := metadata[hostapi.MetadataSelectedAuthIndex].(string); ok {
		authIndex = v
	}
	return authID, authIndex
}
