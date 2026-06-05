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
		// branch already exists. Stash uncommitted agent edits, fetch+rebase,
		// restore the edits, then fall through to add+commit+push.
		if err := p.run(ctx, args.RepoDir, "stash", "--include-untracked"); err != nil {
			return nil, fmt.Errorf("gitops: stash: %w", err)
		}
		if err := p.run(ctx, args.RepoDir, "fetch", remote, args.Branch); err != nil {
			return nil, fmt.Errorf("gitops: fetch %s: %w", args.Branch, err)
		}
		// DWIM checkout creates a local tracking branch when it doesn't
		// exist yet; is a no-op (switches to it) when it does.
		if err := p.run(ctx, args.RepoDir, "checkout", args.Branch); err != nil {
			return nil, fmt.Errorf("gitops: checkout %s: %w", args.Branch, err)
		}
		if err := p.run(ctx, args.RepoDir, "pull", "--rebase", remote, args.Branch); err != nil {
			return nil, fmt.Errorf("gitops: pull --rebase: %w", err)
		}
		if err := p.run(ctx, args.RepoDir, "stash", "pop"); err != nil {
			return nil, fmt.Errorf("gitops: stash pop: %w", err)
		}
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
	if args.PushToken != "" && strings.HasPrefix(args.RemoteURL, "https://") {
		host, err := pushHost(args.RemoteURL)
		if err != nil {
			return nil, fmt.Errorf("gitops: parse remote URL: %w", err)
		}
		header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+args.PushToken))
		if err := p.run(ctx, args.RepoDir, "config", "--local", "--replace-all",
			"http."+host+".extraheader", header); err != nil {
			return nil, fmt.Errorf("gitops: refresh extraheader: %w", err)
		}
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
	declared := make(map[string]bool, len(scopeFiles))
	for _, f := range scopeFiles {
		declared[f] = true
	}
	var toStage []string
	for _, line := range strings.Split(status, "\n") {
		path := porcelainPath(line)
		if path == "" {
			continue
		}
		if declared[path] {
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
