package forge

import "context"

// Forge is the forge-neutral surface Fishhawk's flows drive a code host
// through (ADR-058 / E45.4): ref creation + SHA reads, git-data commit
// authoring, the pull-request lifecycle, commit status, branch
// protection, diffs, and repo-scope resolution. GitHub is the only
// implementation today (forge/github); the interface exists so a second
// forge (GitLab per ADR-058) has a seam to land against instead of a
// tree full of *githubclient.Client fields.
//
// Every method takes a CredentialScope first, mirroring the concrete
// client's scope-first shape (#2009): the scope names WHICH installation
// to act as, and the implementation resolves it to a token. The one
// exception is ResolveRepoScope, which produces a scope and so cannot
// take one.
//
// PUSH CREDENTIALS ARE DELIBERATELY NOT A METHOD HERE. The
// forge-neutral push-credential seam is the existing CredentialProvider
// (#1855): it resolves a CredentialScope to a bearer token, which is
// exactly what a git push needs and what a runner already consumes.
// Duplicating it as a Forge method would give the same capability two
// interfaces to drift between.
//
// Errors: implementations return the sentinels in types.go
// (ErrNotFound, ErrForbidden, ErrValidation, …) so callers switch on
// forge-neutral values via errors.Is rather than on a forge's status
// codes.
//
// Consumers should prefer a NARROW local interface naming just the
// methods they call (Go's consumer-side interface convention) —
// *github.Forge satisfies both. Forge is the full surface and the
// registry's value type.
type Forge interface {
	// Name is the forge id this implementation registers under and the
	// key Get resolves. See registry.go.
	Name() string

	// ResolveRepoScope resolves the credential scope Fishhawk should act
	// as for repo. Returns ErrNotInstalled when Fishhawk is not installed
	// on the repo. It takes no scope because it PRODUCES one — the call
	// authenticates as the app itself.
	ResolveRepoScope(ctx context.Context, repo RepoRef) (CredentialScope, error)

	// --- refs -----------------------------------------------------------

	// CreateRef creates branch pointing at sha.
	CreateRef(ctx context.Context, scope CredentialScope, repo RepoRef, branch, sha string) error
	// ForceUpdateRef force-updates branch to newSHA, discarding any
	// commits the ref carried (the ADR-035 lineage reset, #867).
	ForceUpdateRef(ctx context.Context, scope CredentialScope, repo RepoRef, branch, newSHA string) error
	// GetBranchSHA returns branch's tip SHA. The bool reports existence:
	// a missing branch is ("", false, nil), not an error.
	GetBranchSHA(ctx context.Context, scope CredentialScope, repo RepoRef, branch string) (string, bool, error)
	// MergeBranch merges head into base server-side and returns the merge
	// commit SHA. Returns ErrMergeConflict on a conflicting merge (the
	// ADR-041 fan-in signal, #1142).
	MergeBranch(ctx context.Context, scope CredentialScope, repo RepoRef, base, head, commitMessage string) (string, error)

	// --- git data -------------------------------------------------------

	// GetRepository fetches repository metadata (the default branch).
	GetRepository(ctx context.Context, scope CredentialScope, repo RepoRef) (*Repository, error)
	// GetCommit fetches a git commit object by SHA — notably its TREE
	// sha, which is what CreateTree's baseTree needs.
	GetCommit(ctx context.Context, scope CredentialScope, repo RepoRef, sha string) (*GitCommit, error)
	// CreateTree layers entries on top of baseTree (a TREE sha, not a
	// commit sha) and returns the new tree's SHA.
	CreateTree(ctx context.Context, scope CredentialScope, repo RepoRef, baseTree string, entries []TreeEntry) (string, error)
	// CreateCommit creates a commit pointing at treeSHA with parents and
	// returns its SHA.
	CreateCommit(ctx context.Context, scope CredentialScope, repo RepoRef, message, treeSHA string, parents []string) (string, error)

	// --- pull requests --------------------------------------------------

	// CreatePullRequest opens a PR from head into base. Returns
	// ErrPullRequestExists when one already exists for the head/base pair
	// (the ADR-032 benign lost race, #714).
	CreatePullRequest(ctx context.Context, scope CredentialScope, repo RepoRef, head, base, title, body string) (*PullRequest, error)
	// GetPullRequest fetches a PR by number.
	GetPullRequest(ctx context.Context, scope CredentialScope, repo RepoRef, number int) (*PullRequest, error)
	// EditPullRequest replaces a PR's body.
	EditPullRequest(ctx context.Context, scope CredentialScope, repo RepoRef, number int, body string) error
	// ClosePullRequest closes a PR without merging it.
	ClosePullRequest(ctx context.Context, scope CredentialScope, repo RepoRef, number int) error
	// ListOpenPullRequestsByHead lists the open PRs from headBranch into
	// base — the recovery read for ErrPullRequestExists.
	ListOpenPullRequestsByHead(ctx context.Context, scope CredentialScope, repo RepoRef, headBranch, base string) ([]PullRequest, error)
	// ListPullRequestsForCommit returns the MERGED PRs associated with a
	// commit — how the release-evidence walk maps a commit back to its PR.
	ListPullRequestsForCommit(ctx context.Context, scope CredentialScope, repo RepoRef, sha string) ([]PullRequestRef, error)
	// EnableAutoMerge queues a PR to merge once branch protection clears.
	// Returns ErrPullRequestCleanStatus when the PR is already merge-ready
	// and the forge refuses to queue it (#1954) — the caller's cue to fall
	// back to a synchronous MergePullRequest.
	EnableAutoMerge(ctx context.Context, scope CredentialScope, repo RepoRef, prNumber int, method MergeMethod) error
	// MergePullRequest merges a PR synchronously. Returns
	// ErrPullRequestNotMergeable when the PR is not in a mergeable state.
	MergePullRequest(ctx context.Context, scope CredentialScope, repo RepoRef, prNumber int, method MergeMethod) error

	// --- commit status --------------------------------------------------

	// CreateCheckRun publishes a commit status on a head commit (#231).
	CreateCheckRun(ctx context.Context, scope CredentialScope, repo RepoRef, p CreateCheckRunParams) (*CreateCheckRunResult, error)

	// --- protection -----------------------------------------------------

	// GetBranchProtection fetches classic branch protection. Returns
	// ErrNotFound when the branch has no classic protection configured —
	// a normal shape on a ruleset-only repo, which the dispatcher treats
	// as "no classic protection" rather than an error (ADR-017).
	GetBranchProtection(ctx context.Context, scope CredentialScope, repo RepoRef, branch string) (*BranchProtection, error)
	// ListRulesetRequiredChecks returns the active rulesets targeting
	// branch and their required-status-check contexts. nil + nil when
	// there are none (ADR-017).
	ListRulesetRequiredChecks(ctx context.Context, scope CredentialScope, repo RepoRef, branch string) ([]RulesetRequiredCheck, error)

	// --- diffs ----------------------------------------------------------

	// CompareCommits returns the changed file paths for base...head
	// (merge-base anchored).
	CompareCommits(ctx context.Context, scope CredentialScope, repo RepoRef, base, head string) ([]string, error)
	// ComparePatch returns the unified diff + changed-file list for
	// base...head (#1060). Truncation is reported in the result, not as an
	// error.
	ComparePatch(ctx context.Context, scope CredentialScope, repo RepoRef, base, head string) (*ComparePatchResult, error)
}

// FileFetcher is the forge-neutral single-file read capability (#2022).
// It is deliberately a STANDALONE capability interface rather than a new
// method on Forge: widening Forge would churn every existing
// implementation and test fake for a capability only the per-repo
// conventions loader consumes. Both registered forges implement it
// (compile-asserted in forge/github and forge/gitlab); consumers name it
// directly, per the narrow-interface convention above.
type FileFetcher interface {
	// FetchFile reads one file from repo at ref and returns its decoded
	// content. path is repo-relative (no leading slash). An empty ref
	// means the repo's default branch: the GitHub implementation omits
	// the ref parameter, and the GitLab implementation substitutes HEAD
	// because the Repository Files API requires an explicit ref. A
	// missing file — or a repo the scope cannot see — is ErrNotFound.
	FetchFile(ctx context.Context, scope CredentialScope, repo RepoRef, path, ref string) (*FileContent, error)
}
