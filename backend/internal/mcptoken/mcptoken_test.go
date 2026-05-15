package mcptoken

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGeneratePlaintext_StartsWithPrefix(t *testing.T) {
	pt, err := generatePlaintext()
	if err != nil {
		t.Fatalf("generatePlaintext: %v", err)
	}
	if !strings.HasPrefix(pt, TokenPrefix) {
		t.Errorf("plaintext = %q, want prefix %q", pt, TokenPrefix)
	}
	// Total length should be roughly prefix + base64(32) = 4 + 43.
	if len(pt) < 40 || len(pt) > 60 {
		t.Errorf("plaintext len = %d, want ~47", len(pt))
	}
}

func TestGeneratePlaintext_DistinctValues(t *testing.T) {
	a, _ := generatePlaintext()
	b, _ := generatePlaintext()
	if a == b {
		t.Errorf("two consecutive Generate calls returned identical token: %q", a)
	}
}

func TestHashPlaintext_HappyPath(t *testing.T) {
	pt, _ := generatePlaintext()
	h, err := HashPlaintext(pt)
	if err != nil {
		t.Fatalf("HashPlaintext: %v", err)
	}
	if len(h) != 64 {
		t.Errorf("hash len = %d, want 64 hex chars (sha256)", len(h))
	}
	h2, _ := HashPlaintext(pt)
	if h != h2 {
		t.Errorf("hash not deterministic: %q vs %q", h, h2)
	}
}

func TestHashPlaintext_DistinctTokensProduceDistinctHashes(t *testing.T) {
	a, _ := generatePlaintext()
	b, _ := generatePlaintext()
	ha, _ := HashPlaintext(a)
	hb, _ := HashPlaintext(b)
	if ha == hb {
		t.Errorf("distinct plaintexts produced the same hash: %q == %q", ha, hb)
	}
}

func TestHashPlaintext_RejectsMissingPrefix(t *testing.T) {
	_, err := HashPlaintext("not-a-fishhawk-mcp-token")
	if !errors.Is(err, ErrMalformedToken) {
		t.Errorf("err = %v, want ErrMalformedToken", err)
	}
}

func TestHashPlaintext_RejectsTooShortBody(t *testing.T) {
	// The prefix is there but the body is implausibly small.
	_, err := HashPlaintext(TokenPrefix + "abc")
	if !errors.Is(err, ErrMalformedToken) {
		t.Errorf("err = %v, want ErrMalformedToken on short token", err)
	}
}

func TestHashPlaintext_RejectsApiTokenPrefix(t *testing.T) {
	// Critical for the bearer-auth middleware's route decision:
	// an apitoken's "fhk_" string must never validate as an MCP
	// token. Catches a typo where a developer accidentally hands
	// the wrong token to the wrong authenticator.
	_, err := HashPlaintext("fhk_someoperatortokencontentvaluethatislongenough")
	if !errors.Is(err, ErrMalformedToken) {
		t.Errorf("err = %v, want ErrMalformedToken for fhk_-prefixed token", err)
	}
}

func TestHasPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{TokenPrefix + "abc123", true},
		{"fhk_apiTokenStyle", false},
		{"", false},
		{"random", false},
	}
	for _, c := range cases {
		if got := HasPrefix(c.in); got != c.want {
			t.Errorf("HasPrefix(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestToken_IsRevoked(t *testing.T) {
	var live Token
	if live.IsRevoked() {
		t.Error("live token reports IsRevoked=true")
	}
	now := time.Now()
	revoked := Token{RevokedAt: &now}
	if !revoked.IsRevoked() {
		t.Error("revoked token reports IsRevoked=false")
	}
}

func TestToken_IsExpired(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"zero value never expired", time.Time{}, false},
		{"future expiry", now.Add(time.Hour), false},
		{"past expiry", now.Add(-time.Hour), true},
		{"exact now", now, false}, // not strictly past
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tk := Token{ExpiresAt: c.expiresAt}
			if got := tk.IsExpired(now); got != c.want {
				t.Errorf("IsExpired = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIssueParams_TTLBoundary(t *testing.T) {
	// Sanity that DefaultTTL is a reasonable positive value.
	if DefaultTTL <= 0 {
		t.Errorf("DefaultTTL = %v, want positive", DefaultTTL)
	}
	if DefaultTTL > 24*time.Hour {
		t.Errorf("DefaultTTL = %v, suspiciously long for a per-stage token", DefaultTTL)
	}
}

// Compile-time guard: changing IssueParams must keep the field
// names accessible to callers (the handler reads RunID + TTL
// directly).
func TestIssueParams_FieldsAccessible(t *testing.T) {
	p := IssueParams{
		RunID: uuid.New(),
		TTL:   5 * time.Minute,
	}
	if p.RunID == uuid.Nil {
		t.Error("RunID zero")
	}
	if p.TTL == 0 {
		t.Error("TTL zero")
	}
}
