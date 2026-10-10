//go:build unix

package procgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// TestProcgroupHelper is the Go stdlib test-helper-process pattern: when
// PROCGROUP_HELPER=1 is set, this test re-exec pretends to be the wedged review
// CLI. PROCGROUP_ROLE selects between the top-level "parent" (the process
// Harden'd by the test) and the "grandchild" it forks to hold the inherited
// stdout pipe open. PROCGROUP_ESCAPE=1 makes the grandchild call setpgid itself
// so it escapes the parent's process group (the WaitDelay path); the default
// keeps it in the group (the group-kill path). PROCGROUP_SPAWN_DELAY (the #4177
// margin pin) delays the parent's fork to model a slow spawn on a loaded host.
func TestProcgroupHelper(t *testing.T) {
	if os.Getenv("PROCGROUP_HELPER") != "1" {
		return
	}
	defer os.Exit(0)

	switch os.Getenv("PROCGROUP_ROLE") {
	case "parent":
		escape := os.Getenv("PROCGROUP_ESCAPE") == "1"
		if d, err := time.ParseDuration(os.Getenv("PROCGROUP_SPAWN_DELAY")); err == nil {
			time.Sleep(d)
		}
		spawnGrandchild(escape)
		if escape {
			// Stamp the exit instant for the escape case's arm-to-exit
			// diagnostic, then exit immediately, leaving the escaped grandchild
			// holding the stdout pipe. Only WaitDelay can now unblock the
			// parent's cmd.Output(): the group kill has nothing left in the
			// group to reap.
			if pf := os.Getenv("PROCGROUP_GC_PIDFILE"); pf != "" {
				_ = os.WriteFile(exitStampPath(pf), []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600)
			}
			return
		}
		// Stay alive past any short test deadline so the deadline — not a
		// natural exit — is what triggers the group kill that reaps us AND the
		// in-group grandchild. Scaled by the shared factor so the wedge stays
		// well above the (also-scaled) elapsed bound at any factor.
		time.Sleep(timescale.D(30 * time.Second))
	case "grandchild":
		// Inherit stdout (set by the parent) and hold it open past the
		// deadline. Our pid is recorded by the parent right after
		// exec.Cmd.Start (#4177), not here, so the spawn precondition excludes
		// this process's runtime init.
		time.Sleep(timescale.D(30 * time.Second))
	}
}

// spawnGrandchild re-execs the test binary as a stdout-inheriting grandchild.
// escape=true makes it its own process-group leader so kill(-parentpgid) misses
// it; escape=false leaves it in the parent's group so the group kill reaps it.
// Right after Start it records the grandchild pid in PROCGROUP_GC_PIDFILE
// (atomic tmp+rename, #4177): Start returns only once the child has exec'd, so
// a recorded pid proves the grandchild exists, holds the inherited stdout and
// (escape) has already left the group — the spawn precondition the driving
// test's SpawnGate arms on.
func spawnGrandchild(escape bool) {
	gc := exec.Command(os.Args[0], "-test.run=TestProcgroupHelper") //nolint:gosec // re-exec of the test binary itself
	gc.Env = append(os.Environ(), "PROCGROUP_HELPER=1", "PROCGROUP_ROLE=grandchild")
	gc.Stdout = os.Stdout // inherit the pipe write-end so it stays open after the parent dies
	if escape {
		gc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if gc.Start() != nil {
		return // no pid recorded: the driving test fails as a SpawnGate PRECONDITION
	}
	if pf := os.Getenv("PROCGROUP_GC_PIDFILE"); pf != "" {
		_ = timescale.WritePidFile(pf, gc.Process.Pid)
	}
}

// parentHelperCmd builds a Harden-able cmd that re-execs the test binary as the
// PROCGROUP_ROLE=parent helper, sleeping spawnDelay before it forks.
func parentHelperCmd(ctx context.Context, escape bool, pidfile string, spawnDelay time.Duration) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestProcgroupHelper")
	esc := "0"
	if escape {
		esc = "1"
	}
	cmd.Env = append(os.Environ(),
		"PROCGROUP_HELPER=1",
		"PROCGROUP_ROLE=parent",
		"PROCGROUP_ESCAPE="+esc,
		"PROCGROUP_GC_PIDFILE="+pidfile,
		"PROCGROUP_SPAWN_DELAY="+spawnDelay.String(),
	)
	return cmd
}

