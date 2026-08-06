package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- wall-clock sizing (binding condition 6) ---------------------------------
//
// The cli module cannot import backend/internal/timescale across module
// boundaries, so the discrimination ratios are sized here explicitly for a
// 5x-loaded CI runner:
//
//	verifyTestDeadline  =  2s  — the rung's effective per-command timeout.
//	verifyTestBound     = 30s  — the elapsed upper bound the test asserts.
//	verifyTestLooseCap  = 60s  — a doctor cap deliberately LOOSER than the bound.
//	verifyTestWedge     = 120s — how long the wedging command / grandchild lives.
//
// bound/deadline = 15x: a 2s deadline fires at 2s even under 5x load, and
// worktree provisioning plus the group kill are sub-second, so 30s leaves an
// order of magnitude of headroom — the bound cannot be reached by slowness.
// wedge/bound = 4x: when the group-kill control is deleted the grandchild
// holds the output pipe for 120s, which is 4x past the bound, so the assertion
// lands RED on the control's absence rather than on a marginal timing race.
// looseCap/bound = 2x: when the spec-timeout min() is deleted the 60s cap
// applies instead of the 2s spec value, which is 2x past the bound — enough to
// land RED without doubling the suite's worst-case runtime.
const (
	verifyTestDeadline = 2 * time.Second
	verifyTestBound    = 30 * time.Second
	verifyTestLooseCap = 60 * time.Second
	verifyTestWedge    = "120"
)

// verifyRepoFile is one file committed into a fixture repo.
type verifyRepoFile struct {
	path     string
	contents string
}

// newVerifyRepo creates a git repo containing the given spec YAML at
// .fishhawk/workflows.yaml plus any extra files, all COMMITTED, and returns
// the repo path. Files written after this call are untracked and therefore
// absent from a fresh worktree — which is what the headline test exploits.
func newVerifyRepo(t *testing.T, specYAML string, files ...verifyRepoFile) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init")
	if specYAML != "" {
		writeRepoFile(t, dir, ".fishhawk/workflows.yaml", specYAML)
	}
	for _, f := range files {
		writeRepoFile(t, dir, f.path, f.contents)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", "init")
	return dir
}

// gitIn runs a git command in dir with the operator's global/system config
// neutralized (so a global commit.gpgsign cannot red-line the fixture) and
// returns its trimmed stdout.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

