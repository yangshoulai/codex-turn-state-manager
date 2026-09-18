package web

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestPanelDecodesManagementSession pins the decoding of CPA's management
// session blob.
//
// The panel inherits the management key from localStorage rather than asking
// for it. That format is not a published contract -- it was read out of the
// shipped management bundle -- so a silent change upstream would leave the
// operator staring at a login box for no visible reason. This test re-encodes a
// known session with CPA's own algorithm, transcribed from the bundle, and
// checks the panel decodes it.
//
// Skipped when node is unavailable: the panel is JavaScript, and shelling out
// is only worth it as a guard, not a hard build requirement.
func TestPanelDecodesManagementSession(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping panel decode check")
	}

	script := `
const fs = require('fs');

// CPA's encoder, transcribed from the shipped management bundle.
const ml = 'enc::v1::';
const hl = 'cli-proxy-api-webui::secure-storage';
const enc = new TextEncoder(), dec = new TextDecoder();
const keyBytes = enc.encode(hl + '|example.test:8317|Mozilla/5.0 (Test)');
function xor(a, b) { const o = new Uint8Array(a.length); for (let i = 0; i < a.length; i++) o[i] = a[i] ^ b[i % b.length]; return o; }
function encode(e) {
  const x = xor(enc.encode(e), keyBytes);
  let s = ''; for (const b of x) s += String.fromCharCode(b);
  return ml + btoa(s);
}

const src = fs.readFileSync('app.js', 'utf8');
const start = src.indexOf('const CPA_SESSION_KEY');
const end = src.indexOf('let managementKey = "";');
if (start < 0 || end < 0) { console.error('panel decode helpers not found'); process.exit(2); }

const build = new Function('location', 'navigator', 'localStorage', 'atob', 'btoa',
  src.slice(start, end) + '\nreturn { readInheritedKey };');

function inherit(store) {
  return build({ host: 'example.test:8317' }, { userAgent: 'Mozilla/5.0 (Test)' },
    store, atob, btoa).readInheritedKey();
}
function expect(name, got, want) {
  if (got.key !== want.key || got.reason !== want.reason) {
    console.error(name + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
    process.exit(1);
  }
}

// A remembered session carries the key.
expect('remembered session',
  inherit({ getItem: () => encode(JSON.stringify({ state: { managementKey: 'local-key' } })) }),
  { key: 'local-key', reason: 'ok' });

// Plain JSON still works, so a format change degrades to the login prompt
// rather than breaking outright.
expect('plain session',
  inherit({ getItem: () => JSON.stringify({ managementKey: 'plain-key' }) }),
  { key: 'plain-key', reason: 'ok' });

// The management center only persists the key when "remember password" was
// ticked. That case must be reported distinctly, or the operator sees a login
// form with no idea why.
expect('session without key',
  inherit({ getItem: () => encode(JSON.stringify({ state: { apiBase: '/v0/management', rememberPassword: false } })) }),
  { key: '', reason: 'session-without-key' });

expect('no session', inherit({ getItem: () => null }), { key: '', reason: 'no-session' });

expect('corrupt session',
  inherit({ getItem: () => 'enc::v1::not-valid-base64!!' }),
  { key: '', reason: 'undecodable' });

process.exit(0);
`

	cmd := exec.Command(node, "-e", script)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("panel session decode check failed: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Logf("node output: %s", out)
	}
}

// TestPanelSurfacesTheKeyInTheConfigArea covers the layout the operator asked
// for: the key is visible and editable in one place, rather than hidden behind
// a toggle in the chrome.
//
// The earlier design masked it behind a "show" button in the top bar, which
// meant answering "is a key configured, and is it the right one?" took two
// clicks and still did not show the value in place. A password input in the
// config area answers both at a glance while still not printing the key on
// screen by default.
func TestPanelSurfacesTheKeyInTheConfigArea(t *testing.T) {
	html, err := ReadAsset("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	page := string(html)

	for _, want := range []string{`id="mgmt-key"`, `id="mgmt-key-apply"`, `id="mgmt-key-source"`} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html is missing %s", want)
		}
	}
	// It must be a password field: shown in place, still not printed by default.
	if !strings.Contains(page, `id="mgmt-key" type="password"`) {
		t.Error("the key field should be a password input")
	}
	// The key belongs inside the config section, not in the top bar.
	keyAt := strings.Index(page, `id="mgmt-key"`)
	configAt := strings.Index(page, `id="config-body"`)
	accountsAt := strings.Index(page, `id="accounts"`)
	if !(configAt < keyAt && keyAt < accountsAt) {
		t.Error("the key field should sit inside the config section, above the account list")
	}
	// And it has to be seeded, or "is a key configured" is unanswerable.
	if !strings.Contains(string(js), "function fillKeyField(value, source)") {
		t.Error("app.js never fills the key field")
	}
	if !strings.Contains(string(js), "fillKeyField(inheritedKey || managementKey") {
		t.Error("enterPanel does not seed the key field with the working key")
	}
}

