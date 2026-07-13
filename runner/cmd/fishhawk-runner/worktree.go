package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
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
//
// parallelIsolate (E24.4 / #1144) inverts the child case ONLY: when set
// AND this is a decomposed child, the root is the child's OWN id so each
// concurrent sibling provisions an isolated worktree (run-<child>) instead
// of racing the one shared run-<parent> tree. Concurrent children already
// own distinct per-slice sole-writer branches, so isolation is the correct
// shape for parallel; serial drive leaves it off and keeps the shared tree.
// The solo path is unaffected either way (it already keys on its own id).
func lineageRoot(runID, decomposedFromRunID string, parallelIsolate bool) string {
	if decomposedFromRunID != "" && !parallelIsolate {
		return shortID(decomposedFromRunID)
	}
	return shortID(runID)
}

// lineageRootFull returns the FULL (un-shortened) lineage-root run id —
// the parent run id for a decomposed child, else the run's own id. The
// worktree directory is keyed on the SHORT id (lineageRoot), which the
// backend's lineage_complete read can't take, so the full id is recorded
// in a sidecar beside the worktree (writeLineageRunID) for the sweep to
// resolve the short directory name back to a run id.
//
// parallelIsolate mirrors lineageRoot exactly so the sidecar names the
// SAME run id the (now per-child) worktree directory is keyed on: under
// the flag a decomposed child records its own full id, so the sweep's
// lineage_complete read targets the child (which, having no children of
// its own, is terminal == lineage_complete).
func lineageRootFull(runID, decomposedFromRunID string, parallelIsolate bool) string {
	if decomposedFromRunID != "" && !parallelIsolate {
		return decomposedFromRunID
	}
	return runID
}

// lineageStatusClient is the backend read sweepTerminalWorktrees needs:
// whether a lineage-root run is terminal with every decomposed child
// terminal. *upload.Client satisfies it via RunLineageComplete.
type lineageStatusClient interface {
	RunLineageComplete(ctx context.Context, runID string) (bool, error)
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

// baseDivergenceError reports that a FRESH lineage worktree would be seeded
// from an operator checkout whose HEAD is NOT an ancestor of (or equal to) the
// declared base's remote-tracking ref — the #1866 diverged-seed footgun (a
// leftover unmerged commit on the operator HEAD, the MCP-cwd-default footgun,
// contaminates the agent's tree, the reviewer diff, and the NoChanges probe).
// Its Error() names both resolved SHAs and the recovery so a diverged operator
// environment surfaces loudly at dispatch — before any agent budget burns.
type baseDivergenceError struct {
	headSHA string
	baseRef string
	baseSHA string
}

func (e *baseDivergenceError) Error() string {
	return fmt.Sprintf(
		"seed checkout HEAD %s is not an ancestor of the declared base %s (origin/%s at %s): "+
			"a fresh run worktree seeded from this diverged operator checkout would contaminate "+
			"the agent's tree and the review diff (#1866, ADR-035 declared-base). Recovery: "+
			"re-dispatch with working_dir pointing at a clean checkout of %s, or reset/rebase the "+
			"checkout onto origin/%s.",
		e.headSHA, e.baseRef, e.baseRef, e.baseSHA, e.baseRef, e.baseRef)
}

// ancestryProbe runs `git merge-base --is-ancestor <headRev> <baseRev>` in
// repoDir, returning its raw error: nil when headRev is an ancestor of (or
// equal to) baseRev, a *exec.ExitError with code 1 when it is provably NOT,
// and any other error when the probe itself failed. It is a package-level var
// so a test can force the probe-error degrade branch (#1866
// ancestry_probe_failed) deterministically without corrupting a real repo.
var ancestryProbe = func(ctx context.Context, repoDir, headRev, baseRev string) error {
	return exec.CommandContext(ctx, "git", "-C", repoDir,
		"merge-base", "--is-ancestor", headRev, baseRev).Run()
}

// resolveHead resolves the operator checkout's current HEAD to a concrete
// commit SHA. The caller pins this value so the seed-ancestry check and the
// subsequent `git worktree add` reference the SAME immutable commit rather than
// re-resolving the mutable symbolic HEAD twice (the #1866 check-then-add race).
func resolveHead(ctx context.Context, repoDir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"rev-parse", "HEAD").Output()
	if err != nil {
		return "", gitErr(err)
	}
	return strings.TrimSpace(string(out)), nil
}

