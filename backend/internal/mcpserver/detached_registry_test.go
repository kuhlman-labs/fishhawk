package mcpserver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// --- #2545 detached-runner registry ---

// freshRegistry swaps the package-level registry for an isolated one so a test
// never observes another test's handles or tombstones, and restores the
// package defaults it perturbs.
func freshRegistry(t *testing.T) *detachedRunnerRegistry {
	t.Helper()
	orig := detachedRunners
	origTTL := detachedCancelTombstoneTTL
	origGrace := detachedTerminateGrace
	origHook := detachedSpawnPreStartHook
	detachedRunners = newDetachedRunnerRegistry()
	t.Cleanup(func() {
		detachedRunners = orig
		detachedCancelTombstoneTTL = origTTL
		detachedTerminateGrace = origGrace
		detachedSpawnPreStartHook = origHook
	})
	return detachedRunners
}

// longLivedRunnerCmd builds (but does NOT start) a detached-style child that
// sleeps well past any test bound, in its own process group so the group
// signal path is the one exercised.
func longLivedRunnerCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 120")
	runStageSetProcessGroup(cmd)
	return cmd
}

// TestDetachedRegistry_CancelBeforeSpawn_RefusesStart asserts a tombstoned run
// id refuses the spawn WITHOUT forking: startAndRegister returns
// errRunCancelledBeforeSpawn and the command was never started (its
// cmd.Process is still nil, i.e. no fork happened).
func TestDetachedRegistry_CancelBeforeSpawn_RefusesStart(t *testing.T) {
	reg := freshRegistry(t)
	const runID = "11111111-1111-1111-1111-111111111111"

	reg.terminateRunners(runID, nil)

	cmd := longLivedRunnerCmd(t)
	h, err := reg.startAndRegister(cmd, runID, "stage-1")
	if !errors.Is(err, errRunCancelledBeforeSpawn) {
		t.Fatalf("err = %v, want errRunCancelledBeforeSpawn", err)
	}
	if h != nil {
		t.Errorf("handle = %v, want nil", h)
	}
	if cmd.Process != nil {
		t.Errorf("a process was FORKED for a cancelled run: pid %d", cmd.Process.Pid)
	}
}

// TestDetachedRegistry_CancelThenRevive_Admits is the anti-wedge assertion
// (binding condition 2): the tombstone must not refuse a re-dispatch of a run
// that has since left the cancelled state. No sleeping and no TTL expiry — the
// live-state probe the cancel recorded reports the run running, so the
// re-dispatch is ADMITTED immediately and the tombstone is retired.
func TestDetachedRegistry_CancelThenRevive_Admits(t *testing.T) {
	reg := freshRegistry(t)
	const runID = "22222222-2222-2222-2222-222222222222"

	var (
		mu    sync.Mutex
		state = runStateCancelled
		calls int
	)
	probe := func(context.Context, string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return state, nil
	}
	reg.terminateRunners(runID, probe)

	// Still cancelled → refused, no fork.
	refused := longLivedRunnerCmd(t)
	if _, err := reg.startAndRegister(refused, runID, "stage-1"); !errors.Is(err, errRunCancelledBeforeSpawn) {
		t.Fatalf("spawn for a still-cancelled run: err = %v, want errRunCancelledBeforeSpawn", err)
	}
	if refused.Process != nil {
		t.Errorf("a process was forked for a still-cancelled run")
	}

	// Revive: the operator ran revive_run / retry_stage.
	mu.Lock()
	state = "running"
	mu.Unlock()

	cmd := longLivedRunnerCmd(t)
	h, err := reg.startAndRegister(cmd, runID, "stage-2")
	if err != nil {
		t.Fatalf("re-dispatch of a cancelled-then-revived run REFUSED: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if h == nil || h.pid <= 0 {
		t.Fatalf("handle = %v, want a registered handle with a pid", h)
	}
	// The tombstone was retired, not merely bypassed.
	reg.mu.Lock()
	_, stillTombstoned := reg.cancelled[runID]
	reg.mu.Unlock()
	if stillTombstoned {
		t.Errorf("tombstone survived a proven-revived run")
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls < 2 {
		t.Errorf("probe called %d times, want it consulted on each tombstoned spawn", gotCalls)
	}
}

// TestDetachedRegistry_TombstoneProbeFailsClosed asserts the probe's own
// failure modes refuse rather than admit: a nil probe, a probe error, and an
// empty state each leave the spawn refused and unforked.
func TestDetachedRegistry_TombstoneProbeFailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		probe runStateProbe
	}{
		{name: "nil probe", probe: nil},
		{
			name:  "probe errors",
			probe: func(context.Context, string) (string, error) { return "", errors.New("backend down") },
		},
		{
			name:  "probe returns an empty state",
			probe: func(context.Context, string) (string, error) { return "", nil },
		},
		{
			name:  "probe still reports cancelled",
			probe: func(context.Context, string) (string, error) { return runStateCancelled, nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := freshRegistry(t)
			const runID = "33333333-3333-3333-3333-333333333333"
			reg.terminateRunners(runID, tc.probe)
			cmd := longLivedRunnerCmd(t)
			if _, err := reg.startAndRegister(cmd, runID, "s"); !errors.Is(err, errRunCancelledBeforeSpawn) {
				t.Fatalf("err = %v, want errRunCancelledBeforeSpawn", err)
			}
			if cmd.Process != nil {
				t.Errorf("a process was forked despite a fail-closed probe")
			}
		})
	}
}

