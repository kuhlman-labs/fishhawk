package timescale

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every timing assertion below is a negative HOLD (Done stays open for a
// window) or a LOWER bound; none is a raw upper bound on a race. The only
// upper bounds are generous liveness waits that fail the test instead of
// hanging it.

// recordingFataler is a Fataler that records Fatalf calls instead of stopping
// the goroutine, so RequireArmed's precondition branch can be asserted.
type recordingFataler struct {
	calls []string
}

func (r *recordingFataler) Helper() {}

func (r *recordingFataler) Fatalf(format string, args ...any) {
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

// waitDone waits for ctx to finish, failing the test after a generous
// liveness bound rather than hanging it.
func waitDone(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	select {
	case <-ctx.Done():
		return time.Now()
	case <-time.After(D(10 * time.Second)):
		t.Fatal("gate never finished within the liveness bound")
		return time.Time{}
	}
}

// TestSpawnGate_ArmsOnlyAfterReady pins the core control: the cheap deadline
// does not start until ready() reports true. Done must stay open for a hold of
// 4x the deadline while ready is false, then close a full deadline after the
// arm instant with a genuine context.DeadlineExceeded.
func TestSpawnGate_ArmsOnlyAfterReady(t *testing.T) {
	const deadline = 50 * time.Millisecond
	var ready atomic.Bool
	g := NewSpawnGate(D(10*time.Second), deadline, ready.Load)
	t.Cleanup(g.Stop)

	select {
	case <-g.Done():
		t.Fatalf("Done closed during the not-ready hold (Err=%v) — the deadline must not start before the spawn precondition holds", g.Err())
	case <-time.After(4 * deadline):
	}
	if _, ok := g.ArmedAt(); ok {
		t.Fatal("ArmedAt reports armed while ready() was still false")
	}

	flip := time.Now()
	ready.Store(true)
	closed := waitDone(t, g)

	if !errors.Is(g.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err() = %v, want context.DeadlineExceeded", g.Err())
	}
	armedAt, ok := g.ArmedAt()
	if !ok {
		t.Fatal("ArmedAt reports never armed after the gate's deadline fired")
	}
	if armedAt.Before(flip) {
		t.Errorf("armedAt %v precedes the ready flip %v", armedAt, flip)
	}
	if got := closed.Sub(armedAt); got < deadline {
		t.Errorf("Done closed %s after arm, want >= the %s deadline (the deadline is measured from the arm instant)", got, deadline)
	}
}

// TestSpawnGate_SpawnBudgetExhausted pins the precondition branch: a ready()
// that never holds ends the gate with DeadlineExceeded once the spawn budget
// is spent (so the code under test unblocks), the gate never arms, and
// RequireArmed reports a named PRECONDITION exactly once.
func TestSpawnGate_SpawnBudgetExhausted(t *testing.T) {
	const budget = 50 * time.Millisecond
	// A short deadline too, so a budget branch that wrongly ARMED would finish
	// promptly and fail on the ArmedAt assertion, not on the liveness bound.
	g := NewSpawnGate(budget, 20*time.Millisecond, func() bool { return false })
	t.Cleanup(g.Stop)

	waitDone(t, g)
	if !errors.Is(g.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err() = %v, want context.DeadlineExceeded after the spawn budget", g.Err())
	}
	if _, ok := g.ArmedAt(); ok {
		t.Fatal("ArmedAt reports armed although ready() never held")
	}

	var f recordingFataler
	if got := g.RequireArmed(&f); !got.IsZero() {
		t.Errorf("RequireArmed returned %v on a never-armed gate, want the zero time", got)
	}
	if len(f.calls) != 1 {
		t.Fatalf("Fatalf called %d times, want exactly 1", len(f.calls))
	}
	msg := f.calls[0]
	for _, want := range []string{"PRECONDITION", "host load", budget.String(), "spawn budget was exhausted", "NOT a defect in the control under test"} {
		if !strings.Contains(msg, want) {
			t.Errorf("precondition message %q missing %q", msg, want)
		}
	}
}

// TestSpawnGate_RequireArmedReturnsArmInstant asserts the armed branch:
// RequireArmed returns ArmedAt's instant and never calls Fatalf.
func TestSpawnGate_RequireArmedReturnsArmInstant(t *testing.T) {
	g := NewSpawnGate(D(10*time.Second), 10*time.Millisecond, func() bool { return true })
	t.Cleanup(g.Stop)
	waitDone(t, g)

	var f recordingFataler
	got := g.RequireArmed(&f)
	if len(f.calls) != 0 {
		t.Fatalf("Fatalf called on an armed gate: %v", f.calls)
	}
	want, ok := g.ArmedAt()
	if !ok || !got.Equal(want) || got.IsZero() {
		t.Errorf("RequireArmed = %v, want the arm instant %v (ok=%v)", got, want, ok)
	}
}

// TestSpawnGate_StopBeforeArm asserts Stop ends a not-yet-armed gate with
// context.Canceled synchronously, is idempotent, and that RequireArmed then
// names the call-returned-early cause (not budget exhaustion).
func TestSpawnGate_StopBeforeArm(t *testing.T) {
	g := NewSpawnGate(D(10*time.Second), time.Hour, func() bool { return false })
	g.Stop()

	select {
	case <-g.Done():
	default:
		t.Fatal("Done still open after Stop returned")
	}
	if !errors.Is(g.Err(), context.Canceled) {
		t.Fatalf("Err() = %v, want context.Canceled", g.Err())
	}
	if _, ok := g.ArmedAt(); ok {
		t.Error("ArmedAt reports armed after a pre-arm Stop")
	}
	g.Stop() // idempotent: no panic on a second close, no hang
	if !errors.Is(g.Err(), context.Canceled) {
		t.Errorf("Err() after a second Stop = %v, want context.Canceled", g.Err())
	}

	var f recordingFataler
	g.RequireArmed(&f)
	if len(f.calls) != 1 || !strings.Contains(f.calls[0], "returned before the spawn precondition held") {
		t.Errorf("RequireArmed calls = %v, want one call naming the early return", f.calls)
	}
}

// TestSpawnGate_StopAfterArm asserts Stop on an armed gate whose deadline has
// not fired yields context.Canceled (not DeadlineExceeded) and is idempotent.
func TestSpawnGate_StopAfterArm(t *testing.T) {
	g := NewSpawnGate(D(10*time.Second), time.Hour, func() bool { return true })
	for end := time.Now().Add(D(10 * time.Second)); ; {
		if _, ok := g.ArmedAt(); ok {
			break
		}
		if time.Now().After(end) {
			t.Fatal("gate never armed with ready() always true")
		}
		time.Sleep(spawnPollInterval)
	}
	select {
	case <-g.Done():
		t.Fatalf("Done closed before Stop on a one-hour deadline (Err=%v)", g.Err())
	default:
	}

	g.Stop()
	g.Stop()
	select {
	case <-g.Done():
	default:
		t.Fatal("Done still open after Stop returned")
	}
	if !errors.Is(g.Err(), context.Canceled) {
		t.Errorf("Err() = %v, want context.Canceled", g.Err())
	}
}

// TestSpawnGate_ErrNilUntilDone pins the context contract and the C1 ordering
// invariant: Err is nil while Done is open, and non-nil IMMEDIATELY once a
// receive from Done returns. Under -race, a finish that closed done before
// setting err is reported as a data race on err.
func TestSpawnGate_ErrNilUntilDone(t *testing.T) {
	var ready atomic.Bool
	g := NewSpawnGate(D(10*time.Second), 20*time.Millisecond, ready.Load)
	t.Cleanup(g.Stop)

	if err := g.Err(); err != nil {
		t.Fatalf("Err() = %v before Done closed, want nil", err)
	}
	ready.Store(true)
	<-g.Done()
	if g.Err() == nil {
		t.Fatal("Err() = nil immediately after Done closed — err must be set BEFORE close(done)")
	}
}

// TestSpawnGate_ReportsDeadlineAndNilValue asserts the conservative Deadline
// upper bound (construction + budget + deadline, ok=true), a non-nil Done, and
// a nil Value for any key.
func TestSpawnGate_ReportsDeadlineAndNilValue(t *testing.T) {
	const budget, deadline = time.Minute, time.Hour
	before := time.Now()
	g := NewSpawnGate(budget, deadline, func() bool { return false })
	after := time.Now()
	t.Cleanup(g.Stop)

	dl, ok := g.Deadline()
	if !ok {
		t.Fatal("Deadline() ok = false, want true (the adapters must see a caller deadline and add no overlay)")
	}
	if dl.Before(before.Add(budget+deadline)) || dl.After(after.Add(budget+deadline)) {
		t.Errorf("Deadline() = %v, want construction + %s + %s", dl, budget, deadline)
	}
	if g.Done() == nil {
		t.Error("Done() = nil; os/exec only watches a context whose Done is non-nil")
	}
	if v := g.Value("any-key"); v != nil {
		t.Errorf("Value() = %v, want nil", v)
	}
}

// TestSpawnGate_DerivedContextInheritsDeadlineExceeded pins that a stdlib
// context derived from the gate (as exec.CommandContext's watcher and the
// adapters' overlay would) observes the gate's DeadlineExceeded — both when
// derived before the gate finishes and when derived after.
func TestSpawnGate_DerivedContextInheritsDeadlineExceeded(t *testing.T) {
	g := NewSpawnGate(D(10*time.Second), 20*time.Millisecond, func() bool { return true })
	t.Cleanup(g.Stop)

	early, cancelEarly := context.WithTimeout(g, time.Hour)
	defer cancelEarly()
	waitDone(t, early)
	if !errors.Is(early.Err(), context.DeadlineExceeded) {
		t.Errorf("derived-before ctx Err() = %v, want context.DeadlineExceeded", early.Err())
	}

	late, cancelLate := context.WithTimeout(g, time.Hour)
	defer cancelLate()
	waitDone(t, late)
	if !errors.Is(late.Err(), context.DeadlineExceeded) {
		t.Errorf("derived-after ctx Err() = %v, want context.DeadlineExceeded", late.Err())
	}
}

// TestPidFile_RoundTrip asserts WritePidFile/ReadPidFile/PidFileReady agree
// and that the atomic write leaves no .tmp file behind.
func TestPidFile_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gc.pid")
	if err := WritePidFile(path, 4242); err != nil {
		t.Fatalf("WritePidFile: %v", err)
	}
	if pid, ok := ReadPidFile(path); !ok || pid != 4242 {
		t.Errorf("ReadPidFile = (%d, %v), want (4242, true)", pid, ok)
	}
	if !PidFileReady(path)() {
		t.Error("PidFileReady = false after a successful write")
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s.tmp = %v, want not-exist (the rename must consume it)", path, err)
	}
}

// TestPidFile_MissingFile asserts an absent pid file is not ready.
func TestPidFile_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.pid")
	if pid, ok := ReadPidFile(path); ok {
		t.Errorf("ReadPidFile on a missing file = (%d, true), want ok=false", pid)
	}
	if PidFileReady(path)() {
		t.Error("PidFileReady = true for a missing file")
	}
}

