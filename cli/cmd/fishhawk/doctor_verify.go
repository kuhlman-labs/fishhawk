package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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

// --- credential stripping for the spec-supplied child ------------------------
//
// This rung executes a command string read from the repository's committed
// workflow spec. That is the same `sh -c <verifyCmd>` child the RUNNER executes
// during every implement stage — and the runner does NOT let it inherit the
// runner's process environment, for the reason ADR-029 (#650) item 4 states:
// agent-authored code plus network plus the invoking process's credentials is
// the lethal-trifecta shape. Running the same string from `doctor` without the
// same stripping would move that execution to the OPERATOR's machine while
// handing the child the operator's `FISHHAWK_API_TOKEN`, GitHub token, and
// agent API keys — a strictly larger exfiltration surface than the runner's,
// which is what makes the stripping load-bearing here and not merely tidy.
//
// The policy mirrors runner/cmd/fishhawk-runner/gateenv.go: DEFAULT-DENY
// (allow-list), so a credential env var introduced later is dropped
// automatically, plus an explicit known-secret denylist as belt-and-suspenders.
// The two lists are deliberately duplicated rather than shared: `cli` and
// `runner` are separate Go modules and the runner's copy lives in package main.

// verifyEnvAllowExact is the set of system-essential variable names a verify
// command needs to run at all: PATH to find its interpreter and tools, HOME for
// the default GOPATH/GOCACHE when those are unset, plus the usual
// locale/terminal/temp essentials.
var verifyEnvAllowExact = map[string]struct{}{
	"PATH":    {},
	"HOME":    {},
	"USER":    {},
	"LOGNAME": {},
	"SHELL":   {},
	"TMPDIR":  {},
	"TMP":     {},
	"TEMP":    {},
	"TERM":    {},
	"TZ":      {},
	"LANG":    {},
	"CC":      {},
	"CXX":     {},
}

// verifyEnvAllowPrefix lists key prefixes admitted wholesale: every GO* var
// (GOPATH/GOCACHE/GOMODCACHE/GOPROXY/GOFLAGS/GOTOOLCHAIN/…), every CGO_* var,
// and every LC_* locale var. Dropping the GO* prefix would turn a real verify
// failure into a spurious one on any host with a private module proxy or a
// non-default toolchain.
var verifyEnvAllowPrefix = []string{"GO", "CGO_", "LC_"}

// verifyEnvDeny is the explicit known-secret denylist layered on top of the
// default-deny allow-list. These keys are dropped unconditionally.
var verifyEnvDeny = map[string]struct{}{
	"FISHHAWK_GITHUB_TOKEN": {},
	"FISHHAWK_GITLAB_TOKEN": {},
	"GITHUB_TOKEN":          {},
	"GH_TOKEN":              {},
	"ANTHROPIC_API_KEY":     {},
	"OPENAI_API_KEY":        {},
	"FISHHAWK_API_TOKEN":    {},
}

// sanitizedVerifyEnv returns the allow-listed environment assigned to the
// verify child's cmd.Env. Assigning a non-nil cmd.Env replaces the child's
// environment wholesale (os/exec.Cmd.Env: "If Env is nil, the new process uses
// the current process's environment"), so the child sees only these entries.
func sanitizedVerifyEnv() []string {
	return sanitizeVerifyEnv(os.Environ())
}

// sanitizeVerifyEnv applies the default-deny allow-list to base (a slice of
// "KEY=VALUE" entries). It is the testable inner core of sanitizedVerifyEnv.
func sanitizeVerifyEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			// No '=' (malformed) or an empty key — not a usable assignment.
			continue
		}
		key := kv[:eq]
		if _, denied := verifyEnvDeny[key]; denied {
			continue
		}
		if !verifyEnvAllowed(key) {
			continue
		}
		if strings.HasPrefix(key, "GO") {
			// GOPROXY/GOSUMDB may embed operator credentials in a URL; strip
			// the userinfo before the value reaches spec-supplied code.
			out = append(out, key+"="+redactVerifyGoEnvUserinfo(kv[eq+1:]))
			continue
		}
		out = append(out, kv)
	}
	return out
}

