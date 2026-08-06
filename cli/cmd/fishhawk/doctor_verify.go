package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// verifyRungLabel is the doctor rung's display label.
const verifyRungLabel = "verify command"

// freshWorktreeCaveat is the one-sentence explanation of WHY a command
// that passes in the operator's checkout can still fail here. It is the
// same sentence the shipped presets carry above `command:`.
const freshWorktreeCaveat = "a fresh worktree contains only TRACKED files, so gitignored build " +
	"artifacts and downloaded dependencies are absent by construction"

// Output-tail bounds for a failing command's captured output. The
// remediation must be readable in a terminal, so the tail is capped by
// BOTH a line count and a byte count and marks any truncation.
const (
	verifyOutputTailLines = 20
	verifyOutputTailBytes = 2000
)

// defaultVerifyTimeout is the doctor-side cap on how long EACH configured
// verify command may run. The presets ship `timeout: "15m"` because the
// runner's gate is the real test run; `doctor` is a preflight that should
// cost seconds, so the cap — not the spec value — is the default ceiling.
// An operator who wants the spec value to win outright passes a larger
// --verify-timeout.
const defaultVerifyTimeout = 5 * time.Minute

// verifyCleanupTimeout bounds the two git cleanup commands, which must
// run even after the caller's context was cancelled to kill a wedged
// verify command. They are detached from that cancellation but never
// unbounded.
const verifyCleanupTimeout = 30 * time.Second

// verifySpecState enumerates why collectVerifyCommands produced no
// runnable command. Each value maps to exactly one warn branch in
// checkVerifyCommand's outcome table.
type verifySpecState int

const (
	// verifySpecRunnable — at least one non-empty verify command was found.
	verifySpecRunnable verifySpecState = iota
	// verifySpecNoSpec — no .fishhawk/workflows.yaml under the working dir.
	verifySpecNoSpec
	// verifySpecReadError — the spec exists but could not be read.
	verifySpecReadError
	// verifySpecResolveError — same-document reuse could not be resolved.
	verifySpecResolveError
	// verifySpecParseError — the resolved document is not parseable YAML.
	verifySpecParseError
	// verifySpecNoVerifyBlock — no stage declares an executor.verify block.
	verifySpecNoVerifyBlock
	// verifySpecEmptyCommand — a verify block exists but its command is empty.
	verifySpecEmptyCommand
)

// verifyCandidate is one distinct verify command discovered in the spec,
// carrying the stage that declared it (so a failure can name WHICH
// command failed) and the stage's own configured timeout.
type verifyCandidate struct {
	command string
	stage   string
	// timeout is the spec's configured `executor.verify.timeout`, or 0
	// when absent or unparseable — the doctor-side cap then applies.
	timeout time.Duration
}

