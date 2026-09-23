package timezone

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// nyc is the zone used throughout: it differs from UTC by a whole day at the
// hours these tests pin, which is what makes the date coupling visible.
func nyc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return loc
}

// fixture is a rewriter bound to the New York target, so each test reads as
// "rewrite this body" without repeating the zone. The production call passes the
// zone explicitly, because one rewriter serves concurrent requests.
type fixture struct {
	r   *Rewriter
	loc *time.Location
}

func (f fixture) rewrite(body []byte) ([]byte, Rewrite) { return f.r.Rewrite(body, f.loc) }

// at returns a fixture whose clock is fixed, so the date and offset it derives
// are deterministic.
func at(t *testing.T, iso string) fixture {
	t.Helper()
	when, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatalf("parse %q: %v", iso, err)
	}
	r := New()
	r.SetClock(func() time.Time { return when })
	return fixture{r: r, loc: nyc(t)}
}

// environmentBody is the shape the Codex client emits: an environment context
// content item carrying the zone and the date, indented with two spaces.
func environmentBody(zone, date string) string {
	return `{"input":[{"type":"message","role":"user","content":[{"type":"input_text",` +
		`"text":"<environment_context>\n  <cwd>/home/me</cwd>\n  <timezone>` + zone +
		`</timezone>\n  <current_date>` + date + `</current_date>\n</environment_context>"}]}]}`
}

func TestRewrite_ReplacesTheZoneAndItsDate(t *testing.T) {
	// 01:00 UTC on the 21st is 21:00 on the 20th in New York. Rewriting the zone
	// without the date would tell the model it is in New York on a day New York
	// has not reached yet.
	f := at(t, "2026-09-21T01:00:00Z")

	out, res := f.rewrite([]byte(environmentBody("Asia/Shanghai", "2026-09-21")))

	if !res.Changed {
		t.Fatal("Changed = false")
	}
	if res.From != "Asia/Shanghai" {
		t.Errorf("From = %q, want Asia/Shanghai", res.From)
	}
	got := string(out)
	if !strings.Contains(got, "<timezone>America/New_York</timezone>") {
		t.Errorf("zone not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "<current_date>2026-09-20</current_date>") {
		t.Errorf("date not rewritten:\n%s", got)
	}
	// Everything else survives byte for byte: the payload is an opaque
	// conversation and only one value in it was ours to change.
	if !strings.Contains(got, "<cwd>/home/me</cwd>") {
		t.Errorf("surrounding content was lost:\n%s", got)
	}
	if !json.Valid(out) {
		t.Errorf("the result is no longer valid JSON:\n%s", got)
	}
}

// TestRewrite_LeavesTheRestOfTheBodyAlone is the property that matters most:
// the request is an opaque conversation and a rewrite must not normalise it.
func TestRewrite_LeavesTheRestOfTheBodyAlone(t *testing.T) {
	f := at(t, "2026-09-21T12:00:00Z") // 08:00 in New York, same date

	body := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user",` +
		`"content":[{"type":"input_text","text":"  <timezone>Europe/Berlin</timezone>"}]}],` +
		`"tools":[],"store":false,"unknown_future_field":{"a":1}}`)

	out, res := f.rewrite(body)
	if !res.Changed {
		t.Fatal("Changed = false")
	}

	want := strings.Replace(string(body), "Europe/Berlin", "America/New_York", 1)
	if string(out) != want {
		t.Errorf("only the zone should differ.\n got: %s\nwant: %s", out, want)
	}
	// Key order, escaping and unknown fields are untouched by construction --
	// nothing was unmarshalled.
	if !strings.Contains(string(out), `"unknown_future_field":{"a":1}`) {
		t.Errorf("an unknown field was lost:\n%s", out)
	}
}

