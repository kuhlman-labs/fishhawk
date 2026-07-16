package githubapp

import (
	"context"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// ScopedProvider adapts an existing TokenProvider to the forge-neutral
// forge.CredentialProvider interface (ADR-058, #2009). It resolves the
// scope's GitHub installation id and delegates to the wrapped
// TokenProvider — the TokenProvider itself is unchanged.
type ScopedProvider struct {
	// Tokens is the wrapped TokenProvider. Required.
	Tokens TokenProvider
}

// NewScopedProvider returns a ScopedProvider wrapping tokens.
func NewScopedProvider(tokens TokenProvider) *ScopedProvider {
	return &ScopedProvider{Tokens: tokens}
}

// Token resolves scope to a GitHub installation id and delegates to the
// wrapped TokenProvider. It fails closed on a zero scope or an
// unparseable ref, naming the offending ref, and never invokes the
// delegate in that case.
func (p *ScopedProvider) Token(ctx context.Context, scope forge.CredentialScope) (string, error) {
	id, err := scope.GitHubInstallationID()
	if err != nil {
		return "", fmt.Errorf("githubapp: scoped provider: %w", err)
	}
	return p.Tokens.Token(ctx, id)
}

var _ forge.CredentialProvider = (*ScopedProvider)(nil)
