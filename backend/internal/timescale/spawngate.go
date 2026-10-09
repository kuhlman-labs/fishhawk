package timescale

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"
)

// spawnPollInterval is how often a SpawnGate re-checks its spawn precondition.
// It is sub-boundary sampling granularity, not a boundary, so it is NOT scaled
// (the same rule as the 20ms poll intervals in the boundary-timeout tests).
const spawnPollInterval = 20 * time.Millisecond

// Fataler is the slice of *testing.T that RequireArmed needs. It is an
// interface so this package does not import testing and so the precondition
// failure can be asserted against a recording fake.
type Fataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

// SpawnGate is a test-support context.Context for a boundary-timeout test whose
// cheap deadline must not race process spawn (#4177, the #3587 deadline-role
// separation). A raw context.WithTimeout(D(300ms)) starts its clock when the
// test calls the code under test, so on a loaded host (where Factor() is 1 —
// the runner's gate container does not set CI) the deadline can fire before the
// fake CLI has fork/exec'd its grandchild, and the test then fails on a symptom
// that looks like a defect in the control under test.
//
// The gate separates the two deadline ROLES:
//
//   - the SPAWN BUDGET, a generous must-complete window for the spawn
//     precondition (ready() reporting true — in the #1805 fixtures, the
//     grandchild's pid recorded by the fake right after exec.Cmd.Start). It
//     costs nothing on the happy path because the gate arms the moment the
//     precondition holds. Exhausting it ends the gate with
//     context.DeadlineExceeded (so the code under test unblocks) and makes
//     RequireArmed fail the test as a named PRECONDITION, never as the control.
//   - the CHEAP DEADLINE, whose expiry IS the verdict under test. It is a real
//     stdlib context.WithTimeout that is only ARMED once the precondition
//     holds, so its expiry is a genuine context.DeadlineExceeded measured from
//     the arm instant (ArmedAt), which is where elapsed bounds should start.
//
// Err is nil until Done is closed and non-nil once it is: every finishing path
// (the armed deadline, spawn-budget exhaustion, Stop) sets the error BEFORE it
// closes done, inside one critical section. A stdlib context derived from a
// non-stdlib parent reads parent.Err() right after it observes parent.Done()
// and panics on nil, so that ordering is load-bearing.
//
// Deadline reports construction+spawnBudget+deadline: a conservative UPPER
// bound, not the instant Done fires. The review adapters read only the
// presence bit (so they add no cfg.Timeout overlay of their own).
//
// SpawnGate and the pid-file helpers below are TEST SUPPORT, like the rest of
// this package: only _test.go files import them.
type SpawnGate struct {
	spawnBudget time.Duration
	deadline    time.Duration
	created     time.Time

	done     chan struct{} // closed exactly once, by finish, after err is set
	stop     chan struct{} // closed by Stop
	stopOnce sync.Once
	exited   chan struct{} // closed when the watcher goroutine returns

	mu            sync.Mutex
	err           error // written once, before close(done); read only after done is closed
	armed         bool
	armedAt       time.Time
	spawnTimedOut bool
}

// NewSpawnGate returns a gate that arms a real deadline-long timer once ready()
// reports true, polling it immediately and then every spawnPollInterval, for at
// most spawnBudget. Callers MUST arrange for Stop to run (t.Cleanup(gate.Stop))
// so the watcher goroutine exits with the test.
func NewSpawnGate(spawnBudget, deadline time.Duration, ready func() bool) *SpawnGate {
	g := &SpawnGate{
		spawnBudget: spawnBudget,
		deadline:    deadline,
		created:     time.Now(),
		done:        make(chan struct{}),
		stop:        make(chan struct{}),
		exited:      make(chan struct{}),
	}
	go g.watch(ready)
	return g
}

