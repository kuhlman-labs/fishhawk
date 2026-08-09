package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// runStateCancelled is THE literal wire vocabulary word for a cancelled run,
// and the ONE definition every runner-side consumer of that vocabulary reads
// (#2545). The lineage-lock reclaim below evicts a holder ONLY when the
// backend reports its run in exactly this state, so a second hand-typed copy
// anywhere in this module would let the two drift apart silently — a case
// change or an enum rename on the backend would disarm the reclaim while
// every runner test stayed green. The backend half of the pin lives in
// backend/internal/server/runs_get_test.go, which serialises a CANCELLED run
// and asserts the wire carries this exact literal.
const runStateCancelled = "cancelled"

// runIDArgFlag is the runner-spawn flag token that carries the run id. It is
// the single source of truth the identity matcher derives its expectation
// from: the MCP side's composeRunnerArgv emits `--run-id` and the id as two
// ADJACENT argv tokens, and runnerIdentityMatches requires exactly that pair,
// so a reshape of the spawn argv breaks a test rather than silently disarming
// the identity check (the #1970 stageIDArgFlag discipline).
const runIDArgFlag = "--run-id"

// runnerBinaryName is the basename every fishhawk-runner process carries in
// argv[0]. runnerIdentityMatches matches it on a path/basename BOUNDARY (see
// binaryTokenIsRunner), never as a floating substring.
const runnerBinaryName = "fishhawk-runner"

// Named refusal reasons for the lineage-lock holder rule. Each is a distinct
// branch of evictTerminalLockHolder and each has its own test: an
// unprovable holder is NEVER signalled and keeps today's fail-loud refusal.
var (
	// errHolderClientUnavailable: no backend client was threaded in, so the
	// holder's run state cannot be read at all.
	errHolderClientUnavailable = errors.New("holder run state unreadable: no backend client")
	// errHolderRunIDUnknown: the lockfile's line-2 run id is absent or empty.
	errHolderRunIDUnknown = errors.New("holder run id not recorded in the lockfile")
	// errHolderStateUnknown: the run-state read failed, 404'd, or returned an
	// empty state.
	errHolderStateUnknown = errors.New("holder run state unknown")
	// errHolderNotCancelled: the holder's run is not cancelled. running, failed
	// AND succeeded all refuse — a run flips terminal in the backend BEFORE its
	// runner has necessarily finished post-stage work (PR push, trace upload),
	// so only an explicit operator cancel is evictable.
	errHolderNotCancelled = errors.New("holder run is not cancelled")
	// errHolderIdentityUnprovable: the pid's real argv does not prove it is a
	// fishhawk-runner carrying `--run-id <holder run id>`.
	errHolderIdentityUnprovable = errors.New("holder run cancelled but process identity unprovable")
	// errHolderSurvivedKill: the holder was signalled TERM then KILL and is
	// STILL alive. The lockfile is left INTACT — never break a lock a live
	// holder still owns.
	errHolderSurvivedKill = errors.New("holder survived TERM and KILL")
	// errSignalUnsupported is returned by the non-unix signalLockHolder stub.
	errSignalUnsupported = errors.New("signalling a lock holder is unsupported on this platform")
)

// runStateClient is the backend read the lineage-lock reclaim needs: the live
// state of the run recorded in a contended lockfile. Satisfied by
// *upload.Client; nil selects the refuse branch.
type runStateClient interface {
	RunState(ctx context.Context, runID string) (string, error)
}

// lockHolderTermGrace bounds how long the eviction waits for a TERMed holder
// to exit before escalating to KILL. lockHolderKillGrace bounds the wait after
// KILL before the eviction gives up and refuses (leaving the lock intact).
// Package vars so tests can shrink them without real wall-clock waits.
var (
	lockHolderTermGrace = 5 * time.Second
	lockHolderKillGrace = 5 * time.Second
)

// lockHolderPollInterval is the liveness poll cadence inside the two graces.
// Deliberately unscaled — it is a poll interval, not a deadline.
const lockHolderPollInterval = 20 * time.Millisecond

// processArgvTimeout bounds the `ps` exec so a wedged process table read can
// never hang a stage.
const processArgvTimeout = 5 * time.Second

// processArgv returns the full argument list of pid as `ps` renders it.
// Injectable so tests can drive the unprovable-identity branches without
// depending on the host's process table; the production body is exercised
// directly by TestProcessArgv_ReadsOwnArgv against os.Getpid().
//
// `ps -o args= -p <pid>` prints the target's full argument list on both macOS
// (BSD ps) and Linux (procps), and prints nothing with a non-zero status for a
// pid that does not exist. Every failure mode routes to a refuse branch.
var processArgv = func(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), processArgvTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "args=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("read argv of pid %d: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// binaryTokenIsRunner reports whether an argv token names the fishhawk-runner
// binary on a PATH/BASENAME boundary. The token is treated as a path, its
// basename taken, and the segment before the first '.' compared for EQUALITY
// with runnerBinaryName — so `/opt/bin/fishhawk-runner`, `fishhawk-runner.exe`
// and the package test binary `fishhawk-runner.test` all match, while
// `evil-fishhawk-runner`, `fishhawk-runnerx` and any longer token that merely
// CONTAINS the name do not. Substring containment is deliberately NOT used: it
// would let an unrelated process authorize its own eviction by carrying the
// product name somewhere in its command line.
func binaryTokenIsRunner(token string) bool {
	base := filepath.Base(token)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return base == runnerBinaryName
}

