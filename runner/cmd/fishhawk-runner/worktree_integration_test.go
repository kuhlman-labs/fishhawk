package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Per-run working-tree isolation — cross-layer integration test (E22.X / #1137).
//
// The unit tests in worktree_test.go cover each primitive in isolation
// (keying, provision/reuse, locking, sweep). This test drives the two real
// run SHAPES end to end and concurrently against a fake lineage-status
// backend, exercising the way main.go composes those primitives — sweep →
// provision → sidecar → lock → relocate working dir → commit in the
// worktree — to prove the invariants that only emerge from the whole
// sequence:
//
//   (a) two SOLO runs racing one operator checkout land in DISTINCT
//       worktrees and each run's commit carries only its own diff (no
//       cross-contamination from the sibling's tree);
//   (b) a decomposition PARENT with two children shares ONE worktree
//       (reuse), and each child COMMIT carries only its own diff layered on
//       the sibling's — per-commit isolation on the cumulative shared
//       fishhawk/run-<parent> branch (ADR-035), NOT per-branch;
//   (c) once the backend reports a lineage terminal, the NEXT provision on
//       this host sweeps that worktree away (lazy host-side reclamation);
//
// and the load-bearing safety property throughout (a)+(b)+(c): the
// operator's tracked tree stays clean — worktrees live under the shared
// gitdir, invisible to `git status` (#1137 condition 2).

// syncLineageClient is a thread-safe lineageStatusClient for the concurrent
// phase: multiple goroutines call sweepTerminalWorktrees → RunLineageComplete
// at provision start, so the completeness map and the query log are guarded.
type syncLineageClient struct {
	mu       sync.Mutex
	complete map[string]bool
	queried  []string
}

func (c *syncLineageClient) RunLineageComplete(_ context.Context, runID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queried = append(c.queried, runID)
	return c.complete[runID], nil
}

func (c *syncLineageClient) setComplete(runID string, v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.complete[runID] = v
}

// gitAuthorEnv returns an environment with a deterministic git identity so
// commits in worktrees succeed without depending on the operator's config.
func gitAuthorEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
}

