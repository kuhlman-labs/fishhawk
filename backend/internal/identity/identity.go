// Package identity is the backend's forge-neutral identity surface:
// verifying a human operator, reading their permission tier on a
// repository, and resolving org/team membership. The vocabulary here
// deliberately carries NO github.com/* forge type — the interface
// speaks provider-qualified subject strings ("github:<login>") and a
// forge-neutral Permission enum so a non-GitHub forge can implement
// the same contract later.
//
// The concrete GitHub implementation (github.go) is a hand-rolled
// REST client mirroring internal/githubclient + internal/githuboidc:
// net/http + encoding/json, a test-overridable base URL, and explicit
// rate-limit handling. The GitLab implementation (gitlab.go) is its
// co-equal sibling (E66.4 / #2392): the RFC 8628 device flow against a
// configurable GitLab base URL, server-side access-token re-verification,
// and a bounded paginated exact-match members walk. The server depends
// only on IdentityProvider; every forge specific lives in this package
// and its _test.go files.
package identity

import (
	"context"
	"errors"
	"strings"
)

// Permission is the forge-neutral repository permission vocabulary.
// Concrete providers map their own tier names onto these constants;
// callers gate on the ordered set without knowing the forge.
type Permission string

// The permission tiers, least to most privileged. PermissionNone is
// the deny-by-default returned when a subject has no access (or the
// forge returns 404) — never treat an unknown tier as write.
const (
	PermissionNone     Permission = "none"
	PermissionRead     Permission = "read"
	PermissionTriage   Permission = "triage"
	PermissionWrite    Permission = "write"
	PermissionMaintain Permission = "maintain"
	PermissionAdmin    Permission = "admin"
)

// permissionRank orders the tiers so callers can gate on "at least" a
// minimum. An unrecognized ACTUAL permission is absent from the map and
// ranks 0 (none), so a garbage value never satisfies a real minimum —
// deny-by-default. An unrecognized MINIMUM is handled separately by
// AtLeast (comma-ok lookup, fails closed) rather than by this map alone,
// since a plain index would silently treat it as rank 0 and be satisfied
// by any permission.
var permissionRank = map[Permission]int{
	PermissionNone:     0,
	PermissionRead:     1,
	PermissionTriage:   2,
	PermissionWrite:    3,
	PermissionMaintain: 4,
	PermissionAdmin:    5,
}

// AtLeast reports whether p is at least as privileged as min in the
// ordered tier set (none < read < triage < write < maintain < admin).
// Used by the token-mint authz gate (E39.3 / #1708) to require a
// configured minimum repository permission of the verified subject. An
// unrecognized min (e.g. a typo'd config value) is not a recognized
// tier and returns false — deny-by-default — rather than being treated
// as PermissionNone, which any permission (including PermissionNone)
// would satisfy.
func (p Permission) AtLeast(min Permission) bool {
	minRank, ok := permissionRank[min]
	if !ok {
		return false
	}
	return permissionRank[p] >= minRank
}

// ParsePermission maps a tier name to a Permission, reporting ok=false
// for an unrecognized name so a caller (e.g. serve.go flag parsing) can
// fail configuration loudly rather than silently under-gating on a
// typo. The empty string is not recognized; callers supply their own
// default before calling.
func ParsePermission(name string) (Permission, bool) {
	switch Permission(name) {
	case PermissionNone, PermissionRead, PermissionTriage,
		PermissionWrite, PermissionMaintain, PermissionAdmin:
		return Permission(name), true
	default:
		return PermissionNone, false
	}
}

// The provider names that qualify a subject. Every caller that needs to
// discriminate a forge uses these constants rather than re-spelling the
// discriminator as a string literal, so the vocabulary has exactly one
// definition (E66.4 / #2392).
const (
	ProviderGitHub = "github"
	ProviderGitLab = "gitlab"
)

// ProviderOf returns the provider that qualifies subject — the substring
// before the first ':' of a "<provider>:<login>" subject. An unqualified
// subject (no ':') returns "", so a caller can tell "no provider stated"
// apart from a stated one rather than guessing a default. The function is
// deliberately total and allocation-light: it never errors and never
// panics, because it sits on the authorization path where a parse failure
// must degrade to "unknown provider" (which every caller treats as a
// deny) rather than to a 500.
func ProviderOf(subject string) string {
	i := strings.IndexByte(subject, ':')
	if i < 0 {
		return ""
	}
	return subject[:i]
}

