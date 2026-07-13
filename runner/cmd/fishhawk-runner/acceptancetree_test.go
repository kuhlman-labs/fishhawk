package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// acceptanceTreeRepo builds a dispatch checkout whose main branch carries
// keep.txt AND deleted.txt, plus an out-of-worktree merge-candidate commit (on
// branch "mc", not checked out anywhere) that DELETES deleted.txt. It returns the
// dispatch dir and the merge-candidate SHA. This is the exact #1881 shape: the
// dispatch checkout sits on main (deleted.txt present) while the merge candidate
// removes it, so evaluating a "no live references remain" criterion against the
// dispatch tree false-fails and against the merge-candidate tree passes.
func acceptanceTreeRepo(t *testing.T) (dispatch, mcSHA string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dispatch = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dispatch
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dispatch, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dispatch, "deleted.txt"), []byte("gone soon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "main: keep + deleted")

	// The merge candidate deletes deleted.txt, committed on a branch we then
	// leave — so no worktree has it checked out (the #1881 precondition).
	git("checkout", "-q", "-b", "mc")
	git("rm", "-q", "deleted.txt")
	git("commit", "-q", "-m", "mc: delete deleted.txt")
	mcSHA = git("rev-parse", "HEAD")
	git("checkout", "-q", "main")
	return dispatch, mcSHA
}

// redirectAcceptanceTreeDir points acceptanceTreeDir at a throwaway temp dir so
// the provisioned checkout never lands in the shared /tmp path.
func redirectAcceptanceTreeDir(t *testing.T) {
	t.Helper()
	orig := acceptanceTreeDir
	acceptanceTreeDir = t.TempDir()
	t.Cleanup(func() { acceptanceTreeDir = orig })
}

func worktreeRegistered(t *testing.T, repoDir, target string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", repoDir, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	ct := canonPath(target)
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			if canonPath(strings.TrimSpace(p)) == ct {
				return true
			}
		}
	}
	return false
}

// TestAcceptanceTreePath pins the run/stage-keyed checkout path literal — the
// runner side of the byte-identical lockstep pair the prompt's AcceptanceTreePath
// mirrors (backend/internal/prompt/prompt.go). A drift on either side is caught
// by the paired literal tests.
func TestAcceptanceTreePath(t *testing.T) {
	orig := acceptanceTreeDir
	acceptanceTreeDir = "/tmp"
	defer func() { acceptanceTreeDir = orig }()
	const runID = "11111111-2222-3333-4444-555555555555"
	const stageID = "66666666-7777-8888-9999-000000000000"
	want := "/tmp/fishhawk-acceptance-tree-" + runID + "-" + stageID
	if got := acceptanceTreePath(runID, stageID); got != want {
		t.Errorf("acceptanceTreePath(%q,%q) = %q, want %q (the prompt mirrors this exact format)",
			runID, stageID, got, want)
	}
}

