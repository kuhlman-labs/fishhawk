package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Hermetic fixtures for the plan-declared counterfactual executor (E85.3 /
// #4143). Each test builds a tiny committed Go module in a t.TempDir git repo
// standing in for the lineage worktree and drives the REAL `go test` through
// the REAL runBoundedGateArgvDisposed host seam, so test2json action
// semantics are pinned against the toolchain the repo pins.

var cfFixtureFiles = map[string]string{
	"go.mod": "module example.com/cf\n\ngo 1.25\n",
	"ctl.go": "package cf\n\n// Allowed is the pinned control.\nfunc Allowed(n int) bool { return n > 0 }\n",
	"unpinned.go": "package cf\n\n// Unpinned is a control no test exercises.\nfunc Unpinned(n int) bool { return n > 1 }\n\n" +
		"// Seven and AlsoSeven make `return 7` ambiguous.\nfunc Seven() int { return 7 }\n\nfunc AlsoSeven() int { return 7 }\n",
	"delay.go":       "package cf\n\nimport \"time\"\n\n// Delay is what TestSlow sleeps.\nfunc Delay() time.Duration { return 0 }\n",
	"tagged.go":      "//go:build never\n\npackage cf\n\n// Tagged is build-excluded.\nfunc Tagged() bool { return true }\n",
	"ctl_test.go":    "package cf\n\nimport \"testing\"\n\nfunc TestAllowed(t *testing.T) {\n\tif Allowed(0) {\n\t\tt.Fatal(\"Allowed(0) must be false\")\n\t}\n\tif !Allowed(1) {\n\t\tt.Fatal(\"Allowed(1) must be true\")\n\t}\n}\n",
	"slow_test.go":   "package cf\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestSlow(t *testing.T) { time.Sleep(Delay()) }\n\nvar _ = time.Second\n",
	"broken_test.go": "package cf\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) { t.Fatal(\"always broken\") }\n",
}

func cfGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newCFFixture commits the fixture module and returns (repo, head, tree).
// The returned head is a commit NO ref points to (a "wip" commit the branch
// was reset off), the shape a verify-fix loop leaves behind.
func newCFFixture(t *testing.T) (repo, head, tree string) {
	t.Helper()
	repo = t.TempDir()
	cfGit(t, repo, "init", "-q", "-b", "main")
	for name, body := range cfFixtureFiles {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfGit(t, repo, "add", "-A")
	cfGit(t, repo, "commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfGit(t, repo, "add", "-A")
	cfGit(t, repo, "commit", "-q", "-m", "wip")
	head = cfGit(t, repo, "rev-parse", "HEAD")
	tree = cfGit(t, repo, "rev-parse", "HEAD^{tree}")
	cfGit(t, repo, "reset", "-q", "--hard", "HEAD~1")
	return repo, head, tree
}

func newTestCFExecutor(t *testing.T, repo string) *counterfactualExecutor {
	t.Helper()
	x := newCounterfactualExecutor(context.Background(), repo)
	if x.lockPath == "" {
		t.Fatal("lock path did not resolve for the fixture repo")
	}
	x.runTimeout = 2 * time.Minute
	x.reacquireDelay = 10 * time.Millisecond
	return x
}

func replaceSpec(i int, control, find, replace, run string) upload.CounterfactualSpec {
	return upload.CounterfactualSpec{Index: i, ControlPath: control, MutationKind: upload.CounterfactualMutationReplace,
		Find: find, Replace: replace, TestRun: run, TestPackage: ".", Expect: "red"}
}

func patchSpec(i int, control, patch string) upload.CounterfactualSpec {
	return upload.CounterfactualSpec{Index: i, ControlPath: control, MutationKind: upload.CounterfactualMutationPatch,
		Patch: patch, TestRun: "^TestAllowed$", TestPackage: ".", Expect: "red"}
}

func runCF(t *testing.T, x *counterfactualExecutor, repo, head, tree string, specs ...upload.CounterfactualSpec) []counterfactualObservationEvidence {
	t.Helper()
	obs := runCounterfactualPhase(context.Background(), x, repo, head, tree, specs)
	if len(obs) != len(specs) {
		t.Fatalf("observations = %d, want one per spec (%d)", len(obs), len(specs))
	}
	return obs
}

func wantObs(t *testing.T, o counterfactualObservationEvidence, observed, reason string) {
	t.Helper()
	if o.Observed != observed || o.Reason != reason {
		t.Fatalf("spec %d: observed=%q reason=%q (baseline %q), want observed=%q reason=%q", o.Index, o.Observed, o.Reason, o.Baseline, observed, reason)
	}
}

const pinnedFind = "return n > 0"

func TestCounterfactual_RedForPinnedControl(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree, replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeRed, "")
	if obs[0].Baseline != cfOutcomeGreen {
		t.Fatalf("baseline = %q, want green", obs[0].Baseline)
	}
	if obs[0].HeadSHA != head || obs[0].TreeSHA != tree {
		t.Fatalf("head/tree = %s/%s, want %s/%s", obs[0].HeadSHA, obs[0].TreeSHA, head, tree)
	}
	if len(obs[0].MutationSHA256) != 64 || obs[0].TimeoutSeconds != 120 {
		t.Fatalf("mutation_sha256=%q timeout_seconds=%d", obs[0].MutationSHA256, obs[0].TimeoutSeconds)
	}
}

func TestCounterfactual_GreenForUnpinnedControl(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree, replaceSpec(3, "unpinned.go", "return n > 1", "return true", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeGreen, "")
	if obs[0].Index != 3 {
		t.Fatalf("index = %d, want 3", obs[0].Index)
	}
}

func TestCounterfactual_LineageWorktreeUnmodified(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	before, _ := os.ReadFile(filepath.Join(repo, "ctl.go"))
	headBefore := cfGit(t, repo, "rev-parse", "HEAD")
	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree,
		replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestAllowed$"),
		replaceSpec(1, "unpinned.go", "return n > 1", "return true", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeRed, "")
	wantObs(t, obs[1], cfOutcomeGreen, "")
	if st := cfGit(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("lineage worktree dirty after the phase:\n%s", st)
	}
	after, _ := os.ReadFile(filepath.Join(repo, "ctl.go"))
	if !bytes.Equal(before, after) || cfGit(t, repo, "rev-parse", "HEAD") != headBefore {
		t.Fatal("lineage ctl.go or HEAD changed")
	}
}

func TestCounterfactual_TimeoutRecorded(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	x := newTestCFExecutor(t, repo)
	x.runTimeout = 20 * time.Second
	obs := runCF(t, x, repo, head, tree, replaceSpec(0, "delay.go", "return 0", "return 10 * time.Minute", "^TestSlow$"))
	wantObs(t, obs[0], cfOutcomeTimeout, "")
	if obs[0].Baseline != cfOutcomeGreen {
		t.Fatalf("baseline = %q, want green", obs[0].Baseline)
	}
}

func TestCounterfactual_BuildFailIsNotRed(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree, replaceSpec(0, "ctl.go", pinnedFind, "return undefinedSymbol", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeError, cfReasonBuildFailed)
}

func TestCounterfactual_NoTestsMatched(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	// The baseline itself matches nothing, so it is not green; the
	// classifier's own no_tests_matched verdict is pinned on the real
	// stream below.
	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree, replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestNothingMatchesThis$"))
	wantObs(t, obs[0], cfOutcomeError, cfReasonBaselineNotGreen)
	if obs[0].Baseline != cfOutcomeError {
		t.Fatalf("baseline = %q, want error (no_tests_matched)", obs[0].Baseline)
	}
	out, code, disp := runBoundedGateArgvDisposed(context.Background(), []string{"go", "test", "-json", "-count=1", "-run=^TestNothingMatchesThis$", "."}, repo, t.TempDir(), time.Minute)
	if got, reason := classifyGoTestJSON(out, code, disp); got != cfOutcomeError || reason != cfReasonNoTestsMatched {
		t.Fatalf("classify(no match) = %q/%q, want error/no_tests_matched; exit %d\n%s", got, reason, code, out)
	}
}

func TestCounterfactual_BaselineNotGreen(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree, replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestBroken$"))
	wantObs(t, obs[0], cfOutcomeError, cfReasonBaselineNotGreen)
	if obs[0].Baseline != cfOutcomeRed || obs[0].MutationSHA256 != "" {
		t.Fatalf("baseline=%q mutation_sha256=%q, want red and empty", obs[0].Baseline, obs[0].MutationSHA256)
	}
}

func TestCounterfactual_MutationAnchorMissingAndAmbiguous(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree,
		replaceSpec(0, "ctl.go", "return n >= 0", "return true", "^TestAllowed$"),
		replaceSpec(1, "unpinned.go", "return 7", "return 8", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeError, cfReasonMutationNotApplied)
	wantObs(t, obs[1], cfOutcomeError, cfReasonMutationAmbiguous)
}

// cfStagedPatch makes a change in the fixture with mutate, captures it as a
// `git diff --cached -M --binary` patch, and restores the fixture.
func cfStagedPatch(t *testing.T, repo string, mutate func()) string {
	t.Helper()
	mutate()
	cfGit(t, repo, "add", "-A")
	cmd := exec.Command("git", "diff", "--cached", "-M", "--binary")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	cfGit(t, repo, "reset", "-q", "--hard", "HEAD")
	cfGit(t, repo, "clean", "-fdxq")
	return string(out)
}

func TestCounterfactual_PatchForm(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	ctl := filepath.Join(repo, "ctl.go")
	textPatch := cfStagedPatch(t, repo, func() {
		_ = os.WriteFile(ctl, []byte(strings.Replace(cfFixtureFiles["ctl.go"], pinnedFind, "return true", 1)), 0o644)
	})
	otherPatch := cfStagedPatch(t, repo, func() {
		_ = os.WriteFile(filepath.Join(repo, "unpinned.go"), []byte(strings.Replace(cfFixtureFiles["unpinned.go"], "return n > 1", "return true", 1)), 0o644)
	})
	deletePatch := cfStagedPatch(t, repo, func() { _ = os.Remove(ctl) })
	binaryPatch := cfStagedPatch(t, repo, func() { _ = os.WriteFile(ctl, []byte("\x00\x01\x02binary\x00"), 0o644) })
	renamePatch := cfStagedPatch(t, repo, func() { _ = os.Rename(ctl, filepath.Join(repo, "moved.go")) })
	modePatch := "diff --git a/ctl.go b/ctl.go\nold mode 100644\nnew mode 100755\n"
	createPatch := "diff --git a/ctl.go b/ctl.go\nnew file mode 100644\n--- /dev/null\n+++ b/ctl.go\n@@ -0,0 +1 @@\n+package cf\n"
	staleContext := "--- a/ctl.go\n+++ b/ctl.go\n@@ -1 +1 @@\n-this line is not in ctl.go\n+package cf\n"

	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree,
		patchSpec(0, "ctl.go", textPatch),
		patchSpec(1, "ctl.go", otherPatch),
		patchSpec(2, "ctl.go", deletePatch),
		patchSpec(3, "ctl.go", binaryPatch),
		patchSpec(4, "ctl.go", renamePatch),
		patchSpec(5, "ctl.go", modePatch),
		patchSpec(6, "ctl.go", createPatch),
		patchSpec(7, "ctl.go", staleContext),
	)
	wantObs(t, obs[0], cfOutcomeRed, "")
	for i := 1; i <= 6; i++ {
		wantObs(t, obs[i], cfOutcomeError, cfReasonMutationOutside)
	}
	wantObs(t, obs[7], cfOutcomeError, cfReasonMutationNotApplied)
}

func TestCheckPatchConfinedToControl(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"1\t1\tctl.go\n":   "",
		"-\t-\tctl.go\n":   cfReasonMutationOutside,
		"1\t1\tother.go\n": cfReasonMutationOutside,
		"0\t0\tctl.go\n mode change 100644 => 100755 ctl.go\n": cfReasonMutationOutside,
		"1\t0\tctl.go\n create mode 100644 ctl.go\n":           cfReasonMutationOutside,
		"0\t3\tctl.go\n delete mode 100644 ctl.go\n":           cfReasonMutationOutside,
		" copy a.go => ctl.go (100%)\n":                        cfReasonMutationOutside,
		"garbage line\n":                                       cfReasonMutationOutside,
		"":                                                     cfReasonMutationNotApplied,
	}
	for in, want := range cases {
		if got := checkPatchConfinedToControl(in, "ctl.go"); got != want {
			t.Errorf("checkPatchConfinedToControl(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCounterfactual_RestoredBetweenSpecs(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree,
		replaceSpec(0, "ctl.go", pinnedFind, "return n < 0", "^TestAllowed$"),
		replaceSpec(1, "ctl.go", pinnedFind, "return true", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeRed, "")
	wantObs(t, obs[1], cfOutcomeRed, "")
}

func TestCounterfactual_SymlinkControlRefused(t *testing.T) {
	t.Parallel()
	repo, _, _ := newCFFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.go")
	const outsideBody = "package outside\n\nfunc X() bool { return n > 0 }\n"
	if err := os.WriteFile(outside, []byte(outsideBody), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "ctl.go"), []byte(outsideBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "link.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outDir, filepath.Join(repo, "linkdir")); err != nil {
		t.Fatal(err)
	}
	cfGit(t, repo, "add", "-A")
	cfGit(t, repo, "commit", "-q", "-m", "symlinks")
	head := cfGit(t, repo, "rev-parse", "HEAD")
	tree := cfGit(t, repo, "rev-parse", "HEAD^{tree}")
	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree,
		replaceSpec(0, "link.go", "return n > 0", "return true", "^TestAllowed$"),
		replaceSpec(1, "linkdir/ctl.go", "return n > 0", "return true", "^TestAllowed$"),
		patchSpec(2, "link.go", "--- a/link.go\n+++ b/link.go\n@@ -3 +3 @@\n-func X() bool { return n > 0 }\n+func X() bool { return true }\n"))
	for _, o := range obs {
		wantObs(t, o, cfOutcomeError, cfReasonInvalidSpec)
	}
	for _, p := range []string{outside, filepath.Join(outDir, "ctl.go")} {
		if got, _ := os.ReadFile(p); string(got) != outsideBody {
			t.Fatalf("symlink target %s was modified: %q", p, got)
		}
	}
}

// The committed sub/ctl.go and the OUTSIDE file share the mutation anchor and
// the patch preimage, so a mutation that followed a planted symlink would land
// on the outside file; their package clauses differ so any write is visible.
const (
	cfSubCtlBody     = "package sub\n\n// X is the control.\nfunc X(n int) bool { return n > 0 }\n"
	cfOutsideCtlBody = "package outside\n\n// X is the control.\nfunc X(n int) bool { return n > 0 }\n"
	cfSubPatch       = "--- a/sub/ctl.go\n+++ b/sub/ctl.go\n@@ -4 +4 @@\n-func X(n int) bool { return n > 0 }\n+func X(n int) bool { return true }\n"
)

// TestCounterfactual_BaselinePlantedSymlinkRefused (fix-up of the C2 review
// finding): the baseline executes repository code, which on the container
// path can rewrite the writable checkout. Here the "baseline" replaces the
// control's intermediate directory with a symlink to an outside directory
// AFTER the pre-run C2 walk passed; the mutation boundary must refuse it for
// both mutation forms (invalid_spec, so the re-walk — not git apply's own
// beyond-symlink refusal — is what fired), run no mutated test, and leave the
// outside target byte-identical through the mutation AND the restore.
func TestCounterfactual_BaselinePlantedSymlinkRefused(t *testing.T) {
	t.Parallel()
	repo, _, _ := newCFFixture(t)
	if err := os.Mkdir(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "ctl.go"), []byte(cfSubCtlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cfGit(t, repo, "add", "-A")
	cfGit(t, repo, "commit", "-q", "-m", "sub")
	head := cfGit(t, repo, "rev-parse", "HEAD")
	tree := cfGit(t, repo, "rev-parse", "HEAD^{tree}")
	outDir := t.TempDir()
	outside := filepath.Join(outDir, "ctl.go")
	if err := os.WriteFile(outside, []byte(cfOutsideCtlBody), 0o644); err != nil {
		t.Fatal(err)
	}

	x := newTestCFExecutor(t, repo)
	var tests int
	x.exec = func(ctx context.Context, argv []string, dir, lc string, to time.Duration) (string, int, gateDisposition) {
		if argv[1] == "list" {
			return runBoundedGateArgvDisposed(ctx, argv, dir, lc, to)
		}
		tests++
		sub := filepath.Join(dir, "sub")
		if err := os.RemoveAll(sub); err != nil {
			t.Errorf("remove %s: %v", sub, err)
		}
		if err := os.Symlink(outDir, sub); err != nil {
			t.Errorf("plant symlink: %v", err)
		}
		return `{"Action":"pass","Package":"example.com/cf/sub","Test":"TestX"}` + "\n", 0, gateExecuted
	}
	replace := replaceSpec(0, "sub/ctl.go", "return n > 0", "return true", "^TestX$")
	replace.TestPackage = "./sub"
	patch := patchSpec(1, "sub/ctl.go", cfSubPatch)
	patch.TestRun, patch.TestPackage = "^TestX$", "./sub"
	obs := runCF(t, x, repo, head, tree, replace, patch)
	for _, o := range obs {
		wantObs(t, o, cfOutcomeError, cfReasonInvalidSpec)
		if o.Baseline != cfOutcomeGreen || o.MutationSHA256 != "" {
			t.Fatalf("spec %d: baseline=%q mutation_sha256=%q, want green and empty", o.Index, o.Baseline, o.MutationSHA256)
		}
	}
	if tests != 2 {
		t.Fatalf("go test ran %d times, want 2 (the baselines only, no mutated run)", tests)
	}
	if got, _ := os.ReadFile(outside); string(got) != cfOutsideCtlBody {
		t.Fatalf("outside target was modified through the planted symlink: %q", got)
	}
}

// TestReplaceInControlFile_RootRefusesEscape isolates the os.Root half of the
// mutation boundary: with NO component walk in front of it, a find/replace
// whose intermediate directory is a symlink out of the clone still neither
// reads nor writes the outside file.
func TestReplaceInControlFile_RootRefusesEscape(t *testing.T) {
	t.Parallel()
	clone, outDir := t.TempDir(), t.TempDir()
	outside := filepath.Join(outDir, "ctl.go")
	if err := os.WriteFile(outside, []byte(cfOutsideCtlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outDir, filepath.Join(clone, "sub")); err != nil {
		t.Fatal(err)
	}
	s := replaceSpec(0, "sub/ctl.go", "return n > 0", "return true", "^TestX$")
	if sum, reason := replaceInControlFile(clone, s); sum != "" || reason != cfReasonInvalidSpec {
		t.Fatalf("replaceInControlFile through an escaping symlink = (%q, %q), want (\"\", %q)", sum, reason, cfReasonInvalidSpec)
	}
	if got, _ := os.ReadFile(outside); string(got) != cfOutsideCtlBody {
		t.Fatalf("outside target was modified: %q", got)
	}
	// The same helper does mutate a contained file.
	if err := os.Remove(filepath.Join(clone, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(clone, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "sub", "ctl.go"), []byte(cfSubCtlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if sum, reason := replaceInControlFile(clone, s); len(sum) != 64 || reason != "" {
		t.Fatalf("contained replace = (%q, %q)", sum, reason)
	}
	if got, _ := os.ReadFile(filepath.Join(clone, "sub", "ctl.go")); !strings.Contains(string(got), "return true") {
		t.Fatalf("contained replace did not write: %q", got)
	}
}

func TestContainedControlFile_EvalSymlinksEscape(t *testing.T) {
	t.Parallel()
	// The Lstat walk refuses symlink components; this pins the second line,
	// the EvalSymlinks containment, on a clone root that is itself reached
	// through a symlink (as /var -> /private/var is on macOS).
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := containedControlFile(alias, "a.go"); err != nil {
		t.Fatalf("a regular file under a symlinked clone root must be accepted: %v", err)
	}
	for _, bad := range []string{"missing.go", "a.go/x", "."} {
		if _, err := containedControlFile(real, bad); err == nil {
			t.Fatalf("containedControlFile(%q) = nil error", bad)
		}
	}
	if err := os.Mkdir(filepath.Join(real, "d.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := containedControlFile(real, "d.go"); err == nil {
		t.Fatal("a directory leaf must be refused")
	}
}

func TestCounterfactual_ControlNotBuilt(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	obs := runCF(t, newTestCFExecutor(t, repo), repo, head, tree, replaceSpec(0, "tagged.go", "return true", "return false", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeError, cfReasonControlNotBuilt)
	if obs[0].Baseline != cfOutcomeSkipped {
		t.Fatalf("baseline = %q, want skipped (nothing ran)", obs[0].Baseline)
	}
}

// lockProbe wraps the real exec seam, recording the verify lock's state and
// the in-flight count at every `go test` invocation.
type lockProbe struct {
	mu        sync.Mutex
	lockPath  string
	inFlight  int
	maxFlight int
	tests     int
	badLink   []string
	starts    []time.Time
	ends      []time.Time
}

func (p *lockProbe) exec(ctx context.Context, argv []string, dir, lintCacheDir string, timeout time.Duration) (string, int, gateDisposition) {
	isTest := len(argv) > 1 && argv[1] == "test"
	if isTest {
		p.mu.Lock()
		p.tests++
		p.inFlight++
		if p.inFlight > p.maxFlight {
			p.maxFlight = p.inFlight
		}
		p.starts = append(p.starts, time.Now())
		if got, err := os.Readlink(p.lockPath); err != nil || got != strconv.Itoa(os.Getpid())+":runner" {
			p.badLink = append(p.badLink, got+" "+errString(err))
		}
		p.mu.Unlock()
	}
	out, code, disp := runBoundedGateArgvDisposed(ctx, argv, dir, lintCacheDir, timeout)
	if isTest {
		p.mu.Lock()
		p.inFlight--
		p.ends = append(p.ends, time.Now())
		p.mu.Unlock()
	}
	return out, code, disp
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestCounterfactual_HoldsVerifyLockPerExecution(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	x := newTestCFExecutor(t, repo)
	x.reacquireDelay = 300 * time.Millisecond
	p := &lockProbe{lockPath: x.lockPath}
	x.exec = p.exec
	obs := runCF(t, x, repo, head, tree,
		replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestAllowed$"),
		replaceSpec(1, "unpinned.go", "return n > 1", "return true", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeRed, "")
	wantObs(t, obs[1], cfOutcomeGreen, "")
	if p.tests != 4 || p.maxFlight != 1 || len(p.badLink) != 0 {
		t.Fatalf("go test runs=%d max in-flight=%d bad lock reads=%v; want 4, 1, none", p.tests, p.maxFlight, p.badLink)
	}
	for i := 1; i < len(p.starts); i++ {
		if gap := p.starts[i].Sub(p.ends[i-1]); gap < x.reacquireDelay {
			t.Fatalf("re-acquire gap %v < %v (C4)", gap, x.reacquireDelay)
		}
	}
	if _, err := os.Lstat(x.lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock link still present after the phase: %v", err)
	}
}

func TestCounterfactual_LockHeldByLiveHolder(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	x := newTestCFExecutor(t, repo)
	x.lockWait = time.Second
	x.lockPoll = 100 * time.Millisecond
	p := &lockProbe{lockPath: x.lockPath}
	x.exec = p.exec
	if err := os.Symlink("1:shell", x.lockPath); err != nil {
		t.Fatal(err)
	}
	obs := runCF(t, x, repo, head, tree, replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeError, cfReasonLockUnavailable)
	if p.tests != 0 {
		t.Fatalf("go test ran %d times under a foreign holder", p.tests)
	}
	if got, _ := os.Readlink(x.lockPath); got != "1:shell" {
		t.Fatalf("foreign holder was pruned or stolen: %q", got)
	}
}

func TestCounterfactual_NoLockPathFailsClosed(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	x := newTestCFExecutor(t, repo)
	x.lockPath = ""
	obs := runCF(t, x, repo, head, tree, replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeError, cfReasonLockUnavailable)
}

func TestVerifyLockRelease_OnlyOwnHolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	x := &counterfactualExecutor{lockPath: filepath.Join(dir, "fishhawk-verify.lock"), pid: os.Getpid(),
		lockWait: time.Second, lockPoll: 10 * time.Millisecond, now: time.Now}
	release, err := x.acquireLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(x.lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("424242:shell", x.lockPath); err != nil {
		t.Fatal(err)
	}
	release()
	if got, err := os.Readlink(x.lockPath); err != nil || got != "424242:shell" {
		t.Fatalf("release removed a foreign holder: %q %v", got, err)
	}
}

func TestVerifyLockPathFor(t *testing.T) {
	t.Parallel()
	repo, _, _ := newCFFixture(t)
	got, err := verifyLockPathFor(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if filepath.Dir(got) != filepath.Join(repo, ".git") && filepath.Dir(got) != real || filepath.Base(got) != verifyLockFileName {
		t.Fatalf("verifyLockPathFor = %q, want <repo>/.git/%s", got, verifyLockFileName)
	}
	if _, err := verifyLockPathFor(context.Background(), t.TempDir()); err == nil {
		t.Fatal("a non-repository must not resolve a lock path")
	}
}

func TestCounterfactual_OneObservationPerSpec(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	x := newTestCFExecutor(t, repo)
	// The phase budget expires after the first spec STARTS: the clock
	// advances an hour on every read after the first two.
	reads := 0
	base := time.Now()
	x.now = func() time.Time {
		reads++
		if reads <= 2 {
			return base
		}
		return base.Add(time.Hour)
	}
	bad := replaceSpec(0, "../x.go", "a", "b", "^TestAllowed$")
	specs := []upload.CounterfactualSpec{bad,
		replaceSpec(1, "ctl.go", pinnedFind, "return true", "^TestAllowed$"),
		replaceSpec(2, "ctl.go", pinnedFind, "return true", "^TestAllowed$"),
		replaceSpec(3, "ctl.go", pinnedFind, "return true", "^TestAllowed$"),
		replaceSpec(4, "ctl.go", pinnedFind, "return true", "^TestAllowed$"),
	}
	obs := runCF(t, x, repo, head, tree, specs...)
	wantObs(t, obs[0], cfOutcomeError, cfReasonInvalidSpec)
	for _, o := range obs[1:] {
		wantObs(t, o, cfOutcomeTimeout, cfReasonPhaseBudget)
	}
	for i, o := range obs {
		if o.Index != i {
			t.Fatalf("observation %d carries index %d", i, o.Index)
		}
	}
}

func TestValidCounterfactualSpec(t *testing.T) {
	t.Parallel()
	ok := replaceSpec(0, "a/b.go", "x", "y", "^T$")
	if !validCounterfactualSpec(ok) {
		t.Fatal("well-formed spec rejected")
	}
	mut := func(f func(*upload.CounterfactualSpec)) upload.CounterfactualSpec { s := ok; f(&s); return s }
	bad := map[string]upload.CounterfactualSpec{
		"dotdot":       mut(func(s *upload.CounterfactualSpec) { s.ControlPath = "a/../b.go" }),
		"absolute":     mut(func(s *upload.CounterfactualSpec) { s.ControlPath = "/etc/passwd" }),
		"backslash":    mut(func(s *upload.CounterfactualSpec) { s.ControlPath = `a\b.go` }),
		"blank":        mut(func(s *upload.CounterfactualSpec) { s.ControlPath = " " }),
		"test file":    mut(func(s *upload.CounterfactualSpec) { s.ControlPath = "a/b_test.go" }),
		"blank run":    mut(func(s *upload.CounterfactualSpec) { s.TestRun = " " }),
		"blank pkg":    mut(func(s *upload.CounterfactualSpec) { s.TestPackage = "" }),
		"expect green": mut(func(s *upload.CounterfactualSpec) { s.Expect = "green" }),
		"no-op":        mut(func(s *upload.CounterfactualSpec) { s.Replace = s.Find }),
		"empty find":   mut(func(s *upload.CounterfactualSpec) { s.Find = "" }),
		"both forms":   mut(func(s *upload.CounterfactualSpec) { s.Patch = "p" }),
		"unknown kind": mut(func(s *upload.CounterfactualSpec) { s.MutationKind = "sed" }),
		"empty patch":  patchSpec(0, "a.go", " "),
		"patch+find":   mut(func(s *upload.CounterfactualSpec) { s.MutationKind = upload.CounterfactualMutationPatch; s.Patch = "p" }),

		// Trimmed before every check (fix-up E): with the TrimSpace in
		// validCounterfactualSpec deleted, the trailing space defeats the
		// `_test.go` suffix test and nothing else refuses it.
		"padded test file": mut(func(s *upload.CounterfactualSpec) { s.ControlPath = " a/b_test.go " }),
	}
	for name, s := range bad {
		if validCounterfactualSpec(s) {
			t.Errorf("%s: invalid spec accepted", name)
		}
	}
}

func TestResolveCounterfactualPackage(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "go.work"), []byte("go 1.25\n"), 0o644)
	for in, want := range map[string]string{"./a/b": "./a/b", "a/b/...": "./a/b/...", ".": ".", "./...": "./...", "-x": "./-x"} {
		dir, arg, err := resolveCounterfactualPackage(ws, in)
		if err != nil || dir != ws || arg != want {
			t.Errorf("workspace %q -> (%q,%q,%v), want (%q,%q)", in, dir, arg, err, ws, want)
		}
	}
	if _, _, err := resolveCounterfactualPackage(ws, "./a/../../x"); err == nil {
		t.Error("a `..` package must be refused")
	}
	nested := t.TempDir()
	if err := os.MkdirAll(filepath.Join(nested, "mod", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(nested, "mod", "go.mod"), []byte("module m\n"), 0o644)
	dir, arg, err := resolveCounterfactualPackage(nested, "./mod/pkg/...")
	if err != nil || dir != filepath.Join(nested, "mod") || arg != "./pkg/..." {
		t.Errorf("nested -> (%q,%q,%v)", dir, arg, err)
	}
	if _, _, err := resolveCounterfactualPackage(nested, "./elsewhere"); err == nil {
		t.Error("a package with no go.mod above it must be refused")
	}
}

func TestClassifyGoTestJSON_Dispositions(t *testing.T) {
	t.Parallel()
	pass := `{"Action":"pass","Package":"p","Test":"TestA"}` + "\n"
	cases := []struct {
		out         string
		code        int
		disp        gateDisposition
		obs, reason string
	}{
		{pass, 0, gateTimedOut, cfOutcomeTimeout, ""},
		{pass, 0, gateRefused, cfOutcomeError, cfReasonGateRefused},
		{pass, 0, gateCheckoutRefused, cfOutcomeError, cfReasonGateRefused},
		{pass, 0, gateUnavailable, cfOutcomeError, cfReasonGateUnavailable},
		{pass, 0, gateExecuted, cfOutcomeGreen, ""},
		{pass, 1, gateExecuted, cfOutcomeError, cfReasonTestCommandFailed},
		{"not json\n", 1, gateExecuted, cfOutcomeError, cfReasonTestCommandFailed},
		{`{"Action":"output","Package":"p","Output":"FAIL\tp [setup failed]\n"}` + "\n" + `{"Action":"fail","Package":"p"}` + "\n", 1, gateExecuted, cfOutcomeError, cfReasonBuildFailed},
		{`{"Action":"fail","Package":"p","FailedBuild":"p"}` + "\n", 1, gateExecuted, cfOutcomeError, cfReasonBuildFailed},
		// A TEST printing a build-failed marker cannot turn its own
		// test-level failure into an error.
		{`{"Action":"output","Package":"p","Output":"[build failed]\n"}` + "\n" + `{"Action":"fail","Package":"p","Test":"TestA"}` + "\n" + `{"Action":"fail","Package":"p"}` + "\n", 1, gateExecuted, cfOutcomeRed, ""},
	}
	for i, c := range cases {
		if obs, reason := classifyGoTestJSON(c.out, c.code, c.disp); obs != c.obs || reason != c.reason {
			t.Errorf("case %d: %q/%q, want %q/%q", i, obs, reason, c.obs, c.reason)
		}
	}
}

func TestCounterfactual_GateDispositionNeverRed(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	x := newTestCFExecutor(t, repo)
	x.exec = func(ctx context.Context, argv []string, dir, lc string, to time.Duration) (string, int, gateDisposition) {
		if argv[1] == "list" {
			return runBoundedGateArgvDisposed(ctx, argv, dir, lc, to)
		}
		return "", -1, gateUnavailable
	}
	obs := runCF(t, x, repo, head, tree, replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeError, cfReasonGateUnavailable)
	x.exec = func(ctx context.Context, argv []string, dir, lc string, to time.Duration) (string, int, gateDisposition) {
		return "", -1, gateRefused
	}
	obs = runCF(t, x, repo, head, tree, replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeError, cfReasonGateRefused)
}

func TestCounterfactual_NoVerifiedTreeAndCloneFailure(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	x := newTestCFExecutor(t, repo)
	spec := replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestAllowed$")
	wantObs(t, runCF(t, x, repo, "", "", spec)[0], cfOutcomeError, cfReasonNoVerifiedTree)
	x.materialize = func(context.Context, string, string, string) (string, error) { return "", os.ErrNotExist }
	wantObs(t, runCF(t, x, repo, head, tree, spec)[0], cfOutcomeError, cfReasonCloneFailed)
}

func TestCounterfactual_RestoreFailurePoisonsRemainingSpecs(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	x := newTestCFExecutor(t, repo)
	// The first spec's control path is invalid, so it runs nothing — but the
	// clone it would restore is removed underneath the phase, so the restore
	// fails and every later spec must record restore_failed, not run.
	x.materialize = func(ctx context.Context, repoDir, headSHA, parent string) (string, error) {
		wt, err := materializeGateCheckout(ctx, repoDir, headSHA, parent)
		if err == nil {
			_ = os.RemoveAll(filepath.Join(wt, ".git"))
		}
		return wt, err
	}
	obs := runCF(t, x, repo, head, tree,
		replaceSpec(0, "../x.go", "a", "b", "^TestAllowed$"),
		replaceSpec(1, "ctl.go", pinnedFind, "return true", "^TestAllowed$"))
	wantObs(t, obs[0], cfOutcomeError, cfReasonInvalidSpec)
	wantObs(t, obs[1], cfOutcomeError, cfReasonRestoreFailed)
}

func TestRunPlanCounterfactuals_NoSpecsNoEvents(t *testing.T) {
	t.Parallel()
	if evs, obs := runPlanCounterfactuals(context.Background(), config{}, t.TempDir(), nil, "t", nil, io.Discard); evs != nil || obs != nil {
		t.Fatalf("zero specs produced %d events, %d observations", len(evs), len(obs))
	}
}

func TestRunPlanCounterfactuals_ObservationsComposeIntoGateEvidence(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	cfg := config{runID: "r", stageID: "s"}
	verifyRun := agent.Event{Kind: "verify_run", Payload: agent.MakePayload(map[string]any{"head_sha": head, "tree_sha": tree})}
	spec := replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestAllowed$")

	// A verify_run whose tree is NOT the verified tree is not a target.
	evs, obs := runPlanCounterfactuals(context.Background(), cfg, repo, []agent.Event{verifyRun}, "someothertree", []upload.CounterfactualSpec{spec}, io.Discard)
	if len(evs) != 1 || evs[0].Kind != "counterfactual_observed" || len(obs) != 1 {
		t.Fatalf("events = %+v, observations = %+v", evs, obs)
	}
	var w struct {
		Observation counterfactualObservationEvidence `json:"observation"`
	}
	if err := json.Unmarshal(evs[0].Payload, &w); err != nil {
		t.Fatal(err)
	}
	wantObs(t, w.Observation, cfOutcomeError, cfReasonNoVerifiedTree)

	evs, obs = runPlanCounterfactuals(context.Background(), cfg, repo, []agent.Event{verifyRun}, tree, []upload.CounterfactualSpec{spec}, io.Discard)
	ge := composeGateEvidence(append([]agent.Event{verifyRun}, evs...), 1, runnerCounterfactualObservations(obs))
	if ge == nil {
		t.Fatal("no gate_evidence composed")
	}
	if !strings.Contains(string(ge.Payload), `"counterfactual_observations":[{"index":0,"control_path":"ctl.go"`) ||
		!strings.Contains(string(ge.Payload), `"observed":"red"`) {
		t.Fatalf("gate_evidence lacks the red observation: %s", ge.Payload)
	}
}

func TestCounterfactual_InvalidSpecRunsNothing(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	x := newTestCFExecutor(t, repo)
	p := &lockProbe{lockPath: x.lockPath}
	x.exec = p.exec
	// A contained, existing, built control: only the re-validation (expect
	// must be red) refuses it.
	s := replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestAllowed$")
	s.Expect = "green"
	wantObs(t, runCF(t, x, repo, head, tree, s)[0], cfOutcomeError, cfReasonInvalidSpec)
	if p.tests != 0 {
		t.Fatalf("an invalid spec ran go test %d times", p.tests)
	}
}

// --- fix-up pass 2 (C, D, E, F, G, A) -----------------------------------------

// TestCounterfactual_SymlinkedPackageDirRefused (C): a symlinked intermediate
// directory COMMITTED before the phase, pointing outside the clone at a
// BUILDABLE package with a passing test (tests.package ./linkdir), is refused
// error/invalid_spec before anything runs. The outside package compiles and
// its test passes through the symlink, so with C2 deleted go list, the
// baseline and the mutated run would all execute against the OUTSIDE files —
// the downstream os.Root would still refuse the write, so the discriminating
// assertions are baseline=skipped and ZERO go test runs, plus the outside
// target byte-unchanged. COUNTERFACTUAL: make containedControlFile return
// (path, nil) → the baseline runs (green) → RED.
func TestCounterfactual_SymlinkedPackageDirRefused(t *testing.T) {
	t.Parallel()
	repo, _, _ := newCFFixture(t)
	outDir := t.TempDir()
	const outsideCtl = "package linkdir\n\n// X is the control.\nfunc X(n int) bool { return n > 0 }\n"
	const outsideTest = "package linkdir\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {\n\tif X(0) || !X(1) {\n\t\tt.Fatal(\"X\")\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(outDir, "ctl.go"), []byte(outsideCtl), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "ctl_test.go"), []byte(outsideTest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outDir, filepath.Join(repo, "linkdir")); err != nil {
		t.Fatal(err)
	}
	cfGit(t, repo, "add", "-A")
	cfGit(t, repo, "commit", "-q", "-m", "symlinked package dir")
	head := cfGit(t, repo, "rev-parse", "HEAD")
	tree := cfGit(t, repo, "rev-parse", "HEAD^{tree}")

	// The fixture is live: the outside package builds and passes on its own.
	if out, code, disp := runBoundedGateArgvDisposed(context.Background(), []string{"go", "test", "-count=1", "./linkdir"}, repo, t.TempDir(), time.Minute); code != 0 || disp != gateExecuted {
		t.Fatalf("fixture precondition: go test ./linkdir through the symlink = exit %d disp %v, want a pass:\n%s", code, disp, out)
	}

	x := newTestCFExecutor(t, repo)
	p := &lockProbe{lockPath: x.lockPath}
	x.exec = p.exec
	s := replaceSpec(0, "linkdir/ctl.go", "return n > 0", "return true", "^TestX$")
	s.TestPackage = "./linkdir"
	o := runCF(t, x, repo, head, tree, s)[0]
	wantObs(t, o, cfOutcomeError, cfReasonInvalidSpec)
	if o.Baseline != cfOutcomeSkipped || p.tests != 0 {
		t.Fatalf("baseline=%q go test runs=%d; want skipped and 0 (nothing may run through the symlink)", o.Baseline, p.tests)
	}
	if got, _ := os.ReadFile(filepath.Join(outDir, "ctl.go")); string(got) != outsideCtl {
		t.Fatalf("outside target modified: %q", got)
	}
}

// TestLstatControlComponents_RefusesInsideSymlink (C2, walk layer alone): an
// intermediate symlink pointing INSIDE the clone passes the EvalSymlinks
// containment, so only the component walk refuses it. COUNTERFACTUAL: make
// lstatControlComponents return the joined path without Lstat-ing → the walk
// accepts and containedControlFile returns nil → RED.
func TestLstatControlComponents_RefusesInsideSymlink(t *testing.T) {
	t.Parallel()
	clone := t.TempDir()
	if err := os.Mkdir(filepath.Join(clone, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "real", "ctl.go"), []byte("package real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(clone, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := controlResolvesInsideClone(clone, filepath.Join(clone, "alias", "ctl.go")); err != nil {
		t.Fatalf("fixture precondition: the inside symlink must pass containment, got %v", err)
	}
	if _, err := lstatControlComponents(clone, "alias/ctl.go"); err == nil {
		t.Fatal("lstatControlComponents accepted a symlink component")
	}
	if _, err := containedControlFile(clone, "alias/ctl.go"); err == nil {
		t.Fatal("containedControlFile accepted a symlink component")
	}
	if _, err := lstatControlComponents(clone, "real/ctl.go"); err != nil {
		t.Fatalf("a real path must pass the walk: %v", err)
	}
}

// TestControlResolvesInsideClone_RefusesEscape (C2, containment layer alone):
// called directly, so the walk that would refuse the symlink first is
// bypassed — an escaping intermediate symlink is refused by the EvalSymlinks
// containment itself. COUNTERFACTUAL: make controlResolvesInsideClone return
// nil → RED.
func TestControlResolvesInsideClone_RefusesEscape(t *testing.T) {
	t.Parallel()
	clone, outDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "ctl.go"), []byte(cfOutsideCtlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outDir, filepath.Join(clone, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := controlResolvesInsideClone(clone, filepath.Join(clone, "sub", "ctl.go")); err == nil {
		t.Fatal("controlResolvesInsideClone accepted a path resolving outside the clone")
	}
	if err := os.WriteFile(filepath.Join(clone, "in.go"), []byte("package in\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := controlResolvesInsideClone(clone, filepath.Join(clone, "in.go")); err != nil {
		t.Fatalf("a contained path must pass: %v", err)
	}
}

// cfHostSentinel is a host env var the gate's default-deny allow-list does
// not carry.
const cfHostSentinel = "FISHHAWK_CF_HOST_SENTINEL"

// TestCounterfactual_HostEnvNotInherited (D): a sentinel set in the runner's
// own env is ABSENT from the counterfactual go test env. The committed
// TestEnvClean fails when it sees the sentinel, so a leak turns the baseline
// RED (error/baseline_not_green) instead of green-then-red. COUNTERFACTUAL:
// replace sanitizedGateEnv() with os.Environ() in runBoundedGateArgvDisposed →
// baseline red → RED. Not t.Parallel: it uses t.Setenv.
func TestCounterfactual_HostEnvNotInherited(t *testing.T) {
	t.Setenv(cfHostSentinel, "leaked")
	repo, _, _ := newCFFixture(t)
	envTest := "package cf\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestEnvClean(t *testing.T) {\n" +
		"\tif v, ok := os.LookupEnv(\"" + cfHostSentinel + "\"); ok {\n\t\tt.Fatalf(\"host env leaked into the counterfactual run: %q\", v)\n\t}\n" +
		"\tif Allowed(0) {\n\t\tt.Fatal(\"Allowed(0) must be false\")\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(repo, "env_test.go"), []byte(envTest), 0o644); err != nil {
		t.Fatal(err)
	}
	cfGit(t, repo, "add", "-A")
	cfGit(t, repo, "commit", "-q", "-m", "env test")
	head := cfGit(t, repo, "rev-parse", "HEAD")
	tree := cfGit(t, repo, "rev-parse", "HEAD^{tree}")
	o := runCF(t, newTestCFExecutor(t, repo), repo, head, tree, replaceSpec(0, "ctl.go", pinnedFind, "return true", "^TestEnvClean$"))[0]
	if o.Baseline != cfOutcomeGreen {
		t.Fatalf("baseline = %q (observed %q/%q), want green: the host sentinel reached the counterfactual go test env", o.Baseline, o.Observed, o.Reason)
	}
	wantObs(t, o, cfOutcomeRed, "")
}

// TestCounterfactual_PaddedControlPathTrimmed (E, executor side): a padded
// " ctl.go " is trimmed before every check and executes like "ctl.go".
// COUNTERFACTUAL: delete the TrimSpace at the top of executeCounterfactual →
// the C2 walk Lstats "<clone>/ ctl.go", which does not exist →
// error/invalid_spec, not red → RED.
func TestCounterfactual_PaddedControlPathTrimmed(t *testing.T) {
	t.Parallel()
	repo, head, tree := newCFFixture(t)
	o := runCF(t, newTestCFExecutor(t, repo), repo, head, tree, replaceSpec(0, " ctl.go ", pinnedFind, "return true", "^TestAllowed$"))[0]
	wantObs(t, o, cfOutcomeRed, "")
}

// TestCounterfactualBoundsPinned (F): the per-run timeout and the C4 lock
// re-acquire gap are named constants with pinned values, and the production
// executor uses them. COUNTERFACTUAL: change either constant, or wire a
// literal into newCounterfactualExecutor → RED.
func TestCounterfactualBoundsPinned(t *testing.T) {
	t.Parallel()
	if counterfactualRunTimeout != 5*time.Minute {
		t.Errorf("counterfactualRunTimeout = %v, want 5m (runner/README.md § Plan-declared counterfactuals)", counterfactualRunTimeout)
	}
	if counterfactualLockReacquireDelay != 5*time.Second {
		t.Errorf("counterfactualLockReacquireDelay = %v, want 5s (C4; scripts/README.md § The verify lock)", counterfactualLockReacquireDelay)
	}
	x := newCounterfactualExecutor(context.Background(), t.TempDir())
	if x.runTimeout != counterfactualRunTimeout || x.reacquireDelay != counterfactualLockReacquireDelay {
		t.Errorf("executor bounds = (%v, %v), want the named constants", x.runTimeout, x.reacquireDelay)
	}
}

// TestRunPlanCounterfactuals_CapsAtEight (G): a delivery of nine specs
// executes and records only the first eight, matching the plan and backend
// ingest cap. COUNTERFACTUAL: delete the cap block in runPlanCounterfactuals →
// 9 observations → RED; raise gateEvidenceCounterfactualCap → RED.
func TestRunPlanCounterfactuals_CapsAtEight(t *testing.T) {
	t.Parallel()
	if gateEvidenceCounterfactualCap != 8 {
		t.Fatalf("gateEvidenceCounterfactualCap = %d, want 8 (plan.MaxCounterfactuals, the backend ingest cap)", gateEvidenceCounterfactualCap)
	}
	specs := make([]upload.CounterfactualSpec, 9)
	for i := range specs {
		specs[i] = replaceSpec(i, "ctl.go", pinnedFind, "return true", "^TestAllowed$")
	}
	// No verified tree: every executed spec records no_verified_tree, cheaply.
	evs, obs := runPlanCounterfactuals(context.Background(), config{runID: "r", stageID: "s"}, t.TempDir(), nil, "", specs, io.Discard)
	if len(obs) != 8 || len(evs) != 8 {
		t.Fatalf("observations=%d events=%d, want 8 each", len(obs), len(evs))
	}
	if obs[7].Index != 7 {
		t.Fatalf("last kept observation has index %d, want 7 (spec order)", obs[7].Index)
	}
}

// TestCounterfactualOutcomes_MatchSharedFixture (A, runner side): the
// cfOutcome* / cfReason* constants equal the closed vocabulary in
// testdata/wire/counterfactual_outcomes.json, which the backend ingest's
// admission sets are pinned against too. COUNTERFACTUAL: rename any constant's
// value → RED. Residual: a NEW constant must be added to the lists below.
func TestCounterfactualOutcomes_MatchSharedFixture(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "counterfactual_outcomes.json"))
	if err != nil {
		t.Fatalf("read shared fixture: %v", err)
	}
	var f struct {
		Observed []string `json:"observed"`
		Baseline []string `json:"baseline"`
		Reason   []string `json:"reason"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("decode shared fixture: %v", err)
	}
	observed := []string{cfOutcomeRed, cfOutcomeGreen, cfOutcomeTimeout, cfOutcomeError}
	baseline := []string{cfOutcomeGreen, cfOutcomeRed, cfOutcomeTimeout, cfOutcomeError, cfOutcomeSkipped}
	reason := []string{
		cfReasonInvalidSpec, cfReasonNoVerifiedTree, cfReasonCloneFailed, cfReasonRestoreFailed,
		cfReasonControlNotBuilt, cfReasonMutationNotApplied, cfReasonMutationAmbiguous,
		cfReasonMutationOutside, cfReasonBaselineNotGreen, cfReasonBuildFailed,
		cfReasonNoTestsMatched, cfReasonTestCommandFailed, cfReasonGateRefused,
		cfReasonGateUnavailable, cfReasonLockUnavailable, cfReasonPhaseBudget,
	}
	for name, pair := range map[string][2][]string{"observed": {f.Observed, observed}, "baseline": {f.Baseline, baseline}, "reason": {f.Reason, reason}} {
		if len(pair[0]) == 0 || strings.Join(pair[0], ",") != strings.Join(pair[1], ",") {
			t.Errorf("%s: runner constants %v, shared fixture %v", name, pair[1], pair[0])
		}
	}
}
