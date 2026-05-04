package githuboidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// signedToken builds a JWT signed by priv with the given header
// and claims. The header's `alg` defaults to RS256 when empty;
// `kid` defaults to "test-kid".
func signedToken(t *testing.T, priv *rsa.PrivateKey, header map[string]any, claims map[string]any) string {
	t.Helper()
	if header == nil {
		header = map[string]any{}
	}
	if _, ok := header["alg"]; !ok {
		header["alg"] = "RS256"
	}
	if _, ok := header["kid"]; !ok {
		header["kid"] = "test-kid"
	}
	if _, ok := header["typ"]; !ok {
		header["typ"] = "JWT"
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + enc.EncodeToString(sig)
}

// jwksServer returns an httptest.Server serving a static JWKS for
// the given key under "test-kid". cacheControl, when non-empty, is
// set as the Cache-Control response header so tests can drive TTL
// extraction.
func jwksServer(t *testing.T, pub *rsa.PublicKey, cacheControl string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		enc := base64.RawURLEncoding
		body := map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": "test-kid",
				"n":   enc.EncodeToString(pub.N.Bytes()),
				"e":   enc.EncodeToString(big2bytes(pub.E)),
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		if cacheControl != "" {
			w.Header().Set("Cache-Control", cacheControl)
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, hits
}

// big2bytes encodes an integer in the byte form RSA's e expects.
func big2bytes(e int) []byte {
	if e <= 0 {
		return nil
	}
	var out []byte
	for e > 0 {
		out = append([]byte{byte(e & 0xff)}, out...)
		e >>= 8
	}
	return out
}

func newTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func validClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss":        DefaultIssuer,
		"aud":        "https://fishhawk.example.com",
		"sub":        "repo:kuhlman-labs/example:ref:refs/heads/main",
		"iat":        now.Unix(),
		"nbf":        now.Unix(),
		"exp":        now.Add(10 * time.Minute).Unix(),
		"repository": "kuhlman-labs/example",
		"workflow":   "feature_change",
		"event_name": "issues",
		"ref":        "refs/heads/main",
		"sha":        "abc123",
		"run_id":     "1234567890",
	}
}

func defaultExpectations(now time.Time) Expectations {
	return Expectations{
		Audience:      "https://fishhawk.example.com",
		Repository:    "kuhlman-labs/example",
		Workflow:      "feature_change",
		AllowedEvents: []string{"issues", "issue_comment", "workflow_dispatch"},
		Now:           func() time.Time { return now },
	}
}

func TestVerify_HappyPath(t *testing.T) {
	priv := newTestKey(t)
	srv, hits := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	tok := signedToken(t, priv, nil, validClaims(now))

	got, err := v.Verify(context.Background(), tok, defaultExpectations(now))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Repository != "kuhlman-labs/example" || got.Workflow != "feature_change" {
		t.Errorf("claims = %+v", got)
	}
	if hits.Load() != 1 {
		t.Errorf("JWKS fetched %d times, want 1", hits.Load())
	}

	// Second verification reuses the cached JWKS.
	if _, err := v.Verify(context.Background(), tok, defaultExpectations(now)); err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("JWKS fetched %d times after cache hit, want still 1", hits.Load())
	}
}