// The #4177 deadline roles (the #3587 separation). The CHEAP deadline is the
// one whose expiry IS the verdict under test. The spawn budget is the
// must-complete window for the parent to fork its grandchild, 50x the cheap
// deadline; it costs nothing on the happy path because the timescale.SpawnGate
// arms the cheap deadline the moment the grandchild pid is recorded, and
// exhausting it fails the test as a named PRECONDITION, never as the group
// kill. Functions, not consts, so the factor is read at call time.
func hardenDeadline() time.Duration { return timescale.D(200 * time.Millisecond) }
func spawnBudget() time.Duration    { return timescale.D(10 * time.Second) }

// escapeGrace is the escape case's WaitDelay grace. os/exec starts the
// WaitDelay timer when Wait observes the parent's exit, and the escape parent
// exits right after it records the grandchild pid, i.e. near the gate's arm
// instant — so the gated deadline must still fire before exit+grace for the
// trigger to be the deadline. The margin: arm-to-exit latency (armedAt minus
// the parent's exit) must stay below escapeGrace() - hardenDeadline() (1.8s at
// factor 1). Do NOT shrink this back toward the deadline (#4177).
func escapeGrace() time.Duration { return 10 * hardenDeadline() }

// exitStampPath is where the escape parent records its exit instant.
func exitStampPath(pidfile string) string { return pidfile + ".exit" }

// armToExitDiag renders armedAt minus the escape parent's recorded exit
// instant against the escape margin, reported by a failing escape case.
func armToExitDiag(pidfile string, armedAt time.Time) string {
	b, err := os.ReadFile(exitStampPath(pidfile))
	if err != nil {
		return fmt.Sprintf("armedAt - child exit unknown (no exit stamp: %v)", err)
	}
	ns, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Sprintf("armedAt - child exit unknown (bad exit stamp %q)", b)
	}
	return fmt.Sprintf("armedAt - child exit = %s; the escape case needs it below escapeGrace() - hardenDeadline() = %s (#4177)",
		armedAt.Sub(time.Unix(0, ns)), escapeGrace()-hardenDeadline())
}

// recordedPid returns the grandchild pid the parent recorded, failing the test
// when it is absent (the caller has already passed gate.RequireArmed, so a
// missing pid here is a fixture defect).
func recordedPid(t *testing.T, pidfile string) int {
	t.Helper()
	pid, ok := timescale.ReadPidFile(pidfile)
	if !ok {
		t.Fatalf("grandchild pid not recorded in %s", pidfile)
	}
	return pid
}

