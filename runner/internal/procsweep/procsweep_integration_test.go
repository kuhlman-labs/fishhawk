//go:build !windows

package procsweep

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The DONE-MEANS integration test (#3663) drives REAL processes through the real
// `ps` reader and the real SIGKILL path.
//
// Shape: a re-exec helper spawns a grandchild in a NEW SESSION and then EXITS,
// so the grandchild is re-parented to pid 1 and its session id differs from the
// test process's — i.e. it has escaped both the ppid chain and the test's process
// group, which is exactly the class #3663 leaked. The only thing left linking it
// to the "stage" is the process-group id it inherited from the helper.
//
// Setsid is requested ALONE, never together with Setpgid: setsid(2) makes the
// caller a session leader AND a new process-group leader, so a simultaneous
// request to place it in a specified process group is rejected with EPERM.

const (
	helperEnvSpawn = "FISHHAWK_PROCSWEEP_TEST_SPAWN"
	helperEnvChild = "FISHHAWK_PROCSWEEP_TEST_CHILD"
)

// TestMain dispatches the two helper roles before running the suite.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnvChild) == "1" {
		// The escaping grandchild: sleep long enough that only a kill ends it.
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	if os.Getenv(helperEnvSpawn) == "1" {
		os.Exit(runSpawnHelper())
	}
	os.Exit(m.Run())
}

// runSpawnHelper spawns the grandchild with Setsid, prints its pid on stdout,
// then waits for a line on stdin before exiting — so the test can sample the
// process table while the helper (and therefore the ppid chain) is still alive.
func runSpawnHelper() int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: executable:", err)
		return 1
	}
	cmd := exec.Command(self, "-test.run=TestProcsweepHelperNoop")
	cmd.Env = append(os.Environ(), helperEnvChild+"=1")
	cmd.Env = removeEnv(cmd.Env, helperEnvSpawn)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "helper: start grandchild:", err)
		return 1
	}
	fmt.Println(cmd.Process.Pid)
	// Block until the test releases us. A read error (pipe closed) releases too.
	buf := make([]byte, 1)
	for {
		n, rerr := os.Stdin.Read(buf)
		if n > 0 || rerr != nil {
			break
		}
	}
	return 0
}

func removeEnv(env []string, key string) []string {
	out := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// TestProcsweepHelperNoop exists only as a -test.run target for the grandchild
// re-exec; the grandchild never reaches it, because TestMain intercepts it first.
func TestProcsweepHelperNoop(t *testing.T) {}

func TestSweep_ReapsOrphanedSetsidDescendant(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not available on this host")
	}

	// The stage-start floor is taken BEFORE the helper spawns, so the grandchild
	// passes it by construction.
	floor := time.Now()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	helper := exec.Command(self, "-test.run=TestProcsweepHelperNoop")
	helper.Env = append(os.Environ(), helperEnvSpawn+"=1")
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	var grandchild int
	readDone := make(chan error, 1)
	go func() {
		var pid int
		_, serr := fmt.Fscanf(stdout, "%d\n", &pid)
		grandchild = pid
		readDone <- serr
	}()
	select {
	case serr := <-readDone:
		if serr != nil {
			t.Fatalf("read grandchild pid: %v", serr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not report a grandchild pid within 30s")
	}
	if grandchild <= 1 {
		t.Fatalf("grandchild pid = %d", grandchild)
	}
	// Whatever happens below, never leak the grandchild.
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	// Sample SYNCHRONOUSLY while the helper is still alive, so the recorder sees
	// the helper as a descendant and records its process group. No timer, no race.
	r := NewRecorder(os.Getpid(), floor)
	if err := r.Sample(context.Background()); err != nil {
		t.Fatalf("Sample: %v", err)
	}

	// Release the helper and reap it, orphaning the grandchild.
	_, _ = stdin.Write([]byte("\n"))
	_ = stdin.Close()
	if err := helper.Wait(); err != nil {
		t.Fatalf("helper exited with error: %v", err)
	}

	// The grandchild must now be ALIVE, re-parented to pid 1, and in a DIFFERENT
	// session from the test process — the #3663 escape.
	waitFor(t, 30*time.Second, "grandchild re-parented to pid 1", func() bool {
		return processPPID(t, grandchild) == 1
	})
	if err := syscall.Kill(grandchild, 0); err != nil {
		t.Fatalf("grandchild %d is not alive before the sweep: %v", grandchild, err)
	}
	childSID, err := unix.Getsid(grandchild)
	if err != nil {
		t.Fatalf("Getsid(grandchild): %v", err)
	}
	selfSID, err := unix.Getsid(os.Getpid())
	if err != nil {
		t.Fatalf("Getsid(self): %v", err)
	}
	if childSID == selfSID {
		t.Fatalf("grandchild session id %d equals the test process's — Setsid did not take effect, so this fixture does not exercise the escape", childSID)
	}

	// SWEEP, with the test process's REAL process group — never a posed
	// selfPGID == selfPID. Under `go test` the test binary does NOT lead its own
	// group (it shares `go`'s, which shares the invoking shell's), so posing
	// leadership would DISARM the group-leader guard and authorise kills against
	// every live member of that shared group whose start is after the floor:
	// `go test` itself, and the shell that launched it. Observed for real while
	// writing this test — the suite killed its own `go test` parent.
	//
	// killPID is additionally fenced to the grandchild for the duration: the real
	// SIGKILL still goes to the real orphan (which is what the ESRCH poll below
	// proves), while any OTHER pid the selection admits is recorded and NOT
	// signalled, so a selection regression fails this test loudly instead of
	// reaping the harness.
	realSelfPGID := SelfPGID(os.Getpid())
	restoreKill := killPID
	var forbidden []int
	killPID = func(pid int) error {
		if pid == grandchild {
			return restoreKill(pid)
		}
		forbidden = append(forbidden, pid)
		return nil
	}
	res, err := r.Sweep(context.Background(), os.Getpid(), realSelfPGID)
	killPID = restoreKill
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(forbidden) != 0 {
		t.Fatalf("Sweep selected pids outside the orphaned grandchild: %v — selection must not reach the test harness", forbidden)
	}
	reaped := false
	for _, p := range res.Reaped {
		if p.PID == grandchild {
			reaped = true
		}
	}
	if !reaped {
		t.Fatalf("Sweep did not report reaping the orphaned grandchild %d; reaped=%v already_gone=%d errors=%v",
			grandchild, pids(res.Reaped), res.AlreadyGone, res.Errors)
	}

	waitFor(t, 30*time.Second, fmt.Sprintf("orphaned grandchild %d to be gone", grandchild), func() bool {
		return syscall.Kill(grandchild, 0) == syscall.ESRCH
	})
}

func waitFor(t *testing.T, budget time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", budget, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func processPPID(t *testing.T, pid int) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1
	}
	return v
}
