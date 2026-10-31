package server

import (
	"context"

	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// resolveRunDefaultBranch resolves runRow's base branch from the forge
// default_branch through s.cfg.DefaultBranch (#4660). It never guesses a
// branch: every failure is a typed *defaultbranch.UnresolvedError whose
// reason (defaultbranch.ReasonOf) the caller surfaces on its own existing
// failure surface.
//
//   - an unparseable owner/name → repository_invalid;
//   - a GitLab run → forge_unsupported (this path's consumers are
//     GitHub-only, ADR-110 3(c));
//   - otherwise scope from the run's installation id (nil → the zero scope →
//     no_installation) and the shared resolver decides; a nil resolver →
//     resolver_unconfigured.
func (s *Server) resolveRunDefaultBranch(ctx context.Context, runRow *run.Run) (string, error) {
	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		return "", defaultbranch.Unresolved(forge.RepoRef{}, defaultbranch.ReasonRepositoryInvalid, err)
	}
	if isGitLabRun(runRow) {
		return "", defaultbranch.Unresolved(repo, defaultbranch.ReasonForgeUnsupported, nil)
	}
	var scope forge.CredentialScope
	if runRow.InstallationID != nil {
		scope = forge.FromGitHubInstallationID(*runRow.InstallationID)
	}
	return defaultbranch.Resolve(ctx, s.cfg.DefaultBranch, scope, repo)
}
