package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"

	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// These tests pin the #3122 raw-container leak fix WITHOUT Docker. They drive
// startContainerWith — the injectable seam CONTAINING the container-start error
// branch — so the assertions exercise the branch that decides whether the leaked
// container is terminated at all (operator condition 1: test the call site, not
// just the leaf helper). Deleting the terminateStartFailure CALL from that branch
// reddens TestStartContainerWith_TerminatesOnStartError.

// fakeContainer implements testcontainers.Container by embedding the interface
// (nil at runtime — the embedded methods are never called) and overriding only
// Terminate, which records its call count and the context it received. The
// Terminate signature MUST match Container.Terminate(ctx, ...TerminateOption)
// exactly, or it would not override the promoted interface method.
type fakeContainer struct {
	testcontainers.Container
	mu             sync.Mutex
	terminateCalls int
	lastCtx        context.Context
	lastErrAtCall  error // ctx.Err() captured AT the call, before terminateStartFailure's deferred cancel
	termErr        error
}

func (f *fakeContainer) Terminate(ctx context.Context, _ ...testcontainers.TerminateOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminateCalls++
	f.lastCtx = ctx
	f.lastErrAtCall = ctx.Err()
	return f.termErr
}

func (f *fakeContainer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.terminateCalls
}

// fakeFataler stands in for *testing.T. Unlike the real thing its Skipf/Fatalf
// RECORD and RETURN rather than ending the goroutine, so startContainerWith runs
// past them to its explicit post-Fatalf return — which is exactly how the seam
// lets a test observe the error branch.
type fakeFataler struct {
	skipfCalled  bool
	fatalfCalled bool
	lastFormat   string
	lastArgs     []any
	// logs holds every Logf line, formatted. Logf is non-terminal, so it sets
	// neither skipfCalled nor fatalfCalled: a test can tell a log from a
	// terminal call.
	logs []string
}

func (f *fakeFataler) Helper() {}
func (f *fakeFataler) Skipf(format string, args ...any) {
	f.skipfCalled = true
	f.lastFormat, f.lastArgs = format, args
}
func (f *fakeFataler) Fatalf(format string, args ...any) {
	f.fatalfCalled = true
	f.lastFormat, f.lastArgs = format, args
}
func (f *fakeFataler) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}
func (f *fakeFataler) message() string {
	return fmt.Sprintf(f.lastFormat, f.lastArgs...)
}

// noopCleanup is a cleanup sink for the error-path tests, where the success-path
// cleanup registration is never reached.
func noopCleanup(func()) {}

// (a) Terminate IS called on the start-error path. This is the load-bearing
// counterfactual vehicle for condition 1: deleting terminateStartFailure(...)
// from startContainerWith's error branch leaves terminateCalls == 0 here.
func TestStartContainerWith_TerminatesOnStartError(t *testing.T) {
	t.Setenv("FISHHAWK_SKIP_INTEGRATION", "") // force the Fatalf branch, not Skipf
	fc := &fakeContainer{}
	f := &fakeFataler{}
	startErr := errors.New("start container: started hook: context deadline exceeded")

	got := startContainerWith(f, func(func()) {
		t.Fatal("success cleanup must not run on the start-error path")
	}, context.Background(), func(context.Context) (testcontainers.Container, connStringFunc, error) {
		return fc, nil, startErr
	})

	if got != "" {
		t.Errorf("startContainerWith on error = %q, want empty", got)
	}
	if fc.calls() != 1 {
		t.Errorf("Terminate called %d times on the start-error path, want 1", fc.calls())
	}
	if !f.fatalfCalled {
		t.Errorf("expected Fatalf on a non-docker-unavailable start error (skipf=%v)", f.skipfCalled)
	}
}

// (b) A TYPED-NIL handle (a nil *fakeContainer boxed in a non-nil interface) must
// not panic and must not reach Terminate. Deleting the reflect guard makes this
// PANIC (a bare c != nil is true for a typed nil).
func TestStartContainerWith_TypedNilHandleNoPanic(t *testing.T) {
	var typedNil *fakeContainer               // nil pointer
	var c testcontainers.Container = typedNil // non-nil interface, nil pointee
	f := &fakeFataler{}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("startContainerWith panicked on a typed-nil handle: %v", r)
		}
	}()

	startContainerWith(f, noopCleanup, context.Background(),
		func(context.Context) (testcontainers.Container, connStringFunc, error) {
			return c, nil, errors.New("boom with a typed-nil handle")
		})
	if !f.fatalfCalled && !f.skipfCalled {
		t.Error("expected a terminal Fatalf/Skipf after the typed-nil guard")
	}
}

