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
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// DefaultRemote is the git remote ShipBranch pushes to. Always
// "origin" in the GitHub Actions checkout shape.
const DefaultRemote = "origin"

// DefaultAuthorName + DefaultAuthorEmail are the bot identity used
// when the caller doesn't override. Matches the pattern Actions uses
// for github-actions[bot] but with a Fishhawk slug so audit consumers
// can distinguish.
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

	if err := p.run(ctx, args.RepoDir, "checkout", "-b", args.Branch); err != nil {
		return nil, fmt.Errorf("gitops: checkout -b %s: %w", args.Branch, err)
	}
	if err := p.run(ctx, args.RepoDir, "add", "-A"); err != nil {
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

	if err := p.run(ctx, args.RepoDir, "push", args.RemoteURL, fmt.Sprintf("HEAD:%s", args.Branch)); err != nil {
		return nil, fmt.Errorf("gitops: push %s: %w", remote, err)
	}

	return &CommitAndPushResult{
		HeadSHA: headSHA,
		BaseSHA: baseSHA,
	}, nil
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
