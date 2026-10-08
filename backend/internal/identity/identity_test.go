package identity

import (
	"context"
	"errors"
	"testing"
)

// Compile-time interface-satisfaction assertions for both concrete
// providers (also asserted in their own files; repeated here as the
// package's single documented contract check).
var (
	_ IdentityProvider = (*GitHubIdentityProvider)(nil)
	_ IdentityProvider = (*GitLabIdentityProvider)(nil)
	_ IdentityProvider = (*NoOpIdentityProvider)(nil)
)

func TestNoOp_SafeDefaults(t *testing.T) {
	p := NewNoOp()

	if _, err := p.VerifyUser(context.Background(), nil); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("VerifyUser err = %v, want ErrNotConfigured", err)
	}

	perm, err := p.PermissionLevel(context.Background(), "owner/repo", "github:octocat")
	if err != nil {
		t.Errorf("PermissionLevel err = %v, want nil", err)
	}
	if perm != PermissionNone {
		t.Errorf("PermissionLevel = %q, want %q", perm, PermissionNone)
	}

	member, err := p.ResolveMembership(context.Background(), "acme", "github:octocat")
	if err != nil {
		t.Errorf("ResolveMembership err = %v, want nil", err)
	}
	if member {
		t.Error("ResolveMembership = true, want false (deny-by-default)")
	}
}

// TestProviderOf covers the subject discriminator. It is total by
// contract — it sits on the authorization path, where a parse failure
// must degrade to "unknown provider" (which every caller treats as a
// deny) rather than to a 500 — so every shape below returns a string
// rather than erroring.
func TestProviderOf(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    string
	}{
		{"github subject", "github:octocat", ProviderGitHub},
		{"gitlab subject", "gitlab:alice", ProviderGitLab},
		{"unqualified login reports no provider", "octocat", ""},
		{"empty", "", ""},
		{"leading colon reports an empty provider", ":octocat", ""},
		{"only the FIRST colon splits", "mcp:run:abc123", "mcp"},
		{"login containing a colon", "gitlab:al:ice", ProviderGitLab},
		{"operator agent subject", "operator-agent:runner", "operator-agent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderOf(tc.subject); got != tc.want {
				t.Errorf("ProviderOf(%q) = %q, want %q", tc.subject, got, tc.want)
			}
		})
	}
}

