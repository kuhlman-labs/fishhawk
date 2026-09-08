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
	"fmt"

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

// Compile-time assertion that the adapter provides the standalone
// issue-thread capability the split-parent auto-close watcher consumes
// (E50.17 / #2900).
var _ forge.IssueOperations = (*Forge)(nil)

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

// --- forge.IssueOperations (E50.17 / #2900) -----------------------------
//
// Four thin wrappers over the embedded client's issue surface, each
// mapping the concrete result onto the forge-neutral vocabulary and
// propagating errors UNMODIFIED — the same shape FetchFile uses. They are
// named FetchIssue / FetchIssueComments / PostIssueComment / SetIssueState
// rather than GetIssue / ListIssueComments / CreateIssueComment /
// UpdateIssue so they do not SHADOW the promoted client methods of those
// names (an outer method wins over an embedded one, silently changing the
// type any caller of f.GetIssue receives).

// FetchIssue implements forge.IssueOperations by delegating to the
// embedded client's GetIssue (GET /repos/{owner}/{repo}/issues/{number}).
// GitHub's state vocabulary is already the forge-neutral one, so State
// and StateReason pass through verbatim.
func (f *Forge) FetchIssue(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int) (*forge.Issue, error) {
	is, err := f.GetIssue(ctx, scope, repo, number)
	if err != nil {
		return nil, err
	}
	return &forge.Issue{
		Number:      is.Number,
		Title:       is.Title,
		Body:        is.Body,
		State:       is.State,
		StateReason: is.StateReason,
		Labels:      is.Labels,
	}, nil
}

// FetchIssueComments implements forge.IssueOperations by delegating to the
// embedded client's ListIssueComments, which already pages to exhaustion
// via the rel="next" Link header.
func (f *Forge) FetchIssueComments(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int) ([]forge.IssueComment, error) {
	comments, err := f.ListIssueComments(ctx, scope, repo, number)
	if err != nil {
		return nil, err
	}
	out := make([]forge.IssueComment, 0, len(comments))
	for _, c := range comments {
		out = append(out, forge.IssueComment{ID: c.ID, Author: c.Author, Body: c.Body, CreatedAt: c.CreatedAt})
	}
	return out, nil
}

// PostIssueComment implements forge.IssueOperations by delegating to the
// embedded client's CreateIssueComment. The created comment's id is not
// surfaced: the capability's one consumer keys idempotency on a body
// marker, not on a recorded id.
func (f *Forge) PostIssueComment(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int, body string) error {
	_, err := f.CreateIssueComment(ctx, scope, repo, number, body)
	return err
}

// SetIssueState implements forge.IssueOperations by delegating to the
// embedded client's UpdateIssue with BOTH State and StateReason carried
// onto githubclient.UpdateIssueParams — a state-only PATCH would silently
// drop the completion reason. A nil State is refused locally with
// forge.ErrValidation before any HTTP call, per the interface contract.
func (f *Forge) SetIssueState(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int, u forge.IssueStateUpdate) error {
	if u.State == nil {
		return fmt.Errorf("%w: set issue state requires a target state", forge.ErrValidation)
	}
	_, err := f.UpdateIssue(ctx, scope, repo, number, githubclient.UpdateIssueParams{
		State:       u.State,
		StateReason: u.StateReason,
	})
	return err
}
