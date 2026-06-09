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
		BaseSHA:    baseSHA,
		ScopeDrift: scopeDrift,
	}, nil
}

// StageScoped stages exactly the declared scope paths and returns the
// set of dirty-but-undeclared paths (scope drift). It reads `git status
// --porcelain -uall` to enumerate every dirty path, stages the ones that
// match a declared path via a single `git add -A -- <paths>` (per-path
// -A covers create, modify, AND delete), and returns the remainder as
// drift. Drift paths are never staged; declared paths that are clean
// are a no-op. Staging only the paths git already reports dirty means
// `git add` never errors on a pathspec that matches nothing.
//
// -uall (--untracked-files=all) enumerates untracked files inside a
// brand-new directory individually rather than collapsing them to a
// single directory entry (e.g. `?? backend/internal/budget/`), which
// matches no file-level scope.files entry and would be misclassified as
// drift, leaving the declared file unstaged (#691).
func (p *Pusher) StageScoped(ctx context.Context, repoDir string, scopeFiles []string) (drift []string, err error) {
	status, err := p.runOut(ctx, repoDir, "status", "--porcelain", "-uall")
	if err != nil {
		return nil, fmt.Errorf("gitops: status: %w", err)
	}
	// Split declared entries into exact-match paths and directory prefixes.
	// A trailing-slash entry (#824) is a folded directory: every created
	// file beneath it should stage. A plain entry stays exact-match — a
	// regular file must not prefix-match a sibling (foo/bar.go must never
	// stage foo/bar.go.bak).
	declared := make(map[string]bool, len(scopeFiles))
	var dirPrefixes []string
	for _, f := range scopeFiles {
		if strings.HasSuffix(f, "/") {
			dirPrefixes = append(dirPrefixes, f)
			continue
		}
		declared[f] = true
	}
	var toStage []string
	for _, line := range strings.Split(status, "\n") {
		path := porcelainPath(line)
		if path == "" {
			continue
		}
		if declared[path] || hasDirPrefix(path, dirPrefixes) {
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
// (a pop conflict does not drop the stash) — and returns ErrBaseRebaseConflict.
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
	if err := p.run(ctx, repoDir, "reset", "--hard"); err != nil {
		return fmt.Errorf("gitops: reset --hard after conflicted stash pop: %w", err)
	}
	return fmt.Errorf("%w: working tree reset to fetched base, stashed edits preserved (git stash list) — re-base the run or replan: %v",
		ErrBaseRebaseConflict, popErr)
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
