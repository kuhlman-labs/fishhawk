package workmgmt

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// This file carries the generic, provider-agnostic IDEMPOTENCY-KEY primitives
// (#2064, E50.7): mint a deterministic key, stamp it into a work-item body as
// an inert hidden HTML comment, and detect it again when the body is read back
// off the forge.
//
// WHY A BODY MARKER. GitHub's issue-create REST API offers no idempotency key,
// so a create that succeeded but whose local record never persisted is
// indistinguishable — from the caller's side — from a create that never
// happened. The only durable, forge-side handle we can attach to a created
// issue is its BODY. Stamping a deterministic key into the body turns
// "did I already file this?" into a query the forge itself can answer.
//
// The marker renders as NOTHING in GitHub-flavoured Markdown (an HTML comment
// is hidden content, per GitHub's basic writing and formatting syntax), yet is
// returned verbatim in the raw body — the same mechanism the shipped #1793
// sticky-comment orphan re-discovery already relies on in
// backend/internal/issuecomment/notifier.go, in production use in this repo.
//
// These helpers are deliberately provider-agnostic: they know nothing about
// runs, split proposals or HTTP. backend/internal/splitfiling composes them
// into the split-filing keys; FilingRequest.IdempotencyKey routes a key through
// Apply so any filing path can adopt the mechanism (refinement.ExecuteFiling's
// identical residual is a named follow-up, not wired here).

// idempotencyMarkerPrefix and idempotencyMarkerSuffix bracket the hidden
// marker line. The prefix is namespaced to Fishhawk so a marker can never
// collide with an unrelated HTML comment in a hand-authored body.
const (
	idempotencyMarkerPrefix = "<!-- fishhawk-idempotency-key "
	idempotencyMarkerSuffix = " -->"
)

// idempotencyKeyDigestLen is how many hex characters of the SHA-256 digest a
// minted key carries. 32 hex chars = 128 bits, far beyond collision range for
// the per-run key sets this mechanism mints, while keeping the marker line
// short enough to read in a raw body.
const idempotencyKeyDigestLen = 32

// MintIdempotencyKey returns a deterministic, opaque idempotency key for the
// given namespace and parts. It is a PURE function of its inputs: no clock, no
// randomness, no process state — re-deriving it from the same inputs on a later
// pass (a re-approval, a different process) yields a byte-identical key, which
// is the whole basis for query-before-file adoption.
//
// The parts are joined with a NUL separator before hashing, so
// MintIdempotencyKey("ns", "a", "bc") and MintIdempotencyKey("ns", "ab", "c")
// are DISTINCT — a part boundary cannot be forged by concatenation.
//
// The namespace is carried in the clear (sanitized to a marker-safe character
// set) so a raw body names which mechanism stamped it; the digest carries the
// parts. A key never contains a newline or the HTML-comment terminator, so it
// can never break the marker line it is embedded in.
func MintIdempotencyKey(namespace string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(namespace))
	h.Write([]byte{0})
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	digest := hex.EncodeToString(h.Sum(nil))[:idempotencyKeyDigestLen]
	return sanitizeKeyNamespace(namespace) + ":" + digest
}

// sanitizeKeyNamespace reduces a namespace to the marker-safe character set
// [A-Za-z0-9._-], replacing anything else with '_'. It exists so a stray space,
// newline or '>' in a caller-supplied namespace can never break the marker line
// (and therefore can never defeat the whole-line match in
// BodyHasIdempotencyKey). It does NOT affect the digest, which hashes the
// namespace verbatim.
func sanitizeKeyNamespace(ns string) string {
	var b strings.Builder
	b.Grow(len(ns))
	for _, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// idempotencyMarker renders the hidden marker line for key.
func idempotencyMarker(key string) string {
	return idempotencyMarkerPrefix + key + idempotencyMarkerSuffix
}

// StampIdempotencyKey appends the hidden idempotency marker for key to body and
// returns the result. It is STAMP-ONCE: a body that already carries that exact
// key is returned UNCHANGED, so re-stamping (a retry, a body assembled from a
// previously-stamped source) can never accumulate duplicate markers. An empty
// key is a no-op.
//
// The marker is appended as its own line, separated by a blank line, so it
// never fuses with trailing prose — which matters because detection is a
// WHOLE-LINE match.
func StampIdempotencyKey(body, key string) string {
	if key == "" {
		return body
	}
	if BodyHasIdempotencyKey(body, key) {
		return body
	}
	marker := idempotencyMarker(key)
	if strings.TrimSpace(body) == "" {
		return marker
	}
	return strings.TrimRight(body, "\n") + "\n\n" + marker
}

// BodyHasIdempotencyKey reports whether body carries the hidden marker for key.
//
// It matches a WHOLE marker LINE, never a prefix or a substring: a key that is
// a strict prefix of the stamped key does NOT match, and the key text appearing
// inside prose (rather than as its own marker line) does NOT match. That is the
// load-bearing property — a substring match would adopt a DIFFERENT child whose
// key merely starts the same way, which is a duplicate-avoidance mechanism
// silently causing the wrong adoption.
//
// Line endings are normalized before matching: GitHub can return \r\n in a body
// authored or edited through the web interface, and an exact-line matcher would
// then fail to recognize a marker it stamped itself.
func BodyHasIdempotencyKey(body, key string) bool {
	if key == "" {
		return false
	}
	marker := idempotencyMarker(key)
	normalized := strings.ReplaceAll(body, "\r\n", "\n")
	for _, line := range strings.Split(normalized, "\n") {
		// TrimSpace also absorbs a lone trailing \r (a bare-CR body) and any
		// indentation a forge or editor introduced around the marker.
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
}