// TestDetachedRegistry_NewerCancelDuringProbe_Refuses asserts the tombstone
// generation guard: the live-state probe runs OUTSIDE the registry lock, so a
// SECOND cancel can land while it is in flight. When it does, the probe's
// "no longer cancelled" answer is stale and the spawn must REFUSE rather than
// admit against the newer cancel.
func TestDetachedRegistry_NewerCancelDuringProbe_Refuses(t *testing.T) {
	reg := freshRegistry(t)
	const runID = "aaaaaaaa-1111-2222-3333-444444444444"

	probe := func(context.Context, string) (string, error) {
		// A newer cancel lands while this probe is in flight.
		reg.terminateRunners(runID, nil)
		// ...and this (now stale) read says the run was revived.
		return "running", nil
	}
	reg.terminateRunners(runID, probe)

	cmd := longLivedRunnerCmd(t)
	if _, err := reg.startAndRegister(cmd, runID, "s"); !errors.Is(err, errRunCancelledBeforeSpawn) {
		t.Fatalf("err = %v, want errRunCancelledBeforeSpawn — a newer cancel must win over a stale probe", err)
	}
	if cmd.Process != nil {
		t.Errorf("a process was forked against a NEWER cancel")
	}
	reg.mu.Lock()
	_, stillTombstoned := reg.cancelled[runID]
	reg.mu.Unlock()
	if !stillTombstoned {
		t.Errorf("the newer cancel's tombstone was retired by a stale probe answer")
	}
}

// TestDetachedRegistry_TombstoneExpires asserts the TTL backstop: past
// detachedCancelTombstoneTTL a spawn for the same run id is admitted even with
// no probe at all.
func TestDetachedRegistry_TombstoneExpires(t *testing.T) {
	reg := freshRegistry(t)
	detachedCancelTombstoneTTL = time.Nanosecond
	const runID = "44444444-4444-4444-4444-444444444444"

	reg.terminateRunners(runID, nil)
	time.Sleep(time.Millisecond)

	cmd := longLivedRunnerCmd(t)
	h, err := reg.startAndRegister(cmd, runID, "s")
	if err != nil {
		t.Fatalf("spawn after the tombstone TTL REFUSED: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if h == nil {
		t.Fatal("handle = nil after an expired tombstone")
	}
}

// TestDetachedRegistry_CancelDuringSpawn_ReapsChild is the registration-race
// assertion. The interleaving is driven by a REAL synchronization signal, not
// a bare sleep: the pre-Start hook fires INSIDE the registry lock, launches the
// canceller, and then waits until the registry's committed-waiter count is
// non-zero — which happens only once that goroutine has entered the
// mutex-acquire path and therefore cannot run past Lock until this hold is
// released. It then holds a scaled beat longer so the parked canceller crosses
// sync.Mutex's starvation threshold and the mutex is handed to it directly
// rather than barged past by the spawner.
//
// Because a mutex handoff is a runtime scheduling property rather than a
// language guarantee, the interleaving is repeated: the child must be reaped on
// EVERY iteration. That is the honest claim — not that one run is provably
// deterministic, but that the property holds across repeated genuinely-contended
// interleavings.
//
// Verified as a counterfactual vehicle: releasing the mutex BEFORE cmd.Start()
// and re-taking it to register (the real register-after-Start defect this
// design exists to prevent) makes every iteration RED — reaped = 0 and the
// child survives. Note that merely unlocking-and-immediately-relocking around
// the registration does NOT go red on this runtime, because the spawner barges
// back into the mutex ahead of the parked canceller; that variant is not
// observably broken by this test and the fork-window variant above is the one
// this test discriminates.
func TestDetachedRegistry_CancelDuringSpawn_ReapsChild(t *testing.T) {
	const iterations = 10
	for i := 0; i < iterations; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			reg := freshRegistry(t)
			detachedTerminateGrace = timescale.D(3 * time.Second)
			runID := "55555555-5555-5555-5555-" + strconv.Itoa(100000000000+i)

			cancelDone := make(chan struct{})
			var reaped int
			detachedSpawnPreStartHook = func(id string) {
				go func() {
					defer close(cancelDone)
					reaped, _ = reg.terminateRunners(id, nil)
				}()
				// Wait until the canceller is COMMITTED to acquiring the registry
				// mutex: waiters is incremented immediately before mu.Lock(), so
				// once it reads non-zero that goroutine is parked and cannot make
				// progress until this (lock-holding) hook returns. Bounded, so a
				// variant that released the lock BEFORE this hook (where the
				// canceller acquires instantly and the count is already back to
				// zero) fails on the assertion below instead of spinning forever.
				deadline := time.Now().Add(timescale.D(2 * time.Second))
				for reg.waiters.Load() == 0 && time.Now().Before(deadline) {
					runtime.Gosched()
				}
			}

			cmd := longLivedRunnerCmd(t)
			h, err := reg.startAndRegister(cmd, runID, "stage-1")
			if err != nil {
				t.Fatalf("startAndRegister: %v", err)
			}
			// Production's reaper closes done after Wait; run the same shape here.
			go func() {
				defer reg.deregister(h)
				_ = cmd.Wait()
			}()

			<-cancelDone
			if reaped != 1 {
				t.Errorf("reaped = %d, want 1 — the cancel that arrived mid-spawn must see the child", reaped)
			}
			if processStillAlive(h.pid) {
				t.Errorf("child pid %d survived a cancel that arrived mid-spawn", h.pid)
			}
			reg.mu.Lock()
			_, stillRegistered := reg.byRun[runID]
			reg.mu.Unlock()
			if stillRegistered {
				t.Errorf("registry entry survived the reap")
			}
		})
	}
}

