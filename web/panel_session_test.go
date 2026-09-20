package web

import (
	"fmt"
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

// The two encoders in the wild, transcribed from the panels that write them.
const hl = 'cli-proxy-api-webui::secure-storage';
const HOST = 'example.test:8317';
const UA = 'Mozilla/5.0 (Test)';
const enc = new TextEncoder();
function xor(a, b) { const o = new Uint8Array(a.length); for (let i = 0; i < a.length; i++) o[i] = a[i] ^ b[i % b.length]; return o; }
function obfuscate(prefix, key, e) {
  const x = xor(enc.encode(e), enc.encode(key));
  let s = ''; for (const b of x) s += String.fromCharCode(b);
  return prefix + btoa(s);
}
// v1: what CPA's own management center writes. Keyed on the user agent.
const encode = (e) => obfuscate('enc::v1::', hl + '|' + HOST + '|' + UA, e);
// v2: what CPA-Manager-Plus writes. The user agent is gone, which is the point
// of the version -- v1 stops decoding whenever the browser updates.
const encodeV2 = (e) => obfuscate('enc::v2::', hl + '|v2|' + HOST, e);

const src = fs.readFileSync('app.js', 'utf8');
const start = src.indexOf('const CPA_SESSION_KEY');
const end = src.indexOf('let managementKey = "";');
if (start < 0 || end < 0) { console.error('panel decode helpers not found'); process.exit(2); }

const build = new Function('location', 'navigator', 'localStorage', 'atob', 'btoa',
  src.slice(start, end) + '\nreturn { readInheritedKey };');

function inherit(store) {
  return build({ host: HOST }, { userAgent: UA }, store, atob, btoa).readInheritedKey();
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

// A session written by CPA-Manager-Plus instead of CPA's own panel. Reading
// only v1 reported "undecodable", which is the login prompt operators hit when
// the two panels share one browser.
expect('v2 session from the other panel',
  inherit({ getItem: () => encodeV2(JSON.stringify({ state: { managementKey: 'v2-key' } })) }),
  { key: 'v2-key', reason: 'ok' });

// v2 deliberately omits the user agent, so a different UA must not matter.
expect('v2 session survives a browser update',
  build({ host: HOST }, { userAgent: 'Mozilla/5.0 (Updated)' },
    { getItem: () => encodeV2(JSON.stringify({ state: { managementKey: 'v2-key' } })) },
    atob, btoa).readInheritedKey(),
  { key: 'v2-key', reason: 'ok' });

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
	src := stripWholeLineComments(string(js))

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

// TestStylesheetCoversEveryControlType keeps the panel's controls on one shared
// shape.
//
// Two defects shipped here, both invisible to a DOM check and obvious on screen.
// The select in the probe-history header was left out of the rule that styles
// text/number/password inputs, so it rendered as a raw native control at a
// different height and radius from everything beside it. Separately, a CJK
// button label has no spaces to break at, so a narrow flex row wrapped
// "同步账号" onto two lines -- each container was patched on its own before the
// rule was put on .btn where it belongs.
func TestStylesheetCoversEveryControlType(t *testing.T) {
	css, err := ReadAsset("style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	sheet := stripCSSComments(string(css))

	// The shared control rule is the one that styles text inputs; every other
	// control type the panel renders has to be in the same selector list.
	rule := ruleForSelector(sheet, `input[type="text"]`)
	if rule == "" {
		t.Fatal("no shared control rule styles text inputs")
	}
	for _, selector := range []string{`input[type="number"]`, `input[type="password"]`, "select"} {
		if !strings.Contains(rule, selector) {
			t.Errorf("%s is not in the shared control rule; it will render at native metrics", selector)
		}
	}

	btn := ruleForSelector(sheet, ".btn")
	if btn == "" {
		t.Fatal("no .btn rule")
	}
	if !strings.Contains(btn, "white-space: nowrap") {
		t.Error(".btn does not set white-space: nowrap; a CJK label in a flex row wraps mid-word")
	}
}

// ruleForSelector returns the declaration block whose selector list contains
// want as a whole selector, so callers assert on the rule as a unit. Matching
// the raw text instead would treat ".btn" as present inside ".btn-primary".
func ruleForSelector(sheet, want string) string {
	for _, block := range strings.Split(sheet, "}") {
		open := strings.Index(block, "{")
		if open < 0 {
			continue
		}
		for _, sel := range strings.Split(block[:open], ",") {
			// Drop a leading comment remnant and the selector's own line breaks.
			sel = strings.TrimSpace(sel)
			if i := strings.LastIndex(sel, "*/"); i >= 0 {
				sel = strings.TrimSpace(sel[i+2:])
			}
			if sel == want {
				return block + "}"
			}
		}
	}
	return ""
}

// TestPanelAssetURLsCarryTheVersion pins the cache-busting that makes an update
// take effect behind a CDN.
//
// Measured against a real deployment: the CDN cached app.js and style.css with
// its own 4h TTL, overriding the no-cache this package sets, while index.html
// passed through untouched. A browser refresh -- even a hard one, which only
// bypasses the browser's cache -- therefore kept loading the previous release's
// panel. The version in the URL is what makes each release a distinct resource.
func TestPanelAssetRoutesCarryAContentHash(t *testing.T) {
	served, err := ServedAsset("/index.html")
	if err != nil {
		t.Fatalf("serve index.html: %v", err)
	}
	html := string(served)

	// The served HTML must ask for the hashed routes, not the bare file names.
	for _, bare := range []string{`"app.js"`, `"style.css"`} {
		if strings.Contains(html, bare) {
			t.Errorf("index.html still refers to %s", bare)
		}
	}

	var hashed []string
	for _, route := range Assets {
		if route == "/index.html" {
			continue
		}
		hashed = append(hashed, route)
		// Relative: the panel is served from a subpath, so an absolute
		// reference would resolve to the site root and 404.
		if !strings.Contains(html, `"`+strings.TrimPrefix(route, "/")+`"`) {
			t.Errorf("index.html does not refer to %s", route)
		}
		// A hash in the name is what makes the route safe to cache forever.
		if !regexp.MustCompile(`/[a-z]+\.[0-9a-f]{12}\.(js|css)$`).MatchString(route) {
			t.Errorf("asset route %q does not carry a content hash", route)
		}
		if !strings.Contains(CacheControl(route), "immutable") {
			t.Errorf("asset route %q is not cacheable immutably", route)
		}
	}
	if len(hashed) != 2 {
		t.Fatalf("expected 2 hashed routes, got %d: %v", len(hashed), hashed)
	}

	// index.html is the entry point the host menu links to, so its address is
	// fixed and it must stay revalidated.
	if CacheControl("/index.html") != "no-cache" {
		t.Errorf("index.html Cache-Control = %q, want no-cache", CacheControl("/index.html"))
	}

	// Asset bodies are served byte for byte: rewriting them would corrupt them.
	raw, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	jsRoute := ""
	for _, route := range Assets {
		if strings.HasSuffix(route, ".js") {
			jsRoute = route
		}
	}
	got, err := ServedAsset(jsRoute)
	if err != nil {
		t.Fatalf("serve %s: %v", jsRoute, err)
	}
	if string(got) != string(raw) {
		t.Error("app.js was modified on the way out")
	}

	// An unknown route is not a file.
	if _, err := ServedAsset("/app.js"); err == nil {
		t.Error("the bare app.js route still resolves; it must not be served")
	}
}

// TestPanelWeekdayMappingRoundTrips covers the day-list mapping in both
// directions.
//
// An empty list means "every day" to the server, and the panel used to render
// that as none-checked -- so choosing all seven, saving, and watching the
// selection disappear was reproducible, and the panel could not display the
// state it had just written. The two directions are pure functions now so this
// is asserted rather than eyeballed.
func TestPanelWeekdayMappingRoundTrips(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping panel weekday check")
	}

	script := `
const fs = require('fs');
const src = fs.readFileSync('app.js', 'utf8');
const start = src.indexOf('const DAY_NAMES');
const end = src.indexOf('async function saveWindow');
if (start < 0 || end < 0) { console.error('weekday helpers not found'); process.exit(2); }
const build = new Function(src.slice(start, end) + '\nreturn { daysToChecked, checkedToDays };');
const { daysToChecked, checkedToDays } = build();

function fail(msg) { console.error(msg); process.exit(1); }
function eq(got, want, label) {
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    fail(label + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
  }
}

// "Every day" has to show as all seven selected, which is the bug being fixed.
eq(daysToChecked([]), [true, true, true, true, true, true, true], 'empty means every day');
eq(daysToChecked(undefined), [true, true, true, true, true, true, true], 'missing means every day');
eq(daysToChecked([1, 2, 3, 4, 5]), [false, true, true, true, true, true, false], 'weekdays');
eq(daysToChecked([0, 6]), [true, false, false, false, false, false, true], 'weekend');

// A fake cell: checkedToDays only reads checkbox inputs.
function cell(checked) {
  return { querySelectorAll: () => checked.map((on) => ({ checked: on })) };
}
eq(checkedToDays(cell([true, true, true, true, true, true, true])), [], 'all checked canonicalises to every day');
eq(checkedToDays(cell([false, true, true, true, true, true, false])), [1, 2, 3, 4, 5], 'weekdays');
eq(checkedToDays(cell([false, true, false, false, false, false, false])), [1], 'single day');

// Round trip: what is rendered must parse back to the same stored value.
for (const stored of [[], [1, 2, 3, 4, 5], [0, 6], [3]]) {
  const rendered = daysToChecked(stored);
  const reparsed = checkedToDays(cell(rendered));
  const want = stored.length === 7 ? [] : stored;
  eq(reparsed, want, 'round trip of ' + JSON.stringify(stored));
}
process.exit(0);
`
	cmd := exec.Command(node, "-e", script)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("panel weekday check failed: %v\n%s", err, out)
	}
}

// TestPanelTargetLengthIsDescribedAsAFallback guards the one place the rule is
// read by an operator rather than by the code.
//
// The setting kept its name and its number when the shape became per-plan, and
// the panel went on saying "only this length is bound" -- which is false for
// every account whose plan is known. The behaviour was documented and tested;
// the sentence the operator actually reads was not.
func TestPanelTargetLengthIsDescribedAsAFallback(t *testing.T) {
	html, err := ReadAsset("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	page := string(html)

	// The old absolute claim must not come back.
	if strings.Contains(page, "只有这个长度会被绑定") {
		t.Error("the panel still claims the configured length is the only one bound")
	}
	// And the control has to say what it is now for.
	label := "目标长度（兜底）"
	if !strings.Contains(page, label) {
		t.Errorf("index.html does not label the setting as %q", label)
	}
	for _, want := range []string{"套餐未知时使用", "332"} {
		if !strings.Contains(page, want) {
			t.Errorf("the setting's hint does not mention %q", want)
		}
	}
}

// TestPanelProxyMergeKeepsUnsavedRows is the regression guard for a row that
// vanished mid-edit.
//
// The proxy pool is one list kept in two places, and the server's copy replaced
// the local one on every round trip. A row that had been added but not yet
// saved was therefore dropped when a periodic refresh landed, taking whatever
// had been typed into it -- reported as the entry box disappearing at the fifth
// node and the address never being recorded.
//
// mergeProxies is pure, so the rules can be asserted directly rather than
// through a browser: a draft survives, a draft the server has confirmed is
// handed over to the server's copy, and a row the server no longer lists is not
// resurrected.
func TestPanelProxyMergeKeepsUnsavedRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping panel proxy merge check")
	}

	script := `
const fs = require('fs');
const src = fs.readFileSync('app.js', 'utf8');
const start = src.indexOf('function mergeProxies');
const end = src.indexOf('function renderProxies');
if (start < 0 || end < 0) { console.error('mergeProxies not found'); process.exit(2); }

// proxiesState is a module-level binding in the panel; injecting it as a
// parameter of the same name is what lets the function resolve it here.
function withState(state) {
  return new Function('proxiesState', src.slice(start, end) + '\nreturn mergeProxies;')(state);
}
function fail(msg) { console.error(msg); process.exit(1); }
function eq(got, want, label) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g !== w) fail(label + ': got ' + g + ', want ' + w);
}

const saved = (id, url) => ({ id, url, enabled: true });

// A row the server has never seen survives, whatever it holds.
eq(withState([{ id: 'p1', url: '', draft: true }])([]),
   [{ id: 'p1', url: '', draft: true }], 'an empty draft survives');

eq(withState([{ id: 'p1', url: 'http://typing.example:1', draft: true }])([]),
   [{ id: 'p1', url: 'http://typing.example:1', draft: true }],
   'a draft being typed survives');

// Once the server lists the address the server's copy is the one kept.
eq(withState([{ id: 'p1', url: 'http://a.example:1', draft: true }])([saved('a.example:1', 'http://a.example:1')]),
   [saved('a.example:1', 'http://a.example:1')],
   'a confirmed draft is not duplicated despite the id changing');

// A server row with no local counterpart is passed through untouched.
const two = [saved('a.example:1', 'http://a.example:1'), saved('b.example:2', 'http://b.example:2')];
eq(withState([])(two), two, 'server rows pass through');

// A row removed elsewhere is not resurrected by a stale local copy.
eq(withState([{ id: 'gone', url: 'http://gone.example:1' }])([]), [],
   'a non-draft row the server dropped does not come back');

// Mixed: saved rows keep their order, drafts follow.
eq(withState([
     { id: 'a.example:1', url: 'http://a.example:1', draft: true },
     { id: 'draft', url: '', draft: true },
   ])([saved('a.example:1', 'http://a.example:1')]),
   [saved('a.example:1', 'http://a.example:1'), { id: 'draft', url: '', draft: true }],
   'saved first, draft appended');
process.exit(0);
`
	cmd := exec.Command(node, "-e", script)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("panel proxy merge check failed: %v\n%s", err, out)
	}

	// The function's rules are only half of it: the defect was that the state
	// was replaced wholesale at the call sites, and a test of the function
	// cannot see that. Every path that adopts a server list has to go through
	// the merge, so assert that on the source.
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	source := string(js)
	for _, call := range []string{
		"proxiesState = mergeProxies(payload.proxies || [])",
		"proxiesState = mergeProxies(resp.proxies || [])",
	} {
		if !strings.Contains(source, call) {
			t.Errorf("a server round trip adopts the response without merging: %s", call)
		}
	}
	if strings.Contains(source, "proxiesState = payload.proxies") ||
		strings.Contains(source, "proxiesState = resp.proxies") {
		t.Error("the proxy state is replaced wholesale again; unsaved rows would vanish")
	}
}