func writeRepoFile(t *testing.T, dir, rel, contents string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// specWithCommand renders a minimal workflow-v2 spec whose implement stage
// declares the given verify command and timeout string.
func specWithCommand(command, timeout string) string {
	return fmt.Sprintf(`version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: %q
            timeout: %q
`, command, timeout)
}

// doctorVerifyWorktreeFor returns the absolute path checkVerifyCommand would
// provision inside repoDir, so a test can assert on filesystem state after
// the call returns.
func doctorVerifyWorktreeFor(t *testing.T, repoDir string) string {
	t.Helper()
	common := gitIn(t, repoDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	return filepath.Join(common, "fishhawk-worktrees", doctorVerifyWorktreeName())
}

// assertWorktreeGone asserts the throwaway worktree is absent from BOTH the
// filesystem and `git worktree list`. It reads COMMITTED state after the call
// returns rather than the returned error, so a cleanup that never ran cannot
// hide behind a byte-identical result.
func assertWorktreeGone(t *testing.T, repoDir, worktree string) {
	t.Helper()
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("throwaway worktree %s still on disk after the call (stat err = %v)", worktree, err)
	}
	if list := gitIn(t, repoDir, "worktree", "list"); strings.Contains(list, worktree) {
		t.Errorf("throwaway worktree %s still registered:\n%s", worktree, list)
	}
}

// --- default-deny opt-in gate ------------------------------------------------

// TestCheckVerifyCommandGated_DefaultDoesNotExecute is the counterfactual
// vehicle for the opt-in gate: with NEITHER flag set the rung must warn and
// spawn no subprocess at all.
//
// The assertion reads COMMITTED STATE — the marker file the spec's command
// would create — rather than the returned error, because a gate that never
// fired would return a rung result that merely LOOKS different; only the
// filesystem proves no code ran. The command is `touch <marker>`, which exits 0
// in the fresh worktree, so deleting the gate turns this test's status
// assertion green-to-red AND materializes the marker: the test cannot pass
// without the control.
func TestCheckVerifyCommandGated_DefaultDoesNotExecute(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	repo := newVerifyRepo(t, specWithCommand("touch "+marker, "15m"))

	r := checkVerifyCommandGated(repo, false, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "--run-verify-command") {
		t.Errorf("remediate = %q, want it to name --run-verify-command", r.remediate)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker %s exists — a subprocess was spawned without --run-verify-command", marker)
	}
}

// TestCheckVerifyCommandGated_OptInExecutes: --run-verify-command runs the
// command. Without this the gate could be a blanket refusal and the
// default-deny test above would still pass.
func TestCheckVerifyCommandGated_OptInExecutes(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	repo := newVerifyRepo(t, specWithCommand("touch "+marker, "15m"))

	r := checkVerifyCommandGated(repo, true, false, verifyTestDeadline)
	if r.status != "ok" {
		t.Errorf("status = %q, want ok; detail: %s", r.status, r.detail)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker %s absent — the command did not run under --run-verify-command: %v", marker, err)
	}
	assertWorktreeGone(t, repo, doctorVerifyWorktreeFor(t, repo))
}

// TestCheckVerifyCommandGated_SkipWinsOverOptIn: both flags set -> skipped,
// no subprocess. Pinning the precedence keeps --skip-verify-command a reliable
// per-invocation override of a wrapper script that opts in.
func TestCheckVerifyCommandGated_SkipWinsOverOptIn(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	repo := newVerifyRepo(t, specWithCommand("touch "+marker, "15m"))

	r := checkVerifyCommandGated(repo, true, true, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "--skip-verify-command") {
		t.Errorf("detail = %q, want it to name --skip-verify-command", r.detail)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker %s exists — --skip-verify-command did not win over --run-verify-command", marker)
	}
}

// --- outcome-table branch coverage -------------------------------------------

// TestCheckVerifyCommand_SkippedByFlag: --skip-verify-command warns and
// spawns NO subprocess (asserted via a marker file the command would create).
func TestCheckVerifyCommand_SkippedByFlag(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	repo := newVerifyRepo(t, specWithCommand("touch "+marker, "15m"))

	r := checkVerifyCommand(repo, true, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "--skip-verify-command") {
		t.Errorf("detail = %q, want it to name --skip-verify-command", r.detail)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker %s exists — a subprocess was spawned despite the skip flag", marker)
	}
}

// TestCheckVerifyCommand_NoSpec: no .fishhawk/workflows.yaml -> warn.
func TestCheckVerifyCommand_NoSpec(t *testing.T) {
	r := checkVerifyCommand(t.TempDir(), false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, ".fishhawk/workflows.yaml") {
		t.Errorf("remediate = %q, want it to point at creating the spec", r.remediate)
	}
}

// TestCheckVerifyCommand_SpecReadError: the spec path exists but cannot be
// read (it is a directory) -> warn, never a fail. checkSpec is the authority.
func TestCheckVerifyCommand_SpecReadError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk", "workflows.yaml"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r := checkVerifyCommand(dir, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if r.detail != "spec read error" {
		t.Errorf("detail = %q, want %q", r.detail, "spec read error")
	}
}

// TestCheckVerifyCommand_SpecUnresolvable: a workflow whose `extends` base
// does not exist -> warn pointing at `fishhawk validate`, never fail.
func TestCheckVerifyCommand_SpecUnresolvable(t *testing.T) {
	repo := newVerifyRepo(t, `version: "2"
workflows:
  feature_change:
    extends: no_such_base
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "true"
`)
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "fishhawk validate") {
		t.Errorf("remediate = %q, want it to point at `fishhawk validate`", r.remediate)
	}
}

