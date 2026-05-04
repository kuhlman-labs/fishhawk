package apitoken

import (
	"errors"
	"strings"
	"testing"
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
