// Package timezone rewrites the timezone a request declares about its caller.
//
// Codex traffic carries it in the environment context the client appends to the
// conversation:
//
//	<timezone>Asia/Shanghai</timezone>
//	<current_date>2026-09-20</current_date>
//
// The rewrite is byte-level and anchored on those markers. It deliberately does
// not unmarshal and re-marshal the body: the payload is an opaque conversation
// whose key order, escaping and unknown fields are not ours to normalise, and a
// round trip through a Go struct would quietly rewrite parts of a request we
// were only asked to change one value in.
//
// Nothing here is allowed to damage a request. A body with no recognisable
// marker is returned byte-for-byte, and every rewrite is attempted on a copy so
// a partial match can never leave half a substitution behind.
package timezone

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rewrite describes what one rewrite did, for the pipeline counters.
type Rewrite struct {
	// Changed reports whether the body was modified.
	Changed bool
	// From is the timezone the request declared, when one was found. Empty when
	// no marker was present.
	From string
	// Matched reports that a recognisable marker was found even if its value was
	// already the target, which is the difference between "this client declares
	// a timezone and it is already right" and "this client declares none".
	Matched bool
	// Forms names which markers were rewritten, so an operator can see what
	// their traffic actually carries rather than guessing.
	Forms []string
}

// Marker names, as they appear in the environment context.
const (
	tagTimezone    = "timezone"
	tagCurrentDate = "current_date"
)

// Result dimensions recorded by the pipeline counters.
const (
	FormTag         = "tag"
	FormCurrentDate = "current_date"
	FormJSONField   = "json_field"
	FormOffsetMin   = "offset_min"
)

// Rewriter changes the timezone a request declares.
//
// The target zone is an argument to Rewrite rather than a field, so one
// Rewriter can serve concurrent requests without any shared mutable state: the
// caller passes the zone it read from its own settings snapshot, which also
// means a settings change mid-flight cannot half-apply to one request.
type Rewriter struct {
	// now is injectable so the date and offset calculations are testable.
	now func() time.Time
}

// New builds a rewriter.
func New() *Rewriter { return &Rewriter{now: time.Now} }

// SetClock overrides the time source. Tests only.
func (r *Rewriter) SetClock(now func() time.Time) { r.now = now }

// Rewrite replaces the timezone markers in body with those of loc.
//
// A nil loc is the "conversion is off" state, not an error: it is how an
// enabled switch with an empty or unloadable target behaves, and it must convert
// nothing.
//
// body is never mutated: on any change a new slice is returned. When nothing
// matched, the input slice is returned unchanged and identical, so a caller can
// compare elements to know whether to send a replacement body at all.
func (r *Rewriter) Rewrite(body []byte, loc *time.Location) ([]byte, Rewrite) {
	var out Rewrite
	if r == nil || loc == nil || len(body) == 0 {
		return body, out
	}

	// One scan decides whether this body is worth touching. The payload is a
	// whole conversation, and most requests in a session are continuations that
	// carry no environment context at all.
	if !bytes.Contains(body, []byte("<"+tagTimezone+">")) &&
		!bytes.Contains(body, []byte(`"timezone"`)) &&
		!bytes.Contains(body, []byte(`"timezone_offset_min"`)) {
		return body, out
	}

	next := body
	// apply records one substitution and folds what it found into the report.
	// First match wins for From: the tag is the field a client is most likely
	// to have derived the others from, so it is tried first.
	apply := func(v replaced, form string) {
		next = v.body
		if out.From == "" {
			out.From = v.from
		}
		out.Matched = true
		out.Forms = append(out.Forms, form)
	}

	if v, ok := replaceTag(next, tagTimezone, loc.String()); ok {
		apply(v, FormTag)
	}
	// The date travels with the zone. Rewriting one without the other can tell
	// the model it is in New York on a date New York has not reached yet, and
	// the date is the half it will actually use.
	if v, ok := replaceTag(next, tagCurrentDate, r.now().In(loc).Format("2006-01-02")); ok {
		apply(v, FormCurrentDate)
	}
	if v, ok := replaceJSONField(next, "timezone", loc.String(), false); ok {
		apply(v, FormJSONField)
	}
	// Not part of this API -- timezone_offset_min belongs to the ChatGPT web
	// backend, and neither the Codex CLI nor CPA ever sends it. Handled anyway,
	// because a client that impersonates the web UI may pass it through, and
	// leaving a stale offset next to a rewritten zone would be worse than
	// either alone. JavaScript's sign convention: UTC+8 is -480.
	_, offset := r.now().In(loc).Zone()
	if v, ok := replaceJSONField(next, "timezone_offset_min", strconv.Itoa(-offset/60), true); ok {
		apply(v, FormOffsetMin)
	}

	if bytes.Equal(next, body) {
		// Every marker already held the target value. Worth reporting as a
		// match, but there is no new body to send.
		return body, out
	}
	out.Changed = true
	return next, out
}

// replaced carries what a substitution found.
type replaced struct {
	body []byte
	from string
}

