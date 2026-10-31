// Package defaultbranch resolves a repository's base branch from the
// forge's default_branch through ONE cached resolver (#4660, the ADR-110
// rule-4 realization for the base branch). Every server-side consumer that
// used to fall back to a literal "main" — the orchestrator's consolidated PR,
// fan-in base and consolidated-review trigger, the first-round review re-run
// and githubclient.ListRulesetRequiredChecks — reads the branch through a
// Source and, when the read fails, refuses with a named Reason on its own
// existing failure surface instead of guessing a branch.
//
// The package imports only forge and the standard library, so githubclient,
// orchestrator and server can all depend on it without an import cycle.
package defaultbranch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// Reason names why a default branch could not be resolved. The vocabulary is
// closed: exactly the eight constants below.
type Reason string

const (
	// ReasonResolverUnconfigured means no Source (or a Resolver with no Getter)
	// is wired — the CLI/dev posture or a fixture that wires no resolver.
	ReasonResolverUnconfigured Reason = "resolver_unconfigured"
	// ReasonForgeUnsupported means the run's forge has no default-branch read on
	// this path (a GitLab run reaching a GitHub-only consumer).
	ReasonForgeUnsupported Reason = "forge_unsupported"
	// ReasonNoInstallation means the credential scope is zero, so no forge read
	// can authenticate.
	ReasonNoInstallation Reason = "no_installation"
	// ReasonRepositoryInvalid means the repository reference is malformed (empty
	// owner or name, or an unparseable owner/name string).
	ReasonRepositoryInvalid Reason = "repository_invalid"
	// ReasonRepositoryNotVisible means the forge answered not-found — the
	// repository does not exist or the installation cannot see it.
	ReasonRepositoryNotVisible Reason = "repository_not_visible"
	// ReasonForbidden means the forge rejected the credential.
	ReasonForbidden Reason = "forbidden"
	// ReasonDefaultBranchMissing means the forge answered but reported no
	// default branch (nil repository or an empty/whitespace value).
	ReasonDefaultBranchMissing Reason = "default_branch_missing"
	// ReasonLookupFailed means any other read failure (transport error, timeout,
	// 5xx, decode failure).
	ReasonLookupFailed Reason = "lookup_failed"
)

// UnresolvedError is the typed refusal every consumer surfaces. Callers read
// the Reason with ReasonOf rather than matching on the message.
type UnresolvedError struct {
	Repo   forge.RepoRef
	Reason Reason
	Err    error
}

