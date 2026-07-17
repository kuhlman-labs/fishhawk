package forge

import "errors"

// This file holds the forge-neutral vocabulary the Forge interface
// speaks (ADR-058 / E45.4). Every declaration here moved verbatim off
// the concrete githubclient package, which now re-declares each name as
// an ALIAS to the canonical here — so the two spellings are the same
// type and the same error value, and the move is behavior-preserving by
// construction. The types are GitHub-shaped today because GitHub is the
// only implementation; the shapes generalize (a GitLab merge request is
// a PullRequest, a pipeline status is a check run) and get their
// forge-neutral refinement when the second implementation lands, not
// from this pass guessing it.
//
// forge imports only stdlib. That is load-bearing: githubclient already
// imports forge (scoped.go, #2009), so a stdlib-only forge is what keeps
// the alias direction cycle-free.

// Errors callers may want to switch on.
//
// The message prefixes stay "githubclient:" deliberately. These are the
// SAME error values the concrete client has always returned — they reach
// operators through logs and audit entries, and this refactor is a
// zero-behavior-change move. Re-prefixing them would be a user-visible
// change smuggled into a mechanical extraction. A forge-neutral
// re-wording is a follow-up, not this pass.
var (
	// ErrNotFound means the resource (repo, file, workflow)
	// doesn't exist OR the App's installation lacks access. GitHub
	// returns 404 for both cases by design — we can't distinguish.
	ErrNotFound = errors.New("githubclient: not found")
	// ErrForbidden means the installation token was rejected (401)
	// or the App lacks permission for the request (403).
	ErrForbidden = errors.New("githubclient: forbidden")
	// ErrValidation means the forge rejected the request as malformed
	// (422). Typical: bad ref name, missing required input.
	ErrValidation = errors.New("githubclient: validation failed")
	// ErrNotInstalled means the GitHub App is not installed on the
	// target repo (GET /repos/{owner}/{repo}/installation returned
	// 404). Distinct from ErrNotFound so callers can surface a
	// precise user-facing error instead of a generic "not found".
	ErrNotInstalled = errors.New("githubclient: app not installed on repo")
	// ErrPullRequestExists means CreatePullRequest hit a 422 whose
	// body indicates a PR already exists for the requested head/base
	// pair. The orchestrator's consolidated-PR path treats this as a
	// benign lost race (ADR-032 / #714) and recovers the existing PR
	// URL via ListOpenPullRequestsByHead rather than failing the
	// settle. Distinct from ErrValidation so the caller can switch on
	// it without re-parsing the 422 body.
	ErrPullRequestExists = errors.New("githubclient: pull request already exists for head/base")
	// ErrMergeConflict means MergeBranch hit a 409 — the head branch
	// could not be merged into the base because of a merge conflict.
	// The fan-in integration step (ADR-041 / #1142) switches on this to
	// fail the decomposed parent's implement stage category-B RECOVERABLE
	// (a dedicated slice_integration_conflict audit + next_action) rather
	// than treating it as an opaque error. Distinct from ErrValidation so
	// the caller distinguishes a genuine conflict from a malformed request.
	ErrMergeConflict = errors.New("githubclient: merge conflict")
	// ErrPullRequestCleanStatus means EnableAutoMerge was rejected
	// because the PR is ALREADY in a merge-ready ("clean") status — GitHub
	// refuses to queue auto-merge on a PR that could be merged synchronously
	// right now (E48.7 / #1954). This is the common operator flow: the
	// operator's `gh pr review --approve` plus green required checks settle the
	// PR clean before the merge verb enables auto-merge, so the enable errors.
	// The githubAutoMerger falls back to a synchronous REST squash merge on
	// this sentinel (serve.go). Wrapped ALONGSIDE ErrValidation (both are
	// reported via errors.Is) so existing ErrValidation callers are unaffected.
	ErrPullRequestCleanStatus = errors.New("githubclient: pull request already in clean status")
	// ErrPullRequestNotMergeable means the synchronous merge
	// (PUT /repos/{owner}/{repo}/pulls/{number}/merge) returned 405 — the PR
	// is not in a mergeable state (base moved, checks not settled, draft, …).
	// Distinct from ErrValidation so the merge caller can surface an actionable
	// retryable error rather than an opaque 4xx (E48.7 / #1954).
	ErrPullRequestNotMergeable = errors.New("githubclient: pull request not mergeable")
)