// verifyEnvAllowed reports whether key is on the allow-list (exact match or an
// allowed prefix).
func verifyEnvAllowed(key string) bool {
	if _, ok := verifyEnvAllowExact[key]; ok {
		return true
	}
	for _, p := range verifyEnvAllowPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// redactVerifyGoEnvUserinfo strips embedded URL userinfo from a GO* env value.
// The value may be a list of proxy entries separated by ',' (fall through on
// 404/410) or '|' (fall through on any error) per the GOPROXY protocol
// (https://go.dev/ref/mod#goproxy-protocol); both separators and the entry
// order are preserved exactly.
func redactVerifyGoEnvUserinfo(value string) string {
	var b strings.Builder
	start := 0
	for i := 0; i < len(value); i++ {
		if c := value[i]; c == ',' || c == '|' {
			b.WriteString(redactVerifyURLEntryUserinfo(value[start:i]))
			b.WriteByte(c)
			start = i + 1
		}
	}
	b.WriteString(redactVerifyURLEntryUserinfo(value[start:]))
	return b.String()
}

// redactVerifyURLEntryUserinfo returns entry with any embedded URL userinfo
// removed. net/url.Parse populates URL.User only when the entry has a scheme,
// so non-URL forms (off, direct, a bare 'user:pass@host', or a parse error)
// pass through verbatim.
func redactVerifyURLEntryUserinfo(entry string) string {
	u, err := url.Parse(entry)
	if err != nil || u.Scheme == "" || u.User == nil {
		return entry
	}
	u.User = nil
	return u.String()
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
	// The command string comes from the repository's committed spec, so the
	// child never inherits the doctor process's credentials (see above).
	cmd.Env = sanitizedVerifyEnv()
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
	// The deadline is only a TIMEOUT verdict when it actually cost the command
	// its run. Classifying on childCtx.Err() alone races: a command that exits
	// 0 microseconds before the deadline fires would be reported timedOut with
	// exitCode 0, turning a real pass into a spurious preflight fail. Requiring
	// cmdErr != nil removes that window — a successful run never carries one.
	timedOut := cmdErr != nil && errors.Is(childCtx.Err(), context.DeadlineExceeded)
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

// checkVerifyCommandGated is the doctor entry point for the `verify command`
// rung, and the DEFAULT-DENY gate in front of it.
//
// Every other doctor rung reads bytes or queries a service; this one executes a
// command string supplied by the checkout under inspection. `doctor` is the
// first thing an operator runs against a repository — including one they have
// just cloned and not yet read — so executing that string by default would make
// `fishhawk doctor` a code-execution primitive for any repository whose
// `.fishhawk/workflows.yaml` an attacker controls. Under Fishhawk's
// lethal-trifecta and uncontrolled-egress threat model the credential stripping
// in sanitizeVerifyEnv bounds what the child can STEAL, but nothing in the CLI
// bounds what it can SEND: the child inherits the operator's network position,
// and the operator's machine is a strictly higher-value execution context than
// the runner's. So execution is off unless the operator asks for it by name.
//
// Precedence: --skip-verify-command wins over --run-verify-command, so an alias
// or wrapper script that opts in can always be overridden on one invocation.
// Both non-executing paths are a warn that names the flag which changes the
// outcome — the rung stays visible on every `doctor` run rather than silently
// disappearing, which is what #2485 is about.
func checkVerifyCommandGated(workingDir string, optIn, skip bool, maxTimeout time.Duration) checkResult {
	if skip {
		return checkVerifyCommand(workingDir, true, maxTimeout)
	}
	if !optIn {
		return checkResult{
			label:  verifyRungLabel,
			detail: "not executed (opt-in)",
			status: "warn",
			remediate: "this rung EXECUTES the command your repository's committed " +
				".fishhawk/workflows.yaml configures as executor.verify.command, so it is off by " +
				"default and never runs code from a checkout you have not reviewed; pass " +
				"--run-verify-command to execute it in a throwaway detached worktree at HEAD",
		}
	}
	return checkVerifyCommand(workingDir, false, maxTimeout)
}

// checkVerifyCommand is the `verify command` doctor rung: it EXECUTES the
// spec's configured `executor.verify.command` in a throwaway detached git
// worktree at HEAD and reports the result before any run is started.
//
// It is reached only through checkVerifyCommandGated, which owns the
// default-deny opt-in gate; `skip` here is the explicit --skip-verify-command
// opt-out.
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

	for _, c := range candidates {
		// Provision a FRESH worktree PER COMMAND, not once for the whole
		// loop (#2485 implement review, high/correctness). Sharing one
		// worktree lets an earlier command leave an untracked artifact
		// behind that a later command then reads, so the later command
		// passes here while still failing in the runner — a FALSE GREEN in
		// the very rung that exists to catch gitignored-dependency
		// failures. Per-command provisioning is also the faithful model:
		// the runner gives each STAGE its own worktree, and these
		// candidates are collected across stages.
		worktree, cleanup, err := provisionDoctorWorktree(ctx, workingDir)
		if err != nil {
			return checkResult{label: label, detail: "clean worktree unavailable", status: "warn",
				remediate: "the verify command was not executed: " + err.Error()}
		}
		res, done := func() (checkResult, bool) {
			defer cleanup()
			return runVerifyCandidate(ctx, worktree, c, maxTimeout)
		}()
		if done {
			return res
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

// runVerifyCandidate executes ONE candidate command in an already-provisioned
// throwaway worktree. It returns (result, true) when the candidate produced a
// terminal verdict for the whole rung (timeout or non-zero exit) and
// (zero, false) when the command passed and the loop should continue.
//
// Split out of checkVerifyCommand so each candidate's worktree cleanup can run
// via defer at the end of ITS OWN iteration rather than accumulating until the
// enclosing function returns (#2485).
func runVerifyCandidate(ctx context.Context, worktree string, c verifyCandidate, maxTimeout time.Duration) (checkResult, bool) {
	const label = "verify command"

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
		}, true
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
		}, true
	}
	return checkResult{}, false
}