// TestCheckVerifyCommand_SpecUnparseable: malformed YAML -> warn.
func TestCheckVerifyCommand_SpecUnparseable(t *testing.T) {
	repo := newVerifyRepo(t, "version: \"2\"\nworkflows: [oh: no: :\n")
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "fishhawk validate") {
		t.Errorf("remediate = %q, want it to point at `fishhawk validate`", r.remediate)
	}
}

// TestCheckVerifyCommand_ResolvedDocParseError: a resolved document whose
// `workflows` map has the wrong SHAPE for the rung's typed decode (stages as
// a scalar) -> the parse-error warn branch. This is the branch after
// ResolveReuse succeeds but yaml.Unmarshal into the typed struct fails.
func TestCheckVerifyCommand_ResolvedDocParseError(t *testing.T) {
	repo := newVerifyRepo(t, `version: "2"
workflows:
  feature_change: "not-a-mapping"
`)
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if r.detail != "spec parse error" {
		t.Errorf("detail = %q, want %q", r.detail, "spec parse error")
	}
}

// TestCheckVerifyCommand_NoVerifyBlock: a valid spec whose implement stage
// declares no `verify:` -> warn saying no test gate will run. The preset
// explicitly permits removing the block, so this can never be a fail.
func TestCheckVerifyCommand_NoVerifyBlock(t *testing.T) {
	repo := newVerifyRepo(t, `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
`)
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "no test gate will run") {
		t.Errorf("remediate = %q, want it to state no test gate will run", r.remediate)
	}
}

// TestCheckVerifyCommand_EmptyCommand: a `verify:` block whose command is
// whitespace-only -> warn, distinct from the absent-block branch.
func TestCheckVerifyCommand_EmptyCommand(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand("   ", "15m"))
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "empty command") {
		t.Errorf("detail = %q, want it to name the empty command", r.detail)
	}
}

// TestCheckVerifyCommand_NotAGitRepo: a spec with a runnable command in a
// directory outside any git work tree -> warn naming the git failure.
func TestCheckVerifyCommand_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, ".fishhawk/workflows.yaml", specWithCommand("true", "15m"))
	r := checkVerifyCommand(dir, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "git") {
		t.Errorf("remediate = %q, want it to name the git failure", r.remediate)
	}
}

// TestCheckVerifyCommand_GitUnavailable: the git executable is not on PATH
// -> warn naming that, never a fail.
func TestCheckVerifyCommand_GitUnavailable(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand("true", "15m"))
	withFakeDoctorLookPath(t, func(name string) (string, error) {
		return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
	})
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "git executable not found") {
		t.Errorf("remediate = %q, want it to name the missing git executable", r.remediate)
	}
}

// TestCheckVerifyCommand_HeadUnresolvable: a git repo with NO commits, so
// `git rev-parse HEAD` fails before any worktree is attempted -> warn.
// (This is the HEAD-resolution branch, distinct from worktree-add failure.)
func TestCheckVerifyCommand_HeadUnresolvable(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	writeRepoFile(t, dir, ".fishhawk/workflows.yaml", specWithCommand("true", "15m"))
	r := checkVerifyCommand(dir, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "rev-parse HEAD") {
		t.Errorf("remediate = %q, want it to name the HEAD resolution failure", r.remediate)
	}
}

// TestCheckVerifyCommand_WorktreeAddFails: HEAD resolves, but the target
// path is already an occupied non-empty directory, so `git worktree add`
// itself fails -> warn naming it.
func TestCheckVerifyCommand_WorktreeAddFails(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand("true", "15m"))
	target := doctorVerifyWorktreeFor(t, repo)
	writeRepoFile(t, target, "occupant", "not empty\n")

	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "warn" {
		t.Errorf("status = %q, want warn; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "worktree add") {
		t.Errorf("remediate = %q, want it to name the worktree add failure", r.remediate)
	}
	// The pre-existing directory is not ours to delete.
	if _, err := os.Stat(filepath.Join(target, "occupant")); err != nil {
		t.Errorf("pre-existing occupant was removed: %v", err)
	}
}