func TestRewrite_NoMarkerLeavesTheBodyIdentical(t *testing.T) {
	f := at(t, "2026-09-21T12:00:00Z")

	bodies := []struct {
		name string
		body string
	}{
		{"a conversation with no environment context", `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`},
		{"a body mentioning the word timezone in prose", `{"input":[{"type":"input_text","text":"what timezone are you in?"}]}`},
		{"an unrelated json field", `{"timezone_mentioned":true,"input":[]}`},
		{"an empty object", `{}`},
		{"not json at all", `plain text`},
	}

	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.body)
			out, res := f.rewrite(in)
			if res.Changed || res.Matched {
				t.Errorf("changed=%v matched=%v, want neither", res.Changed, res.Matched)
			}
			if !bytes.Equal(out, in) {
				t.Errorf("body was altered:\n got: %s\nwant: %s", out, in)
			}
			// Same backing array: nothing was copied, so a caller can skip
			// sending a replacement body entirely.
			if len(in) > 0 && &out[0] != &in[0] {
				t.Error("body was copied despite no match")
			}
		})
	}
}

// TestRewrite_AnUnterminatedTagIsLeftAlone: an opening tag with no close is not
// a shape we understand, and guessing where the value ends could damage a
// request.
func TestRewrite_AnUnterminatedTagIsLeftAlone(t *testing.T) {
	f := at(t, "2026-09-21T12:00:00Z")
	body := []byte(`{"input":[{"type":"input_text","text":"<timezone>Asia/Shanghai"}]}`)

	out, res := f.rewrite(body)
	if res.Changed {
		t.Errorf("an unterminated tag was rewritten:\n%s", out)
	}
	if !bytes.Equal(out, body) {
		t.Errorf("body was altered:\n%s", out)
	}
}

func TestRewrite_AlreadyAtTargetReportsAMatchWithoutChangingAnything(t *testing.T) {
	f := at(t, "2026-09-21T12:00:00Z")
	body := []byte(environmentBody("America/New_York", "2026-09-21"))

	out, res := f.rewrite(body)
	if res.Changed {
		t.Error("Changed = true when the value was already correct")
	}
	if !res.Matched {
		t.Error("Matched = false; the difference from 'no timezone declared' matters")
	}
	if !bytes.Equal(out, body) {
		t.Error("body was altered")
	}
}

func TestRewrite_JSONField(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no space after the colon",
			body: `{"timezone":"Asia/Shanghai","input":[]}`,
			want: `{"timezone":"America/New_York","input":[]}`,
		},
		{
			name: "a space after the colon is preserved",
			body: `{"timezone": "Asia/Shanghai"}`,
			want: `{"timezone": "America/New_York"}`,
		},
		{
			name: "a UTC offset value",
			body: `{"timezone":"+08:00"}`,
			want: `{"timezone":"America/New_York"}`,
		},
		{
			name: "the literal UTC",
			body: `{"timezone":"UTC"}`,
			want: `{"timezone":"America/New_York"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := at(t, "2026-09-21T12:00:00Z")
			out, res := f.rewrite([]byte(tc.body))
			if !res.Changed {
				t.Fatalf("Changed = false for %s", tc.body)
			}
			if string(out) != tc.want {
				t.Errorf("got  %s\nwant %s", out, tc.want)
			}
		})
	}
}

// TestRewrite_DoesNotTouchASameNamedFieldHoldingSomethingElse: a key called
// "timezone" can hold a value that is not a timezone, and rewriting it would be
// editing a field we were not asked to touch.
func TestRewrite_DoesNotTouchASameNamedFieldHoldingSomethingElse(t *testing.T) {
	f := at(t, "2026-09-21T12:00:00Z")

	bodies := []string{
		`{"timezone":"the server's own setting"}`,
		`{"timezone":"Europe/Berlin and also a sentence"}`,
		`{"timezone":{"nested":"object"}}`,
		`{"timezone":123}`,
		`{"timezone":null}`,
		`{"timezone":["Asia/Shanghai"]}`,
		`{"timezone":"Asia/Shanghai` + `}`, // unterminated string
	}
	for _, body := range bodies {
		out, res := f.rewrite([]byte(body))
		if res.Changed {
			t.Errorf("rewrote a value that is not a timezone:\n in: %s\nout: %s", body, out)
		}
	}
}

