package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/conflictresolve"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// The conflict-resolution wiring is tested against REAL temporary git
// repositories with a stub agent, one test per named refusal reason, and EVERY
// failure test asserts the recovery postcondition as well as the reason —
// because the contract this pass owes the operator is that a merge which starts
// and cannot finish leaves the branch tip and the working tree exactly as they
// were before the merge began. A test that asserted only the returned error
// would stay green with the whole recovery path deleted: the error is
// byte-identical with and without it, and only the COMMITTED STATE discriminates.

// gitT runs a git command in dir and fails the test on error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitTIn runs a git command with stdin, failing the test on error.
func gitTIn(t *testing.T, dir, stdin string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func writeFileT(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func readFileT(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// conflictRepo is a real repository sitting one command away from a conflicting
// merge: branch `work` (checked out) and branch `base` both changed the middle
// line of conflictPath, and `base` additionally carries a CLEAN change git will
// auto-stage during the merge — the authorized-baseline case.
type conflictRepo struct {
	dir          string
	preTip       string
	baseTip      string
	conflictPath string
}

// newConflictRepo builds that repository. conflictPath is a parameter so the
// special-character path case drives the SHIPPED enumeration rather than a
// parallel one.
func newConflictRepo(t *testing.T, conflictPath string) conflictRepo {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "work")
	gitT(t, dir, "config", "user.email", "runner@example.test")
	gitT(t, dir, "config", "user.name", "Fishhawk Test")
	// Never inherit an operator's global commit.gpgsign (#912): a down signing
	// agent would red-line these tests for an unrelated reason.
	gitT(t, dir, "config", "commit.gpgsign", "false")

	writeFileT(t, dir, conflictPath, "one\ntwo\nthree\n")
	writeFileT(t, dir, "other.txt", "untouched\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "root")

	gitT(t, dir, "checkout", "-q", "-b", "base")
	writeFileT(t, dir, conflictPath, "one\nBASE\nthree\n")
	writeFileT(t, dir, "clean-from-base.txt", "added on base\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base change")
	baseTip := gitT(t, dir, "rev-parse", "HEAD")

	gitT(t, dir, "checkout", "-q", "work")
	writeFileT(t, dir, conflictPath, "one\nWORK\nthree\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "work change")

	// A real `origin` (this repository itself) so `origin/<base>` resolves.
	// Production qualifies the served base BRANCH name with the runner's
	// remote, so the end-to-end routing tests exercise the shipped
	// conflictMergeRef path rather than a local-branch shortcut.
	gitT(t, dir, "remote", "add", "origin", dir)
	gitT(t, dir, "fetch", "-q", "origin")

	return conflictRepo{
		dir:          dir,
		preTip:       gitT(t, dir, "rev-parse", "HEAD"),
		baseTip:      baseTip,
		conflictPath: conflictPath,
	}
}

func (r conflictRepo) params(t *testing.T, invoke conflictResolutionAgent) conflictResolveParams {
	t.Helper()
	return conflictResolveParams{
		repoDir:  r.dir,
		mergeRef: "base",
		request:  conflictResolutionRequest{Branch: "work", BaseRef: "base", ExpectedHeadSHA: r.preTip},
		invoke:   invoke,
		logSink:  &strings.Builder{},
	}
}

// assertPreMergeStateRestored is the recovery POSTCONDITION every refusal owes:
// HEAD back at the pre-merge tip and an empty `git status --porcelain`. It reads
// the repository AFTER the call returned, so it discriminates on committed state
// and not on the (recovery-independent) error value.
func assertPreMergeStateRestored(t *testing.T, r conflictRepo) {
	t.Helper()
	if head := gitT(t, r.dir, "rev-parse", "HEAD"); head != r.preTip {
		t.Errorf("HEAD = %s, want pre-merge tip %s (recovery did not restore the branch tip)", head, r.preTip)
	}
	if status := gitT(t, r.dir, "status", "--porcelain"); status != "" {
		t.Errorf("git status --porcelain = %q, want empty (recovery left the working tree dirty)", status)
	}
}

// runRefusalCase drives one refusal end to end: real repo, real merge, stub
// agent, then both assertions.
func runRefusalCase(t *testing.T, r conflictRepo, invoke conflictResolutionAgent, wantReason string) conflictResolveResult {
	t.Helper()
	res := runConflictResolution(context.Background(), r.params(t, invoke))
	if res.Reason != wantReason {
		t.Errorf("reason = %q (detail %q), want %q", res.Reason, res.Detail, wantReason)
	}
	if res.CommitSHA != "" {
		t.Errorf("commit %s was created for a refused resolution", res.CommitSHA)
	}
	if !res.Recovered {
		t.Errorf("Recovered = false; the pass reported it could not restore the pre-merge state")
	}
	assertPreMergeStateRestored(t, r)
	return res
}

// resolveConflictedFile is the stub agent's honest resolution: keep OURS from
// every conflict hunk and drop the markers.
func resolveConflictedFile(t *testing.T, dir, rel string) {
	t.Helper()
	var kept []string
	side := 0 // 0 = outside a hunk, 1 = ours, 2 = base/theirs
	for _, line := range strings.SplitAfter(readFileT(t, dir, rel), "\n") {
		trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case strings.HasPrefix(trimmed, "<<<<<<<"):
			side = 1
		case strings.HasPrefix(trimmed, "|||||||"), strings.HasPrefix(trimmed, "======="):
			side = 2
		case strings.HasPrefix(trimmed, ">>>>>>>"):
			side = 0
		default:
			if side != 2 {
				kept = append(kept, line)
			}
		}
	}
	writeFileT(t, dir, rel, strings.Join(kept, ""))
}

// --- happy path -------------------------------------------------------------

// TestConflictResolve_CommitsResolutionWithBothParents drives the whole pass to
// a commit and asserts the merge's PARENT SET is exactly {pre-merge tip, base
// tip} — the artifact the confinement gate exists to authorize.
func TestConflictResolve_CommitsResolutionWithBothParents(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	res := runConflictResolution(context.Background(), r.params(t, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		return nil
	}))
	if res.Reason != "" {
		t.Fatalf("reason = %q (detail %q), want a committed resolution", res.Reason, res.Detail)
	}
	if res.CommitSHA == "" {
		t.Fatal("no commit SHA reported")
	}
	parents := strings.Fields(gitT(t, r.dir, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 3 || parents[1] != r.preTip || parents[2] != r.baseTip {
		t.Errorf("parents = %v, want [%s %s %s]", parents, res.CommitSHA, r.preTip, r.baseTip)
	}
	if got := readFileT(t, r.dir, r.conflictPath); got != "one\nWORK\nthree\n" {
		t.Errorf("resolved content = %q, want the OURS side with markers dropped", got)
	}
	if status := gitT(t, r.dir, "status", "--porcelain"); status != "" {
		t.Errorf("git status --porcelain = %q, want empty after the commit", status)
	}
	// The clean base change git auto-staged is part of the baseline and is
	// therefore AUTHORIZED: it must ride along in the merge commit.
	if _, err := os.Stat(filepath.Join(r.dir, "clean-from-base.txt")); err != nil {
		t.Errorf("clean base change is missing from the merge result: %v", err)
	}
}

// TestConflictResolve_GatesPathsWithSpecialCharacters drives the same happy path
// over a conflicted file whose name carries a space, a quote, a backslash and a
// non-ASCII byte. Git C-quotes such a path in its non-`-z` output, so an
// enumeration that did not split on NUL would mis-report it and the gate would
// be evadable by filename.
func TestConflictResolve_GatesPathsWithSpecialCharacters(t *testing.T) {
	path := `we ird "q\b" café.txt`
	r := newConflictRepo(t, path)
	res := runConflictResolution(context.Background(), r.params(t, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, path)
		return nil
	}))
	if res.Reason != "" {
		t.Fatalf("reason = %q (detail %q), want a committed resolution", res.Reason, res.Detail)
	}
	// Assert on the committed TREE, not on a name-status diff: the resolution
	// kept OURS, so the merge commit has no diff against its first parent — a
	// name-only listing would be empty for reasons unrelated to path handling.
	if got := gitT(t, r.dir, "show", "HEAD:"+path); got != "one\nWORK\nthree" {
		t.Errorf("committed content of %q = %q, want the resolved OURS side", path, got)
	}
	if status := gitT(t, r.dir, "status", "--porcelain"); status != "" {
		t.Errorf("git status --porcelain = %q, want empty; the special-character path was not gated by exact name", status)
	}
}

// --- pre-merge refusals -----------------------------------------------------

// TestConflictResolve_RefusesUnexpectedHead pins the pre-merge tip check: a base
// that advanced under the pass must not be merged unnoticed. Nothing has been
// mutated when it fires, so the postcondition holds trivially — which is the
// point: the refusal costs nothing.
func TestConflictResolve_RefusesUnexpectedHead(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	p := r.params(t, func(context.Context, string) error {
		t.Error("agent invoked after an unexpected-head refusal")
		return nil
	})
	p.request.ExpectedHeadSHA = strings.Repeat("0", 40)
	res := runConflictResolution(context.Background(), p)
	if res.Reason != reasonUnexpectedHead {
		t.Errorf("reason = %q, want %q", res.Reason, reasonUnexpectedHead)
	}
	assertPreMergeStateRestored(t, r)
}

// TestConflictResolve_AbortsWhenMergeHasNoConflict pins the no-conflict abort:
// the base advanced PAST the conflict between the trigger and the pass, so the
// merge succeeds — and pushing it would be an unauthorized merge the operator
// never approved.
func TestConflictResolve_AbortsWhenMergeHasNoConflict(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	// A ref that merges cleanly into `work`: it touches only a new file.
	gitT(t, r.dir, "checkout", "-q", "-b", "clean-base")
	writeFileT(t, r.dir, "clean-only.txt", "no conflict here\n")
	gitT(t, r.dir, "add", "-A")
	gitT(t, r.dir, "commit", "-q", "-m", "clean base change")
	gitT(t, r.dir, "checkout", "-q", "work")

	p := r.params(t, func(context.Context, string) error {
		t.Error("agent invoked for a merge that had no conflict")
		return nil
	})
	p.mergeRef = "clean-base"
	res := runConflictResolution(context.Background(), p)
	if res.Reason != reasonNoConflict {
		t.Errorf("reason = %q (detail %q), want %q", res.Reason, res.Detail, reasonNoConflict)
	}
	if !res.Recovered {
		t.Error("Recovered = false after a no-conflict abort")
	}
	assertPreMergeStateRestored(t, r)
}

// TestConflictResolve_RefusesUnresolvableMergeRef pins the merge-failed arm: the
// merge command failed for a reason that is NOT a content conflict, so there are
// no conflicted paths to confine an agent to.
func TestConflictResolve_RefusesUnresolvableMergeRef(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	p := r.params(t, func(context.Context, string) error {
		t.Error("agent invoked after a failed merge")
		return nil
	})
	p.mergeRef = "no/such/ref"
	res := runConflictResolution(context.Background(), p)
	if res.Reason != reasonMergeFailed {
		t.Errorf("reason = %q (detail %q), want %q", res.Reason, res.Detail, reasonMergeFailed)
	}
	assertPreMergeStateRestored(t, r)
}

// --- agent failure ----------------------------------------------------------

// TestConflictResolve_AgentFailureRestoresPreMergeTip is the recovery-path
// counterfactual vehicle. It reads HEAD and `git status --porcelain` back AFTER
// the call returns, so deleting the WHOLE recovery path (abort AND the verified
// reset/clean fallback) turns it red on the state assertions — the returned
// error is byte-identical either way.
func TestConflictResolve_AgentFailureRestoresPreMergeTip(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(context.Context, string) error {
		return errors.New("stub agent exited non-zero")
	}, reasonAgentFailed)
}

// --- confinement-gate refusals ---------------------------------------------

// TestConflictResolve_RefusesHeadMoved pins the HEAD input. The stub moves HEAD
// with `update-ref`, which leaves MERGE_HEAD, the index and the working tree
// untouched — so nothing but the HEAD comparison can catch it. It also exercises
// the recovery FALLBACK: `git merge --abort` restores to the MOVED head, so the
// postcondition check must fail it over to the verified hard reset.
func TestConflictResolve_RefusesHeadMoved(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	root := gitT(t, r.dir, "rev-parse", "HEAD~1")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		gitT(t, dir, "update-ref", "HEAD", root)
		return nil
	}, string(conflictresolve.ReasonHeadMoved))
}

