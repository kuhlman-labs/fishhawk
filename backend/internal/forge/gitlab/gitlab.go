// Package gitlab is the GitLab implementation of forge.Forge (ADR-058 /
// E45.5, #1859). It adapts the GitLab REST v4 client (gitlabclient) onto
// the forge-neutral vocabulary: it resolves a per-call token through a
// forge.CredentialProvider, constructs a cheap gitlabclient.Client for the
// call against a configurable base URL (GitLab.com SaaS and self-managed
// alike), and maps GitLab's status codes onto the forge sentinels so a
// forge-neutral caller switches on forge.ErrNotFound / ErrForbidden / …
// rather than on a GitLab-shaped error.
//
// # Scope refs
//
// ResolveRepoScope returns a "gitlab:<numeric-project-id>" credential scope.
// The ref is non-numeric AS A WHOLE STRING, so forge.CredentialScope's
// GitHubInstallationID() keeps failing closed on it per the forge README
// parse rule — a GitLab ref can never be mistaken for a GitHub installation
// id. Every other method parses the project id back out of the scope ref
// (see projectIDFromScope) and rejects a non-gitlab-shaped ref rather than
// dispatching against a wrong project.
//
// # Unsupported operations
//
// Three Forge methods have no GitLab REST expression and fail closed with
// forge.ErrUnsupported: the git-data trio (GetCommit's tree SHA,
// CreateTree, CreateCommit — GitLab has no git-data API; commit authoring is
// POST .../repository/commits with an actions[] array) and MergeBranch
// (GitLab has no server-side branch-merge endpoint outside a merge request).
// Their only consumers today are GitHub-flow-specific (onboarding scaffold,
// ADR-041 fan-in), so GitLab fan-in is a documented deferral.
//
// # Credentials
//
// v0 GitLab credentials are the configured group/project access token
// (ADR-058 scope decision 2's fallback), supplied via a static
// forge.CredentialProvider (see StaticCredentialProvider). The
// group-scoped OAuth application broker is deliberately out of scope here.
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
)

// forgeName is the registry id this adapter registers under and Name
// returns. It is the value serve.go passes to forge.Get and the value
// stored on run rows as the forge discriminator.
const forgeName = "gitlab"

// scopeRefPrefix is the "gitlab:<id>" credential-scope ref prefix
// ResolveRepoScope emits and every other method parses back.
const scopeRefPrefix = "gitlab:"

// Forge adapts the GitLab REST v4 client onto forge.Forge. It holds the
// instance base URL, a forge.CredentialProvider that resolves a scope to a
// PRIVATE-TOKEN bearer, and an optional injectable Doer so tests drive every
// method against a stub without touching the network. A concrete
// gitlabclient.Client is a cheap struct, constructed per call around the
// freshly-resolved token rather than cached, mirroring the scope-first shape
// the interface declares.
type Forge struct {
	baseURL  string
	provider forge.CredentialProvider
	doer     gitlabclient.Doer // optional; nil → gitlabclient's http.DefaultClient
}

// Compile-time assertion that *Forge satisfies the full Forge surface.
var _ forge.Forge = (*Forge)(nil)

// Option customises a Forge at construction.
type Option func(*Forge)

// WithHTTPClient injects the HTTP transport used for every per-call
// gitlabclient.Client. Tests pass a stub Doer here; production leaves it
// unset so the client uses http.DefaultClient.
func WithHTTPClient(d gitlabclient.Doer) Option {
	return func(f *Forge) { f.doer = d }
}