// TestRewrite_OffsetMinutes covers the ChatGPT web field. It is not part of the
// Codex API, so this is defensive -- but a stale offset beside a rewritten zone
// would be worse than either alone.
func TestRewrite_OffsetMinutes(t *testing.T) {
	f := at(t, "2026-09-21T12:00:00Z") // New York is on EDT then: UTC-4

	out, res := f.rewrite([]byte(`{"timezone_offset_min":-480,"timezone":"Asia/Shanghai"}`))
	if !res.Changed {
		t.Fatal("Changed = false")
	}
	// JavaScript's sign convention: minutes to add to local time to reach UTC,
	// so UTC-4 is 240.
	if !strings.Contains(string(out), `"timezone_offset_min":240`) {
		t.Errorf("offset not rewritten:\n%s", out)
	}
	if !strings.Contains(string(out), `"timezone":"America/New_York"`) {
		t.Errorf("zone not rewritten:\n%s", out)
	}
}

// TestRewrite_OffConvertsNothing covers the state the requirement names: the
// switch is on but the target is empty or unloadable, so nothing is converted.
//
// A nil location is how that arrives -- ParseTarget returns nil for an empty
// name and an error plus nil for one it cannot load -- so it has to be a
// supported input rather than a programming mistake.
func TestRewrite_OffConvertsNothing(t *testing.T) {
	body := []byte(environmentBody("Asia/Shanghai", "2026-09-21"))
	f := at(t, "2026-09-21T12:00:00Z")

	cases := []struct {
		name string
		r    *Rewriter
		loc  *time.Location
	}{
		{"no target configured", f.r, nil},
		{"no rewriter at all", nil, f.loc},
		{"neither", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, res := tc.r.Rewrite(body, tc.loc)
			if res.Changed || res.Matched {
				t.Errorf("changed=%v matched=%v, want neither", res.Changed, res.Matched)
			}
			if !bytes.Equal(out, body) {
				t.Errorf("body was altered:\n%s", out)
			}
		})
	}
}

func TestRewrite_EmptyBody(t *testing.T) {
	f := at(t, "2026-09-21T12:00:00Z")
	out, res := f.rewrite(nil)
	if res.Changed || len(out) != 0 {
		t.Errorf("out = %q, res = %+v", out, res)
	}
}