// TestConflictResolve_RefusesMergeHeadChanged pins CONDITION 1's first input.
// MERGE_HEAD is the merge commit's SECOND PARENT: rewriting it changes the
// resulting commit's ancestry while HEAD, the index and the working tree stay
// byte-identical, so every other gate input reads clean.
func TestConflictResolve_RefusesMergeHeadChanged(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	root := gitT(t, r.dir, "rev-parse", "HEAD~1")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		gitDir := gitT(t, dir, "rev-parse", "--absolute-git-dir")
		if err := os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), []byte(root+"\n"), 0o644); err != nil {
			return err
		}
		return nil
	}, string(conflictresolve.ReasonMergeHeadChanged))
}

// TestConflictResolve_RefusesMergeMessageChanged pins CONDITION 1's second
// input. The merge message source is what `git commit --no-edit` writes, so an
// agent that rewrites it authors the commit's message — again with HEAD, the
// index and the working tree untouched.
func TestConflictResolve_RefusesMergeMessageChanged(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		gitDir := gitT(t, dir, "rev-parse", "--absolute-git-dir")
		return os.WriteFile(filepath.Join(gitDir, "MERGE_MSG"), []byte("attacker-authored message\n"), 0o644)
	}, string(conflictresolve.ReasonMergeMessageChanged))
}

