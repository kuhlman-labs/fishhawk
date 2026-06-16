package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Per-run working-tree isolation (E22.X / #1137).
//
// On the local MCP loop every concurrent run historically shared one
// working tree (cfg.workingDir), so two agents driving runs on one host
// raced each other's checkouts, commits, and verify gates. This file
// provisions a lineage-keyed git worktree per run under the operator
// checkout's SHARED git dir, so each run (or each lineage, for a
// decomposed fan-out) gets its own isolated checkout while the operator's
// tracked tree stays untouched — worktrees live under .git and are
// invisible to `git status`.
//
// The keying is by LINEAGE root, not by run: a solo run keys on its own
// id, but all children of one decomposition parent key on the parent id
// so they share a single worktree and layer their commits onto the one
// shared fishhawk/run-<parent> branch (ADR-035). git refuses to check the
// same branch out in two worktrees, so sharing — not splitting — is the
// only correct shape for a fan-out.

// lineageRoot returns the worktree-keying root for a run: the parent run
// id for a decomposed child (so siblings share one worktree), else the
// run's own id for a solo run. It is the same key shortID derives for the
// shared branch at the decomposition push path, so the worktree and the
// shared branch agree.
func lineageRoot(runID, decomposedFromRunID string) string {
	if decomposedFromRunID != "" {
		return shortID(decomposedFromRunID)
	}
	return shortID(runID)
}

// worktreesDir resolves the fishhawk-worktrees directory under the repo's
// SHARED git dir. It uses `git rev-parse --git-common-dir` (NOT --git-dir)
// so an operator checkout that is itself a linked worktree still resolves
// to the one shared gitdir — every lineage worktree for a repo lands under
// a single root. The returned path is absolute.
func worktreesDir(ctx context.Context, repoDir string) (string, error) {
	if repoDir == "" {
		repoDir = "."
	}
	absRepo, err := filepath.Abs(repoDir)
	if err != nil {
		return "", fmt.Errorf("worktreesDir: abs repo dir: %w", err)
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", fmt.Errorf("worktreesDir: rev-parse --git-common-dir: %w", gitErr(err))
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", fmt.Errorf("worktreesDir: empty --git-common-dir")
	}
	// git emits the common dir relative to the -C directory for a normal
	// repo (".git") and absolute for a linked worktree; normalize either to
	// an absolute path rooted at the operator checkout.
	if !filepath.IsAbs(common) {
		common = filepath.Join(absRepo, common)
	}
	// Canonicalize: git's absolute --git-common-dir is symlink-resolved
	// (e.g. /private/var on macOS) while filepath.Abs is not, so resolving
	// the common dir here makes worktreesDir return the SAME path whether
	// it is reached from the main checkout or from a linked worktree of it.
	if resolved, rerr := filepath.EvalSymlinks(common); rerr == nil {
		common = resolved
	}
	return filepath.Join(common, "fishhawk-worktrees"), nil
}

// provisionLineageWorktree returns the absolute path to the lineage's
// worktree, creating it on first use and reusing it for every subsequent
// run that keys on the same root (decomposed-child sharing). The worktree
// is a detached checkout of HEAD at <worktrees-dir>/run-<root>; downstream
// git ops re-derive their repo dir from cfg.workingDir, so relocating that
// one field into the returned path isolates the whole stage.
func provisionLineageWorktree(ctx context.Context, repoDir, root string, logSink io.Writer) (string, error) {
	if repoDir == "" {
		repoDir = "."
	}
	wtDir, err := worktreesDir(ctx, repoDir)
	if err != nil {
		return "", fmt.Errorf("provisionLineageWorktree: %w", err)
	}
	target := filepath.Join(wtDir, "run-"+root)

	// Decomposed-child reuse: a sibling child of the same parent already
	// provisioned this lineage's worktree. Reuse it so every child layers
	// onto the one shared branch in one tree.
	registered, err := listWorktreePaths(ctx, repoDir)
	if err != nil {
		return "", fmt.Errorf("provisionLineageWorktree: %w", err)
	}
	if isRegisteredWorktree(target, registered) {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_worktree_reused","root":%q,"path":%q}`+"\n", root, target)
		return target, nil
	}

	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		return "", fmt.Errorf("provisionLineageWorktree: mkdir worktrees dir: %w", err)
	}
	// `git worktree add --detach <path> HEAD` — the same pattern the verify
	// gate uses (main.go runVerifyCommittedTree). --detach avoids claiming a
	// branch; the run's own branch/commit work happens via the downstream
	// FreshFetchBase / checkoutChildBase / commit sequences in the worktree.
	if out, err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"worktree", "add", "--detach", target, "HEAD").CombinedOutput(); err != nil {
		return "", fmt.Errorf("provisionLineageWorktree: worktree add: %v: %s",
			err, strings.TrimSpace(string(out)))
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"lineage_worktree_created","root":%q,"path":%q}`+"\n", root, target)
	return target, nil
}

// listWorktreePaths enumerates the absolute paths of every worktree
// registered against repoDir's shared gitdir via
// `git worktree list --porcelain` (the `worktree <path>` records).
func listWorktreePaths(ctx context.Context, repoDir string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil, fmt.Errorf("worktree list: %w", gitErr(err))
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			paths = append(paths, strings.TrimSpace(p))
		}
	}
	return paths, nil
}

// isRegisteredWorktree reports whether target is among the registered
// worktree paths, comparing canonicalized (symlink-resolved) paths so a
// macOS /var vs /private/var mismatch doesn't read a real reuse as a fresh
// provision.
func isRegisteredWorktree(target string, registered []string) bool {
	tr := canonPath(target)
	for _, p := range registered {
		if canonPath(p) == tr {
			return true
		}
	}
	return false
}

// canonPath resolves symlinks where possible, falling back to a cleaned
// path when the target doesn't yet exist on disk.
func canonPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// acquireLineageLock serializes same-lineage stages with an O_EXCL
// lockfile keyed to the lineage root. Decomposition drives a lineage's
// stages strictly sequentially, so a held lock by a LIVE process means a
// concurrent same-lineage stage is racing the shared tree — fail loud
// (category-A) rather than corrupt it. A lock left by a CRASHED prior
// stage (pid no longer alive) is reclaimed and the acquire retried.
//
// The lockfile lives BESIDE the worktree under <worktrees-dir> (NOT inside
// it) so it can never be swept into the run's commit by the in-worktree
// `git add -A` / scoped staging — a file at the worktree root would be.
// It records pid + run id for diagnosis and is removed by the returned
// release func (deferred to stage end).
func acquireLineageLock(ctx context.Context, repoDir, root, runID string, logSink io.Writer) (func(), error) {
	wtDir, err := worktreesDir(ctx, repoDir)
	if err != nil {
		return nil, fmt.Errorf("acquireLineageLock: %w", err)
	}
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		return nil, fmt.Errorf("acquireLineageLock: mkdir worktrees dir: %w", err)
	}
	lockPath := filepath.Join(wtDir, "run-"+root+".lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n%s\n", os.Getpid(), runID)
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquireLineageLock: open lock: %w", err)
		}
		pid := readLockPID(lockPath)
		if pid > 0 && processAlive(pid) {
			return nil, fmt.Errorf(
				"acquireLineageLock: lineage %s already locked by live pid %d "+
					"(concurrent same-lineage stage — decomposition stages must run sequentially)",
				root, pid)
		}
		// Stale lock from a crashed prior stage — reclaim and retry once.
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_lock_reclaimed","root":%q,"stale_pid":%d}`+"\n", root, pid)
		if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("acquireLineageLock: reclaim stale lock: %w", err)
		}
	}
	return nil, fmt.Errorf("acquireLineageLock: lineage %s lock still contended after reclaim", root)
}

// readLockPID reads the pid recorded on the first line of a lockfile.
// Returns 0 when the file is unreadable or the line is not an integer.
func readLockPID(lockPath string) int {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0
	}
	line := data
	if i := strings.IndexByte(string(data), '\n'); i >= 0 {
		line = data[:i]
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(line)))
	if err != nil {
		return 0
	}
	return pid
}

// processAlive reports whether a process with the given pid currently
// exists, using signal 0 (the portable liveness probe on unix): a nil
// error means alive, EPERM means alive-but-not-ours, and ESRCH/finished
// means dead.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// gitErr enriches an *exec.ExitError with its captured stderr so a git
// failure produces an actionable message rather than a bare exit status.
func gitErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}