// stripWholeLineComments drops lines that are entirely a comment before a
// source scan looks at them.
//
// The scan that guards against a renamed function is textual, so prose counts
// as code: a comment reading "by address (its host)" was reported as a call to
// an undeclared function named address. Only whole-line comments are removed,
// deliberately -- a naive strip of everything after "//" would cut a line at
// the "//" inside "http://..." and hide real code, which is a worse failure
// than the false positive it fixes.
func stripWholeLineComments(src string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			out = append(out, "")
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestPanelDoesNotNestButtonsInTheAccountHeader guards a markup invariant that
// is invisible until it is clicked.
//
// The account header used to be a single <button> wrapping the whole row,
// which left nowhere to put the per-account 同步 and 调用历史 controls except
// inside it. A button inside a button is invalid, and in practice activating
// the inner one also fired the outer one -- so syncing an account collapsed it
// at the same time.
func TestPanelDoesNotNestButtonsInTheAccountHeader(t *testing.T) {
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := stripWholeLineComments(string(js))

	// Three regions, in source order: the toggle button, the sibling actions,
	// and the header that contains both.
	toggleStart := strings.Index(src, "const toggle = el(\"button\"")
	actionsStart := strings.Index(src, "const actions = el(\"div\", { class: \"row-actions\" }")
	headStart := strings.Index(src, "const head = el(\"div\", { class: \"account-head\" }")
	if toggleStart < 0 || actionsStart < 0 || headStart < 0 {
		t.Fatal("the account header was not found; this guard is not reading the file")
	}
	if !(toggleStart < actionsStart && actionsStart < headStart) {
		t.Fatal("the account header is not laid out as toggle, then actions, then container")
	}

	toggle := src[toggleStart:actionsStart]
	if got := strings.Count(toggle, "el(\"button\""); got != 1 {
		t.Errorf("the toggle region contains %d buttons, want exactly 1", got)
	}
	if strings.Contains(toggle, "syncAccount(") || strings.Contains(toggle, "showCallHistory(") {
		t.Error("a row action is nested inside the toggle button")
	}

	actions := src[actionsStart:headStart]
	if !strings.Contains(actions, "syncAccount(") {
		t.Error("the account does not offer a per-account sync")
	}
	if !strings.Contains(actions, "showCallHistory(") {
		t.Error("the account does not offer the call history")
	}
}

// TestPanelSyncsModelsWithTheAccountButton pins that the one button does both
// halves of "make this account current".
//
// Splitting it -- a modifier key, a second button -- would make the panel's
// behaviour depend on something the operator cannot see. The model fetch is one
// request against the account's own quota and happens only when pressed.
func TestPanelSyncsModelsWithTheAccountButton(t *testing.T) {
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := stripWholeLineComments(string(js))

	if !strings.Contains(src, "models: 1") {
		t.Error("the per-account sync does not ask for a model refresh")
	}
	if !strings.Contains(src, "/accounts/sync?") {
		t.Error("the per-account sync does not call the sync endpoint")
	}
}

// TestPanelNeverRendersFullProxyURLsInHistory keeps credentials off the screen.
//
// A proxy URL carries its password, and the history tables render dozens of
// rows nobody reads closely -- a truncated password there is both useless and a
// secret in the page for no reason. scheme://host:port is what the column needs
// to answer, and the full address stays editable in the pool table.
func TestPanelNeverRendersFullProxyURLsInHistory(t *testing.T) {
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := stripWholeLineComments(string(js))

	start := strings.Index(src, "function proxyLabel(")
	if start < 0 {
		t.Fatal("proxyLabel was not found")
	}
	end := strings.Index(src[start:], "function maskProxyURL(")
	if end < 0 {
		t.Fatal("proxyLabel does not delegate to maskProxyURL")
	}
	body := src[start : start+end]

	if strings.Contains(body, "return node ? node.url") {
		t.Error("proxyLabel returns the raw URL, credentials and all")
	}
	if !strings.Contains(body, "maskProxyURL(") {
		t.Error("proxyLabel does not mask the address")
	}
}

// TestPanelFillsTheHostPage guards the layout rule that the panel is a plugin
// page, not a document.
//
// It was capped at 1080px and centred, which inside CPA's plugin page left a
// third of a wide screen as dead margin while the account tables -- the densest
// thing on the page -- scrolled horizontally instead.
func TestPanelFillsTheHostPage(t *testing.T) {
	css, err := ReadAsset("style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	sheet := stripCSSComments(string(css))

	main := cssBlock(sheet, "main {")
	if main == "" {
		t.Fatal("the main rule was not found")
	}
	if strings.Contains(main, "max-width") {
		t.Errorf("main caps its width:\n%s", main)
	}
	if strings.Contains(main, "margin: 0 auto") {
		t.Errorf("main centres itself instead of filling the page:\n%s", main)
	}
}

// cssBlock returns the body of the first rule whose selector line matches.
func cssBlock(sheet, selector string) string {
	start := strings.Index(sheet, selector)
	if start < 0 {
		return ""
	}
	end := strings.Index(sheet[start:], "}")
	if end < 0 {
		return ""
	}
	return sheet[start : start+end]
}

// TestPanelLoadsModelsBeforeTheAccounts guards the fix for the permanent
// "加载中…".
//
// loadAll never fetched the model tables at all, so the first render of an
// expanded account always showed the placeholder and only the 15-second refresh
// replaced it. On first connect that reads as a panel that cannot load models.
func TestPanelLoadsModelsBeforeTheAccounts(t *testing.T) {
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := stripWholeLineComments(string(js))

	start := strings.Index(src, "async function loadAll()")
	if start < 0 {
		t.Fatal("loadAll was not found")
	}
	end := strings.Index(src[start:], "\n}")
	if end < 0 {
		t.Fatal("the end of loadAll was not found")
	}
	body := src[start : start+end]

	models := strings.Index(body, "loadAllModels()")
	accounts := strings.Index(body, "loadAccounts()")
	if models < 0 {
		t.Fatal("loadAll does not load the model tables")
	}
	if accounts < 0 {
		t.Fatal("loadAll does not load the accounts")
	}
	if models > accounts {
		t.Error("loadAll loads accounts before models, so the first render shows the placeholder")
	}
}

// TestPanelDistinguishesACooldownFromAPausedAccount pins the label that caused
// the most confusion in production.
//
// "已暂停探测" means probing has actually stopped, and only four states cause
// that. CPA sets error/Unavailable/NextRetryAfter for a temporary quota or rate
// limit, which the plugin deliberately keeps probing -- showing the paused label
// for one made an account the operator had already refreshed look written off.
func TestPanelDistinguishesACooldownFromAPausedAccount(t *testing.T) {
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := stripWholeLineComments(string(js))

	if !strings.Contains(src, "verdict.blocked") {
		t.Error("the panel does not branch on whether probing is actually blocked")
	}
	if !strings.Contains(src, "限流冷却") {
		t.Error("a cooldown has no label of its own")
	}
	if !strings.Contains(src, "verdictPill(") {
		t.Error("the verdict is not rendered")
	}
}

// TestPanelOffersTheCallHistory keeps the new endpoint wired to a control.
func TestPanelOffersTheCallHistory(t *testing.T) {
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := stripWholeLineComments(string(js))

	if !strings.Contains(src, "/call-history?") {
		t.Error("the panel never calls the call-history endpoint")
	}
	if !strings.Contains(src, "showCallHistory(") {
		t.Error("no control opens the call history")
	}
	// Paging: the history is bounded by retention, not by a row count, so a
	// page is mandatory rather than a nicety.
	if !strings.Contains(src, "callPage.offset") {
		t.Error("the call history has no pager")
	}
}

// TestPanelShowsTheProbeStatus keeps the new column wired to the data.
//
// The probe history recorded only the outcome until migration v6, and
// UPSTREAM_ERROR covers any 5xx plus a non-model 400/404 -- so a 400 (the
// request shape was rejected) and a 503 (upstream is unwell) rendered
// identically, while the right response to each is different.
func TestPanelShowsTheProbeStatus(t *testing.T) {
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := stripWholeLineComments(string(js))

	if !strings.Contains(src, "p.statusCode") {
		t.Error("the probe table does not render the recorded status")
	}
	if !strings.Contains(src, "httpStatusCell(") {
		t.Error("no status renderer")
	}
	// 0 means no response arrived. Rendering it as "0" would read like a
	// status code, so the zero case has to say what kind of nothing it was.
	if !strings.Contains(src, "NO_RESPONSE") {
		t.Error("the no-response case has no wording of its own")
	}
	// The labels sit in a width-pinned column, so they have to stay short: a
	// long phrase truncates and takes the explanation with it. The full
	// sentence belongs in the tooltip.
	for _, long := range []string{"无响应（无可用代理）", "无响应（超时）", "无响应（网络）"} {
		if strings.Contains(src, long) {
			t.Errorf("label %q is too long for the pinned column; use the tooltip", long)
		}
	}
}

// TestStylesheetProbeColumnsAreNumberedForEight guards the positional column
// widths.
//
// They are nth-child rules, so inserting a column silently re-points every rule
// below it: the outcome column would have taken the HTTP width and the readings
// would have shifted, with nothing failing until someone looked at the panel.
func TestStylesheetProbeColumnsAreNumberedForEight(t *testing.T) {
	css, err := ReadAsset("style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	sheet := stripCSSComments(string(css))

	for _, n := range []int{1, 3, 5, 6, 7, 8} {
		needle := fmt.Sprintf("#probes .table td:nth-child(%d)", n)
		if !strings.Contains(sheet, needle) {
			t.Errorf("no width rule for probe column %d", n)
		}
	}
	// The table has eight columns; a rule for a ninth means the panel and the
	// stylesheet have drifted apart.
	if strings.Contains(sheet, "#probes .table td:nth-child(9)") {
		t.Error("there is a width rule for a ninth probe column, but the table has eight")
	}
}