// TestConflictResolve_RefusesNonConflictedIndexEntryChanged pins the index
// input: the agent restaged a file that was never in conflict.
func TestConflictResolve_RefusesNonConflictedIndexEntryChanged(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		writeFileT(t, dir, "other.txt", "smuggled\n")
		gitT(t, dir, "add", "other.txt")
		return nil
	}, string(conflictresolve.ReasonIndexEntryChanged))
}

// TestConflictResolve_RefusesNewlyStagedPath pins the newly-staged rule: a
// stage-0 entry for a path the baseline never carried.
func TestConflictResolve_RefusesNewlyStagedPath(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		writeFileT(t, dir, "smuggled.txt", "new file\n")
		gitT(t, dir, "add", "smuggled.txt")
		return nil
	}, string(conflictresolve.ReasonNewlyStagedPath))
}

// TestConflictResolve_RefusesUnstagedChangeOutsideSet pins the working-tree
// rule. The edit is never staged, so the index reads clean — only the
// worktree-vs-index comparison catches it.
func TestConflictResolve_RefusesUnstagedChangeOutsideSet(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		writeFileT(t, dir, "other.txt", "edited but not staged\n")
		return nil
	}, string(conflictresolve.ReasonUnstagedChangeOutsideSet))
}

// TestConflictResolve_RefusesUntrackedPathOutsideSet pins the untracked rule: a
// file the agent dropped in the tree without staging it is invisible to both the
// index and the worktree-vs-index diff.
func TestConflictResolve_RefusesUntrackedPathOutsideSet(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		writeFileT(t, dir, "dropped.txt", "untracked\n")
		return nil
	}, string(conflictresolve.ReasonUntrackedPathOutsideSet))
}