// verifySeedAncestry is the #1866 seed-ancestry guard: before a FRESH lineage
// worktree is seeded from the operator checkout, it verifies headRev — the
// PINNED commit the caller will seed the worktree from — is an ancestor of (or
// equal to) the declared base's remote-tracking ref refs/remotes/origin/<base>.
// On PROVEN divergence it returns a *baseDivergenceError (mapped by the caller
// to the working_dir_diverged_from_base runner_failed reason). It degrades to a
// logged skip — never a block — when evidence is unavailable (remote
// unconfigured, tracking ref unresolvable, or the ancestry probe itself
// errors), mirroring the #1302/#1363 not-wired-vs-transient degrade contract:
// an infra flake must never hard-fail a stage the old code would have run.
//
// headRev is a concrete commit SHA the caller resolved ONCE (not the symbolic
// "HEAD"): the same immutable commit is both checked here and handed to
// `git worktree add`, so an operator HEAD that advances to a local-only commit
// between this check and the seed cannot slip a diverged commit past the guard
// (the check-then-add TOCTOU the mutable operator checkout would otherwise open).
//
// The check is offline/auth-free by design (no fetch): `git push` updates the
// remote-tracking ref, so a checkout whose extra commits were genuinely pushed
// cannot false-refuse against the local origin/<base> ref.
func verifySeedAncestry(ctx context.Context, repoDir, headRev, baseRef string, logSink io.Writer) error {
	if baseRef == "" {
		return nil
	}
	// (b) Remote not configured — the bare local-test-repo / GitHub-not-wired
	// degrade state (#1302): no origin/<base> to compare against, so skip.
	if !remoteConfigured(ctx, repoDir, gitops.DefaultRemote) {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_worktree_base_guard_skipped","reason":"remote_unconfigured"}`+"\n")
		return nil
	}
	trackingRef := "refs/remotes/" + gitops.DefaultRemote + "/" + baseRef
	// (c) Tracking ref unresolvable (never fetched, shallow single-ref
	// checkout, deleted) — skip: there is no local base tip to compare against.
	baseOut, err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"rev-parse", "--verify", "--quiet", trackingRef).Output()
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_worktree_base_guard_skipped","reason":"base_ref_unresolvable"}`+"\n")
		return nil
	}
	// (d) headRev ancestor-of/equal-to the base tip → nil (commit-time
	// FreshFetchBase handles a base that has since advanced, ADR-043).
	probeErr := ancestryProbe(ctx, repoDir, headRev, trackingRef)
	if probeErr == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(probeErr, &ee) && ee.ExitCode() == 1 {
		// Proven not-an-ancestor: refuse loud with both resolved SHAs. headRev
		// is already the pinned SHA, so no second (racy) HEAD resolution.
		return &baseDivergenceError{
			headSHA: headRev,
			baseRef: baseRef,
			baseSHA: strings.TrimSpace(string(baseOut)),
		}
	}
	// Any other probe failure (git unavailable, transient) → skip, never
	// hard-fail a stage the old code would have run.
	_, _ = fmt.Fprintf(logSink,
		`{"event":"lineage_worktree_base_guard_skipped","reason":"ancestry_probe_failed"}`+"\n")
	return nil
}

// worktreeProvisionFailureReason maps a provisionLineageWorktree error to the
// runner_failed reason string: "working_dir_diverged_from_base" for the #1866
// seed-ancestry refusal, else the generic "worktree_provision". Factored out
// so main.go's call site stays a two-line change and the reason mapping is
// unit-testable.
func worktreeProvisionFailureReason(err error) string {
	var bde *baseDivergenceError
	if errors.As(err, &bde) {
		return "working_dir_diverged_from_base"
	}
	return "worktree_provision"
}

