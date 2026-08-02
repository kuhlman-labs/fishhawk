package oauthas

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// CodeChallengeMethod is a PKCE code_challenge_method (RFC 7636). This server
// supports S256 only.
type CodeChallengeMethod string

// CodeChallengeMethodS256 is the only supported PKCE method.
const CodeChallengeMethodS256 CodeChallengeMethod = "S256"

// ParseCodeChallengeMethod accepts "S256" only. It rejects "plain", any unknown
// value, and the EMPTY string: RFC 7636 §4.3 defaults an absent method to plain,
// which this server refuses, so an absent method must be rejected, never defaulted.
func ParseCodeChallengeMethod(raw string) (CodeChallengeMethod, error) {
	switch raw {
	case "S256":
		return CodeChallengeMethodS256, nil
	case "":
		return "", newError(ErrCodeInvalidRequest, "code_challenge_method is absent; it defaults to \"plain\" under RFC 7636 §4.3, which is unsupported — send \"S256\"")
	case "plain":
		return "", newError(ErrCodeInvalidRequest, "code_challenge_method \"plain\" is unsupported — send \"S256\"")
	default:
		return "", newError(ErrCodeInvalidRequest, "code_challenge_method %q is unsupported — send \"S256\"", raw)
	}
}

// ValidateCodeVerifier enforces RFC 7636 §4.1: a length of 43..128 inclusive with
// every byte in the unreserved set [A-Za-z0-9-._~].
func ValidateCodeVerifier(v string) error {
	if len(v) < 43 {
		return newError(ErrCodeInvalidGrant, "code_verifier is shorter than the RFC 7636 minimum of 43 characters")
	}
	if len(v) > 128 {
		return newError(ErrCodeInvalidGrant, "code_verifier is longer than the RFC 7636 maximum of 128 characters")
	}
	for i := 0; i < len(v); i++ {
		if !isUnreservedByte(v[i]) {
			return newError(ErrCodeInvalidGrant, "code_verifier contains a character outside the RFC 7636 unreserved set [A-Za-z0-9-._~]")
		}
	}
	return nil
}

// ValidateCodeChallenge requires a non-empty 43-character unpadded base64url value
// that decodes to exactly 32 bytes (a SHA-256 digest). A missing challenge is
// refused here, so a downgrade to no-PKCE is impossible through VerifyPKCE.
func ValidateCodeChallenge(c string) error {
	if c == "" {
		return newError(ErrCodeInvalidRequest, "code_challenge is missing")
	}
	if len(c) != 43 {
		return newError(ErrCodeInvalidRequest, "code_challenge must be a 43-character base64url-encoded SHA-256 digest")
	}
	dec, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return newError(ErrCodeInvalidRequest, "code_challenge is not valid unpadded base64url").withCause(err)
	}
	if len(dec) != 32 {
		return newError(ErrCodeInvalidRequest, "code_challenge does not decode to a 32-byte SHA-256 digest")
	}
	return nil
}

// DeriveS256Challenge returns the base64url (unpadded) SHA-256 of verifier, i.e.
// the RFC 7636 S256 code challenge for the given code verifier.
func DeriveS256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyPKCE validates the method (S256 only), then the challenge shape, then the
// verifier shape, then compares the derived challenge against the stored one with
// a constant-time comparison. A mismatch returns ErrPKCEMismatch wrapped in an
// invalid_grant *Error.
func VerifyPKCE(method CodeChallengeMethod, challenge, verifier string) error {
	if method != CodeChallengeMethodS256 {
		return newError(ErrCodeInvalidGrant, "unsupported code_challenge_method %q; only S256 is supported", string(method))
	}
	if err := ValidateCodeChallenge(challenge); err != nil {
		return err
	}
	if err := ValidateCodeVerifier(verifier); err != nil {
		return err
	}
	derived := DeriveS256Challenge(verifier)
	if subtle.ConstantTimeCompare([]byte(derived), []byte(challenge)) != 1 {
		return newError(ErrCodeInvalidGrant, "PKCE verification failed").withCause(ErrPKCEMismatch)
	}
	return nil
}

// isUnreservedByte reports whether c is in the RFC 7636 unreserved set.
func isUnreservedByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '-' || c == '.' || c == '_' || c == '~':
		return true
	default:
		return false
	}
}