// TestConflictResolve_RefusesUnmergedEntryOutsideSet pins the unmerged rule: the
// agent hand-wrote conflict STAGES for a path that was never in conflict, which
// would let it ride into the commit as a "resolution".
//
// The path is one the baseline index never carried, deliberately: writing
// stages for an ALREADY-INDEXED path necessarily drops its stage-0 entry, and
// that mutation is pinned by TestConflictResolve_RefusesIndexEntryRemoved
// instead. Seeding a never-indexed path is what makes THIS rule the one that
// fires.
func TestConflictResolve_RefusesUnmergedEntryOutsideSet(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		oid := gitT(t, dir, "rev-parse", "HEAD:other.txt")
		gitTIn(t, dir, fmt.Sprintf("100644 %s 2\t%s\n100644 %s 3\t%s\n", oid, "smuggled.txt", oid, "smuggled.txt"),
			"update-index", "--index-info")
		return nil
	}, string(conflictresolve.ReasonUnmergedEntryOutsideSet))
}

// TestConflictResolve_RefusesIndexEntryRemoved pins the other half of the index
// rule: a baseline stage-0 entry that is simply GONE. Without it, an agent could
// drop a file from the merge by un-indexing it rather than by editing it.
func TestConflictResolve_RefusesIndexEntryRemoved(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		gitT(t, dir, "rm", "-q", "--cached", "other.txt")
		return nil
	}, string(conflictresolve.ReasonIndexEntryRemoved))
}