func TestVerify_BadSignature(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Now().UTC()
	tok := signedToken(t, priv, nil, validClaims(now))

	// Tamper: flip the last byte of the signature.
	tampered := []byte(tok)
	tampered[len(tampered)-1] ^= 0xff

	_, err := v.Verify(context.Background(), string(tampered), defaultExpectations(now))
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestVerify_WrongAlg(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Now().UTC()
	tok := signedToken(t, priv, map[string]any{"alg": "HS256"}, validClaims(now))

	_, err := v.Verify(context.Background(), tok, defaultExpectations(now))
	if !errors.Is(err, ErrInvalidToken) || !strings.Contains(err.Error(), "RS256") {
		t.Errorf("err = %v, want ErrInvalidToken about RS256", err)
	}
}

func TestVerify_MalformedToken(t *testing.T) {
	v := NewWithJWKSURL("http://nowhere")
	_, err := v.Verify(context.Background(), "not.a.token!", Expectations{Audience: "x"})
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
	_, err = v.Verify(context.Background(), "two-parts", Expectations{Audience: "x"})
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestVerify_UnknownKID(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Now().UTC()
	tok := signedToken(t, priv, map[string]any{"kid": "different-kid"}, validClaims(now))

	_, err := v.Verify(context.Background(), tok, defaultExpectations(now))
	if !errors.Is(err, ErrUnknownKID) {
		t.Errorf("err = %v, want ErrUnknownKID", err)
	}
}

func TestVerify_Expired(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	tokenIssued := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	claims := validClaims(tokenIssued)
	tok := signedToken(t, priv, nil, claims)

	exp := defaultExpectations(tokenIssued.Add(2 * time.Hour))
	_, err := v.Verify(context.Background(), tok, exp)
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("err = %v, want ErrTokenExpired", err)
	}
}

func TestVerify_NotYetValid(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	tokenIssued := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	claims := validClaims(tokenIssued)
	claims["nbf"] = tokenIssued.Add(time.Hour).Unix()
	tok := signedToken(t, priv, nil, claims)

	_, err := v.Verify(context.Background(), tok, defaultExpectations(tokenIssued))
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("err = %v, want ErrTokenExpired", err)
	}
}

func TestVerify_WrongIssuer(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Now().UTC()
	claims := validClaims(now)
	claims["iss"] = "https://attacker.example.com"
	tok := signedToken(t, priv, nil, claims)

	_, err := v.Verify(context.Background(), tok, defaultExpectations(now))
	if !errors.Is(err, ErrClaimMismatch) {
		t.Errorf("err = %v, want ErrClaimMismatch", err)
	}
}

func TestVerify_WrongAudience(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Now().UTC()
	tok := signedToken(t, priv, nil, validClaims(now))

	exp := defaultExpectations(now)
	exp.Audience = "https://other.example.com"
	_, err := v.Verify(context.Background(), tok, exp)
	if !errors.Is(err, ErrClaimMismatch) {
		t.Errorf("err = %v, want ErrClaimMismatch", err)
	}
}

func TestVerify_AudienceRequired(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Now().UTC()
	tok := signedToken(t, priv, nil, validClaims(now))

	exp := Expectations{Now: func() time.Time { return now }}
	_, err := v.Verify(context.Background(), tok, exp)
	if !errors.Is(err, ErrClaimMismatch) || !strings.Contains(err.Error(), "Audience") {
		t.Errorf("err = %v, want ErrClaimMismatch about Audience", err)
	}
}

func TestVerify_RepositoryMismatch(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Now().UTC()
	claims := validClaims(now)
	claims["repository"] = "evil/repo"
	tok := signedToken(t, priv, nil, claims)

	_, err := v.Verify(context.Background(), tok, defaultExpectations(now))
	if !errors.Is(err, ErrClaimMismatch) {
		t.Errorf("err = %v, want ErrClaimMismatch", err)
	}
}

func TestVerify_WorkflowMismatch(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Now().UTC()
	claims := validClaims(now)
	claims["workflow"] = "different_workflow"
	tok := signedToken(t, priv, nil, claims)

	_, err := v.Verify(context.Background(), tok, defaultExpectations(now))
	if !errors.Is(err, ErrClaimMismatch) {
		t.Errorf("err = %v, want ErrClaimMismatch", err)
	}
}

func TestVerify_DisallowedEvent(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Now().UTC()
	claims := validClaims(now)
	claims["event_name"] = "schedule"
	tok := signedToken(t, priv, nil, claims)

	_, err := v.Verify(context.Background(), tok, defaultExpectations(now))
	if !errors.Is(err, ErrClaimMismatch) {
		t.Errorf("err = %v, want ErrClaimMismatch", err)
	}
}