// Error names the repository, the reason and (when present) the cause.
func (e *UnresolvedError) Error() string {
	msg := fmt.Sprintf("default branch of %s unresolved: %s", e.Repo, e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap exposes the underlying cause (a forge sentinel, a context error).
func (e *UnresolvedError) Unwrap() error { return e.Err }

// Unresolved builds an *UnresolvedError.
func Unresolved(repo forge.RepoRef, reason Reason, cause error) *UnresolvedError {
	return &UnresolvedError{Repo: repo, Reason: reason, Err: cause}
}

// ReasonOf reports the Reason carried by an *UnresolvedError anywhere in
// err's chain.
func ReasonOf(err error) (Reason, bool) {
	var ue *UnresolvedError
	if errors.As(err, &ue) {
		return ue.Reason, true
	}
	return "", false
}

// Getter is the forge read the Resolver wraps. *githubclient.Client
// satisfies it through its RepoRef/Repository aliases.
type Getter interface {
	GetRepository(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef) (*forge.Repository, error)
}

// Source resolves a repository's default branch. Production wires the
// githubclient's shared *Resolver; tests may wire Static.
type Source interface {
	Resolve(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef) (string, error)
}

// Resolve is the ONE nil check every consumer uses: a nil Source refuses with
// resolver_unconfigured instead of guessing a branch.
func Resolve(ctx context.Context, src Source, scope forge.CredentialScope, repo forge.RepoRef) (string, error) {
	if src == nil {
		return "", Unresolved(repo, ReasonResolverUnconfigured, nil)
	}
	return src.Resolve(ctx, scope, repo)
}

// Static is a fixed-branch Source for test fixtures (the migration target of
// the retired Orchestrator.DefaultRef). An empty Static refuses with
// resolver_unconfigured. Production never wires it.
type Static string

// Resolve returns the fixed branch, ignoring scope and repo.
func (s Static) Resolve(_ context.Context, _ forge.CredentialScope, repo forge.RepoRef) (string, error) {
	if s == "" {
		return "", Unresolved(repo, ReasonResolverUnconfigured, nil)
	}
	return string(s), nil
}

const (
	// lookupTimeout caps one forge read, layered over the caller's ctx.
	lookupTimeout = 10 * time.Second
	// positiveTTL bounds how long a resolved branch is reused: a
	// default-branch rename is observed within this window.
	positiveTTL = 10 * time.Minute
	// negativeTTL bounds how long a failed read is reused: a recovered forge
	// is observed within this window.
	negativeTTL = 30 * time.Second
	// maxEntries bounds the cache map; past it expired entries are swept and,
	// if still over, the map is cleared.
	maxEntries = 1024
)

type cacheEntry struct {
	branch  string
	err     *UnresolvedError
	expires time.Time
}

// Resolver is the forge-backed, TTL-cached Source. Construct with New; the
// zero value (nil getter) refuses every lookup with resolver_unconfigured.
// Safe for concurrent use. Concurrent cold misses for the same key are not
// deduplicated: each may read the forge once.
type Resolver struct {
	getter Getter

	timeout     time.Duration
	positiveTTL time.Duration
	negativeTTL time.Duration
	now         func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

// New returns a Resolver over g.
func New(g Getter) *Resolver {
	return &Resolver{
		getter:      g,
		timeout:     lookupTimeout,
		positiveTTL: positiveTTL,
		negativeTTL: negativeTTL,
		now:         time.Now,
		entries:     make(map[string]cacheEntry),
	}
}

// Resolve returns repo's default branch under scope. Locally decidable
// refusals (no getter, zero scope, invalid repo) never read the forge and are
// never cached. A forge read is cached for positiveTTL on success and
// negativeTTL on failure, except a failure observed after the CALLER's ctx is
// done, which is never cached so a cancelled request cannot poison the next
// caller.
func (r *Resolver) Resolve(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef) (string, error) {
	if r == nil || r.getter == nil {
		return "", Unresolved(repo, ReasonResolverUnconfigured, nil)
	}
	if scope.IsZero() {
		return "", Unresolved(repo, ReasonNoInstallation, nil)
	}
	if repo.Owner == "" || repo.Name == "" {
		return "", Unresolved(repo, ReasonRepositoryInvalid, nil)
	}

	key := scope.Ref() + "\x00" + strings.ToLower(repo.Owner) + "/" + strings.ToLower(repo.Name)
	if branch, uerr, ok := r.lookup(key); ok {
		if uerr != nil {
			return "", uerr
		}
		return branch, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, r.timeout)
	got, err := r.getter.GetRepository(lookupCtx, scope, repo)
	cancel()

	var branch string
	var uerr *UnresolvedError
	switch {
	case err != nil:
		uerr = Unresolved(repo, classify(err), err)
	case got == nil || strings.TrimSpace(got.DefaultBranch) == "":
		uerr = Unresolved(repo, ReasonDefaultBranchMissing, nil)
	default:
		branch = got.DefaultBranch
	}

	if uerr != nil {
		if ctx.Err() == nil {
			r.store(key, cacheEntry{err: uerr, expires: r.now().Add(r.negativeTTL)})
		}
		return "", uerr
	}
	r.store(key, cacheEntry{branch: branch, expires: r.now().Add(r.positiveTTL)})
	return branch, nil
}

// classify maps a forge-read error to its Reason.
func classify(err error) Reason {
	switch {
	case errors.Is(err, forge.ErrNotFound):
		return ReasonRepositoryNotVisible
	case errors.Is(err, forge.ErrForbidden):
		return ReasonForbidden
	default:
		return ReasonLookupFailed
	}
}

func (r *Resolver) lookup(key string) (string, *UnresolvedError, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok {
		return "", nil, false
	}
	if !r.now().Before(e.expires) {
		delete(r.entries, key)
		return "", nil, false
	}
	return e.branch, e.err, true
}

func (r *Resolver) store(key string, e cacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]cacheEntry)
	}
	if len(r.entries) >= maxEntries {
		now := r.now()
		for k, v := range r.entries {
			if !now.Before(v.expires) {
				delete(r.entries, k)
			}
		}
		if len(r.entries) >= maxEntries {
			clear(r.entries)
		}
	}
	r.entries[key] = e
}
