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
func provisionFlow(ctx context.Context, repo, runID, decomposedFrom string, client lineageStatusClient) (wt string, release func(), err error) {
	root := lineageRoot(runID, decomposedFrom)
	sweepTerminalWorktrees(ctx, repo, client, io.Discard)
	wt, err = provisionLineageWorktree(ctx, repo, root, io.Discard)
	if err != nil {
		return "", nil, err
	}
	writeLineageRunID(ctx, repo, root, lineageRootFull(runID, decomposedFrom), io.Discard)
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
	wt, release, err := provisionFlow(ctx, repo, runID, "", client)
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
	wt, release, err := provisionFlow(ctx, repo, childID, parentID, client)
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
