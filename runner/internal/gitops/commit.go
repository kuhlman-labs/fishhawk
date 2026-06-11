// Package gitops contains the runner's git-write surface: create a
// branch, commit any agent edits, push to origin, and open a PR via
// GitHub's REST API. Distinct from internal/gitdiff (which is
// read-only and produces a constraint.Diff) because the write path
// is opinionated about author identity, branch naming, and the
// shape of the resulting pull-request artifact.
//
// Push auth is the calling environment's responsibility. In the
// hosted Actions flow (#201) `actions/checkout` is called with the
// Fishhawk App's installation token, which sets a local
// `http.<host>.extraheader` that subsequent git operations
// (including this package's push) authenticate with. We don't
// embed credentials in the URL or set our own extraheader — both
// approaches caused failure modes in #199 / #200 (the URL-embedded
// path was overridden by actions/checkout's existing extraheader,
// and `-c http.<host>.extraheader=…` produced a duplicate
// Authorization header because git's extraheader is multi-valued).
// PR creation (OpenPR) takes its token directly because GitHub's
// REST API needs an explicit `Authorization: Bearer <token>`.
package gitops

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// DefaultRemote is the git remote ShipBranch pushes to. Always
// "origin" in the GitHub Actions checkout shape.
const DefaultRemote = "origin"

// ErrCommitWouldNotCompile is the sentinel a VerifyCommit hook wraps
// (via fmt.Errorf("%w: ...")) when the scope-only committed tree fails
// to compile because build-required files were excluded as scope drift
// (#728). CommitAndPush returns it BEFORE pushing, so the failure leaves
// origin untouched — no broken branch, no PR. The runner classifies it
// as a category-B failure (wrong-shaped output → re-scope/re-plan).
var ErrCommitWouldNotCompile = errors.New("gitops: committed tree would not compile")

// ErrCreatedOutOfScope is the general created-out-of-scope sentinel (#825,
// extending #818). It is the analogue of ErrCommitWouldNotCompile /
// ErrCommittedTestsFailed for the case where the agent CREATED net-new
// (untracked) files outside the approved scope.files: StageScoped (#581) would
// silently strip them from the scope-only commit while the in-scope edits that
// REFERENCE them land, shipping a self-inconsistent, misleadingly-green partial
// result. The gate is no longer fix-up-specific — it covers both the open-PR
// implement push and the fix-up pass; ErrFixupCreatedOutOfScope is the fix-up
// specialization that wraps this sentinel. The runner wraps the relevant value
// (via fmt.Errorf("%w: ...")) from a VerifyCommit branch and returns it BEFORE
// pushing, so origin is untouched. It is classified category-B (re-scope/re-plan).
// Only CREATED (untracked) out-of-scope files trip this; modified-but-out-of-scope
// drift stays flag-only (ADR-027).
var ErrCreatedOutOfScope = errors.New("gitops: created out-of-scope files")

// ErrFixupCreatedOutOfScope is the fix-up specialization of ErrCreatedOutOfScope
// (#818). A fix-up pass cannot widen the stage's fixed scope.files, so a net-new
// file the fix-up needed to create is out of scope by construction. It wraps
// ErrCreatedOutOfScope, so it satisfies BOTH errors.Is(err, ErrFixupCreatedOutOfScope)
// and errors.Is(err, ErrCreatedOutOfScope). The runner wraps this from the fix-up
// VerifyCommit branch and returns it BEFORE pushing, so origin is untouched. It is
// classified category-B (re-scope/re-plan); the backend's #788 fix-up recovery then
// restores the run to its pre-fix-up review gate. Only CREATED (untracked)
// out-of-scope files trip this; modified-but-out-of-scope drift stays flag-only
// (ADR-027).
var ErrFixupCreatedOutOfScope = fmt.Errorf("%w (fix-up)", ErrCreatedOutOfScope)

// ErrCommitOutOfScope is the post-commit out-of-scope assertion sentinel
// (#980): after the scope-only commit is created on the ScopeFiles path,
// CommitAndPush diffs the new commit against its parent and asserts every
// committed path matches the SAME declared-set matcher StageScoped staged
// with. A violation means the staging invariant broke — a path the drift
// report claims was excluded actually landed in the commit (run 4c2c6374's
// 21MB pre-staged binary) — so it fails loud naming each violating path,
// returned BEFORE the push (origin untouched, no broken branch, no PR).
// The runner classifies it category-B (artifact broken → re-scope/re-plan),
// symmetric with ErrCommitWouldNotCompile / ErrPushedTreeNotVerified. It is
// the commit-side complement of #960's tree-equivalence invariant: #960
// proves the pushed tree is the gate-verified tree, this proves the pushed
// commit's content is the declared scope and nothing else.
var ErrCommitOutOfScope = errors.New("gitops: commit contains out-of-scope paths")

// ErrCommittedTestsFailed is the test-gate analogue of
// ErrCommitWouldNotCompile (#800): the scope-only committed tree COMPILES
// (go vet passes) but a touched package's tests fail because a build- or
// test-required file was excluded as scope drift — e.g. a test fake/helper
// the plan didn't declare, present only in the agent's working tree, so the
// committed tree's tests are red while the working tree's are green
// (#780/#776). Like the compile gate it is returned BEFORE pushing, so the
// failure leaves origin untouched, and the runner classifies it as a
// category-B failure (wrong-shaped output → re-scope/re-plan).
var ErrCommittedTestsFailed = errors.New("gitops: committed tree tests failed")

// ErrPushedTreeNotVerified is the verified-SHA-invariant sentinel (#960):
// a VerifyCommit hook wraps it (via fmt.Errorf("%w: ...")) and returns it
// BEFORE the push when the staged commit's tree is NOT the tree the
// committed-tree verify gates (#651/#802) passed against and a single strict
// re-verify of the real committed HEAD did not explicitly pass. The gates
// verify a throwaway scope-only commit that is reset away; the real commit
// CommitAndPush builds later can differ (e.g. FreshFetchBase fetched a moved
// origin/<base> between gate and push), and without this check stage_state
// = succeeded would vouch for a pushed head no gate ever saw (run 07bce059).
// Returned before the push, so origin stays untouched; the runner classifies
// it category-B (artifact broken → re-scope/re-plan), symmetric with
// ErrCommitWouldNotCompile / ErrCommittedTestsFailed.
var ErrPushedTreeNotVerified = errors.New("gitops: pushed tree was not verified by the committed-tree gates")

