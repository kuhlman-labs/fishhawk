// Package role resolves workflow-spec role references like
// `@org/tech-leads` to the GitHub-login allowlist of users who
// satisfy them. Used by the approval handler to enforce that a
// submitting subject is in the gate's approvers (E4.4 / #50).
//
// Resolution path:
//
//	spec gate "approvers": { any_of: ["tech_lead"] }
//	spec roles: { tech_lead: { members: ["@org/tech-leads"] } }
//	GitHub API: /orgs/org/teams/tech-leads/members → [...logins...]
//	approval check: subject in expanded set?
//
// A role can carry multiple `@org/team` refs; the package fetches
// each and unions the results. Plain strings (without `@`) are
// passed through as literal logins, since the spec schema allows
// either format.
//
// Caching: per-team membership is cached with a configurable TTL.
// Default 5 minutes — long enough that high-volume approval bursts
// don't pummel the GitHub API, short enough that a deliberate role
// change (removing a member ahead of an audit) takes effect within
// one cache cycle. Operators with stricter staleness budgets pass
// a smaller TTL at construction time.
package role

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// DefaultCacheTTL is the staleness budget we'll accept by default.
// Overridable via NewResolver's options.
const DefaultCacheTTL = 5 * time.Minute

// TeamMember is the slice of GitHub's team-member object the
// resolver consumes. Mirrors githubclient.TeamMember so the
// production wiring needs no glue type.
type TeamMember struct {
	Login string
	ID    int64
}

// TeamLister is the GitHub API surface the resolver uses. Defining
// it here as an interface lets tests substitute a stub without
// pulling in the whole githubclient package.
type TeamLister interface {
	ListTeamMembers(ctx context.Context, installationID int64, org, slug string) ([]TeamMember, error)
}

// Errors callers may want to switch on.
var (
	// ErrUnknownRole means the named role isn't in the workflow
	// spec's `roles:` map. Distinct from "role exists but has no
	// members" so the approval handler can surface a precise
	// 400 / 403.
	ErrUnknownRole = errors.New("role: unknown role name")

	// ErrInvalidRef means a member ref didn't parse as a literal
	// login or `@org/team`. Defense in depth: the spec schema
	// already constrains the format.
	ErrInvalidRef = errors.New("role: invalid member reference")
)

// Resolver expands role names to login allowlists and decides
// whether a subject is authorized to approve a gate.
type Resolver struct {
	gh    TeamLister
	ttl   time.Duration
	now   func() time.Time
	mu    sync.Mutex
	teams map[string]cachedTeam
}

type cachedTeam struct {
	members   []string
	expiresAt time.Time
}

// Option configures the Resolver. Defaults: TTL=5m, clock=time.Now.
type Option func(*Resolver)

// WithTTL overrides the per-team cache TTL. Zero / negative falls
// back to DefaultCacheTTL.
func WithTTL(d time.Duration) Option {
	return func(r *Resolver) {
		if d > 0 {
			r.ttl = d
		}
	}
}

// WithNow overrides the clock for cache expiry. Tests inject a
// fake clock; production leaves it unset.
func WithNow(now func() time.Time) Option {
	return func(r *Resolver) {
		r.now = now
	}
}

