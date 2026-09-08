package forge

import "context"

// IssueOperations is the forge-neutral issue-thread capability the
// split-parent auto-close watcher needs (E50.17 / #2900): read an issue's
// state, list its comments, post a comment, and set its state. Exactly
// those four — the watcher's whole surface — and nothing wider.
//
// It is deliberately a STANDALONE capability interface rather than four
// new methods on Forge, for the same reason FileFetcher is (#2022):
// widening Forge would churn every existing implementation and test fake
// for a capability ONE consumer uses, while a standalone interface lets
// both registered adapters carry it off-interface with a compile-time
// assertion (`var _ forge.IssueOperations = (*Forge)(nil)` in forge/github
// and forge/gitlab). Consumers name it directly, per the narrow-interface
// convention on Forge, and resolve a registered forge.Forge to it with a
// type assertion — a forge that does not implement it is a fail-closed
// nil, never a fabricated no-op.
//
// The method names are chosen NOT to shadow the same-purpose methods the
// GitHub adapter promotes from its embedded *githubclient.Client
// (GetIssue / ListIssueComments / CreateIssueComment / UpdateIssue).
// Shadowing would compile — an outer method wins over an embedded one —
// but would silently change the type a caller of f.GetIssue receives.
// FileFetcher.FetchFile-vs-GetFile is the same choice for the same reason.
//
// Errors: implementations return the sentinels in types.go (ErrNotFound,
// ErrForbidden, ErrValidation, …) so callers switch on forge-neutral
// values via errors.Is rather than on a forge's status codes.
type IssueOperations interface {
	// FetchIssue reads issue number in repo. Every adapter normalizes
	// State onto the forge-neutral "open"|"closed" vocabulary (GitLab's
	// native "opened" becomes "open"), because the watcher's already-closed
	// short-circuit compares against "closed" and a passed-through native
	// word would silently make that branch unreachable. StateReason is
	// populated only by forges that carry one; see Issue.StateReason.
	FetchIssue(ctx context.Context, scope CredentialScope, repo RepoRef, number int) (*Issue, error)
	// FetchIssueComments lists the issue's comment thread, paging to
	// exhaustion — a single-page read would miss a marker that scrolled
	// onto page 2 and re-post an idempotency-keyed comment on every
	// redelivery.
	FetchIssueComments(ctx context.Context, scope CredentialScope, repo RepoRef, number int) ([]IssueComment, error)
	// PostIssueComment appends body as a new comment on the issue.
	PostIssueComment(ctx context.Context, scope CredentialScope, repo RepoRef, number int, body string) error
	// SetIssueState changes the issue's state. u.State is REQUIRED — an
	// update that sets no state is refused locally (ErrValidation) rather
	// than sent. u.StateReason is BEST-EFFORT: a forge with a state_reason
	// concept (GitHub) transmits it alongside the state; a forge with none
	// (GitLab) records the state change and IGNORES the reason rather than
	// failing, so a caller may always pass the reason it would want.
	SetIssueState(ctx context.Context, scope CredentialScope, repo RepoRef, number int, u IssueStateUpdate) error
}

// Issue is an issue as IssueOperations.FetchIssue reads it back.
type Issue struct {
	// Number is the forge's user-facing issue number (GitHub `number`,
	// GitLab project-scoped `iid`).
	Number int
	Title  string
	Body   string
	// State is the forge-neutral lifecycle word: "open" | "closed". Every
	// adapter normalizes its native vocabulary onto these two (GitLab
	// "opened" -> "open"); an unrecognized native value passes through
	// unchanged so a consumer sees it rather than a guessed mapping.
	State string
	// StateReason is GitHub's issue `state_reason` ("completed",
	// "not_planned", "duplicate", "reopened", or "" when absent). GitLab's
	// issue object has NO equivalent field, so the GitLab adapter leaves
	// this EMPTY — a documented, named gap, never a fabricated value.
	// Consumers gating on it must treat "" as "no reason carried", which
	// is also what GitHub sends for a plain close.
	StateReason string
	// Labels is the issue's label names.
	Labels []string
}

// IssueComment is one entry of an issue's comment thread as
// IssueOperations.FetchIssueComments reads it back.
type IssueComment struct {
	// ID is the forge's comment id (GitHub comment id, GitLab note id).
	ID int64
	// Author is the commenting user's login/username.
	Author string
	// Body is the comment's raw markdown, byte-intact — an idempotency
	// marker embedded as an HTML comment must round-trip through here.
	Body string
	// CreatedAt is the forge's creation timestamp, passed through as the
	// RFC 3339 string the forge returned.
	CreatedAt string
}

// IssueStateUpdate is the input to IssueOperations.SetIssueState. Both
// fields are POINTERS for the absent-vs-empty reason
// githubclient.UpdateIssueParams documents: only a set field is
// transmitted, so a nil StateReason sends no reason at all rather than an
// empty one.
type IssueStateUpdate struct {
	// State is the target forge-neutral state: "open" or "closed". Required.
	State *string
	// StateReason is the optional GitHub-shaped reason ("completed",
	// "not_planned", "duplicate", "reopened"). Best-effort: ignored by a
	// forge with no state_reason concept.
	StateReason *string
}