// (c) An UNTYPED-NIL handle must not panic (the plain c == nil guard).
func TestStartContainerWith_UntypedNilHandleNoPanic(t *testing.T) {
	f := &fakeFataler{}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("startContainerWith panicked on an untyped-nil handle: %v", r)
		}
	}()
	startContainerWith(f, noopCleanup, context.Background(),
		func(context.Context) (testcontainers.Container, connStringFunc, error) {
			return nil, nil, errors.New("boom with a nil handle")
		})
	if !f.fatalfCalled && !f.skipfCalled {
		t.Error("expected a terminal Fatalf/Skipf after the untyped-nil guard")
	}
}

// (d) The context handed to Terminate carries its OWN fresh deadline, NOT the
// caller's remainder. startCtx is passed already EXPIRED (mimicking a start that
// fails late with ~0 budget left); the fix ignores it and derives a fresh 30s
// deadline from context.Background(), so the recorded ctx has ~30s remaining and
// is not yet Done. The counterfactual — deriving the cleanup ctx from startCtx —
// yields a deadline in the past and reddens the >=29s assertion (condition 3).
func TestStartContainerWith_FreshTerminateContext(t *testing.T) {
	fc := &fakeContainer{}
	f := &fakeFataler{}
	// Expired outer context: deadline one second in the PAST.
	startCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	startContainerWith(f, noopCleanup, startCtx,
		func(context.Context) (testcontainers.Container, connStringFunc, error) {
			return fc, nil, errors.New("late start failure")
		})

	if fc.lastCtx == nil {
		t.Fatal("Terminate was not called, so no cleanup context to inspect")
	}
	deadline, ok := fc.lastCtx.Deadline()
	if !ok {
		t.Fatal("cleanup context has no deadline; want a fresh bounded 30s deadline")
	}
	if remaining := time.Until(deadline); remaining < 29*time.Second {
		t.Errorf("cleanup context deadline is %v out, want >= ~29s (fresh 30s budget, not the caller's remainder)", remaining)
	}
	// Captured AT the Terminate call (before terminateStartFailure's deferred
	// cancel): a fresh context is live even though startCtx is already expired.
	// Reusing startCtx would make this DeadlineExceeded.
	if fc.lastErrAtCall != nil {
		t.Errorf("cleanup context was already errored when handed to Terminate (%v); a fresh context must be live even when startCtx is expired", fc.lastErrAtCall)
	}
}

// (e) A Terminate error is SWALLOWED: the ORIGINAL start error reaches the
// caller's Fatalf verbatim and the swallowed cleanup error never appears.
func TestStartContainerWith_SwallowsTerminateError(t *testing.T) {
	t.Setenv("FISHHAWK_SKIP_INTEGRATION", "") // force the Fatalf branch
	distinct := errors.New("terminate-boom-DISTINCT-SENTINEL")
	fc := &fakeContainer{termErr: distinct}
	f := &fakeFataler{}
	startErr := errors.New("start postgres: ORIGINAL-START-SIGNATURE")

	startContainerWith(f, noopCleanup, context.Background(),
		func(context.Context) (testcontainers.Container, connStringFunc, error) {
			return fc, nil, startErr
		})

	if !f.fatalfCalled {
		t.Fatalf("expected Fatalf on the start-error path (skipf=%v)", f.skipfCalled)
	}
	msg := f.message()
	if !strings.Contains(msg, "ORIGINAL-START-SIGNATURE") {
		t.Errorf("terminal message = %q, want it to carry the original start error verbatim", msg)
	}
	if strings.Contains(msg, "terminate-boom-DISTINCT-SENTINEL") {
		t.Errorf("terminal message leaked the swallowed Terminate error: %q", msg)
	}
	if fc.calls() != 1 {
		t.Errorf("Terminate called %d times, want 1", fc.calls())
	}
}

// --- #2137: raw databases on a runner-provided server ---

