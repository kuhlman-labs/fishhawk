package account

import "testing"

func TestValidateResolvedBaseURL(t *testing.T) {
	valid := []string{
		"https://acme.ghe.com",
		"https://acme.ghe.com/api/v3",
	}
	for _, s := range valid {
		if err := ValidateResolvedBaseURL(s); err != nil {
			t.Errorf("ValidateResolvedBaseURL(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{
		"http://acme.ghe.com", // not https
		"https://",            // no host
		"acme.ghe.com",        // no scheme
		"://acme.ghe.com",     // empty scheme
		"https://\x00bad",     // parse error
	}
	for _, s := range invalid {
		if err := ValidateResolvedBaseURL(s); err == nil {
			t.Errorf("ValidateResolvedBaseURL(%q) = nil, want error", s)
		}
	}
}

// TestHostAllowed covers HostAllowed directly, including the fail-closed
// parse-error branch (a malformed URL → NOT allowed) which is unreachable from
// a routed consumer (ValidateResolvedBaseURL parses first) but must still fail
// closed if reached.
func TestHostAllowed(t *testing.T) {
	cases := []struct {
		name      string
		resolved  string
		allowlist []string
		want      bool
	}{
		{"exact host allowed", "https://acme.ghe.com", []string{"acme.ghe.com"}, true},
		{"suffix subdomain allowed", "https://acme.ghe.com/api/v3", []string{".ghe.com"}, true},
		{"host with port stripped", "https://acme.ghe.com:443", []string{"acme.ghe.com"}, true},
		{"uppercase host normalized", "https://ACME.GHE.COM", []string{"acme.ghe.com"}, true},
		{"look-alike rejected", "https://notghe.com", []string{".ghe.com"}, false},
		{"non-allowlisted rejected", "https://evil.example.com", []string{"acme.ghe.com"}, false},
		{"empty allowlist matches nothing", "https://acme.ghe.com", nil, false},
		{"malformed url fails closed", "https://\x00bad", []string{"anything"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostAllowed(tc.resolved, tc.allowlist); got != tc.want {
				t.Errorf("HostAllowed(%q, %v) = %v, want %v", tc.resolved, tc.allowlist, got, tc.want)
			}
		})
	}
}

// TestMatchesHostAllowlist pins the label-boundary matcher directly (the
// dot-prefixed suffix vs the raw-substring trap #2093).
func TestMatchesHostAllowlist(t *testing.T) {
	cases := []struct {
		host      string
		allowlist []string
		want      bool
	}{
		{"acme.ghe.com", []string{"acme.ghe.com"}, true},           // exact
		{"acme.ghe.com", []string{".ghe.com"}, true},               // suffix subdomain
		{"deep.acme.ghe.com", []string{".ghe.com"}, true},          // multi-label subdomain
		{"ghe.com", []string{"ghe.com"}, true},                     // exact apex
		{"ghe.com", []string{".ghe.com"}, false},                   // apex not admitted by dotted suffix
		{"notghe.com", []string{".ghe.com"}, false},                // look-alike vs dotted suffix
		{"notghe.com", []string{"ghe.com"}, false},                 // look-alike vs exact apex
		{"other.example.com", []string{"acme.ghe.com"}, false},     // unrelated
		{"acme.ghe.com", []string{".other.com", ".ghe.com"}, true}, // second entry matches
		{"acme.ghe.com", nil, false},                               // empty allowlist matches nothing
	}
	for _, tc := range cases {
		if got := matchesHostAllowlist(tc.host, tc.allowlist); got != tc.want {
			t.Errorf("matchesHostAllowlist(%q, %v) = %v, want %v", tc.host, tc.allowlist, got, tc.want)
		}
	}
}
