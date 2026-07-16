// Package forge holds the forge-neutral credential-scope seam
// (ADR-058, #2009): a CredentialScope that names "which installation
// to authenticate as" without committing to a specific forge (GitHub
// today; GitLab per ADR-058), plus the CredentialProvider interface
// that resolves a scope to a bearer token. See README.md for the
// full contract.
package forge

import (
	"context"
	"fmt"
	"strconv"
)

// CredentialScope is an opaque credential-scope key. Its canonical
// form is the installation_ref TEXT column (ADR-057/ADR-058): today
// that's the stringified GitHub App installation id, but the type
// itself carries no forge assumption — construction never validates
// which forge a ref belongs to. Validation happens only at the point
// of use, e.g. GitHubInstallationID's fail-closed parse. The zero
// value (empty ref) is the unresolved-installation sentinel; see
// IsZero.
type CredentialScope struct {
	ref string
}

// FromRef wraps an arbitrary installation_ref verbatim. This is the
// forge-neutral constructor: it performs no validation, so it never
// pre-judges which forge the ref belongs to. Non-GitHub credential
// implementations (and cross-package tests exercising non-GitHub
// refs) construct scopes through this constructor.
func FromRef(ref string) CredentialScope {
	return CredentialScope{ref: ref}
}

// FromGitHubInstallationID is the GitHub convenience wrapper around
// FromRef: the canonical GitHub ref is the id's base-10 decimal
// string. id 0 is the codebase's unresolved-installation sentinel
// and maps to the zero scope (empty ref), preserving IsZero after
// the type change from a bare int64.
func FromGitHubInstallationID(id int64) CredentialScope {
	if id == 0 {
		return CredentialScope{}
	}
	return FromRef(strconv.FormatInt(id, 10))
}

// Ref returns the canonical installation_ref string.
func (s CredentialScope) Ref() string { return s.ref }

// IsZero reports whether s is the empty-ref zero scope — the
// unresolved-installation sentinel (id 0 in the pre-scope int64
// world).
func (s CredentialScope) IsZero() bool { return s.ref == "" }

// GitHubInstallationID parses the scope's ref as a GitHub App
// installation id. It fails closed — never a silent 0 with nil
// error — on the empty/zero scope or a non-numeric ref, naming the
// offending ref in the error.
func (s CredentialScope) GitHubInstallationID() (int64, error) {
	if s.ref == "" {
		return 0, fmt.Errorf("forge: empty credential scope ref")
	}
	id, err := strconv.ParseInt(s.ref, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("forge: credential scope ref %q is not a valid github installation id: %w", s.ref, err)
	}
	return id, nil
}

// CredentialProvider is the forge-neutral analogue of
// githubapp.TokenProvider: it resolves a CredentialScope to a
// bearer token, ready to set as `Authorization: Bearer <token>`.
type CredentialProvider interface {
	Token(ctx context.Context, scope CredentialScope) (string, error)
}