// TestCheckVerifyCommand_PassingCommandOK: a command that exits 0 -> ok,
// with the detail naming the command.
func TestCheckVerifyCommand_PassingCommandOK(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand("true", "15m"))
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "ok" {
		t.Fatalf("status = %q, want ok; detail: %s; hint: %s", r.status, r.detail, r.remediate)
	}
	if !strings.Contains(r.detail, `"true"`) {
		t.Errorf("detail = %q, want it to name the command", r.detail)
	}
}

// TestCheckVerifyCommand_NonZeroExitFails: `exit 3` -> fail whose detail
// carries the exit status and whose remediation carries BOTH the throwaway
// worktree path and a tail of the captured output.
func TestCheckVerifyCommand_NonZeroExitFails(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand("echo boom-marker; exit 3", "15m"))
	worktree := doctorVerifyWorktreeFor(t, repo)

	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "fail" {
		t.Fatalf("status = %q, want fail; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "exited 3") {
		t.Errorf("detail = %q, want it to carry exit status 3", r.detail)
	}
	if !strings.Contains(r.remediate, worktree) {
		t.Errorf("remediate = %q, want it to carry the throwaway worktree path %s", r.remediate, worktree)
	}
	if !strings.Contains(r.remediate, "boom-marker") {
		t.Errorf("remediate = %q, want it to carry a tail of the captured output", r.remediate)
	}
	if !strings.Contains(r.remediate, "TRACKED files") {
		t.Errorf("remediate = %q, want the fresh-worktree explanation", r.remediate)
	}
}

// TestCheckVerifyCommand_OutputTailTruncated: output far beyond the cap ->
// fail whose remediation is bounded and marks the truncation.
func TestCheckVerifyCommand_OutputTailTruncated(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand(
		"i=0; while [ $i -lt 400 ]; do echo line-$i; i=$((i+1)); done; exit 1", "15m"))
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "fail" {
		t.Fatalf("status = %q, want fail; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.remediate, "[output truncated]") {
		t.Errorf("remediate = %q, want an explicit truncation marker", r.remediate)
	}
	if strings.Contains(r.remediate, "line-0\n") {
		t.Errorf("remediate carries the FIRST line, so the tail was not bounded:\n%s", r.remediate)
	}
	if !strings.Contains(r.remediate, "line-399") {
		t.Errorf("remediate = %q, want the LAST line of output", r.remediate)
	}
}

// TestCheckVerifyCommand_TimeoutFails: a command whose backgrounded
// grandchild outlives the deadline while holding the output pipe -> fail
// naming the timeout, returned within the sized elapsed bound.
//
// This is the Setpgid + kill(-pgid) group-kill vehicle: without the group
// kill, CombinedOutput blocks on the grandchild's inherited pipe for
// verifyTestWedge seconds, 4x past verifyTestBound.
func TestCheckVerifyCommand_TimeoutFails(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand(
		"sleep "+verifyTestWedge+" & sleep "+verifyTestWedge, "15m"))

	start := time.Now()
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	elapsed := time.Since(start)

	if r.status != "fail" {
		t.Fatalf("status = %q, want fail; detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "timed out") {
		t.Errorf("detail = %q, want it to name the timeout", r.detail)
	}
	if elapsed > verifyTestBound {
		t.Errorf("checkVerifyCommand took %s, want under %s — the process group was not killed",
			elapsed, verifyTestBound)
	}
}

