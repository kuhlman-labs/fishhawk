//go:build unix

package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestProcgroupHelper is the Go stdlib test-helper-process pattern: when
// PROCGROUP_HELPER=1 is set, this test re-exec pretends to be the wedged review
// CLI. PROCGROUP_ROLE selects between the top-level "parent" (the process
// Harden'd by the test) and the "grandchild" it forks to hold the inherited
// stdout pipe open. PROCGROUP_ESCAPE=1 makes the grandchild call setpgid itself
// so it escapes the parent's process group (the WaitDelay path); the default
// keeps it in the group (the group-kill path).
func TestProcgroupHelper(t *testing.T) {
	if os.Getenv("PROCGROUP_HELPER") != "1" {
		return
	}
	defer os.Exit(0)

	switch os.Getenv("PROCGROUP_ROLE") {
	case "parent":
		escape := os.Getenv("PROCGROUP_ESCAPE") == "1"
		spawnGrandchild(escape)
		if escape {
			// Exit immediately, leaving the escaped grandchild holding the
			// stdout pipe. Only WaitDelay can now unblock the parent's
			// cmd.Output(): the group kill has nothing left in the group to
			// reap.
			return
		}
		// Stay alive past any short test deadline so the deadline — not a
		// natural exit — is what triggers the group kill that reaps us AND the
		// in-group grandchild.
		time.Sleep(30 * time.Second)
	case "grandchild":
		// Inherit stdout (set by the parent) and hold it open past the
		// deadline. Record our pid first so the test can assert we were reaped
		// (group mode) or is able to clean us up (escape mode).
		if pf := os.Getenv("PROCGROUP_GC_PIDFILE"); pf != "" {
			_ = os.WriteFile(pf, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		time.Sleep(30 * time.Second)
	}
}

// spawnGrandchild re-execs the test binary as a stdout-inheriting grandchild.
// escape=true makes it its own process-group leader so kill(-parentpgid) misses
// it; escape=false leaves it in the parent's group so the group kill reaps it.
func spawnGrandchild(escape bool) {
	gc := exec.Command(os.Args[0], "-test.run=TestProcgroupHelper") //nolint:gosec // re-exec of the test binary itself
	env := append(os.Environ(), "PROCGROUP_HELPER=1", "PROCGROUP_ROLE=grandchild")
	if pf := os.Getenv("PROCGROUP_GC_PIDFILE"); pf != "" {
		env = append(env, "PROCGROUP_GC_PIDFILE="+pf)
	}
	gc.Env = env
	gc.Stdout = os.Stdout // inherit the pipe write-end so it stays open after the parent dies
	if escape {
		gc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	_ = gc.Start()
}

// parentHelperCmd builds a Harden-able cmd that re-execs the test binary as the
// PROCGROUP_ROLE=parent helper.
func parentHelperCmd(ctx context.Context, escape bool, pidfile string) *exec.Cmd {
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
	)
	return cmd
}

// readPidWhenReady polls pidfile until the grandchild has written its pid, or
// fails the test after a bounded wait.
func readPidWhenReady(t *testing.T, pidfile string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidfile)
		if err == nil {
			if pid, perr := strconv.Atoi(string(b)); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild never wrote its pid to %s", pidfile)
	return 0
}

// pidAlive reports whether pid is still a live process (kill(pid, 0) succeeds).
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestHarden_GroupKillReapsGrandchild is the group-kill case: a parent holds an
// in-group grandchild that inherits stdout and outlives a naive direct-child
// kill. With Harden, the context DEADLINE fires the whole-group SIGKILL, which
// reaps the grandchild — closing the pipe so cmd.Output() returns AT the
// deadline, well before the (deliberately long) WaitDelay grace. Without the fix
// the default single-child kill would leave the grandchild holding the pipe and
// cmd.Output() would hang.
func TestHarden_GroupKillReapsGrandchild(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	cmd := parentHelperCmd(ctx, false /* in-group */, pidfile)
	// A long grace proves the group kill (not WaitDelay) is what unblocks
	// Output: if Output returns quickly, the pipe closed via the reap.
	Harden(cmd, 10*time.Second)

	start := time.Now()
	_, err := cmd.Output()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from the deadline-killed helper, got nil")
	}
	// Assert the trigger was a genuine context DEADLINE, not a bare cancel.
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want context.DeadlineExceeded (the deadline must be the trigger)", ctx.Err())
	}
	if elapsed > 3*time.Second {
		t.Errorf("Output took %s — the group kill should close the pipe at the deadline, not wait the 10s grace", elapsed)
	}

	// The in-group grandchild must have been reaped by the group SIGKILL.
	gcPid := readPidWhenReady(t, pidfile)
	gone := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
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

// TestHarden_WaitDelayForceClosesEscapedPipe is the WaitDelay case: the
// grandchild calls setpgid itself and escapes the parent's group, and the parent
// exits immediately — so the group kill has nothing to reap and only WaitDelay
// can end the hang. With Harden, cmd.Output() force-closes the parent-side pipe
// fd after grace and returns near deadline+grace; without WaitDelay it would
// block until the escaped grandchild's own 30s sleep ended.
func TestHarden_WaitDelayForceClosesEscapedPipe(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "gc.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	cmd := parentHelperCmd(ctx, true /* escape */, pidfile)
	const grace = 300 * time.Millisecond
	Harden(cmd, grace)

	// Clean up the escaped grandchild, which the group kill cannot reach.
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidfile); err == nil {
			if pid, perr := strconv.Atoi(string(b)); perr == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	start := time.Now()
	_, err := cmd.Output()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from the WaitDelay-forced return, got nil")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want context.DeadlineExceeded (the deadline must be the trigger)", ctx.Err())
	}
	// Returned via WaitDelay: after the deadline + grace, but not after the
	// escaped grandchild's 30s sleep (which is what a missing WaitDelay would
	// force us to wait for).
	if elapsed > 5*time.Second {
		t.Errorf("Output took %s — WaitDelay should force-close the escaped pipe near deadline+grace, not hang on the grandchild", elapsed)
	}

	// The escaped grandchild really did survive the group kill (proving this is
	// the WaitDelay path, not an accidental reap).
	gcPid := readPidWhenReady(t, pidfile)
	if !pidAlive(gcPid) {
		t.Errorf("grandchild pid %d was reaped — it should have escaped the group and been force-closed via WaitDelay", gcPid)
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
