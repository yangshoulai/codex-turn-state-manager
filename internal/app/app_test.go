package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/accounts"
	"github.com/yangshoulai/codex-turn-state-manager/internal/callhistory"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/intercept"
	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
	"github.com/yangshoulai/codex-turn-state-manager/internal/version"
	"github.com/yangshoulai/codex-turn-state-manager/web"
)

const targetLength = 292

func stateOf(n int) string { return strings.Repeat("s", n) }

func newTestApp(t *testing.T, host hostapi.Host) *App {
	t.Helper()
	a, err := New(context.Background(), Config{
		DataDir: t.TempDir(),
		Host:    host,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Stop)
	return a
}

func mockHost(accounts int) *hostapi.MockHost {
	host := hostapi.NewMockHost()
	for i := 0; i < accounts; i++ {
		host.AddAccount(hostapi.Account{
			AuthIndex: "codex-auth-" + string(rune('1'+i)),
			AuthID:    "auth-id-" + string(rune('1'+i)),
			Provider:  hostapi.ProviderCodex,
			Label:     "user" + string(rune('1'+i)),
			Status:    hostapi.AccountStatusActive,
			Priority:  10 - i,
		})
	}
	return host
}

// TestApp_StartsAndRestoresState is the NF-04 contract: everything the plugin
// persisted comes back after a restart.
func TestApp_StartsAndRestoresState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	host := mockHost(2)

	first, err := New(ctx, Config{DataDir: dir, Host: host})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if _, err := first.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if got := len(first.Accounts().All()); got != 2 {
		t.Fatalf("synced accounts = %d, want 2", got)
	}

	if err := first.Accounts().SetProbeEnabled(ctx, "codex-auth-1", "gpt-5-codex", true); err != nil {
		t.Fatalf("SetProbeEnabled: %v", err)
	}
	if _, err := first.States().Bind(ctx, states.Binding{
		Pair:       states.Pair{AuthIndex: "codex-auth-1", Model: "gpt-5-codex"},
		StateValue: stateOf(targetLength),
		Source:     states.SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := first.Proxies().Upsert(ctx, proxiesFixture("proxy-1")); err != nil {
		t.Fatalf("Upsert proxy: %v", err)
	}
	if _, err := first.Settings().Update(ctx, settings.Patch{
		ProbeConcurrency: intPtr(5),
	}); err != nil {
		t.Fatalf("Update settings: %v", err)
	}
	first.Stop()

	second, err := New(ctx, Config{DataDir: dir, Host: host})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer second.Stop()

	if got := second.Settings().Current().ProbeConcurrency; got != 5 {
		t.Errorf("restored probe_concurrency = %d, want 5", got)
	}
	if !second.Accounts().ProbeEnabled("codex-auth-1", "gpt-5-codex") {
		t.Error("probe toggle was not restored")
	}
	binding, status := second.States().Lookup("codex-auth-1", "gpt-5-codex")
	if !status.Usable() {
		t.Errorf("binding status = %s, want usable", status)
	}
	if len(binding.StateValue) != targetLength {
		t.Errorf("restored state length = %d, want %d", len(binding.StateValue), targetLength)
	}
	if got := len(second.Proxies().All()); got != 1 {
		t.Errorf("restored proxies = %d, want 1", got)
	}
}

// TestApp_SwitchOffBypassesEverything walks the design's master-switch rule
// across all four capabilities at once.
func TestApp_SwitchOffBypassesEverything(t *testing.T) {
	ctx := context.Background()
	host := mockHost(1)
	a := newTestApp(t, host)

	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if err := a.Accounts().SetProbeEnabled(ctx, "codex-auth-1", "gpt-5-codex", true); err != nil {
		t.Fatalf("SetProbeEnabled: %v", err)
	}
	if _, err := a.States().Bind(ctx, states.Binding{
		Pair:       states.Pair{AuthIndex: "codex-auth-1", Model: "gpt-5-codex"},
		StateValue: stateOf(targetLength),
		Source:     states.SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// Sanity: with the switches on, all four capabilities are live.
	req := &hostapi.InterceptedRequest{
		Stage: hostapi.StageAfterAuth, RequestID: "req-1", Model: "gpt-5-codex",
		AuthID: "auth-id-1", AuthIndex: "codex-auth-1", Headers: http.Header{},
	}
	if got := a.InjectState(req); got.Action != "injected" {
		t.Fatalf("InjectState action = %s, want injected", got.Action)
	}
	pick := a.PickCredential(ctx, hostapi.SchedulerPickRequest{
		RequestID: "req-1", Provider: hostapi.ProviderCodex, Model: "gpt-5-codex",
		Candidates: []hostapi.Candidate{{ID: "auth-id-1", Priority: 1}},
	})
	if pick.Delegated() {
		t.Error("expected the plugin to steer routing while the switches are on")
	}

	// Now turn the master switch off.
	off := false
	if _, err := a.Settings().Update(ctx, settings.Patch{GlobalEnabled: &off}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	req2 := &hostapi.InterceptedRequest{
		Stage: hostapi.StageAfterAuth, RequestID: "req-2", Model: "gpt-5-codex",
		AuthID: "auth-id-1", AuthIndex: "codex-auth-1", Headers: http.Header{},
	}
	if got := a.InjectState(req2); got.Action != "passthrough" {
		t.Errorf("InjectState action = %s, want passthrough", got.Action)
	}
	if req2.Headers.Get("X-Codex-Turn-State") != "" {
		t.Error("state must not be injected while the master switch is off")
	}

	captured := a.ObserveStreamChunk(ctx, hostapi.StreamChunk{
		RequestID: "req-2", Model: "gpt-5-codex", AuthIndex: "codex-auth-1",
		ChunkIndex:      hostapi.StreamChunkHeaderInitIndex,
		ResponseHeaders: http.Header{"X-Codex-Turn-State": []string{stateOf(targetLength)}},
	})
	if captured.Action != "skipped" {
		t.Errorf("capture action = %s, want skipped", captured.Action)
	}

	pick2 := a.PickCredential(ctx, hostapi.SchedulerPickRequest{
		RequestID: "req-2", Provider: hostapi.ProviderCodex, Model: "gpt-5-codex",
		Candidates: []hostapi.Candidate{{ID: "auth-id-1", Priority: 1}},
	})
	if !pick2.Delegated() {
		t.Error("routing must defer to the host while the master switch is off")
	}
}

// TestApp_StatePrioritySwitchStopsOnlyRouting isolates the new switch: turning
// it off must take the plugin out of the scheduling path while leaving
// injection alone.
func TestApp_StatePrioritySwitchStopsOnlyRouting(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if _, err := a.States().Bind(ctx, states.Binding{
		Pair:       states.Pair{AuthIndex: "codex-auth-1", Model: "gpt-5-codex"},
		StateValue: stateOf(targetLength),
		Source:     states.SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	off := false
	if _, err := a.Settings().Update(ctx, settings.Patch{StatePriorityEnabled: &off}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Routing stands down.
	pick := a.PickCredential(ctx, hostapi.SchedulerPickRequest{
		RequestID: "req-1", Provider: hostapi.ProviderCodex, Model: "gpt-5-codex",
		Candidates: []hostapi.Candidate{{ID: "auth-id-1", Priority: 1}},
	})
	if !pick.Delegated() {
		t.Error("routing must not interfere while state_priority_enabled is off")
	}

	// Injection carries on.
	req := &hostapi.InterceptedRequest{
		Stage: hostapi.StageAfterAuth, RequestID: "req-1", Model: "gpt-5-codex",
		AuthID: "auth-id-1", AuthIndex: "codex-auth-1", Headers: http.Header{},
	}
	if got := a.InjectState(req); got.Action != "injected" {
		t.Errorf("InjectState action = %s, want injected; only routing should be off", got.Action)
	}
}

// TestApp_ManagementAPI exercises the panel's endpoints end to end.
func TestApp_ManagementAPI(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()

	base := srv.URL + version.ManagementBasePath

	get := func(path string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return resp.StatusCode, payload
	}

	if code, payload := get("/status"); code != http.StatusOK {
		t.Errorf("GET /status = %d (%v)", code, payload)
	}

	code, payload := get("/settings")
	if code != http.StatusOK {
		t.Fatalf("GET /settings = %d", code)
	}
	if payload["probeConcurrency"] != float64(2) {
		t.Errorf("probeConcurrency = %v, want 2", payload["probeConcurrency"])
	}
	if payload["targetStateLength"] != float64(292) {
		t.Errorf("targetStateLength = %v, want 292", payload["targetStateLength"])
	}

	code, payload = get("/accounts")
	if code != http.StatusOK {
		t.Fatalf("GET /accounts = %d", code)
	}
	if accounts, _ := payload["accounts"].([]any); len(accounts) != 1 {
		t.Errorf("accounts = %d, want 1", len(accounts))
	}

	if code, _ := get("/bindings"); code != http.StatusOK {
		t.Errorf("GET /bindings = %d", code)
	}
	if code, _ := get("/proxy-nodes"); code != http.StatusOK {
		t.Errorf("GET /proxy-nodes = %d", code)
	}
	if code, _ := get("/probe-history?limit=10"); code != http.StatusOK {
		t.Errorf("GET /probe-history = %d", code)
	}
	if code, _ := get("/time-windows"); code != http.StatusOK {
		t.Errorf("GET /time-windows = %d", code)
	}

	// A settings write round-trips.
	body := strings.NewReader(`{"probeConcurrency":4}`)
	req, _ := http.NewRequest(http.MethodPut, base+"/settings", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /settings: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /settings = %d", resp.StatusCode)
	}
	if got := a.Settings().Current().ProbeConcurrency; got != 4 {
		t.Errorf("probeConcurrency after PUT = %d, want 4", got)
	}

	// An out-of-range write is rejected and leaves the snapshot alone.
	badBody := strings.NewReader(`{"probeConcurrency":999}`)
	badReq, _ := http.NewRequest(http.MethodPut, base+"/settings", badBody)
	badReq.Header.Set("Content-Type", "application/json")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("PUT /settings (invalid): %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT /settings with an invalid value = %d, want 400", badResp.StatusCode)
	}
	if got := a.Settings().Current().ProbeConcurrency; got != 4 {
		t.Errorf("a rejected write changed the snapshot to %d", got)
	}
}

// TestApp_ManagementAPINeverLeaksFullStateValues guards the rule that the
// panel sees a prefix, not the whole token.
func TestApp_ManagementAPINeverLeaksFullStateValues(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	secret := stateOf(targetLength)
	if _, err := a.States().Bind(ctx, states.Binding{
		Pair:       states.Pair{AuthIndex: "codex-auth-1", Model: "gpt-5-codex"},
		StateValue: secret,
		Source:     states.SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	base := srv.URL + version.ManagementBasePath

	resp, err := http.Get(base + "/bindings")
	if err != nil {
		t.Fatalf("GET /bindings: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Error("the bindings endpoint returned a full state value")
	}
	if !strings.Contains(string(raw), secret[:8]) {
		t.Error("the bindings endpoint should still return the state prefix")
	}
}

func proxiesFixture(id string) proxies.Node {
	now := time.Now().UTC().Truncate(time.Second)
	return proxies.Node{
		ID: id, URL: "http://" + id + ":8080", Enabled: true, LastUsedAt: &now,
	}
}

func intPtr(v int) *int { return &v }

// ---------------------------------------------------------------------------
// binding management endpoints (design doc 5.4 / 5.5)

// bindingAPI drives the binding endpoints against a running app.
type bindingAPI struct {
	t    *testing.T
	base string
}

func (b *bindingAPI) do(method, path string) (int, []byte) {
	b.t.Helper()
	req, err := http.NewRequest(method, b.base+path, nil)
	if err != nil {
		b.t.Fatalf("build %s %s: %v", method, path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, body
}

func (b *bindingAPI) history() []map[string]any {
	b.t.Helper()
	code, body := b.do(http.MethodGet, "/bindings/history?authIndex=codex-auth-1&model=gpt-5-codex")
	if code != http.StatusOK {
		b.t.Fatalf("GET history = %d (%s)", code, body)
	}
	var payload struct {
		History []map[string]any `json:"history"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		b.t.Fatalf("decode history: %v", err)
	}
	return payload.History
}

func (b *bindingAPI) bindingCount() int {
	b.t.Helper()
	code, body := b.do(http.MethodGet, "/bindings")
	if code != http.StatusOK {
		b.t.Fatalf("GET bindings = %d (%s)", code, body)
	}
	var payload struct {
		Bindings []map[string]any `json:"bindings"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		b.t.Fatalf("decode bindings: %v", err)
	}
	return len(payload.Bindings)
}

// TestApp_DeleteBindingEndpoint covers the manual delete: it must remove the
// binding, leave a 'deleted' trail in history, and bring the next probe forward
// rather than leaving the pair waiting out its old backoff.
func TestApp_DeleteBindingEndpoint(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	pair := states.Pair{AuthIndex: "codex-auth-1", Model: "gpt-5-codex"}
	first := strings.Repeat("a", targetLength)
	second := strings.Repeat("b", targetLength)

	if _, err := a.States().Bind(ctx, states.Binding{
		Pair: pair, StateValue: first, Source: states.SourceProbe, ProxyID: "p1",
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if _, err := a.States().Bind(ctx, states.Binding{
		Pair: pair, StateValue: second, Source: states.SourceTraffic,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &bindingAPI{t: t, base: srv.URL + version.ManagementBasePath}

	if got := api.bindingCount(); got != 1 {
		t.Fatalf("bindings = %d, want 1", got)
	}

	// Deleting a binding is an operator saying "this value is wrong", so the
	// pair must become due immediately. ProbeNow is what the handler calls; the
	// zero time is how "due now" is represented.
	a.ProbeScheduler().ProbeNow(pair)
	if at, ok := a.ProbeScheduler().NextProbeAt(pair); !ok || !at.IsZero() {
		t.Fatalf("ProbeNow did not clear the backoff: %v (ok=%v)", at, ok)
	}

	code, body := api.do(http.MethodDelete, "/bindings?authIndex=codex-auth-1&model=gpt-5-codex")
	if code != http.StatusOK {
		t.Fatalf("DELETE binding = %d (%s)", code, body)
	}

	if got := api.bindingCount(); got != 0 {
		t.Errorf("bindings after delete = %d, want 0", got)
	}
	if _, status := a.States().Lookup(pair.AuthIndex, pair.Model); status != states.StatusMissing {
		t.Errorf("status after delete = %s, want missing", status)
	}

	// The deleted value is retained in history so the panel can show what was
	// removed, together with the two binds that preceded it.
	history := api.history()
	if len(history) != 3 {
		t.Fatalf("history rows = %d, want 3 (bound, replaced, deleted)", len(history))
	}
	if history[0]["action"] != "deleted" {
		t.Errorf("newest action = %v, want deleted", history[0]["action"])
	}
	if history[0]["source"] != "manual" {
		t.Errorf("delete source = %v, want manual", history[0]["source"])
	}
	if history[1]["action"] != "replaced" {
		t.Errorf("second action = %v, want replaced", history[1]["action"])
	}
	if history[2]["action"] != "bound" {
		t.Errorf("oldest action = %v, want bound", history[2]["action"])
	}
}

// TestApp_BindingHistoryEndpoint covers the history read, including that the
// full state value is only returned when it is asked for explicitly.
func TestApp_BindingHistoryEndpoint(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	secret := strings.Repeat("z", targetLength)
	pair := states.Pair{AuthIndex: "codex-auth-1", Model: "gpt-5-codex"}
	if _, err := a.States().Bind(ctx, states.Binding{
		Pair: pair, StateValue: secret, Source: states.SourceProbe, ProxyID: "p1",
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &bindingAPI{t: t, base: srv.URL + version.ManagementBasePath}

	history := api.history()
	if len(history) != 1 {
		t.Fatalf("history rows = %d, want 1", len(history))
	}
	entry := history[0]

	if entry["statePrefix"] != secret[:8] {
		t.Errorf("statePrefix = %v, want %q", entry["statePrefix"], secret[:8])
	}
	if _, present := entry["stateValue"]; present {
		t.Error("the history endpoint returned a full state value without expand=1")
	}
	if entry["stateLength"] != float64(targetLength) {
		t.Errorf("stateLength = %v, want %d", entry["stateLength"], targetLength)
	}
	if entry["proxyId"] != "p1" {
		t.Errorf("proxyId = %v, want p1", entry["proxyId"])
	}
	if entry["source"] != "probe" {
		t.Errorf("source = %v, want probe", entry["source"])
	}

	// expand=1 is the documented way to reveal the whole value (design doc 5.3).
	code, body := api.do(http.MethodGet,
		"/bindings/history?authIndex=codex-auth-1&model=gpt-5-codex&expand=1")
	if code != http.StatusOK {
		t.Fatalf("GET history?expand=1 = %d (%s)", code, body)
	}
	if !strings.Contains(string(body), secret) {
		t.Error("expand=1 should reveal the full state value")
	}
}

func TestApp_ClearBindingHistoryEndpoint(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	pair := states.Pair{AuthIndex: "codex-auth-1", Model: "gpt-5-codex"}
	if _, err := a.States().Bind(ctx, states.Binding{
		Pair: pair, StateValue: stateOf(targetLength), Source: states.SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &bindingAPI{t: t, base: srv.URL + version.ManagementBasePath}

	if got := len(api.history()); got != 1 {
		t.Fatalf("history rows = %d, want 1", got)
	}

	code, body := api.do(http.MethodDelete, "/bindings/history?authIndex=codex-auth-1&model=gpt-5-codex")
	if code != http.StatusOK {
		t.Fatalf("DELETE history = %d (%s)", code, body)
	}

	if got := len(api.history()); got != 0 {
		t.Errorf("history rows after clear = %d, want 0", got)
	}
	// Clearing history must not touch the binding itself.
	if _, status := a.States().Lookup(pair.AuthIndex, pair.Model); !status.Usable() {
		t.Errorf("clearing history removed the binding (status %s)", status)
	}
}

// TestApp_DeleteMissingBindingIsIdempotent covers a panel double-click: the
// delete must succeed without an error when there is nothing to remove.
func TestApp_DeleteMissingBindingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &bindingAPI{t: t, base: srv.URL + version.ManagementBasePath}

	for i := 0; i < 2; i++ {
		code, body := api.do(http.MethodDelete, "/bindings?authIndex=codex-auth-1&model=gpt-5-codex")
		if code != http.StatusOK {
			t.Fatalf("delete attempt %d = %d (%s)", i+1, code, body)
		}
	}
	// A pair that was never bound must not accumulate phantom history rows.
	if got := len(api.history()); got != 0 {
		t.Errorf("history rows = %d, want 0 for a pair that was never bound", got)
	}
}

// ---------------------------------------------------------------------------
// account and proxy management endpoints (F-02, design doc 5.5)

type panelAPI struct {
	t    *testing.T
	base string
}

func (p *panelAPI) do(method, path string, payload string) (int, []byte) {
	p.t.Helper()
	var body io.Reader
	if payload != "" {
		body = strings.NewReader(payload)
	}
	req, err := http.NewRequest(method, p.base+path, body)
	if err != nil {
		p.t.Fatalf("build %s %s: %v", method, path, err)
	}
	if payload != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// TestApp_ProbeToggleEndpoint covers F-02: probing is enabled per
// (account, model) pair, and the toggle round-trips.
func TestApp_ProbeToggleEndpoint(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &panelAPI{t: t, base: srv.URL + version.ManagementBasePath}

	const path = "/accounts/models/probe?authIndex=codex-auth-1&model=gpt-5-codex"

	for _, want := range []bool{true, false, true} {
		code, body := api.do(http.MethodPut, path,
			fmt.Sprintf(`{"enabled":%t}`, want))
		if code != http.StatusOK {
			t.Fatalf("PUT probe=%t = %d (%s)", want, code, body)
		}
		if got := a.Accounts().ProbeEnabled("codex-auth-1", "gpt-5-codex"); got != want {
			t.Errorf("ProbeEnabled = %v, want %v", got, want)
		}
	}

	// The pair list the scheduler works from must follow the toggle.
	if got := len(a.Accounts().EnabledPairs()); got != 1 {
		t.Errorf("EnabledPairs = %d, want 1", got)
	}
	code, body := api.do(http.MethodPut, path, `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", code, body)
	}
	if got := len(a.Accounts().EnabledPairs()); got != 0 {
		t.Errorf("EnabledPairs = %d, want 0 after disabling", got)
	}
}

// TestApp_AccountModelsEndpoint covers the per-account model table the panel
// renders, including that a bound pair reports its state status.
func TestApp_AccountModelsEndpoint(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	pair := states.Pair{AuthIndex: "codex-auth-1", Model: "gpt-5.6-luna"}
	// The plugin cannot read an account's model list from CPA, so a model exists
	// only once it is configured.
	if err := a.Accounts().SetProbeEnabled(ctx, pair.AuthIndex, pair.Model, true); err != nil {
		t.Fatalf("SetProbeEnabled: %v", err)
	}
	if _, err := a.States().Bind(ctx, states.Binding{
		Pair: pair, StateValue: stateOf(targetLength), Source: states.SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &panelAPI{t: t, base: srv.URL + version.ManagementBasePath}

	code, body := api.do(http.MethodGet, "/accounts/models?authIndex=codex-auth-1", "")
	if code != http.StatusOK {
		t.Fatalf("GET models = %d (%s)", code, body)
	}
	var payload struct {
		Models []struct {
			Model        string `json:"model"`
			ProbeEnabled bool   `json:"probeEnabled"`
			Status       string `json:"status"`
			StateLength  int    `json:"stateLength"`
			MinReasoning string `json:"minReasoning"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(payload.Models) == 0 {
		t.Fatal("no models returned")
	}

	var found bool
	for _, m := range payload.Models {
		if m.Model != "gpt-5.6-luna" {
			continue
		}
		found = true
		if m.Status != "fresh" {
			t.Errorf("status = %q, want fresh", m.Status)
		}
		if m.StateLength != targetLength {
			t.Errorf("stateLength = %d, want %d", m.StateLength, targetLength)
		}
		if m.MinReasoning == "" {
			t.Error("minReasoning should be reported so the panel can show the probe cost")
		}
	}
	if !found {
		t.Error("the bound model is missing from the table")
	}

	// An unknown account is a 404, not an empty table.
	if code, _ := api.do(http.MethodGet, "/accounts/models?authIndex=nope", ""); code != http.StatusNotFound {
		t.Errorf("GET models for an unknown account = %d, want 404", code)
	}
}

func TestApp_AccountSyncEndpoint(t *testing.T) {
	host := mockHost(2)
	a := newTestApp(t, host)

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &panelAPI{t: t, base: srv.URL + version.ManagementBasePath}

	if got := len(a.Accounts().All()); got != 0 {
		t.Fatalf("accounts before sync = %d, want 0", got)
	}

	code, body := api.do(http.MethodPost, "/accounts/sync", "")
	if code != http.StatusOK {
		t.Fatalf("POST sync = %d (%s)", code, body)
	}
	if got := len(a.Accounts().All()); got != 2 {
		t.Errorf("accounts after sync = %d, want 2", got)
	}

	// An account CPA no longer reports must disappear on the next sync.
	host.SetAccounts([]hostapi.Account{{
		AuthIndex: "codex-auth-1", AuthID: "auth-id-1",
		Provider: hostapi.ProviderCodex, Label: "user1",
		Status: hostapi.AccountStatusActive, Priority: 10,
	}})
	if code, body := api.do(http.MethodPost, "/accounts/sync", ""); code != http.StatusOK {
		t.Fatalf("POST sync = %d (%s)", code, body)
	}
	if got := len(a.Accounts().All()); got != 1 {
		t.Errorf("accounts after the pool shrank = %d, want 1", got)
	}
}

// TestApp_ReplaceProxiesEndpoint covers "代理池可管理": the panel saves the
// whole pool at once, and omitted nodes are removed.
func TestApp_ReplaceProxiesEndpoint(t *testing.T) {
	a := newTestApp(t, mockHost(1))

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &panelAPI{t: t, base: srv.URL + version.ManagementBasePath}

	payload := `{"proxies":[` +
		`{"id":"p1","url":"http://proxy1:8080","enabled":true},` +
		`{"id":"p2","url":"http://proxy2:8080","enabled":false}]}`
	code, body := api.do(http.MethodPut, "/proxy-nodes", payload)
	if code != http.StatusOK {
		t.Fatalf("PUT proxy-nodes = %d (%s)", code, body)
	}

	all := a.Proxies().All()
	if len(all) != 2 {
		t.Fatalf("pool size = %d, want 2", len(all))
	}

	// A disabled node is stored but never selected.
	if got := len(a.Proxies().AvailableFor("codex-auth-1", time.Now())); got != 1 {
		t.Errorf("available nodes = %d, want 1", got)
	}

	// Saving a shorter list removes what is gone.
	code, body = api.do(http.MethodPut, "/proxy-nodes",
		`{"proxies":[{"id":"p2","url":"http://proxy2:8080","enabled":true}]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT proxy-nodes = %d (%s)", code, body)
	}
	if got := len(a.Proxies().All()); got != 1 {
		t.Errorf("pool size after removing p1 = %d, want 1", got)
	}

	// A node without a URL is rejected rather than stored broken.
	if code, _ := api.do(http.MethodPut, "/proxy-nodes",
		`{"proxies":[{"id":"bad","url":""}]}`); code != http.StatusBadRequest {
		t.Errorf("PUT with an empty url = %d, want 400", code)
	}
}

// ---------------------------------------------------------------------------
// time window endpoints
//
// These three were the last uncovered management handlers, and they are also
// the ones the route rework touched, so they are tested through the same HTTP
// path the host uses.

func TestApp_TimeWindowEndpoints(t *testing.T) {
	a := newTestApp(t, mockHost(1))
	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &panelAPI{t: t, base: srv.URL + version.ManagementBasePath}

	type window struct {
		ID         string `json:"id"`
		Label      string `json:"label"`
		DaysOfWeek []int  `json:"daysOfWeek"`
		StartTime  string `json:"startTime"`
		EndTime    string `json:"endTime"`
		Enabled    bool   `json:"enabled"`
	}

	// Create.
	code, body := api.do(http.MethodPost, "/time-windows",
		`{"label":"work","daysOfWeek":[1,2,3,4,5],"startTime":"08:00","endTime":"20:00","enabled":true}`)
	if code != http.StatusOK {
		t.Fatalf("POST /time-windows = %d (%s)", code, body)
	}
	var created window
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created window: %v", err)
	}
	if created.ID == "" {
		t.Fatal("the created window has no id")
	}

	// List.
	code, body = api.do(http.MethodGet, "/time-windows", "")
	if code != http.StatusOK {
		t.Fatalf("GET /time-windows = %d", code)
	}
	var list struct {
		Windows []window `json:"windows"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode window list: %v", err)
	}
	if len(list.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(list.Windows))
	}

	// Update by query parameter.
	code, body = api.do(http.MethodPut, "/time-windows?id="+created.ID,
		`{"label":"work","daysOfWeek":[1],"startTime":"22:00","endTime":"06:00","enabled":true}`)
	if code != http.StatusOK {
		t.Fatalf("PUT /time-windows = %d (%s)", code, body)
	}
	if got := len(a.Windows().All()); got != 1 {
		t.Fatalf("windows after update = %d, want 1", got)
	}
	stored := a.Windows().All()[0]
	if stored.StartTime != "22:00" || stored.EndTime != "06:00" {
		t.Errorf("window = %s-%s, want 22:00-06:00", stored.StartTime, stored.EndTime)
	}
	// A cross-midnight window must admit its evening leg. The window is
	// Monday-only, so the instant has to be a Monday too.
	monday := time.Date(2026, 9, 21, 23, 0, 0, 0, time.UTC)
	if monday.Weekday() != time.Monday {
		t.Fatalf("fixture is %s, expected Monday", monday.Weekday())
	}
	if !a.Windows().ShouldProbeNow(monday) {
		t.Error("a Monday 22:00-06:00 window should admit Monday 23:00")
	}
	// ...and its next-day morning leg, which belongs to Monday's window.
	if !a.Windows().ShouldProbeNow(time.Date(2026, 9, 22, 5, 0, 0, 0, time.UTC)) {
		t.Error("the Tuesday morning leg should belong to Monday's window")
	}
	// ...but not the same clock time on a Tuesday evening.
	if a.Windows().ShouldProbeNow(time.Date(2026, 9, 22, 23, 0, 0, 0, time.UTC)) {
		t.Error("Tuesday 23:00 is outside a Monday-only window")
	}

	// Delete by query parameter.
	code, body = api.do(http.MethodDelete, "/time-windows?id="+created.ID, "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /time-windows = %d (%s)", code, body)
	}
	if got := len(a.Windows().All()); got != 0 {
		t.Errorf("windows after delete = %d, want 0", got)
	}
}

func TestApp_TimeWindowEndpointsValidateInput(t *testing.T) {
	a := newTestApp(t, mockHost(1))
	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &panelAPI{t: t, base: srv.URL + version.ManagementBasePath}

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"update without an id", http.MethodPut, "/time-windows", `{"startTime":"08:00","endTime":"20:00"}`, http.StatusBadRequest},
		{"delete without an id", http.MethodDelete, "/time-windows", "", http.StatusBadRequest},
		{"unpadded clock is rejected", http.MethodPost, "/time-windows", `{"label":"x","startTime":"8:00","endTime":"20:00"}`, http.StatusBadRequest},
		{"invalid day is rejected", http.MethodPost, "/time-windows", `{"label":"x","startTime":"08:00","endTime":"20:00","daysOfWeek":[9]}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := api.do(tc.method, tc.path, tc.body)
			if code != tc.want {
				t.Errorf("%s %s = %d (%s), want %d", tc.method, tc.path, code, body, tc.want)
			}
		})
	}

	if got := len(a.Windows().All()); got != 0 {
		t.Errorf("windows = %d, want 0 after only rejected writes", got)
	}
}

// TestApp_ManagementRoutesCarryNoPathParameters guards the constraint that
// forced the query-parameter design: the host matches exact paths and rejects
// anything containing a wildcard.
func TestApp_ManagementRoutesCarryNoPathParameters(t *testing.T) {
	a := newTestApp(t, mockHost(1))
	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &panelAPI{t: t, base: srv.URL + version.ManagementBasePath}

	// A path segment where a query parameter belongs must not accidentally
	// reach a handler.
	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/bindings/codex-auth-1/gpt-5-codex"},
		{http.MethodGet, "/bindings/codex-auth-1/gpt-5-codex/history"},
		{http.MethodGet, "/accounts/codex-auth-1/models"},
	} {
		code, _ := api.do(tc.method, tc.path, "")
		if code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404; path parameters must not be routed", tc.method, tc.path, code)
		}
	}

	// And the query-parameter form must reject a missing pair rather than
	// silently operating on an empty account id.
	if code, _ := api.do(http.MethodDelete, "/bindings", ""); code != http.StatusBadRequest {
		t.Errorf("DELETE /bindings without parameters = %d, want 400", code)
	}
	if code, _ := api.do(http.MethodDelete, "/bindings?authIndex=only", ""); code != http.StatusBadRequest {
		t.Errorf("DELETE /bindings with a partial pair = %d, want 400", code)
	}
}

// TestApp_ModelListComesFromTheCatalog covers where the account model list
// comes from.
//
// Two rejected designs came first: a hardcoded table (which went stale and put
// gpt-5-codex on screen) and operator-managed entries (which pushed the work
// onto the operator for data the plugin should be able to obtain). The list now
// comes from the shared manifest CPA itself syncs, and the operator only
// toggles probing per model.
func TestApp_ModelListComesFromTheCatalog(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	list, ok := a.Accounts().Models("codex-auth-1")
	if !ok {
		t.Fatal("account missing")
	}
	if len(list) == 0 {
		t.Fatal("no models offered; the catalog should always seed something")
	}

	// Every catalog entry is offered, and every one starts with probing off:
	// nothing should probe until an operator chooses it.
	for _, m := range list {
		if m.ProbeEnabled {
			t.Errorf("%s probes by default; it should be opt-in", m.Model)
		}
	}

	// A toggle is visible immediately, without waiting for a sync.
	const model = "gpt-5.6-luna"
	if !a.Accounts().CatalogHas(model) {
		t.Skipf("%s is not in the fallback catalog", model)
	}
	if err := a.Accounts().SetProbeEnabled(ctx, "codex-auth-1", model, true); err != nil {
		t.Fatalf("SetProbeEnabled: %v", err)
	}
	found := false
	for _, m := range mustModels(t, a, "codex-auth-1") {
		if m.Model == model {
			found, _ = true, m
			if !m.ProbeEnabled {
				t.Error("the toggle did not take effect")
			}
		}
	}
	if !found {
		t.Errorf("%s disappeared from the list after being toggled", model)
	}

	// And it survives a restart, because the toggle is persisted while the list
	// is re-derived.
	dir := a.cfg.DataDir
	a.Stop()
	restarted, err := New(ctx, Config{DataDir: dir, Host: mockHost(1)})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer restarted.Stop()
	if _, err := restarted.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts after restart: %v", err)
	}
	for _, m := range mustModels(t, restarted, "codex-auth-1") {
		if m.Model == model && !m.ProbeEnabled {
			t.Error("the probe toggle did not survive the restart")
		}
	}
}

func mustModels(t *testing.T, a *App, authIndex string) []accounts.ModelState {
	t.Helper()
	list, ok := a.Accounts().Models(authIndex)
	if !ok {
		t.Fatalf("account %s missing", authIndex)
	}
	return list
}

func TestApp_ForgetModelRemovesBindingAndConfig(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	const model = "gpt-5.6-luna"
	pair := states.Pair{AuthIndex: "codex-auth-1", Model: model}
	if err := a.Accounts().SetProbeEnabled(ctx, pair.AuthIndex, model, true); err != nil {
		t.Fatalf("SetProbeEnabled: %v", err)
	}
	if _, err := a.States().Bind(ctx, states.Binding{
		Pair: pair, StateValue: stateOf(targetLength), Source: states.SourceProbe,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &panelAPI{t: t, base: srv.URL + version.ManagementBasePath}

	code, body := api.do(http.MethodDelete,
		"/accounts/models?authIndex=codex-auth-1&model="+model, "")
	if code != http.StatusOK {
		t.Fatalf("DELETE /accounts/models = %d (%s)", code, body)
	}

	// The binding must go too: a state value attached to a pair that no longer
	// appears in the panel would be invisible and unremovable.
	if _, status := a.States().Lookup(pair.AuthIndex, pair.Model); status != states.StatusMissing {
		t.Errorf("binding survived removal (status %s)", status)
	}
	if a.Accounts().ProbeEnabled(pair.AuthIndex, model) {
		t.Error("probe toggle survived removal")
	}
	if a.Accounts().ModelConfigured(pair.AuthIndex, model) {
		t.Error("configured model survived removal")
	}

	// The model stays listed -- the catalog is the authority on what exists --
	// but it is no longer configured to probe.
	for _, m := range mustModels(t, a, "codex-auth-1") {
		if m.Model == model && m.ProbeEnabled {
			t.Error("the probe toggle survived removal")
		}
	}

	// Missing parameters are a 400, not a silent no-op.
	if code, _ := api.do(http.MethodDelete, "/accounts/models?authIndex=only", ""); code != http.StatusBadRequest {
		t.Errorf("DELETE without a model = %d, want 400", code)
	}
}

// TestApp_BackgroundWorkSurvivesStartup is the regression test for a defect that
// only a real instance could reveal.
//
// The adapter used to hand App.Start the registration timeout context, and its
// `defer cancel()` fired the moment registration returned -- so the scan loop
// exited immediately and nothing background ever ran. Every check still passed,
// because the management API is request-driven and kept answering; the plugin
// simply never probed anything.
//
// A scan is observable as an account sync, which the loop performs on its own
// cadence, so counting Host.ListAccounts calls distinguishes "running" from
// "started and immediately dead".
func TestApp_BackgroundWorkSurvivesStartup(t *testing.T) {
	ctx := context.Background()
	host := mockHost(1)

	a, err := New(ctx, Config{DataDir: t.TempDir(), Host: host})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Stop()

	const intervalSec = 5
	if _, err := a.Settings().Update(ctx, settings.Patch{
		ScanInterval: durPtrSeconds(intervalSec),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	a.Start()

	// Start() kicks off an initial account sync of its own. Waiting for it
	// first is the whole point: counting from before it would let the initial
	// sync satisfy the assertion, which is how the first version of this test
	// passed in 0.1s while proving nothing.
	settleDeadline := time.Now().Add(2 * time.Second)
	for host.ListAccountsCalls() == 0 && time.Now().Before(settleDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if host.ListAccountsCalls() == 0 {
		t.Fatal("the initial account sync never ran")
	}
	before := host.ListAccountsCalls()

	// Anything after this point can only come from a scan tick.
	deadline := time.Now().Add(time.Duration(intervalSec*3) * time.Second)
	for time.Now().Before(deadline) {
		if host.ListAccountsCalls() > before {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no scan ran within %s (ListAccounts calls stuck at %d): "+
		"the background loop died at startup", time.Duration(intervalSec*3)*time.Second, before)
}

func durPtrSeconds(n int) *time.Duration {
	d := time.Duration(n) * time.Second
	return &d
}

// TestApp_PanelMountServesTheEntryPoint guards the harness panel's two routing
// seams at once, because both are silent when wrong: the bare base path has to
// redirect *into* the mount, and a real asset path has to reach the asset
// handler with the prefix stripped. Stripping the trailing slash together with
// the prefix made the bare path strip to "", which the inner mux redirected to
// the harness root -- so the panel 404'd at its own advertised URL.
func TestApp_PanelMountServesTheEntryPoint(t *testing.T) {
	a := newTestApp(t, mockHost(1))
	srv := httptest.NewServer(a.Handler(web.Handler()))
	t.Cleanup(srv.Close)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	resp, err := client.Get(srv.URL + version.ResourceBasePath + "/")
	if err != nil {
		t.Fatalf("GET bare base path: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("bare base path: got %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc := resp.Header.Get("Location")
	if loc != version.ResourceBasePath+"/index.html" {
		t.Errorf("bare base path redirects to %q, want %q", loc, version.ResourceBasePath+"/index.html")
	}

	// The redirect target must actually serve, not bounce again.
	resp2, err := client.Get(srv.URL + version.ResourceBasePath + "/index.html")
	if err != nil {
		t.Fatalf("GET index.html: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("index.html: got %d, want %d", resp2.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(string(body), "<html") {
		t.Errorf("index.html did not serve the panel document (%d bytes)", len(body))
	}
}

// TestApp_AccountModelsBulkEndpoint covers the shape the panel actually polls.
//
// The panel used to request one account's models per expanded account on every
// refresh, so the cycle cost grew with how many accounts were open. The bulk
// form answers every account at once, and the per-account form has to keep
// working because it is what the probe toggle and the delete endpoints use.
func TestApp_AccountModelsBulkEndpoint(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(2))
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	// Every account is seeded with the whole catalog, so the two accounts differ
	// only in which models have probing switched on. That is the difference the
	// bulk response has to preserve: if it collapsed the accounts onto one
	// shared list, the toggle below would show up under both.
	if err := a.Accounts().SetProbeEnabled(ctx, "codex-auth-1", "gpt-5.6-luna", true); err != nil {
		t.Fatalf("SetProbeEnabled: %v", err)
	}

	srv := httptest.NewServer(a.Handler(nil))
	defer srv.Close()
	api := &panelAPI{t: t, base: srv.URL + version.ManagementBasePath}

	code, body := api.do(http.MethodGet, "/accounts/models", "")
	if code != http.StatusOK {
		t.Fatalf("GET /accounts/models = %d (%s)", code, body)
	}
	var payload struct {
		Accounts map[string][]struct {
			Model        string `json:"model"`
			ProbeEnabled bool   `json:"probeEnabled"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}

	// Both synced accounts must be present, keyed by authIndex, so the panel can
	// look each expanded account's table up without a request of its own.
	for _, authIndex := range []string{"codex-auth-1", "codex-auth-2"} {
		if _, ok := payload.Accounts[authIndex]; !ok {
			t.Errorf("account %s is missing from the bulk response", authIndex)
		}
	}

	probeOn := func(authIndex, model string) (bool, bool) {
		for _, m := range payload.Accounts[authIndex] {
			if m.Model == model {
				return m.ProbeEnabled, true
			}
		}
		return false, false
	}

	if on, found := probeOn("codex-auth-1", "gpt-5.6-luna"); !found || !on {
		t.Errorf("codex-auth-1/gpt-5.6-luna probeEnabled = %v (found %v), want true", on, found)
	}
	if on, found := probeOn("codex-auth-2", "gpt-5.6-luna"); !found || on {
		t.Errorf("codex-auth-2/gpt-5.6-luna probeEnabled = %v (found %v), want false", on, found)
	}
	if n := len(payload.Accounts["codex-auth-1"]); n == 0 {
		t.Error("codex-auth-1 came back with no models")
	}
}

// TestApp_CallHistoryRecordsAWholeRequest drives the three request-path entry
// points in the order the host calls them and reads the row back.
//
// This is the wiring test for the call log: the injector, the response
// interceptor and the completion callback each see a third of the request, and
// a row only appears if all three are connected to the same recorder. The unit
// tests in internal/callhistory cover the joining; this covers the plumbing.
func TestApp_CallHistoryRecordsAWholeRequest(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	a.Start()
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if _, err := a.States().Bind(ctx, states.Binding{
		Pair: states.Pair{AuthIndex: "codex-auth-1", Model: "gpt-5.5"},
		// A real state value has a readable envelope, but the injector does not
		// parse it -- it serves whatever is bound -- so a placeholder is enough
		// to observe injection.
		StateValue: "BOUND-STATE-VALUE",
		Source:     states.SourceManual,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	const requestID = "req-e2e-1"
	req := &hostapi.InterceptedRequest{
		Stage:     hostapi.StageAfterAuth,
		RequestID: requestID,
		Provider:  hostapi.ProviderCodex,
		Model:     "gpt-5.5",
		AuthIndex: "codex-auth-1",
		Headers:   http.Header{"X-Codex-Turn-State": []string{"CLIENT-SUPPLIED"}},
	}
	decision := a.InjectState(req)
	if decision.Action != intercept.ActionInjected {
		t.Fatalf("InjectState = %q, want %q", decision.Action, intercept.ActionInjected)
	}

	a.ObserveStreamChunk(ctx, hostapi.StreamChunk{
		RequestID:  requestID,
		Model:      "gpt-5.5",
		AuthIndex:  "codex-auth-1",
		ChunkIndex: hostapi.StreamChunkHeaderInitIndex,
		ResponseHeaders: http.Header{
			"X-Codex-Turn-State": []string{"UPSTREAM-RETURNED"},
		},
	})
	a.ObserveCompletion(ctx, hostapi.Completion{
		RequestID:  requestID,
		Model:      "gpt-5.5",
		Outcome:    hostapi.CompletionSucceeded,
		StatusCode: http.StatusOK,
	}, nil)

	// The writer is asynchronous and batches on the queue going quiet, so give
	// it a moment rather than asserting straight away.
	store := a.CallHistory()
	var rows []callhistory.Record
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		rows, err = store.ListCalls(ctx, callhistory.Query{AuthIndex: "codex-auth-1"})
		if err != nil {
			t.Fatalf("ListCalls: %v", err)
		}
		if len(rows) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}

	row := rows[0]
	if row.CarriedState != "CLIENT-SUPPLIED" {
		t.Errorf("CarriedState = %q, want what the request arrived with", row.CarriedState)
	}
	if row.InjectedState != "BOUND-STATE-VALUE" {
		t.Errorf("InjectedState = %q, want the bound value", row.InjectedState)
	}
	if row.ResponseState != "UPSTREAM-RETURNED" {
		t.Errorf("ResponseState = %q, want what upstream answered", row.ResponseState)
	}
	if row.StatusCode != http.StatusOK || row.Outcome != "succeeded" {
		t.Errorf("status = %d outcome = %q", row.StatusCode, row.Outcome)
	}
	if row.Model != "gpt-5.5" {
		t.Errorf("Model = %q", row.Model)
	}
}

// TestApp_CallHistoryIsEmptyWhenTheMasterSwitchIsOff pins the master switch as
// the gate for the log too. A plugin that is switched off should look like it
// was never loaded, not like it is quietly observing.
func TestApp_CallHistoryIsEmptyWhenTheMasterSwitchIsOff(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	a.Start()
	if _, err := a.Settings().Update(ctx, settings.Patch{GlobalEnabled: boolPtr(false)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	a.InjectState(&hostapi.InterceptedRequest{
		Stage: hostapi.StageAfterAuth, RequestID: "req-off",
		Model: "gpt-5.5", AuthIndex: "codex-auth-1", Headers: http.Header{},
	})
	a.ObserveCompletion(ctx, hostapi.Completion{
		RequestID: "req-off", Model: "gpt-5.5",
		Outcome: hostapi.CompletionSucceeded, StatusCode: 200,
	}, nil)

	time.Sleep(100 * time.Millisecond)
	rows, err := a.CallHistory().ListCalls(ctx, callhistory.Query{})
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %d, want 0 with the master switch off", len(rows))
	}
}

// TestApp_SyncOneAccountRefreshesStatusAndModels covers the panel's per-account
// button end to end: CPA's view is re-read, and the call to LoadModels is made
// even though upstream cannot answer in a test.
func TestApp_SyncOneAccountRefreshesStatusAndModels(t *testing.T) {
	ctx := context.Background()
	host := mockHost(1)
	a := newTestApp(t, host)
	a.Start()
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	// The operator fixes the account in CPA.
	host.SetAccountStatus("codex-auth-1", hostapi.AccountStatusActive, false)

	view, err := a.SyncOneAccount(ctx, "codex-auth-1", true)
	if err != nil {
		t.Fatalf("SyncOneAccount: %v", err)
	}
	if view.Status != hostapi.AccountStatusActive {
		t.Errorf("status = %q, want active", view.Status)
	}
	if view.Verdict.Blocked {
		t.Errorf("verdict = %+v, want probeable", view.Verdict)
	}

	if _, err := a.SyncOneAccount(ctx, "nobody", false); !errors.Is(err, accounts.ErrUnknownAccount) {
		t.Errorf("unknown account error = %v, want ErrUnknownAccount", err)
	}
}

// TestApp_ForgetsAnAccountCPADropped covers the removal path from the outside:
// a sync that no longer lists an account must take its binding with it.
func TestApp_ForgetsAnAccountCPADropped(t *testing.T) {
	ctx := context.Background()
	host := mockHost(2)
	a := newTestApp(t, host)
	a.Start()
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if _, err := a.States().Bind(ctx, states.Binding{
		Pair:       states.Pair{AuthIndex: "codex-auth-2", Model: "gpt-5.5"},
		StateValue: "VALUE",
		Source:     states.SourceManual,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	host.RemoveAccount("codex-auth-2")
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts after removal: %v", err)
	}

	if _, ok := a.Accounts().Get("codex-auth-2"); ok {
		t.Error("the removed account is still tracked")
	}
	for pair := range a.States().All() {
		if pair.AuthIndex == "codex-auth-2" {
			t.Error("a binding for the removed account survived")
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// TestApp_ExpiredCooldownIsNotPresentedAsAFault is the end-to-end version of
// the defect that was reported from production: "this account is fine, why does
// the panel say error".
//
// CPA leaves unavailable=true and status=error set forever after a 503 -- only a
// token refresh or an explicit quota reset clears them -- and derives
// availability from those flags plus the clock instead. Reading the flag alone
// is what made the plugin disagree with CPA's own panel.
func TestApp_ExpiredCooldownIsNotPresentedAsAFault(t *testing.T) {
	ctx := context.Background()
	host := mockHost(1)
	a := newTestApp(t, host)
	a.Start()
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	host.MarkCooldownExpired("codex-auth-1", `{"error":{"code":"server_is_overloaded",`+
		`"message":"Our servers are currently overloaded. Please try again later."}}`)
	if _, err := a.SyncAccounts(ctx); err != nil {
		t.Fatalf("SyncAccounts after cooldown: %v", err)
	}

	views := a.Accounts().AllWithVerdict(time.Now())
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	v := views[0]

	if v.Verdict.Kind != accounts.VerdictStale {
		t.Errorf("Kind = %q (%q), want %q", v.Verdict.Kind, v.Verdict.Reason, accounts.VerdictStale)
	}
	if v.Verdict.Blocked {
		t.Error("an expired cooldown must not stop probing")
	}
	// The summary is what the panel renders, so the JSON envelope must not
	// survive into it.
	if strings.Contains(v.Verdict.Reason, "{") {
		t.Errorf("Reason still carries JSON: %q", v.Verdict.Reason)
	}
	if !strings.Contains(v.Verdict.Reason, "server_is_overloaded") {
		t.Errorf("Reason = %q, want the upstream code", v.Verdict.Reason)
	}
	if !strings.Contains(v.Verdict.Detail, "Our servers are currently overloaded") {
		t.Errorf("Detail = %q, want the verbatim message", v.Verdict.Detail)
	}
	// And the account is still a probe target, which is the whole point: the
	// label was wrong, not the behaviour.
	if err := a.Accounts().SetProbeEnabled(ctx, "codex-auth-1", "gpt-5.5", true); err != nil {
		t.Fatalf("SetProbeEnabled: %v", err)
	}
	if got := a.Accounts().EnabledPairs(); len(got) != 1 {
		t.Errorf("EnabledPairs = %v, want the account to stay probeable", got)
	}
}

// TestApp_RewritesTheTimezoneOnTheRequestPath drives the whole way through the
// app: settings, the live snapshot, the injector, and the body the adapter
// would send back.
func TestApp_RewritesTheTimezoneOnTheRequestPath(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))

	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text",` +
		`"text":"<environment_context>\n  <timezone>Asia/Shanghai</timezone>\n` +
		`  <current_date>2026-09-20</current_date>\n</environment_context>"}]}]}`)

	request := func() *hostapi.InterceptedRequest {
		return &hostapi.InterceptedRequest{
			Stage: hostapi.StageAfterAuth, RequestID: "req-tz",
			Model: "gpt-5.5", AuthIndex: "codex-auth-1",
			Headers: http.Header{},
			Body:    append([]byte(nil), body...),
		}
	}

	// Off by default: the body must come back untouched.
	req := request()
	a.InjectState(req)
	if !bytes.Equal(req.Body, body) {
		t.Fatalf("a request was rewritten while disabled:\n%s", req.Body)
	}
	if got := a.TimezoneStats().Disabled; got != 1 {
		t.Errorf("disabled count = %d, want 1", got)
	}

	// Enabled, but with no target: still nothing, which is the requirement.
	if _, err := a.Settings().Update(ctx, settings.Patch{
		TimezoneEnabled: boolPtr(true),
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	req = request()
	a.InjectState(req)
	if !bytes.Equal(req.Body, body) {
		t.Fatalf("a request was rewritten with no target configured:\n%s", req.Body)
	}

	// Enabled with a target: rewritten.
	if _, err := a.Settings().Update(ctx, settings.Patch{
		TimezoneTarget: strPtr("Asia/Tokyo"),
	}); err != nil {
		t.Fatalf("set target: %v", err)
	}
	req = request()
	a.InjectState(req)
	if bytes.Equal(req.Body, body) {
		t.Fatal("the body was not rewritten")
	}
	got := string(req.Body)
	if !strings.Contains(got, "<timezone>Asia/Tokyo</timezone>") {
		t.Errorf("zone not rewritten:\n%s", got)
	}
	// The date is rewritten too, to today's date in the target zone. Shanghai
	// and Tokyo are close enough that it lands on the same day, so the check is
	// against the clock rather than against a fixed string.
	tokyoToday := time.Now().In(mustLoad(t, "Asia/Tokyo")).Format("2006-01-02")
	if !strings.Contains(got, "<current_date>"+tokyoToday+"</current_date>") {
		t.Errorf("date not rewritten to the target zone's today:\n%s", got)
	}
	if strings.Contains(got, "2026-09-20") {
		t.Errorf("the fixture's original date survived:\n%s", got)
	}

	stats := a.TimezoneStats()
	if stats.Matched != 1 || stats.Changed != 1 {
		t.Errorf("counters = %+v, want matched and changed to each be 1", stats)
	}

	// A body with no marker is counted separately, which is the answer to "why
	// does nothing happen".
	plain := request()
	plain.Body = []byte(`{"input":[{"type":"input_text","text":"hello"}]}`)
	a.InjectState(plain)
	if got := a.TimezoneStats().NoMarker; got != 1 {
		t.Errorf("noMarker = %d, want 1", got)
	}
}

// TestApp_TimezoneIsGatedByTheMasterSwitch: it is a request-path intervention,
// so the master switch has to stop it like the others.
func TestApp_TimezoneIsGatedByTheMasterSwitch(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.Settings().Update(ctx, settings.Patch{
		TimezoneEnabled: boolPtr(true),
		TimezoneTarget:  strPtr("Asia/Tokyo"),
		GlobalEnabled:   boolPtr(false),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	body := []byte(`<timezone>Asia/Shanghai</timezone>`)
	req := &hostapi.InterceptedRequest{
		Stage: hostapi.StageAfterAuth, RequestID: "req-off",
		Headers: http.Header{}, Body: append([]byte(nil), body...),
	}
	a.InjectState(req)
	if !bytes.Equal(req.Body, body) {
		t.Errorf("the master switch did not stop the rewrite:\n%s", req.Body)
	}
}

// TestApp_InstructionsAreUntouchedWhenTheMarkerIsAbsent: the rewrite is
// byte-level and anchored, so a conversation that merely mentions a timezone
// must survive unchanged.
func TestApp_InstructionsAreUntouchedWhenTheMarkerIsAbsent(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, mockHost(1))
	if _, err := a.Settings().Update(ctx, settings.Patch{
		TimezoneEnabled: boolPtr(true),
		TimezoneTarget:  strPtr("Asia/Tokyo"),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// "<timezone>" appears in prose inside the user's own message, not as a
	// marker with a closing tag.
	prose := []byte(`{"input":[{"type":"input_text","text":"explain the <timezone> tag"}]}`)
	req := &hostapi.InterceptedRequest{
		Stage: hostapi.StageAfterAuth, RequestID: "req-prose",
		Headers: http.Header{}, Body: append([]byte(nil), prose...),
	}
	a.InjectState(req)
	if !bytes.Equal(req.Body, prose) {
		t.Errorf("prose was rewritten:\n%s", req.Body)
	}
}

func strPtr(s string) *string { return &s }

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation %s: %v", name, err)
	}
	return loc
}