func envMap(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// TestStartContainer_GateContainerWithoutURLFatals: inside the gate container
// with no provisioned server, rawDBSource Fatalf's naming the runner remedy and
// never attempts the container start (no daemon is reachable in there).
func TestStartContainer_GateContainerWithoutURLFatals(t *testing.T) {
	f := &fakeFataler{}
	started := false
	got := rawDBSource(f, noopCleanup, envMap(map[string]string{gateContainerEnv: "1"}),
		func() string { started = true; return "postgres://container" })
	if started {
		t.Error("container start was attempted inside the gate container")
	}
	if !f.fatalfCalled || f.skipfCalled {
		t.Fatalf("fatalf=%v skipf=%v, want a Fatalf", f.fatalfCalled, f.skipfCalled)
	}
	if !strings.Contains(f.message(), "FISHHAWK_GATE_SERVICES=postgres") {
		t.Errorf("fatal message %q should name the runner remedy", f.message())
	}
	if got != "" {
		t.Errorf("rawDBSource = %q, want empty", got)
	}

	// FISHHAWK_SKIP_INTEGRATION keeps precedence: skip, still no start.
	sf := &fakeFataler{}
	rawDBSource(sf, noopCleanup, envMap(map[string]string{gateContainerEnv: "1", "FISHHAWK_SKIP_INTEGRATION": "1"}),
		func() string { started = true; return "" })
	if !sf.skipfCalled || sf.fatalfCalled || started {
		t.Errorf("SKIP_INTEGRATION in the gate container: skipf=%v fatalf=%v started=%v, want a skip and no start", sf.skipfCalled, sf.fatalfCalled, started)
	}
}

// TestRawDBSource_SkipIntegrationPrecedesExternalURL: FISHHAWK_SKIP_INTEGRATION
// wins over FISHHAWK_TEST_PG_URL (pgtest's resolveBase order). The URL points
// at a dead socket, so if the external branch ran first it would Fatalf; the
// skip must land with no database created, no drop registered and no start.
func TestRawDBSource_SkipIntegrationPrecedesExternalURL(t *testing.T) {
	dead := "postgres://fishhawk:fishhawk@/fishhawk?host=" + t.TempDir() + "&sslmode=disable&connect_timeout=2"
	for _, env := range []map[string]string{
		{"FISHHAWK_SKIP_INTEGRATION": "1", externalPGURLEnv: dead},
		{"FISHHAWK_SKIP_INTEGRATION": "1", externalPGURLEnv: dead, gateContainerEnv: "1"},
		{"FISHHAWK_SKIP_INTEGRATION": "1"},
	} {
		f := &fakeFataler{}
		started := false
		got := rawDBSource(f, func(func()) { t.Error("no drop may be registered under FISHHAWK_SKIP_INTEGRATION") },
			envMap(env), func() string { started = true; return "postgres://container" })
		if !f.skipfCalled || f.fatalfCalled || started || got != "" {
			t.Errorf("env %v: skipf=%v fatalf=%v started=%v got=%q, want a skip before any server or container (%q)",
				env, f.skipfCalled, f.fatalfCalled, started, got, f.message())
		}
	}
}

// TestRawDBSource_NoEnvUsesContainer pins the unchanged default arm.
func TestRawDBSource_NoEnvUsesContainer(t *testing.T) {
	f := &fakeFataler{}
	if got := rawDBSource(f, noopCleanup, envMap(nil), func() string { return "postgres://container" }); got != "postgres://container" {
		t.Errorf("rawDBSource with no env = %q, want the container URL", got)
	}
	if f.fatalfCalled || f.skipfCalled {
		t.Errorf("unexpected terminal call: %q", f.message())
	}
}

// TestRawDBSource_ExternalUnreachableFatalsNeverSkips: a provided server that
// cannot be reached fails closed (Fatalf), never skips, and never falls back to
// the container start.
func TestRawDBSource_ExternalUnreachableFatalsNeverSkips(t *testing.T) {
	f := &fakeFataler{}
	dead := "postgres://fishhawk:fishhawk@/fishhawk?host=" + t.TempDir() + "&sslmode=disable&connect_timeout=2"
	started := false
	rawDBSource(f, func(func()) { t.Error("no drop may be registered when create never happened") },
		envMap(map[string]string{externalPGURLEnv: dead}),
		func() string { started = true; return "postgres://container" })
	if started {
		t.Error("fell back to the container start although FISHHAWK_TEST_PG_URL is set")
	}
	if f.skipfCalled || !f.fatalfCalled {
		t.Fatalf("skipf=%v fatalf=%v, want a Fatalf (a promised server must fail closed)", f.skipfCalled, f.fatalfCalled)
	}
	if !strings.Contains(f.message(), externalPGURLEnv) {
		t.Errorf("fatal message %q should name %s", f.message(), externalPGURLEnv)
	}
}

// TestRetryObjectInUse: 55006 is retried, the cap gives up, anything else
// returns immediately.
func TestRetryObjectInUse(t *testing.T) {
	inUse := &pgconn.PgError{Code: "55006"}
	calls := 0
	if err := retryObjectInUse(5, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return inUse
		}
		return nil
	}); err != nil || calls != 3 {
		t.Errorf("transient 55006: err=%v calls=%d, want nil after 3", err, calls)
	}
	calls = 0
	if err := retryObjectInUse(3, time.Millisecond, func() error { calls++; return inUse }); err == nil || calls != 3 {
		t.Errorf("persistent 55006: err=%v calls=%d, want an error after the 3-attempt cap", err, calls)
	}
	calls = 0
	other := &pgconn.PgError{Code: "42501"}
	if err := retryObjectInUse(5, time.Millisecond, func() error { calls++; return other }); !errors.Is(err, other) || calls != 1 {
		t.Errorf("non-55006: err=%v calls=%d, want the error after 1 call", err, calls)
	}
}

