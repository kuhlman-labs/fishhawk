package server

import (
	"context"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// patchComparer is the narrow diff-source seam the four implement-review sites
// in trace.go read a run's review diff through: ComparePatch(base, head). Both
// forge shapes satisfy it — *githubclient.Client (whose RepoRef /
// ComparePatchResult are aliases of the forge types) and any forge.Forge — so
// forgeCompareFor can hand back either behind one interface.
type patchComparer interface {
	ComparePatch(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, base, head string) (*forge.ComparePatchResult, error)
}

// Both diff sources satisfy patchComparer. The assertions keep a signature drift
// on either side a compile error rather than a runtime miss.
var (
	_ patchComparer = (*githubclient.Client)(nil)
	_ patchComparer = (forge.Forge)(nil)
)

// forgeCompareFor resolves the diff-source patchComparer for a run's forge
// FAMILY, plus the credential scope and repo ref to call it with (E45.47 /
// #3464). It is the shared resolution the four trace.go implement-review sites —
// the consolidated review, the fix-up re-review backstop, the fix-up delta
// framing, and the cumulative evaluation — route onto so a GitLab run reaches
// the review diff the GitHub-only guards previously refused it.
//
// reason is non-empty exactly when no comparer is available; the caller then
// degrades per its own contract (skip the dispatch, or fall back to the full
// bundle diff). The ladder mirrors prStateReaderFor:
//
//   - a github-family run resolves ONLY through cfg.GitHub. The explicit nil
//     check on the CONCRETE *githubclient.Client is load-bearing: a nil pointer
//     assigned into the patchComparer interface is a NON-nil interface holding a
//     nil pointer, so the check must run before the assignment. A github-family
//     run NEVER reaches the resolver, so registry availability can never change
//     a GitHub outcome.
//   - any other family resolves through cfg.ForgeResolver (defaulting to
//     forge.Get), a resolver error or an isNilForge result yielding an
//     "unresolved" reason. The repo ref is split on the LAST slash
//     (splitParentRepoRef) so a nested GitLab group path resolves; the GitLab
//     adapter ignores RepoRef and scopes by the credential ref, so the ref value
//     is advisory there.
func (s *Server) forgeCompareFor(runRow *run.Run) (c patchComparer, scope forge.CredentialScope, repo forge.RepoRef, reason string) {
	family := observationForgeID(runRow.InstallationRef)
	if family == observationForgeGitHub {
		if s.cfg.GitHub == nil {
			return nil, forge.CredentialScope{}, forge.RepoRef{}, "github client not wired"
		}
		if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
			return nil, forge.CredentialScope{}, forge.RepoRef{}, "no installation id"
		}
		r, err := parseRepoOwnerName(runRow.Repo)
		if err != nil {
			return nil, forge.CredentialScope{}, forge.RepoRef{}, "parse repo: " + err.Error()
		}
		return s.cfg.GitHub, forge.FromGitHubInstallationID(*runRow.InstallationID), r, ""
	}

	if runRow.InstallationRef == nil || *runRow.InstallationRef == "" {
		return nil, forge.CredentialScope{}, forge.RepoRef{}, "no installation_ref"
	}
	ref := *runRow.InstallationRef
	resolver := s.cfg.ForgeResolver
	if resolver == nil {
		resolver = forge.Get
	}
	f, err := resolver(family)
	if err != nil || isNilForge(f) {
		return nil, forge.CredentialScope{}, forge.RepoRef{}, "forge " + family + " unresolved"
	}
	r, ok := splitParentRepoRef(runRow.Repo)
	if !ok {
		return nil, forge.CredentialScope{}, forge.RepoRef{}, "parse repo"
	}
	return f, forge.FromRef(ref), r, ""
}
