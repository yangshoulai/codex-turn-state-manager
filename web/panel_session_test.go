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

const blob = encode(JSON.stringify({ state: { managementKey: 'local-key' } }));
const fromBlob = build({ host: 'example.test:8317' }, { userAgent: 'Mozilla/5.0 (Test)' },
  { getItem: () => blob }, atob, btoa).readInheritedKey();
if (fromBlob !== 'local-key') { console.error('obfuscated session decoded to ' + JSON.stringify(fromBlob)); process.exit(1); }

// Plain JSON must still work, so a change of format degrades to the login
// prompt instead of breaking outright.
const fromPlain = build({ host: 'example.test:8317' }, { userAgent: 'Mozilla/5.0 (Test)' },
  { getItem: () => JSON.stringify({ managementKey: 'plain-key' }) }, atob, btoa).readInheritedKey();
if (fromPlain !== 'plain-key') { console.error('plain session decoded to ' + JSON.stringify(fromPlain)); process.exit(1); }

// A foreign or corrupt blob yields nothing rather than throwing.
const fromJunk = build({ host: 'example.test:8317' }, { userAgent: 'Mozilla/5.0 (Test)' },
  { getItem: () => 'enc::v1::not-valid-base64!!' }, atob, btoa).readInheritedKey();
if (fromJunk !== '') { console.error('corrupt session decoded to ' + JSON.stringify(fromJunk)); process.exit(1); }

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
