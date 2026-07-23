// Package repoacl is the per-identity forge repo-permission MIRROR behind
// repo-scoped in-workspace visibility (ADR-057 Amendment A2, E44.10 / #2071).
//
// It caches, per (provider, subject, repo), the forge-neutral
// identity.Permission tier identity.IdentityProvider.PermissionLevel resolved,
// stamped with a checked_at the reader TTLs against. The forge stays
// AUTHORITATIVE: a miss or an expired row re-resolves live, and a login purges
// the subject's rows so a fresh sign-in never reads a pre-login answer.
//
// # The two failure classes (binding, never collapsed)
//
// The single rule this package exists to keep honest — see README.md for the
// long form:
//
//   - A FORGE error (including identity.ErrRateLimited) means the permission
//     is UNKNOWN. That repo is NOT VISIBLE for this request, NOTHING is
//     written to the mirror, and the request otherwise proceeds. Visible
//     returns (false, nil) and logs at WARN, so a list page silently
//     shortened by a forge fault is still distinguishable in the logs from
//     one shortened because the caller genuinely lacks access.
//
//   - A STORE error means the filter itself cannot function. Visible returns
//     a non-nil error wrapping ErrStoreUnavailable and the caller fails the
//     request 503.
//
// The classification lives HERE, in Visible's return shape, rather than at
// each caller: a handler holding (bool, error) cannot accidentally turn a
// forge fault into a 503 or a DB outage into a silent short page.
package repoacl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
)

// DefaultTTL is the shipped freshness window for a mirrored entry. It bounds
// STALE-ALLOW: a permission revoked on the forge stays visible for at most
// this long. Short enough that a revocation lands within a coffee break; long
// enough that a wide list page does not fan out one forge call per repo on
// every request.
const DefaultTTL = 15 * time.Minute

var (
	// ErrNotConfigured means no mirror is wired — a nil *Mirror, or one with
	// no store or no identity provider. Callers translate it into the
	// untenanted-allow posture (filtering disabled), which is exactly the
	// pre-#2071 visibility surface. It is deliberately NOT a deny: a
	// deployment with no database must keep working.
	ErrNotConfigured = errors.New("repoacl: mirror not configured")

	// ErrStoreUnavailable marks a failure of the MIRROR STORE itself (a DB
	// read or write error). The filter cannot function, so the caller fails
	// the request closed with 503 rather than guessing. Never returned for a
	// forge fault.
	ErrStoreUnavailable = errors.New("repoacl: mirror store unavailable")

	// ErrForgeUnavailable marks a failure to ASK the forge — a transport
	// error, a 5xx, or identity.ErrRateLimited. The permission is unknown,
	// nothing is memoized, and the repo is not visible for this request.
	// Visible absorbs this into (false, nil); Permission surfaces it for
	// callers that need the distinction.
	ErrForgeUnavailable = errors.New("repoacl: forge permission unavailable")
)

// Store is the persistence surface Mirror needs. postgres.go's
// NewPostgresStore satisfies it against repoacldb; tests inject a fake.
//
// Get reports found=false for a miss (the underlying pgx.ErrNoRows is
// translated there, not here) so a miss is never confused with an error.
type Store interface {
	Get(ctx context.Context, provider, subject, repo string) (Entry, bool, error)
	Upsert(ctx context.Context, provider, subject, repo string, perm identity.Permission) error
	DeleteForSubject(ctx context.Context, provider, subject string) error
}

// Entry is one mirrored permission fact.
type Entry struct {
	Permission identity.Permission
	CheckedAt  time.Time
}

// PermissionResolver is the slice of identity.IdentityProvider the mirror
// consumes. Narrowing it to the one method keeps the test fake honest and
// documents that the mirror never drives a device flow or a membership read.
// *identity.GitHubIdentityProvider (and any IdentityProvider) satisfies it.
type PermissionResolver interface {
	PermissionLevel(ctx context.Context, repo, subject string) (identity.Permission, error)
}

// Mirror resolves a subject's permission on a repo, memoized with a TTL.
//
// The zero value is not usable; construct with NewMirror. A nil *Mirror is
// tolerated on every method and reports ErrNotConfigured, so a server holding
// an unwired seam degrades to untenanted-allow rather than panicking.
type Mirror struct {
	store    Store
	resolver PermissionResolver
	ttl      time.Duration
	logger   *slog.Logger
}

// NewMirror wires a store and a permission resolver. A non-positive ttl falls
// back to DefaultTTL — a zero TTL would re-resolve on every repo of every list
// page, which is a forge rate-limit incident, not a safe default. A nil logger
// falls back to slog.Default() so the mandated WARN on a forge fault is never
// swallowed by an unwired logger.
func NewMirror(store Store, resolver PermissionResolver, ttl time.Duration, logger *slog.Logger) *Mirror {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Mirror{store: store, resolver: resolver, ttl: ttl, logger: logger}
}

// TTL reports the freshness window this mirror was constructed with. Exported
// so the fishhawkd wiring test can assert the SHIPPED default rather than
// re-deriving it.
func (m *Mirror) TTL() time.Duration {
	if m == nil {
		return 0
	}
	return m.ttl
}

// configured reports whether the mirror can function at all.
func (m *Mirror) configured() bool {
	return m != nil && m.store != nil && m.resolver != nil
}

