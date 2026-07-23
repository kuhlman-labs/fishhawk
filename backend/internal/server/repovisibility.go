package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
)

// RepoVisibility is the handler-facing seam over the per-identity forge
// repo-permission mirror (ADR-057 Amendment A2, E44.10 / #2071).
// *repoacl.Mirror satisfies it; the server package deliberately does NOT
// import repoacl, exactly as it does not import account for AccountRoles.
//
// The two-failure-class contract is the mirror's, and this seam preserves it
// verbatim because the classification is encoded in the RETURN SHAPE:
//
//   - Visible returns (false, nil) when the FORGE could not be asked (a
//     transport fault, a 5xx, or a rate limit). The permission is unknown, so
//     the repo is not visible for THIS request, nothing is memoized, and the
//     request otherwise proceeds — a list page omits that row, a point read
//     403s repo_forbidden. The mirror logs it at WARN naming the repo and the
//     reason, so an operator can tell a short page caused by "you lack access"
//     from one caused by "we could not ask the forge".
//   - Visible returns a non-nil error when the mirror STORE could not be read
//     or written. The filter cannot function, so the request fails 503
//     service_unavailable. Never a silent allow, never a silent deny.
//
// The two are never collapsed at this layer either: repoFilter.allows passes
// the error straight through and every caller maps a non-nil error to 503.
type RepoVisibility interface {
	Visible(ctx context.Context, provider, subject, repo string) (bool, error)
	InvalidateSubject(ctx context.Context, provider, subject string) error
}

// repoFilter is the resolved per-request repo-visibility decision procedure.
//
// A nil *repoFilter means "filtering does not apply to this request" — every
// method is nil-safe and allows, so a handler holds one value and never has to
// branch on whether filtering is on.
type repoFilter struct {
	vis RepoVisibility
	// providers resolves a repo's forge for the cross-forge deny. A NIL
	// resolver means the cross-forge check is not wired at all, so the mirror
	// decides. A wired resolver answering found=false means the row's forge is
	// AMBIGUOUS, which fails closed — see allows.
	providers ProviderResolver
	// denyAll makes the filter deny every repo without asking anything. It is
	// the fail-closed posture for an identity that cannot be keyed into the
	// mirror at all (a cookie subject with no "<provider>:" prefix).
	denyAll bool
	// logger, when non-nil, receives the WARN signals that explain a short
	// page: an ambiguous row forge, or a mirror-unkeyable subject.
	logger *slog.Logger
	// provider is the caller's forge, from their identity subject prefix.
	provider string
	// subject is the forge-neutral member ref (the "<provider>:" prefix
	// stripped generically), the same derivation account.Store.MemberRole
	// performs, so a mirror row and a membership grant key on one string.
	subject string
	// decided memoizes per-repo answers for the life of ONE request, so a
	// list page never asks the mirror about the same repo twice.
	decided map[string]bool
}

// repoFilterFor resolves ONCE per request whether repo filtering applies, and
// to whom. It returns (nil, nil) for every posture in which the pre-#2071
// visibility surface is preserved exactly:
//
//   - Config.RepoVisibility is nil — no mirror wired (untenanted-allow).
//   - The caller is anonymous — there is no identity to filter against;
//     anonymous access is governed by the per-handler auth checks as before.
//   - The caller is a bearer / MCP token (TokenID != "") — deliberately
//     UNFILTERED. Those identities are bounded by token ownership and the
//     account middleware; a repo-permission mirror keyed on a human forge
//     subject has nothing to say about them, and filtering them would break
//     the CLI and the runner's own MCP token.
//   - The caller resolves to the workspace admin role — the admin bypass,
//     resolved through the SAME Config.AccountRoles seam #1829 already uses.
//
// A cookie subject with no "<provider>:" prefix is NOT one of those postures:
// it returns a deny-all filter, because "unfiltered" is the one default that
// must never be reached by accident.
//
// A role-resolution error is NOT a bypass and NOT a deny: it surfaces so the
// caller fails the request 503, per the store-fault rule.
func (s *Server) repoFilterFor(ctx context.Context) (*repoFilter, error) {
	if s.cfg.RepoVisibility == nil {
		return nil, nil
	}
	id := IdentityFrom(ctx)
	if id.IsAnonymous() || id.TokenID != "" {
		return nil, nil
	}
	provider := providerFromSubject(id.Subject)
	if provider == "" || provider == id.Subject {
		// A subject with no "<provider>:" prefix cannot be keyed into the
		// mirror at all, so no repo can be shown to be visible to it. Fail
		// CLOSED — deny every repo — rather than fall through unfiltered: the
		// only subjects reaching here today are cookie sessions, which always
		// carry the prefix, so this branch is unreachable in practice, and an
		// unreachable branch must not be the one path that silently bypasses
		// repo filtering if a future auth path ever mints a prefixless
		// subject. The WARN names the subject so the resulting empty page is
		// diagnosable rather than mysterious.
		if s.cfg.Logger != nil {
			s.cfg.Logger.WarnContext(ctx,
				"repo visibility: subject carries no provider prefix; denying all repos (fail closed)",
				"subject", id.Subject)
		}
		return &repoFilter{denyAll: true, logger: s.cfg.Logger}, nil
	}
	if s.cfg.AccountRoles != nil && id.AccountID != "" {
		role, err := s.cfg.AccountRoles.MemberRole(ctx, id.AccountID, provider, id.Subject)
		if err != nil {
			return nil, err
		}
		if role == account.RoleAdmin {
			// Admin bypass: a workspace admin sees all of the workspace's
			// data, including rows for repos they hold no forge grant on.
			return nil, nil
		}
	}
	return &repoFilter{
		vis:       s.cfg.RepoVisibility,
		providers: s.cfg.RepoProviders,
		provider:  provider,
		subject:   strings.TrimPrefix(id.Subject, provider+":"),
		logger:    s.cfg.Logger,
		decided:   make(map[string]bool, 8),
	}, nil
}

