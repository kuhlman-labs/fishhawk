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