// watch waits for the spawn precondition, then arms the cheap deadline.
func (g *SpawnGate) watch(ready func() bool) {
	defer close(g.exited)

	budget := time.NewTimer(g.spawnBudget)
	defer budget.Stop()
	tick := time.NewTicker(spawnPollInterval)
	defer tick.Stop()

	for !ready() {
		select {
		case <-g.stop:
			g.finish(context.Canceled, false)
			return
		case <-budget.C:
			// The precondition never held. End the gate so the code under
			// test unblocks, and flag it so RequireArmed names the cause.
			g.finish(context.DeadlineExceeded, true)
			return
		case <-tick.C:
		}
	}

	// Record the arm instant BEFORE starting the deadline timer, so
	// (Done-observed instant - armedAt) >= deadline holds by construction.
	g.mu.Lock()
	g.armed = true
	g.armedAt = time.Now()
	g.mu.Unlock()

	armed, cancel := context.WithTimeout(context.Background(), g.deadline)
	defer cancel()
	select {
	case <-armed.Done():
		g.finish(armed.Err(), false)
	case <-g.stop:
		g.finish(context.Canceled, false)
	}
}

// finish ends the gate. The watcher goroutine is its only caller and calls it
// exactly once on each of its three paths (armed deadline, spawn-budget
// exhaustion, Stop), so done is closed exactly once. err is set BEFORE
// close(done), inside one critical section, so a reader that has observed Done
// closed always reads a non-nil Err (the C1 invariant; see the SpawnGate doc).
func (g *SpawnGate) finish(err error, spawnTimedOut bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.err = err
	g.spawnTimedOut = spawnTimedOut
	close(g.done)
}

// Deadline implements context.Context. It is a conservative upper bound
// (construction + spawn budget + deadline), and ok is always true.
func (g *SpawnGate) Deadline() (time.Time, bool) {
	return g.created.Add(g.spawnBudget + g.deadline), true
}

// Done implements context.Context. It is never nil, which os/exec requires
// before it starts the goroutine that runs cmd.Cancel on expiry.
func (g *SpawnGate) Done() <-chan struct{} { return g.done }

// Err implements context.Context: nil until Done is closed, then the finishing
// error. It reads err only after observing done closed — the close→receive
// edge publishes the write made before close(done).
func (g *SpawnGate) Err() error {
	select {
	case <-g.done:
		return g.err
	default:
		return nil
	}
}

// Value implements context.Context. The gate carries no values.
func (*SpawnGate) Value(any) any { return nil }

// ArmedAt reports the instant the cheap deadline was armed, and whether it
// ever was.
func (g *SpawnGate) ArmedAt() (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.armedAt, g.armed
}

// RequireArmed returns the arm instant, or fails the test as a named
// PRECONDITION when the gate never armed. Call it right after the code under
// test returns and BEFORE any behavioral assertion, so a starved host is
// reported as host load rather than as a defect in the control under test.
func (g *SpawnGate) RequireArmed(t Fataler) time.Time {
	t.Helper()
	g.mu.Lock()
	armed, armedAt, timedOut := g.armed, g.armedAt, g.spawnTimedOut
	g.mu.Unlock()
	if armed {
		return armedAt
	}
	cause := "the call under test returned before the spawn precondition held (a fork/exec failure of the fake CLI or its grandchild)"
	if timedOut {
		cause = "the spawn budget was exhausted (host load / fork-exec latency, or a fork/exec failure)"
	}
	t.Fatalf("PRECONDITION (#4177): the spawn gate never armed within the %s spawn budget — %s; the %s cheap deadline never started. "+
		"This is host load or a spawn failure, NOT a defect in the control under test.",
		g.spawnBudget, cause, g.deadline)
	return time.Time{}
}

// Stop ends the gate with context.Canceled if it has not already finished, and
// waits for the watcher goroutine to exit. It is idempotent.
func (g *SpawnGate) Stop() {
	g.stopOnce.Do(func() { close(g.stop) })
	<-g.exited
}

// WritePidFile records pid at path atomically: it writes path+".tmp" and
// renames it into place, so a concurrent ReadPidFile never sees a partial
// write. A fake CLI calls it right after exec.Cmd.Start of its grandchild.
func WritePidFile(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ReadPidFile returns the pid recorded at path; ok is true only for a positive
// integer.
func ReadPidFile(path string) (pid int, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(string(b))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// PidFileReady is a SpawnGate ready func: true once path holds a valid pid.
func PidFileReady(path string) func() bool {
	return func() bool {
		_, ok := ReadPidFile(path)
		return ok
	}
}