// dialErr builds the connect-level shapes (EAGAIN, ECONNREFUSED) the raw-database
// drop must retry (#5170): a unix-socket dial that fails instead of queueing.
func dialErr(errno syscall.Errno) error {
	return fmt.Errorf("failed to connect: %w", &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", errno)})
}

// testDropPolicy keeps every unit test fast and free of wall-clock assertions:
// a 1ms delay, a generous per-attempt slice and a cap high enough that only a
// test that WANTS the cap reaches it.
func testDropPolicy() rawDropPolicy {
	return rawDropPolicy{delay: time.Millisecond, attemptTimeout: time.Minute, maxConnectFailures: 1000}
}

// runawayGuard is how a test turns a mutated, never-ending retry loop into a
// RED assertion instead of a hang, with no wall clock involved: past n calls the
// context is cancelled, which ends dropRawDB's loop.
const runawayGuard = 50

// TestIsRetryableRawDropErr: the classifier table. A transient refusal and every
// non-PgError failure (the connect-level class) retries; any other SQLSTATE is a
// definitive answer and does not.
func TestIsRetryableRawDropErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"53300 wrapped", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "53300"}), true},
		{"57P03 wrapped", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "57P03"}), true},
		{"55006 wrapped", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "55006"}), true},
		{"42501 bare", &pgconn.PgError{Code: "42501"}, false},
		{"28P01 wrapped", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "28P01"}), false},
		{"unix dial EAGAIN", dialErr(syscall.EAGAIN), true},
		{"unix dial ECONNREFUSED", dialErr(syscall.ECONNREFUSED), true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"wrapped deadline exceeded", fmt.Errorf("connect: %w", context.DeadlineExceeded), true},
	}
	for _, tc := range cases {
		if got := isRetryableRawDropErr(tc.err); got != tc.want {
			t.Errorf("%s: isRetryableRawDropErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDropRawDB_RetriesNonPgErrorFailure is the #5170 counterfactual vehicle:
// ONE non-PgError dial failure, then success. The pre-fix loop retried only a
// server refusal, so it stopped after call 1 and leaked the database.
func TestDropRawDB_RetriesNonPgErrorFailure(t *testing.T) {
	calls := 0
	err := dropRawDB(context.Background(), testDropPolicy(), func(context.Context) error {
		calls++
		if calls == 1 {
			return dialErr(syscall.EAGAIN)
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Errorf("one dial EAGAIN then success: err=%v calls=%d, want nil after exactly 2 calls", err, calls)
	}
}

// TestDropRawDB_TransientPgRefusalStillRetried: the 53300 class keeps retrying.
func TestDropRawDB_TransientPgRefusalStillRetried(t *testing.T) {
	calls := 0
	err := dropRawDB(context.Background(), testDropPolicy(), func(context.Context) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("connect: %w", &pgconn.PgError{Code: "53300"})
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Errorf("one 53300 then success: err=%v calls=%d, want nil after exactly 2 calls", err, calls)
	}
}

// TestDropRawDB_StopsOnNonTransientPgError: a definitive SQLSTATE ends the loop
// at once and is returned, wrapped and still reachable through errors.As, with a
// message that says it was NOT a budget exit.
func TestDropRawDB_StopsOnNonTransientPgError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := dropRawDB(ctx, testDropPolicy(), func(context.Context) error {
		calls++
		if calls >= runawayGuard {
			cancel()
		}
		return &pgconn.PgError{Code: "42501"}
	})
	if calls != 1 {
		t.Errorf("42501: calls=%d, want exactly 1 (a definitive answer is not retried)", calls)
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "42501" {
		t.Fatalf("err=%v, want it to wrap the 42501 PgError", err)
	}
	if !strings.Contains(err.Error(), "non-retryable") || strings.Contains(err.Error(), "budget") {
		t.Errorf("err=%q, want the non-retryable wording and no budget clause", err)
	}
}

// TestDropRawDB_AttemptTimeoutIsRetried (approval condition 1): each attempt runs
// under its OWN deadline. Attempt 1 blocks until that per-attempt deadline fires
// while the overall context is still live; the loop retries and attempt 2
// succeeds. The overall context has no deadline here, so nothing in this test
// depends on wall-clock scheduling: attempt 1 simply waits for its own ctx.
func TestDropRawDB_AttemptTimeoutIsRetried(t *testing.T) {
	overall, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := testDropPolicy()
	p.attemptTimeout = 20 * time.Millisecond
	calls := 0
	var firstErr error
	err := dropRawDB(overall, p, func(ctx context.Context) error {
		calls++
		if calls != 1 {
			return nil
		}
		if _, ok := ctx.Deadline(); !ok {
			// The per-attempt deadline is missing: fail without blocking.
			t.Errorf("attempt 1 context has no deadline; want a per-attempt context.WithTimeout")
			return nil
		}
		<-ctx.Done()
		firstErr = ctx.Err()
		return fmt.Errorf("connect: %w", ctx.Err())
	})
	if err != nil || calls != 2 {
		t.Errorf("stalled attempt then success: err=%v calls=%d, want nil after exactly 2 calls", err, calls)
	}
	if !errors.Is(firstErr, context.DeadlineExceeded) {
		t.Errorf("attempt 1 ended with %v, want its own context.DeadlineExceeded", firstErr)
	}
	if overall.Err() != nil {
		t.Errorf("overall context ended (%v); only the attempt's own deadline should have fired", overall.Err())
	}
}

// TestDropRawDB_BudgetExpiryReturnsLastError (approval condition 2): the budget
// ends the retries and the last error is returned, not discarded. Expiry is
// driven by the attempt itself (it cancels the overall context on call 3), so
// the call count is exact and nothing depends on wall-clock scheduling.
func TestDropRawDB_BudgetExpiryReturnsLastError(t *testing.T) {
	sentinel := errors.New("sentinel connect failure")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := dropRawDB(ctx, testDropPolicy(), func(context.Context) error {
		calls++
		switch {
		case calls == 3:
			cancel() // the budget runs out during this attempt
		case calls >= runawayGuard:
			return nil // a mutated loop that ignores the budget ends here, as a RED
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v, want it to wrap the last attempt's error", err)
	}
	if calls != 3 {
		t.Errorf("calls=%d, want exactly 3 (the attempt that cancelled is the last)", calls)
	}
	msg := err.Error()
	if !strings.Contains(msg, "3 attempt(s)") || !strings.Contains(msg, "budget exhausted") || strings.Contains(msg, "non-retryable") {
		t.Errorf("err=%q, want the attempt count and the budget-exhausted wording, not the non-retryable one", msg)
	}
}

// TestDropRawDB_BudgetExpiryDuringWaitReturnsLastError: the budget ends BETWEEN
// attempts, with the loop waiting on its (hour-long) delay timer, and the
// returned error is still the last REAL attempt error (a transient 53300), not
// the context error a further attempt on the expired ctx would produce. The
// attempt honours its ctx like pgx.Connect does. Attempt 1 schedules the cancel
// from a goroutine, so the loop reaches the wait either side of it, and the
// delay timer can never fire first: the outcome is the same under any
// scheduling, with no wall-clock assertion. A mutated loop that retries on the
// expired ctx gets only context errors back and ends at runawayGuard with nil.
func TestDropRawDB_BudgetExpiryDuringWaitReturnsLastError(t *testing.T) {
	lastReal := &pgconn.PgError{Code: "53300", Message: "sorry, too many clients already"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := testDropPolicy()
	p.delay = time.Hour
	calls := 0
	err := dropRawDB(ctx, p, func(actx context.Context) error {
		calls++
		switch {
		case calls == 1:
			go cancel() // the budget runs out while the loop is between attempts
			return fmt.Errorf("connect: %w", lastReal)
		case calls >= runawayGuard:
			return nil // a mutated loop that ignores the budget ends here, as a RED
		}
		if cerr := actx.Err(); cerr != nil {
			return fmt.Errorf("connect: %w", cerr)
		}
		return nil
	})
	if !errors.Is(err, lastReal) {
		t.Fatalf("err=%v, want it to wrap the last REAL attempt error (53300), not a context error", err)
	}
	if calls != 1 {
		t.Errorf("calls=%d, want exactly 1 (no attempt starts on an expired budget)", calls)
	}
	msg := err.Error()
	if !strings.Contains(msg, "1 attempt(s)") || !strings.Contains(msg, "budget exhausted") || strings.Contains(msg, "non-retryable") {
		t.Errorf("err=%q, want the attempt count and the budget-exhausted wording", msg)
	}
}

// TestDropRawDB_ConnectFailureCap (approval condition 4): a persistently
// unreachable server stops at the connect-failure cap, long before the budget
// (here the overall context never ends on its own), and says so.
func TestDropRawDB_ConnectFailureCap(t *testing.T) {
	sentinel := errors.New("sentinel connect failure")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := testDropPolicy()
	p.maxConnectFailures = 3
	calls := 0
	err := dropRawDB(ctx, p, func(context.Context) error {
		calls++
		if calls >= runawayGuard {
			cancel() // a mutated loop with no cap ends here, as a RED
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) || calls != 3 {
		t.Fatalf("err=%v calls=%d, want the last error after exactly 3 calls (the cap)", err, calls)
	}
	msg := err.Error()
	if !strings.Contains(msg, "cap of 3") || strings.Contains(msg, "budget") || strings.Contains(msg, "non-retryable") {
		t.Errorf("err=%q, want the connect-failure-cap wording, distinct from the budget and non-retryable exits", msg)
	}
}

// TestRawDropCleanup_LogsAndRecordsFailure: a give-up is logged once through the
// non-terminal Logf and recorded per name for the parent test's survival
// message; it is never a Fatalf or Skipf. A successful drop logs and records
// nothing. Names are uuid-unique, so the shared rawDropFailures map cannot leak
// a record between sub-cases or from another test.
func TestRawDropCleanup_LogsAndRecordsFailure(t *testing.T) {
	failName := "fh_raw_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f := &fakeFataler{}
	rawDropCleanup(f, failName, time.Minute, testDropPolicy(), func(context.Context) error {
		return &pgconn.PgError{Code: "42501"}
	})()
	if len(f.logs) != 1 || !strings.Contains(f.logs[0], failName) || !strings.Contains(f.logs[0], "42501") {
		t.Errorf("logs=%q, want exactly one entry naming %s and 42501", f.logs, failName)
	}
	if got := rawDropErr(failName); !strings.Contains(got, "42501") {
		t.Errorf("rawDropErr(%s)=%q, want the recorded error naming 42501", failName, got)
	}
	if f.fatalfCalled || f.skipfCalled {
		t.Errorf("fatalf=%v skipf=%v, want neither: the drop is best-effort", f.fatalfCalled, f.skipfCalled)
	}

	okName := "fh_raw_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	g := &fakeFataler{}
	rawDropCleanup(g, okName, time.Minute, testDropPolicy(), func(context.Context) error { return nil })()
	if len(g.logs) != 0 {
		t.Errorf("success logged %q, want nothing", g.logs)
	}
	if got := rawDropErr(okName); got != "no drop error recorded" {
		t.Errorf("rawDropErr(%s)=%q after a successful drop, want %q", okName, got, "no drop error recorded")
	}
}

// TestStartContainer_ExternalURLCreatesFreshRawDB drives the REAL startContainer
// through FISHHAWK_TEST_PG_URL. Off the gate container it stands the server up
// itself and connects as a mirror of the role the runner's service hands the
// gate (createGateRole; #2137 approval condition 1, widened by #4050) — so it
// also proves the raw-DB flow plus golang-migrate over pgx work under that
// role, that CREATE ROLE succeeds from its DSN, and that superuser-only
// statements are still refused. Inside the gate container it uses the
// provided URL as-is.
func TestStartContainer_ExternalURLCreatesFreshRawDB(t *testing.T) {
	server := os.Getenv(externalPGURLEnv)
	leastPrivilege := false
	if server == "" {
		server = createGateRole(t, startContainer(t))
		leastPrivilege = true
	}

	var names []string
	t.Run("two calls give distinct empty databases", func(t *testing.T) {
		t.Setenv(externalPGURLEnv, server)
		a, b := startContainer(t), startContainer(t)
		if a == b {
			t.Fatalf("two startContainer calls returned the same URL %q", a)
		}
		ctx := context.Background()
		for _, dsn := range []string{a, b} {
			cfg, err := pgx.ParseConfig(dsn)
			if err != nil {
				t.Fatalf("parse raw db url: %v", err)
			}
			names = append(names, cfg.Database)
			if !strings.HasPrefix(cfg.Database, "fh_raw_") {
				t.Errorf("raw database %q, want an fh_raw_ name", cfg.Database)
			}
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("connect raw db: %v", err)
			}
			var reg *string
			err = conn.QueryRow(ctx, "SELECT to_regclass('schema_migrations')::text").Scan(&reg)
			_ = conn.Close(ctx)
			if err != nil {
				t.Fatalf("probe schema_migrations: %v", err)
			}
			if reg != nil {
				t.Errorf("raw database %q already has schema_migrations; want an EMPTY database", cfg.Database)
			}
		}
		// pgx + golang-migrate over the provided server as the gate role.
		if err := postgres.MigrateUp(a); err != nil {
			t.Fatalf("MigrateUp on the raw database (least privilege=%v): %v", leastPrivilege, err)
		}
		if leastPrivilege {
			conn, err := pgx.Connect(ctx, server)
			if err != nil {
				t.Fatalf("connect as the gate role: %v", err)
			}
			defer func() { _ = conn.Close(ctx) }()
			// CREATEROLE (#4050): the backend's RLS tests create probe roles.
			probe := pgx.Identifier{"fh_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]}.Sanitize()
			if _, err := conn.Exec(ctx, "CREATE ROLE "+probe); err != nil {
				t.Errorf("CREATE ROLE from the gate role's DSN: %v, want success (CREATEROLE)", err)
			} else if _, err := conn.Exec(ctx, "DROP ROLE "+probe); err != nil {
				t.Errorf("DROP ROLE %s: %v", probe, err)
			}
			// NOSUPERUSER: the escape routes stay refused.
			for _, stmt := range []string{
				"COPY (SELECT 1) TO PROGRAM 'true'",
				"GRANT pg_execute_server_program TO CURRENT_USER",
			} {
				_, err := conn.Exec(ctx, stmt)
				var pg *pgconn.PgError
				if !errors.As(err, &pg) || pg.Code != "42501" {
					t.Errorf("%s from the gate role's DSN = %v, want SQLSTATE 42501 (insufficient_privilege)", stmt, err)
				}
			}
		}
	})

	// The subtest's cleanups have run: both raw databases are dropped.
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, server)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, n := range names {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", n).Scan(&exists); err != nil {
			t.Fatalf("probe pg_database: %v", err)
		}
		if exists {
			t.Errorf("raw database %q survived its cleanup; drop error: %s", n, rawDropErr(n))
		}
	}
}

// createGateRole creates a login role mirroring the gate role's posture on the
// server behind admin and returns admin's URL re-pointed at that role. The
// attributes mirror postgresGateRoleAttributes in
// runner/internal/gateiso/service.go by convention (a separate module).
func createGateRole(t *testing.T, admin string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	role := "fh_gate_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	pass := strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER CREATEDB CREATEROLE BYPASSRLS",
		pgx.Identifier{role}.Sanitize(), pass)); err != nil {
		t.Fatalf("create gate role: %v", err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse admin url: %v", err)
	}
	u.User = url.UserPassword(role, pass)
	return u.String()
}
