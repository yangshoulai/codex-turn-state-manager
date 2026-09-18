package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/proxies"
	"github.com/yangshoulai/codex-turn-state-manager/internal/settings"
	"github.com/yangshoulai/codex-turn-state-manager/internal/states"
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
			Status:    hostapi.AccountStatusAvailable,
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

	// Sanity: with the switch on, all four are live.
	req := &hostapi.InterceptedRequest{
		RequestID: "req-1", Model: "gpt-5-codex", Headers: http.Header{},
	}
	if got := a.BeforeAuth(req); got.Action != "correlated" {
		t.Fatalf("BeforeAuth action = %s, want correlated", got.Action)
	}
	a.Correlation().AttachAuth("req-1", "auth-id-1", "codex-auth-1")
	if got := a.AfterAuth(req); got.Action != "injected" {
		t.Fatalf("AfterAuth action = %s, want injected", got.Action)
	}
	pick := a.PickCredential(ctx, hostapi.SchedulerPickRequest{
		RequestID: "req-1", Provider: hostapi.ProviderCodex, Model: "gpt-5-codex",
		Candidates: []hostapi.Candidate{{AuthID: "auth-id-1", AuthIndex: "codex-auth-1", Priority: 1}},
	})
	if pick.Delegate {
		t.Error("expected the plugin to steer routing while the master switch is on")
	}

	// Now turn it off.
	off := false
	if _, err := a.Settings().Update(ctx, settings.Patch{GlobalEnabled: &off}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	req2 := &hostapi.InterceptedRequest{
		RequestID: "req-2", Model: "gpt-5-codex", Headers: http.Header{},
	}
	if got := a.BeforeAuth(req2); got.Action != "passthrough" {
		t.Errorf("BeforeAuth action = %s, want passthrough", got.Action)
	}
	if req2.Headers.Get("X-CPA-Turn-State-Correlation") != "" {
		t.Error("no correlation header may be set while the master switch is off")
	}
	if got := a.AfterAuth(req2); got.Action != "passthrough" {
		t.Errorf("AfterAuth action = %s, want passthrough", got.Action)
	}
	if req2.Headers.Get("X-Codex-Turn-State") != "" {
		t.Error("state must not be injected while the master switch is off")
	}

	a.Correlation().AttachAuth("req-2", "auth-id-1", "codex-auth-1")
	captured := a.ObserveResponse(ctx, hostapi.ResponseHeaders{
		RequestID: "req-2", Model: "gpt-5-codex", Status: 200,
		Header: http.Header{"X-Codex-Turn-State": []string{stateOf(targetLength)}},
	})
	if captured.Action != "skipped" {
		t.Errorf("capture action = %s, want skipped", captured.Action)
	}

	pick2 := a.PickCredential(ctx, hostapi.SchedulerPickRequest{
		RequestID: "req-2", Provider: hostapi.ProviderCodex, Model: "gpt-5-codex",
		Candidates: []hostapi.Candidate{{AuthID: "auth-id-1", AuthIndex: "codex-auth-1", Priority: 1}},
	})
	if !pick2.Delegate {
		t.Error("routing must defer to CPA while the master switch is off")
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

	base := srv.URL + "/v0/management/plugins/codex-turn-state-manager"

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
	base := srv.URL + "/v0/management/plugins/codex-turn-state-manager"

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
