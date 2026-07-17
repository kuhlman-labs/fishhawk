package githubclient

import (
	"context"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
)

// credentialTokens adapts a forge.CredentialProvider into a
// githubapp.TokenProvider by wrapping the int64 installation id into a
// forge.CredentialScope before delegating. It is what lets a Client
// constructed via NewWithCredentialProvider share the one token-fetch
// path with a Client built from a githubapp.TokenProvider: the Client
// resolves a scope to an id at its exported boundary, and this adapter
// turns that id back into a scope for the forge-neutral provider.
type credentialTokens struct {
	provider forge.CredentialProvider
}

func (c *credentialTokens) Token(ctx context.Context, installationID int64) (string, error) {
	return c.provider.Token(ctx, forge.FromGitHubInstallationID(installationID))
}

// NewWithCredentialProvider constructs a Client whose token source is a
// forge-neutral forge.CredentialProvider rather than a
// githubapp.TokenProvider. It builds via the unchanged New and wires
// Tokens to a credentialTokens adapter.
//
// A nil p is passed through to New as a nil githubapp.TokenProvider
// rather than wrapped: wrapping it would install a non-nil Tokens whose
// provider field is nil, bypassing New's "client missing TokenProvider"
// nil check and panicking on first use instead.
func NewWithCredentialProvider(p forge.CredentialProvider) *Client {
	if p == nil {
		return New(nil)
	}
	return New(&credentialTokens{provider: p})
}

// installationIDForScope resolves scope to a GitHub installation id.
// Every exported *Client method calls it exactly once on entry: it is
// THE forge boundary, above which the surface is scope-typed and below
// which the plumbing speaks GitHub's int64. It fails closed — rejecting
// a zero scope, and parsing the ref via scope.GitHubInstallationID()
// so a non-numeric ref errors with the offending ref named, before any
// request is issued.
func installationIDForScope(scope forge.CredentialScope) (int64, error) {
	if scope.IsZero() {
		return 0, fmt.Errorf("githubclient: credential scope is empty")
	}
	id, err := scope.GitHubInstallationID()
	if err != nil {
		return 0, fmt.Errorf("githubclient: %w", err)
	}
	return id, nil
}

// compile-time assertion that credentialTokens implements
// githubapp.TokenProvider.
var _ githubapp.TokenProvider = (*credentialTokens)(nil)
