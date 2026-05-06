// Package gitops contains the runner's git-write surface: create a
// branch, commit any agent edits, push to origin, and open a PR via
// GitHub's REST API. Distinct from internal/gitdiff (which is
// read-only and produces a constraint.Diff) because the write path
// is opinionated about author identity, branch naming, and the
// shape of the resulting pull-request artifact.
//
// v0 uses the workflow's GITHUB_TOKEN for both push (HTTPS basic
// auth with x-access-token:$TOKEN) and PR creation (REST). The PR's
// author attribution will resolve to the GitHub Actions bot rather
// than the Fishhawk App; v0.x can swap to the App's installation
// token once the runner is allowed to call back to the backend for
// it. See issue #195.
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

	// Token is the GitHub token used for HTTPS push auth. Required.
	Token string

	// RemoteURL is the original origin URL (e.g.
	// "https://github.com/owner/repo"). Required for token rewrite.
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
// via HTTPS using Token. Returns NoChanges=true with no other side
// effects when the working tree is clean.
func (p *Pusher) CommitAndPush(ctx context.Context, args CommitAndPushArgs) (*CommitAndPushResult, error) {
	if args.RepoDir == "" {
		return nil, errors.New("gitops: RepoDir required")
	}
	if args.Branch == "" {
		return nil, errors.New("gitops: Branch required")
	}
	if args.Token == "" {
		return nil, errors.New("gitops: Token required for push auth")
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

	// actions/checkout sets a `http.https://github.com/.extraheader` at
	// LOCAL scope pointing at the workflow's GITHUB_TOKEN. That header
	// wins over any URL-embedded credential on the push.
	//
	// `extraheader` is a multi-valued config key — `-c` ADDS an entry
	// rather than replacing the existing one, which gets us two
	// `Authorization` headers on the request and a 400 "Duplicate
	// header" from GitHub. So we have to clear the existing local
	// entry first, then `-c` ours on the push call.
	//
	// Unset is best-effort: returns non-zero when no value matches,
	// which is fine for non-Actions environments where the setting
	// was never there to begin with.
	pushAuthHeader, headerHost, err := pushExtraHeader(args.RemoteURL, args.Token)
	if err != nil {
		return nil, fmt.Errorf("gitops: build push auth header: %w", err)
	}
	if pushAuthHeader != "" {
		_ = p.run(ctx, args.RepoDir, "config", "--local", "--unset-all",
			"http."+headerHost+".extraheader")
	}
	pushArgs := []string{}
	if pushAuthHeader != "" {
		pushArgs = append(pushArgs,
			"-c", "http."+headerHost+".extraheader="+pushAuthHeader,
		)
	}
	pushArgs = append(pushArgs,
		"push", args.RemoteURL, fmt.Sprintf("HEAD:%s", args.Branch),
	)
	if err := p.run(ctx, args.RepoDir, pushArgs...); err != nil {
		return nil, fmt.Errorf("gitops: push %s: %w", remote, err)
	}

	return &CommitAndPushResult{
		HeadSHA: headSHA,
		BaseSHA: baseSHA,
	}, nil
}

// pushExtraHeader returns the `AUTHORIZATION: basic <base64>` value
// suitable for `git -c http.<host>.extraheader=…` to authenticate
// the push, plus the `<host>` (scheme://host[:port]) it scopes to.
// Mirrors actions/checkout's auth model exactly so we override its
// existing extraheader cleanly rather than racing it.
//
// Empty header + empty host → caller skips the `-c` and pushes
// unauthenticated. We return (empty, empty) for non-HTTPS remotes
// (SSH, local file paths) since those use their own auth model.
func pushExtraHeader(remoteURL, token string) (header, host string, err error) {
	if !strings.HasPrefix(remoteURL, "https://") {
		return "", "", nil
	}
	u, err := url.Parse(remoteURL)
	if err != nil {
		return "", "", err
	}
	// `<scheme>://<host>` — git config keys are scoped at the host
	// level, e.g. `http.https://github.com/.extraheader`.
	host = u.Scheme + "://" + u.Host + "/"
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	header = "AUTHORIZATION: basic " + encoded
	return header, host, nil
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