// TestCheckVerifyCommand_SpecTimeoutCappedByFlag: the spec declares 15m but
// the doctor cap is verifyTestDeadline, so the cap wins.
func TestCheckVerifyCommand_SpecTimeoutCappedByFlag(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand("sleep "+verifyTestWedge, "15m"))

	start := time.Now()
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	elapsed := time.Since(start)

	if r.status != "fail" || !strings.Contains(r.detail, "timed out") {
		t.Fatalf("status = %q detail = %q, want a timeout fail (the cap must beat the spec's 15m)",
			r.status, r.detail)
	}
	if !strings.Contains(r.detail, verifyTestDeadline.String()) {
		t.Errorf("detail = %q, want it to name the capped timeout %s", r.detail, verifyTestDeadline)
	}
	if elapsed > verifyTestBound {
		t.Errorf("took %s, want under %s", elapsed, verifyTestBound)
	}
}

// TestCheckVerifyCommand_UnparseableSpecTimeoutFallsBackToCap: a garbage
// duration string is not a fail invented from plumbing — the cap applies and
// the command still runs.
func TestCheckVerifyCommand_UnparseableSpecTimeoutFallsBackToCap(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand("true", "not-a-duration"))
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "ok" {
		t.Errorf("status = %q, want ok (the cap applies, the command still runs); detail: %s; hint: %s",
			r.status, r.detail, r.remediate)
	}
}

// TestCheckVerifyCommand_SpecTimeoutShorterThanCapWins is the OTHER direction
// of the per-command min(): the doctor cap is verifyTestLooseCap but the spec
// declares the much shorter verifyTestDeadline, so the SPEC value must bound
// the command — the behavior cli/README.md and docs/onboarding.md both
// document ("the spec's own executor.verify.timeout wins only when it is
// shorter"). Deleting the `c.timeout < effective` branch leaves the loose cap
// in force, which this test catches twice over: the detail would name the cap
// rather than the spec value, and the call would outlive verifyTestBound.
func TestCheckVerifyCommand_SpecTimeoutShorterThanCapWins(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand("sleep "+verifyTestWedge, verifyTestDeadline.String()))

	start := time.Now()
	r := checkVerifyCommand(repo, false, verifyTestLooseCap)
	elapsed := time.Since(start)

	if r.status != "fail" || !strings.Contains(r.detail, "timed out") {
		t.Fatalf("status = %q detail = %q, want a timeout fail", r.status, r.detail)
	}
	if !strings.Contains(r.detail, verifyTestDeadline.String()) {
		t.Errorf("detail = %q, want it to name the SPEC timeout %s, not the looser cap %s",
			r.detail, verifyTestDeadline, verifyTestLooseCap)
	}
	if elapsed > verifyTestBound {
		t.Errorf("took %s, want under %s — the shorter spec timeout did not bound the command",
			elapsed, verifyTestBound)
	}
}

// TestCheckVerifyCommand_NonPositiveTimeoutFallsBackToDefault pins the
// `if maxTimeout <= 0 { maxTimeout = defaultVerifyTimeout }` guard, reachable
// from the CLI via `--verify-timeout 0`. Without the guard the child context is
// constructed with an already-elapsed deadline, so even `true` never runs and
// the rung reports a timeout fail.
func TestCheckVerifyCommand_NonPositiveTimeoutFallsBackToDefault(t *testing.T) {
	for _, max := range []time.Duration{0, -1 * time.Second} {
		t.Run(max.String(), func(t *testing.T) {
			repo := newVerifyRepo(t, specWithCommand("true", "15m"))
			r := checkVerifyCommand(repo, false, max)
			if r.status != "ok" {
				t.Errorf("status = %q with maxTimeout=%s, want ok (the default cap applies); "+
					"detail: %s; hint: %s", r.status, max, r.detail, r.remediate)
			}
		})
	}
}

// --- credential stripping (implement-review security concern) -----------------