// provisionLineageWorktree returns the absolute path to the lineage's
// worktree, creating it on first use and reusing it for every subsequent
// run that keys on the same root (decomposed-child sharing). The worktree
// is a detached checkout of HEAD at <worktrees-dir>/run-<root>; downstream
// git ops re-derive their repo dir from cfg.workingDir, so relocating that
// one field into the returned path isolates the whole stage.
//
// baseRef is the declared PR base branch; on a FRESH provision (not the reuse
// path) verifySeedAncestry refuses to seed from an operator HEAD that has
// diverged from origin/<baseRef> (#1866). The reuse path is exempt: a
// mid-lineage worktree's HEAD is legitimately the run branch, and its seed was
// validated at first provision.
func provisionLineageWorktree(ctx context.Context, repoDir, root, baseRef string, logSink io.Writer) (string, error) {
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

	// Resolve HEAD to a concrete commit ONCE and pin it: the same immutable
	// SHA is both checked by the seed-ancestry guard and handed to `git
	// worktree add` below. Because the operator checkout is mutable, resolving
	// the symbolic "HEAD" twice (once in the guard, once at `worktree add`)
	// would open a TOCTOU window — HEAD could advance to a local-only commit
	// in between and seed the fresh worktree from the diverged commit the guard
	// is meant to prevent (#1866 concurrency). Pinning closes that window.
	headSHA, err := resolveHead(ctx, repoDir)
	if err != nil {
		return "", fmt.Errorf("provisionLineageWorktree: resolve HEAD: %w", err)
	}

	// #1866 seed-ancestry guard — FRESH provisions only (the reuse return
	// above exempts mid-lineage stages and already-seeded siblings). A fresh
	// worktree is seeded from the pinned HEAD SHA, so a diverged operator HEAD
	// would contaminate the agent's tree and the review diff.
	if err := verifySeedAncestry(ctx, repoDir, headSHA, baseRef, logSink); err != nil {
		return "", err
	}

	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		return "", fmt.Errorf("provisionLineageWorktree: mkdir worktrees dir: %w", err)
	}
	// `git worktree add --detach <path> <pinned-sha>` — the same pattern the
	// verify gate uses (main.go runVerifyCommittedTree), but seeded from the
	// pinned SHA rather than the symbolic HEAD so it agrees with the guard
	// above. --detach avoids claiming a branch; the run's own branch/commit
	// work happens via the downstream FreshFetchBase / checkoutChildBase /
	// commit sequences in the worktree.
	if out, err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"worktree", "add", "--detach", target, headSHA).CombinedOutput(); err != nil {
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

const (
	// worktreeAdminLockName is the single per-gitdir lockfile that serializes
	// cross-lineage worktree-admin operations. It sits BESIDE the worktrees
	// under <worktrees-dir> (never inside a worktree), so it is never swept
	// into a run's commit — the same placement invariant as the lineage lock
	// and the run-id sidecar.
	worktreeAdminLockName = ".worktree-admin.lock"
	// worktreeAdminLockMaxWait bounds how long acquireWorktreeAdminLock blocks
	// on a LIVE holder before returning an actionable timeout error rather
	// than hanging the stage forever.
	worktreeAdminLockMaxWait = 30 * time.Second
	// worktreeAdminLockBackoff is the poll interval between contended acquire
	// attempts.
	worktreeAdminLockBackoff = 25 * time.Millisecond
)

// acquireWorktreeAdminLock serializes the cross-lineage worktree-admin
// critical section — the provision-time sweep (`git worktree remove
// --force`) plus the `git worktree list`/`add` ops — against the repo's
// shared gitdir.
//
// Cross-lineage safety decision (#1181, issue option (b)): git's
// worktree-admin subcommands are NOT a documented mutual-exclusion contract
// across concurrent invocations on a shared gitdir, and whatever internal
// locking they take varies by git version. Rather than audit git internals
// and depend on those version-specific guarantees (option (a)), we add our
// own deterministic, version-independent serialization here: a sibling run
// of a DIFFERENT lineage can no longer interleave a `git worktree remove
// --force` of a terminal lineage with another lineage's `git worktree
// add`/`list` on the same gitdir.
//
// Unlike acquireLineageLock — which fails LOUD because same-lineage
// concurrency is a decomposition bug — cross-lineage concurrent provisions
// are the feature's EXPECTED steady state on a multi-run host, so this lock
// BLOCKS rather than fails: it retries on contention with a short backoff,
// reclaims a stale lock whose recorded pid is no longer alive, and is
// bounded by a max-wait (and the caller's context deadline) so a wedged
// holder yields an actionable error instead of hanging the stage forever.
// The caller MUST hold it ONLY around the fast sweep+provision critical
// section (acquire before the sweep, release the moment provision returns,
// before the long stage).
//
// Records os.Getpid() on the first line (the format readLockPID parses) for
// diagnosis. The returned release func removes the lockfile.
func acquireWorktreeAdminLock(ctx context.Context, repoDir string, logSink io.Writer) (func(), error) {
	wtDir, err := worktreesDir(ctx, repoDir)
	if err != nil {
		return nil, fmt.Errorf("acquireWorktreeAdminLock: %w", err)
	}
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		return nil, fmt.Errorf("acquireWorktreeAdminLock: mkdir worktrees dir: %w", err)
	}
	lockPath := filepath.Join(wtDir, worktreeAdminLockName)
	deadline := time.Now().Add(worktreeAdminLockMaxWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("acquireWorktreeAdminLock: open lock: %w", err)
		}
		// Contended. Reclaim a lock whose recorded holder has died (a crashed
		// prior stage), else back off and retry until the bounded deadline.
		pid := readLockPID(lockPath)
		if pid <= 0 || !processAlive(pid) {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"worktree_admin_lock_reclaimed","stale_pid":%d}`+"\n", pid)
			if rmErr := os.Remove(lockPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return nil, fmt.Errorf("acquireWorktreeAdminLock: reclaim stale lock: %w", rmErr)
			}
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"acquireWorktreeAdminLock: worktree-admin lock held by live pid %d "+
					"still contended after %s max-wait", pid, worktreeAdminLockMaxWait)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("acquireWorktreeAdminLock: %w", ctx.Err())
		case <-time.After(worktreeAdminLockBackoff):
		}
	}
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

// lineageRunIDPath returns the sidecar path that records a lineage root's
// FULL run id beside its worktree. It lives in <worktrees-dir> next to the
// `run-<root>` worktree and the `run-<root>.lock` lock — never inside the
// worktree, so it can't be swept into a run's commit.
func lineageRunIDPath(wtDir, root string) string {
	return filepath.Join(wtDir, "run-"+root+".runid")
}

// writeLineageRunID records the lineage root's FULL run id beside its
// worktree so a later sweepTerminalWorktrees can resolve the short
// `run-<root>` directory name back to the run id the backend's
// lineage_complete read takes. Best-effort: a write failure logs a
// degradation event and never fails the stage — the only consequence is
// that this lineage's worktree won't be reclaimable by short-id lookup
// (it stays under .git, invisible to git status).
func writeLineageRunID(ctx context.Context, repoDir, root, fullRunID string, logSink io.Writer) {
	wtDir, err := worktreesDir(ctx, repoDir)
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_runid_write_degraded","root":%q,"detail":%q}`+"\n", root, err.Error())
		return
	}
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_runid_write_degraded","root":%q,"detail":%q}`+"\n", root, err.Error())
		return
	}
	if err := os.WriteFile(lineageRunIDPath(wtDir, root), []byte(fullRunID+"\n"), 0o644); err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_runid_write_degraded","root":%q,"detail":%q}`+"\n", root, err.Error())
	}
}