// allows reports whether repo is visible to the filtered caller.
//
// Order matters: the CROSS-FORGE deny is evaluated first and short-circuits
// with ZERO forge calls. A row whose forge differs from the caller's login
// forge is never visible — a GitHub-only login sees no GitLab-installation
// data — and asking GitHub about a GitLab repo would be both wrong and a
// wasted rate-limit unit.
//
// The row's forge is resolved through the ProviderResolver seam (accounts.
// provider keyed by the repo owner), and the resolution outcome maps to three
// distinct behaviours — the found=false case FAILS CLOSED (fix-up, E44.10):
//
//   - No resolver wired (f.providers == nil): the cross-forge check is not
//     configured at all, so it draws no conclusion and the mirror decides.
//     In production the resolver and the mirror are wired together (both gated
//     on pool != nil in serve.go), so this is a test/degraded posture only.
//   - A wired resolver answering found=false (the repo owner is unregistered,
//     or — per account.Resolver's contract — registered under BOTH forges):
//     the row's forge is AMBIGUOUS. Deny, with ZERO forge calls. Falling
//     through to the mirror here would ask the CALLER'S forge about the row's
//     repo, so a GitLab-installation row "acme/app" could be made visible to a
//     GitHub-only login that happens to hold read on a same-named GitHub repo
//     — both a leak and exactly the forge lookup the [cross-forge-default-deny]
//     criterion forbids. Logged at WARN so the resulting short page is
//     attributable to an unregistered/dual-registered owner, which is an
//     operator-fixable account-registration state, not a permission answer.
//   - A wired resolver answering a forge different from the caller's: deny,
//     with ZERO forge calls (the criterion's main case).
//
// A resolver ERROR is a store fault and surfaces for the 503.
func (f *repoFilter) allows(ctx context.Context, repo string) (bool, error) {
	if f == nil {
		return true, nil
	}
	if f.denyAll {
		return false, nil
	}
	if v, ok := f.decided[repo]; ok {
		return v, nil
	}
	if f.providers != nil {
		rowProvider, found, err := f.providers.ResolveProvider(ctx, repo)
		if err != nil {
			return false, err
		}
		if !found {
			if f.logger != nil {
				f.logger.WarnContext(ctx,
					"repo visibility: row forge is ambiguous (owner unregistered or registered under both forges); denying (fail closed)",
					"repo", repo, "caller_provider", f.provider)
			}
			f.decided[repo] = false
			return false, nil
		}
		if rowProvider != f.provider {
			f.decided[repo] = false
			return false, nil
		}
	}
	v, err := f.vis.Visible(ctx, f.provider, f.subject, repo)
	if err != nil {
		// Store fault: do NOT memoize — the next request may well succeed.
		return false, err
	}
	f.decided[repo] = v
	return v, nil
}

// requestRepoFilter is the handler-side wrapper around repoFilterFor: it
// resolves the filter and, on a resolution failure, writes the 503 envelope
// and reports ok=false so the handler returns immediately.
func (s *Server) requestRepoFilter(w http.ResponseWriter, r *http.Request) (*repoFilter, bool) {
	f, err := s.repoFilterFor(r.Context())
	if err != nil {
		s.writeRepoFilterUnavailable(w, r)
		return nil, false
	}
	return f, true
}

// isReadRequest reports whether r is a read. It is how a surface whose loader
// is SHARED between reads and writes (refinement) keeps repo visibility on the
// read side only: the mirror is a non-authoritative TTL'd cache of a forge
// READ permission and #2071 scopes it to read visibility, so it must never
// decide whether a mutation or an approval may proceed. Where a surface
// already carries an explicit tier (enforceAccount), that tier is used instead.
func isReadRequest(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

// enforceRepoVisibility is the POINT-READ counterpart to list filtering: a
// non-visible repo answers 403 repo_forbidden rather than being silently
// dropped, matching the #1829 convention that lists FILTER while point reads
// DENY. Returns true when the request may proceed.
func (s *Server) enforceRepoVisibility(w http.ResponseWriter, r *http.Request, repo string) bool {
	f, ok := s.requestRepoFilter(w, r)
	if !ok {
		return false
	}
	return s.repoVisibleOr403(w, r, f, repo)
}

// repoVisibleOr403 applies an ALREADY-RESOLVED filter to one repo, writing the
// 403/503 envelope on denial. Split out from enforceRepoVisibility so a
// caller that already holds a filter (the account middleware, which resolves
// once for the whole request) does not re-resolve it.
func (s *Server) repoVisibleOr403(w http.ResponseWriter, r *http.Request, f *repoFilter, repo string) bool {
	ok, err := f.allows(r.Context(), repo)
	if err != nil {
		s.writeRepoFilterUnavailable(w, r)
		return false
	}
	if !ok {
		s.writeError(w, r, http.StatusForbidden, "repo_forbidden",
			"you do not have read access to this repository on the forge", nil)
		return false
	}
	return true
}

// writeRepoFilterUnavailable is the single 503 envelope for a filter that
// cannot function (a mirror-store fault, a provider-resolution fault, or a
// role-resolution fault). Centralized so no caller can accidentally turn a
// filter fault into a 200 with a short page.
func (s *Server) writeRepoFilterUnavailable(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, r, http.StatusServiceUnavailable, "service_unavailable",
		"could not resolve repository visibility; retry shortly", nil)
}