// New returns a GitLab Forge for the instance at baseURL, resolving tokens
// through provider. baseURL covers GitLab.com (https://gitlab.com) and a
// self-managed host identically — gitlabclient normalises the trailing
// slash. provider is the credential seam; v0 wires a StaticCredentialProvider
// carrying the configured group/project access token.
func New(baseURL string, provider forge.CredentialProvider, opts ...Option) *Forge {
	f := &Forge{baseURL: baseURL, provider: provider}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Name returns the forge id ("gitlab").
func (*Forge) Name() string { return forgeName }

// StaticCredentialProvider is a forge.CredentialProvider that returns one
// fixed token for every scope — the v0 GitLab credential path (ADR-058 scope
// decision 2's fallback: the configured group/project access token, resolved
// without a broker). The group-scoped OAuth application broker is deferred.
type StaticCredentialProvider struct {
	token string
}

// NewStaticCredentialProvider wraps token as a fixed-token
// forge.CredentialProvider.
func NewStaticCredentialProvider(token string) StaticCredentialProvider {
	return StaticCredentialProvider{token: token}
}

// Token returns the fixed token, ignoring the scope. The scope still carries
// the "gitlab:<id>" project ref the adapter parses for addressing; it just
// does not select a credential in v0.
func (p StaticCredentialProvider) Token(context.Context, forge.CredentialScope) (string, error) {
	return p.token, nil
}

var _ forge.CredentialProvider = StaticCredentialProvider{}

// client constructs a gitlabclient.Client for one call, authed with token
// and pointed at the configured base URL. The injected Doer (if any) is
// threaded so tests observe the request.
func (f *Forge) client(token string) *gitlabclient.Client {
	if f.doer != nil {
		return gitlabclient.New(f.baseURL, token, gitlabclient.WithHTTPClient(f.doer))
	}
	return gitlabclient.New(f.baseURL, token)
}

// resolve is the per-call entry shared by every scope-taking method: it
// resolves the token for scope and parses the project id back out of the
// scope ref. A non-gitlab-shaped ref (or a token-resolution failure) fails
// closed here before any HTTP call.
func (f *Forge) resolve(ctx context.Context, scope forge.CredentialScope) (*gitlabclient.Client, int, error) {
	pid, err := projectIDFromScope(scope)
	if err != nil {
		return nil, 0, err
	}
	token, err := f.provider.Token(ctx, scope)
	if err != nil {
		return nil, 0, err
	}
	return f.client(token), pid, nil
}

// projectIDFromScope parses the numeric project id out of a
// "gitlab:<id>" credential-scope ref. It rejects a ref that is not
// gitlab-shaped or that carries a non-positive/non-numeric id, so a wrong
// forge's scope can never address a GitLab project.
func projectIDFromScope(scope forge.CredentialScope) (int, error) {
	ref := scope.Ref()
	rest, ok := strings.CutPrefix(ref, scopeRefPrefix)
	if !ok {
		return 0, fmt.Errorf("gitlab: credential scope ref %q is not gitlab-shaped (want %q<project-id>)", ref, scopeRefPrefix)
	}
	id, err := strconv.Atoi(rest)
	if err != nil {
		return 0, fmt.Errorf("gitlab: credential scope ref %q carries a non-numeric project id: %w", ref, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("gitlab: credential scope ref %q carries a non-positive project id %d", ref, id)
	}
	return id, nil
}

// apiStatus returns the HTTP status of a *gitlabclient.APIError anywhere in
// err's chain, or 0 when err is not (or does not wrap) an APIError.
func apiStatus(err error) int {
	var apiErr *gitlabclient.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// mapError translates a gitlabclient error into the forge sentinel a
// forge-neutral caller switches on, preserving the original (APIError
// status + body) alongside the sentinel via errors.Join so both remain
// matchable. A non-APIError (transport failure) and any unmapped status pass
// through unchanged.
func mapError(err error) error {
	switch apiStatus(err) {
	case http.StatusNotFound:
		return errors.Join(forge.ErrNotFound, err)
	case http.StatusUnauthorized, http.StatusForbidden:
		return errors.Join(forge.ErrForbidden, err)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return errors.Join(forge.ErrValidation, err)
	default:
		return err
	}
}

// --- repo scope ---------------------------------------------------------

// ResolveRepoScope resolves the credential scope for repo by looking its
// namespaced path (Owner may carry nested groups) up to a numeric project id
// and returning a "gitlab:<id>" scope. A 404 on the lookup means the project
// is not visible to the configured token — mapped to forge.ErrNotInstalled
// (distinct from ErrNotFound) so a caller surfaces a precise not-installed
// error rather than a generic not-found.
func (f *Forge) ResolveRepoScope(ctx context.Context, repo forge.RepoRef) (forge.CredentialScope, error) {
	// ResolveRepoScope PRODUCES a scope, so it cannot take one; it resolves
	// the token against the zero scope (the static provider ignores it).
	token, err := f.provider.Token(ctx, forge.CredentialScope{})
	if err != nil {
		return forge.CredentialScope{}, err
	}
	proj, err := f.client(token).GetProject(ctx, repo.String())
	if err != nil {
		if apiStatus(err) == http.StatusNotFound {
			return forge.CredentialScope{}, errors.Join(forge.ErrNotInstalled, err)
		}
		return forge.CredentialScope{}, mapError(err)
	}
	return forge.FromRef(scopeRefPrefix + strconv.Itoa(proj.ID)), nil
}

// --- refs ---------------------------------------------------------------

// CreateRef creates branch pointing at sha.
func (f *Forge) CreateRef(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, branch, sha string) error {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return err
	}
	if _, err := c.CreateBranch(ctx, pid, branch, sha); err != nil {
		return mapError(err)
	}
	return nil
}

// ForceUpdateRef force-updates branch to newSHA. GitLab's Branches API has
// no update/force operation, so this is delete-then-recreate: it deletes the
// existing branch (tolerating a 404 — the branch may already be absent) and
// recreates it at newSHA. The window between the two calls is NON-ATOMIC and
// briefly drops branch protection state on a protected branch; this is the
// documented cost of GitLab's missing force-update.
func (f *Forge) ForceUpdateRef(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, branch, newSHA string) error {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return err
	}
	if err := c.DeleteBranch(ctx, pid, branch); err != nil {
		// A missing branch is fine — the goal is a branch pointing at newSHA.
		if apiStatus(err) != http.StatusNotFound {
			return mapError(err)
		}
	}
	if _, err := c.CreateBranch(ctx, pid, branch, newSHA); err != nil {
		return mapError(err)
	}
	return nil
}

// GetBranchSHA returns branch's tip SHA. A missing branch is ("", false,
// nil) — the interface's existence contract — not an error.
func (f *Forge) GetBranchSHA(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, branch string) (string, bool, error) {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return "", false, err
	}
	b, err := c.GetBranch(ctx, pid, branch)
	if err != nil {
		if apiStatus(err) == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, mapError(err)
	}
	if b.Commit == nil {
		return "", false, nil
	}
	return b.Commit.ID, true, nil
}