// RepoRef identifies a repository by owner + name.
type RepoRef struct {
	Owner string
	Name  string
}

// String returns "owner/name" for use in log lines and URLs.
func (r RepoRef) String() string { return r.Owner + "/" + r.Name }

// Repository is the slice of a repository API response Fishhawk needs for
// the App-PR onboarding path (E29.7): the default branch is the base ref
// the scaffold commit and PR target. Other fields land here as callers
// need them.
type Repository struct {
	// DefaultBranch is the repo's default branch name (e.g. "main"),
	// decoded from `default_branch`. The onboarding scaffolder resolves
	// it per repo because the installation webhook payload does not carry
	// it (only repository names).
	DefaultBranch string
}

// GitCommit is the slice of a git-commit object Fishhawk needs to author a
// follow-on commit: the commit's own SHA and the tree it points at.
type GitCommit struct {
	// SHA is the commit object's SHA.
	SHA string
	// TreeSHA is the SHA of the tree the commit points at — the base_tree
	// for a create-tree call that adds files on top of this commit.
	TreeSHA string
}

// TreeEntry is one file placed into a new tree by CreateTree. Content is
// the file's full text — the Git Data API creates the underlying blob
// implicitly from inline content, so no separate CreateBlob call is
// needed. Path is repo-relative (no leading slash).
type TreeEntry struct {
	Path    string
	Content string
}

// PullRequest is a pull request as the forge surface reads it back.
type PullRequest struct {
	NodeID  string
	HeadSHA string
	State   string // "open" | "closed"
	Merged  bool   // true when state=closed and the PR was merged
	// BaseRef is the PR's target branch name (the `base.ref` field).
	// It is the independently-trustworthy compare anchor for the run
	// branch lineage guard (ADR-035, #858): the forge knows what the PR
	// targets, so a contaminated branch commit cannot launder it the
	// way a runner-reported base_sha can.
	BaseRef string
	// HeadRef is the PR's source branch name (the `head.ref` field) —
	// the run branch. The ADR-035 reset remediation (#867) force-updates
	// THIS ref to rewind a foreign on-top commit off the run branch.
	HeadRef string
	// Number and HTMLURL are populated by CreatePullRequest and
	// ListOpenPullRequestsByHead (the consolidated-PR path, #714).
	// GetPullRequest leaves HTMLURL empty — its callers only need
	// NodeID/HeadSHA/State/Merged/Body.
	Number  int
	HTMLURL string
	// Body is the PR description. Populated by GetPullRequest so the
	// merge-time economics stamp (#1702) can splice its delimited section
	// into the existing body idempotently. Empty on CreatePullRequest /
	// ListOpenPullRequestsByHead results (they don't read it back).
	Body string
}

// PullRequestRef is the thin PR identity ListPullRequestsForCommit
// returns — enough for the release-evidence walk to name the PR that
// landed a commit without reading the whole PR back.
type PullRequestRef struct {
	Number int
	URL    string
	Title  string
}

// MergeMethod names how a PR is merged.
type MergeMethod string

// Merge methods accepted by EnableAutoMerge and MergePullRequest.
const (
	MergeMethodSquash MergeMethod = "SQUASH"
	MergeMethodMerge  MergeMethod = "MERGE"
	MergeMethodRebase MergeMethod = "REBASE"
)

// BranchProtection is the classic branch-protection slice the dispatcher
// needs.
type BranchProtection struct {
	// RequiredStatusCheckContexts is the closed list of context
	// names a PR must report green before the forge allows merge.
	// Empty (nil or zero-length) means classic protection has no
	// required-status-checks rule for the branch — rulesets may
	// still contribute. The dispatcher derives the union per
	// ADR-017.
	RequiredStatusCheckContexts []string
}