// runGitErr runs a git command in dir and returns an error (with captured
// output) on failure. It takes no *testing.T so it is safe to call from a
// goroutine, where t.Fatal is forbidden.
func runGitErr(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitAuthorEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// runGitOut runs a git command in dir and returns its trimmed stdout. Safe
// to call from a goroutine.
func runGitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitAuthorEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// runResult captures one simulated run's worktree path and commit sha (or
// the first error encountered), stored by a goroutine and read after join.
type runResult struct {
	wt     string
	commit string
	err    error
}

// provisionFlow mirrors main.go's #1137 wiring exactly: compute the lineage
// root, sweep terminal worktrees, provision/reuse this lineage's worktree
// against the ORIGINAL operator checkout, record the run-id sidecar, and
// take the same-lineage lock. The returned release MUST be deferred by the
// caller (stage end). decomposedFrom is "" for a solo run, the parent run id
// for a decomposed child.
func provisionFlow(ctx context.Context, repo, runID, decomposedFrom string, parallelIsolate bool, client lineageStatusClient) (wt string, release func(), err error) {
	root := lineageRoot(runID, decomposedFrom, parallelIsolate)
	// Cross-lineage worktree-admin lock (#1181): serialize the fast
	// sweep+provision critical section against sibling lineages, then release
	// before the (here, simulated) long stage — exactly as main.go wires it.
	adminRelease, err := acquireWorktreeAdminLock(ctx, repo, io.Discard)
	if err != nil {
		return "", nil, err
	}
	sweepTerminalWorktrees(ctx, repo, client, io.Discard)
	wt, err = provisionLineageWorktree(ctx, repo, root, io.Discard)
	if err != nil {
		adminRelease()
		return "", nil, err
	}
	adminRelease()
	writeLineageRunID(ctx, repo, root, lineageRootFull(runID, decomposedFrom, parallelIsolate), io.Discard)
	release, err = acquireLineageLock(ctx, repo, root, runID, io.Discard)
	if err != nil {
		return "", nil, err
	}
	return wt, release, nil
}

// simulateSolo drives one solo run's full stage: provision its own worktree,
// cut the run's stage branch (ADR-035 fishhawk/run-<id>/stage-<id>), and
// commit one file there. The commit lands on the worktree's HEAD, isolated
// from any concurrent run.
func simulateSolo(ctx context.Context, repo, runID, stageID, file, content string, client lineageStatusClient) (res runResult) {
	wt, release, err := provisionFlow(ctx, repo, runID, "", false, client)
	if err != nil {
		res.err = err
		return res
	}
	defer release()
	branch := fmt.Sprintf("fishhawk/run-%s/stage-%s", shortID(runID), shortID(stageID))
	if res.err = runGitErr(wt, "checkout", "-b", branch); res.err != nil {
		return res
	}
	if res.err = commitFile(wt, file, content, "run "+shortID(runID)); res.err != nil {
		return res
	}
	res.commit, res.err = runGitOut(wt, "rev-parse", "HEAD")
	res.wt = wt
	return res
}

// simulateChild drives one decomposed child's stage: reuse the lineage's
// shared worktree, ensure the shared fishhawk/run-<parent> branch is checked
// out (the first child creates it; subsequent children reuse it), and commit
// one file there. Each child runs as its own stage with its own lock
// acquire/release, mirroring two sequential runner invocations.
func simulateChild(ctx context.Context, repo, parentID, childID, file, content string, isFirst bool, client lineageStatusClient) (wt, commit string, err error) {
	wt, release, err := provisionFlow(ctx, repo, childID, parentID, false, client)
	if err != nil {
		return "", "", err
	}
	defer release()
	shared := "fishhawk/run-" + shortID(parentID)
	if isFirst {
		err = runGitErr(wt, "checkout", "-b", shared)
	} else {
		err = runGitErr(wt, "checkout", shared)
	}
	if err != nil {
		return "", "", err
	}
	if err = commitFile(wt, file, content, "child "+shortID(childID)); err != nil {
		return "", "", err
	}
	commit, err = runGitOut(wt, "rev-parse", "HEAD")
	return wt, commit, err
}

// commitFile writes file under dir and commits only that path, so the
// resulting commit's diff carries exactly this change.
func commitFile(dir, file, content, msg string) error {
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		return err
	}
	if err := runGitErr(dir, "add", file); err != nil {
		return err
	}
	return runGitErr(dir, "commit", "-q", "-m", msg)
}

// commitFiles returns the sorted paths a commit changed relative to its first
// parent (`git show --name-only`), i.e. the diff that commit alone carries.
func commitFiles(t *testing.T, repo, commit string) []string {
	t.Helper()
	out, err := runGitOut(repo, "show", "--name-only", "--pretty=format:", commit)
	if err != nil {
		t.Fatalf("git show %s: %v", commit, err)
	}
	return splitSortedLines(out)
}

// treeFiles returns the sorted paths present in a commit's tree
// (`git ls-tree -r --name-only`), i.e. the cumulative content at that ref.
func treeFiles(t *testing.T, repo, ref string) []string {
	t.Helper()
	out, err := runGitOut(repo, "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		t.Fatalf("git ls-tree %s: %v", ref, err)
	}
	return splitSortedLines(out)
}