// TestConflictResolve_RefusesConflictedPathStagedByAgent pins the sole-writer
// rule over the index: the agent staged the conflicted path itself, which
// resolves its stages and takes the decision out of the gate's hands.
func TestConflictResolve_RefusesConflictedPathStagedByAgent(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		gitT(t, dir, "add", r.conflictPath)
		return nil
	}, string(conflictresolve.ReasonConflictedPathNotUnmerged))
}

// TestConflictResolve_RefusesEditOutsideHunk pins the hunk-level partition
// check: the resolution mutated a NON-CONFLICT segment of the conflicted file,
// which no other rule sees because the path is legitimately in the conflicted
// set.
func TestConflictResolve_RefusesEditOutsideHunk(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		// "one" and "three" are the non-conflict segments; "one" is mutated.
		writeFileT(t, dir, r.conflictPath, "ONE\nWORK\nthree\n")
		return nil
	}, string(conflictresolve.ReasonOutsideHunk))
}

// TestConflictResolve_RefusesResidualMarker pins the residual-marker check: the
// agent left the file exactly as git wrote it, markers and all, which is a
// "resolution" that would commit conflict markers to the run branch.
func TestConflictResolve_RefusesResidualMarker(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(context.Context, string) error {
		return nil // touch nothing
	}, string(conflictresolve.ReasonResidualMarker))
}

// TestConflictResolve_RefusesExecutableBitOnConflictedPath is the BEHAVIORAL
// refusal test for the file-mode rule (#3202 review).
//
// The stub agent resolves the conflict HONESTLY — the bytes it writes are the
// same accepted resolution the happy-path test commits — and additionally
// chmods the file executable without staging it. Every other gate input is
// therefore identical to the accepted case: HEAD, MERGE_HEAD and the merge
// message are untouched, no index entry moved, the path is still unmerged, and
// its working-tree change is permitted because the path IS the conflicted set.
// Only the mode rule can refuse, and without it the scoped `git add` would
// stage the executable bit into the merge commit.
func TestConflictResolve_RefusesExecutableBitOnConflictedPath(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		resolveConflictedFile(t, dir, r.conflictPath)
		return os.Chmod(filepath.Join(dir, r.conflictPath), 0o755)
	}, string(conflictresolve.ReasonConflictedModeChanged))
}

// TestConflictResolve_RefusesSymlinkSwapOnConflictedPath covers the other mode
// shape an unstaged working-tree write can reach: the conflicted regular file
// is replaced by a symlink, which `git add` would commit as a 120000 entry.
func TestConflictResolve_RefusesSymlinkSwapOnConflictedPath(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		full := filepath.Join(dir, r.conflictPath)
		if err := os.Remove(full); err != nil {
			return err
		}
		return os.Symlink("/etc/passwd", full)
	}, string(conflictresolve.ReasonConflictedModeChanged))
}

// TestConflictResolve_RefusesConflictedPathMissing pins the deletion rule for a
// CONTENT conflict: only a delete/modify conflict may be resolved by deletion.
func TestConflictResolve_RefusesConflictedPathMissing(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	runRefusalCase(t, r, func(_ context.Context, dir string) error {
		return os.Remove(filepath.Join(dir, r.conflictPath))
	}, string(conflictresolve.ReasonConflictedPathMissing))
}