// replaceTag substitutes the text of <name>...</name>.
//
// Anchored on the exact opening and closing tags rather than a regexp: the
// values are short, the tags are literal, and a scan is both faster and easier
// to reason about than a pattern that could match across a conversation.
func replaceTag(body []byte, name, value string) (replaced, bool) {
	open := []byte("<" + name + ">")
	closeTag := []byte("</" + name + ">")

	start := bytes.Index(body, open)
	if start < 0 {
		return replaced{}, false
	}
	inner := start + len(open)
	end := bytes.Index(body[inner:], closeTag)
	if end < 0 {
		// An opening tag with no close is not something we understand. Leave it
		// alone rather than guessing where the value ends.
		return replaced{}, false
	}
	innerEnd := inner + end

	current := string(body[inner:innerEnd])
	// Only the first occurrence is rewritten, matching how the client emits one
	// environment context per conversation. A body carrying several is not
	// something this code has evidence for, and rewriting only the first is a
	// partial edit that shows up immediately in the counters rather than
	// silently changing a second block it did not understand.
	if current == value {
		return replaced{body: body, from: current}, true
	}

	out := make([]byte, 0, len(body)-len(current)+len(value))
	out = append(out, body[:inner]...)
	out = append(out, value...)
	out = append(out, body[innerEnd:]...)
	return replaced{body: out, from: current}, true
}

// replaceJSONField substitutes the value of "name": "value" or "name":"value".
//
// numericOK decides whether a bare number is a value this field can hold. A
// zone is never a number, so rewriting `"timezone":123` into a zone name would
// be editing a field that means something else; an offset is always a number.
//
// A string value must be recognisable as a timezone, so an unrelated key that
// happens to share the name is left alone. The surrounding bytes are preserved
// exactly, including whether the colon is followed by a space.
func replaceJSONField(body []byte, name, value string, numericOK bool) (replaced, bool) {
	key := []byte(`"` + name + `"`)
	at := bytes.Index(body, key)
	if at < 0 {
		return replaced{}, false
	}

	i := at + len(key)
	// Optional whitespace, then a colon.
	for i < len(body) && isSpace(body[i]) {
		i++
	}
	if i >= len(body) || body[i] != ':' {
		return replaced{}, false
	}
	i++
	for i < len(body) && isSpace(body[i]) {
		i++
	}
	if i >= len(body) {
		return replaced{}, false
	}

	var (
		innerEnd int
		current  string
	)
	if body[i] == '"' {
		end := bytes.IndexByte(body[i+1:], '"')
		if end < 0 {
			return replaced{}, false
		}
		innerEnd = i + 1 + end
		current = string(body[i+1 : innerEnd])
		if !looksLikeZone(current) {
			// The same key name holding something else. Not ours to touch.
			return replaced{}, false
		}
		innerEnd++ // include the closing quote in the replaced span
	} else if numericOK {
		end := i
		if end < len(body) && (body[end] == '-' || body[end] == '+') {
			end++
		}
		digits := end
		for end < len(body) && body[end] >= '0' && body[end] <= '9' {
			end++
		}
		if end == digits {
			return replaced{}, false
		}
		innerEnd = end
		current = string(body[i:end])
	} else {
		// A number where a name is expected: a same-named field that means
		// something else.
		return replaced{}, false
	}

	if current == value {
		return replaced{body: body, from: current}, true
	}

	out := make([]byte, 0, len(body)-len(current)+len(value))
	out = append(out, body[:i]...)
	if body[i] == '"' {
		out = append(out, '"')
	}
	out = append(out, value...)
	if body[i] == '"' {
		out = append(out, '"')
	}
	out = append(out, body[innerEnd:]...)
	return replaced{body: out, from: current}, true
}

// looksLikeZone reports whether a JSON string value is plausibly a timezone.
//
// Deliberately loose: the point is only to avoid rewriting a same-named field
// that holds something unrelated, so anything shaped like an IANA name, a UTC
// offset or the literal UTC passes.
func looksLikeZone(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 64 {
		return false
	}
	if strings.EqualFold(v, "UTC") || strings.EqualFold(v, "GMT") {
		return true
	}
	if v[0] == '+' || v[0] == '-' {
		// +08:00 / -0530
		rest := v[1:]
		for _, r := range rest {
			if (r < '0' || r > '9') && r != ':' {
				return false
			}
		}
		return len(rest) >= 2
	}
	// IANA: Area/City, letters, digits, underscore, plus and minus.
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '/', r == '_', r == '+', r == '-':
		default:
			return false
		}
	}
	return strings.Contains(v, "/") || strings.EqualFold(v, "Asia") || len(v) > 2
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// ParseTarget validates a configured timezone name.
//
// Empty is valid and means "conversion off" -- the requirement that an enabled
// switch with no usable target converts nothing. Anything else must load as a
// location; the error is phrased for the panel, which shows it verbatim.
func ParseTarget(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("无法识别的时区 %q：请使用 IANA 名称，例如 Asia/Shanghai、America/New_York、UTC", name)
	}
	return loc, nil
}
