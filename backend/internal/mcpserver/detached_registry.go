package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The detached-runner registry (#2545).
//
// fishhawk_dispatch_stage and fishhawk_drive_run spawn fishhawk-runner
// DETACHED, in its own process group, deliberately outliving the tool call.
// fishhawk_cancel_run then flipped the run's state and returned — leaving that
// child alive, still holding its lineage lockfile, so every later
// same-lineage dispatch failed category-C until an operator killed the process
// by hand. This registry is the MCP half of the fix: it tracks the children
// this process spawned so a cancel can reap them.
//
// The load-bearing invariant is that the tombstone check, cmd.Start() and the
// registration all happen under ONE hold of the registry mutex. That closes
// the register-after-Start race BY CONSTRUCTION:
//
//   - a cancel that wins the lock records a tombstone, and the spawn is
//     REFUSED before any fork — no process is created for a cancelled run;
//   - a spawn that wins the lock is REGISTERED before it releases, so the
//     cancel enumerating afterwards always sees the child.
//
// There is no interval in which a started child is unregistered.
//
// Honest limitation: the registry is PER MCP PROCESS. A cancel issued from a
// different MCP server process than the one that dispatched reaps nothing and
// falls through to the runner-side net (runner/cmd/fishhawk-runner's
// evictTerminalLockHolder), which reclaims the lineage lock from a holder it
// can prove is an orphaned runner of a cancelled run.

// runStateCancelled is THE literal wire vocabulary word for a cancelled run,
// shared by every consumer in this package that has to reason about it (the
// tombstone's revived-run admit branch below). The runner side keeps its own
// single definition of the same literal — the two modules have no dependency
// edge — and backend/internal/server/runs_get_test.go pins the wire so a
// vocabulary drift turns a test RED instead of silently disarming both.
const runStateCancelled = "cancelled"

// errRunCancelledBeforeSpawn is returned by startAndRegister when a cancel for
// this run landed before the spawn reached cmd.Start(). No process is forked.
var errRunCancelledBeforeSpawn = errors.New(
	"run was cancelled; not spawning a runner (revive or retry the run first)")

// detachedCancelTombstoneTTL bounds how long a cancel's tombstone can refuse a
// spawn for the same run id.
//
// The tombstone exists ONLY to catch a dispatch already in flight at the
// moment of cancel — the window between "the caller decided to spawn" (its
// host-dispatch marker POST, argv compose, log-file open) and cmd.Start(). That
// is a seconds-scale window, so the TTL is seconds, NOT minutes: a tombstone
// that outlived its purpose would trade the #2545 wedge for a fresh
// dispatch-refusal wedge on any cancel-then-revive inside the window, which is
// unacceptable in a change whose whole point is removing a wedge.
//
// The TTL is only the backstop. The PRIMARY release is the live-state probe in
// startAndRegister: a tombstoned spawn asks the backend for the run's current
// state and is ADMITTED immediately — no waiting — when the run is no longer
// cancelled (revive_run / retry_stage). Package var so tests can shrink it.
var detachedCancelTombstoneTTL = 30 * time.Second

// detachedTombstoneProbeTimeout bounds the live-state probe so a wedged
// backend cannot pin a dispatch.
var detachedTombstoneProbeTimeout = 5 * time.Second

// detachedTerminateGrace is how long terminateRunners waits for a signalled
// child to report exited (its reaper closing done) before escalating
// TERM→KILL, and again after KILL before recording a warning. Package var test
// seam, following the reapReportBackoff precedent.
var detachedTerminateGrace = 5 * time.Second

// detachedSpawnPreStartHook is a test seam that fires INSIDE the registry lock
// immediately before cmd.Start(). The cancel-during-spawn test uses it to park
// a terminateRunners call on the registry mutex — provably contending, not
// merely launched — so the interleaving it exercises is deterministic.
var detachedSpawnPreStartHook func(runID string)

// runStateProbe reads a run's live state. cancelRun hands one to
// terminateRunners so the tombstone it records can admit a later re-dispatch
// the moment the run leaves the cancelled state.
type runStateProbe func(ctx context.Context, runID string) (string, error)

// detachedRunnerHandle is one live detached child this process spawned.
type detachedRunnerHandle struct {
	cmd     *exec.Cmd
	runID   string
	stageID string
	// pid is captured at registration: os/exec's Cmd.Wait releases the Cmd's
	// resources, so cmd.Process must not be read after the reaper's Wait
	// returns. Diagnostics use this copy instead.
	pid int
	// done is closed by the reaper once cmd.Wait has returned. terminateRunners
	// waits on it rather than polling the pid, for the same reason.
	done     chan struct{}
	doneOnce sync.Once
}

// cancelTombstone records that a run was cancelled, with the probe that can
// later prove it is no longer cancelled. gen strictly increases so a spawn that
// released the lock to probe can tell "the tombstone I saw" from "a NEWER
// cancel landed while I probed".
type cancelTombstone struct {
	gen   uint64
	at    time.Time
	probe runStateProbe
}

type detachedRunnerRegistry struct {
	mu sync.Mutex
	// waiters counts goroutines COMMITTED to acquiring mu (incremented before
	// Lock, decremented once acquired). It exists so the cancel-during-spawn
	// test can wait on a real synchronization signal — "the canceller is
	// contending" — instead of sleeping and hoping.
	waiters   atomic.Int64
	gen       uint64
	byRun     map[string][]*detachedRunnerHandle
	cancelled map[string]cancelTombstone
}

var detachedRunners = newDetachedRunnerRegistry()

func newDetachedRunnerRegistry() *detachedRunnerRegistry {
	return &detachedRunnerRegistry{
		byRun:     map[string][]*detachedRunnerHandle{},
		cancelled: map[string]cancelTombstone{},
	}
}

// lock acquires mu, counting the caller as a committed waiter while it blocks.
func (reg *detachedRunnerRegistry) lock() {
	reg.waiters.Add(1)
	reg.mu.Lock()
	reg.waiters.Add(-1)
}

// pruneLocked drops tombstones past their TTL. Called on every mutation so an
// idle registry never accumulates them. Caller holds mu.
func (reg *detachedRunnerRegistry) pruneLocked(now time.Time) {
	for runID, t := range reg.cancelled {
		if now.Sub(t.at) >= detachedCancelTombstoneTTL {
			delete(reg.cancelled, runID)
		}
	}
}

// startAndRegister is the ONE chokepoint through which every detached runner
// is started. See the file comment for the check→Start→register-under-one-lock
// invariant it exists to hold.
//
// On a tombstoned run id it does NOT fork. It first gives the tombstone's
// live-state probe a chance to clear it: a run that has since been revived or
// retried is no longer cancelled, so the tombstone has outlived its purpose and
// the spawn is admitted. A probe that is absent, errors, returns an empty
// state, or still reports `cancelled` refuses — fail closed.
func (reg *detachedRunnerRegistry) startAndRegister(cmd *exec.Cmd, runID, stageID string) (*detachedRunnerHandle, error) {
	// At most two passes: one to observe the tombstone and clear it, one to
	// start. A newer cancel arriving in between refuses rather than looping.
	for attempt := 0; attempt < 2; attempt++ {
		reg.lock()
		reg.pruneLocked(time.Now())
		tomb, tombstoned := reg.cancelled[runID]
		if !tombstoned {
			if detachedSpawnPreStartHook != nil {
				detachedSpawnPreStartHook(runID)
			}
			if err := cmd.Start(); err != nil {
				reg.mu.Unlock()
				return nil, fmt.Errorf("spawn fishhawk-runner: %w", err)
			}
			h := &detachedRunnerHandle{
				cmd:     cmd,
				runID:   runID,
				stageID: stageID,
				pid:     cmd.Process.Pid,
				done:    make(chan struct{}),
			}
			reg.byRun[runID] = append(reg.byRun[runID], h)
			reg.mu.Unlock()
			return h, nil
		}
		probe := tomb.probe
		reg.mu.Unlock()

		if probe == nil {
			return nil, errRunCancelledBeforeSpawn
		}
		ctx, cancel := context.WithTimeout(context.Background(), detachedTombstoneProbeTimeout)
		state, err := probe(ctx, runID)
		cancel()
		if err != nil || state == "" || state == runStateCancelled {
			return nil, errRunCancelledBeforeSpawn
		}
		// The run left the cancelled state. Retire the tombstone WE observed —
		// but only if it is still the one in the map; a newer cancel means the
		// probe's answer is stale and the spawn must refuse.
		reg.lock()
		cur, still := reg.cancelled[runID]
		if still && cur.gen != tomb.gen {
			reg.mu.Unlock()
			return nil, errRunCancelledBeforeSpawn
		}
		delete(reg.cancelled, runID)
		reg.mu.Unlock()
	}
	return nil, errRunCancelledBeforeSpawn
}

// deregister drops a finished child's handle and releases anyone waiting on it.
// Called by the reaper after cmd.Wait returns.
func (reg *detachedRunnerRegistry) deregister(h *detachedRunnerHandle) {
	if h == nil {
		return
	}
	reg.lock()
	remaining := reg.byRun[h.runID][:0]
	for _, existing := range reg.byRun[h.runID] {
		if existing != h {
			remaining = append(remaining, existing)
		}
	}
	if len(remaining) == 0 {
		delete(reg.byRun, h.runID)
	} else {
		reg.byRun[h.runID] = remaining
	}
	reg.mu.Unlock()
	h.doneOnce.Do(func() { close(h.done) })
}

// terminateRunners records the cancel tombstone and reaps every detached
// runner this process spawned for runID: TERM → grace → KILL → grace. It
// returns how many children it confirmed exited plus one warning per child
// that never reported done (naming run id, stage id and pid).
//
// probe is the live-state read the tombstone keeps so a later re-dispatch of a
// revived run is admitted immediately; nil is accepted (the TTL is then the
// only release).
//
// A warning NEVER fails the cancel verb: the state change already landed, and
// turning a landed mutation into a tool error is the exact failure #2510
// removed from this very type.
func (reg *detachedRunnerRegistry) terminateRunners(runID string, probe runStateProbe) (int, []string) {
	reg.lock()
	now := time.Now()
	reg.pruneLocked(now)
	reg.gen++
	reg.cancelled[runID] = cancelTombstone{gen: reg.gen, at: now, probe: probe}
	handles := reg.byRun[runID]
	delete(reg.byRun, runID)
	reg.mu.Unlock()

	reaped := 0
	var warnings []string
	for _, h := range handles {
		runStageSignalGroup(h.cmd, syscall.SIGTERM)
		if waitHandleDone(h, detachedTerminateGrace) {
			reaped++
			continue
		}
		runStageSignalGroup(h.cmd, syscall.SIGKILL)
		if waitHandleDone(h, detachedTerminateGrace) {
			reaped++
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"detached runner for stage %s (pid %d) did not exit after TERM and KILL; "+
				"it may still hold the lineage lock — kill pid %d by hand", h.stageID, h.pid, h.pid))
	}
	return reaped, warnings
}

// waitHandleDone waits up to grace for the reaper to report the child exited.
func waitHandleDone(h *detachedRunnerHandle, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-h.done:
		return true
	case <-timer.C:
		return false
	}
}