// TestPanelCollapsesConfigByDefault pins that the account matrix is the
// protagonist: configuration is one click away, not in the way.
func TestPanelCollapsesConfigByDefault(t *testing.T) {
	html, err := ReadAsset("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	page := string(html)

	if !strings.Contains(page, `id="config-body" hidden`) {
		t.Error("the config body should start collapsed")
	}
	if !strings.Contains(page, `id="config-toggle"`) {
		t.Error("no control expands the config")
	}
	if !strings.Contains(page, `aria-expanded="false"`) {
		t.Error("the toggle should start with aria-expanded=false")
	}
	// The collapsed header must still say something useful.
	if !strings.Contains(string(js), "function refreshConfigSummary()") {
		t.Error("the collapsed header has no summary")
	}
	// Collapse state is a UI preference, not a secret, so remembering it is fine.
	if !strings.Contains(string(js), "CONFIG_OPEN_KEY") {
		t.Error("the collapse state is not remembered")
	}
}

// TestPanelGroupsProxyPoolWithConfig covers the requested move: the proxy pool
// is configuration, so it lives with the rest of it rather than as a peer card
// competing with the account list.
func TestPanelGroupsProxyPoolWithConfig(t *testing.T) {
	html, err := ReadAsset("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	page := string(html)

	configAt := strings.Index(page, `id="config-body"`)
	proxiesAt := strings.Index(page, `id="proxies"`)
	accountsAt := strings.Index(page, `id="accounts"`)
	if proxiesAt < 0 {
		t.Fatal("no proxy list in the page")
	}
	if !(configAt < proxiesAt && proxiesAt < accountsAt) {
		t.Error("the proxy pool should sit inside the config section, above the account list")
	}
}

// TestPanelShipsDefaults keeps the restore action honest: it can only offer
// defaults it actually knows.
func TestPanelShipsDefaults(t *testing.T) {
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(js)

	for _, field := range []string{
		"globalEnabled", "globalProbeEnabled", "globalReverseBindEnabled",
		"statePriorityEnabled", "scanIntervalSec", "probeConcurrency",
		"stateTtlMin", "refreshThresholdPct", "targetStateLength",
		"maxProbeDurationSec", "routingStrategy",
	} {
		if !strings.Contains(src, field+":") {
			t.Errorf("DEFAULTS is missing %s", field)
		}
	}
	if !strings.Contains(src, `on("restore-defaults"`) {
		t.Error("no control restores the defaults")
	}
	// Every numeric input should say what its default is, so the field is
	// readable without the restore action.
	html, err := ReadAsset("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if got := strings.Count(string(html), "field-hint"); got < 6 {
		t.Errorf("%d field hints, want at least one per numeric setting", got)
	}
}

// TestPanelBindsListenersDefensively guards the failure that made the panel ask
// for a key it should have inherited.
//
// The panel is one script of top-level bindings. A single null element lookup
// aborts everything after it, including the bootstrap that decides between
// inheriting a key and asking for one -- and because the login form is the
// visible fallback, the symptom points at authentication instead of at the
// missing element. Every binding goes through on(), and the form starts hidden
// so a dead script is not mistaken for a prompt.
func TestPanelBindsListenersDefensively(t *testing.T) {
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	html, err := ReadAsset("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}

	// No unguarded top-level binding may remain.
	for _, line := range strings.Split(string(js), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `$("`) &&
			strings.Contains(line, "addEventListener") {
			t.Errorf("unguarded top-level binding: %s", strings.TrimSpace(line))
		}
	}

	if !strings.Contains(string(js), "function on(id, event, handler)") {
		t.Error("app.js has no guarded binding helper")
	}
	// Bootstrap must report its own failure rather than leaving the gate up.
	if !strings.Contains(string(js), "bootstrap failed") {
		t.Error("a bootstrap failure would be silent")
	}
	// The login form must start hidden: showing it while the script is still
	// deciding is what made a dead script look like an auth problem.
	if !strings.Contains(string(html), `id="gate" hidden`) {
		t.Error("the login gate is visible by default")
	}
}

// TestStylesheetHidesHiddenElements pins the CSS rule that made the login gate
// render on top of a working panel.
//
// An author `display` declaration outranks the user agent's [hidden] rule, so
// `.gate { display: flex }` kept the form on screen while the DOM property read
// hidden=true -- every programmatic check reported success and the user still
// saw a login box. One global rule fixes it; a per-selector rule for each
// element would be a second source of truth for the same behaviour.
func TestStylesheetHidesHiddenElements(t *testing.T) {
	css, err := ReadAsset("style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	sheet := string(css)

	if !strings.Contains(sheet, "[hidden] { display: none !important; }") {
		t.Error("no global [hidden] rule; any element with an author display value will stay visible")
	}

	// A layout rule on an element that is toggled via the hidden attribute is
	// the exact combination that caused the bug, so make sure both remain
	// present and the global rule is what covers it.
	if !strings.Contains(sheet, ".gate { display: flex") {
		t.Error("expected .gate to still be laid out with flex")
	}
	for _, redundant := range []string{".gate[hidden]", "main[hidden]"} {
		if strings.Contains(sheet, redundant) {
			t.Errorf("%s is redundant; the global [hidden] rule is the single authority", redundant)
		}
	}
}

// TestStylesheetSeparatesGroupsOnce pins that a section divider and the first
// row under it do not each draw a line.
//
// The original rule was `.switch-row:first-of-type { border-top: none }`, which
// silently did nothing: `:first-of-type` matches by element type, and the first
// <div> in that container is the management-key row, so no switch row was ever
// "first". The group rendered with two hairlines above it, which is only
// visible by looking at the page. The fix states the relationship instead.
// stripCSSComments removes /* ... */ blocks so rule assertions do not match the
// comments that explain them.
func stripCSSComments(css string) string {
	out := css
	for {
		start := strings.Index(out, "/*")
		if start < 0 {
			return out
		}
		end := strings.Index(out[start:], "*/")
		if end < 0 {
			return out[:start]
		}
		out = out[:start] + out[start+end+2:]
	}
}

func TestStylesheetSeparatesGroupsOnce(t *testing.T) {
	css, err := ReadAsset("style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	sheet := string(css)

	if !strings.Contains(sheet, ".divider + .switch-row { border-top: none; }") {
		t.Error("no rule stops a switch row from doubling the divider above it")
	}
	// Type-based positional selectors are the trap this fell into. Check the
	// rules, not the prose: the file explains the trap in a comment, and a
	// naive substring match flags its own documentation.
	if strings.Contains(stripCSSComments(sheet), ":first-of-type") ||
		strings.Contains(stripCSSComments(sheet), ":nth-of-type") {
		t.Error("positional type selectors are in use; they match by element type, not by class")
	}
	// The remaining switches still need their own separators.
	if !strings.Contains(sheet, ".switch-row {") {
		t.Error("switch rows lost their separator entirely")
	}
}

// TestPanelCallsOnlyDefinedFunctions guards against a rename leaving a stale
// call behind.
//
// Renaming loadModelSuggestions to loadModelCatalog left one call site behind,
// which surfaced in the browser as "loadModelSuggestions is not defined" and
// only because the panel reports its own bootstrap failure. A static check is
// cheaper than that round trip.
//
// Declarations have to be collected without assuming where they appear: the
// panel declares helpers inside other functions (`const pad = ...`, `const set = ...`)
// and uses a named function expression for its bootstrap, none of which an
// anchored pattern finds.
func TestPanelCallsOnlyDefinedFunctions(t *testing.T) {
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(js)

	declared := map[string]bool{}
	patterns := []string{
		`(?:async\s+)?function\s+([A-Za-z_$][\w$]*)`,                      // declarations and named expressions
		`(?m)^\s*(?:const|let|var)\s+([A-Za-z_$][\w$]*)`,                  // bindings, however indented
		`(?m)^\s*([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\(|[A-Za-z_$])`, // bare assignment
	}
	for _, pattern := range patterns {
		for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(src, -1) {
			declared[m[1]] = true
		}
	}
	if len(declared) < 30 {
		t.Fatalf("only %d declarations found; the patterns are not reading the file", len(declared))
	}

	// Browser and language builtins the panel is entitled to call.
	allowed := map[string]bool{
		"atob": true, "btoa": true, "fetch": true, "setTimeout": true, "clearTimeout": true,
		"setInterval": true, "clearInterval": true, "confirm": true, "alert": true, "prompt": true,
		"parseInt": true, "parseFloat": true, "isNaN": true, "isFinite": true,
		"decodeURIComponent": true, "encodeURIComponent": true, "structuredClone": true,
		"if": true, "for": true, "while": true, "switch": true, "catch": true, "return": true,
		"typeof": true, "function": true, "await": true, "async": true, "new": true,
		"case": true, "do": true, "delete": true, "void": true, "in": true, "of": true,
		"TextEncoder": true, "TextDecoder": true, "Uint8Array": true, "URL": true,
		"URLSearchParams": true, "MutationObserver": true, "Event": true,
	}

	var missing []string
	// A call is a name followed by "(" that is not a property access.
	for _, m := range regexp.MustCompile(`(?:^|[^.\w$])([a-z_$][\w$]*)\s*\(`).FindAllStringSubmatch(src, -1) {
		name := m[1]
		if declared[name] || allowed[name] {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		t.Errorf("calls to undeclared functions: %s", strings.Join(dedupe(missing), ", "))
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