// MergeBranch is unsupported: GitLab has no server-side branch-merge
// endpoint outside a merge request. See the package doc + ErrUnsupported.
func (*Forge) MergeBranch(context.Context, forge.CredentialScope, forge.RepoRef, string, string, string) (string, error) {
	return "", fmt.Errorf("gitlab: MergeBranch: %w", forge.ErrUnsupported)
}

// --- git data -----------------------------------------------------------

// GetRepository fetches repository metadata (the default branch) by parsing
// the project id out of the scope ref and reading the project.
func (f *Forge) GetRepository(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef) (*forge.Repository, error) {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	info, err := c.GetProjectByID(ctx, pid)
	if err != nil {
		return nil, mapError(err)
	}
	return &forge.Repository{DefaultBranch: info.DefaultBranch}, nil
}

// GetCommit is unsupported: GitLab exposes no tree SHA on a commit object,
// which is the field this method exists to read. See ErrUnsupported.
func (*Forge) GetCommit(context.Context, forge.CredentialScope, forge.RepoRef, string) (*forge.GitCommit, error) {
	return nil, fmt.Errorf("gitlab: GetCommit: %w", forge.ErrUnsupported)
}

// CreateTree is unsupported: GitLab has no git-data tree API. See
// ErrUnsupported.
func (*Forge) CreateTree(context.Context, forge.CredentialScope, forge.RepoRef, string, []forge.TreeEntry) (string, error) {
	return "", fmt.Errorf("gitlab: CreateTree: %w", forge.ErrUnsupported)
}

// CreateCommit is unsupported: GitLab has no git-data commit-object API
// (commit authoring is POST .../repository/commits with an actions[] array).
// See ErrUnsupported.
func (*Forge) CreateCommit(context.Context, forge.CredentialScope, forge.RepoRef, string, string, []string) (string, error) {
	return "", fmt.Errorf("gitlab: CreateCommit: %w", forge.ErrUnsupported)
}