// TestConflictResolve_RefusesBinaryConflict pins the binary case. Git leaves
// OURS on disk with NO markers for a file it cannot merge textually, so there is
// no hunk boundary to confine an edit to — the gate refuses rather than
// accepting whatever bytes the agent wrote. The file is marked `-merge` via
// .gitattributes, which is the mechanically identical presentation.
func TestConflictResolve_RefusesBinaryConflict(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "work")
	gitT(t, dir, "config", "user.email", "runner@example.test")
	gitT(t, dir, "config", "user.name", "Fishhawk Test")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	writeFileT(t, dir, ".gitattributes", "blob.dat -merge\n")
	writeFileT(t, dir, "blob.dat", "original\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "root")
	gitT(t, dir, "checkout", "-q", "-b", "base")
	writeFileT(t, dir, "blob.dat", "base bytes\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")
	gitT(t, dir, "checkout", "-q", "work")
	writeFileT(t, dir, "blob.dat", "work bytes\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "work")

	r := conflictRepo{dir: dir, preTip: gitT(t, dir, "rev-parse", "HEAD"), conflictPath: "blob.dat"}
	runRefusalCase(t, r, func(_ context.Context, d string) error {
		writeFileT(t, d, "blob.dat", "agent picked these bytes\n")
		return nil
	}, string(conflictresolve.ReasonBinaryConflict))
}

// TestConflictResolve_AcceptsDeleteModifyDeletion pins the delete/modify case:
// keeping the DELETION is one of the two admissible sides, expressed by the path
// being absent from the working tree rather than by any content.
func TestConflictResolve_AcceptsDeleteModifyDeletion(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "work")
	gitT(t, dir, "config", "user.email", "runner@example.test")
	gitT(t, dir, "config", "user.name", "Fishhawk Test")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	writeFileT(t, dir, "doomed.txt", "original\n")
	writeFileT(t, dir, "other.txt", "untouched\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "root")
	gitT(t, dir, "checkout", "-q", "-b", "base")
	writeFileT(t, dir, "doomed.txt", "modified on base\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base modifies")
	gitT(t, dir, "checkout", "-q", "work")
	gitT(t, dir, "rm", "-q", "doomed.txt")
	gitT(t, dir, "commit", "-q", "-m", "work deletes")

	preTip := gitT(t, dir, "rev-parse", "HEAD")
	res := runConflictResolution(context.Background(), conflictResolveParams{
		repoDir:  dir,
		mergeRef: "base",
		request:  conflictResolutionRequest{Branch: "work", BaseRef: "base", ExpectedHeadSHA: preTip},
		// Git leaves the MODIFIED side on disk for a modify/delete conflict, so
		// keeping the DELETION means removing it — absence IS the resolution.
		invoke:  func(_ context.Context, d string) error { return os.Remove(filepath.Join(d, "doomed.txt")) },
		logSink: &strings.Builder{},
	})
	if res.Reason != "" {
		t.Fatalf("reason = %q (detail %q), want the deletion accepted", res.Reason, res.Detail)
	}
	if _, err := os.Stat(filepath.Join(dir, "doomed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("doomed.txt still present after a delete-side resolution: %v", err)
	}
}

// TestConflictResolve_RefusesDeleteModifyThirdContent pins the other half of the
// delete/modify rule: content that is NEITHER side is not a resolution, it is an
// unreviewed edit smuggled in under a conflicted path.
func TestConflictResolve_RefusesDeleteModifyThirdContent(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "work")
	gitT(t, dir, "config", "user.email", "runner@example.test")
	gitT(t, dir, "config", "user.name", "Fishhawk Test")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	writeFileT(t, dir, "doomed.txt", "original\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "root")
	gitT(t, dir, "checkout", "-q", "-b", "base")
	writeFileT(t, dir, "doomed.txt", "modified on base\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base modifies")
	gitT(t, dir, "checkout", "-q", "work")
	gitT(t, dir, "rm", "-q", "doomed.txt")
	gitT(t, dir, "commit", "-q", "-m", "work deletes")

	r := conflictRepo{dir: dir, preTip: gitT(t, dir, "rev-parse", "HEAD"), conflictPath: "doomed.txt"}
	runRefusalCase(t, r, func(_ context.Context, d string) error {
		writeFileT(t, d, "doomed.txt", "neither side wrote this\n")
		return nil
	}, string(conflictresolve.ReasonDeleteModifyContent))
}

// --- capture helpers --------------------------------------------------------

// TestBaselineCapture_DeduplicatesConflictedStages pins the `git ls-files
// --unmerged` reading: it reports one record PER STAGE (1 = ancestor, 2 = ours,
// 3 = theirs), so a conflicted path appears up to three times and the set must
// be de-duplicated. A duplicated path would be added three times and, worse,
// would make the conflicted-set membership test depend on record order.
func TestBaselineCapture_DeduplicatesConflictedStages(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	if out, err := gitRun(context.Background(), r.dir, "merge", "--no-commit", "--no-ff", "base"); err == nil {
		t.Fatalf("merge did not conflict: %s", out)
	}
	t.Cleanup(func() { _, _ = gitRun(context.Background(), r.dir, "merge", "--abort") })

	unmerged, err := gitUnmergedPaths(context.Background(), r.dir)
	if err != nil {
		t.Fatalf("gitUnmergedPaths: %v", err)
	}
	if len(unmerged) != 1 {
		t.Fatalf("unmerged = %v, want exactly one de-duplicated path", unmerged)
	}
	if len(unmerged["file.txt"]) < 2 {
		t.Errorf("stages for file.txt = %v, want at least ours(2) and theirs(3)", unmerged["file.txt"])
	}
	base, err := captureBaseline(context.Background(), r.dir, unmerged)
	if err != nil {
		t.Fatalf("captureBaseline: %v", err)
	}
	if len(base.Conflicted) != 1 {
		t.Errorf("baseline conflicted = %d paths, want 1", len(base.Conflicted))
	}
	if _, ok := base.Index["file.txt"]; ok {
		t.Error("the conflicted path leaked into the baseline's non-conflicted index map")
	}
	// The clean base change git auto-staged IS part of the baseline, which is
	// what authorizes it to ride into the commit.
	if _, ok := base.Index["clean-from-base.txt"]; !ok {
		t.Error("the auto-staged clean base change is missing from the baseline index")
	}
	if base.MergeHeadSHA == "" || base.MergeHeadSHA != r.baseTip {
		t.Errorf("baseline MERGE_HEAD = %q, want the base tip %s", base.MergeHeadSHA, r.baseTip)
	}
	if base.MergeMessage == "" {
		t.Error("baseline merge message is empty; the commit message source was not captured")
	}
}

// TestReadMergeMessage_AbsentIsEmptyNotError pins the ABSENT-vs-unreadable
// distinction: removing MERGE_MSG is one of the tampering shapes the gate must
// catch as a CHANGED message, so reporting it as a read error would misroute it
// to the observe-failed arm and hide which rule was broken.
func TestReadMergeMessage_AbsentIsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "work")
	msg, err := readMergeMessage(context.Background(), dir)
	if err != nil {
		t.Fatalf("readMergeMessage on a repo with no MERGE_MSG: %v", err)
	}
	if msg != "" {
		t.Errorf("message = %q, want empty", msg)
	}
}

