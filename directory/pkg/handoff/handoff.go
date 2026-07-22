// Package handoff is the ONE codec for the region handoff the global
// directory appends to a redirect and a regional cell validates on arrival
// (ADR-062, E44.7 / #1831).
//
// The directory decides an account's home region, records it, and 302s the
// caller into that region's cell. Because the redirect crosses a trust
// boundary — the browser can rewrite anything in the URL — the region pin is
// carried as an HMAC-SHA256-signed parameter set rather than a bare query
// value. This package owns BOTH sides of that wire format: the directory
// imports it to sign, the backend (cell) imports it to verify. Keeping encode
// and verify in one public package is deliberate — two hand-mirrored
// implementations would drift at exactly the serialization boundary an
// attacker probes.
//
// It is stdlib-only on purpose so the backend can depend on it without
// dragging the directory module's transitive dependencies into the cell.
package handoff

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Query parameter names the handoff occupies. They are namespaced with an
// `fh_` prefix so appending them to a forge's OAuth/App-install callback URL
// cannot collide with that forge's own parameters (`code`, `state`,
// `installation_id`, `setup_action`, …), which MUST survive the redirect
// untouched.
const (
	ParamProvider   = "fh_provider"
	ParamAccountKey = "fh_account_key"
	ParamHomeRegion = "fh_home_region"
	ParamExpiresAt  = "fh_expires_at"
	ParamNonce      = "fh_nonce"
	ParamSignature  = "fh_sig"
)

// Errors the cell distinguishes when rejecting a pin. Every one of them is a
// fail-closed rejection: there is no degraded "accept anyway" path.
var (
	// ErrMissing reports that no handoff is present at all (an unsigned
	// request that reached the cell without transiting the directory).
	ErrMissing = errors.New("handoff: no region pin present")
	// ErrMalformed reports a structurally invalid pin — a blank required
	// field or an unparseable expiry.
	ErrMalformed = errors.New("handoff: malformed region pin")
	// ErrBadSignature reports a signature that does not match the shared
	// secret over the canonical string: a forged or tampered pin.
	ErrBadSignature = errors.New("handoff: region pin signature is invalid")
	// ErrExpired reports a pin whose expires_at is at or before now.
	ErrExpired = errors.New("handoff: region pin has expired")
	// ErrNoSecret reports that the verifying side holds no shared secret, so
	// no pin can be authenticated. Fails closed rather than trusting the
	// parameters unauthenticated.
	ErrNoSecret = errors.New("handoff: no shared secret configured")
)

// Params is the signed parameter set: which account, in which region, valid
// until when, with a per-issue nonce so two pins for the same account are
// distinguishable in an audit trail.
type Params struct {
	Provider   string
	AccountKey string
	HomeRegion string
	ExpiresAt  time.Time
	Nonce      string
}

// validate rejects a structurally incomplete parameter set. Called on BOTH
// sides: the directory never signs a blank field, and the cell never accepts
// one even if it were somehow signed.
func (p Params) validate() error {
	switch {
	case strings.TrimSpace(p.Provider) == "":
		return fmt.Errorf("%w: provider is empty", ErrMalformed)
	case strings.TrimSpace(p.AccountKey) == "":
		return fmt.Errorf("%w: account_key is empty", ErrMalformed)
	case strings.TrimSpace(p.HomeRegion) == "":
		return fmt.Errorf("%w: home_region is empty", ErrMalformed)
	case strings.TrimSpace(p.Nonce) == "":
		return fmt.Errorf("%w: nonce is empty", ErrMalformed)
	case p.ExpiresAt.IsZero():
		return fmt.Errorf("%w: expires_at is zero", ErrMalformed)
	}
	return nil
}

// canonicalString renders the parameter set as the exact bytes both sides
// MAC. The field order is fixed (not map iteration), each value is
// percent-escaped, and fields are joined with "\n" — so no value can smuggle
// a delimiter and shift a byte from one field into another (the classic
// length-extension-by-concatenation forgery: ("ab","c") and ("a","bc")
// MUST NOT share a preimage).
func canonicalString(p Params) string {
	fields := [...]string{
		ParamProvider + "=" + url.QueryEscape(p.Provider),
		ParamAccountKey + "=" + url.QueryEscape(p.AccountKey),
		ParamHomeRegion + "=" + url.QueryEscape(p.HomeRegion),
		ParamExpiresAt + "=" + strconv.FormatInt(p.ExpiresAt.Unix(), 10),
		ParamNonce + "=" + url.QueryEscape(p.Nonce),
	}
	return strings.Join(fields[:], "\n")
}