func splitSortedLines(s string) []string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	sort.Strings(lines)
	return lines
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestWorktreeIsolation_Integration(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	client := &syncLineageClient{complete: map[string]bool{}}

	// The seed commit every worktree is detached at; each run's commit is
	// cut from here, so its first-parent diff is exactly its own change.
	seed := gitPorcelainHead(t, repo)
	if status := gitPorcelain(t, repo); status != "" {
		t.Fatalf("operator tree dirty before any run:\n%s", status)
	}

	const (
		soloA  = "a1a1a1a1-0000-0000-0000-000000000001"
		stageA = "1a1a1a1a-0000-0000-0000-00000000000a"
		soloB  = "b2b2b2b2-0000-0000-0000-000000000002"
		stageB = "2b2b2b2b-0000-0000-0000-00000000000b"
		parent = "c3c3c3c3-0000-0000-0000-000000000003"
		child1 = "d4d4d4d4-0000-0000-0000-000000000004"
		child2 = "e5e5e5e5-0000-0000-0000-000000000005"
	)

	// --- Concurrent active phase: two solo runs and one parent+2-children
	// lineage drive the host at once. The children run sequentially WITHIN
	// their goroutine (decomposition is sequential by design); the three
	// shapes race each other. ---
	var (
		wg              sync.WaitGroup
		aRes, bRes      runResult
		linWT1, linTip1 string
		linWT2, linTip2 string
		linErr          error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		aRes = simulateSolo(ctx, repo, soloA, stageA, "a.txt", "A\n", client)
	}()
	go func() {
		defer wg.Done()
		bRes = simulateSolo(ctx, repo, soloB, stageB, "b.txt", "B\n", client)
	}()
	go func() {
		defer wg.Done()
		linWT1, linTip1, linErr = simulateChild(ctx, repo, parent, child1, "c1.txt", "C1\n", true, client)
		if linErr != nil {
			return
		}
		linWT2, linTip2, linErr = simulateChild(ctx, repo, parent, child2, "c2.txt", "C2\n", false, client)
	}()
	wg.Wait()

	for _, r := range []struct {
		name string
		err  error
	}{{"soloA", aRes.err}, {"soloB", bRes.err}, {"lineage", linErr}} {
		if r.err != nil {
			t.Fatalf("%s flow failed: %v", r.name, r.err)
		}
	}

	// (a) Solo isolation: the two solo runs landed in DISTINCT worktrees,
	// and each commit carries only its own file — neither solo's tree leaked
	// into the other's commit (which a shared working tree would have caused).
	if canonPath(aRes.wt) == canonPath(bRes.wt) {
		t.Errorf("solo runs shared a worktree: %q", aRes.wt)
	}
	if got := commitFiles(t, repo, aRes.commit); !equalStrings(got, []string{"a.txt"}) {
		t.Errorf("soloA commit diff = %v, want [a.txt]", got)
	}
	if got := commitFiles(t, repo, bRes.commit); !equalStrings(got, []string{"b.txt"}) {
		t.Errorf("soloB commit diff = %v, want [b.txt]", got)
	}
	if tree := treeFiles(t, repo, aRes.commit); containsStr(tree, "b.txt") {
		t.Errorf("soloA tree leaked soloB's file: %v", tree)
	}
	if tree := treeFiles(t, repo, bRes.commit); containsStr(tree, "a.txt") {
		t.Errorf("soloB tree leaked soloA's file: %v", tree)
	}

	// (b) Decomposed sharing: both children resolved to the SAME worktree
	// (reuse), distinct from the solo worktrees, and each child COMMIT
	// carries only its own diff while the shared branch is cumulative.
	if canonPath(linWT1) != canonPath(linWT2) {
		t.Errorf("decomposed children did not share a worktree: %q vs %q", linWT1, linWT2)
	}
	if canonPath(linWT1) == canonPath(aRes.wt) || canonPath(linWT1) == canonPath(bRes.wt) {
		t.Errorf("lineage worktree collided with a solo worktree: %q", linWT1)
	}
	if got := commitFiles(t, repo, linTip1); !equalStrings(got, []string{"c1.txt"}) {
		t.Errorf("child1 commit diff = %v, want [c1.txt] (per-commit isolation)", got)
	}
	if got := commitFiles(t, repo, linTip2); !equalStrings(got, []string{"c2.txt"}) {
		t.Errorf("child2 commit diff = %v, want [c2.txt] (per-commit isolation, layered on sibling)", got)
	}
	if tree := treeFiles(t, repo, linTip2); !containsStr(tree, "c1.txt") || !containsStr(tree, "c2.txt") {
		t.Errorf("shared branch tip is not cumulative: %v, want both c1.txt and c2.txt", tree)
	}

	// The operator's tracked tree stayed clean through provisioning AND
	// sharing — worktrees live under .git, invisible to git status, and the
	// operator HEAD never moved off the seed.
	if status := gitPorcelain(t, repo); status != "" {
		t.Errorf("operator git status not clean after provisioning + sharing:\n%s", status)
	}
	if head := gitPorcelainHead(t, repo); head != seed {
		t.Errorf("operator HEAD moved: %q, want seed %q", head, seed)
	}

	// (c) Lazy host-side reclamation: once the backend reports soloA's
	// lineage terminal, the NEXT provision on this host sweeps soloA's
	// worktree away while the still-live lineages (soloB, the parent
	// lineage) survive.
	client.setComplete(soloA, true)
	const (
		soloC  = "f6f6f6f6-0000-0000-0000-000000000006"
		stageC = "6f6f6f6f-0000-0000-0000-00000000000c"
	)
	cRes := simulateSolo(ctx, repo, soloC, stageC, "cc.txt", "CC\n", client)
	if cRes.err != nil {
		t.Fatalf("subsequent run C failed: %v", cRes.err)
	}
	if _, err := os.Stat(aRes.wt); !os.IsNotExist(err) {
		t.Errorf("terminal soloA worktree not swept: stat err = %v", err)
	}
	if st, err := os.Stat(bRes.wt); err != nil || !st.IsDir() {
		t.Errorf("live soloB worktree was swept: %v", err)
	}
	if st, err := os.Stat(linWT1); err != nil || !st.IsDir() {
		t.Errorf("live lineage worktree was swept: %v", err)
	}
	if st, err := os.Stat(cRes.wt); err != nil || !st.IsDir() {
		t.Errorf("run C worktree missing after provision: %v", err)
	}

	// Clean operator tree through the sweep, too (#1137 condition 2 — the
	// invariant holds across provisioning, sharing, AND sweep).
	if status := gitPorcelain(t, repo); status != "" {
		t.Errorf("operator git status not clean after sweep:\n%s", status)
	}
}

