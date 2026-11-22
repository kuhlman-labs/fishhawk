package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// host_dispatch_lineage_lock.go is the host dispatcher's lineage-lock probe
// (#5485). A runner holds <git-common-dir>/fishhawk-worktrees/run-<root>.lock
// for its whole process lifetime, which outlives its stage's terminal state: an
// acceptance runner tearing down its preview still holds it after the stage
// reads succeeded. A stage spawned into that lock dies category C
// lineage_lock_contention. The dispatcher's ledger cannot see the holder, so
// evaluate asks the runner itself, through the read-only
// `fishhawk-runner lineage-lock-status` subcommand, with the same lineage
// inputs the spawn will carry. The runner owns the path rule and the live-holder
// predicate; this module carries only the subcommand name, flag names and JSON
// field names, which TestProbeHostDispatchLineageLock_RealRunner pins against
// the real binary.

// hostDispatchLineageLockSubcommand byte-matches the runner's
// lineageLockStatusSubcommand.
const hostDispatchLineageLockSubcommand = "lineage-lock-status"

// hostDispatchLineageLockProbeTimeout bounds one probe exec. Every candidate
// that reaches the probe counts against the per-tick cap, so a tick spends at
// most PerTickCap × this on probes: 4 × 5s = 20s at the default cap, well under
// the server's 60s concurrency QueueTTL that a queued stage's re-POST must beat.
// A var so a test can shorten it.
var hostDispatchLineageLockProbeTimeout = 5 * time.Second

// hostDispatchLineageLockStdoutMax bounds the probe's stdout.
const hostDispatchLineageLockStdoutMax = 64 << 10

// hostDispatchLineageLockInput is the lineage a stage spawn would lock.
type hostDispatchLineageLockInput struct {
	WorkingDir      string
	RunID           string
	DecomposedFrom  string
	ParallelIsolate bool
}

// hostDispatchLineageLock is one probe verdict. Err non-nil means the probe
// could not decide; evaluate then refuses fail closed.
type hostDispatchLineageLock struct {
	Held        bool
	HolderPID   int
	HolderRunID string
	LockPath    string
	Root        string
	Err         error
}

// hostDispatchLineageLockInputFor maps a candidate to the lineage inputs its
// spawned runner computes its root from: the run id, the decomposed_from the
// prompt serves (runs.decomposed_from), and --parallel-isolate, which
// childRunnerArgv adds exactly when the spawn is a decomposed child's
// implement stage (isChild).
func hostDispatchLineageLockInputFor(run *Run, isChild bool) hostDispatchLineageLockInput {
	in := hostDispatchLineageLockInput{
		WorkingDir:      run.WorkingDir,
		RunID:           run.ID,
		ParallelIsolate: isChild,
	}
	if run.DecomposedFrom != nil {
		in.DecomposedFrom = *run.DecomposedFrom
	}
	return in
}

// hostDispatchLineageLockArgv is the subcommand argv for in.
func hostDispatchLineageLockArgv(in hostDispatchLineageLockInput) []string {
	argv := []string{hostDispatchLineageLockSubcommand, "--working-dir", in.WorkingDir, "--run-id", in.RunID}
	if in.DecomposedFrom != "" {
		argv = append(argv, "--decomposed-from", in.DecomposedFrom)
	}
	if in.ParallelIsolate {
		argv = append(argv, "--parallel-isolate")
	}
	return argv
}

// hdCappedBuffer keeps the first max bytes written and records an overflow.
type hdCappedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *hdCappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

// hostDispatchLineageLockWire is the subcommand's JSON line. Held is a pointer
// so an absent or null "held" is distinguishable from false.
type hostDispatchLineageLockWire struct {
	Root        string `json:"root"`
	LockPath    string `json:"lock_path"`
	Present     bool   `json:"present"`
	Held        *bool  `json:"held"`
	HolderPID   int    `json:"holder_pid"`
	HolderRunID string `json:"holder_run_id"`
}

