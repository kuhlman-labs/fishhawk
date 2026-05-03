package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// generateTestKey returns a fresh 2048-bit RSA keypair and its
// PKCS#1 PEM encoding. 2048 is the minimum GitHub accepts.
func generateTestKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, pemBytes
}

// generatePKCS8Key returns the same key wrapped in PKCS#8 PEM.
func generatePKCS8Key(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestNewSignerFromPEM_PKCS1(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	s, err := NewSignerFromPEM(12345, pemBytes)
	if err != nil {
		t.Fatalf("NewSignerFromPEM: %v", err)
	}
	if s.AppID() != 12345 {
		t.Errorf("AppID = %d", s.AppID())
	}
}

func TestNewSignerFromPEM_PKCS8(t *testing.T) {
	key, _ := generateTestKey(t)
	pemBytes := generatePKCS8Key(t, key)
	if _, err := NewSignerFromPEM(12345, pemBytes); err != nil {
		t.Errorf("PKCS#8: %v", err)
	}
}

func TestNewSignerFromPEM_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		appID   int64
		pem     []byte
		wantSub string
	}{
		{"zero appID", 0, []byte("anything"), "appID must be positive"},
		{"empty pem", 12345, []byte(""), "PEM decode"},
		{"unknown pem type", 12345, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}), "unexpected PEM type"},
		{"corrupt pkcs1", 12345, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("not der")}), "parse PKCS#1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSignerFromPEM(tc.appID, tc.pem)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestSign_VerifiableByPublicKey(t *testing.T) {
	// End-to-end check: sign a JWT with our Signer, decompose it,
	// verify the signature with the corresponding public key, and
	// parse the claims.
	key, pemBytes := generateTestKey(t)
	s, err := NewSignerFromPEM(99999, pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return t0 }

	jwt, err := s.Sign(0)
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts, want 3", len(parts))
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}

	// Header shape pinned.
	var header struct {
		Alg, Typ string
	}
	_ = json.Unmarshal(headerBytes, &header)
	if header.Alg != "RS256" || header.Typ != "JWT" {
		t.Errorf("header = %+v, want RS256/JWT", header)
	}

	// Claim shape pinned.
	var claims struct {
		Iat, Exp, Iss int64
	}
	_ = json.Unmarshal(claimsBytes, &claims)
	if claims.Iss != 99999 {
		t.Errorf("iss = %d, want 99999", claims.Iss)
	}
	// iat is backdated 60s, exp uses the default TTL (9m).
	if claims.Iat != t0.Add(-60*time.Second).Unix() {
		t.Errorf("iat = %d, want %d", claims.Iat, t0.Add(-60*time.Second).Unix())
	}
	if claims.Exp != t0.Add(DefaultJWTTTL).Unix() {
		t.Errorf("exp = %d, want %d", claims.Exp, t0.Add(DefaultJWTTTL).Unix())
	}

	// Signature verifies under the corresponding public key.
	signingInput := []byte(parts[0] + "." + parts[1])
	digest := sha256.Sum256(signingInput)
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Errorf("signature did not verify: %v", err)
	}
}

func TestSign_TTLClamping(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	s, err := NewSignerFromPEM(1, pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return t0 }

	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero uses default", 0, DefaultJWTTTL},
		{"negative uses default", -1 * time.Minute, DefaultJWTTTL},
		{"valid passes through", 5 * time.Minute, 5 * time.Minute},
		{"over max clamps", 1 * time.Hour, MaxJWTTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jwt, err := s.Sign(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(jwt, ".")
			claimsBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct{ Exp int64 }
			_ = json.Unmarshal(claimsBytes, &claims)
			wantExp := t0.Add(tc.want).Unix()
			if claims.Exp != wantExp {
				t.Errorf("exp = %d, want %d (ttl=%v)", claims.Exp, wantExp, tc.want)
			}
		})
	}
}

func TestFormatInt64(t *testing.T) {
	if formatInt64(12345) != "12345" {
		t.Errorf("formatInt64(12345) = %q", formatInt64(12345))
	}
	if formatInt64(0) != "0" {
		t.Errorf("formatInt64(0) = %q", formatInt64(0))
	}
}
