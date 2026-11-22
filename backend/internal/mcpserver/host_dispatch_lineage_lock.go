package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
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
// most PerTickCap × (this + hostDispatchLineageLockWaitDelay) on hung probes:
// 4 × 6s = 24s at the default cap, well under the server's 60s concurrency
// QueueTTL that a queued stage's re-POST must beat. PerTickCap has no upper
// limit, so the bound stays well under the QueueTTL only for a cap of 8 or less
// (48s); at 10 it reaches 60s (mcpserver README, accepted risks). A var so a
// test can shorten it.
var hostDispatchLineageLockProbeTimeout = 5 * time.Second

// hostDispatchLineageLockWaitDelay is how long a killed probe's Wait waits for
// a grandchild (git) still holding its pipes.
const hostDispatchLineageLockWaitDelay = time.Second

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
	cmd.WaitDelay = hostDispatchLineageLockWaitDelay
	stdout := &hdCappedBuffer{max: hostDispatchLineageLockStdoutMax}
	stderr := &hdCappedBuffer{max: hostDispatchTextMax}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	// Each message leads with the discriminator and remedy, then the stderr
	// tail, then the paths, so the hostDispatchTextMax cut on the refusal
	// detail drops the paths first.
	tail := boundHostDispatchText(stderr.buf.String())
	compose := func(msg string, paths ...string) error {
		if tail != "" {
			msg += "; stderr: " + tail
		}
		return errors.New(strings.Join(append([]string{msg}, paths...), "; "))
	}
	runner := "runner " + binary
	if runErr != nil {
		if errors.Is(pctx.Err(), context.DeadlineExceeded) {
			return hostDispatchLineageLock{Err: compose(fmt.Sprintf(
				"%s timed out after %s; check the host for a hung git or fishhawk-runner process",
				hostDispatchLineageLockSubcommand, timeout), runner)}
		}
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return hostDispatchLineageLock{Err: fmt.Errorf(
				"could not exec %s: deploy fishhawk-runner beside fishhawk-mcp; %w",
				hostDispatchLineageLockSubcommand, runErr)}
		}
		switch code := ee.ExitCode(); code {
		case 2:
			return hostDispatchLineageLock{Err: compose(fmt.Sprintf(
				"%s exited 2 (usage): this fishhawk-runner does not support the subcommand or its flags; deploy one that supports %s beside fishhawk-mcp",
				hostDispatchLineageLockSubcommand, hostDispatchLineageLockSubcommand), runner)}
		case 1:
			return hostDispatchLineageLock{Err: compose(fmt.Sprintf(
				"%s exited 1: working_dir is not a git checkout or the lockfile is unreadable",
				hostDispatchLineageLockSubcommand), fmt.Sprintf("working_dir %q", in.WorkingDir), runner)}
		default:
			return hostDispatchLineageLock{Err: compose(fmt.Sprintf(
				"%s exited %d", hostDispatchLineageLockSubcommand, code), runner)}
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
		Held: *w.Held, HolderPID: w.HolderPID, HolderRunID: hostDispatchLockHolderRunID(w.HolderRunID),
		LockPath: w.LockPath, Root: w.Root,
	}
}

// hostDispatchLockHolderRunID blanks a holder run id that is not a canonical
// UUID. The lockfile's second line is free text any writer to the worktrees
// directory controls, and the id is embedded in the refusal detail and the
// overlay's operator guidance; a blank id renders as "unknown".
func hostDispatchLockHolderRunID(s string) string {
	u, err := uuid.Parse(s)
	if err != nil || u.String() != strings.ToLower(s) {
		return ""
	}
	return s
}