// killPidFromFile best-effort SIGKILLs the pid recorded in pidfile — cleanup for
// an escaped grandchild the group kill cannot reap.
func killPidFromFile(pidfile string) {
	if pid, ok := timescale.ReadPidFile(pidfile); ok {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// pidAlive reports whether pid is still a live process (kill(pid, 0) succeeds).
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// runGroupKillCase is the group-kill case: a parent holds an in-group
// grandchild that inherits stdout and outlives a naive direct-child kill. With
// Harden, the context DEADLINE fires the whole-group SIGKILL, which reaps the
// grandchild — closing the pipe so cmd.Output() returns AT the deadline, well
// before the (deliberately long) WaitDelay grace. Without the fix the default
// single-child kill would leave the grandchild holding the pipe and
// cmd.Output() would hang. The deadline is a timescale.SpawnGate (#4177): it
// arms only once the grandchild pid is recorded, so it cannot fire the group
// kill before the grandchild exists, and the elapsed bound is measured from
// that ARM instant so a slow spawn cannot eat it.
func runGroupKillCase(t *testing.T, spawnDelay time.Duration) {
	t.Helper()
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	// Every deadline-competing duration derives from timescale.D (base value ×
	// the shared factor), so the discrimination ratios (bound/deadline,
	// long-grace/bound) hold at any factor.
	gate := timescale.NewSpawnGate(spawnBudget(), hardenDeadline(), timescale.PidFileReady(pidfile))
	t.Cleanup(gate.Stop)

	cmd := parentHelperCmd(gate, false /* in-group */, pidfile, spawnDelay)
	// A long grace proves the group kill (not WaitDelay) is what unblocks
	// Output: if Output returns quickly, the pipe closed via the reap.
	Harden(cmd, timescale.D(10*time.Second))

	_, err := cmd.Output()
	returned := time.Now()
	// PRECONDITION first: a starved host fails here, named as host load, never
	// below as a defect in the group kill.
	armedAt := gate.RequireArmed(t)
	elapsed := returned.Sub(armedAt)

	if err == nil {
		t.Fatal("expected an error from the deadline-killed helper, got nil")
	}
	// Assert the trigger was a genuine context DEADLINE, not a bare cancel.
	if !errors.Is(gate.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want context.DeadlineExceeded (the deadline must be the trigger)", gate.Err())
	}
	if elapsed > timescale.D(3*time.Second) {
		t.Errorf("Output returned %s after the deadline armed — the group kill should close the pipe at the deadline, not wait the 10s grace", elapsed)
	}

	// The in-group grandchild must have been reaped by the group SIGKILL. This
	// is a reap-liveness wait (kernel signal delivery), NOT the kill-latency
	// asserted above: raised to the shared 30s liveness base and scaled to match
	// the claudecode/codex copies, so a loaded runner slow to reap does not fatal
	// spuriously.
	gcPid := recordedPid(t, pidfile)
	gone := false
	for deadline := time.Now().Add(timescale.D(30 * time.Second)); time.Now().Before(deadline); {
		if !pidAlive(gcPid) {
			gone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gone {
		_ = syscall.Kill(gcPid, syscall.SIGKILL) // best-effort cleanup on failure
		t.Errorf("grandchild pid %d still alive — the group kill did not reap it", gcPid)
	}
}

// runEscapeCase is the WaitDelay case: the grandchild calls setpgid itself and
// escapes the parent's group, and the parent exits immediately — so the group
// kill has nothing to reap and only WaitDelay can end the hang. With Harden,
// cmd.Output() force-closes the parent-side pipe fd after grace and returns
// near exit+grace; without WaitDelay it would block until the escaped
// grandchild's own 30s sleep ended.
//
// The deadline is a timescale.SpawnGate (#4177), so it arms near the parent's
// exit, which is also when os/exec starts the WaitDelay timer. The margin: the
// arm-to-exit latency must stay below escapeGrace() - hardenDeadline(), or
// WaitDelay fires before the deadline and the trigger is no longer the
// deadline. A failing run reports armedAt minus the parent's exit beside the
// error.
func runEscapeCase(t *testing.T, spawnDelay time.Duration) {
	t.Helper()
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	// Clean up the escaped grandchild, which the group kill cannot reach.
	t.Cleanup(func() { killPidFromFile(pidfile) })
	// Deadline and grace both derive from timescale.D so their ratio (and the
	// wedge/bound ratio) is preserved at any factor.
	gate := timescale.NewSpawnGate(spawnBudget(), hardenDeadline(), timescale.PidFileReady(pidfile))
	t.Cleanup(gate.Stop)

	cmd := parentHelperCmd(gate, true /* escape */, pidfile, spawnDelay)
	Harden(cmd, escapeGrace())

	_, err := cmd.Output()
	returned := time.Now()
	armedAt := gate.RequireArmed(t) // PRECONDITION first (see runGroupKillCase)
	elapsed := returned.Sub(armedAt)
	defer func() {
		if t.Failed() {
			t.Logf("escape margin: %s", armToExitDiag(pidfile, armedAt))
		}
	}()

	if err == nil {
		t.Fatal("expected an error from the WaitDelay-forced return, got nil")
	}
	if !errors.Is(gate.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want context.DeadlineExceeded (the deadline must be the trigger)", gate.Err())
	}
	// Returned via WaitDelay: after exit + grace, but not after the escaped
	// grandchild's 30s sleep (which is what a missing WaitDelay would force us
	// to wait for).
	if elapsed > timescale.D(5*time.Second) {
		t.Errorf("Output returned %s after the deadline armed — WaitDelay should force-close the escaped pipe near exit+grace, not hang on the grandchild", elapsed)
	}

	// The escaped grandchild really did survive the group kill (proving this is
	// the WaitDelay path, not an accidental reap).
	gcPid := recordedPid(t, pidfile)
	if !pidAlive(gcPid) {
		t.Errorf("grandchild pid %d was reaped — it should have escaped the group and been force-closed via WaitDelay", gcPid)
	}
}

// TestHarden_GroupKillReapsGrandchild is the group-kill case (runGroupKillCase)
// with a fast spawn.
func TestHarden_GroupKillReapsGrandchild(t *testing.T) { runGroupKillCase(t, 0) }

// TestHarden_WaitDelayForceClosesEscapedPipe is the WaitDelay case
// (runEscapeCase) with a fast spawn.
func TestHarden_WaitDelayForceClosesEscapedPipe(t *testing.T) { runEscapeCase(t, 0) }

// TestHarden_ToleratesSlowSpawn is the #4177 margin pin: the same group and
// escape cases with the parent sleeping D(1s) before it forks — between the old
// ungated cheap deadline and the spawn budget — must pass under the SpawnGate.
// Its NEGATIVE old-shape arm runs the same slow fixture under a plain
// context.WithTimeout(cheap deadline) started at call time and asserts the
// grandchild pid is never recorded: that deadline group-kills the parent during
// its pre-spawn sleep, and Output returns only after that parent is reaped, so
// it can no longer fork.
func TestHarden_ToleratesSlowSpawn(t *testing.T) {
	cases := []struct {
		name   string
		escape bool
		grace  func() time.Duration
		run    func(*testing.T, time.Duration)
	}{
		{"group", false, func() time.Duration { return timescale.D(10 * time.Second) }, runGroupKillCase},
		{"escape", true, escapeGrace, runEscapeCase},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slow := timescale.D(1 * time.Second)
			if slow <= hardenDeadline() {
				t.Fatalf("pin precondition: the slow spawn %s must exceed the cheap %s deadline, or the pin cannot tell the gate from the old raw deadline", slow, hardenDeadline())
			}
			if slow >= spawnBudget() {
				t.Fatalf("pin precondition: the slow spawn %s must stay below the %s spawn budget, or the gated case fails as a PRECONDITION", slow, spawnBudget())
			}

			tc.run(t, slow)

			// NEGATIVE old-shape arm.
			pidfile := filepath.Join(t.TempDir(), "old.pid")
			t.Cleanup(func() { killPidFromFile(pidfile) })
			ctx, cancel := context.WithTimeout(context.Background(), hardenDeadline())
			defer cancel()
			cmd := parentHelperCmd(ctx, tc.escape, pidfile, slow)
			Harden(cmd, tc.grace())
			if _, err := cmd.Output(); err == nil {
				t.Fatal("old-shape arm: expected the ungated deadline to kill the slow parent, got nil")
			}
			if pid, ok := timescale.ReadPidFile(pidfile); ok {
				t.Fatalf("old-shape arm: grandchild pid %d was recorded under the ungated %s deadline — the slow spawn no longer outlasts it, so this pin does not discriminate the gate from the old shape", pid, hardenDeadline())
			}
		})
	}
}

// TestKillGroup_NilProcessNoOp asserts the defensive nil-Process branch of
// killGroup: calling the Harden'd cmd.Cancel before the process is ever started
// returns nil instead of dereferencing a nil Process.
func TestKillGroup_NilProcessNoOp(t *testing.T) {
	cmd := exec.Command("true")
	Harden(cmd, time.Second)
	if err := cmd.Cancel(); err != nil {
		t.Errorf("Cancel on an unstarted cmd = %v, want nil (nil-Process no-op)", err)
	}
}
