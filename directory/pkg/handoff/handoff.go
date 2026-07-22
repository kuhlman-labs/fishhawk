// Package handoff is the SINGLE owning implementation of the signed
// region-handoff parameter set the directory appends to a redirect and the
// cell verifies on arrival (ADR-062 A2.6). Both planes import this package;
// neither re-implements the codec, so signer and verifier can never drift.
//
// A handoff carries (provider, account_key, home_region, expires_at, nonce)
// authenticated by an HMAC-SHA256 tag over a canonical, length-prefixed
// serialization of those fields in a fixed order. Length prefixing — not a
// naive separator-joined concatenation — is what makes the encoding
// unambiguous: without it a separator character inside account_key could be
// made to collide with a different field split under the same MAC.
//
// The nonce and expiry BIND each handoff to one issuance; they are not
// consumed against a store. Replay safety on the cell comes from the
// conditional, first-write-wins UPDATE that stamps the account's home
// region, so replaying an unexpired handoff verbatim is a no-op rather than
// a privilege.
//
// An empty secret is a configuration error at both Sign and Verify. There is
// no unsigned mode.
package handoff

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Query parameter names. They are prefixed so they cannot collide with a
// caller's own parameters, which the router preserves untouched.
const (
	ParamProvider   = "fh_provider"
	ParamAccountKey = "fh_account_key"
	ParamRegion     = "fh_region"
	ParamExpiresAt  = "fh_expires_at"
	ParamNonce      = "fh_nonce"
	ParamSignature  = "fh_sig"
)

// Typed errors. Callers match with errors.Is; the cell's middleware maps
// them onto status codes.
var (
	// ErrNoSecret reports an empty signing secret at Sign or Verify — a
	// configuration fault, never a silent unsigned pass.
	ErrNoSecret = errors.New("handoff: signing secret is empty")
	// ErrMissingParam reports an absent required parameter.
	ErrMissingParam = errors.New("handoff: missing parameter")
	// ErrMalformed reports a present but unusable parameter (unparsable
	// timestamp, non-hex signature, empty required field).
	ErrMalformed = errors.New("handoff: malformed parameter")
	// ErrBadSignature reports a signature that does not authenticate the
	// accompanying fields.
	ErrBadSignature = errors.New("handoff: signature mismatch")
	// ErrExpired reports a handoff whose expires_at is at or before now.
	ErrExpired = errors.New("handoff: expired")
)

// Params is the authenticated payload of a handoff.
type Params struct {
	Provider   string
	AccountKey string
	HomeRegion string
	ExpiresAt  time.Time
	Nonce      string
}

// NonceBytes is the entropy of a generated nonce.
const NonceBytes = 16

// NewNonce returns a fresh random nonce, hex encoded. Callers mint one per
// issued handoff so two redirects are never byte-identical.
func NewNonce() (string, error) {
	b := make([]byte, NonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("handoff: generate nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Sign returns the fh_* parameter set for p, including the signature.
// The returned values are meant to be merged into the redirect's existing
// query string; Sign never touches the caller's own parameters.
//
// An empty secret, or an empty required field, is an error.
func Sign(secret string, p Params) (url.Values, error) {
	if secret == "" {
		return nil, ErrNoSecret
	}
	expires := p.ExpiresAt.UTC().Format(time.RFC3339)
	fields := []string{p.Provider, p.AccountKey, p.HomeRegion, expires, p.Nonce}
	names := []string{ParamProvider, ParamAccountKey, ParamRegion, ParamExpiresAt, ParamNonce}
	for i, f := range fields {
		if f == "" {
			return nil, fmt.Errorf("%w: %s is empty", ErrMalformed, names[i])
		}
	}
	if p.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("%w: %s is the zero time", ErrMalformed, ParamExpiresAt)
	}

	v := url.Values{}
	for i, name := range names {
		v.Set(name, fields[i])
	}
	v.Set(ParamSignature, sign(secret, fields))
	return v, nil
}

// Verify authenticates the fh_* parameters in q against secret and checks
// the handoff has not expired as of now. It returns the authenticated
// Params, or a typed error.
//
// Checks run in a fixed order — presence, then well-formedness, then
// signature, then expiry — so an attacker cannot learn whether an unsigned
// guess happened to be unexpired.
func Verify(secret string, q url.Values, now time.Time) (Params, error) {
	if secret == "" {
		return Params{}, ErrNoSecret
	}

	names := []string{ParamProvider, ParamAccountKey, ParamRegion, ParamExpiresAt, ParamNonce, ParamSignature}
	got := make([]string, len(names))
	for i, name := range names {
		if !q.Has(name) {
			return Params{}, fmt.Errorf("%w: %s", ErrMissingParam, name)
		}
		got[i] = q.Get(name)
		if got[i] == "" {
			return Params{}, fmt.Errorf("%w: %s is empty", ErrMalformed, name)
		}
	}

	expires, err := time.Parse(time.RFC3339, got[3])
	if err != nil {
		return Params{}, fmt.Errorf("%w: %s is not RFC3339: %v", ErrMalformed, ParamExpiresAt, err)
	}
	if _, err := hex.DecodeString(got[5]); err != nil {
		return Params{}, fmt.Errorf("%w: %s is not hex: %v", ErrMalformed, ParamSignature, err)
	}

	// hmac.Equal is constant time; a plain == on the tag would leak the
	// matching prefix length to a timing attacker.
	want := sign(secret, got[:5])
	if !hmac.Equal([]byte(want), []byte(got[5])) {
		return Params{}, ErrBadSignature
	}

	if !now.Before(expires) {
		return Params{}, fmt.Errorf("%w: expired at %s", ErrExpired, expires.UTC().Format(time.RFC3339))
	}

	return Params{
		Provider:   got[0],
		AccountKey: got[1],
		HomeRegion: got[2],
		ExpiresAt:  expires,
		Nonce:      got[4],
	}, nil
}

// Has reports whether q carries a handoff signature at all. The cell uses
// it to distinguish "no handoff, pass through" (a single-cell deployment)
// from "handoff present but invalid, refuse".
func Has(q url.Values) bool {
	return q.Get(ParamSignature) != ""
}

// sign returns the hex HMAC-SHA256 tag over the canonical serialization of
// fields, which must be in the fixed order Provider, AccountKey,
// HomeRegion, ExpiresAt(RFC3339), Nonce.
func sign(secret string, fields []string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical(fields)))
	return hex.EncodeToString(mac.Sum(nil))
}

// canonical renders fields as `<len>:<value>` segments. The length prefix
// makes the encoding injective: no two distinct field vectors serialize to
// the same string, whatever characters the values contain.
func canonical(fields []string) string {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteString(":")
		b.WriteString(f)
	}
	return b.String()
}