// TestRewrite_DoesNotDependOnTheZoneTheProcessRunsIn: the result is a function
// of the configured target and the clock, never of the host's TZ.
func TestRewrite_DoesNotDependOnTheZoneTheProcessRunsIn(t *testing.T) {
	body := []byte(environmentBody("Asia/Shanghai", "2026-09-21"))
	want := ""
	for _, host := range []string{"UTC", "Asia/Tokyo", "America/Los_Angeles"} {
		loc, err := time.LoadLocation(host)
		if err != nil {
			t.Fatalf("LoadLocation %s: %v", host, err)
		}
		// A fixed instant, reinterpreted through different host zones. The
		// process's own TZ must not reach the result.
		instant, _ := time.Parse(time.RFC3339, "2026-09-21T01:00:00Z")
		r := New()
		r.SetClock(func() time.Time { return instant.In(loc) })

		out, _ := r.Rewrite(body, nyc(t))
		if want == "" {
			want = string(out)
			continue
		}
		if string(out) != want {
			t.Errorf("host zone %s changed the result:\n got: %s\nwant: %s", host, out, want)
		}
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"an IANA name", "Asia/Shanghai", "Asia/Shanghai", false},
		{"surrounding space", "  America/New_York  ", "America/New_York", false},
		{"UTC", "UTC", "UTC", false},
		{"empty means off", "", "", false},
		{"whitespace means off", "   ", "", false},
		{"not a timezone", "Beijing", "", true},
		{"a bare offset is not an IANA name", "+08:00", "", true},
		{"nonsense", "Mars/Olympus", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loc, err := ParseTarget(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTarget(%q) = %v, want an error", tc.input, loc)
				}
				// The message is shown verbatim in the panel, so it has to name
				// the input and suggest what a valid one looks like.
				if !strings.Contains(err.Error(), tc.input) && tc.input != "" {
					t.Errorf("error does not name the input: %v", err)
				}
				if !strings.Contains(err.Error(), "Asia/Shanghai") {
					t.Errorf("error gives no example: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tc.input, err)
			}
			got := ""
			if loc != nil {
				got = loc.String()
			}
			if got != tc.want {
				t.Errorf("ParseTarget(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestRewrite_IsCheapWhenThereIsNoMarker guards the hot path: the pre-check must
// reject a large body without copying it.
func TestRewrite_IsCheapWhenThereIsNoMarker(t *testing.T) {
	f := at(t, "2026-09-21T12:00:00Z")
	// A conversation-sized body with no marker anywhere in it.
	body := []byte(`{"input":[{"type":"input_text","text":"` +
		strings.Repeat("lorem ipsum dolor sit amet ", 20000) + `"}]}`)

	out, res := f.rewrite(body)
	if res.Changed || res.Matched {
		t.Fatal("a body with no marker was treated as a match")
	}
	if &out[0] != &body[0] {
		t.Error("the body was copied; the pre-check did not short-circuit")
	}
}

// TestRewrite_LeavesTheSameZoneNameElsewhereAlone is the safety property that
// separates this from a global string replace.
//
// A user talking about where they are, or quoting a log, puts the zone name in
// their own message. Rewriting that would edit the conversation, not the
// metadata -- and a bytes.ReplaceAll would do it silently, because the string is
// identical. Every substitution here is anchored: inside a specific tag, or as
// the value of a specific key.
func TestRewrite_LeavesTheSameZoneNameElsewhereAlone(t *testing.T) {
	f := at(t, "2026-09-23T02:00:00Z")

	body := []byte(`{"timezone":"Asia/Shanghai","timezone_offset_min":-480,` +
		`"input":[{"type":"input_text","text":"<environment_context>\n` +
		`  <timezone>Asia/Shanghai</timezone>\n` +
		`  <current_date>2026-09-23</current_date>\n</environment_context>"},` +
		`{"type":"input_text","text":"我人在 Asia/Shanghai，这段别动"}]}`)

	out, res := f.rewrite(body)
	if !res.Changed {
		t.Fatal("nothing was rewritten")
	}
	got := string(out)

	// The prose survives verbatim, including the zone name it mentions.
	if !strings.Contains(got, "我人在 Asia/Shanghai，这段别动") {
		t.Errorf("the user's own message was edited:\n%s", got)
	}
	// And it is the only occurrence of the source zone left.
	if n := strings.Count(got, "Asia/Shanghai"); n != 1 {
		t.Errorf("Asia/Shanghai appears %d times, want exactly 1 (the prose)", n)
	}
	// The structured sites all moved.
	if n := strings.Count(got, "America/New_York"); n != 2 {
		t.Errorf("the target appears %d times, want 2 (the json field and the tag)", n)
	}
	if !json.Valid(out) {
		t.Errorf("the result is no longer valid JSON:\n%s", got)
	}
}

// TestRewrite_CountsSpansNotOccurrences pins what "how many places" means: one
// span per marker, and the number depends on which marker forms the request
// carries rather than on how often a zone name appears in it.
func TestRewrite_CountsSpansNotOccurrences(t *testing.T) {
	f := at(t, "2026-09-23T02:00:00Z")

	cases := []struct {
		name  string
		body  string
		forms []string
	}{
		{
			name:  "json fields only",
			body:  `{"timezone":"Asia/Shanghai","timezone_offset_min":-480}`,
			forms: []string{FormJSONField, FormOffsetMin},
		},
		{
			name: "environment context only",
			body: `{"input":[{"type":"input_text","text":"<timezone>Asia/Shanghai</timezone>` +
				`<current_date>2026-09-23</current_date>"}]}`,
			forms: []string{FormTag, FormCurrentDate},
		},
		{
			name:  "the tag without a date",
			body:  `{"input":[{"type":"input_text","text":"<timezone>Asia/Shanghai</timezone>"}]}`,
			forms: []string{FormTag},
		},
		{
			name: "both shapes in one body",
			body: `{"timezone":"Asia/Shanghai","timezone_offset_min":-480,` +
				`"input":[{"type":"input_text","text":"<timezone>Asia/Shanghai</timezone>` +
				`<current_date>2026-09-23</current_date>"}]}`,
			forms: []string{FormTag, FormCurrentDate, FormJSONField, FormOffsetMin},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, res := f.rewrite([]byte(tc.body))
			if len(res.Forms) != len(tc.forms) {
				t.Fatalf("forms = %v, want %v", res.Forms, tc.forms)
			}
			for i := range tc.forms {
				if res.Forms[i] != tc.forms[i] {
					t.Fatalf("forms = %v, want %v", res.Forms, tc.forms)
				}
			}
		})
	}
}
