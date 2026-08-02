package oauthas

import (
	"errors"
	"strings"
	"testing"
)

// RFC 7636 Appendix B worked example.
const (
	rfc7636Verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	rfc7636Challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

func TestVerifyPKCE_S256Succeeds(t *testing.T) {
	t.Parallel()
	if got := DeriveS256Challenge(rfc7636Verifier); got != rfc7636Challenge {
		t.Fatalf("DeriveS256Challenge = %q, want %q", got, rfc7636Challenge)
	}
	if err := VerifyPKCE(CodeChallengeMethodS256, rfc7636Challenge, rfc7636Verifier); err != nil {
		t.Fatalf("VerifyPKCE = %v, want success", err)
	}
}

func TestVerifyPKCE_WrongVerifierRefused(t *testing.T) {
	t.Parallel()
	// A valid-shape but wrong verifier (43 chars, unreserved).
	wrong := strings.Repeat("a", 43)
	err := VerifyPKCE(CodeChallengeMethodS256, rfc7636Challenge, wrong)
	if err == nil {
		t.Fatalf("VerifyPKCE with wrong verifier = nil, want error")
	}
	assertCode(t, err, ErrCodeInvalidGrant)
	assertSentinel(t, err, ErrPKCEMismatch)
}

func TestValidateCodeVerifier_TooShort(t *testing.T) {
	t.Parallel()
	assertCode(t, mustErr(t, ValidateCodeVerifier(strings.Repeat("a", 42))), ErrCodeInvalidGrant)
}

func TestValidateCodeVerifier_TooLong(t *testing.T) {
	t.Parallel()
	assertCode(t, mustErr(t, ValidateCodeVerifier(strings.Repeat("a", 129))), ErrCodeInvalidGrant)
}

func TestValidateCodeVerifier_BoundsAccepted(t *testing.T) {
	t.Parallel()
	if err := ValidateCodeVerifier(strings.Repeat("a", 43)); err != nil {
		t.Fatalf("43-char verifier rejected: %v", err)
	}
	if err := ValidateCodeVerifier(strings.Repeat("a", 128)); err != nil {
		t.Fatalf("128-char verifier rejected: %v", err)
	}
}

func TestValidateCodeVerifier_CharsetRefused(t *testing.T) {
	t.Parallel()
	base := strings.Repeat("a", 42)
	for _, bad := range []string{"+", "/", "=", " ", "é"} {
		v := base + bad
		if err := ValidateCodeVerifier(v); err == nil {
			t.Errorf("ValidateCodeVerifier(...%q) = nil, want charset error", bad)
		} else {
			assertCode(t, err, ErrCodeInvalidGrant)
		}
	}
}

func TestParseCodeChallengeMethod_PlainRefused(t *testing.T) {
	t.Parallel()
	_, err := ParseCodeChallengeMethod("plain")
	if err == nil {
		t.Fatalf("plain accepted, want refusal")
	}
	assertCode(t, err, ErrCodeInvalidRequest)
	if !strings.Contains(err.Error(), "plain") {
		t.Fatalf("error should name plain: %v", err)
	}
}

func TestParseCodeChallengeMethod_EmptyRefused(t *testing.T) {
	t.Parallel()
	// RFC 7636 §4.3 defaults an absent method to plain; we refuse rather than default.
	_, err := ParseCodeChallengeMethod("")
	if err == nil {
		t.Fatalf("empty method accepted, want refusal (no plain downgrade)")
	}
	assertCode(t, err, ErrCodeInvalidRequest)
}

func TestParseCodeChallengeMethod_UnknownRefused(t *testing.T) {
	t.Parallel()
	_, err := ParseCodeChallengeMethod("S512")
	if err == nil {
		t.Fatalf("unknown method accepted, want refusal")
	}
	assertCode(t, err, ErrCodeInvalidRequest)
}

func TestParseCodeChallengeMethod_S256Accepted(t *testing.T) {
	t.Parallel()
	m, err := ParseCodeChallengeMethod("S256")
	if err != nil || m != CodeChallengeMethodS256 {
		t.Fatalf("ParseCodeChallengeMethod(S256) = %q, %v", m, err)
	}
}

func TestVerifyPKCE_MissingChallengeRefused(t *testing.T) {
	t.Parallel()
	// An empty challenge is refused, so a downgrade to no-PKCE is impossible.
	err := VerifyPKCE(CodeChallengeMethodS256, "", rfc7636Verifier)
	if err == nil {
		t.Fatalf("missing challenge accepted, want refusal")
	}
	assertCode(t, err, ErrCodeInvalidRequest)
}

func TestVerifyPKCE_NonS256MethodRefused(t *testing.T) {
	t.Parallel()
	err := VerifyPKCE(CodeChallengeMethod("plain"), rfc7636Challenge, rfc7636Verifier)
	if err == nil {
		t.Fatalf("non-S256 method accepted, want refusal")
	}
	assertCode(t, err, ErrCodeInvalidGrant)
}

func TestValidateCodeChallenge_WrongLengthRefused(t *testing.T) {
	t.Parallel()
	// 43 chars is required; 42 is too short.
	if err := ValidateCodeChallenge(strings.Repeat("A", 42)); err == nil {
		t.Fatalf("42-char challenge accepted")
	} else {
		assertCode(t, err, ErrCodeInvalidRequest)
	}
	// A 43-char value that does not decode to 32 bytes is refused. "=" is not
	// base64url; use a value with an invalid final char alignment instead: a
	// 43-char all-'A' decodes to 32 zero bytes and is VALID, so assert that too.
	if err := ValidateCodeChallenge(strings.Repeat("A", 43)); err != nil {
		t.Fatalf("valid 43-char base64url challenge rejected: %v", err)
	}
	// Non-base64url charset.
	if err := ValidateCodeChallenge(strings.Repeat("A", 42) + "*"); err == nil {
		t.Fatalf("non-base64url challenge accepted")
	}
}

func mustErr(t *testing.T, err error) error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	return err
}

func assertSentinel(t *testing.T, err, sentinel error) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("error %v does not wrap sentinel %v", err, sentinel)
	}
}