// --- pull requests ------------------------------------------------------

// CreatePullRequest opens a merge request from head into base. A 409 means
// an MR already exists for the head/base pair — mapped to
// forge.ErrPullRequestExists so the ADR-032 lost-race recovery
// (ListOpenPullRequestsByHead) runs unchanged.
func (f *Forge) CreatePullRequest(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, head, base, title, body string) (*forge.PullRequest, error) {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	mr, err := c.CreateMergeRequest(ctx, pid, gitlabclient.CreateMergeRequestParams{
		SourceBranch: head,
		TargetBranch: base,
		Title:        title,
		Description:  body,
	})
	if err != nil {
		if apiStatus(err) == http.StatusConflict {
			return nil, errors.Join(forge.ErrPullRequestExists, err)
		}
		return nil, mapError(err)
	}
	return mergeRequestToPR(mr), nil
}

// GetPullRequest fetches a merge request by its project-scoped iid.
func (f *Forge) GetPullRequest(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, number int) (*forge.PullRequest, error) {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	mr, err := c.GetMergeRequest(ctx, pid, number)
	if err != nil {
		return nil, mapError(err)
	}
	return mergeRequestToPR(mr), nil
}

// EditPullRequest replaces a merge request's description.
func (f *Forge) EditPullRequest(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, number int, body string) error {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return err
	}
	if _, err := c.UpdateMergeRequest(ctx, pid, number, gitlabclient.UpdateMergeRequestParams{Description: &body}); err != nil {
		return mapError(err)
	}
	return nil
}

// ClosePullRequest closes a merge request without merging it.
func (f *Forge) ClosePullRequest(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, number int) error {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return err
	}
	if _, err := c.UpdateMergeRequest(ctx, pid, number, gitlabclient.UpdateMergeRequestParams{StateEvent: "close"}); err != nil {
		return mapError(err)
	}
	return nil
}

// ListOpenPullRequestsByHead lists the open merge requests from headBranch
// into base — the recovery read for ErrPullRequestExists.
func (f *Forge) ListOpenPullRequestsByHead(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, headBranch, base string) ([]forge.PullRequest, error) {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	mrs, err := c.ListMergeRequests(ctx, pid, gitlabclient.ListMergeRequestsParams{
		State:        "opened",
		SourceBranch: headBranch,
		TargetBranch: base,
	})
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]forge.PullRequest, 0, len(mrs))
	for i := range mrs {
		out = append(out, *mergeRequestToPR(&mrs[i]))
	}
	return out, nil
}

// ListPullRequestsForCommit returns the merge requests associated with a
// commit — how the release-evidence walk maps a commit back to its MR.
func (f *Forge) ListPullRequestsForCommit(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, sha string) ([]forge.PullRequestRef, error) {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	mrs, err := c.ListMergeRequestsForCommit(ctx, pid, sha)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]forge.PullRequestRef, 0, len(mrs))
	for i := range mrs {
		out = append(out, forge.PullRequestRef{
			Number: mrs[i].IID,
			URL:    mrs[i].WebURL,
			Title:  mrs[i].Title,
		})
	}
	return out, nil
}

// EnableAutoMerge queues a merge request to merge once its pipeline
// succeeds (merge_when_pipeline_succeeds=true). A 405/406 means the MR is
// not in a mergeable state — mapped to forge.ErrPullRequestNotMergeable.
// GitLab has no ErrPullRequestCleanStatus analog: a merge-ready MR merges
// synchronously on the same call, so that sentinel is never produced here.
func (f *Forge) EnableAutoMerge(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, prNumber int, method forge.MergeMethod) error {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return err
	}
	_, err = c.MergeMergeRequest(ctx, pid, prNumber, gitlabclient.MergeMergeRequestParams{
		Squash:                    method == forge.MergeMethodSquash,
		MergeWhenPipelineSucceeds: true,
	})
	return mapMergeError(err)
}

// MergePullRequest merges a merge request synchronously. A 405/406 maps to
// forge.ErrPullRequestNotMergeable.
func (f *Forge) MergePullRequest(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, prNumber int, method forge.MergeMethod) error {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return err
	}
	_, err = c.MergeMergeRequest(ctx, pid, prNumber, gitlabclient.MergeMergeRequestParams{
		Squash: method == forge.MergeMethodSquash,
	})
	return mapMergeError(err)
}

