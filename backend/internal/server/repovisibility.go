package server

import (
	"context"
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
	// providers resolves a repo's forge for the cross-forge deny. Nil (or a
	// not-found answer) means the row's forge is unknown, in which case no
	// cross-forge conclusion is drawn and the mirror decides.
	providers ProviderResolver
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
		// mirror at all. Leave it unfiltered rather than deny-all: the only
		// subjects reaching here are cookie sessions, which always carry the
		// prefix, so this is a defensive fall-through, not a policy.
		return nil, nil
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
// provider keyed by the repo owner). found=false (no resolver wired, an
// unregistered owner, or an owner registered under BOTH forges) means the
// row's forge is UNKNOWN, so no cross-forge conclusion is drawn and the mirror
// decides — a resolver that cannot answer must not silently deny every row.
// A resolver ERROR is a store fault and surfaces for the 503.
func (f *repoFilter) allows(ctx context.Context, repo string) (bool, error) {
	if f == nil {
		return true, nil
	}
	if v, ok := f.decided[repo]; ok {
		return v, nil
	}
	if f.providers != nil {
		rowProvider, found, err := f.providers.ResolveProvider(ctx, repo)
		if err != nil {
			return false, err
		}
		if found && rowProvider != f.provider {
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
