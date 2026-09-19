package states

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// makeToken builds a value with the envelope the parser expects, so the checks
// can be exercised without a live upstream.
func makeToken(t *testing.T, blocks int, issued time.Time) string {
	t.Helper()
	raw := make([]byte, envelopeOverhead+envelopeBlockSize*blocks)
	raw[0] = envelopePrefix
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	for i := 9; i < len(raw); i++ {
		raw[i] = byte(i)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func TestParseEnvelope_ReadsShapeAndIssueTime(t *testing.T) {
	issued := time.Date(2026, 9, 19, 3, 0, 0, 0, time.UTC)

	for _, blocks := range []int{10, 12} {
		token := makeToken(t, blocks, issued)
		if got, want := len(token), EncodedLength(blocks); got != want {
			t.Fatalf("%d blocks encodes to %d chars, want %d", blocks, got, want)
		}
		env, err := ParseEnvelope(token)
		if err != nil {
			t.Fatalf("%d blocks: %v", blocks, err)
		}
		if env.Blocks != blocks {
			t.Errorf("Blocks = %d, want %d", env.Blocks, blocks)
		}
		if !env.Issued.Equal(issued) {
			t.Errorf("Issued = %v, want %v", env.Issued, issued)
		}
		if env.Fingerprint == "" {
			t.Error("Fingerprint is empty")
		}
	}
}

// TestEncodedLength_MatchesObservedValues pins the two lengths that matter.
// They are measurements, not a specification: 10 blocks is what personal
// accounts produce and 12 what team accounts produce.
func TestEncodedLength_MatchesObservedValues(t *testing.T) {
	if got := EncodedLength(10); got != 292 {
		t.Errorf("EncodedLength(10) = %d, want the observed 292", got)
	}
	if got := EncodedLength(12); got != 332 {
		t.Errorf("EncodedLength(12) = %d, want the observed 332", got)
	}
}

func TestPlanBlocks_SplitsTeamFromPersonal(t *testing.T) {
	cases := map[string]int{
		"team": 12, "TEAM": 12, " business ": 12,
		"plus": 10, "free": 10, "pro": 10, "": 10, "k12": 10,
	}
	for plan, want := range cases {
		if got := PlanBlocks(plan); got != want {
			t.Errorf("PlanBlocks(%q) = %d, want %d", plan, got, want)
		}
	}
}

// TestAcceptState_ShapeNotLength is the point of the change: a value of the
// account's shape binds, and one of the other shape does not -- even though
// both are legitimately shaped tokens.
func TestAcceptState_ShapeNotLength(t *testing.T) {
	issued := time.Now().Add(-time.Minute)
	personal := makeToken(t, 10, issued)
	team := makeToken(t, 12, issued)

	personalWant := ExpectationFor("plus", 292)
	teamWant := ExpectationFor("team", 292)

	if _, ok := AcceptState(personal, personalWant); !ok {
		t.Error("a personal value was rejected for a personal account")
	}
	if _, ok := AcceptState(team, personalWant); ok {
		t.Error("a team value was accepted for a personal account")
	}
	if _, ok := AcceptState(team, teamWant); !ok {
		t.Error("a team value was rejected for a team account")
	}
	if _, ok := AcceptState(personal, teamWant); ok {
		t.Error("a personal value was accepted for a team account")
	}
}

// TestExpectationFor_UnknownPlanUsesTheConfiguredLength covers the case the
// upstream gives us nothing to go on.
func TestExpectationFor_UnknownPlanUsesTheConfiguredLength(t *testing.T) {
	personal := ExpectationFor("", 292)
	if personal.Blocks != 10 {
		t.Errorf("blank plan with 292 resolved to %d blocks, want 10", personal.Blocks)
	}
	team := ExpectationFor("", 332)
	if team.Blocks != 12 {
		t.Errorf("blank plan with 332 resolved to %d blocks, want 12", team.Blocks)
	}
	// An unrecognised length keeps the personal rule rather than inventing one.
	if got := ExpectationFor("", 411); got.Blocks != 10 {
		t.Errorf("an unknown length resolved to %d blocks, want the personal 10", got.Blocks)
	}
}

// TestAcceptState_UnreadableValueFallsBackToLength keeps the old behaviour for
// values whose envelope we cannot read: a length match still binds, so an
// upstream format change degrades to what we did before rather than to nothing.
func TestAcceptState_UnreadableValueFallsBackToLength(t *testing.T) {
	opaque := strings.Repeat("a", 292)

	// The configured length is only consulted when the plan is unknown: with a
	// plan in hand the expectation comes from the plan, and a length setting
	// cannot contradict it.
	if _, ok := AcceptState(opaque, ExpectationFor("", 292)); !ok {
		t.Error("an opaque 292-character value was rejected against a configured 292")
	}
	if _, ok := AcceptState(opaque, ExpectationFor("", 300)); ok {
		t.Error("an opaque 292-character value was accepted against a configured 300")
	}
	if got := ExpectationFor("plus", 300); got.Length != 292 {
		t.Errorf("a known plan resolved to length %d, want the plan's 292", got.Length)
	}
	if _, ok := AcceptState("", ExpectationFor("", 292)); ok {
		t.Error("an empty value was accepted")
	}
}

// TestParseEnvelope_RejectsWhatItCannotRead covers the guards. Each of these
// must be treated as unreadable rather than as a hard error, which is what lets
// the caller fall back to length.
func TestParseEnvelope_RejectsWhatItCannotRead(t *testing.T) {
	issued := time.Now()
	good := makeToken(t, 10, issued)

	bad := map[string]string{
		"empty":            "",
		"not base64":       "!!!not-base64!!!",
		"wrong version":    strings.Repeat("A", 292),
		"too much padding": good + "===",
		"with whitespace":  good[:10] + " " + good[10:],
	}
	for name, value := range bad {
		if _, err := ParseEnvelope(value); err == nil {
			t.Errorf("%s: parsed as an envelope, want a rejection", name)
		}
	}

	// A plausible-length value whose timestamp is nonsense must not be read as
	// a valid envelope: the length lining up is not evidence on its own.
	raw := make([]byte, envelopeOverhead+envelopeBlockSize*10)
	raw[0] = envelopePrefix
	binary.BigEndian.PutUint64(raw[1:9], 1)
	if _, err := ParseEnvelope(base64.URLEncoding.EncodeToString(raw)); err == nil {
		t.Error("an out-of-range timestamp parsed as an envelope")
	}
}
