// Package gateiso implements the ADR-063 (#2127 / #2134) isolation layer the
// runner applies at its one gate-exec seam: throwaway clone materialization,
// the Linux no-network sandbox, the per-exec build-cache posture, and (in
// sibling files) safe-runtime detection, path selection and the container
// argv builder.
//
// This file owns the throwaway CLONE. Before ADR-063 the runner materialized
// its verify/diff-coverage checkout with `git worktree add --detach`, which
// shares the primary's object store, refs and hooks: a gate command that
// planted a ref or a hook wrote straight into the operator's repository. A
// `--no-hardlinks` clone has its own `.git` directory, so anything a gate
// plants stays in the throwaway tree and is removed with it.
package gateiso

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrCommitNotFound is returned (wrapped) by MaterializeClone when the
// requested head SHA does not resolve to a commit inside the fresh clone.
var ErrCommitNotFound = errors.New("gateiso: head commit not found in clone")

// CloneReport records what MaterializeClone did.
type CloneReport struct {
	// Dest is the clone's work tree root.
	Dest string
	// HeadSHA is the full commit id checked out (the resolved form of the
	// requested head).
	HeadSHA string
	// RemoteRefsMirrored reports whether the best-effort fetch of the
	// source's refs/remotes/origin/* succeeded. A local clone maps the
	// source's refs/heads/* to refs/remotes/origin/* but does NOT copy the
	// source's own refs/remotes, so without this fetch the clone's
	// origin/main would be the source's main, not the source's origin/main —
	// the ref the verify gate's diff-base ladder resolves first.
	RemoteRefsMirrored bool
	// RemoteRefsError carries the fetch's output when it failed.
	RemoteRefsError string
}

// MaterializeClone creates an INDEPENDENT throwaway checkout of repoDir at
// dest with headSHA checked out (detached):
//
//  1. `git clone --quiet --no-hardlinks --no-checkout <repoDir> <dest>` — a
//     copy, never hardlinked objects, never a worktree of the primary.
//     repoDir may be a linked worktree; git resolves its .git file.
//  2. best-effort `git fetch origin +refs/remotes/origin/*:refs/remotes/origin/*`
//     so the clone carries the source's origin/* refs (reported, not fatal).
//  3. `git rev-parse --verify <headSHA>^{commit}` must succeed, else
//     ErrCommitNotFound.
//  4. `git checkout --quiet --detach <sha>`.
//  5. `git config --unset remote.origin.url` so nothing inside the clone can
//     push or fetch back to the primary by its default remote.
//
// git is the git binary to use ("" means "git" from PATH).
func MaterializeClone(ctx context.Context, git, repoDir, headSHA, dest string) (CloneReport, error) {
	if git == "" {
		git = "git"
	}
	rep := CloneReport{Dest: dest}
	if repoDir == "" || dest == "" {
		return rep, errors.New("gateiso: MaterializeClone needs a source repository and a destination")
	}
	if strings.TrimSpace(headSHA) == "" {
		return rep, errors.New("gateiso: MaterializeClone needs a head commit")
	}
	if out, err := runGit(ctx, git, "", "clone", "--quiet", "--no-hardlinks", "--no-checkout", "--", repoDir, dest); err != nil {
		return rep, fmt.Errorf("gateiso: clone %s: %w: %s", repoDir, err, out)
	}
	if out, err := runGit(ctx, git, dest, "fetch", "--quiet", "origin", "+refs/remotes/origin/*:refs/remotes/origin/*"); err != nil {
		rep.RemoteRefsError = strings.TrimSpace(fmt.Sprintf("%v: %s", err, out))
	} else {
		rep.RemoteRefsMirrored = true
	}
	out, err := runGit(ctx, git, dest, "rev-parse", "--verify", "--quiet", headSHA+"^{commit}")
	if err != nil {
		return rep, fmt.Errorf("%w: %q: %s", ErrCommitNotFound, headSHA, strings.TrimSpace(out))
	}
	rep.HeadSHA = strings.TrimSpace(out)
	if out, err := runGit(ctx, git, dest, "checkout", "--quiet", "--detach", rep.HeadSHA); err != nil {
		return rep, fmt.Errorf("gateiso: checkout %s: %w: %s", rep.HeadSHA, err, out)
	}
	if out, err := runGit(ctx, git, dest, "config", "--unset", "remote.origin.url"); err != nil {
		return rep, fmt.Errorf("gateiso: unset remote.origin.url: %w: %s", err, out)
	}
	return rep, nil
}

// IsIndependentGitDir reports whether dir's git metadata lives entirely
// under dir (dir/.git is a real directory and is also the repository's
// common dir) — i.e. it is a clone, not a linked worktree sharing a
// primary's refs, objects and hooks.
func IsIndependentGitDir(ctx context.Context, git, dir string) (bool, error) {
	if git == "" {
		git = "git"
	}
	dotGit := filepath.Join(dir, ".git")
	st, err := os.Lstat(dotGit)
	if err != nil {
		return false, err
	}
	if !st.IsDir() {
		// A .git FILE is the gitfile of a linked worktree.
		return false, nil
	}
	out, err := runGit(ctx, git, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, fmt.Errorf("gateiso: git-common-dir of %s: %w: %s", dir, err, out)
	}
	common, err := filepath.EvalSymlinks(strings.TrimSpace(out))
	if err != nil {
		return false, err
	}
	want, err := filepath.EvalSymlinks(dotGit)
	if err != nil {
		return false, err
	}
	return common == want, nil
}

// runGit runs git with args in dir ("" = inherited cwd) and returns the
// combined output. The operator's global/system git config is pinned to
// /dev/null so a throwaway clone never inherits commit signing or hooks
// configuration (#912, #3102).
func runGit(ctx context.Context, git, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, git, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
