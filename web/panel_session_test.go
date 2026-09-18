package web

import (
	"os/exec"
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

// TestPanelKeepsTheInheritedKeyOutOfTheLoginForm pins the deliberate choice not
// to pre-fill the management key input.
//
// The input only appears when authentication failed, where the useful value is
// a different key -- pre-filling a rejected one is misleading. The key is shown
// in the session banner instead, masked until the operator asks for it.
func TestPanelKeepsTheInheritedKeyOutOfTheLoginForm(t *testing.T) {
	html, err := ReadAsset("index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	js, err := ReadAsset("app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}

	if !strings.Contains(string(html), `id="session"`) {
		t.Error("index.html has no session banner to report an inherited key")
	}
	if !strings.Contains(string(html), `id="session-switch"`) {
		t.Error("the banner offers no way to switch to another key")
	}

	// enterPanel must take the inherited key so the banner can report it.
	if !strings.Contains(string(js), "function enterPanel(inheritedKey)") {
		t.Error("enterPanel does not receive the inherited key")
	}
	// The form field is only ever cleared, never seeded with a key.
	if !strings.Contains(string(js), `$("key").value = ""`) {
		t.Error("switching keys should clear the field rather than pre-fill it")
	}
	// The top-level `key` JSON field and the form field share a name in some
	// refactors; make sure the form is not being written from the session.
	if strings.Contains(string(js), `$("key").value = inherited`) {
		t.Error("the inherited key is being pre-filled into the login form")
	}
}
