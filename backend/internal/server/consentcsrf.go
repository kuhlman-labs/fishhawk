package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

// Consent-form CSRF token (E66.23 / #2442).
//
// The consent page rendered by GET /v0/oauth/authorize embeds a STATELESS,
// SESSION-BOUND signed token in its hidden csrf_token field instead of a mirror
// of the host-wide __Host-csrf cookie. The cookie double-submit collided across
// concurrent authorization flows: the Strict cookie is never sent on the
// cross-site arrival, so every render minted and OVERWROTE the single cookie
// slot, and tab B's mint invalidated tab A's outstanding form. With a signed
// token every rendered page carries its own independently-verifiable value, no
// cookie is read or written on this route, and SameSite is out of the picture.
//
// Format: base64url( ts[8] || nonce[16] || HMAC-SHA256(key, label || ts || nonce) )
// with ts the big-endian unix seconds of the mint, and
// key = SHA-256(label || 0x00 || session-cookie-plaintext). The key is derived
// per SESSION rather than from a server-wide secret because Config carries no
// signing secret and a process-random key would break across replicas and
// across a fishhawkd restart mid-consent. A forger needs the HttpOnly session
// cookie plaintext to compute a valid token — the same secret the double-submit
// ultimately protected — and cannot read the consent HTML cross-origin. HMAC
// output does not reveal its key (RFC 2104 §6), so embedding the MAC in the page
// does not expose the session-derived key.

// consentCSRFTokenTTL bounds how long a rendered consent page stays submittable:
// long enough to read the page, short enough to bound replay of a leaked one.
const consentCSRFTokenTTL = time.Hour

// consentCSRFKeyLabel domain-separates the CSRF key derivation from every other
// use of the session plaintext, and is also bound into the MAC input.
const consentCSRFKeyLabel = "fishhawk-consent-csrf-v1"

const (
	consentCSRFTsLen    = 8
	consentCSRFNonceLen = 16
	consentCSRFMACLen   = sha256.Size
	consentCSRFTokenLen = consentCSRFTsLen + consentCSRFNonceLen + consentCSRFMACLen
)

// Refusal modes of verifyConsentCSRFToken. Callers assert identity with errors.Is.
var (
	// ErrConsentCSRFMalformed: not base64url, or not exactly ts||nonce||mac long.
	ErrConsentCSRFMalformed = errors.New("consent csrf token is malformed")
	// ErrConsentCSRFExpired: now is past mint + consentCSRFTokenTTL.
	ErrConsentCSRFExpired = errors.New("consent csrf token has expired")
	// ErrConsentCSRFInvalid: the MAC does not verify under this session's key.
	ErrConsentCSRFInvalid = errors.New("consent csrf token is not bound to this session")
)

// consentCSRFKey derives the per-session HMAC key. It is DOMAIN-SEPARATED from
// auth.HashPlaintext — the unlabelled SHA-256(plaintext) persisted in the
// sessions table — by the label and a 0x00 separator, so a sessions-table read
// never yields the CSRF key (TestConsentCSRFKey_IsNotThePersistedSessionHash).
func consentCSRFKey(sessionPlaintext string) []byte {
	h := sha256.New()
	h.Write([]byte(consentCSRFKeyLabel))
	h.Write([]byte{0})
	h.Write([]byte(sessionPlaintext))
	return h.Sum(nil)
}

// consentCSRFMAC computes HMAC-SHA256(key, label || ts || nonce).
func consentCSRFMAC(key, tsAndNonce []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(consentCSRFKeyLabel))
	m.Write(tsAndNonce)
	return m.Sum(nil)
}

// mintConsentCSRFToken mints a token bound to sessionPlaintext at now.
func mintConsentCSRFToken(sessionPlaintext string, now time.Time) (string, error) {
	if sessionPlaintext == "" {
		return "", errors.New("consent csrf: empty session plaintext")
	}
	raw := make([]byte, consentCSRFTsLen+consentCSRFNonceLen, consentCSRFTokenLen)
	binary.BigEndian.PutUint64(raw[:consentCSRFTsLen], uint64(now.Unix()))
	if _, err := rand.Read(raw[consentCSRFTsLen:]); err != nil {
		return "", err
	}
	raw = append(raw, consentCSRFMAC(consentCSRFKey(sessionPlaintext), raw)...)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// verifyConsentCSRFToken checks token against sessionPlaintext at now. The
// length check runs before the TTL check, which runs before the MAC check, so
// each refusal mode is distinguishable; the MAC comparison is constant-time.
func verifyConsentCSRFToken(sessionPlaintext, token string, now time.Time) error {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != consentCSRFTokenLen {
		return ErrConsentCSRFMalformed
	}
	ts := time.Unix(int64(binary.BigEndian.Uint64(raw[:consentCSRFTsLen])), 0)
	if now.After(ts.Add(consentCSRFTokenTTL)) {
		return ErrConsentCSRFExpired
	}
	body := raw[:consentCSRFTsLen+consentCSRFNonceLen]
	want := consentCSRFMAC(consentCSRFKey(sessionPlaintext), body)
	if subtle.ConstantTimeCompare(raw[consentCSRFTsLen+consentCSRFNonceLen:], want) != 1 {
		return ErrConsentCSRFInvalid
	}
	return nil
}

// sessionPlaintextFrom returns the raw fishhawk_session cookie value, or "" when
// absent. Both the csrf middleware and renderConsent read the cookie directly
// rather than carrying the plaintext on Identity: Identity is context-visible
// and must not carry a credential.
func sessionPlaintextFrom(r *http.Request) string {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
