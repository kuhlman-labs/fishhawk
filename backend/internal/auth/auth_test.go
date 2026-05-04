package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGeneratePlaintext_ProductPrefix(t *testing.T) {
	pt, err := generatePlaintext()
	if err != nil {
		t.Fatalf("generatePlaintext: %v", err)
	}
	if !strings.HasPrefix(pt, SessionTokenPrefix) {
		t.Errorf("plaintext = %q, want prefix %q", pt, SessionTokenPrefix)
	}
	// prefix (4) + base64(32 bytes) = 4 + 43 = 47.
	if len(pt) < 40 || len(pt) > 60 {
		t.Errorf("plaintext len = %d", len(pt))
	}
}

func TestGeneratePlaintext_Unique(t *testing.T) {
	a, _ := generatePlaintext()
	b, _ := generatePlaintext()
	if a == b {
		t.Error("two consecutive Generate calls returned identical token")
	}
}

func TestHashPlaintext_HappyPath(t *testing.T) {
	pt, _ := generatePlaintext()
	h, err := HashPlaintext(pt)
	if err != nil {
		t.Fatalf("HashPlaintext: %v", err)
	}
	if len(h) != 64 {
		t.Errorf("hash len = %d, want 64 (sha256 hex)", len(h))
	}
	h2, _ := HashPlaintext(pt)
	if h != h2 {
		t.Errorf("HashPlaintext non-deterministic")
	}
}

func TestHashPlaintext_Malformed(t *testing.T) {
	cases := []string{
		"",
		"random_string_no_prefix",
		"fhk_aaaa",  // wrong product prefix
		"fhs_",      // prefix only
		"fhs_short", // too short
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

func TestGenerateState_Unique(t *testing.T) {
	a, err := GenerateState()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateState()
	if a == b {
		t.Error("state values collided")
	}
	if len(a) < 20 {
		t.Errorf("state len = %d, want >= 20", len(a))
	}
}

func TestSession_IsExpired(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		s    Session
		want bool
	}{
		{
			name: "active",
			s: Session{
				SlidingExpiresAt:  now.Add(time.Hour),
				AbsoluteExpiresAt: now.Add(24 * time.Hour),
			},
			want: false,
		},
		{
			name: "sliding expired",
			s: Session{
				SlidingExpiresAt:  now.Add(-time.Minute),
				AbsoluteExpiresAt: now.Add(time.Hour),
			},
			want: true,
		},
		{
			name: "absolute expired",
			s: Session{
				SlidingExpiresAt:  now.Add(time.Hour),
				AbsoluteExpiresAt: now.Add(-time.Minute),
			},
			want: true,
		},
		{
			name: "revoked",
			s: Session{
				RevokedAt:         &now,
				SlidingExpiresAt:  now.Add(time.Hour),
				AbsoluteExpiresAt: now.Add(time.Hour),
			},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.IsExpired(now); got != c.want {
				t.Errorf("IsExpired = %v, want %v", got, c.want)
			}
		})
	}
}