// probeHostDispatchLineageLock execs `<binary> lineage-lock-status` with env
// under hostDispatchLineageLockProbeTimeout and decodes its verdict. Every
// undecidable outcome sets Err: a timeout, an exec error, a non-zero exit
// (exit 2 is a runner without the subcommand, exit 1 a working_dir that is not
// a git checkout), stdout past the bound, undecodable JSON, an absent or null
// "held", and an empty "lock_path".
func probeHostDispatchLineageLock(ctx context.Context, binary string, env []string, in hostDispatchLineageLockInput) hostDispatchLineageLock {
	timeout := hostDispatchLineageLockProbeTimeout
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(pctx, binary, hostDispatchLineageLockArgv(in)...)
	cmd.Env = env
	// A killed probe's grandchild (git) may still hold the pipes; stop
	// waiting for them shortly after the kill.
	cmd.WaitDelay = time.Second
	stdout := &hdCappedBuffer{max: hostDispatchLineageLockStdoutMax}
	stderr := &hdCappedBuffer{max: hostDispatchTextMax}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	tail := boundHostDispatchText(stderr.buf.String())
	withTail := func(msg string) error {
		if tail != "" {
			msg += "; stderr: " + tail
		}
		return errors.New(msg)
	}
	if runErr != nil {
		if errors.Is(pctx.Err(), context.DeadlineExceeded) {
			return hostDispatchLineageLock{Err: withTail(fmt.Sprintf(
				"%s %s timed out after %s; check the host for a hung git or fishhawk-runner process",
				binary, hostDispatchLineageLockSubcommand, timeout))}
		}
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return hostDispatchLineageLock{Err: fmt.Errorf(
				"could not exec %s %s: %w; deploy fishhawk-runner beside fishhawk-mcp",
				binary, hostDispatchLineageLockSubcommand, runErr)}
		}
		switch code := ee.ExitCode(); code {
		case 2:
			return hostDispatchLineageLock{Err: withTail(fmt.Sprintf(
				"%s %s exited 2 (usage): this fishhawk-runner does not support the %s subcommand or its flags; deploy a fishhawk-runner that supports %s beside fishhawk-mcp",
				binary, hostDispatchLineageLockSubcommand, hostDispatchLineageLockSubcommand, hostDispatchLineageLockSubcommand))}
		case 1:
			return hostDispatchLineageLock{Err: withTail(fmt.Sprintf(
				"%s %s exited 1: the runner could not resolve the lineage lock under working_dir %q (not a git checkout, or the lockfile could not be read)",
				binary, hostDispatchLineageLockSubcommand, in.WorkingDir))}
		default:
			return hostDispatchLineageLock{Err: withTail(fmt.Sprintf(
				"%s %s exited %d", binary, hostDispatchLineageLockSubcommand, code))}
		}
	}
	if stdout.overflow {
		return hostDispatchLineageLock{Err: fmt.Errorf("%s output exceeded %d bytes", hostDispatchLineageLockSubcommand, hostDispatchLineageLockStdoutMax)}
	}
	var w hostDispatchLineageLockWire
	if err := json.Unmarshal(bytes.TrimSpace(stdout.buf.Bytes()), &w); err != nil {
		return hostDispatchLineageLock{Err: fmt.Errorf("%s output is not a JSON verdict: %w", hostDispatchLineageLockSubcommand, err)}
	}
	if w.Held == nil {
		return hostDispatchLineageLock{Err: fmt.Errorf("%s verdict has no held value", hostDispatchLineageLockSubcommand)}
	}
	if w.LockPath == "" {
		return hostDispatchLineageLock{Err: fmt.Errorf("%s verdict has an empty lock_path", hostDispatchLineageLockSubcommand)}
	}
	return hostDispatchLineageLock{
		Held: *w.Held, HolderPID: w.HolderPID, HolderRunID: w.HolderRunID,
		LockPath: w.LockPath, Root: w.Root,
	}
}
