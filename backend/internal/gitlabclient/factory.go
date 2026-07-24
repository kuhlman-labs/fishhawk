package gitlabclient

import (
	"context"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
)

// Factory constructs per-installation *Client values, extending the single
// deployment-wide gitlabclient (Mode 1) into per-installation client
// construction (Mode 2) for the GitLab forge consumers — the ADR-057
// Amendment A1 endpoint routing, promoted forge-neutral in E44.16 (#2094)
// following the GitHub App / githubclient precedent (#1826).
//
// A Factory carries a deployment-default base URL, an optional injectable Doer
// (threaded onto every constructed Client so tests observe the request), and
// two additive, backward-compatible hooks:
//
//   - ResolveBaseURL, when non-nil, resolves the per-installation instance base
//     URL for an installation ref (the CredentialScope ref the forge adapter
//     derives). A non-empty resolved base is validated as a well-formed
//     absolute https URL (account.ValidateResolvedBaseURL) and, when
//     AllowedInstallationHosts is non-empty, pinned to the allowlist
//     (account.HostAllowed) BEFORE the PRIVATE-TOKEN ships. An http:// value
//     would transmit the token without TLS; a malformed or off-allowlist value
//     could send it to an unintended host — so each of a resolver error, a bad
//     scheme, and a disallowed host FAILS CLOSED: Client returns an error and
//     constructs no Client, so no request is ever issued.
//
//   - An EMPTY resolved base (nil resolver / NULL forge_base_url column /
//     unknown installation / empty installation ref) yields a Client on the
//     deployment-default base — byte-identical to Mode 1.
//
// The validation + allowlist contract is the ONE forge-neutral account
// contract shared by every per-installation forge consumer (the GitHub App
// mint, the githubclient REST client, this factory, and the identity
// provider), so the rules cannot drift.
type Factory struct {
	baseURL string
	doer    Doer

	// resolveBaseURL is the late-bound per-installation base-URL resolver; nil
	// leaves every construction on the deployment default.
	resolveBaseURL func(ctx context.Context, installationRef string) (string, error)

	// allowedInstallationHosts, when non-empty, pins the resolved
	// per-installation host to an operator-configured allowlist (see
	// account.HostAllowed). Empty = scheme/parse validation only.
	allowedInstallationHosts []string
}

// FactoryOption customises a Factory at construction.
type FactoryOption func(*Factory)

// WithFactoryHTTPClient injects the HTTP transport threaded onto every Client
// the Factory constructs. Without it constructed clients use
// http.DefaultClient. Tests pass a stub Doer here.
func WithFactoryHTTPClient(d Doer) FactoryOption {
	return func(f *Factory) { f.doer = d }
}

// WithFactoryResolveBaseURL sets the per-installation base-URL resolver. A nil
// fn is a no-op (deployment default everywhere).
func WithFactoryResolveBaseURL(fn func(ctx context.Context, installationRef string) (string, error)) FactoryOption {
	return func(f *Factory) { f.resolveBaseURL = fn }
}

// WithFactoryAllowedInstallationHosts sets the resolved-host allowlist. An
// empty slice leaves the resolved base subject to scheme/parse validation
// only.
func WithFactoryAllowedInstallationHosts(hosts []string) FactoryOption {
	return func(f *Factory) { f.allowedInstallationHosts = hosts }
}

// NewFactory returns a Factory whose deployment default is baseURL. baseURL
// covers GitLab.com (https://gitlab.com) and a self-managed host identically —
// gitlabclient.New normalises the trailing slash.
func NewFactory(baseURL string, opts ...FactoryOption) *Factory {
	f := &Factory{baseURL: baseURL}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Client resolves the per-installation base URL for installationRef and
// returns a *Client authed with token bound to that base.
//
// An EMPTY installationRef, or a nil resolver, skips resolution and returns a
// Client on the deployment-default base — the no-installation posture (e.g. a
// scope-producing lookup that has no installation context yet). A non-empty
// installationRef with a resolver configured resolves the base and, on a
// non-empty resolved value, validates it (absolute https, host present) and —
// when an allowlist is configured — gates it, FAILING CLOSED (nil Client,
// error, no request ever issued) on a resolver fault, a bad scheme, or a
// disallowed host. A resolved value of "" (NULL column / unknown installation)
// keeps the deployment default.
func (f *Factory) Client(ctx context.Context, installationRef, token string) (*Client, error) {
	base := f.baseURL
	if f.resolveBaseURL != nil && installationRef != "" {
		resolved, err := f.resolveBaseURL(ctx, installationRef)
		if err != nil {
			return nil, fmt.Errorf("gitlabclient: resolve installation base url: %w", err)
		}
		if resolved != "" {
			// Validate BEFORE the token ships: a value that is not a
			// well-formed https URL fails closed rather than transmitting the
			// PRIVATE-TOKEN to an unvalidated host.
			if err := account.ValidateResolvedBaseURL(resolved); err != nil {
				return nil, err
			}
			// Optional host allowlist pins the resolved host before the token
			// ships. Empty allowlist = scheme/parse validation only.
			if len(f.allowedInstallationHosts) > 0 && !account.HostAllowed(resolved, f.allowedInstallationHosts) {
				return nil, fmt.Errorf("gitlabclient: installation base url host not in configured allowlist: %q", resolved)
			}
			base = resolved
		}
	}
	if f.doer != nil {
		return New(base, token, WithHTTPClient(f.doer)), nil
	}
	return New(base, token), nil
}
