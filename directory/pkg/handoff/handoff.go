// Package handoff is the ONE codec for the region handoff the global
// directory signs and a regional cell verifies (ADR-062, E44.7 / #1831).
//
// The directory decides an account's home region, records it, and then
// 302s the browser into that region's cell. The cell must be able to
// trust the region carried across that redirect without calling back to
// the directory, so the directory signs a small parameter set
//
//	provider, account_key, home_region, expires_at, nonce
//
// with HMAC-SHA256 over a canonically-ordered string using a secret
// shared by both planes via environment config. The cell verifies the
// signature and the (short) expiry and fails closed on anything else.
//
// This package is deliberately public (pkg/, not internal/) and
// stdlib-only: the backend imports it for cell-side validation, so
// there is exactly one serialization of the handoff and no chance of an
// encoder/decoder drifting apart across the module boundary.
//
// The signature is NOT a replay defence on its own — a signed pin can be
// replayed until it expires. The replay bound lives in the cell: it
// accepts a pin only when the account's home_region is currently NULL or
// already equals the incoming value (first-write-wins), so a replayed pin
// can never move an account's region.
package handoff

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Query parameter names carried across the redirect. The fh_ prefix keeps
// them clear of the OAuth/App-install parameters (code, state,
// installation_id, setup_action) the redirect preserves verbatim.
const (
	ParamProvider   = "fh_provider"
	ParamAccountKey = "fh_account_key"
	ParamHomeRegion = "fh_home_region"
	ParamExpiresAt  = "fh_expires_at"
	ParamNonce      = "fh_nonce"
	ParamSignature  = "fh_sig"
)

// Rejection reasons. Every one of these is a fail-closed branch on the
// cell side: a pin that produces any of them must not be honoured.
var (
	// ErrMissing means one or more required handoff parameters were absent
	// or empty — including an entirely unsigned request.
	ErrMissing = errors.New("handoff: missing parameters")
	// ErrMalformed means a parameter was present but unparseable.
	ErrMalformed = errors.New("handoff: malformed parameters")
	// ErrBadSignature means the HMAC did not match — a forged secret or a
	// tampered parameter value.
	ErrBadSignature = errors.New("handoff: signature mismatch")
	// ErrExpired means the pin is past its expires_at.
	ErrExpired = errors.New("handoff: expired")
	// ErrNoSecret means the caller supplied an empty shared secret.
	ErrNoSecret = errors.New("handoff: empty shared secret")
)

// Params is the signed handoff payload.
type Params struct {
	Provider   string
	AccountKey string
	HomeRegion string
	ExpiresAt  time.Time
	Nonce      string
}

// Validate reports whether every field carries a value. Empty fields are
// rejected before signing so an under-populated pin can never be minted.
func (p Params) Validate() error {
	switch {
	case p.Provider == "":
		return fmt.Errorf("%w: provider", ErrMissing)
	case p.AccountKey == "":
		return fmt.Errorf("%w: account_key", ErrMissing)
	case p.HomeRegion == "":
		return fmt.Errorf("%w: home_region", ErrMissing)
	case p.Nonce == "":
		return fmt.Errorf("%w: nonce", ErrMissing)
	case p.ExpiresAt.IsZero():
		return fmt.Errorf("%w: expires_at", ErrMissing)
	}
	return nil
}

// Sign returns the handoff query parameters (payload + signature) for p.
// The caller merges them into the redirect's query string.
func Sign(p Params, secret string) (url.Values, error) {
	if secret == "" {
		return nil, ErrNoSecret
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	v := payloadValues(p)
	v.Set(ParamSignature, sign(v, secret))
	return v, nil
}

// Verify checks the handoff parameters carried in q against the shared
// secret and the supplied wall clock, returning the payload on success.
//
// It fails closed, in this order: absent parameters (an unsigned request)
// → ErrMissing; a signature that does not match (forged secret or tampered
// value) → ErrBadSignature; an unparseable expiry → ErrMalformed; an
// expiry in the past → ErrExpired.
func Verify(q url.Values, secret string, now time.Time) (Params, error) {
	if secret == "" {
		return Params{}, ErrNoSecret
	}
	got := q.Get(ParamSignature)
	if got == "" {
		return Params{}, fmt.Errorf("%w: %s", ErrMissing, ParamSignature)
	}
	raw := url.Values{}
	for _, k := range []string{ParamProvider, ParamAccountKey, ParamHomeRegion, ParamExpiresAt, ParamNonce} {
		val := q.Get(k)
		if val == "" {
			return Params{}, fmt.Errorf("%w: %s", ErrMissing, k)
		}
		raw.Set(k, val)
	}

	// Signature first: never parse or act on a payload we have not
	// authenticated.
	if !hmac.Equal([]byte(sign(raw, secret)), []byte(got)) {
		return Params{}, ErrBadSignature
	}

	secs, err := strconv.ParseInt(raw.Get(ParamExpiresAt), 10, 64)
	if err != nil {
		return Params{}, fmt.Errorf("%w: %s: %v", ErrMalformed, ParamExpiresAt, err)
	}
	expires := time.Unix(secs, 0).UTC()
	if now.After(expires) {
		return Params{}, fmt.Errorf("%w: expired at %s", ErrExpired, expires.Format(time.RFC3339))
	}

	return Params{
		Provider:   raw.Get(ParamProvider),
		AccountKey: raw.Get(ParamAccountKey),
		HomeRegion: raw.Get(ParamHomeRegion),
		ExpiresAt:  expires,
		Nonce:      raw.Get(ParamNonce),
	}, nil
}

// payloadValues renders p as the unsigned parameter set.
func payloadValues(p Params) url.Values {
	v := url.Values{}
	v.Set(ParamProvider, p.Provider)
	v.Set(ParamAccountKey, p.AccountKey)
	v.Set(ParamHomeRegion, p.HomeRegion)
	v.Set(ParamExpiresAt, strconv.FormatInt(p.ExpiresAt.Unix(), 10))
	v.Set(ParamNonce, p.Nonce)
	return v
}

// sign computes the hex HMAC-SHA256 over the canonical encoding of the
// payload. url.Values.Encode sorts by key and percent-escapes both keys
// and values, so the canonical string is unambiguous: no value can inject
// a separator that forges a different parameter split.
func sign(payload url.Values, secret string) string {
	canonical := url.Values{}
	for k, vals := range payload {
		if k == ParamSignature {
			continue
		}
		if len(vals) > 0 {
			canonical.Set(k, vals[0])
		}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical.Encode()))
	return hex.EncodeToString(mac.Sum(nil))
}