func sign(secret []byte, p Params) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(canonicalString(p)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Values renders the signed handoff as query parameters ready to be merged
// into a redirect target. It returns an error rather than an unsigned set
// when the secret is empty or the params are incomplete, so a misconfigured
// directory cannot emit a pin the cell would have to reject.
func Values(secret []byte, p Params) (url.Values, error) {
	if len(secret) == 0 {
		return nil, ErrNoSecret
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	v := url.Values{}
	v.Set(ParamProvider, p.Provider)
	v.Set(ParamAccountKey, p.AccountKey)
	v.Set(ParamHomeRegion, p.HomeRegion)
	v.Set(ParamExpiresAt, strconv.FormatInt(p.ExpiresAt.Unix(), 10))
	v.Set(ParamNonce, p.Nonce)
	v.Set(ParamSignature, sign(secret, p))
	return v, nil
}

// AppendTo returns cellBaseURL joined with the ORIGINAL request path and
// query, plus the signed handoff parameters — the routing contract the
// directory's 302 Location must satisfy. Every inbound parameter (`code`,
// `state`, `installation_id`, …) survives; the handoff is additive.
//
// A parameter the caller supplied under an `fh_` name is REPLACED by the
// directory's signed value, so a client cannot pre-seed a pin the directory
// would then leave in place alongside its own.
func AppendTo(cellBaseURL string, origPath string, origQuery url.Values, secret []byte, p Params) (string, error) {
	base, err := url.Parse(cellBaseURL)
	if err != nil {
		return "", fmt.Errorf("handoff: parse cell base url: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("handoff: cell base url %q is not absolute", cellBaseURL)
	}
	hv, err := Values(secret, p)
	if err != nil {
		return "", err
	}
	merged := url.Values{}
	for k, vs := range origQuery {
		merged[k] = append([]string(nil), vs...)
	}
	for k, vs := range hv {
		merged[k] = append([]string(nil), vs...)
	}
	// Join, not replace: a cell mounted under a path prefix keeps it.
	base.Path = strings.TrimSuffix(base.Path, "/") + "/" + strings.TrimPrefix(origPath, "/")
	base.RawQuery = merged.Encode()
	return base.String(), nil
}

// Verify authenticates a handoff carried in q and returns its parameters.
// It fails closed on every abnormal input: absent (ErrMissing), incomplete
// (ErrMalformed), wrong-secret or tampered (ErrBadSignature), and past its
// TTL (ErrExpired). now is injected so callers (and tests) control the clock.
//
// Verify establishes only that the DIRECTORY issued this pin. The two
// remaining invariants — the cell's own residency self-check and the
// first-write-wins replay bound — are the cell's, enforced above this layer
// (backend/internal/account/region.go).
func Verify(secret []byte, q url.Values, now time.Time) (Params, error) {
	if len(secret) == 0 {
		return Params{}, ErrNoSecret
	}
	sigHex := q.Get(ParamSignature)
	if sigHex == "" && q.Get(ParamHomeRegion) == "" && q.Get(ParamAccountKey) == "" {
		return Params{}, ErrMissing
	}
	if sigHex == "" {
		// Parameters present but unsigned: an attacker stripping the MAC
		// must not be indistinguishable from a legitimate caller.
		return Params{}, fmt.Errorf("%w: signature parameter absent", ErrBadSignature)
	}
	expUnix, err := strconv.ParseInt(q.Get(ParamExpiresAt), 10, 64)
	if err != nil {
		return Params{}, fmt.Errorf("%w: expires_at %q is not a unix timestamp", ErrMalformed, q.Get(ParamExpiresAt))
	}
	p := Params{
		Provider:   q.Get(ParamProvider),
		AccountKey: q.Get(ParamAccountKey),
		HomeRegion: q.Get(ParamHomeRegion),
		ExpiresAt:  time.Unix(expUnix, 0).UTC(),
		Nonce:      q.Get(ParamNonce),
	}
	if err := p.validate(); err != nil {
		return Params{}, err
	}
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return Params{}, fmt.Errorf("%w: signature is not hex", ErrBadSignature)
	}
	want, err := hex.DecodeString(sign(secret, p))
	if err != nil { // unreachable: sign always returns hex
		return Params{}, fmt.Errorf("%w: internal signature encoding", ErrBadSignature)
	}
	// Constant-time compare, and signature BEFORE expiry so an unauthenticated
	// caller learns nothing from the error about which fields were accepted.
	if !hmac.Equal(got, want) {
		return Params{}, ErrBadSignature
	}
	if !now.Before(p.ExpiresAt) {
		return Params{}, fmt.Errorf("%w: expired at %s", ErrExpired, p.ExpiresAt.Format(time.RFC3339))
	}
	return p, nil
}
