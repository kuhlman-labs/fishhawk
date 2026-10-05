package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
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

// TestStartContainer_ExternalURLCreatesFreshRawDB drives the REAL startContainer
// through FISHHAWK_TEST_PG_URL. Off the gate container it stands the server up
// itself and connects as a NOSUPERUSER CREATEDB-only login role — the least-
// privilege role the runner's service hands the gate (#2137 approval condition
// 1) — so it also proves the raw-DB flow plus golang-migrate over pgx work
// under that role, and that a superuser-only statement is refused from its
// DSN. Inside the gate container it uses the provided URL as-is.
func TestStartContainer_ExternalURLCreatesFreshRawDB(t *testing.T) {
	server := os.Getenv(externalPGURLEnv)
	leastPrivilege := false
	if server == "" {
		server = createDBOnlyRole(t, startContainer(t))
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
			_, err = conn.Exec(ctx, "CREATE ROLE fh_should_not_exist")
			_ = conn.Close(ctx)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != "42501" {
				t.Errorf("CREATE ROLE from the gate role's DSN = %v, want SQLSTATE 42501 (insufficient_privilege)", err)
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
			t.Errorf("raw database %q survived its cleanup", n)
		}
	}
}

// createDBOnlyRole creates a NOSUPERUSER CREATEDB-only login role on the server
// behind admin and returns admin's URL re-pointed at that role.
func createDBOnlyRole(t *testing.T, admin string) string {
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
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER CREATEDB NOCREATEROLE NOBYPASSRLS",
		pgx.Identifier{role}.Sanitize(), pass)); err != nil {
		t.Fatalf("create least-privilege role: %v", err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse admin url: %v", err)
	}
	u.User = url.UserPassword(role, pass)
	return u.String()
}