// collectVerifyCommands discovers the DISTINCT `executor.verify.command`
// values configured by the working dir's committed workflow spec.
//
// It reads the RESOLVED document (spec.ResolveReuse) rather than the raw
// author bytes, for the same reason checkExecutionPath does (#2340): a
// workflow-v2 stage may inherit its whole executor — including the verify
// block — from a file- or workflow-level `defaults` block or an `extends`
// base. Reading the raw bytes would report "no verify block" on a spec
// that genuinely configures one, silently reinstating the blind spot this
// rung exists to close.
//
// Commands are deduped by command string and returned in a deterministic
// order (workflow name, then stage order) so the rung's output does not
// depend on Go's randomized map iteration.
func collectVerifyCommands(workingDir string) ([]verifyCandidate, verifySpecState) {
	ds, err := discoverSpec(workingDir, "")
	if err != nil {
		return nil, verifySpecReadError
	}
	if ds == nil {
		return nil, verifySpecNoSpec
	}
	resolved, err := spec.ResolveReuse(ds.Contents)
	if err != nil {
		return nil, verifySpecResolveError
	}

	var parsed struct {
		Workflows map[string]struct {
			Stages []struct {
				ID       string `yaml:"id"`
				Executor struct {
					// Pointer so an ABSENT verify block is
					// distinguishable from one whose command is empty.
					Verify *struct {
						Command string `yaml:"command"`
						Timeout string `yaml:"timeout"`
					} `yaml:"verify"`
				} `yaml:"executor"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(resolved, &parsed); err != nil {
		return nil, verifySpecParseError
	}

	wfNames := make([]string, 0, len(parsed.Workflows))
	for name := range parsed.Workflows {
		wfNames = append(wfNames, name)
	}
	sort.Strings(wfNames)

	var out []verifyCandidate
	seen := map[string]bool{}
	sawVerifyBlock := false
	for _, wfName := range wfNames {
		for i, st := range parsed.Workflows[wfName].Stages {
			if st.Executor.Verify == nil {
				continue
			}
			sawVerifyBlock = true
			cmdStr := strings.TrimSpace(st.Executor.Verify.Command)
			if cmdStr == "" || seen[cmdStr] {
				continue
			}
			seen[cmdStr] = true
			stage := st.ID
			if stage == "" {
				stage = fmt.Sprintf("%s[%d]", wfName, i)
			}
			var d time.Duration
			if parsedDur, derr := time.ParseDuration(strings.TrimSpace(st.Executor.Verify.Timeout)); derr == nil && parsedDur > 0 {
				d = parsedDur
			}
			out = append(out, verifyCandidate{command: cmdStr, stage: wfName + "/" + stage, timeout: d})
		}
	}
	if len(out) == 0 {
		if sawVerifyBlock {
			return nil, verifySpecEmptyCommand
		}
		return nil, verifySpecNoVerifyBlock
	}
	return out, verifySpecRunnable
}

// doctorVerifyWorktreeName is the throwaway worktree's directory name.
// Keyed by pid so two concurrent doctor invocations on one checkout never
// collide. Exposed as a function so a test can compute the same path.
func doctorVerifyWorktreeName() string {
	return fmt.Sprintf("doctor-verify-%d", os.Getpid())
}

// provisionDoctorWorktree provisions a throwaway DETACHED git worktree at
// the working dir's current HEAD — the same shape the runner provisions
// for its committed-tree verify gate (`git worktree add --detach <path>
// <pinned sha>`, runner/cmd/fishhawk-runner/worktree.go). A fresh worktree
// materializes only TRACKED files, which is the entire point of the rung.
//
// The worktree lands under the SHARED git dir so a doctor run launched
// from a linked worktree still lands under the one shared root — the same
// reason the runner's worktreesDir uses --git-common-dir rather than
// --git-dir. The path is read with `--path-format=absolute` because a
// bare `rev-parse --git-common-dir` returns a RELATIVE path (".git") even
// under `-C <dir>`, which resolved against the DOCTOR PROCESS's cwd rather
// than the target checkout would provision the worktree — and point
// cmd.Dir — at the wrong place entirely.
//
// The returned cleanup is safe to call more than once and never depends
// on the caller's (possibly already-cancelled) context.
func provisionDoctorWorktree(ctx context.Context, workingDir string) (string, func(), error) {
	if _, err := doctorLookPath("git"); err != nil {
		return "", nil, fmt.Errorf("git executable not found: %w", err)
	}
	dir := workingDir
	if dir == "" {
		dir = "."
	}

	out, err := exec.CommandContext(ctx, "git", "-C", dir,
		"rev-parse", "--path-format=absolute", "--git-common-dir").Output() //nolint:gosec
	if err != nil {
		return "", nil, fmt.Errorf("git rev-parse --git-common-dir: %w", err)
	}
	common := strings.TrimSpace(string(out))
	if common == "" || !filepath.IsAbs(common) {
		return "", nil, fmt.Errorf("git rev-parse --git-common-dir returned a non-absolute path %q", common)
	}

	// Pin HEAD ONCE: the same immutable commit is handed to `worktree add`
	// rather than re-resolving the mutable symbolic HEAD.
	headOut, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:gosec
	if err != nil {
		return "", nil, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	headSHA := strings.TrimSpace(string(headOut))
	if headSHA == "" {
		return "", nil, errors.New("git rev-parse HEAD returned an empty SHA")
	}

	parent := filepath.Join(common, "fishhawk-worktrees")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", nil, fmt.Errorf("create %s: %w", parent, err)
	}
	target := filepath.Join(parent, doctorVerifyWorktreeName())

	// Best-effort reclaim of a leftover from a SIGKILLed earlier doctor on
	// the same pid. Deliberately NOT an os.RemoveAll: an unregistered
	// directory here is not ours to delete, and `worktree add` failing on
	// it is an honest warn.
	gitCleanup := func(args ...string) {
		cctx, ccancel := context.WithTimeout(context.Background(), verifyCleanupTimeout)
		defer ccancel()
		_ = exec.CommandContext(cctx, "git", append([]string{"-C", dir}, args...)...).Run() //nolint:gosec
	}
	gitCleanup("worktree", "remove", "--force", target)
	gitCleanup("worktree", "prune")

	if addOut, addErr := exec.CommandContext(ctx, "git", "-C", dir,
		"worktree", "add", "--detach", target, headSHA).CombinedOutput(); addErr != nil { //nolint:gosec
		gitCleanup("worktree", "prune")
		return "", nil, fmt.Errorf("git worktree add: %v: %s", addErr, strings.TrimSpace(string(addOut)))
	}

	cleanup := func() {
		gitCleanup("worktree", "remove", "--force", target)
		// `worktree remove` refuses a tree it considers dirty; the
		// throwaway is ours by construction, so remove what remains.
		_ = os.RemoveAll(target)
		gitCleanup("worktree", "prune")
	}
	return target, cleanup, nil
}

// runVerifyInWorktree executes `sh -c command` in dir under a bounded
// context, returning the exit code (-1 when the command could not be run
// or was killed), the combined output, and whether the deadline fired.
//
// The child is placed in its own process group and the whole group is
// SIGKILLed on cancellation. SIGKILL to the direct child alone leaves
// grandchildren (a `go test` or `make` subprocess) holding the inherited
// stdout pipe open, and CombinedOutput then blocks forever waiting for an
// EOF that never comes — the same hazard the runner's runBoundedGateCommand
// documents and mitigates the same way.
func runVerifyInWorktree(ctx context.Context, dir, command string, timeout time.Duration) (int, string, bool) {
	childCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(childCtx, "sh", "-c", command) //nolint:gosec
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	output, cmdErr := cmd.CombinedOutput()
	exitCode := 0
	if cmdErr != nil {
		var exitErr *exec.ExitError
		if errors.As(cmdErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	timedOut := errors.Is(childCtx.Err(), context.DeadlineExceeded)
	return exitCode, string(output), timedOut
}

// outputTail returns the last verifyOutputTailLines lines of s, further
// bounded to verifyOutputTailBytes, marking any truncation explicitly.
func outputTail(s string) string {
	trimmed := strings.TrimRight(s, "\n")
	if trimmed == "" {
		return "(no output)"
	}
	truncated := false
	lines := strings.Split(trimmed, "\n")
	if len(lines) > verifyOutputTailLines {
		lines = lines[len(lines)-verifyOutputTailLines:]
		truncated = true
	}
	tail := strings.Join(lines, "\n")
	if len(tail) > verifyOutputTailBytes {
		tail = tail[len(tail)-verifyOutputTailBytes:]
		truncated = true
	}
	if truncated {
		return "[output truncated] " + tail
	}
	return tail
}

// checkVerifyCommand is the `verify command` doctor rung: it EXECUTES the
// spec's configured `executor.verify.command` in a throwaway detached git
// worktree at HEAD and reports the result before any run is started.
//
// The outcome table, exhaustively:
//
//   - skip flag set                             -> warn (no subprocess spawned)
//   - no spec / read / resolve / parse error    -> warn (checkSpec and
//     `fishhawk validate` are the authorities on a broken spec; a doctor
//     rung must never be the thing that reports one as a hard fail)
//   - no verify block / empty command           -> warn (the preset
//     explicitly permits removing the block, so this can never be a fail)
//   - git unavailable / not a work tree /
//     HEAD unresolvable / worktree add failed   -> warn (the rung could
//     not reproduce the runner's tree; that is not the operator's spec
//     being wrong)
//   - every command exits 0                     -> ok
//   - any command exits non-zero                -> FAIL, naming WHICH
//   - any command exceeds the deadline          -> FAIL, naming WHICH
//
// Per the operator's binding condition 2, EVERY distinct command runs, the
// effective timeout cap applies PER COMMAND rather than to the collection,
// and the rung is ok only if all of them pass.
func checkVerifyCommand(workingDir string, skip bool, maxTimeout time.Duration) checkResult {
	label := verifyRungLabel
	if skip {
		return checkResult{label: label, detail: "skipped by --skip-verify-command", status: "warn",
			remediate: "drop --skip-verify-command to execute the spec's verify command in a clean worktree"}
	}

	candidates, state := collectVerifyCommands(workingDir)
	switch state {
	case verifySpecNoSpec:
		return checkResult{label: label, detail: "no spec found", status: "warn",
			remediate: "create .fishhawk/workflows.yaml (see docs/spec/workflows-v0.md)"}
	case verifySpecReadError:
		return checkResult{label: label, detail: "spec read error", status: "warn",
			remediate: "fix the read error on .fishhawk/workflows.yaml; `fishhawk validate` is the authority"}
	case verifySpecResolveError:
		return checkResult{label: label, detail: "spec resolve error", status: "warn",
			remediate: "run `fishhawk validate` for details"}
	case verifySpecParseError:
		return checkResult{label: label, detail: "spec parse error", status: "warn",
			remediate: "run `fishhawk validate` for details"}
	case verifySpecNoVerifyBlock:
		return checkResult{label: label, detail: "no verify block configured", status: "warn",
			remediate: "the preset permits removing the verify block, so no test gate will run after the " +
				"implement agent exits; add executor.verify.command to gate the produced tree"}
	case verifySpecEmptyCommand:
		return checkResult{label: label, detail: "verify block declares an empty command", status: "warn",
			remediate: "set executor.verify.command to your repository's test command, or remove the " +
				"verify block entirely"}
	case verifySpecRunnable:
		// fall through to execution.
	}

	if maxTimeout <= 0 {
		maxTimeout = defaultVerifyTimeout
	}

	ctx := context.Background()
	worktree, cleanup, err := provisionDoctorWorktree(ctx, workingDir)
	if err != nil {
		return checkResult{label: label, detail: "clean worktree unavailable", status: "warn",
			remediate: "the verify command was not executed: " + err.Error()}
	}
	defer cleanup()

	for _, c := range candidates {
		effective := maxTimeout
		if c.timeout > 0 && c.timeout < effective {
			effective = c.timeout
		}
		exitCode, output, timedOut := runVerifyInWorktree(ctx, worktree, c.command, effective)
		if timedOut {
			return checkResult{
				label:  label,
				detail: fmt.Sprintf("%q (stage %s) timed out after %s", c.command, c.stage, effective),
				status: "fail",
				remediate: fmt.Sprintf("ran in the throwaway worktree %s and exceeded %s; "+
					"raise executor.verify.timeout or --verify-timeout, or make the command faster. "+
					"Note %s.\noutput tail:\n%s",
					worktree, effective, freshWorktreeCaveat, outputTail(output)),
			}
		}
		if exitCode != 0 {
			return checkResult{
				label:  label,
				detail: fmt.Sprintf("%q (stage %s) exited %d", c.command, c.stage, exitCode),
				status: "fail",
				remediate: fmt.Sprintf("ran in the throwaway worktree %s: %s, so a command that passes in "+
					"your checkout can still fail here. Commit whatever it needs, or change the command.\n"+
					"output tail:\n%s",
					worktree, freshWorktreeCaveat, outputTail(output)),
			}
		}
	}

	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, fmt.Sprintf("%q", c.command))
	}
	return checkResult{
		label:  label,
		detail: strings.Join(names, ", ") + " passed (clean worktree)",
		status: "ok",
	}
}
