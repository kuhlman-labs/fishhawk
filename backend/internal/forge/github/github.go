// Package github is the GitHub implementation of forge.Forge (ADR-058 /
// E45.4). It is a thin adapter over the concrete githubclient.Client:
// the client already exposes every Forge method in the exact
// scope-first shape the interface declares, so the adapter EMBEDS
// *githubclient.Client to promote those methods verbatim and adds only
// the two the interface needs that the client doesn't spell — Name()
// and ResolveRepoScope. This mirrors workmgmt/github, which adapts the
// same client onto the work-management Provider the same way.
//
// The adapter deliberately holds no logic of its own beyond the scope
// conversion in ResolveRepoScope: the point of the refactor is a seam,
// not a behavior change, so every real GitHub call stays in
// githubclient where it already is and stays tested where it already is.
package github

import (
	"context"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// forgeName is the registry id this adapter registers under and Name
// returns. It is the value serve.go passes to forge.Get and the value
// stored on run rows as the forge discriminator.
const forgeName = "github"

// Forge adapts *githubclient.Client onto forge.Forge. The embedded
// client promotes CreateRef, MergeBranch, CreatePullRequest,
// CreateCheckRun, ComparePatch, and the rest of the covered surface
// directly — their signatures already take a forge.CredentialScope and
// forge.RepoRef (the moved vocabulary), so no wrapping is needed. Only
// Name and ResolveRepoScope are declared here.
type Forge struct {
	*githubclient.Client
}

// Compile-time assertion that the embedded client plus the two methods
// below satisfy the full Forge surface. If a Forge method is ever added
// that the client does not promote, this line fails the build.
var _ forge.Forge = (*Forge)(nil)

// Compile-time assertion that the adapter also provides the standalone
// file-read capability the per-repo conventions loader consumes (#2022).
var _ forge.FileFetcher = (*Forge)(nil)

// New wraps c as the registered "github" forge. c is the same concrete
// client serve.go wires for the non-forge surfaces (issues, comments,
// projects, releases, contents, workflow dispatch); the adapter shares
// it rather than owning a second one.
func New(c *githubclient.Client) *Forge {
	return &Forge{Client: c}
}

// Name returns the forge id ("github").
func (*Forge) Name() string { return forgeName }

// ResolveRepoScope resolves the credential scope Fishhawk should act as
// for repo by looking up the App installation on it (App-JWT auth, no
// scope argument — the call authenticates as the app itself) and
// converting the installation id to a forge.CredentialScope.
//
// It propagates githubclient's errors unmodified: notably
// forge.ErrNotInstalled when the App is not installed on repo, so a
// caller distinguishes "not installed" from a generic not-found. The id
// is passed straight into forge.FromGitHubInstallationID without a local
// int64 binding, keeping the #1855 credential-scope gate green.
func (f *Forge) ResolveRepoScope(ctx context.Context, repo forge.RepoRef) (forge.CredentialScope, error) {
	id, err := f.GetRepoInstallation(ctx, repo)
	if err != nil {
		return forge.CredentialScope{}, err
	}
	return forge.FromGitHubInstallationID(id), nil
}

// FetchFile implements forge.FileFetcher by delegating to the embedded
// client's GetFile — which already issues
// GET /repos/{owner}/{repo}/contents/{path}?ref={ref}, base64-decodes the
// content, and returns forge.ErrNotFound / ErrForbidden — and mapping its
// *githubclient.FileContent onto the forge-neutral *forge.FileContent.
// Errors propagate unmodified.
func (f *Forge) FetchFile(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, path, ref string) (*forge.FileContent, error) {
	fc, err := f.GetFile(ctx, scope, repo, path, ref)
	if err != nil {
		return nil, err
	}
	return &forge.FileContent{Path: fc.Path, Content: fc.Content, SHA: fc.SHA}, nil
}