// TestIsConfigured pins the primitive the server's provider enumeration
// is built on (E66.4 / #2392, binding constraint 8). The NoOp exclusion is
// the load-bearing case: NoOp satisfies IdentityProvider so an
// unconfigured backend has something inert to hold, and a caller that
// merely counted non-nil interface values would advertise it as a
// configured forge and hand a client a provider that can authenticate
// nobody.
func TestIsConfigured(t *testing.T) {
	var nilIface IdentityProvider
	var nilNoOp *NoOpIdentityProvider
	var nilGitHub *GitHubIdentityProvider
	var nilGitLab *GitLabIdentityProvider

	tests := []struct {
		name string
		p    IdentityProvider
		want bool
	}{
		{"nil interface", nilIface, false},
		{"NoOp is never configured", NewNoOp(), false},
		{"typed-nil NoOp is never configured", nilNoOp, false},
		{"typed-nil GitHub provider", nilGitHub, false},
		{"typed-nil GitLab provider", nilGitLab, false},
		{"real GitHub provider", NewGitHubIdentityProvider("Iv1.abc", nil), true},
		{"real GitLab provider", NewGitLabIdentityProvider("https://gitlab.example.com", "cid", nil), true},
		{"an unrecognized implementation is configured", stubIdentityProvider{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsConfigured(tc.p); got != tc.want {
				t.Errorf("IsConfigured(%T) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}

// stubIdentityProvider is a non-NoOp implementation the package does not
// know about. It pins the deny-list posture: the exclusion names the
// known-inert providers, so a future real provider is reported configured
// rather than silently dropped from discovery.
type stubIdentityProvider struct{}

func (stubIdentityProvider) VerifyUser(context.Context, DeviceCodePrompt) (string, error) {
	return "stub:someone", nil
}

func (stubIdentityProvider) VerifyAccessToken(context.Context, string) (string, error) {
	return "stub:someone", nil
}

func (stubIdentityProvider) PermissionLevel(context.Context, string, string) (Permission, error) {
	return PermissionAdmin, nil
}

func (stubIdentityProvider) ResolveMembership(context.Context, string, string) (bool, error) {
	return true, nil
}

// TestNoOp_VerifyAccessTokenFailsClosed pins the NoOp's re-verify leg
// alongside the VerifyUser leg TestNoOp_SafeDefaults already covers: an
// OAuth-unconfigured backend must degrade to ErrNotConfigured rather than
// mint against an unverifiable subject.
func TestNoOp_VerifyAccessTokenFailsClosed(t *testing.T) {
	subject, err := NewNoOp().VerifyAccessToken(context.Background(), "some-token")
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("VerifyAccessToken err = %v, want ErrNotConfigured", err)
	}
	if subject != "" {
		t.Errorf("subject = %q, want empty", subject)
	}
}

// TestCanonicalMember pins the member half of the approvals.members
// canonicalisation rule (#4116): a provider-qualified member is verbatim, a
// plain one is qualified with the run's forge, and nothing is case-folded or
// trimmed.
func TestCanonicalMember(t *testing.T) {
	tests := []struct {
		name, member, forge, want string
	}{
		{"qualified member is verbatim", "github:kuhlman-labs", ProviderGitLab, "github:kuhlman-labs"},
		{"plain member takes the run forge (github)", "alice", ProviderGitHub, "github:alice"},
		{"plain member takes the run forge (gitlab)", "alice", ProviderGitLab, "gitlab:alice"},
		{"no case folding", "Alice", ProviderGitHub, "github:Alice"},
		{"no trimming", " alice", ProviderGitHub, "github: alice"},
		{"empty member stays empty", "", ProviderGitHub, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalMember(tc.member, tc.forge); got != tc.want {
				t.Errorf("CanonicalMember(%q, %q) = %q, want %q", tc.member, tc.forge, got, tc.want)
			}
		})
	}
}

// TestSubjectListed pins the exact-match approver allow-list rule the
// approval gate and `fishhawkd approver-members` both run (#4116). The
// subject is NEVER qualified and must already be a forge identity, so a
// static token subject, an agent subject and a run-bound MCP subject match
// nothing — even a member spelled identically.
func TestSubjectListed(t *testing.T) {
	const runUUID = "mcp:run:0b8f5a8e-6a3c-4c1e-9d55-3f1f1e0c2a11"
	tests := []struct {
		name    string
		members []string
		subject string
		forge   string
		want    bool
	}{
		{"qualified member + same subject", []string{"github:kuhlman-labs"}, "github:kuhlman-labs", ProviderGitHub, true},
		{"qualified member admits on any run forge", []string{"github:kuhlman-labs"}, "github:kuhlman-labs", ProviderGitLab, true},
		{"plain member on github run + github subject", []string{"alice"}, "github:alice", ProviderGitHub, true},
		{"plain member on gitlab run + github subject", []string{"alice"}, "github:alice", ProviderGitLab, false},
		{"plain member on gitlab run + gitlab subject", []string{"alice"}, "gitlab:alice", ProviderGitLab, true},
		{"static plain subject never qualified", []string{"alice"}, "alice", ProviderGitHub, false},
		{"static MCP token subject refused", []string{"github:kuhlman-labs"}, "brett@local-mcp", ProviderGitHub, false},
		{"static subject listed verbatim still refused", []string{"brett@local-mcp"}, "brett@local-mcp", ProviderGitHub, false},
		{"casing mismatch refuses", []string{"github:alice"}, "github:Alice", ProviderGitHub, false},
		{"trailing space refuses", []string{"github:alice "}, "github:alice", ProviderGitHub, false},
		{"empty list", nil, "github:alice", ProviderGitHub, false},
		{"empty explicit list", []string{}, "github:alice", ProviderGitHub, false},
		{"empty subject", []string{"github:alice"}, "", ProviderGitHub, false},
		{"agent-shaped synthetic delegated subject", []string{"github:alice", "alice"}, "operator-agent/delegated", ProviderGitHub, false},
		{"anonymous", []string{"anonymous"}, "anonymous", ProviderGitHub, false},
		{"run-bound MCP subject vs unrelated list", []string{"github:alice"}, runUUID, ProviderGitHub, false},
		{"run-bound MCP subject vs a list naming it verbatim", []string{runUUID, "github:alice"}, runUUID, ProviderGitHub, false},
		{"second member matches", []string{"github:bob", "github:alice"}, "github:alice", ProviderGitHub, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SubjectListed(tc.members, tc.subject, tc.forge); got != tc.want {
				t.Errorf("SubjectListed(%q, %q, %q) = %v, want %v", tc.members, tc.subject, tc.forge, got, tc.want)
			}
		})
	}
}