// Permission returns the subject's tier on repo, from the mirror when a
// non-expired entry exists and from the forge otherwise.
//
// subject is the forge-neutral member ref (no "<provider>:" prefix); use
// SubjectRef to derive it from an identity subject.
//
// Error classes, per the package contract:
//   - ErrNotConfigured  — no store or no resolver.
//   - ErrStoreUnavailable — the mirror read or write failed.
//   - ErrForgeUnavailable — the forge could not be asked. Nothing is memoized.
//
// A resolved answer is ALWAYS memoized, including identity.PermissionNone: a
// legitimate deny is worth caching, and caching it is what keeps a
// no-access caller from re-asking the forge on every page.
func (m *Mirror) Permission(ctx context.Context, provider, subject, repo string) (identity.Permission, error) {
	if !m.configured() {
		return identity.PermissionNone, ErrNotConfigured
	}
	if provider == "" || subject == "" || repo == "" {
		// Defensive: an unkeyable lookup can never be a grant.
		return identity.PermissionNone, fmt.Errorf("repoacl: provider, subject and repo are required")
	}

	entry, found, err := m.store.Get(ctx, provider, subject, repo)
	if err != nil {
		return identity.PermissionNone, fmt.Errorf("%w: get %s/%s: %w", ErrStoreUnavailable, provider, repo, err)
	}
	if found && !m.expired(entry.CheckedAt) {
		return entry.Permission, nil
	}

	// MISS or EXPIRED — the stale value is never served. Ask the forge.
	perm, err := m.resolver.PermissionLevel(ctx, repo, subject)
	if err != nil {
		// No upsert: a transient fault must never be memoized, as a phantom
		// deny or (worse) as a refreshed checked_at on a stale grant.
		return identity.PermissionNone, fmt.Errorf("%w: %s: %w", ErrForgeUnavailable, repo, err)
	}
	if err := m.store.Upsert(ctx, provider, subject, repo, perm); err != nil {
		return identity.PermissionNone, fmt.Errorf("%w: upsert %s/%s: %w", ErrStoreUnavailable, provider, repo, err)
	}
	return perm, nil
}

// Visible is the read-path predicate: does subject hold at least `read` on
// repo? It is where the two failure classes are separated once and for all.
//
//   - Forge fault  → (false, nil) plus a WARN naming the repo and the reason,
//     so an operator can tell "you lack access" from "we could not ask the
//     forge". The caller omits the row / 403s that repo and proceeds.
//   - Store fault  → (false, err). The caller fails the request 503.
//   - Unwired      → (false, ErrNotConfigured). The caller allows
//     (untenanted-allow); it is an error, not a silent false, so an
//     accidentally-unwired mirror cannot masquerade as a deny-all.
//
// Tier ranking is identity.Permission.AtLeast, which fails closed on an
// unrecognized tier — a garbage string mirrored by a future forge denies
// rather than being ranked as write.
func (m *Mirror) Visible(ctx context.Context, provider, subject, repo string) (bool, error) {
	perm, err := m.Permission(ctx, provider, subject, repo)
	switch {
	case err == nil:
		return perm.AtLeast(identity.PermissionRead), nil
	case errors.Is(err, ErrForgeUnavailable):
		m.logger.WarnContext(ctx, "repo visibility unresolved: forge permission lookup failed; treating repo as not visible",
			slog.String("provider", provider),
			slog.String("repo", repo),
			slog.Bool("rate_limited", errors.Is(err, identity.ErrRateLimited)),
			slog.String("reason", err.Error()),
		)
		return false, nil
	default:
		// Store fault, ErrNotConfigured, or a defensive argument error — all
		// surface so the caller decides (503 / untenanted-allow) explicitly.
		return false, err
	}
}

// InvalidateSubject purges every mirrored entry for one identity. Called at
// login so a fresh sign-in re-resolves from the forge instead of inheriting
// pre-login answers.
//
// Callers treat a failure as NON-FATAL to sign-in. See README.md: a failed
// purge leaves the caller at exactly the baseline TTL exposure the design
// already accepts everywhere else — surviving entries, grants included, expire
// within the TTL — so the exposure is bounded by the TTL, not unbounded.
// Failing sign-in closed on a transient DB blip would be the worse trade.
func (m *Mirror) InvalidateSubject(ctx context.Context, provider, subject string) error {
	if !m.configured() {
		return ErrNotConfigured
	}
	if provider == "" || subject == "" {
		return nil
	}
	if err := m.store.DeleteForSubject(ctx, provider, subject); err != nil {
		return fmt.Errorf("%w: purge %s: %w", ErrStoreUnavailable, provider, err)
	}
	return nil
}

// expired reports whether a mirrored entry has aged past the freshness window.
// A zero CheckedAt (an entry written by something that did not stamp it) is
// unboundedly old and therefore expired — fail closed toward re-resolving.
func (m *Mirror) expired(checkedAt time.Time) bool {
	return time.Since(checkedAt) >= m.ttl
}

// SubjectRef derives the forge-neutral member ref from an identity subject by
// stripping the "<provider>:" prefix GENERICALLY — never a hard-coded
// "github:" literal — so github:, gitlab:, and any future forge all resolve.
// This is deliberately the same derivation account.Store.MemberRole performs,
// so a mirror row and a membership grant key on the same string for the same
// human. A subject that lacks the prefix (unexpected) is used verbatim.
func SubjectRef(provider, subject string) string {
	if provider == "" {
		return subject
	}
	return strings.TrimPrefix(subject, provider+":")
}