// TestProvisionAcceptanceTree_HappyPath is the cross-boundary integration test:
// provision the merge-candidate checkout from the dispatch clone and assert the
// deleted file is ABSENT in the provisioned checkout while still PRESENT in the
// dispatch checkout's main tree (the exact #1881 grep shape, now evaluated
// correctly), then teardown removes the directory and unregisters the worktree.
func TestProvisionAcceptanceTree_HappyPath(t *testing.T) {
	dispatch, mcSHA := acceptanceTreeRepo(t)
	redirectAcceptanceTreeDir(t)
	var log strings.Builder

	teardown := provisionAcceptanceTree(context.Background(), dispatch, mcSHA, "run1", "stage1", &log)
	target := acceptanceTreePath("run1", "stage1")

	if _, err := os.Stat(target); err != nil {
		t.Fatalf("provisioned checkout missing at %q: %v\nlog:\n%s", target, err, log.String())
	}
	// The merge-candidate tree does NOT carry deleted.txt...
	if _, err := os.Stat(filepath.Join(target, "deleted.txt")); !os.IsNotExist(err) {
		t.Errorf("deleted.txt must be ABSENT in the merge-candidate checkout (err=%v)", err)
	}
	// ...but keep.txt is present (it is a real checkout of the head).
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Errorf("keep.txt must be present in the merge-candidate checkout: %v", err)
	}
	// The dispatch checkout's main tree still carries deleted.txt — the wrong
	// tree the #1881 false failure grepped.
	if _, err := os.Stat(filepath.Join(dispatch, "deleted.txt")); err != nil {
		t.Errorf("deleted.txt must remain in the dispatch checkout's main tree: %v", err)
	}
	if !strings.Contains(log.String(), `"event":"acceptance_tree_provisioned"`) {
		t.Errorf("missing acceptance_tree_provisioned event:\n%s", log.String())
	}
	if !worktreeRegistered(t, dispatch, target) {
		t.Error("provisioned checkout is not a registered worktree")
	}

	teardown()
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("teardown must remove the checkout dir (err=%v)", err)
	}
	if worktreeRegistered(t, dispatch, target) {
		t.Error("teardown must unregister the worktree")
	}
	if !strings.Contains(log.String(), `"event":"acceptance_tree_removed"`) {
		t.Errorf("missing acceptance_tree_removed event:\n%s", log.String())
	}
}

// TestProvisionAcceptanceTree_EmptyHeadSHA: an empty merge-candidate expectation
// (pre-#1569 backend or an unresolvable lineage ledger) skips provisioning with
// acceptance_tree_skipped and returns a no-op teardown.
func TestProvisionAcceptanceTree_EmptyHeadSHA(t *testing.T) {
	dispatch, _ := acceptanceTreeRepo(t)
	redirectAcceptanceTreeDir(t)
	var log strings.Builder

	teardown := provisionAcceptanceTree(context.Background(), dispatch, "", "run1", "stage1", &log)
	if !strings.Contains(log.String(), `"event":"acceptance_tree_skipped"`) {
		t.Errorf("empty head SHA must emit acceptance_tree_skipped:\n%s", log.String())
	}
	if _, err := os.Stat(acceptanceTreePath("run1", "stage1")); !os.IsNotExist(err) {
		t.Error("no checkout must be provisioned for an empty head SHA")
	}
	teardown() // must be a safe no-op
}

// TestProvisionAcceptanceTree_NotAGitWorkTree: a dispatch dir that is not a git
// work tree (e.g. a GHA runner without a local checkout) skips provisioning.
func TestProvisionAcceptanceTree_NotAGitWorkTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	redirectAcceptanceTreeDir(t)
	notARepo := t.TempDir()
	var log strings.Builder

	teardown := provisionAcceptanceTree(context.Background(), notARepo, "deadbeef", "run1", "stage1", &log)
	if !strings.Contains(log.String(), `"event":"acceptance_tree_skipped"`) {
		t.Errorf("non-work-tree dispatch dir must emit acceptance_tree_skipped:\n%s", log.String())
	}
	if _, err := os.Stat(acceptanceTreePath("run1", "stage1")); !os.IsNotExist(err) {
		t.Error("no checkout must be provisioned when the dispatch dir is not a git work tree")
	}
	teardown()
}