// TestCheckVerifyCommand_ChildDoesNotInheritCredentials is the end-to-end
// counterfactual vehicle for the cmd.Env control: the command string is read
// from the repository's committed spec, so the child must NOT see the doctor
// process's credentials. The secret is seeded into the doctor process's own
// environment and the command echoes it back through the rung's captured
// output, so a child that inherits os.Environ() surfaces the marker verbatim in
// the remediation. The same command also proves PATH survived, so the fix
// cannot be a blanket empty environment that silently breaks every command.
func TestCheckVerifyCommand_ChildDoesNotInheritCredentials(t *testing.T) {
	const secret = "leak-marker-do-not-inherit"
	t.Setenv("FISHHAWK_API_TOKEN", secret)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)

	repo := newVerifyRepo(t, specWithCommand(
		`echo "token=[$FISHHAWK_API_TOKEN] aws=[$AWS_SECRET_ACCESS_KEY] path=[${PATH:+set}]"; exit 1`,
		"15m"))

	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "fail" {
		t.Fatalf("status = %q, want fail; detail: %s; hint: %s", r.status, r.detail, r.remediate)
	}
	if strings.Contains(r.remediate, secret) {
		t.Errorf("the verify child inherited a credential from the doctor process:\n%s", r.remediate)
	}
	if !strings.Contains(r.remediate, "token=[] aws=[] path=[set]") {
		t.Errorf("remediate = %q, want the credentials empty AND PATH still set", r.remediate)
	}
}

// TestSanitizeVerifyEnv pins the default-deny allow-list itself: an explicitly
// denied key, an unrecognized key (the default-deny case that keeps a
// LATER-introduced secret out without a list edit), the system essentials and
// GO*/CGO_*/LC_* prefixes that must survive, a malformed entry, and userinfo
// redaction on a credentialed GOPROXY list.
func TestSanitizeVerifyEnv(t *testing.T) {
	got := sanitizeVerifyEnv([]string{
		"FISHHAWK_API_TOKEN=secret",
		"GITHUB_TOKEN=secret",
		"ANTHROPIC_API_KEY=secret",
		"AWS_SECRET_ACCESS_KEY=secret",
		"FISHHAWK_SOME_FUTURE_SECRET=secret",
		"PATH=/usr/bin",
		"HOME=/home/op",
		"LC_ALL=C",
		"CGO_ENABLED=0",
		"GOFLAGS=-mod=mod",
		"GOPROXY=https://u:p@proxy.example/mod,direct",
		"NOEQUALS",
		"=novalue",
	})
	set := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		set[k] = v
	}
	for _, dropped := range []string{
		"FISHHAWK_API_TOKEN", "GITHUB_TOKEN", "ANTHROPIC_API_KEY",
		"AWS_SECRET_ACCESS_KEY", "FISHHAWK_SOME_FUTURE_SECRET", "NOEQUALS", "",
	} {
		if v, ok := set[dropped]; ok {
			t.Errorf("key %q survived sanitization with value %q", dropped, v)
		}
	}
	for k, want := range map[string]string{
		"PATH": "/usr/bin", "HOME": "/home/op", "LC_ALL": "C",
		"CGO_ENABLED": "0", "GOFLAGS": "-mod=mod",
	} {
		if set[k] != want {
			t.Errorf("%s = %q, want %q", k, set[k], want)
		}
	}
	if set["GOPROXY"] != "https://proxy.example/mod,direct" {
		t.Errorf("GOPROXY = %q, want the userinfo stripped and the entry order preserved", set["GOPROXY"])
	}
}

