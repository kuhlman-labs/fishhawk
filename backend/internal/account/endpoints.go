package account

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// InstallationGetter is the single query surface EndpointResolver needs.
// *accountdb.Queries (constructed via accountdb.New(pool)) satisfies it;
// tests inject a fake.
type InstallationGetter interface {
	GetInstallationByRef(ctx context.Context, arg accountdb.GetInstallationByRefParams) (accountdb.Installation, error)
}

var _ InstallationGetter = (*accountdb.Queries)(nil)

// EndpointResolver resolves the per-installation forge / OAuth base URLs
// recorded on the installations row (ADR-057 Amendment A1). It is the
// forge-agnostic reader the E44.2 (#1826) endpoint routing threads into the
// GitHub App installation-token mint (Mode 2, data-resident installs on
// <slug>.ghe.com). The columns were relocated onto installations by
// migration 0055 (#1825); this is the first production reader.
type EndpointResolver struct {
	q InstallationGetter
}

// NewEndpointResolver wraps an installation getter (accountdb.New(pool)) into
// an EndpointResolver. A nil getter is tolerated: ResolveInstallationEndpoints
// then reports the deployment default (empty, empty, nil) — the no-database
// posture degrades to the per-deployment FISHHAWKD_* endpoint config.
func NewEndpointResolver(q InstallationGetter) *EndpointResolver {
	return &EndpointResolver{q: q}
}

// ResolveInstallationEndpoints looks up the installation identified by
// (provider, installationRef) and returns its recorded per-installation
// endpoints:
//
//   - both columns SET → (forgeBaseURL, oauthBaseURL, nil): the data-resident
//     override the caller routes its per-installation client to.
//   - a NULL column → that return is the empty string: the intentional absence
//     of an override, so the caller keeps its deployment default. A NULL
//     forge_base_url with a SET oauth_base_url (or vice-versa) is honored
//     independently.
//   - not-found row (pgx.ErrNoRows) → ("", "", nil): an unknown installation
//     is likewise the deployment default, never an error.
//   - a REAL DB error → ("", "", err): propagated so the caller FAILS CLOSED.
//     An endpoint-resolution fault must never silently fall back to the
//     default host for a data-resident install (a NULL/not-found is an
//     intentional absence; a query fault is not).
//
// A nil resolver / nil getter reports the deployment default without a query.
func (r *EndpointResolver) ResolveInstallationEndpoints(ctx context.Context, provider, installationRef string) (forgeBaseURL, oauthBaseURL string, err error) {
	if r == nil || r.q == nil {
		return "", "", nil
	}
	inst, err := r.q.GetInstallationByRef(ctx, accountdb.GetInstallationByRefParams{
		Provider:        provider,
		InstallationRef: installationRef,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown installation — intentional absence, deployment default.
			return "", "", nil
		}
		// Real DB fault: fail closed, surface the error to the caller.
		return "", "", err
	}
	if inst.ForgeBaseUrl != nil {
		forgeBaseURL = *inst.ForgeBaseUrl
	}
	if inst.OauthBaseUrl != nil {
		oauthBaseURL = *inst.OauthBaseUrl
	}
	return forgeBaseURL, oauthBaseURL, nil
}