// TestDetachedRegistry_TerminateWarnsOnSurvivor asserts the warning branch: a
// handle whose child never reports done yields a warning naming its stage id
// and pid, and is NOT counted as reaped.
func TestDetachedRegistry_TerminateWarnsOnSurvivor(t *testing.T) {
	reg := freshRegistry(t)
	detachedTerminateGrace = timescale.D(40 * time.Millisecond)
	const runID = "66666666-6666-6666-6666-666666666666"

	// Swap the signal seam so the child is never actually signalled: it stays
	// alive and never closes done, which is exactly the survivor shape.
	origSignal := runStageSignalGroup
	runStageSignalGroup = func(*exec.Cmd, syscall.Signal) {}
	t.Cleanup(func() { runStageSignalGroup = origSignal })

	cmd := longLivedRunnerCmd(t)
	h, err := reg.startAndRegister(cmd, runID, "stage-warn")
	if err != nil {
		t.Fatalf("startAndRegister: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	reaped, warnings := reg.terminateRunners(runID, nil)
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0 for a survivor", reaped)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	if !strings.Contains(warnings[0], "stage-warn") || !strings.Contains(warnings[0], strconv.Itoa(h.pid)) {
		t.Errorf("warning %q does not name the stage id and pid", warnings[0])
	}
}

// TestDetachedRegistry_DeregisterIsIdempotent asserts the reaper's deferred
// deregister is safe after terminateRunners already removed the handle (the
// normal ordering), and does not panic on a double close or a nil handle.
func TestDetachedRegistry_DeregisterIsIdempotent(t *testing.T) {
	reg := freshRegistry(t)
	const runID = "77777777-7777-7777-7777-777777777777"
	cmd := exec.Command("sh", "-c", "exit 0")
	runStageSetProcessGroup(cmd)
	h, err := reg.startAndRegister(cmd, runID, "s")
	if err != nil {
		t.Fatalf("startAndRegister: %v", err)
	}
	_ = cmd.Wait()
	reg.deregister(h)
	reg.deregister(h)
	reg.deregister(nil)
	reg.mu.Lock()
	_, present := reg.byRun[runID]
	reg.mu.Unlock()
	if present {
		t.Errorf("registry entry survived deregister")
	}
}

// TestDetachedRegistry_ConcurrentRunsAreIndependent asserts terminateRunners
// reaps only the target run's children and leaves a sibling run's alone.
func TestDetachedRegistry_ConcurrentRunsAreIndependent(t *testing.T) {
	reg := freshRegistry(t)
	detachedTerminateGrace = timescale.D(3 * time.Second)
	const (
		runA = "88888888-8888-8888-8888-888888888888"
		runB = "99999999-9999-9999-9999-999999999999"
	)
	start := func(runID string) *detachedRunnerHandle {
		cmd := longLivedRunnerCmd(t)
		h, err := reg.startAndRegister(cmd, runID, "s-"+runID[:2])
		if err != nil {
			t.Fatalf("startAndRegister %s: %v", runID, err)
		}
		go func() {
			defer reg.deregister(h)
			_ = cmd.Wait()
		}()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return h
	}
	ha := start(runA)
	hb := start(runB)

	if reaped, _ := reg.terminateRunners(runA, nil); reaped != 1 {
		t.Errorf("reaped = %d, want 1", reaped)
	}
	if processStillAlive(ha.pid) {
		t.Errorf("run A's child pid %d survived its cancel", ha.pid)
	}
	if !processStillAlive(hb.pid) {
		t.Errorf("run B's child pid %d was reaped by run A's cancel", hb.pid)
	}
}

// processStillAlive is the signal-0 liveness probe, polled briefly so a child
// that is mid-exit is not misread as alive.
func processStillAlive(pid int) bool {
	deadline := time.Now().Add(timescale.D(2 * time.Second))
	for {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return false
		}
		if sigErr := proc.Signal(syscall.Signal(0)); sigErr != nil {
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
}