// ErrBaseRebaseConflict is the sentinel returned when reapplying the agent's
// stashed working-tree edits onto a freshly-fetched authoritative base fails
// with a merge conflict — the agent edited lines the base advanced past, so the
// `git stash pop` onto the clean fetched base conflicts (#866, ADR-035
// follow-up). It is returned BEFORE any push (origin untouched), after the
// conflicted pop is aborted with `git reset --hard` (working tree reset to the
// clean fetched base; the stash entry is left in the stash list and recoverable
// via `git stash list`). The runner classifies it category-B (re-base/re-plan).
// Only a real pop CONFLICT trips this; an unrelated stash-pop failure keeps the
// prior generic git-error wrap. Recovery is a clean abort, not auto-resolution —
// auto-merging divergent edits would risk shipping an unreviewed (silent-bad)
// tree, the exact class the fail-loud path avoids.
var ErrBaseRebaseConflict = errors.New("gitops: stash pop conflicted reapplying agent edits onto fetched base")

// conflictCaptureByteCap bounds each captured conflict-context blob
// (ConflictHunks, StashPatch) on a BaseRebaseConflictError so a pathological
// diff cannot bloat the returned error or the re-invoke prompt built from it
// (#989).
const conflictCaptureByteCap = 64 << 10

// BaseRebaseConflictError is the typed form of ErrBaseRebaseConflict (#989):
// it carries the conflict context popStash captures during the clean abort so
// the runner can re-invoke the agent ONCE on the fresh base with both sides of
// the conflict in hand, instead of immediately failing the stage category-B.
// Unwrap returns the ErrBaseRebaseConflict sentinel, so every existing
// errors.Is classification keeps working unchanged. All context fields are
// BEST-EFFORT: a failed capture command degrades the field to empty without
// blocking or reordering the #866/#893 clean-abort sequence.
type BaseRebaseConflictError struct {
	// ConflictPaths are the repo-relative paths the conflicted pop left with
	// unmerged index entries (parsed from `git ls-files --unmerged`).
	ConflictPaths []string

	// ConflictHunks is the `git diff` of the half-applied conflicted pop —
	// the combined-diff representation carrying the conflict markers —
	// captured BETWEEN the unmerged probe and the `reset --hard` abort (the
	// only window the markers exist). Truncated to conflictCaptureByteCap.
	ConflictHunks string

	// StashPatch is the agent's stashed patch (`git stash show -p`), captured
	// AFTER the reset — a pop conflict preserves the stash entry. Truncated
	// to conflictCaptureByteCap.
	StashPatch string

	// popErr is the original `git stash pop` failure, preserved for the
	// error message.
	popErr error
}

func (e *BaseRebaseConflictError) Error() string {
	msg := ErrBaseRebaseConflict.Error() +
		": working tree reset to fetched base, stashed edits preserved (git stash list) — re-base the run or replan"
	if len(e.ConflictPaths) > 0 {
		msg += " (conflicted paths: " + strings.Join(e.ConflictPaths, ", ") + ")"
	}
	if e.popErr != nil {
		msg += ": " + e.popErr.Error()
	}
	return msg
}

// Unwrap returns the ErrBaseRebaseConflict sentinel so
// errors.Is(err, ErrBaseRebaseConflict) holds for the typed error.
func (*BaseRebaseConflictError) Unwrap() error { return ErrBaseRebaseConflict }

// DefaultAuthorName + DefaultAuthorEmail are the dev/CLI fallback bot
// identity used ONLY when the caller doesn't override — i.e. runs with no
// resolvable GitHub App (local-runner, CLI, dev). For App-backed runs the
// backend now resolves the App's own bot identity dynamically (App slug +
// bot user-id) and threads it through CommitAndPushArgs.AuthorName/Email,
// so attributed commits show `<slug>[bot]` rather than this constant (#722).
// Matches the pattern Actions uses for github-actions[bot] but with a
// Fishhawk slug so audit consumers can distinguish.
const (
	DefaultAuthorName  = "fishhawk-runner[bot]"
	DefaultAuthorEmail = "fishhawk-runner@users.noreply.github.com"
)

