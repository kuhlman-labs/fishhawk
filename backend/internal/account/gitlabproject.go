package account

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// GitLabInstallationQueries is the query surface GitLabProjectResolver needs.
// *accountdb.Queries (accountdb.New(pool)) satisfies it; tests inject a fake.
type GitLabInstallationQueries interface {
	ListGitLabInstallationsByProjectPath(ctx context.Context, projectPath *string) ([]accountdb.Installation, error)
	GetInstallationByRef(ctx context.Context, arg accountdb.GetInstallationByRefParams) (accountdb.Installation, error)
}

var _ GitLabInstallationQueries = (*accountdb.Queries)(nil)

// GitLabInstallation is the slice of a registered gitlab installation row the
// run-creation forge seam consumes (E45.46 / #3463).
type GitLabInstallation struct {
	// InstallationRef is the ADR-057 / ADR-058 credential handle stamped
	// onto the run row (`gitlab:<project_id>`).
	InstallationRef string
	// ProjectPath is the registered path_with_namespace the row is bound to.
	ProjectPath string
	// ForgeBaseURL is the installation's registered GitLab instance root, or
	// "" when the column is NULL / empty — the caller falls back to the
	// deployment default (FISHHAWKD_GITLAB_BASE_URL) in that case.
	ForgeBaseURL string
}

// GitLabProjectResolver resolves the registered gitlab installation for a
// GitLab project path, so POST /v0/runs can stamp `installation_ref` on a
// gitlab run created by the MCP / CLI surfaces (E45.46 / #3463) instead of
// leaving the webhook receiver as the only entry point. It sits beside
// Resolver (the provider discriminator) and EndpointResolver (the per-run
// endpoint lookup) and reads the same installations table.
type GitLabProjectResolver struct {
	q GitLabInstallationQueries
}

// NewGitLabProjectResolver wraps the installation queries (accountdb.New(pool))
// into a GitLabProjectResolver. A nil query surface is tolerated: every lookup
// then reports not-found, mirroring NewResolver's no-database posture.
func NewGitLabProjectResolver(q GitLabInstallationQueries) *GitLabProjectResolver {
	return &GitLabProjectResolver{q: q}
}

// ResolveGitLabProject looks up the gitlab installation whose project_path
// EXACTLY (case-sensitively) equals projectPath. Exactly one row resolves
// (inst, true, nil); zero rows report not-found; MORE than one row is
// AMBIGUOUS and also reports not-found — never an arbitrary first row, the
// same posture Resolver.ResolveProvider takes on a doubly-registered
// account_key — so the caller refuses with the registration remedy instead of
// stamping a credential handle that may belong to a different binding. A
// query error is propagated so the caller fails closed on a transient DB
// fault rather than minting an unattributed run.
//
// projectPath is trimmed; an empty path is malformed and reports not-found
// without a query.
func (r *GitLabProjectResolver) ResolveGitLabProject(ctx context.Context, projectPath string) (GitLabInstallation, bool, error) {
	if r == nil || r.q == nil {
		return GitLabInstallation{}, false, nil
	}
	path := strings.TrimSpace(projectPath)
	if path == "" {
		return GitLabInstallation{}, false, nil
	}
	rows, err := r.q.ListGitLabInstallationsByProjectPath(ctx, &path)
	if err != nil {
		return GitLabInstallation{}, false, err
	}
	if len(rows) != 1 {
		return GitLabInstallation{}, false, nil
	}
	return gitlabInstallationFromRow(rows[0]), true, nil
}

// ResolveGitLabInstallationByRef reads back the gitlab installation registered
// under ref (`gitlab:<project_id>`), the reverse lookup the single-run read
// uses to surface a gitlab run's forge_base_url. pgx.ErrNoRows reports
// not-found; any other error is propagated.
func (r *GitLabProjectResolver) ResolveGitLabInstallationByRef(ctx context.Context, ref string) (GitLabInstallation, bool, error) {
	if r == nil || r.q == nil {
		return GitLabInstallation{}, false, nil
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return GitLabInstallation{}, false, nil
	}
	row, err := r.q.GetInstallationByRef(ctx, accountdb.GetInstallationByRefParams{
		Provider:        "gitlab",
		InstallationRef: ref,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GitLabInstallation{}, false, nil
		}
		return GitLabInstallation{}, false, err
	}
	return gitlabInstallationFromRow(row), true, nil
}

// gitlabInstallationFromRow projects the persisted row onto the consumer
// slice, collapsing a NULL or whitespace-only forge_base_url to "".
func gitlabInstallationFromRow(row accountdb.Installation) GitLabInstallation {
	out := GitLabInstallation{InstallationRef: row.InstallationRef}
	if row.ProjectPath != nil {
		out.ProjectPath = *row.ProjectPath
	}
	if row.ForgeBaseUrl != nil {
		out.ForgeBaseURL = strings.TrimSpace(*row.ForgeBaseUrl)
	}
	return out
}
