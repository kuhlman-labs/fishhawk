package alerttrigger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// This file is the request-authentication half of the alert ingress: an
// HMAC-SHA256 over `<timestamp>.<raw body>` with a per-source shared secret,
// plus a timestamp replay window. It follows the Stripe and Slack webhook
// signing conventions and the backend/internal/webhook VerifySignature
// precedent (crypto/hmac.Equal for the comparison). The in-window nonce that
// refuses an EXACT resend is the caller's: it keys the webhook delivery store
// on the decoded MAC Verify returns, and only after Verify succeeded.

// Request headers a sender sets on POST /v0/triggers/alert.
const (
	// HeaderSource names the configured source (Source.ID) whose secret
	// signed the request.
	HeaderSource = "X-Fishhawk-Alert-Source"
	// HeaderTimestamp is the signing time as Unix seconds (1-12 ASCII
	// digits). It is part of the signed string, so it cannot be moved.
	HeaderTimestamp = "X-Fishhawk-Alert-Timestamp"
	// HeaderSignature is `sha256=<hex>` of HMAC-SHA256(secret,
	// timestamp + "." + body). The hex is case-insensitive.
	HeaderSignature = "X-Fishhawk-Alert-Signature"
)

// SignaturePrefix is the algorithm tag every HeaderSignature value carries.
const SignaturePrefix = "sha256="

// DefaultReplayWindow is the timestamp tolerance Verify applies when it is
// handed a non-positive window: a request signed more than this far from the
// receiver's clock, in either direction, is refused ErrStale.
const DefaultReplayWindow = 5 * time.Minute

// maxTimestampDigits bounds HeaderTimestamp: 12 digits of Unix seconds is
// past the year 33000, and the bound keeps the parse far from overflow.
const maxTimestampDigits = 12

// Verification errors. Each maps to one 401 code at the HTTP layer
// (README.md, "Response codes"). They are distinct so an operator debugging
// a sender can tell an unsigned request from a mis-signed or a stale one.
var (
	// ErrSignatureMissing: HeaderSignature is absent, lacks the sha256=
	// prefix, carries no digest, or is not hex.
	ErrSignatureMissing = errors.New("alerttrigger: alert signature missing or malformed (want " + HeaderSignature + ": sha256=<hex>)")
	// ErrTimestampInvalid: HeaderTimestamp is not 1-12 ASCII digits.
	ErrTimestampInvalid = errors.New("alerttrigger: alert timestamp missing or malformed (want " + HeaderTimestamp + ": 1-12 digits of Unix seconds)")
	// ErrSignatureInvalid: the MAC does not match, OR the source is not
	// configured. The two are deliberately indistinguishable so a caller
	// cannot enumerate configured source ids.
	ErrSignatureInvalid = errors.New("alerttrigger: alert signature does not match")
	// ErrStale: the MAC is valid but the timestamp is outside the replay
	// window. Checked AFTER the MAC, so an unsigned or mis-signed stale
	// request reports the signature failure instead.
	ErrStale = errors.New("alerttrigger: alert timestamp outside the replay window")
)

// unknownSourceKey is the HMAC key Verify uses when the named source is not
// configured. Computing (and comparing) a MAC on that path too means an
// unknown source costs the same work as a known source with a wrong
// signature and answers the same error, so response timing and content do
// not reveal which source ids exist. Its value is irrelevant: a result
// computed under it is never accepted.
var unknownSourceKey = []byte("fishhawk-alert-trigger-unknown-source-dummy-key!")

// Sign returns the HeaderSignature value for body signed at timestamp (the
// exact HeaderTimestamp string) under secret: `sha256=` + lowercase hex of
// HMAC-SHA256(secret, timestamp + "." + body).
func Sign(secret []byte, timestamp string, body []byte) string {
	return SignaturePrefix + hex.EncodeToString(computeMAC(secret, timestamp, body))
}

func computeMAC(secret []byte, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return mac.Sum(nil)
}

// Verify authenticates one alert request and returns the configured source
// and the decoded MAC (the nonce material: two encodings of one signature,
// e.g. upper- vs lower-case hex, decode to the same bytes).
//
// Order is load-bearing: the signature header is parsed first
// (ErrSignatureMissing), then the timestamp (ErrTimestampInvalid), then the
// MAC is computed and compared in constant time (ErrSignatureInvalid, also
// for an unknown source), and only a VALID MAC reaches the window check
// (ErrStale). now is the receiver's clock; a non-positive window means
// DefaultReplayWindow.
func Verify(sources Sources, sourceID, tsHeader, sigHeader string, body []byte, now time.Time, window time.Duration) (Source, []byte, error) {
	got, err := decodeSignature(sigHeader)
	if err != nil {
		return Source{}, nil, err
	}
	ts, err := parseTimestamp(tsHeader)
	if err != nil {
		return Source{}, nil, err
	}

	src, known := sources.Lookup(sourceID)
	key := unknownSourceKey
	if known {
		key = src.secret
	}
	want := computeMAC(key, tsHeader, body)
	// Compare BEFORE consulting known, so both paths do the same work.
	match := hmac.Equal(got, want)
	if !match || !known {
		return Source{}, nil, ErrSignatureInvalid
	}

	if window <= 0 {
		window = DefaultReplayWindow
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > window {
		return Source{}, nil, ErrStale
	}
	return src, want, nil
}

// decodeSignature parses a `sha256=<hex>` header into its digest bytes.
func decodeSignature(h string) ([]byte, error) {
	digest, ok := strings.CutPrefix(h, SignaturePrefix)
	if !ok || digest == "" {
		return nil, ErrSignatureMissing
	}
	got, err := hex.DecodeString(digest)
	if err != nil {
		return nil, ErrSignatureMissing
	}
	return got, nil
}

// parseTimestamp accepts 1-12 ASCII digits only: no sign, no whitespace, no
// fraction, so the signed string has exactly one spelling per instant.
func parseTimestamp(h string) (int64, error) {
	if h == "" || len(h) > maxTimestampDigits {
		return 0, ErrTimestampInvalid
	}
	// At most 12 digits, so the accumulation cannot overflow int64.
	var ts int64
	for i := 0; i < len(h); i++ {
		if h[i] < '0' || h[i] > '9' {
			return 0, ErrTimestampInvalid
		}
		ts = ts*10 + int64(h[i]-'0')
	}
	return ts, nil
}