// TestSplitNUL_DropsEmptyTrailingRecord pins the NUL splitting used by every
// path enumeration: git's `-z` output is NUL-TERMINATED, not NUL-separated, so a
// naive split yields a trailing empty record that would become an empty path.
func TestSplitNUL_DropsEmptyTrailingRecord(t *testing.T) {
	got := splitNUL([]byte("a\x00b b\x00"))
	if len(got) != 2 || string(got[0]) != "a" || string(got[1]) != "b b" {
		t.Errorf("splitNUL = %q, want [a, b b]", got)
	}
}

// TestConflictResolutionFromPrompt pins the routing guard, one case per branch.
// A HALF-POPULATED instruction is deliberately NOT a pass: a runner handed an
// empty base ref would merge nothing, and the pass would then refuse with the
// no-conflict reason — a refusal naming the wrong cause, which is worse than
// not routing at all.
func TestConflictResolutionFromPrompt(t *testing.T) {
	full := &upload.FetchedPrompt{
		ConflictResolution:                true,
		ConflictResolutionBranch:          "fishhawk/run-abc/stage-def",
		ConflictResolutionBaseRef:         "main",
		ConflictResolutionExpectedHeadSHA: "cafe",
	}
	for _, tc := range []struct {
		name string
		got  *upload.FetchedPrompt
		want bool
	}{
		{"nil response", nil, false},
		{"ordinary implement dispatch", &upload.FetchedPrompt{}, false},
		{"flag set but no branch", &upload.FetchedPrompt{ConflictResolution: true, ConflictResolutionBaseRef: "main"}, false},
		{"flag set but no base ref", &upload.FetchedPrompt{ConflictResolution: true, ConflictResolutionBranch: "b"}, false},
		{"branch and base without the flag", &upload.FetchedPrompt{ConflictResolutionBranch: "b", ConflictResolutionBaseRef: "main"}, false},
		{"fully populated", full, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := conflictResolutionFromPrompt(tc.got)
			if (req != nil) != tc.want {
				t.Fatalf("conflictResolutionFromPrompt() != nil = %t, want %t", req != nil, tc.want)
			}
			if req != nil && (req.Branch != full.ConflictResolutionBranch ||
				req.BaseRef != full.ConflictResolutionBaseRef ||
				req.ExpectedHeadSHA != full.ConflictResolutionExpectedHeadSHA) {
				t.Errorf("request = %+v, want the served fields verbatim", *req)
			}
		})
	}
}