// runnerIdentityMatches reports whether argv (a whole `ps -o args=` line)
// belongs to a fishhawk-runner process carrying `--run-id <runID>`.
//
// The decision is made on TOKEN BOUNDARIES, never substring containment: argv
// is split on whitespace and the match requires (a) some token whose
// path/basename is the runner binary, and (b) a token exactly equal to
// runIDArgFlag whose IMMEDIATELY FOLLOWING token is exactly equal to runID.
// A longer unrelated argument that merely embeds the flag text or the id text
// therefore does NOT authorize a signal, and the single-token
// `--run-id=<id>` form — which composeRunnerArgv never emits — is refused.
func runnerIdentityMatches(argv, runID string) bool {
	if runID == "" {
		return false
	}
	tokens := strings.Fields(argv)
	binaryOK := false
	for _, tok := range tokens {
		if binaryTokenIsRunner(tok) {
			binaryOK = true
			break
		}
	}
	if !binaryOK {
		return false
	}
	for i, tok := range tokens {
		if tok != runIDArgFlag {
			continue
		}
		if i+1 < len(tokens) && tokens[i+1] == runID {
			return true
		}
	}
	return false
}

// verifyLockHolderIdentity proves pid is a fishhawk-runner carrying
// `--run-id <runID>`. Every unprovable branch — argv read failed (ps absent,
// timed out, pid gone), empty argv, wrong binary, wrong or absent run id —
// returns a named error, and an unprovable holder is NEVER signalled.
func verifyLockHolderIdentity(pid int, runID string) error {
	argv, err := processArgv(pid)
	if err != nil {
		return fmt.Errorf("%w: %v", errHolderIdentityUnprovable, err)
	}
	if strings.TrimSpace(argv) == "" {
		return fmt.Errorf("%w: pid %d reported no argv", errHolderIdentityUnprovable, pid)
	}
	if !runnerIdentityMatches(argv, runID) {
		return fmt.Errorf("%w: pid %d argv is not a fishhawk-runner carrying %s %s",
			errHolderIdentityUnprovable, pid, runIDArgFlag, runID)
	}
	return nil
}

// evictTerminalLockHolder is the lineage-lock reclaim for the #2545 wedge: a
// runner orphaned by a cancel keeps holding its lineage lockfile, so every
// later same-lineage dispatch fails category-C until an operator kills it by
// hand.
//
// It refuses unless ALL of the following are PROVEN, in order — each refusal
// independently observable and separately tested:
//
//	(a) a backend client was threaded in;
//	(b) the lockfile records a holder run id on line 2;
//	(c) the backend reports a state for that run (a read error, a 404 and an
//	    empty state all refuse);
//	(d) that state is exactly runStateCancelled — running, failed AND
//	    succeeded all refuse;
//	(e) the pid's REAL argv identifies it as a fishhawk-runner carrying
//	    `--run-id <that run id>`.
//
// (e) is the AUTHORIZING control: nothing else in this function decides
// whether a signal may be delivered at all. The group-leader test inside
// signalLockHolder only chooses group-signal vs single-pid signal.
//
// Only when all five pass does it signal TERM, poll for death up to
// lockHolderTermGrace, escalate to KILL, and poll up to lockHolderKillGrace. A
// holder that survives both leaves the lockfile INTACT and returns
// errHolderSurvivedKill. Mirrors scripts/dev's _down_port_fallback (#965/#1018):
// refuse rather than kill what you cannot identify.
//
// Returns the holder's observed run state (empty when it could not be read) so
// the caller's refusal error can name it, plus the refusal reason.
func evictTerminalLockHolder(ctx context.Context, lockPath, root string, pid int, client runStateClient, logSink io.Writer) (string, error) {
	if client == nil {
		return "", errHolderClientUnavailable
	}
	holderRunID := readLockRunID(lockPath)
	if holderRunID == "" {
		return "", errHolderRunIDUnknown
	}
	state, err := client.RunState(ctx, holderRunID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errHolderStateUnknown, err)
	}
	if state == "" {
		return "", fmt.Errorf("%w: backend reported no state for run %s", errHolderStateUnknown, holderRunID)
	}
	if state != runStateCancelled {
		return state, fmt.Errorf("%w (state %q)", errHolderNotCancelled, state)
	}
	if idErr := verifyLockHolderIdentity(pid, holderRunID); idErr != nil {
		return state, idErr
	}

	if sigErr := signalLockHolder(pid, syscall.SIGTERM); sigErr != nil {
		return state, fmt.Errorf("%w: TERM: %v", errHolderSurvivedKill, sigErr)
	}
	if !waitProcessGone(pid, lockHolderTermGrace) {
		if sigErr := signalLockHolder(pid, syscall.SIGKILL); sigErr != nil {
			return state, fmt.Errorf("%w: KILL: %v", errHolderSurvivedKill, sigErr)
		}
		if !waitProcessGone(pid, lockHolderKillGrace) {
			return state, errHolderSurvivedKill
		}
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"lineage_lock_evicted","root":%q,"holder_pid":%d,"holder_run_id":%q,"holder_state":%q}`+"\n",
		root, pid, holderRunID, state)
	return state, nil
}

// waitProcessGone polls processAlive until pid is gone or the grace elapses.
func waitProcessGone(pid int, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		if !processAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(lockHolderPollInterval)
	}
}

// readLockRunID reads the run id recorded on the SECOND line of a lineage
// lockfile (the format acquireLineageLock writes: pid, then run id). Returns
// "" when the file is unreadable or carries no second line — which routes to
// the errHolderRunIDUnknown refusal.
func readLockRunID(lockPath string) string {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 {
		return ""
	}
	return strings.TrimSpace(lines[1])
}