// TestProvisionAcceptanceTree_ObjectUnfetchable: the head SHA is neither present
// locally nor fetchable (no reachable remote), so `worktree add` fails and the
// function warns with acceptance_tree_failed and returns a no-op teardown — the
// agent-spawn path proceeds unprovisioned (warn-and-proceed).
func TestProvisionAcceptanceTree_ObjectUnfetchable(t *testing.T) {
	dispatch, _ := acceptanceTreeRepo(t)
	redirectAcceptanceTreeDir(t)
	var log strings.Builder

	// A syntactically-valid but absent SHA; no origin remote is configured, so
	// the bare-SHA fetch cannot resolve it.
	const missing = "0123456789abcdef0123456789abcdef01234567"
	teardown := provisionAcceptanceTree(context.Background(), dispatch, missing, "run1", "stage1", &log)
	if !strings.Contains(log.String(), `"event":"acceptance_tree_failed"`) {
		t.Errorf("unfetchable object must emit acceptance_tree_failed:\n%s", log.String())
	}
	if strings.Contains(log.String(), `"event":"acceptance_tree_provisioned"`) {
		t.Error("no checkout must be provisioned for an unfetchable object")
	}
	if _, err := os.Stat(acceptanceTreePath("run1", "stage1")); !os.IsNotExist(err) {
		t.Error("no checkout dir must exist for an unfetchable object")
	}
	teardown() // no-op
}

// TestProvisionAcceptanceTree_StaleLeftoverSwept: a leftover directory at the
// keyed path from a crashed prior run is swept, then the checkout is provisioned
// successfully.
func TestProvisionAcceptanceTree_StaleLeftoverSwept(t *testing.T) {
	dispatch, mcSHA := acceptanceTreeRepo(t)
	redirectAcceptanceTreeDir(t)
	var log strings.Builder

	target := acceptanceTreePath("run1", "stage1")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "stale.txt"), []byte("crash residue\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	teardown := provisionAcceptanceTree(context.Background(), dispatch, mcSHA, "run1", "stage1", &log)
	defer teardown()
	if !strings.Contains(log.String(), `"event":"acceptance_tree_stale_swept"`) {
		t.Errorf("stale leftover must emit acceptance_tree_stale_swept:\n%s", log.String())
	}
	if !strings.Contains(log.String(), `"event":"acceptance_tree_provisioned"`) {
		t.Errorf("checkout must be provisioned after the sweep:\n%s", log.String())
	}
	// The stale residue is gone and the real checkout content is present.
	if _, err := os.Stat(filepath.Join(target, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale residue must be swept (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Errorf("provisioned checkout content missing after sweep: %v", err)
	}
}

// TestProvisionAcceptanceTree_TeardownRemoveFallback: when `git worktree remove`
// fails (here forced by locking the worktree so a single --force is refused),
// teardown falls back to unlock + os.RemoveAll + `git worktree prune` — the
// directory is removed AND the locked registration is unregistered (the unlock
// clears the lock a plain prune would otherwise skip), the fallback variant is
// logged, and the stage outcome is never affected.
func TestProvisionAcceptanceTree_TeardownRemoveFallback(t *testing.T) {
	dispatch, mcSHA := acceptanceTreeRepo(t)
	redirectAcceptanceTreeDir(t)
	var log strings.Builder

	teardown := provisionAcceptanceTree(context.Background(), dispatch, mcSHA, "run1", "stage1", &log)
	target := acceptanceTreePath("run1", "stage1")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("provision failed: %v\n%s", err, log.String())
	}
	// Lock the worktree so `git worktree remove --force` (single --force) is
	// refused, forcing the rm+prune fallback.
	if out, err := exec.Command("git", "-C", dispatch, "worktree", "lock", target).CombinedOutput(); err != nil {
		t.Fatalf("git worktree lock: %v\n%s", err, out)
	}

	teardown()
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("fallback must remove the checkout dir despite the lock (err=%v)", err)
	}
	if !strings.Contains(log.String(), `"fallback":"rm_prune"`) {
		t.Errorf("teardown remove failure must take the rm_prune fallback:\n%s", log.String())
	}
	// The unlock+prune fallback must ALSO unregister the locked worktree — a plain
	// prune skips locked entries, so without the unlock the registration would
	// persist in the dispatch repo's admin area and acceptance_tree_removed would
	// overclaim.
	if worktreeRegistered(t, dispatch, target) {
		t.Error("fallback must unregister the locked worktree, not leave a stranded registration")
	}
}