// TestPidFile_GarbageContent asserts non-integer and non-positive contents
// (including a partial/empty write) are rejected.
func TestPidFile_GarbageContent(t *testing.T) {
	for _, content := range []string{"", "abc", "0", "-5", "12x"} {
		path := filepath.Join(t.TempDir(), "gc.pid")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if pid, ok := ReadPidFile(path); ok {
			t.Errorf("ReadPidFile(%q) = (%d, true), want ok=false", content, pid)
		}
		if PidFileReady(path)() {
			t.Errorf("PidFileReady(%q) = true, want false", content)
		}
	}
}

// TestPidFile_BadDirectory asserts the temp-write failure surfaces.
func TestPidFile_BadDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "gc.pid")
	if err := WritePidFile(path, 1); err == nil {
		t.Error("WritePidFile into a missing directory = nil, want an error")
	}
}

// TestPidFile_RenameFailureRemovesTemp asserts a failed rename (the target is
// a non-empty directory) surfaces and does not leave the .tmp file behind.
func TestPidFile_RenameFailureRemovesTemp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gc.pid")
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WritePidFile(path, 1); err == nil {
		t.Fatal("WritePidFile over a non-empty directory = nil, want a rename error")
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s.tmp = %v, want not-exist after a failed rename", path, err)
	}
}
