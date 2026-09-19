package states

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// The turn-state token has a recognisable envelope: a version byte, the time it
// was issued, and a run of ciphertext blocks. Reading it is what makes "is this
// the value we wanted" a question about shape rather than about string length,
// and it is the only way to learn how old a value already is when it arrives.
//
// This is a heuristic over undocumented data, never a signature check and never
// a statement about model quality. Anything that does not parse falls back to
// the configured target length.
const (
	// envelopePrefix is the version byte every observed token starts with.
	envelopePrefix = 0x80
	// envelopeOverhead is the envelope's fixed size: one version byte, an
	// eight-byte big-endian timestamp, and the trailing authentication tag and
	// nonce that surround the ciphertext blocks.
	envelopeOverhead = 57
	// envelopeBlockSize is the ciphertext block size.
	envelopeBlockSize = 16

	// maxEnvelopeBytes bounds a token before decoding, so a hostile header
	// cannot make the decoder allocate.
	maxEnvelopeBytes = 2048
)

// Envelope is what a parsed turn-state value says about itself.
type Envelope struct {
	// Issued is when the upstream minted the value. Zero when unknown.
	Issued time.Time
	// Blocks is the number of ciphertext blocks.
	Blocks int
	// Fingerprint identifies the value without retaining it, so two sightings
	// can be compared without keeping the token itself in memory.
	Fingerprint string
}

// ErrNotEnvelope means the value does not have the shape we know how to read.
// Callers fall back to comparing lengths; it is not an error condition.
var ErrNotEnvelope = errors.New("states: unrecognised state envelope")

// ParseEnvelope reads a token's envelope.
//
// Deliberately tolerant about padding and strict about everything else: a value
// that fails any check here is treated as unreadable rather than as malformed,
// because the format is not ours to define.
func ParseEnvelope(value string) (Envelope, error) {
	var env Envelope
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxEnvelopeBytes {
		return env, ErrNotEnvelope
	}
	// Base64 in a header can carry whitespace or a newline from a folding proxy.
	if strings.ContainsAny(value, "\r\n\t ") {
		return env, ErrNotEnvelope
	}

	core := strings.TrimRight(value, "=")
	if len(value)-len(core) > 2 {
		return env, ErrNotEnvelope
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(core)
	if err != nil {
		return env, ErrNotEnvelope
	}
	if len(raw) < envelopeOverhead+envelopeBlockSize || raw[0] != envelopePrefix {
		return env, ErrNotEnvelope
	}
	if (len(raw)-envelopeOverhead)%envelopeBlockSize != 0 {
		return env, ErrNotEnvelope
	}

	issued := binary.BigEndian.Uint64(raw[1:9])
	// A timestamp outside a plausible range means the bytes are not what we
	// think they are, even though the length lined up.
	if issued < 1577836800 || issued >= 4102444800 { // 2020-01-01 .. 2100-01-01
		return env, ErrNotEnvelope
	}

	sum := sha256.Sum256([]byte(value))
	env.Issued = time.Unix(int64(issued), 0)
	env.Blocks = (len(raw) - envelopeOverhead) / envelopeBlockSize
	env.Fingerprint = hex.EncodeToString(sum[:8])
	return env, nil
}

// EncodedLength is the base64 length of a token with this many blocks.
//
// Standard encoding with padding, which is what the upstream emits: ten blocks
// encode to 292 characters, twelve to 332. Both are observed values, not
// published ones.
func EncodedLength(blocks int) int {
	if blocks <= 0 {
		return 0
	}
	return base64.URLEncoding.EncodedLen(envelopeOverhead + envelopeBlockSize*blocks)
}

// PlanBlocks returns the block count an account on this plan is expected to
// produce.
//
// Ten is the personal rule and twelve the team rule. Unknown plans get the
// personal rule: it is the common case, and the alternative -- refusing to bind
// anything until a plan is known -- would leave a working account idle over a
// field the upstream may never send.
func PlanBlocks(plan string) int {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "team", "business":
		return 12
	default:
		return 10
	}
}

// Expectation is what one account's state is expected to look like.
type Expectation struct {
	// Blocks is the ciphertext block count the account's plan produces.
	Blocks int
	// Length is what those blocks encode to. Used for values whose envelope we
	// cannot read, so the check degrades to a length comparison rather than to
	// accepting anything.
	Length int
}

// ExpectationFor resolves the expectation for one account.
//
// A blank plan means the upstream has not told us the account's tier. The
// operator's configured length is then the best evidence available, and the
// block count is derived from it -- so an installation tuned for Team accounts
// does not reject their values merely because the plan field is missing.
func ExpectationFor(plan string, fallbackLength int) Expectation {
	if strings.TrimSpace(plan) == "" {
		return Expectation{Blocks: blocksForLength(fallbackLength), Length: fallbackLength}
	}
	blocks := PlanBlocks(plan)
	return Expectation{Blocks: blocks, Length: EncodedLength(blocks)}
}

// blocksForLength is the inverse of EncodedLength for the block counts we know
// about. An unrecognised length keeps the personal rule, which is what the
// default configuration describes.
func blocksForLength(length int) int {
	for blocks := 1; blocks <= 32; blocks++ {
		if EncodedLength(blocks) == length {
			return blocks
		}
	}
	return PlanBlocks("")
}

// AcceptState reports whether value is a usable state for this account, and
// returns what was read from it.
//
// Shape, not length: two states of the same length can still be different
// shapes, and one account's correct shape is another's rejected value. The
// length comparison remains only as the fallback for values that do not parse,
// because refusing those outright would break on any upstream change to a
// format we were never given.
func AcceptState(value string, want Expectation) (Envelope, bool) {
	if strings.TrimSpace(value) == "" {
		return Envelope{}, false
	}
	env, err := ParseEnvelope(value)
	if err != nil {
		return Envelope{}, len(value) == want.Length
	}
	return env, env.Blocks == want.Blocks
}