// RulesetRequiredCheck is one ruleset's contribution to a branch's
// required-status-check set.
type RulesetRequiredCheck struct {
	// RulesetID is the forge-side ruleset identifier — surfaced so
	// the snapshot's `sources` field can record exactly which
	// ruleset contributed.
	RulesetID int64
	// Contexts is the deduped list of context names the ruleset
	// requires. May be empty when the ruleset doesn't include a
	// `required_status_checks` rule.
	Contexts []string
}

// ComparePatchFile is one changed file in a ComparePatchResult.
type ComparePatchFile struct {
	Path   string
	Status string
}

// ComparePatchResult carries the unified-diff text + changed-file list for
// base...head, plus a truncation signal (#1060). It is the input the
// consolidated decomposition review builds its policy.Diff from, since the
// decomposed parent has no runner trace bundle of its own.
type ComparePatchResult struct {
	// HeadSHA is the tip commit of head (the last commit the comparison
	// reports), reused as the implement-review dedup key. Empty when the
	// comparison reports no commits ahead of base.
	HeadSHA string
	// Patch is the reconstructed unified diff: each changed file's hunks
	// prefixed with a synthetic `diff --git` header so a downstream
	// content reviewer reads it as an ordinary git diff. Empty when no
	// file carried a patch body.
	Patch string
	// Files is every changed file the comparison reported, with its
	// forge word-form status.
	Files []ComparePatchFile
	// Truncated is set when the forge capped the comparison — the file list
	// reached the documented 300-file ceiling, or a changed file's patch
	// body was omitted (oversized diff). The review under-reviews when
	// this is set, so the caller surfaces it loudly rather than silently.
	Truncated bool
	// TruncationReason names which cap tripped, for the degradation
	// log/audit. Empty when Truncated is false.
	TruncationReason string
}

// CheckRunStatus is the commit-status `status` enum.
//
// Check-run status values. Documented at
// https://docs.github.com/en/rest/checks/runs.
type CheckRunStatus string

// Check-run status values.
const (
	CheckRunStatusQueued     CheckRunStatus = "queued"
	CheckRunStatusInProgress CheckRunStatus = "in_progress"
	CheckRunStatusCompleted  CheckRunStatus = "completed"
)

// CheckRunConclusion is the Checks API `conclusion` enum.
// Required when status=completed; must be empty otherwise.
type CheckRunConclusion string

// Check-run conclusion values.
const (
	CheckRunConclusionSuccess        CheckRunConclusion = "success"
	CheckRunConclusionFailure        CheckRunConclusion = "failure"
	CheckRunConclusionNeutral        CheckRunConclusion = "neutral"
	CheckRunConclusionCancelled      CheckRunConclusion = "cancelled"
	CheckRunConclusionTimedOut       CheckRunConclusion = "timed_out"
	CheckRunConclusionActionRequired CheckRunConclusion = "action_required"
	CheckRunConclusionSkipped        CheckRunConclusion = "skipped"
)

// CreateCheckRunParams is the typed body for publishing a commit status.
// Only the fields Fishhawk uses today are surfaced; the GitHub schema is
// wider.
type CreateCheckRunParams struct {
	Name          string
	HeadSHA       string
	Status        CheckRunStatus
	Conclusion    CheckRunConclusion // required when Status==completed
	DetailsURL    string             // where the "Details" link on the forge points (typically a Fishhawk run URL)
	OutputTitle   string
	OutputSummary string
}

// CreateCheckRunResult carries the bits of the forge's response we
// care about. ID lets a caller PATCH the same row later if a
// follow-up surface ever needs progressive updates; v0 callers
// typically POST a fresh row per state change and ignore it.
type CreateCheckRunResult struct {
	ID      int64
	HTMLURL string
}
