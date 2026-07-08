package apitoken

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
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
	// Determinism: same input → same hash.
	h2, _ := HashPlaintext(pt)
	if h != h2 {
		t.Errorf("HashPlaintext non-deterministic")
	}
	// Different input → different hash.
	pt2, _ := generatePlaintext()
	h3, _ := HashPlaintext(pt2)
	if h == h3 {
		t.Errorf("two distinct plaintexts produced same hash")
	}
}

func TestHashPlaintext_Malformed(t *testing.T) {
	cases := []string{
		"",
		"not-a-fishhawk-token",
		"github_pat_xxx", // wrong product prefix
		"fhk_",           // prefix but no entropy
		"fhk_short",      // too short
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			_, err := HashPlaintext(c)
			if !errors.Is(err, ErrMalformedToken) {
				t.Errorf("err = %v, want ErrMalformedToken", err)
			}
		})
	}
}

func TestToken_IsRevoked(t *testing.T) {
	tok := Token{}
	if tok.IsRevoked() {
		t.Errorf("zero-value Token.IsRevoked() = true, want false")
	}
}

// TestIssue_DefaultsAuthMethodStatic pins the #1708 persistence contract:
// the static Issue path (fishhawkd token issue) must record
// auth_method='static' via the column DEFAULT and leave provider NULL, and
// Authenticate must read those back. Without this, an operator token's
// approval could not be attributed to the static credential class.
func TestIssue_DefaultsAuthMethodStatic(t *testing.T) {
	pool := pgtest.NewPool(t)
	r := NewPostgresRepository(pool)

	tok, err := r.Issue(context.Background(), "github:42", []string{"read:runs"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok.AuthMethod != "static" {
		t.Errorf("Issue AuthMethod = %q, want static (column default)", tok.AuthMethod)
	}
	if tok.Provider != "" {
		t.Errorf("Issue Provider = %q, want empty for a static token", tok.Provider)
	}

	// Authenticate returns the persisted method/provider.
	got, err := r.Authenticate(context.Background(), tok.PlainText)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.AuthMethod != "static" {
		t.Errorf("Authenticate AuthMethod = %q, want static", got.AuthMethod)
	}
	if got.Provider != "" {
		t.Errorf("Authenticate Provider = %q, want empty for a static token", got.Provider)
	}
}

// TestIssueOAuth_StampsOAuthAndProvider pins the additive OAuth mint path:
// IssueOAuth records auth_method='oauth' + the provider, and Authenticate
// returns both. This is the persistence half the mint endpoint depends on.
func TestIssueOAuth_StampsOAuthAndProvider(t *testing.T) {
	pool := pgtest.NewPool(t)
	r := NewPostgresRepository(pool).(OAuthIssuer)

	tok, err := r.IssueOAuth(context.Background(), "github:alice", []string{"read:runs"}, "github")
	if err != nil {
		t.Fatalf("IssueOAuth: %v", err)
	}
	if tok.PlainText == "" {
		t.Error("IssueOAuth returned empty PlainText")
	}
	if tok.AuthMethod != "oauth" {
		t.Errorf("IssueOAuth AuthMethod = %q, want oauth", tok.AuthMethod)
	}
	if tok.Provider != "github" {
		t.Errorf("IssueOAuth Provider = %q, want github", tok.Provider)
	}

	got, err := r.Authenticate(context.Background(), tok.PlainText)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.AuthMethod != "oauth" || got.Provider != "github" {
		t.Errorf("Authenticate = (auth_method=%q, provider=%q), want (oauth, github)", got.AuthMethod, got.Provider)
	}
}

// TestIssueOAuth_RejectsEmptyProvider covers the provider-required guard: an
// OAuth mint with no provider is rejected before any row is written, so a row
// can never carry auth_method='oauth' with a NULL provider.
func TestIssueOAuth_RejectsEmptyProvider(t *testing.T) {
	pool := pgtest.NewPool(t)
	r := NewPostgresRepository(pool).(OAuthIssuer)

	_, err := r.IssueOAuth(context.Background(), "github:alice", nil, "")
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Errorf("err = %v, want provider-required error", err)
	}
}

// TestIssueOAuth_RejectsEmptySubject covers the subject-required guard shared
// with Issue.
func TestIssueOAuth_RejectsEmptySubject(t *testing.T) {
	pool := pgtest.NewPool(t)
	r := NewPostgresRepository(pool).(OAuthIssuer)

	_, err := r.IssueOAuth(context.Background(), "", nil, "github")
	if err == nil || !strings.Contains(err.Error(), "subject") {
		t.Errorf("err = %v, want subject-required error", err)
	}
}