func TestVerify_RepositoryEmptyExpSkipsCheck(t *testing.T) {
	// An empty Repository expectation should NOT be treated as
	// "must be empty"; it means "don't check." Same for Workflow
	// and AllowedEvents.
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	v := NewWithJWKSURL(srv.URL + "/.well-known/jwks")

	now := time.Now().UTC()
	tok := signedToken(t, priv, nil, validClaims(now))

	exp := Expectations{
		Audience: "https://fishhawk.example.com",
		Now:      func() time.Time { return now },
	}
	if _, err := v.Verify(context.Background(), tok, exp); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestVerify_JWKSUnreachable(t *testing.T) {
	v := NewWithJWKSURL("http://127.0.0.1:1/never")
	priv := newTestKey(t)
	tok := signedToken(t, priv, nil, validClaims(time.Now().UTC()))
	_, err := v.Verify(context.Background(), tok, defaultExpectations(time.Now().UTC()))
	if err == nil {
		t.Fatal("expected error from unreachable JWKS")
	}
}

func TestJWKSCache_RespectsCacheControlMaxAge(t *testing.T) {
	priv := newTestKey(t)
	srv, hits := jwksServer(t, &priv.PublicKey, "max-age=1")
	c := newJWKSCache(srv.URL + "/.well-known/jwks")

	if _, err := c.Get(context.Background(), "test-kid"); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
	// Within TTL, second Get should hit cache.
	if _, err := c.Get(context.Background(), "test-kid"); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("after second Get hits = %d, want still 1", hits.Load())
	}

	// Sleep past the 1s TTL and re-fetch.
	time.Sleep(1100 * time.Millisecond)
	if _, err := c.Get(context.Background(), "test-kid"); err != nil {
		t.Fatalf("third Get: %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("after expiry hits = %d, want 2", hits.Load())
	}
}

func TestJWKSCache_DefaultTTLWhenNoCacheControl(t *testing.T) {
	priv := newTestKey(t)
	srv, _ := jwksServer(t, &priv.PublicKey, "")
	c := newJWKSCache(srv.URL + "/.well-known/jwks")

	if _, err := c.Get(context.Background(), "test-kid"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	c.mu.Lock()
	expiresAt := c.expiresAt
	c.mu.Unlock()
	if time.Until(expiresAt) < 30*time.Minute {
		t.Errorf("default TTL too short: expiresAt = %v", expiresAt)
	}
}

func TestJWKSCache_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := newJWKSCache(srv.URL)
	_, err := c.Get(context.Background(), "any")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want 500", err)
	}
}

func TestJWKSCache_EmptyKID(t *testing.T) {
	c := newJWKSCache("http://nowhere")
	_, err := c.Get(context.Background(), "")
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestJWKSCache_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "not json")
	}))
	t.Cleanup(srv.Close)
	c := newJWKSCache(srv.URL)
	_, err := c.Get(context.Background(), "any")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestJWKSCache_NoUsableKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// All keys non-RSA → none decode → fetch fails.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{"kty": "EC", "kid": "k"}},
		})
	}))
	t.Cleanup(srv.Close)
	c := newJWKSCache(srv.URL)
	_, err := c.Get(context.Background(), "k")
	if err == nil || !strings.Contains(err.Error(), "no usable") {
		t.Errorf("err = %v, want no usable RSA keys", err)
	}
}

func TestJWKSCache_KeyRotationRefetches(t *testing.T) {
	priv := newTestKey(t)
	srv, hits := jwksServer(t, &priv.PublicKey, "max-age=10")
	c := newJWKSCache(srv.URL + "/.well-known/jwks")

	// Fetch once with "test-kid".
	if _, err := c.Get(context.Background(), "test-kid"); err != nil {
		t.Fatal(err)
	}
	// Now ask for an unknown kid: cache is fresh but missing →
	// refetch → still missing → ErrUnknownKID.
	_, err := c.Get(context.Background(), "rotated-kid")
	if !errors.Is(err, ErrUnknownKID) {
		t.Errorf("err = %v, want ErrUnknownKID", err)
	}
	if hits.Load() < 2 {
		t.Errorf("hits = %d, want >= 2 (refetch on miss)", hits.Load())
	}
}

func TestJWKSTTL_Parsing(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"max-age=600", 600 * time.Second},
		{"max-age=1, no-cache", time.Second},
		{"public, max-age=3600", 3600 * time.Second},
		{"", defaultJWKSCacheTTL},
		{"no-cache", defaultJWKSCacheTTL},
		{"max-age=invalid", defaultJWKSCacheTTL},
		{"max-age=0", defaultJWKSCacheTTL}, // 0 fallback to default
		{"max-age=-1", defaultJWKSCacheTTL},
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			got := jwksTTL(c.header)
			if got != c.want {
				t.Errorf("jwksTTL(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}