// TestRedactVerifyGoEnvUserinfo covers the redaction helper's own branches:
// both GOPROXY separators, a non-URL value, a schemeless value, and a
// credential-free URL (which must pass through unchanged).
func TestRedactVerifyGoEnvUserinfo(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"off", "off"},
		{"direct", "direct"},
		{"user:pass@host", "user:pass@host"},
		{"https://proxy.example/mod", "https://proxy.example/mod"},
		{"https://u:p@a.example|https://v:q@b.example", "https://a.example|https://b.example"},
		{"https://u:p@a.example,direct", "https://a.example,direct"},
	} {
		if got := redactVerifyGoEnvUserinfo(tc.in); got != tc.want {
			t.Errorf("redactVerifyGoEnvUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCheckVerifyCommand_DefaultsInheritedVerifyBlock (binding condition 5):
// a spec whose executor — verify block included — is inherited via a
// workflow-level `defaults` block must EXECUTE the rung (ok or fail), never
// warn "no verify block". This is the #2340 blind spot: reading the raw
// author bytes would report a warn on a spec that genuinely configures one.
func TestCheckVerifyCommand_DefaultsInheritedVerifyBlock(t *testing.T) {
	repo := newVerifyRepo(t, `version: "2"
workflows:
  feature_change:
    defaults:
      executor:
        agent: claude-code
        verify:
          command: "echo inherited-marker; exit 7"
          timeout: "15m"
    stages:
      - id: implement
        type: implement
`)
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status == "warn" {
		t.Fatalf("status = warn (%s) — the inherited verify block was not seen; "+
			"the rung must EXECUTE it (#2340). hint: %s", r.detail, r.remediate)
	}
	if r.status != "fail" {
		t.Fatalf("status = %q, want fail (the inherited command exits 7); detail: %s", r.status, r.detail)
	}
	if !strings.Contains(r.detail, "exited 7") {
		t.Errorf("detail = %q, want it to carry the inherited command's exit status", r.detail)
	}
	if !strings.Contains(r.remediate, "inherited-marker") {
		t.Errorf("remediate = %q, want the inherited command's captured output", r.remediate)
	}
}

// TestCheckVerifyCommand_EveryDistinctCommandRuns (binding condition 2): two
// stages declaring DIFFERENT commands both run, and a failure in the SECOND
// fails the rung naming that command — the first passing must not
// short-circuit the check.
//
// The FIRST command's execution is asserted directly, via a marker file it
// creates OUTSIDE the throwaway worktree (so the worktree's removal cannot
// erase the evidence). Asserting only the second command's failure would leave
// the binding "run EVERY distinct command" behavior unpinned: a loop that ran
// only the LAST candidate would produce a byte-identical checkResult.
func TestCheckVerifyCommand_EveryDistinctCommandRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "first-ran")
	repo := newVerifyRepo(t, fmt.Sprintf(`version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "touch %s"
            timeout: "15m"
      - id: second
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "echo second-marker; exit 5"
            timeout: "15m"
`, marker))
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "fail" {
		t.Fatalf("status = %q, want fail (the second command exits 5); detail: %s", r.status, r.detail)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker %s absent (%v) — the FIRST command never executed", marker, err)
	}
	if !strings.Contains(r.detail, "second-marker") || !strings.Contains(r.detail, "exited 5") {
		t.Errorf("detail = %q, want it to name WHICH command failed and its exit status", r.detail)
	}
	if !strings.Contains(r.detail, "second") {
		t.Errorf("detail = %q, want it to name the declaring stage", r.detail)
	}
}

// TestCheckVerifyCommand_GitignoredArtifactFails is the headline end-to-end
// case and the clean-worktree counterfactual vehicle. The repo commits a
// .gitignore that ignores build/, and build/artifact exists in the OPERATOR
// checkout only. The command reads that file, so it PASSES when run directly
// in the checkout (asserted here by construction, not by calling the control)
// and must FAIL in the fresh worktree, where only tracked files materialize.
func TestCheckVerifyCommand_GitignoredArtifactFails(t *testing.T) {
	repo := newVerifyRepo(t,
		specWithCommand("cat build/artifact", "15m"),
		verifyRepoFile{path: ".gitignore", contents: "build/\n"},
	)
	// Seeded BY CONSTRUCTION into the operator checkout only: never
	// committed, so git alone determines its absence in the worktree.
	writeRepoFile(t, repo, "build/artifact", "prebuilt\n")

	// Fixture proof: the very same command succeeds in the operator checkout.
	probe := exec.Command("sh", "-c", "cat build/artifact")
	probe.Dir = repo
	if err := probe.Run(); err != nil {
		t.Fatalf("fixture is wrong: the command must PASS in the operator checkout, got %v", err)
	}

	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	if r.status != "fail" {
		t.Fatalf("status = %q, want fail — the gitignored artifact must be absent from the fresh "+
			"worktree; detail: %s; hint: %s", r.status, r.detail, r.remediate)
	}
	if !strings.Contains(r.remediate, "TRACKED files") {
		t.Errorf("remediate = %q, want the fresh-worktree explanation", r.remediate)
	}
}

// --- cleanup on every exit path ----------------------------------------------

// TestCheckVerifyCommand_CleanupRemovesWorktree asserts, for BOTH the pass and
// the fail path, that the throwaway worktree is gone from disk and from
// `git worktree list` after the call returns. The assertion reads committed
// filesystem/git state, not the returned error — a cleanup that never ran
// returns a byte-identical checkResult.
func TestCheckVerifyCommand_CleanupRemovesWorktree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		{"pass", "true", "ok"},
		{"fail", "exit 4", "fail"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo := newVerifyRepo(t, specWithCommand(tc.command, "15m"))
			worktree := doctorVerifyWorktreeFor(t, repo)
			r := checkVerifyCommand(repo, false, verifyTestDeadline)
			if r.status != tc.want {
				t.Fatalf("status = %q, want %q; detail: %s", r.status, tc.want, r.detail)
			}
			assertWorktreeGone(t, repo, worktree)
		})
	}
}