// IsConfigured reports whether p is a provider that can actually
// authenticate somebody. It is false for a nil interface, for a
// typed-nil concrete provider, and for *NoOpIdentityProvider — the
// deny-by-default stand-in, which is installed precisely BECAUSE no
// forge is configured and so must never be enumerated as one (E66.4 /
// #2392, binding constraint 8).
//
// This is the single primitive the server's provider enumeration is
// built on, and it lives here so the server and the fishhawkd wiring
// consult ONE definition of "configured" rather than each re-deriving
// it. An unrecognized concrete implementation is reported configured:
// the exclusion is a deny-list of known-inert providers, not an
// allow-list of blessed ones, so a future real provider is not
// silently dropped from discovery.
func IsConfigured(p IdentityProvider) bool {
	switch v := p.(type) {
	case nil:
		return false
	case *NoOpIdentityProvider:
		// Catches a typed-nil NoOp too: it is inert either way.
		return false
	case *GitHubIdentityProvider:
		return v != nil
	case *GitLabIdentityProvider:
		return v != nil
	default:
		return true
	}
}

// DeviceCodePrompt is invoked once during VerifyUser with the user
// code and verification URI the human must visit to authorize the
// device flow. It is a plain func so no GitHub-specific display type
// crosses the interface boundary; the caller renders it however it
// likes (CLI stdout, an issue comment, a UI banner).
type DeviceCodePrompt func(userCode, verificationURI string)

// Errors callers may switch on.
var (
	// ErrVerificationTimeout means the device-flow authorization did
	// not complete before the device code expired or the context was
	// cancelled. The forge returns an expired-token / pending loop;
	// this is the terminal timeout the caller surfaces to the human.
	ErrVerificationTimeout = errors.New("identity: verification timed out")

	// ErrRateLimited means the forge rejected a read with a
	// rate-limit signal (403/429 carrying X-RateLimit-Remaining: 0 or
	// a Retry-After header). Distinct so the caller can back off
	// rather than treating it as a hard authorization failure.
	ErrRateLimited = errors.New("identity: forge rate limited")

	// ErrNotConfigured means no identity provider is wired — the
	// NoOp provider returns this from VerifyUser so an
	// OAuth-unconfigured backend fails closed instead of silently
	// authorizing.
	ErrNotConfigured = errors.New("identity: no identity provider configured")
)

// IdentityProvider is the forge-neutral identity contract. All three
// methods take a context and speak provider-qualified subject strings;
// no github.com/* forge type appears in any signature. The name is
// deliberate (ADR / #1706): the server's Config field is
// identity.IdentityProvider so a future non-GitHub provider slots in
// without renaming the seam.
//
// SUBJECT SHAPE. Every subject is "<provider>:<login>" — "github:octocat",
// "gitlab:alice" — with the provider drawn from the ProviderGitHub /
// ProviderGitLab constants and readable back out with ProviderOf. A
// provider MUST emit only its own qualification and MUST refuse to
// resolve a subject qualified for a different provider: a github: login
// and a gitlab: login of the same spelling are distinct accounts, so
// resolving one against the other's forge would answer an authorization
// question about the wrong human.
//
// THE NoOp IS NOT A CONFIGURED PROVIDER. NoOpIdentityProvider satisfies
// this interface so an unconfigured backend has something inert to hold,
// but it can authenticate nobody. Callers enumerating "which providers
// are configured" must filter through IsConfigured rather than counting
// non-nil interface values (E66.4 / #2392, binding constraint 8).
//
//nolint:revive // interface name is the mandated forge-neutral seam name; the identity.IdentityProvider "stutter" is intentional.
type IdentityProvider interface {
	// VerifyUser drives an interactive verification (the GitHub OAuth
	// device flow) to completion and returns the provider-qualified
	// subject of the authenticated human. prompt is invoked once with
	// the user code + verification URI. Returns ErrVerificationTimeout
	// if authorization does not complete in time.
	VerifyUser(ctx context.Context, prompt DeviceCodePrompt) (subject string, err error)

	// VerifyAccessToken verifies a forge access token — a GitHub user
	// access token the CLI obtained by driving the device flow itself —
	// server-side and returns the provider-qualified subject
	// ("github:<login>") it authenticates. It is the "server-side
	// re-verify" half of the CLI-driven device-flow login (E39.3 /
	// #1708): the CLI drives the device flow and posts the resulting
	// access token; the backend re-derives the subject from the token
	// rather than trusting a client-supplied login. Returns
	// ErrNotConfigured on the NoOp provider (no forge to verify against).
	VerifyAccessToken(ctx context.Context, accessToken string) (subject string, err error)

	// PermissionLevel returns the subject's forge-neutral permission
	// tier on repo (in "owner/name" form). Returns PermissionNone for
	// a subject with no access (or a 404). Returns ErrRateLimited when
	// the forge signals rate limiting.
	PermissionLevel(ctx context.Context, repo, subject string) (Permission, error)

	// ResolveMembership reports whether subject belongs to the org or
	// team named by ref. ref is either "org" (org membership) or
	// "org/team" (team membership). Returns false for a non-member (or
	// a 404). Returns ErrRateLimited when the forge signals rate
	// limiting.
	ResolveMembership(ctx context.Context, ref, subject string) (bool, error)
}
