package auth_test

import (
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

func TestIsEMUOAuthHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"github.com host", "https://github.com/login/oauth/authorize", false},
		{"api.github.com", "https://api.github.com", false},
		{"ghes host", "https://github.example.com/login/oauth/authorize", false},
		{"garbage", "::not a url::", false},
		{"data-resident host only", "acme.ghe.com", true},
		{"data-resident url", "https://acme.ghe.com/login/oauth/authorize", true},
		{"data-resident api url", "https://acme.ghe.com/api/v3", true},
		{"uppercase host", "https://ACME.GHE.COM/login/oauth/authorize", true},
		{"host with port", "https://acme.ghe.com:8443/login/oauth/authorize", true},
		// A bare ghe.com is not a tenant endpoint.
		{"bare ghe.com", "https://ghe.com/login/oauth/authorize", false},
		// A lookalike suffix must not match.
		{"lookalike suffix", "https://notghe.com/login/oauth/authorize", false},
		{"suffix in path only", "https://github.com/ghe.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := auth.IsEMUOAuthHost(tc.in); got != tc.want {
				t.Errorf("IsEMUOAuthHost(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestEnterpriseShortCode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		login    string
		wantCode string
		wantOK   bool
	}{
		{"emu login", "alice_acme", "acme", true},
		{"no underscore", "alice", "", false},
		{"empty short code", "alice_", "", false},
		{"empty username half", "_acme", "", false},
		{"empty login", "", "", false},
		{"lone underscore", "_", "", false},
		{"splits on the LAST underscore", "a_b_c", "c", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, ok := auth.EnterpriseShortCode(tc.login)
			if code != tc.wantCode || ok != tc.wantOK {
				t.Errorf("EnterpriseShortCode(%q) = (%q, %v), want (%q, %v)",
					tc.login, code, ok, tc.wantCode, tc.wantOK)
			}
		})
	}
}