// TestCheckVerifyCommand_CleanupRemovesWorktreeAfterTimeout (binding
// condition 3) asserts the same on the TIMEOUT path, which is the one most
// likely to leak because the group kill runs first.
func TestCheckVerifyCommand_CleanupRemovesWorktreeAfterTimeout(t *testing.T) {
	repo := newVerifyRepo(t, specWithCommand(
		"sleep "+verifyTestWedge+" & sleep "+verifyTestWedge, "15m"))
	worktree := doctorVerifyWorktreeFor(t, repo)

	start := time.Now()
	r := checkVerifyCommand(repo, false, verifyTestDeadline)
	elapsed := time.Since(start)

	if r.status != "fail" || !strings.Contains(r.detail, "timed out") {
		t.Fatalf("status = %q detail = %q, want a timeout fail", r.status, r.detail)
	}
	if elapsed > verifyTestBound {
		t.Errorf("took %s, want under %s", elapsed, verifyTestBound)
	}
	assertWorktreeGone(t, repo, worktree)
}

// --- outputTail bounds --------------------------------------------------------

// TestOutputTail covers the helper's own branches: empty input, a short
// unmarked tail, a line-count truncation, and a byte-count truncation.
func TestOutputTail(t *testing.T) {
	if got := outputTail("\n\n"); got != "(no output)" {
		t.Errorf("outputTail(blank) = %q, want %q", got, "(no output)")
	}
	if got := outputTail("a\nb\n"); got != "a\nb" {
		t.Errorf("outputTail(short) = %q, want %q", got, "a\nb")
	}
	many := strings.Repeat("x\n", verifyOutputTailLines+5)
	if got := outputTail(many); !strings.HasPrefix(got, "[output truncated]") {
		t.Errorf("outputTail(many lines) = %q, want a truncation marker", got)
	}
	long := strings.Repeat("y", verifyOutputTailBytes*2)
	got := outputTail(long)
	if !strings.HasPrefix(got, "[output truncated]") {
		t.Errorf("outputTail(long line) = %q, want a truncation marker", got)
	}
	if len(got) > verifyOutputTailBytes+len("[output truncated] ") {
		t.Errorf("outputTail(long line) length = %d, want bounded by %d", len(got), verifyOutputTailBytes)
	}
}