// NewResolver returns a Resolver wired to the given TeamLister.
func NewResolver(gh TeamLister, opts ...Option) *Resolver {
	r := &Resolver{
		gh:    gh,
		ttl:   DefaultCacheTTL,
		now:   func() time.Time { return time.Now() },
		teams: map[string]cachedTeam{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// ExpandRole returns the deduplicated set of GitHub logins that
// satisfy roleName per the spec's `roles:` map. Members that look
// like `@org/team` are resolved against the GitHub API; plain
// strings pass through as literal logins.
//
// Returns ErrUnknownRole when roleName isn't in roles. A role with
// zero members returns an empty slice and a nil error — that's a
// valid (if odd) configuration choice.
func (r *Resolver) ExpandRole(ctx context.Context, installationID int64, roleName string, roles map[string]spec.Role) ([]string, error) {
	role, ok := roles[roleName]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRole, roleName)
	}
	seen := map[string]struct{}{}
	out := []string{}
	for _, ref := range role.Members {
		members, err := r.expandMemberRef(ctx, installationID, ref)
		if err != nil {
			return nil, err
		}
		for _, login := range members {
			if _, ok := seen[login]; ok {
				continue
			}
			seen[login] = struct{}{}
			out = append(out, login)
		}
	}
	return out, nil
}

// CanApprove reports whether subject satisfies approvers per the
// gate's any_of / all_of semantics:
//
//   - any_of: subject must be in at least one of the named roles
//   - all_of: subject must be in every named role
//
// The schema enforces exactly one of any_of/all_of is set; nil
// approvers (gate type=check) returns (false, nil) — the caller
// should treat that as "not an approval gate; reject."
func (r *Resolver) CanApprove(ctx context.Context, installationID int64, approvers *spec.Approvers, roles map[string]spec.Role, subject string) (bool, error) {
	if approvers == nil {
		return false, nil
	}
	if subject == "" {
		return false, nil
	}
	subject = canonicalLogin(subject)

	switch {
	case len(approvers.AnyOf) > 0:
		for _, role := range approvers.AnyOf {
			members, err := r.ExpandRole(ctx, installationID, role, roles)
			if err != nil {
				return false, err
			}
			if containsLogin(members, subject) {
				return true, nil
			}
		}
		return false, nil
	case len(approvers.AllOf) > 0:
		for _, role := range approvers.AllOf {
			members, err := r.ExpandRole(ctx, installationID, role, roles)
			if err != nil {
				return false, err
			}
			if !containsLogin(members, subject) {
				return false, nil
			}
		}
		return true, nil
	default:
		// Schema enforces this can't happen, but defense in depth:
		// no any_of and no all_of means no approver can satisfy.
		return false, nil
	}
}

// expandMemberRef handles one entry from a role's `members` list.
// `@org/team` → ListTeamMembers (with cache); anything else →
// passthrough as a literal login.
func (r *Resolver) expandMemberRef(ctx context.Context, installationID int64, ref string) ([]string, error) {
	if !strings.HasPrefix(ref, "@") {
		// Literal login. No API call.
		return []string{strings.TrimSpace(ref)}, nil
	}
	body := ref[1:] // drop leading "@"
	parts := strings.SplitN(body, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRef, ref)
	}
	org, slug := parts[0], parts[1]
	return r.team(ctx, installationID, org, slug)
}

// team fetches an org/team's membership, returning a cached copy
// when fresh. Concurrent callers on a cold cache may both fetch;
// fanning that out to a single in-flight request is cheap to add
// later if the load argues for it.
func (r *Resolver) team(ctx context.Context, installationID int64, org, slug string) ([]string, error) {
	key := cacheKey(org, slug)

	r.mu.Lock()
	if entry, ok := r.teams[key]; ok && r.now().Before(entry.expiresAt) {
		members := append([]string(nil), entry.members...)
		r.mu.Unlock()
		return members, nil
	}
	r.mu.Unlock()

	if r.gh == nil {
		return nil, errors.New("role: TeamLister not configured")
	}
	got, err := r.gh.ListTeamMembers(ctx, installationID, org, slug)
	if err != nil {
		return nil, fmt.Errorf("role: list team %s/%s: %w", org, slug, err)
	}
	logins := make([]string, 0, len(got))
	for _, m := range got {
		logins = append(logins, canonicalLogin(m.Login))
	}

	r.mu.Lock()
	r.teams[key] = cachedTeam{
		members:   append([]string(nil), logins...),
		expiresAt: r.now().Add(r.ttl),
	}
	r.mu.Unlock()
	return logins, nil
}

// Invalidate evicts the cache entry for an org/team. Callers
// invoke this after explicit role-change events (e.g., a webhook
// signaling a team membership change) to bypass the TTL.
func (r *Resolver) Invalidate(org, slug string) {
	r.mu.Lock()
	delete(r.teams, cacheKey(org, slug))
	r.mu.Unlock()
}

func cacheKey(org, slug string) string {
	return strings.ToLower(org) + "/" + strings.ToLower(slug)
}

// canonicalLogin lowercases the login. GitHub treats logins case-
// insensitively but echoes the registration casing back; the
// resolver normalizes so a subject "Octocat" matches a member
// "octocat".
func canonicalLogin(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func containsLogin(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