// mapMergeError maps a merge-endpoint error: 405/406 → not-mergeable, else
// the base sentinel mapping. nil passes through.
func mapMergeError(err error) error {
	if err == nil {
		return nil
	}
	if s := apiStatus(err); s == http.StatusMethodNotAllowed || s == http.StatusNotAcceptable {
		return errors.Join(forge.ErrPullRequestNotMergeable, err)
	}
	return mapError(err)
}

// --- commit status ------------------------------------------------------

// CreateCheckRun publishes a commit status on p.HeadSHA. The
// CheckRunStatus/Conclusion pair is mapped onto GitLab's closed set of
// pipeline-status words (see checkState); the status identity (p.Name) is
// sent as the `name` parameter so it does not collapse to GitLab's default
// label. An unmappable status/conclusion fails closed with ErrValidation
// rather than posting an invalid state.
func (f *Forge) CreateCheckRun(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, p forge.CreateCheckRunParams) (*forge.CreateCheckRunResult, error) {
	state, err := checkState(p.Status, p.Conclusion)
	if err != nil {
		return nil, err
	}
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	// Prefer the summary as the human-facing description, falling back to
	// the title; GitLab carries a single free-text description field.
	desc := p.OutputSummary
	if desc == "" {
		desc = p.OutputTitle
	}
	st, err := c.SetCommitStatus(ctx, pid, p.HeadSHA, gitlabclient.SetCommitStatusParams{
		State:       state,
		Name:        p.Name,
		TargetURL:   p.DetailsURL,
		Description: desc,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &forge.CreateCheckRunResult{ID: int64(st.ID), HTMLURL: st.TargetURL}, nil
}

// checkState maps a Forge CheckRunStatus (+ Conclusion when completed) onto
// GitLab's closed pipeline-status set: pending, running, success, failed,
// canceled. It is total over the defined enum members — a queued/in_progress
// status maps directly; a completed status maps on its conclusion — and
// fails closed (ErrValidation) on an unknown status, or a completed status
// carrying an empty/unknown conclusion, rather than emitting an invalid
// GitLab state.
func checkState(status forge.CheckRunStatus, conclusion forge.CheckRunConclusion) (string, error) {
	switch status {
	case forge.CheckRunStatusQueued:
		return "pending", nil
	case forge.CheckRunStatusInProgress:
		return "running", nil
	case forge.CheckRunStatusCompleted:
		switch conclusion {
		case forge.CheckRunConclusionSuccess:
			return "success", nil
		case forge.CheckRunConclusionFailure:
			return "failed", nil
		case forge.CheckRunConclusionCancelled:
			return "canceled", nil
		// Non-failing terminal conclusions map to success so a required
		// status does not block: GitLab has no neutral/skipped state.
		case forge.CheckRunConclusionNeutral, forge.CheckRunConclusionSkipped:
			return "success", nil
		// A timeout or action-required outcome is a non-success terminal
		// state, mapped to failed.
		case forge.CheckRunConclusionTimedOut, forge.CheckRunConclusionActionRequired:
			return "failed", nil
		default:
			return "", fmt.Errorf("gitlab: CreateCheckRun: %w: completed status carries unmappable conclusion %q", forge.ErrValidation, conclusion)
		}
	default:
		return "", fmt.Errorf("gitlab: CreateCheckRun: %w: unmappable status %q", forge.ErrValidation, status)
	}
}

// --- protection ---------------------------------------------------------

// GetBranchProtection reads a branch's classic protection. GitLab protection
// carries NO required-status-check contexts, so a present entry maps to an
// empty context list; a 404 (no protection configured) maps to
// forge.ErrNotFound, which the ADR-017 dispatcher treats as "no classic
// protection".
func (f *Forge) GetBranchProtection(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, branch string) (*forge.BranchProtection, error) {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	if _, err := c.GetProtectedBranch(ctx, pid, branch); err != nil {
		return nil, mapError(err)
	}
	return &forge.BranchProtection{RequiredStatusCheckContexts: nil}, nil
}

// ListRulesetRequiredChecks returns (nil, nil): GitLab protected branches
// carry no required-status-check contexts, which the ADR-017 dispatcher
// treats as "no required checks".
func (*Forge) ListRulesetRequiredChecks(context.Context, forge.CredentialScope, forge.RepoRef, string) ([]forge.RulesetRequiredCheck, error) {
	return nil, nil
}

// --- diffs --------------------------------------------------------------

// CompareCommits returns the changed file paths for base...head. GitLab's
// compare with straight=false gives merge-base (three-dot) semantics
// matching the GitHub base...head contract.
func (f *Forge) CompareCommits(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, base, head string) ([]string, error) {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	cmp, err := c.Compare(ctx, pid, base, head, false)
	if err != nil {
		return nil, mapError(err)
	}
	paths := make([]string, 0, len(cmp.Diffs))
	for i := range cmp.Diffs {
		paths = append(paths, changedPath(&cmp.Diffs[i]))
	}
	return paths, nil
}

// ComparePatch returns the unified diff + changed-file list for base...head,
// reconstructing a git-style patch: each changed file's GitLab hunk body is
// prefixed with a synthetic `diff --git a/<old> b/<new>` header so a
// downstream content reviewer reads it as an ordinary git diff — mirroring
// the GitHub ComparePatchResult contract. A GitLab compare_timeout is
// surfaced as Truncated (not an error).
func (f *Forge) ComparePatch(ctx context.Context, scope forge.CredentialScope, _ forge.RepoRef, base, head string) (*forge.ComparePatchResult, error) {
	c, pid, err := f.resolve(ctx, scope)
	if err != nil {
		return nil, err
	}
	cmp, err := c.Compare(ctx, pid, base, head, false)
	if err != nil {
		return nil, mapError(err)
	}

	res := &forge.ComparePatchResult{}
	if cmp.Commit != nil {
		res.HeadSHA = cmp.Commit.ID
	}
	var patch strings.Builder
	res.Files = make([]forge.ComparePatchFile, 0, len(cmp.Diffs))
	for i := range cmp.Diffs {
		d := &cmp.Diffs[i]
		res.Files = append(res.Files, forge.ComparePatchFile{
			Path:   changedPath(d),
			Status: diffStatus(d),
		})
		if d.Diff != "" {
			fmt.Fprintf(&patch, "diff --git a/%s b/%s\n", d.OldPath, d.NewPath)
			patch.WriteString(d.Diff)
			if !strings.HasSuffix(d.Diff, "\n") {
				patch.WriteByte('\n')
			}
		}
	}
	res.Patch = patch.String()
	if cmp.CompareTimeout {
		res.Truncated = true
		res.TruncationReason = "gitlab compare_timeout: the diff was capped server-side"
	}
	return res, nil
}

// changedPath returns the path a compare diff entry reports as changed — the
// new path, except for a deletion where only the old path exists.
func changedPath(d *gitlabclient.CompareDiff) string {
	if d.DeletedFile && d.NewPath == "" {
		return d.OldPath
	}
	return d.NewPath
}

// diffStatus maps a GitLab compare diff entry onto the GitHub-shaped status
// word the ComparePatchResult contract uses.
func diffStatus(d *gitlabclient.CompareDiff) string {
	switch {
	case d.NewFile:
		return "added"
	case d.DeletedFile:
		return "removed"
	case d.RenamedFile:
		return "renamed"
	default:
		return "modified"
	}
}

// mergeRequestToPR maps a GitLab merge request onto the forge PullRequest
// vocabulary: State collapses GitLab's lifecycle word to open|closed, Merged
// is derived from state=="merged", and the source/target branches become
// Head/BaseRef.
func mergeRequestToPR(mr *gitlabclient.MergeRequest) *forge.PullRequest {
	state := "closed"
	if mr.State == "opened" {
		state = "open"
	}
	return &forge.PullRequest{
		HeadSHA: mr.SHA,
		State:   state,
		Merged:  mr.State == "merged",
		BaseRef: mr.TargetBranch,
		HeadRef: mr.SourceBranch,
		Number:  mr.IID,
		HTMLURL: mr.WebURL,
		Body:    mr.Description,
	}
}