// TestWorktreeAdminLock_ConcurrentSweepWithLiveSibling is the load-bearing
// safety assertion for #1181 condition (2): it interleaves a sweep-remove of
// a TERMINAL lineage's worktree with a live sibling lineage's `git worktree
// add` against the SAME shared gitdir, and asserts both succeed, the live
// sibling survives + stays registered, the terminal worktree is gone, the
// operator tree stays clean, and neither commit cross-contaminated. Run under
// -race (with -count to repeat), an unguarded interleave — a `git worktree
// remove --force` racing a `git worktree add`/`list` — would trip here.
func TestWorktreeAdminLock_ConcurrentSweepWithLiveSibling(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	client := &syncLineageClient{complete: map[string]bool{}}

	seed := gitPorcelainHead(t, repo)
	if status := gitPorcelain(t, repo); status != "" {
		t.Fatalf("operator tree dirty before any run:\n%s", status)
	}

	const (
		termID    = "1eb11a10-0000-0000-0000-0000000000aa"
		sweeperID = "5deeb000-0000-0000-0000-0000000000bb"
		sweepStg  = "55aaee00-0000-0000-0000-0000000000b1"
		liveID    = "11ee0000-0000-0000-0000-0000000000cc"
		liveStg   = "11aaee00-0000-0000-0000-0000000000c1"
	)

	// Pre-provision a terminal lineage's worktree + sidecar, then mark it
	// complete so the next provision's sweep removes it.
	termRoot := lineageRoot(termID, "", false)
	termPath, err := provisionLineageWorktree(ctx, repo, termRoot, io.Discard)
	if err != nil {
		t.Fatalf("provision terminal lineage: %v", err)
	}
	writeLineageRunID(ctx, repo, termRoot, termID, io.Discard)
	client.setComplete(termID, true)

	// Concurrently: (i) a fresh solo run whose provision sweeps the terminal
	// worktree and adds its own; (ii) a fresh solo run of a DIFFERENT lineage
	// doing its own `git worktree add` against the same gitdir.
	var (
		wg                sync.WaitGroup
		sweepRes, liveRes runResult
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		sweepRes = simulateSolo(ctx, repo, sweeperID, sweepStg, "s.txt", "S\n", client)
	}()
	go func() {
		defer wg.Done()
		liveRes = simulateSolo(ctx, repo, liveID, liveStg, "l.txt", "L\n", client)
	}()
	wg.Wait()

	if sweepRes.err != nil {
		t.Fatalf("sweeping run failed: %v", sweepRes.err)
	}
	if liveRes.err != nil {
		t.Fatalf("live sibling run failed: %v", liveRes.err)
	}

	// The terminal lineage's worktree was swept away.
	if _, err := os.Stat(termPath); !os.IsNotExist(err) {
		t.Errorf("terminal worktree not swept: stat err = %v", err)
	}
	// The live sibling's worktree exists AND is registered against the gitdir
	// (the add was not corrupted by the concurrent remove).
	if st, err := os.Stat(liveRes.wt); err != nil || !st.IsDir() {
		t.Errorf("live sibling worktree missing: %v", err)
	}
	registered, err := listWorktreePaths(ctx, repo)
	if err != nil {
		t.Fatalf("list worktrees: %v", err)
	}
	if !isRegisteredWorktree(liveRes.wt, registered) {
		t.Errorf("live sibling worktree not registered: %q not in %v", liveRes.wt, registered)
	}
	if !isRegisteredWorktree(sweepRes.wt, registered) {
		t.Errorf("sweeping run's worktree not registered: %q not in %v", sweepRes.wt, registered)
	}

	// Neither commit cross-contaminated: each carries only its own file.
	if got := commitFiles(t, repo, sweepRes.commit); !equalStrings(got, []string{"s.txt"}) {
		t.Errorf("sweeping run commit diff = %v, want [s.txt]", got)
	}
	if got := commitFiles(t, repo, liveRes.commit); !equalStrings(got, []string{"l.txt"}) {
		t.Errorf("live sibling commit diff = %v, want [l.txt]", got)
	}
	if tree := treeFiles(t, repo, liveRes.commit); containsStr(tree, "s.txt") {
		t.Errorf("live sibling tree leaked the sweeper's file: %v", tree)
	}

	// The operator's tracked tree stayed clean through the concurrent
	// sweep+add, and its HEAD never moved.
	if status := gitPorcelain(t, repo); status != "" {
		t.Errorf("operator git status not clean after concurrent sweep+add:\n%s", status)
	}
	if head := gitPorcelainHead(t, repo); head != seed {
		t.Errorf("operator HEAD moved: %q, want seed %q", head, seed)
	}
}