// Pusher executes git commands against a working tree. Cmd is
// overridable for tests so the helper can drive a fake git binary
// without standing up a real working copy.
type Pusher struct {
	// Binary is the git executable. Empty defaults to "git".
	Binary string

	// Cmd builds the *exec.Cmd. nil defaults to exec.CommandContext.
	Cmd func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// CommitAndPushArgs collects everything CommitAndPush needs.
type CommitAndPushArgs struct {
	// RepoDir is the git working directory the agent edited.
	RepoDir string

	// Branch is the branch name to create. Caller is responsible
	// for namespacing (e.g. "fishhawk/run-<id>/stage-<id>").
	Branch string

	// CommitMessage is used verbatim. Multi-line messages are
	// supported via embedded newlines.
	CommitMessage string

	// AuthorName / AuthorEmail override the default bot identity.
	// Empty values fall back to DefaultAuthorName / DefaultAuthorEmail.
	AuthorName  string
	AuthorEmail string

	// Remote is the git remote name. Empty defaults to DefaultRemote.
	Remote string

	// RemoteURL is the URL `git push` targets. Required.
	// Authentication is the caller's job — actions/checkout's
	// extraheader handles HTTPS auth in the canonical Actions
	// flow; for local-dev / bare-repo tests, file-path remotes
	// don't need auth at all.
	RemoteURL string

	// PushToken, when non-empty, is configured as the local
	// `http.<host>.extraheader` for an HTTPS RemoteURL immediately
	// before push, replacing any existing value (--replace-all).
	// Use this to refresh a stale extraheader from the workflow's
	// initial actions/checkout — App installation tokens have a
	// ~1-hour TTL and a long agent run can outlive the token that
	// was minted at workflow start.
	//
	// Empty value (the default) means "use ambient auth" — caller
	// trusts whatever extraheader the environment set up.
	PushToken string

	// ForceWithLease, when true, adds --force-with-lease to the push
	// so concurrent pushes to the shared branch are rejected rather
	// than silently overwritten. Used for decomposed-child runs.
	ForceWithLease bool

	// RebaseFromRemote, when true, fetches the remote branch and
	// rebases the local work on top instead of creating a new branch
	// with checkout -b. Used for subsequent decomposed-child runs
	// where the shared branch already exists on the remote.
	// Uncommitted agent edits are stashed before the fetch+rebase
	// and restored afterwards.
	RebaseFromRemote bool

	// FreshFetchBase, when non-empty AND RebaseFromRemote is false,
	// names the authoritative base branch (e.g. "main") the new run
	// branch must be cut from via a fresh fetch of origin/<base>
	// instead of the ambient local HEAD. It is the standalone
	// single-writer analogue of the decomposed-child RebaseFromRemote
	// fetch path: it reuses the same stash -> fetch -> checkout -B
	// FETCH_HEAD -> stash pop machinery so the agent's uncommitted
	// edits survive while only the base changes. It exists to prevent
	// a foreign commit another writer made in the same shared local
	// checkout (the #797 shape) from riding in as the run branch base.
	// On the FreshFetchBase path the recorded BaseSHA is re-captured
	// from the freshly-fetched tip, so the value the backend records as
	// the run's fork point (artifact base_sha) is the trustworthy
	// authoritative ref. This brings the local runner to the base
	// isolation GitHub Actions' actions/checkout already provides.
	// Empty (the default) keeps the unchanged `checkout -b` path, so
	// the decomposed-child and fix-up callers are unaffected (ADR-035,
	// #861).
	FreshFetchBase string

	// UpdateTrackingRef, when true, sets the local remote-tracking ref
	// refs/remotes/<remote>/<branch> to the pushed HEAD after a
	// successful push, so subsequent same-clone reads (decomposition
	// fan-out routing + policy base ref) observe the branch as present.
	// A URL push (git push <url> HEAD:<branch>) never creates or updates
	// that tracking ref on its own, so without this the next child in a
	// local fan-out mis-routes to the first-child `checkout -b` path
	// (#770). The remote branch tip equals the pushed HEAD, so the ref is
	// set deterministically with `git update-ref` (no fetch, no auth).
	UpdateTrackingRef bool

	// ScopeFiles is the approved plan's declared paths (#581). When
	// non-empty, CommitAndPush stages exactly these paths instead of
	// `git add -A`, and reports any dirty-but-undeclared paths via
	// CommitAndPushResult.ScopeDrift. When empty, staging falls back
	// to `git add -A` (preserves the plan_missing_for_implement
	// behavior). Paths are repo-relative, matching scope.files entries.
	ScopeFiles []string

	// VerifyCommit, when non-nil, is invoked AFTER the scope-only commit
	// is created and BEFORE the push, with the new HEAD SHA and the scope
	// drift (dirty-but-undeclared paths excluded from the commit). A
	// non-nil error aborts the push and is returned to the caller verbatim
	// — because no push has happened yet, a gate failure leaves origin
	// untouched (no broken branch, no PR). This is the compile-gate hook
	// for #728: the runner supplies a callback that compiles the committed
	// tree in an isolated worktree and returns ErrCommitWouldNotCompile
	// when build-required drift was dropped. Keeping the build logic in a
	// caller-supplied callback keeps gitops toolchain-agnostic. Never
	// invoked on the NoChanges short-circuit paths.
	VerifyCommit func(ctx context.Context, headSHA string, drift []string) error
}

// CommitAndPushResult captures the SHAs the runner needs to populate
// the pull-request artifact.
type CommitAndPushResult struct {
	// HeadSHA is the new branch tip after commit. Empty when
	// NoChanges is true.
	HeadSHA string

	// TreeSHA is the tree object hash of the pushed commit
	// (`git rev-parse HEAD^{tree}`), the content-addressed identity of
	// the exact snapshot that left the runner. Callers stamp it into
	// push/PR audit events so the trail proves the gates' verified tree
	// equals the pushed tree (#960). Empty when NoChanges is true.
	TreeSHA string

	// BaseSHA is the commit the branch was created from (i.e. HEAD
	// of the source branch immediately before our commit).
	BaseSHA string

	// NoChanges is true when the working tree had nothing staged.
	// The caller handles this as a workflow signal — typically a
	// policy_event in the trace and an early return — rather than
	// a hard failure. Branch is NOT created in this case.
	NoChanges bool

	// ScopeDrift lists dirty paths the working tree carried that were
	// NOT in the approved plan's scope.files (#581). These are
	// excluded from the commit — never staged — and surfaced for the
	// caller to flag (non-blocking, matching the spec's flag-only
	// scope-drift treatment, ADR-027). Always empty when ScopeFiles
	// was empty (the `git add -A` fallback path).
	ScopeDrift []string
}

// CommitAndPush configures a bot author, creates Branch, stages
// every change in RepoDir, commits with CommitMessage, and pushes
// to RemoteURL. Push authentication is the caller's responsibility
// (typically via actions/checkout's pre-set extraheader). Returns
// NoChanges=true with no other side effects when the working tree
// is clean.
func (p *Pusher) CommitAndPush(ctx context.Context, args CommitAndPushArgs) (*CommitAndPushResult, error) {
	if args.RepoDir == "" {
		return nil, errors.New("gitops: RepoDir required")
	}
	if args.Branch == "" {
		return nil, errors.New("gitops: Branch required")
	}
	if args.RemoteURL == "" {
		return nil, errors.New("gitops: RemoteURL required")
	}

	authorName := orDefault(args.AuthorName, DefaultAuthorName)
	authorEmail := orDefault(args.AuthorEmail, DefaultAuthorEmail)
	remote := orDefault(args.Remote, DefaultRemote)

	// Capture the base SHA before doing anything so the eventual
	// PR knows where the branch diverged.
	baseSHA, err := p.runOut(ctx, args.RepoDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("gitops: rev-parse HEAD: %w", err)
	}
	baseSHA = strings.TrimSpace(baseSHA)

	// Cheap "is the working tree dirty?" probe. `status --porcelain`
	// is empty when clean.
	status, err := p.runOut(ctx, args.RepoDir, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("gitops: status: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return &CommitAndPushResult{NoChanges: true, BaseSHA: baseSHA}, nil
	}

	// Configure author + email LOCALLY (`config --local`) so we
	// don't pollute any global gitconfig the customer's runner may
	// have. Local config dies with the working tree.
	if err := p.run(ctx, args.RepoDir, "config", "user.name", authorName); err != nil {
		return nil, fmt.Errorf("gitops: config user.name: %w", err)
	}
	if err := p.run(ctx, args.RepoDir, "config", "user.email", authorEmail); err != nil {
		return nil, fmt.Errorf("gitops: config user.email: %w", err)
	}

	if args.RebaseFromRemote {
		// Shared-branch path for subsequent decomposed children: the remote
		// branch already exists. Stash uncommitted agent edits, fetch the
		// remote tip over the run's own credential, reset the local branch
		// to it, restore the edits, then fall through to add+commit+push.
		//
		// The fetch targets args.RemoteURL (the authenticated HTTPS URL),
		// not the named remote — in the operator's checkout the named
		// `origin` is typically an SSH URL whose auth depends on the
		// operator's SSH agent, which may be unavailable (#772). Configure
		// the same extraheader the push uses BEFORE the fetch so the fetch
		// authenticates with the run's installation token.
		if err := p.configureExtraheader(ctx, args.RepoDir, args.RemoteURL, args.PushToken); err != nil {
			return nil, err
		}
		if err := p.run(ctx, args.RepoDir, "stash", "--include-untracked"); err != nil {
			return nil, fmt.Errorf("gitops: stash: %w", err)
		}
		// Fetch the remote branch tip into FETCH_HEAD. A URL fetch does not
		// create or update any refs/remotes/<name>/<branch> tracking ref, so
		// the subsequent checkout references FETCH_HEAD explicitly.
		if err := p.run(ctx, args.RepoDir, "fetch", args.RemoteURL, args.Branch); err != nil {
			return nil, fmt.Errorf("gitops: fetch %s: %w", args.Branch, err)
		}
		// Create/reset the local branch to the fetched remote tip. The agent's
		// edits are stashed (uncommitted), so there are no local commits to
		// rebase — this is equivalent to the prior fetch+checkout+pull --rebase
		// with the branch fast-forwarded to the remote tip.
		if err := p.run(ctx, args.RepoDir, "checkout", "-B", args.Branch, "FETCH_HEAD"); err != nil {
			return nil, fmt.Errorf("gitops: checkout %s: %w", args.Branch, err)
		}
		if err := p.popStash(ctx, args.RepoDir); err != nil {
			return nil, err
		}
	} else if args.FreshFetchBase != "" {
		// Standalone single-writer path (#861, ADR-035 prevention): cut the
		// run branch from a freshly-fetched authoritative base instead of the
		// ambient local HEAD, so a foreign commit another writer made in the
		// same shared checkout (the #797 shape) cannot become the branch base.
		// Mirrors the RebaseFromRemote machinery above: authenticate the fetch
		// over RemoteURL (sidestepping a broken ambient origin, #772), stash
		// the agent's uncommitted scope edits, fetch the base tip into
		// FETCH_HEAD, cut the branch from that fetched tip, then reapply the
		// edits onto the clean base. A stash-pop conflict (agent edited lines
		// the base advanced past) is detected specifically by popStash and
		// surfaces as ErrBaseRebaseConflict (category-B) after a clean
		// reset --hard abort, with no push and never a silent bad tree.
		if err := p.configureExtraheader(ctx, args.RepoDir, args.RemoteURL, args.PushToken); err != nil {
			return nil, err
		}
		if err := p.run(ctx, args.RepoDir, "stash", "--include-untracked"); err != nil {
			return nil, fmt.Errorf("gitops: stash: %w", err)
		}
		// Fetch the authoritative base branch tip into FETCH_HEAD. A URL fetch
		// does not create a refs/remotes tracking ref, so the checkout
		// references FETCH_HEAD explicitly.
		if err := p.run(ctx, args.RepoDir, "fetch", args.RemoteURL, args.FreshFetchBase); err != nil {
			return nil, fmt.Errorf("gitops: fetch %s: %w", args.FreshFetchBase, err)
		}
		if err := p.run(ctx, args.RepoDir, "checkout", "-B", args.Branch, "FETCH_HEAD"); err != nil {
			return nil, fmt.Errorf("gitops: checkout %s: %w", args.Branch, err)
		}
		if err := p.popStash(ctx, args.RepoDir); err != nil {
			return nil, err
		}
		// Re-capture baseSHA from the freshly-fetched tip (now HEAD) so the
		// recorded fork point reflects the trustworthy authoritative ref rather
		// than the stale ambient HEAD captured before the fetch. Scoped to this
		// path only — the RebaseFromRemote and plain checkout -b paths keep
		// their BaseSHA semantics unchanged.
		fetchedBase, err := p.runOut(ctx, args.RepoDir, "rev-parse", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("gitops: rev-parse fetched base: %w", err)
		}
		baseSHA = strings.TrimSpace(fetchedBase)
	} else {
		if err := p.run(ctx, args.RepoDir, "checkout", "-b", args.Branch); err != nil {
			return nil, fmt.Errorf("gitops: checkout -b %s: %w", args.Branch, err)
		}
	}
	// Scope-bounded staging (#581): when the approved plan declared a
	// scope.files set, stage exactly those paths and exclude any
	// dirty-but-undeclared files (recorded as drift). Otherwise fall
	// back to `git add -A` — the plan_missing_for_implement path where
	// we have no scope to bound against.
	var scopeDrift []string
	if len(args.ScopeFiles) > 0 {
		drift, err := p.StageScoped(ctx, args.RepoDir, args.ScopeFiles)
		if err != nil {
			return nil, err
		}
		scopeDrift = drift
		// All dirty files were out of scope → nothing staged. Treat as
		// NoChanges rather than letting `git commit` fail with "nothing
		// to commit"; the caller logs the empty-changes signal (and the
		// drift) instead of a hard error.
		staged, err := p.runOut(ctx, args.RepoDir, "diff", "--cached", "--name-only")
		if err != nil {
			return nil, fmt.Errorf("gitops: diff --cached: %w", err)
		}
		if strings.TrimSpace(staged) == "" {
			return &CommitAndPushResult{NoChanges: true, BaseSHA: baseSHA, ScopeDrift: scopeDrift}, nil
		}
	} else if err := p.run(ctx, args.RepoDir, "add", "-A"); err != nil {
		return nil, fmt.Errorf("gitops: add: %w", err)
	}
	if err := p.run(ctx, args.RepoDir, "commit", "--signoff", "-m", args.CommitMessage); err != nil {
		return nil, fmt.Errorf("gitops: commit: %w", err)
	}

	headSHA, err := p.runOut(ctx, args.RepoDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("gitops: rev-parse new HEAD: %w", err)
	}
	headSHA = strings.TrimSpace(headSHA)

	// Resolve the commit's tree object hash (#960) so the caller can
	// stamp the pushed snapshot's content identity into the audit chain.
	treeSHA, err := p.runOut(ctx, args.RepoDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, fmt.Errorf("gitops: rev-parse new HEAD tree: %w", err)
	}
	treeSHA = strings.TrimSpace(treeSHA)

	// Post-commit out-of-scope assertion (#980), ScopeFiles path only (the
	// add -A fallback has no scope to bound against). StageScoped's mixed
	// reset makes a violation structurally impossible, so this is the
	// fail-loud backstop proving the drift report and the commit's actual
	// content agree — returned BEFORE the push and before the (more
	// expensive) VerifyCommit gates, so origin stays untouched.
	if len(args.ScopeFiles) > 0 {
		if err := p.assertCommitInScope(ctx, args.RepoDir, headSHA, args.ScopeFiles); err != nil {
			return nil, err
		}
	}

	// Compile-gate hook (#728): verify the scope-only committed tree
	// before any push. Runs after the commit exists (so headSHA is real
	// and checkoutable in an isolated worktree) but before the push, so a
	// gate failure leaves origin untouched. scopeDrift names the files the
	// commit excluded — the candidate build-required set the callback
	// reports in its error.
	if args.VerifyCommit != nil {
		if err := args.VerifyCommit(ctx, headSHA, scopeDrift); err != nil {
			return nil, err
		}
	}

	// Refresh the local extraheader if the caller supplied a fresh
	// PushToken. This is the long-running-stage path: the workflow's
	// initial actions/checkout set an extraheader with the auth
	// pre-step's token, but App installation tokens are ~1-hour
	// TTL — agents that take >55min outlive the original. The
	// runner pre-fetches a fresh token and hands it here so the
	// push always authenticates with a non-expired credential.
	if err := p.configureExtraheader(ctx, args.RepoDir, args.RemoteURL, args.PushToken); err != nil {
		return nil, err
	}

	pushArgs := []string{"push", args.RemoteURL, fmt.Sprintf("HEAD:%s", args.Branch)}
	if args.ForceWithLease {
		// A *bare* --force-with-lease compares the push against the local
		// remote-tracking ref, but git cannot associate that ref with a URL
		// push (we push to RemoteURL, not the remote name), so a bare lease
		// on a subsequent shared-branch push always rejects with
		// `(stale info)` regardless of the ref's value (#767). When the
		// tracking ref exists — a subsequent decomposed child, kept current
		// by this function's post-push update-ref — pass it as the explicit
		// lease expected-value so the lease is honored against the URL push.
		// When it is absent (first child / brand-new branch) the bare form
		// correctly permits the create.
		lease := "--force-with-lease"
		trackingRef := fmt.Sprintf("refs/remotes/%s/%s", remote, args.Branch)
		if track, err := p.runOut(ctx, args.RepoDir, "rev-parse", "--verify", "--quiet", trackingRef); err == nil {
			if sha := strings.TrimSpace(track); sha != "" {
				lease = fmt.Sprintf("--force-with-lease=%s:%s", args.Branch, sha)
			}
		}
		pushArgs = append(pushArgs, lease)
	}
	if err := p.run(ctx, args.RepoDir, pushArgs...); err != nil {
		return nil, fmt.Errorf("gitops: push %s: %w", remote, err)
	}

	// Materialize the local remote-tracking ref to the just-pushed HEAD
	// (#770). A URL push doesn't update refs/remotes/<remote>/<branch>, so
	// without this the next decomposed child reading that ref sees the
	// shared branch as absent and mis-routes. The remote branch tip equals
	// headSHA because we just pushed local HEAD to it, so this is exact
	// with no fetch and no remote auth.
	if args.UpdateTrackingRef {
		trackingRef := fmt.Sprintf("refs/remotes/%s/%s", remote, args.Branch)
		if err := p.run(ctx, args.RepoDir, "update-ref", trackingRef, headSHA); err != nil {
			return nil, fmt.Errorf("gitops: update tracking ref %s: %w", trackingRef, err)
		}
	}

	return &CommitAndPushResult{
		HeadSHA:    headSHA,
		TreeSHA:    treeSHA,
		BaseSHA:    baseSHA,
		ScopeDrift: scopeDrift,
	}, nil
}

// StageScoped stages exactly the declared scope paths and returns the
// set of dirty-but-undeclared paths (scope drift). It first runs a mixed
// `git reset -q` (whole-tree, index → HEAD, working tree untouched) so the
// index is the single source of truth (#980), then reads `git status
// --porcelain -uall` to enumerate every dirty path, stages the ones that
// match a declared path via a single `git add -A -- <paths>` (per-path
// -A covers create, modify, AND delete), and returns the remainder as
// drift. Drift paths are never staged; declared paths that are clean
// are a no-op. Staging only the paths git already reports dirty means
// `git add` never errors on a pathspec that matches nothing.
//
// Index-authority postcondition: on return the index equals HEAD plus
// exactly the declared dirty paths. Without the reset, a file the agent
// pre-staged with its own `git add` was never unstaged — `git commit`
// commits the index, so the pre-staged out-of-scope file landed in the
// commit while the drift report claimed it was excluded (#980, run
// 4c2c6374's 21MB binary). The reset also makes a pre-staged net-new file
// untracked again, restoring the #818 created-out-of-scope gate's reach:
// `git ls-files --others` (UntrackedPaths) excludes index-resident files,
// so a staged net-new file was invisible to the gate.
//
// -uall (--untracked-files=all) enumerates untracked files inside a
// brand-new directory individually rather than collapsing them to a
// single directory entry (e.g. `?? backend/internal/budget/`), which
// matches no file-level scope.files entry and would be misclassified as
// drift, leaving the declared file unstaged (#691).
func (p *Pusher) StageScoped(ctx context.Context, repoDir string, scopeFiles []string) (drift []string, err error) {
	if err := p.run(ctx, repoDir, "reset", "-q"); err != nil {
		return nil, fmt.Errorf("gitops: reset index: %w", err)
	}
	status, err := p.runOut(ctx, repoDir, "status", "--porcelain", "-uall")
	if err != nil {
		return nil, fmt.Errorf("gitops: status: %w", err)
	}
	matcher := newScopeMatcher(scopeFiles)
	var toStage []string
	for _, line := range strings.Split(status, "\n") {
		path := porcelainPath(line)
		if path == "" {
			continue
		}
		if matcher.matches(path) {
			toStage = append(toStage, path)
		} else {
			drift = append(drift, path)
		}
	}
	if len(toStage) > 0 {
		addArgs := append([]string{"add", "-A", "--"}, toStage...)
		if err := p.run(ctx, repoDir, addArgs...); err != nil {
			return nil, fmt.Errorf("gitops: add scoped: %w", err)
		}
	}
	return drift, nil
}

// popStash reapplies the stashed agent edits onto the freshly-checked-out base
// via `git stash pop`. On success it returns nil. On a non-zero pop it probes
// for a real merge conflict with `git ls-files --unmerged` (a pop CONFLICT
// leaves unmerged index entries at stages 1/2/3; a non-conflict failure does
// not). When conflicted it aborts the half-applied pop with `git reset --hard`
// — restoring the working tree to the clean fetched base (HEAD) without touching
// the stash stack, so the stashed edits stay recoverable via `git stash list`
// (a pop conflict does not drop the stash) — and returns a typed
// *BaseRebaseConflictError carrying the best-effort-captured conflict context
// (paths, conflict-marker hunks, stash patch) that unwraps to
// ErrBaseRebaseConflict (#989).
// A non-conflict pop failure returns the original wrapped git error unchanged,
// preserving today's generic fail-loud behavior for that case.
//
// If conflict detection ITSELF fails (the `git ls-files --unmerged` probe errors
// — corrupt git state, permission error), popStash still attempts a best-effort
// `git reset --hard` before returning so the clean-abort guarantee holds even
// when the conflict cannot be confirmed: a partially-applied conflicted pop must
// not be left in the working tree (#893). The reset's own failure is tolerated
// and annotated rather than masking the primary pop failure; the branch returns a
// non-nil fail-loud error in every sub-case (no push), and is NOT
// ErrBaseRebaseConflict because the conflict was never confirmed.
func (p *Pusher) popStash(ctx context.Context, repoDir string) error {
	popErr := p.run(ctx, repoDir, "stash", "pop")
	if popErr == nil {
		return nil
	}
	unmerged, lsErr := p.runOut(ctx, repoDir, "ls-files", "--unmerged")
	if lsErr != nil {
		// Conflict detection itself failed (corrupt git state, permission
		// error). The pop may have partially applied, so attempt a best-effort
		// `git reset --hard` to honour the clean-abort invariant even though the
		// conflict could not be confirmed (#893). Tolerate and annotate the
		// reset's own failure rather than masking the primary pop failure: a
		// failed reset leaves the tree for manual recovery but is surfaced
		// explicitly. The branch returns a non-nil fail-loud error in every
		// case — no push — and is NOT ErrBaseRebaseConflict (conflict unconfirmed).
		if resetErr := p.run(ctx, repoDir, "reset", "--hard"); resetErr != nil {
			return fmt.Errorf("gitops: stash pop (conflict detection via ls-files failed: %v; best-effort reset --hard also failed: %v): %w", lsErr, resetErr, popErr)
		}
		return fmt.Errorf("gitops: stash pop (conflict detection via ls-files failed: %v; working tree reset to fetched base): %w", lsErr, popErr)
	}
	if strings.TrimSpace(unmerged) == "" {
		// Not a merge conflict — some other stash-pop failure. Preserve the
		// prior generic behavior.
		return fmt.Errorf("gitops: stash pop: %w", popErr)
	}
	// Real pop conflict: abort the half-applied pop so the working tree returns
	// to the clean fetched base; the stash entry is left intact and recoverable.
	//
	// Capture the conflict context BEST-EFFORT for the runner's bounded
	// re-invoke (#989), never blocking or reordering the clean-abort sequence
	// (#866/#893 invariants): the conflicted paths come from the unmerged
	// listing already in hand; the conflict-marker hunks only exist between
	// this probe and the `reset --hard` abort, so `git diff` is read here; the
	// stash patch is read after the reset (a pop conflict preserves the stash
	// entry). Any capture command failing degrades its field to empty — the
	// abort still runs and the returned error still unwraps to
	// ErrBaseRebaseConflict.
	conflictPaths := unmergedConflictPaths(unmerged)
	var conflictHunks string
	if d, derr := p.runOut(ctx, repoDir, "diff"); derr == nil {
		conflictHunks = truncateCapture(d)
	}
	if err := p.run(ctx, repoDir, "reset", "--hard"); err != nil {
		return fmt.Errorf("gitops: reset --hard after conflicted stash pop: %w", err)
	}
	var stashPatch string
	if sp, serr := p.runOut(ctx, repoDir, "stash", "show", "-p", "stash@{0}"); serr == nil {
		stashPatch = truncateCapture(sp)
	}
	return &BaseRebaseConflictError{
		ConflictPaths: conflictPaths,
		ConflictHunks: conflictHunks,
		StashPatch:    stashPatch,
		popErr:        popErr,
	}
}

// unmergedConflictPaths parses `git ls-files --unmerged` output (lines of
// `<mode> <sha> <stage>\t<path>`, up to three stage entries per conflicted
// path) into a deduplicated, order-preserving path list.
func unmergedConflictPaths(unmerged string) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, line := range strings.Split(unmerged, "\n") {
		_, path, ok := strings.Cut(line, "\t")
		if !ok || path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths
}

// truncateCapture bounds a captured conflict-context blob to
// conflictCaptureByteCap, annotating the cut so a truncated capture is
// distinguishable from a complete one.
func truncateCapture(s string) string {
	if len(s) <= conflictCaptureByteCap {
		return s
	}
	return s[:conflictCaptureByteCap] + "\n… [truncated]"
}

// scopeMatcher is the single source of truth for "does this repo-relative
// path match the declared scope.files set" (#980). StageScoped partitions
// dirty paths with it, and assertCommitInScope re-checks the committed
// paths against it, so staging and the post-commit assertion can never
// disagree on what "in scope" means. Declared entries split into exact-match
// paths and trailing-slash directory prefixes: a trailing-slash entry (#824)
// is a folded directory — every created file beneath it matches — while a
// plain entry stays exact-match, so a regular file never prefix-matches a
// sibling (foo/bar.go must never match foo/bar.go.bak).
type scopeMatcher struct {
	declared    map[string]bool
	dirPrefixes []string
}

func newScopeMatcher(scopeFiles []string) scopeMatcher {
	m := scopeMatcher{declared: make(map[string]bool, len(scopeFiles))}
	for _, f := range scopeFiles {
		if strings.HasSuffix(f, "/") {
			m.dirPrefixes = append(m.dirPrefixes, f)
			continue
		}
		m.declared[f] = true
	}
	return m
}

func (m scopeMatcher) matches(path string) bool {
	return m.declared[path] || hasDirPrefix(path, m.dirPrefixes)
}

// assertCommitInScope enforces the post-commit out-of-scope assertion
// (#980): every path the commit changed relative to its parent must match
// the declared scope set. It runs `git diff-tree -r -z --no-commit-id
// --name-only <sha>` — the commit produced by the staging path is always
// parented (CommitAndPush captures a pre-existing HEAD as BaseSHA before
// committing), so diff-tree lists its changes against that parent; -z
// yields NUL-separated unquoted paths, immune to core.quotePath quoting.
// Any committed path outside the matcher returns ErrCommitOutOfScope
// naming each violating path; callers invoke it BEFORE the push so a
// violation leaves origin untouched.
func (p *Pusher) assertCommitInScope(ctx context.Context, repoDir, headSHA string, scopeFiles []string) error {
	out, err := p.runOut(ctx, repoDir, "diff-tree", "-r", "-z", "--no-commit-id", "--name-only", headSHA)
	if err != nil {
		return fmt.Errorf("gitops: diff-tree %s: %w", headSHA, err)
	}
	matcher := newScopeMatcher(scopeFiles)
	var violations []string
	for _, path := range strings.Split(out, "\x00") {
		if path == "" {
			continue
		}
		if !matcher.matches(path) {
			violations = append(violations, path)
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("%w: commit %s contains %d path(s) outside the declared scope.files: %s",
			ErrCommitOutOfScope, headSHA, len(violations), strings.Join(violations, ", "))
	}
	return nil
}

// hasDirPrefix reports whether path lies under any of the trailing-slash
// directory prefixes (#824). `git status --porcelain -uall` lists each
// created file by its full repo-relative path, so a file inside a folded
// directory (e.g. `corpus/newcase/`) surfaces as `corpus/newcase/x.json` and
// matches the prefix. An empty prefix list short-circuits to false.
func hasDirPrefix(path string, dirPrefixes []string) bool {
	for _, prefix := range dirPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// UntrackedPaths returns the subset of candidates that git reports as
// untracked (created, non-gitignored) in repoDir — i.e. brand-new files
// the working tree carries that are not in the index. It runs
// `git ls-files --others --exclude-standard` (the supported enumeration of
// untracked, non-ignored files) and intersects the result with candidates.
//
// This isolates the created-vs-modified distinction the fix-up scope gate
// (#818) needs: a modified-but-out-of-scope tracked file is flag-only drift
// (ADR-027), but a CREATED out-of-scope file would be silently stripped from a
// fix-up's scope-only commit while the in-scope change that references it lands.
// It is a package-level function (not a *Pusher method) because the caller is a
// VerifyCommit closure with no *Pusher in scope, matching the package-level
// gate helpers.
func UntrackedPaths(ctx context.Context, repoDir string, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	out, err := (&Pusher{}).runOut(ctx, repoDir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("gitops: ls-files --others: %w", err)
	}
	untracked := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			untracked[p] = true
		}
	}
	var created []string
	for _, c := range candidates {
		if untracked[c] {
			created = append(created, c)
		}
	}
	return created, nil
}

// CaptureHead records the operator's current checkout position so it can be
// restored after an implement/fix-up pass moves HEAD onto the run branch
// (#911). It returns the short branch name on an attached HEAD, or the raw
// commit SHA with detached=true on a detached HEAD (the hosted
// actions/checkout shape, where the workflow checks out a SHA rather than a
// branch). Either value is a valid restore target for `git checkout --force`.
//
// `git symbolic-ref --quiet --short HEAD` prints the branch short name and
// exits 0 on an attached HEAD; on a detached HEAD it exits non-zero (with
// --quiet suppressing the error text), which is how the detached case is
// detected. It is a package-level function (not a *Pusher method) to mirror
// UntrackedPaths — the caller has no *Pusher in scope.
func CaptureHead(ctx context.Context, repoDir string) (ref string, detached bool, err error) {
	out, serr := (&Pusher{}).runOut(ctx, repoDir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if serr == nil {
		if branch := strings.TrimSpace(out); branch != "" {
			return branch, false, nil
		}
	}
	// Detached HEAD (or symbolic-ref produced no name): fall back to the SHA.
	sha, rerr := (&Pusher{}).runOut(ctx, repoDir, "rev-parse", "HEAD")
	if rerr != nil {
		return "", false, fmt.Errorf("gitops: capture HEAD: %w", rerr)
	}
	return strings.TrimSpace(sha), true, nil
}

// CheckoutRemoteBranch fetches refs/heads/<branch> from the named remote
// and checks the local working tree out onto it, returning the fetched tip
// SHA so the caller can run the ADR-035 lineage comparison against the
// stage's recorded head (#967). It is the fix-up pass's base-establishment
// primitive: instead of inheriting the operator's incidental checkout, the
// runner pins the working tree to the live PR-branch tip before the agent
// is invoked.
//
// The fetch uses an explicit refspec `+refs/heads/<branch>:<trackingRef>`
// where trackingRef is refs/remotes/<remote>/<branch> derived from the SAME
// remote the fetch targets — fetch source and compared/checked-out ref stay
// in lockstep by construction, never a hard-coded "origin" against a
// different remote. Per git-fetch(1), a refspec given on the command line
// overrides the configured fetch refspecs and an explicit destination ref
// is updated directly (the leading + forces a non-fast-forward update), so
// the tracking ref deterministically holds the fetched tip regardless of
// the repo's fetch config. The checkout is `checkout -B <branch>
// <trackingRef>`, resetting any stale local branch of the same name to the
// fetched tip.
//
// Auth is ambient: the fetch targets the named remote (default
// DefaultRemote), using whatever credentials the operator's checkout
// already has for it — the pre-invoke call site has no installation token
// yet (it is minted post-invoke on the push path). Like CaptureHead /
// RestoreHead it is a package-level function because the caller has no
// *Pusher in scope.
func CheckoutRemoteBranch(ctx context.Context, repoDir, remote, branch string) (tipSHA string, err error) {
	if branch == "" {
		return "", errors.New("gitops: branch required")
	}
	remote = orDefault(remote, DefaultRemote)
	trackingRef := fmt.Sprintf("refs/remotes/%s/%s", remote, branch)
	p := &Pusher{}
	if err := p.run(ctx, repoDir, "fetch", remote,
		fmt.Sprintf("+refs/heads/%s:%s", branch, trackingRef)); err != nil {
		return "", fmt.Errorf("gitops: fetch %s from %s: %w", branch, remote, err)
	}
	tip, err := p.runOut(ctx, repoDir, "rev-parse", trackingRef)
	if err != nil {
		return "", fmt.Errorf("gitops: rev-parse %s: %w", trackingRef, err)
	}
	if err := p.run(ctx, repoDir, "checkout", "-B", branch, trackingRef); err != nil {
		return "", fmt.Errorf("gitops: checkout -B %s %s: %w", branch, trackingRef, err)
	}
	return strings.TrimSpace(tip), nil
}

// RestoreHead returns the operator's checkout to ref via `git checkout
// --force` (#911). After an implement/fix-up pass the runner has switched
// HEAD onto the run branch (via CommitAndPush's `checkout -b`/`-B`), and on a
// failed pass the tree is also dirty (staged+unstaged tracked modifications).
// --force is required so the switch is not refused by those modifications: it
// discards staged and unstaged changes to TRACKED files and moves HEAD off the
// run branch. A committed run-branch commit is NOT lost — the run branch ref
// still points at it (and on the success path it was already pushed to
// origin); restoring only moves HEAD off the branch.
//
// Untracked files are intentionally left in place: `git checkout --force` does
// not remove them, and a `git clean` to do so would risk deleting operator
// files the run never touched. The caller invokes this best-effort and
// log-only — a restore failure must never override the push's primary outcome.
func RestoreHead(ctx context.Context, repoDir, ref string) error {
	if err := (&Pusher{}).run(ctx, repoDir, "checkout", "--force", ref); err != nil {
		return fmt.Errorf("gitops: restore HEAD to %s: %w", ref, err)
	}
	return nil
}

// DirtyPaths enumerates every dirty repo-relative path in repoDir — staged,
// unstaged, and untracked alike — via `git status --porcelain -uall`. -uall
// lists untracked files inside a brand-new directory individually rather than
// collapsing them to one directory entry, the same per-file enumeration
// rationale as StageScoped (#691): the caller compares these paths against
// per-file drift lists, so a folded directory entry would never match. The
// runner captures this snapshot BEFORE the agent is invoked (#943) so the
// post-stage drift cleanup can tell operator pre-existing edits apart from
// agent-introduced drift. It is a package-level function (not a *Pusher
// method) to mirror CaptureHead / UntrackedPaths — the run() call site has no
// *Pusher in scope.
func DirtyPaths(ctx context.Context, repoDir string) ([]string, error) {
	out, err := (&Pusher{}).runOut(ctx, repoDir, "status", "--porcelain", "-uall")
	if err != nil {
		return nil, fmt.Errorf("gitops: status: %w", err)
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if p := porcelainPath(line); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// CleanDriftPaths reverts the given drift paths via a pathspec-limited
// `git stash push --include-untracked -- <paths...>` followed by `git stash
// drop` (#943). One mechanism covers tracked-modified, tracked-deleted, and
// untracked drift in a single step; --include-untracked extends the pathspec
// to untracked files (git >= 2.13). When the named paths turn out to be clean
// the push is a "No local changes" no-op (exit 0, no stash entry created) and
// that is treated as success — the entry-created probe below guards the drop
// so a pre-existing operator stash entry is never destroyed. If the drop
// fails, the entry is left on the stash stack (recoverable via `git stash
// list`) and an annotated error is returned for the caller to log.
//
// Known limitation: a rename's source-path deletion is not in the drift list
// (porcelainPath returns the destination), so a renamed-as-drift source may
// stay dirty — rare, flag-only, matching ADR-027's non-blocking drift posture.
func CleanDriftPaths(ctx context.Context, repoDir string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	p := &Pusher{}
	created, err := stashPushPaths(ctx, p, repoDir, "fishhawk: drift-excluded paths", paths)
	if err != nil {
		return fmt.Errorf("gitops: stash drift paths: %w", err)
	}
	if !created {
		// The named paths were already clean — nothing to revert.
		return nil
	}
	if err := p.run(ctx, repoDir, "stash", "drop"); err != nil {
		return fmt.Errorf("gitops: drop drift stash (entry left recoverable via `git stash list`): %w", err)
	}
	return nil
}

// RestoreHeadPreserving is RestoreHead with a preserved-paths carve-out
// (#943): the named paths — operator edits that pre-dated the agent — are
// stashed before the forced checkout (which would otherwise discard tracked
// modifications) and reapplied after it. An empty preserve set delegates to
// RestoreHead unchanged. The reapply reuses popStash's #989 conflict
// handling: a pop conflict aborts cleanly (reset --hard), leaves the stash
// entry intact and recoverable via `git stash list`, and returns a typed
// error for the caller to log — operator content is never silently
// destroyed. Like RestoreHead, callers invoke this best-effort and log-only.
func RestoreHeadPreserving(ctx context.Context, repoDir, ref string, preserve []string) error {
	if len(preserve) == 0 {
		return RestoreHead(ctx, repoDir, ref)
	}
	p := &Pusher{}
	created, err := stashPushPaths(ctx, p, repoDir, "fishhawk: operator pre-existing edits", preserve)
	if err != nil {
		return fmt.Errorf("gitops: stash operator edits: %w", err)
	}
	if err := p.run(ctx, repoDir, "checkout", "--force", ref); err != nil {
		if created {
			return fmt.Errorf("gitops: restore HEAD to %s (operator edits stashed; recover via `git stash pop`): %w", ref, err)
		}
		return fmt.Errorf("gitops: restore HEAD to %s: %w", ref, err)
	}
	if !created {
		return nil
	}
	if err := p.popStash(ctx, repoDir); err != nil {
		return fmt.Errorf("gitops: reapply operator edits after restore (stash entry recoverable via `git stash list`): %w", err)
	}
	return nil
}

// stashPushPaths runs a pathspec-limited `git stash push --include-untracked`
// and reports whether an entry was actually created. The created probe
// (refs/stash before vs after) is load-bearing: stash push exits 0 with "No
// local changes to save" when the named paths are clean, and a blind follow-up
// drop/pop would then destroy or replay a PRE-EXISTING stash entry the
// operator owns.
func stashPushPaths(ctx context.Context, p *Pusher, repoDir, message string, paths []string) (created bool, err error) {
	before := stashTip(ctx, p, repoDir)
	args := append([]string{"stash", "push", "--include-untracked", "-m", message, "--"}, paths...)
	if err := p.run(ctx, repoDir, args...); err != nil {
		return false, err
	}
	after := stashTip(ctx, p, repoDir)
	return after != "" && after != before, nil
}

// stashTip resolves refs/stash, returning "" when the stash stack is empty.
func stashTip(ctx context.Context, p *Pusher, repoDir string) string {
	out, err := p.runOut(ctx, repoDir, "rev-parse", "--verify", "--quiet", "refs/stash")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// porcelainPath extracts the repo-relative path from one `git status
// --porcelain` line. Returns "" for blank/short lines. For a rename
// ("R  old -> new") it returns the destination path, which is what a
// plan declares for a rename. Surrounding quotes (core.quotePath, used
// for paths with special characters) are stripped.
func porcelainPath(line string) string {
	// Minimum line is "XY p": two status codes, a space, one path char.
	if len(line) < 4 {
		return ""
	}
	rest := line[3:]
	if idx := strings.Index(rest, " -> "); idx >= 0 {
		rest = rest[idx+len(" -> "):]
	}
	return strings.Trim(rest, `"`)
}

// configureExtraheader sets the local `http.<host>.extraheader` to a Basic
// auth header derived from pushToken, so subsequent git operations (fetch and
// push) over an HTTPS remoteURL authenticate with the run's installation
// token rather than the operator's ambient credentials. The token never
// appears on the command line — it's written into the extraheader, preserving
// the no-URL-embedded-credential discipline.
//
// It no-ops when pushToken is empty (ambient-auth path) or remoteURL is not
// HTTPS (file-path bare-repo tests, SSH). `--replace-all` overwrites any
// existing single value rather than appending, so it is idempotent and safe to
// call both before the rebase fetch and before the push without producing the
// duplicate-Authorization-header failure of #199 / #200.
func (p *Pusher) configureExtraheader(ctx context.Context, repoDir, remoteURL, pushToken string) error {
	if pushToken == "" || !strings.HasPrefix(remoteURL, "https://") {
		return nil
	}
	host, err := pushHost(remoteURL)
	if err != nil {
		return fmt.Errorf("gitops: parse remote URL: %w", err)
	}
	header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+pushToken))
	if err := p.run(ctx, repoDir, "config", "--local", "--replace-all",
		"http."+host+".extraheader", header); err != nil {
		return fmt.Errorf("gitops: refresh extraheader: %w", err)
	}
	return nil
}

// pushHost extracts the `<scheme>://<host>/` string git config keys
// scope extraheader to. Mirrors actions/checkout's convention exactly
// so a `--replace-all` overwrites the existing entry rather than
// appending a duplicate.
func pushHost(remoteURL string) (string, error) {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host + "/", nil
}

// run invokes git with cwd=dir, returning a wrapped error including
// stderr on failure so callers can see what git complained about.
func (p *Pusher) run(ctx context.Context, dir string, gitArgs ...string) error {
	_, err := p.runOut(ctx, dir, gitArgs...)
	return err
}

// runOut invokes git with cwd=dir and returns the captured stdout
// trimmed of trailing newlines. stderr is folded into the returned
// error on non-zero exit.
func (p *Pusher) runOut(ctx context.Context, dir string, gitArgs ...string) (string, error) {
	binary := p.Binary
	if binary == "" {
		binary = "git"
	}
	cmdFn := p.Cmd
	if cmdFn == nil {
		cmdFn = exec.CommandContext
	}
	cmd := cmdFn(ctx, binary, gitArgs...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)",
			strings.Join(gitArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