// TestConflictMergeRef pins the remote qualification: the backend serves a base
// BRANCH name because it does not know what the runner calls its remote, and an
// already-qualified ref must not be double-prefixed into origin/origin/main.
func TestConflictMergeRef(t *testing.T) {
	for in, want := range map[string]string{
		"main":            "origin/main",
		"origin/main":     "origin/main",
		"upstream/trunk":  "upstream/trunk",
		"refs/heads/main": "refs/heads/main",
	} {
		if got := conflictMergeRef(in); got != want {
			t.Errorf("conflictMergeRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRunConflictResolutionStage_UnreadablePromptFails pins the stage wrapper's
// pre-flight: an unreadable prompt file is a usage failure BEFORE any merge
// starts, so the repository is never touched.
func TestRunConflictResolutionStage_UnreadablePromptFails(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	var log strings.Builder
	code := runConflictResolutionStage(context.Background(),
		config{workingDir: r.dir, agent: "claude-code"},
		conflictResolutionRequest{Branch: "work", BaseRef: "base", ExpectedHeadSHA: r.preTip},
		filepath.Join(t.TempDir(), "absent-prompt.txt"), &log)
	if code != exitUsage {
		t.Errorf("exit = %d, want exitUsage", code)
	}
	if !strings.Contains(log.String(), `"reason":"read_prompt"`) {
		t.Errorf("log = %q, want a read_prompt failure", log.String())
	}
	assertPreMergeStateRestored(t, r)
}

// TestRunConflictResolutionStage_UnknownAgentFails pins the other pre-flight
// branch: an unknown agent id is rejected before the merge, not after it.
func TestRunConflictResolutionStage_UnknownAgentFails(t *testing.T) {
	r := newConflictRepo(t, "file.txt")
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("resolve it"), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	var log strings.Builder
	code := runConflictResolutionStage(context.Background(),
		config{workingDir: r.dir, agent: "no-such-agent"},
		conflictResolutionRequest{Branch: "work", BaseRef: "base", ExpectedHeadSHA: r.preTip},
		promptPath, &log)
	if code != exitUsage {
		t.Errorf("exit = %d, want exitUsage", code)
	}
	if !strings.Contains(log.String(), `"reason":"agent_select"`) {
		t.Errorf("log = %q, want an agent_select failure", log.String())
	}
	assertPreMergeStateRestored(t, r)
}