// simulateIsolatedChild drives one decomposed child under --parallel-isolate
// (E24.4 / #1144): it provisions the child's OWN worktree (keyed on the child
// id, not the shared parent root), cuts the child's distinct per-slice
// sole-writer branch (E24.1 fishhawk/run-<parent>/slice-<n>) with --detach-free
// checkout -b, and commits one file. Safe to run from a goroutine.
func simulateIsolatedChild(ctx context.Context, repo, parentID, childID, sliceBranch, file, content string, client lineageStatusClient) (wt, commit string, err error) {
	wt, release, err := provisionFlow(ctx, repo, childID, parentID, true, client)
	if err != nil {
		return "", "", err
	}
	defer release()
	if err = runGitErr(wt, "checkout", "-b", sliceBranch); err != nil {
		return "", "", err
	}
	if err = commitFile(wt, file, content, "child "+shortID(childID)); err != nil {
		return "", "", err
	}
	commit, err = runGitOut(wt, "rev-parse", "HEAD")
	return wt, commit, err
}

// TestWorktreeIsolation_ParallelChildren is the runner-side real-git half of
// the E24.4 / #1144 cross-boundary contract: two decomposed children of ONE
// parent provision their worktrees CONCURRENTLY against the same shared gitdir
// under --parallel-isolate and must land in DISTINCT per-child checkouts
// (run-<child1> vs run-<child2>) — not the one shared run-<parent> tree the
// off path uses. Each child owns a distinct per-slice sole-writer branch, so
// the concurrent `git worktree add --detach` never trips git's same-branch
// refusal. The operator's tracked tree stays clean throughout. (The MCP→runner
// seam that actually passes --parallel-isolate is proven end to end by
// TestRunChildren_CrossBoundary_SpawnsRealRunners in the fishhawk-mcp package.)
func TestWorktreeIsolation_ParallelChildren(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	client := &syncLineageClient{complete: map[string]bool{}}

	seed := gitPorcelainHead(t, repo)
	if status := gitPorcelain(t, repo); status != "" {
		t.Fatalf("operator tree dirty before any run:\n%s", status)
	}

	const (
		parent = "c3c3c3c3-0000-0000-0000-000000000003"
		child1 = "d4d4d4d4-0000-0000-0000-000000000004"
		child2 = "e5e5e5e5-0000-0000-0000-000000000005"
	)
	slice1 := "fishhawk/run-" + shortID(parent) + "/slice-0"
	slice2 := "fishhawk/run-" + shortID(parent) + "/slice-1"

	var (
		wg         sync.WaitGroup
		wt1, wt2   string
		tip1, tip2 string
		err1, err2 error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		wt1, tip1, err1 = simulateIsolatedChild(ctx, repo, parent, child1, slice1, "c1.txt", "C1\n", client)
	}()
	go func() {
		defer wg.Done()
		wt2, tip2, err2 = simulateIsolatedChild(ctx, repo, parent, child2, slice2, "c2.txt", "C2\n", client)
	}()
	wg.Wait()

	if err1 != nil {
		t.Fatalf("child1 flow failed: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("child2 flow failed: %v", err2)
	}

	// DISTINCT per-child worktrees — the load-bearing parallel-isolate property.
	if canonPath(wt1) == canonPath(wt2) {
		t.Errorf("parallel-isolate children shared a worktree: %q", wt1)
	}
	// Each is keyed on the CHILD id, not the shared parent root.
	wtDir, err := worktreesDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	wantC1 := filepath.Join(wtDir, "run-"+shortID(child1))
	wantC2 := filepath.Join(wtDir, "run-"+shortID(child2))
	if canonPath(wt1) != canonPath(wantC1) {
		t.Errorf("child1 worktree = %q, want per-child %q", wt1, wantC1)
	}
	if canonPath(wt2) != canonPath(wantC2) {
		t.Errorf("child2 worktree = %q, want per-child %q", wt2, wantC2)
	}
	// Neither child's commit cross-contaminated: each carries only its own file.
	if got := commitFiles(t, repo, tip1); !equalStrings(got, []string{"c1.txt"}) {
		t.Errorf("child1 commit diff = %v, want [c1.txt]", got)
	}
	if got := commitFiles(t, repo, tip2); !equalStrings(got, []string{"c2.txt"}) {
		t.Errorf("child2 commit diff = %v, want [c2.txt]", got)
	}
	if tree := treeFiles(t, repo, tip2); containsStr(tree, "c1.txt") {
		t.Errorf("child2 isolated tree leaked child1's file: %v", tree)
	}
	// Both per-slice branches exist and are registered worktrees.
	registered, err := listWorktreePaths(ctx, repo)
	if err != nil {
		t.Fatalf("list worktrees: %v", err)
	}
	if !isRegisteredWorktree(wt1, registered) || !isRegisteredWorktree(wt2, registered) {
		t.Errorf("a per-child worktree is not registered: %v", registered)
	}
	// Operator tree clean, HEAD never moved.
	if status := gitPorcelain(t, repo); status != "" {
		t.Errorf("operator git status not clean after parallel-isolate children:\n%s", status)
	}
	if head := gitPorcelainHead(t, repo); head != seed {
		t.Errorf("operator HEAD moved: %q, want seed %q", head, seed)
	}
}

// gitPorcelainHead returns the operator checkout's HEAD sha, used to prove
// the run commits never move the operator's HEAD (they land on worktree
// HEADs instead).
func gitPorcelainHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := runGitOut(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return out
}