// readLineageRunID reads the full run id recorded by writeLineageRunID,
// returning "" when the sidecar is absent or unreadable.
func readLineageRunID(wtDir, root string) string {
	data, err := os.ReadFile(lineageRunIDPath(wtDir, root))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// runIDRe matches a canonical-UUID-shaped run id (8-4-4-4-12 hex). It is
// intentionally shape-only, not a strict RFC-4122 version/variant check:
// the goal is to reject non-UUID fixture roots (e.g. "rid") before any
// backend call, and every real fishhawk run id the sidecar records is a
// canonical UUID.
var runIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// looksLikeRunID reports whether s has the canonical-UUID shape a real run
// id carries. A non-UUID sidecar value means the worktree is a leftover
// test fixture, not a real run, so it must be pruned locally rather than
// sent to the backend (which would 400 on an invalid run id).
func looksLikeRunID(s string) bool {
	return runIDRe.MatchString(s)
}

// pruneStaleWorktree removes a worktree the sweep has proven reclaimable
// (a non-run fixture root, or a run the backend definitively reports
// absent) and clears its sidecar + lock so the dir goes quiet. Best-effort:
// on a `git worktree remove` failure it logs one worktree_sweep_degraded
// line and leaves the entry; on success it removes the run-id sidecar and
// the run-<root>.lock and logs exactly one lineage_worktree_pruned line.
func pruneStaleWorktree(ctx context.Context, repoDir, wtDir, root, path, reason string, logSink io.Writer) {
	if out, err := exec.CommandContext(ctx, "git", "-C", repoDir,
		"worktree", "remove", "--force", path).CombinedOutput(); err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"worktree_sweep_degraded","root":%q,"reason":%q,"detail":%q}`+"\n",
			root, reason, strings.TrimSpace(string(out)))
		return
	}
	_ = os.Remove(lineageRunIDPath(wtDir, root))
	_ = os.Remove(filepath.Join(wtDir, "run-"+root+".lock"))
	_, _ = fmt.Fprintf(logSink,
		`{"event":"lineage_worktree_pruned","root":%q,"reason":%q,"path":%q}`+"\n", root, reason, path)
}

// sweepTerminalWorktrees reclaims any lineage worktree whose root run the
// backend reports terminal-with-all-children-terminal (lineage_complete),
// the host-side half of the #1137 teardown contract: fishhawkd is the
// authority on lineage terminality (it can't reach the operator's
// filesystem), so the runner performs the physical `git worktree remove`
// lazily at the next provision. A worktree for a terminal lineage
// therefore lingers until the next run on this host — acceptable: it lives
// under .git, is invisible to git status, and is cheap.
//
// Called at provision start (before adding the new worktree). Entirely
// best-effort: a missing run-id sidecar, a backend error, or a git error
// logs a degradation event and is skipped — the sweep never fails the
// stage, and never removes a worktree it can't prove is reclaimable.
func sweepTerminalWorktrees(ctx context.Context, repoDir string, client lineageStatusClient, logSink io.Writer) {
	if client == nil {
		return
	}
	if repoDir == "" {
		repoDir = "."
	}
	wtDir, err := worktreesDir(ctx, repoDir)
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"worktree_sweep_degraded","detail":%q}`+"\n", err.Error())
		return
	}
	paths, err := listWorktreePaths(ctx, repoDir)
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"worktree_sweep_degraded","detail":%q}`+"\n", err.Error())
		return
	}
	canonWTDir := canonPath(wtDir)
	for _, p := range paths {
		// Only sweep worktrees under our fishhawk-worktrees dir — never
		// the operator's main checkout or an unrelated worktree.
		if filepath.Dir(canonPath(p)) != canonWTDir {
			continue
		}
		root, ok := strings.CutPrefix(filepath.Base(p), "run-")
		if !ok {
			continue
		}
		fullID := readLineageRunID(wtDir, root)
		if fullID == "" {
			// No sidecar → can't resolve the short name to a run id; leave
			// the worktree rather than guess.
			continue
		}
		if !looksLikeRunID(fullID) {
			// A non-UUID sidecar (e.g. a "rid" test fixture) can never be a
			// real run — prune it locally and NEVER query the backend (a
			// non-UUID run id would 400).
			pruneStaleWorktree(ctx, repoDir, wtDir, root, p, "non_run_root", logSink)
			continue
		}
		complete, err := client.RunLineageComplete(ctx, fullID)
		if err != nil {
			if errors.Is(err, upload.ErrNotFound) {
				// The backend definitively reports this run absent (deleted or
				// never created) → the worktree is orphaned. Prune once instead
				// of re-degrading on every runner start.
				pruneStaleWorktree(ctx, repoDir, wtDir, root, p, "run_not_found", logSink)
				continue
			}
			// Any other error may be transient — best-effort: log and leave
			// the worktree.
			_, _ = fmt.Fprintf(logSink,
				`{"event":"worktree_sweep_degraded","root":%q,"detail":%q}`+"\n", root, err.Error())
			continue
		}
		if !complete {
			continue
		}
		if out, err := exec.CommandContext(ctx, "git", "-C", repoDir,
			"worktree", "remove", "--force", p).CombinedOutput(); err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"worktree_sweep_degraded","root":%q,"detail":%q}`+"\n",
				root, strings.TrimSpace(string(out)))
			continue
		}
		// Worktree gone — remove its sidecar + lock so the dir is clean.
		_ = os.Remove(lineageRunIDPath(wtDir, root))
		_ = os.Remove(filepath.Join(wtDir, "run-"+root+".lock"))
		_, _ = fmt.Fprintf(logSink,
			`{"event":"lineage_worktree_swept","root":%q,"path":%q}`+"\n", root, p)
	}
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
