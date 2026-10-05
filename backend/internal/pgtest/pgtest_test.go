package pgtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- pure classifier unit tests (no container) ---

func TestIsContainerNameConflict(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"docker conflict", errors.New(`Error response from daemon: Conflict. The container name "/fishhawk-test-postgres" is already in use by container "abc123". You have to remove (or rename) that container to be able to reuse that name.`), true},
		{"409 conflict", errors.New("create container: request returned status code 409: Conflict"), true},
		{"unrelated docker error", errors.New("error during connect: dial tcp: lookup docker"), false},
		{"pg error", &pgconn.PgError{Code: "42P04", Message: "database already exists"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isContainerNameConflict(tt.err); got != tt.want {
				t.Errorf("isContainerNameConflict() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsStaleContainerRef(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"docker no such container", errors.New(`start container fishhawk-test-postgres in state running: container start: Error response from daemon: No such container: b2428abc123`), true},
		{"lowercase variant", errors.New("no such container: deadbeef"), true},
		{"name conflict not stale", errors.New(`Conflict. The container name "/fishhawk-test-postgres" is already in use`), false},
		{"unrelated error", errors.New("disk full"), false},
		{"pg error", &pgconn.PgError{Code: "42P04", Message: "database already exists"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStaleContainerRef(tt.err); got != tt.want {
				t.Errorf("isStaleContainerRef() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsDuplicateDatabase(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"42P04", &pgconn.PgError{Code: "42P04", Message: `database "fishhawk_tmpl" already exists`}, true},
		{"wrapped 42P04", fmt.Errorf("create template db: %w", &pgconn.PgError{Code: "42P04"}), true},
		{"55006 not duplicate", &pgconn.PgError{Code: "55006"}, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDuplicateDatabase(tt.err); got != tt.want {
				t.Errorf("isDuplicateDatabase() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsTemplate1Contention(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"55006", &pgconn.PgError{Code: "55006", Message: "source database is being accessed by other users"}, true},
		{"wrapped 55006", fmt.Errorf("create db: %w", &pgconn.PgError{Code: "55006"}), true},
		{"42P04 not contention", &pgconn.PgError{Code: "42P04"}, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTemplate1Contention(tt.err); got != tt.want {
				t.Errorf("isTemplate1Contention() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsDockerUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"daemon down", errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"), true},
		{"socket dial", errors.New("dial unix /var/run/docker.sock: connect: no such file or directory"), true},
		{"binary missing", errors.New("exec: \"docker\": executable file not found in $PATH"), true},
		{"unrelated error", errors.New("relation does not exist"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDockerUnavailable(tt.err); got != tt.want {
				t.Errorf("isDockerUnavailable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- behavioral tests, one per failure mode (no container) ---

// TestSharedContainer_NameConflictAttaches injects a name-conflict error
// followed by success and asserts attachWithRetry converges to ONE attached
// handle (no second start) rather than erroring or restarting.
func TestSharedContainer_NameConflictAttaches(t *testing.T) {
	conflict := errors.New(`Conflict. The container name "/fishhawk-test-postgres" is already in use`)
	calls := 0
	got, err := attachWithRetry(5, time.Millisecond, func() (string, error) {
		calls++
		if calls == 1 {
			return "", conflict
		}
		return "attached", nil
	})
	if err != nil {
		t.Fatalf("attachWithRetry: unexpected error %v", err)
	}
	if got != "attached" {
		t.Errorf("attachWithRetry returned %q, want %q", got, "attached")
	}
	if calls != 2 {
		t.Errorf("run invoked %d times, want 2 (one conflict, one success)", calls)
	}

	// Give-up: a persistent conflict exhausts the attempts and errors.
	calls = 0
	if _, err := attachWithRetry(3, time.Millisecond, func() (string, error) {
		calls++
		return "", conflict
	}); err == nil {
		t.Error("attachWithRetry: expected error after attempts exhausted, got nil")
	}
	if calls != 3 {
		t.Errorf("run invoked %d times, want 3 (the attempt cap)", calls)
	}

	// A non-conflict error returns immediately without retrying.
	calls = 0
	other := errors.New("disk full")
	if _, err := attachWithRetry(5, time.Millisecond, func() (string, error) {
		calls++
		return "", other
	}); !errors.Is(err, other) {
		t.Errorf("attachWithRetry returned %v, want the non-conflict error", err)
	}
	if calls != 1 {
		t.Errorf("run invoked %d times on non-conflict error, want 1 (no retry)", calls)
	}
}

// TestSharedContainer_StaleRefReattaches injects a docker stale-reuse-handle
// error ("No such container") followed by success and asserts attachWithRetry
// RE-CREATES the evicted container (converging in exactly 2 calls), distinct
// from the name-conflict retry path. It also asserts the two fail-closed
// modes: a PERSISTENT stale ref exhausts the attempt cap and errors (give-up
// branch), and a genuinely-unretryable error returns immediately with no
// retry (fail-hard preserved).
func TestSharedContainer_StaleRefReattaches(t *testing.T) {
	stale := errors.New(`container start: Error response from daemon: No such container: b2428abc123`)
	calls := 0
	got, err := attachWithRetry(5, time.Millisecond, func() (string, error) {
		calls++
		if calls == 1 {
			return "", stale
		}
		return "attached", nil
	})
	if err != nil {
		t.Fatalf("attachWithRetry: unexpected error %v", err)
	}
	if got != "attached" {
		t.Errorf("attachWithRetry returned %q, want %q", got, "attached")
	}
	if calls != 2 {
		t.Errorf("run invoked %d times, want 2 (one stale ref, one success)", calls)
	}

	// Give-up: a persistent stale ref exhausts the attempts and errors.
	calls = 0
	if _, err := attachWithRetry(3, time.Millisecond, func() (string, error) {
		calls++
		return "", stale
	}); err == nil {
		t.Error("attachWithRetry: expected error after attempts exhausted, got nil")
	}
	if calls != 3 {
		t.Errorf("run invoked %d times, want 3 (the attempt cap)", calls)
	}

	// Fail-hard preserved: a genuinely-unretryable error returns immediately
	// without retrying.
	calls = 0
	other := errors.New("disk full")
	if _, err := attachWithRetry(5, time.Millisecond, func() (string, error) {
		calls++
		return "", other
	}); !errors.Is(err, other) {
		t.Errorf("attachWithRetry returned %v, want the unretryable error", err)
	}
	if calls != 1 {
		t.Errorf("run invoked %d times on unretryable error, want 1 (no retry)", calls)
	}
}

// TestBootstrap_DuplicateDatabaseTolerated injects SQLSTATE 42P04 and
// asserts the bootstrap ADOPTS the existing template (returns nil) instead
// of failing — the cross-process race fix.
func TestBootstrap_DuplicateDatabaseTolerated(t *testing.T) {
	dup := &pgconn.PgError{Code: "42P04", Message: `database "fishhawk_tmpl" already exists`}
	migrated := false
	err := bootstrapWith(
		func() error { return dup },
		func() error { migrated = true; return nil },
	)
	if err != nil {
		t.Fatalf("bootstrapWith tolerating 42P04: unexpected error %v", err)
	}
	if !migrated {
		t.Error("bootstrapWith did not run migrate after adopting the template")
	}

	// A non-duplicate create error propagates (not tolerated).
	createErr := errors.New("permission denied")
	if err := bootstrapWith(
		func() error { return createErr },
		func() error { return nil },
	); !errors.Is(err, createErr) {
		t.Errorf("bootstrapWith returned %v, want the create error", err)
	}

	// A migrate error after a clean create propagates.
	migErr := errors.New("bad migration")
	if err := bootstrapWith(
		func() error { return nil },
		func() error { return migErr },
	); !errors.Is(err, migErr) {
		t.Errorf("bootstrapWith returned %v, want the migrate error", err)
	}
}

// TestNewURL_Template1ContentionRetries injects SQLSTATE 55006 transiently
// and asserts the bounded retry rides it out, then a give-up variant after
// the cap, then immediate return on a non-contention error.
func TestNewURL_Template1ContentionRetries(t *testing.T) {
	contention := &pgconn.PgError{Code: "55006", Message: "source database is being accessed by other users"}
	calls := 0
	if err := createWithContentionRetry(5, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return contention
		}
		return nil
	}); err != nil {
		t.Fatalf("createWithContentionRetry: unexpected error %v", err)
	}
	if calls != 3 {
		t.Errorf("create invoked %d times, want 3 (two contended, one success)", calls)
	}

	// Give-up: persistent 55006 exhausts attempts and errors.
	calls = 0
	if err := createWithContentionRetry(3, time.Millisecond, func() error {
		calls++
		return contention
	}); err == nil {
		t.Error("createWithContentionRetry: expected error after attempts exhausted, got nil")
	}
	if calls != 3 {
		t.Errorf("create invoked %d times, want 3 (the attempt cap)", calls)
	}

	// A non-contention error returns immediately without retrying.
	calls = 0
	other := &pgconn.PgError{Code: "42501", Message: "permission denied"}
	if err := createWithContentionRetry(5, time.Millisecond, func() error {
		calls++
		return other
	}); !errors.Is(err, other) {
		t.Errorf("createWithContentionRetry returned %v, want the non-contention error", err)
	}
	if calls != 1 {
		t.Errorf("create invoked %d times on non-contention error, want 1 (no retry)", calls)
	}
}

// recordingTB is a fake fataler that records which branch failStart took.
type recordingTB struct {
	skipped bool
	fataled bool
}

func (r *recordingTB) Helper()               {}
func (r *recordingTB) Skipf(string, ...any)  { r.skipped = true }
func (r *recordingTB) Fatalf(string, ...any) { r.fataled = true }

// TestNew_DockerUnavailableSkips asserts the daemon-absent shape routes to
// the skip branch while any other start error routes to fatal.
func TestNew_DockerUnavailableSkips(t *testing.T) {
	skipTB := &recordingTB{}
	failStart(skipTB, errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"))
	if !skipTB.skipped || skipTB.fataled {
		t.Errorf("docker-unavailable: skipped=%v fataled=%v, want skipped=true fataled=false", skipTB.skipped, skipTB.fataled)
	}

	fatalTB := &recordingTB{}
	failStart(fatalTB, errors.New("some other start failure"))
	if fatalTB.skipped || !fatalTB.fataled {
		t.Errorf("other-error: skipped=%v fataled=%v, want skipped=false fataled=true", fatalTB.skipped, fatalTB.fataled)
	}
}

func TestReplaceDBName(t *testing.T) {
	got := replaceDBName("postgres://fishhawk:fishhawk@localhost:32768/fishhawk?sslmode=disable", "fh_abc")
	want := "postgres://fishhawk:fishhawk@localhost:32768/fh_abc?sslmode=disable"
	if got != want {
		t.Errorf("replaceDBName() = %q, want %q", got, want)
	}
}

// --- external-server routing (#2137), no container ---

// msgTB is a fake fataler that records the branch AND the message, and returns
// from Skipf/Fatalf (resolveBase returns explicitly after each).
type msgTB struct {
	skipped bool
	fataled bool
	msg     string
}

func (m *msgTB) Helper() {}
func (m *msgTB) Skipf(f string, a ...any) {
	m.skipped = true
	m.msg = fmt.Sprintf(f, a...)
}
func (m *msgTB) Fatalf(f string, a ...any) {
	m.fataled = true
	m.msg = fmt.Sprintf(f, a...)
}

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// runnerDSN is the exact unix-socket DSN shape the runner pins (#2137).
const runnerDSN = "postgres://fishhawk:fishhawk@/fishhawk?host=/pgsock&sslmode=disable"

// TestResolveBase_ExternalURLSkipsContainerStart: with FISHHAWK_TEST_PG_URL set
// the testcontainers start is never called, the template is bootstrapped on the
// provided server, and that URL is the base.
func TestResolveBase_ExternalURLSkipsContainerStart(t *testing.T) {
	tb := &msgTB{}
	started := false
	var bootstrapped string
	got := resolveBase(tb, envOf(map[string]string{externalURLEnv: runnerDSN}),
		func() (string, error) { started = true; return "postgres://container", nil },
		func(u string) error { bootstrapped = u; return nil })
	if started {
		t.Error("testcontainers start was called although FISHHAWK_TEST_PG_URL is set")
	}
	if got != runnerDSN {
		t.Errorf("resolveBase = %q, want the provided URL %q", got, runnerDSN)
	}
	if bootstrapped != runnerDSN {
		t.Errorf("template bootstrapped on %q, want %q", bootstrapped, runnerDSN)
	}
	if tb.skipped || tb.fataled {
		t.Errorf("unexpected terminal call: skipped=%v fataled=%v msg=%q", tb.skipped, tb.fataled, tb.msg)
	}
}

// TestResolveBase_ExternalErrorFatalsNeverSkips: an external bootstrap error
// CRAFTED to carry the Docker-unavailable marker must still Fatalf — the only
// thing between it and a silent Skipf is the external branch's own routing.
func TestResolveBase_ExternalErrorFatalsNeverSkips(t *testing.T) {
	tb := &msgTB{}
	bootErr := errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	if !isDockerUnavailable(bootErr) {
		t.Fatal("fixture: the crafted error must match the docker-unavailable marker")
	}
	got := resolveBase(tb, envOf(map[string]string{externalURLEnv: runnerDSN}),
		func() (string, error) { t.Error("start must not be called"); return "", nil },
		func(string) error { return bootErr })
	if tb.skipped {
		t.Errorf("external error was SKIPPED (%q); a provided server must fail closed", tb.msg)
	}
	if !tb.fataled {
		t.Fatal("external error did not Fatalf")
	}
	if !strings.Contains(tb.msg, externalURLEnv) || !strings.Contains(tb.msg, "Cannot connect") {
		t.Errorf("fatal message %q should name %s and carry the cause", tb.msg, externalURLEnv)
	}
	if got != "" {
		t.Errorf("resolveBase = %q on error, want empty", got)
	}
}

// TestResolveBase_GateContainerWithoutURLFatals: inside the gate container with
// no provisioned server, Fatalf naming the runner remedy, start never called.
func TestResolveBase_GateContainerWithoutURLFatals(t *testing.T) {
	tb := &msgTB{}
	started := false
	resolveBase(tb, envOf(map[string]string{gateContainerEnv: "1"}),
		func() (string, error) { started = true; return "", errors.New("no docker host") },
		func(string) error { t.Error("bootstrap must not be called"); return nil })
	if started {
		t.Error("testcontainers start was attempted inside the gate container")
	}
	if !tb.fataled || tb.skipped {
		t.Fatalf("fataled=%v skipped=%v, want a Fatalf", tb.fataled, tb.skipped)
	}
	if !strings.Contains(tb.msg, "FISHHAWK_GATE_SERVICES=postgres") {
		t.Errorf("fatal message %q should name the runner remedy FISHHAWK_GATE_SERVICES=postgres", tb.msg)
	}
}

// TestResolveBase_SkipIntegrationAndContainerPath pins the unchanged arms:
// FISHHAWK_SKIP_INTEGRATION wins over everything, and with no env the
// container start runs and its error routes through failStart.
func TestResolveBase_SkipIntegrationAndContainerPath(t *testing.T) {
	skipTB := &msgTB{}
	resolveBase(skipTB, envOf(map[string]string{"FISHHAWK_SKIP_INTEGRATION": "1", externalURLEnv: runnerDSN, gateContainerEnv: "1"}),
		func() (string, error) { t.Error("start must not be called"); return "", nil },
		func(string) error { t.Error("bootstrap must not be called"); return nil })
	if !skipTB.skipped || skipTB.fataled {
		t.Errorf("SKIP_INTEGRATION: skipped=%v fataled=%v, want a skip", skipTB.skipped, skipTB.fataled)
	}

	okTB := &msgTB{}
	if got := resolveBase(okTB, envOf(nil), func() (string, error) { return "postgres://container", nil },
		func(string) error { t.Error("bootstrap must not be called"); return nil }); got != "postgres://container" {
		t.Errorf("container path = %q, want the started base", got)
	}

	downTB := &msgTB{}
	resolveBase(downTB, envOf(nil), func() (string, error) {
		return "", errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	}, func(string) error { return nil })
	if !downTB.skipped {
		t.Errorf("container path with Docker down should skip via failStart (fataled=%v)", downTB.fataled)
	}
}

// TestExternalURL_UnixSocketHostParses: pgx reads the runner DSN's host query
// parameter as a unix-socket directory, and replaceDBName keeps it.
func TestExternalURL_UnixSocketHostParses(t *testing.T) {
	for _, dsn := range []string{runnerDSN, replaceDBName(runnerDSN, "fh_abc")} {
		cfg, err := pgconn.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("ParseConfig(%q): %v", dsn, err)
		}
		if cfg.Host != "/pgsock" {
			t.Errorf("%q: Host = %q, want /pgsock", dsn, cfg.Host)
		}
		network, addr := pgconn.NetworkAddress(cfg.Host, cfg.Port)
		if network != "unix" || addr != "/pgsock/.s.PGSQL.5432" {
			t.Errorf("%q: NetworkAddress = (%q, %q), want (unix, /pgsock/.s.PGSQL.5432)", dsn, network, addr)
		}
	}
	got := replaceDBName(runnerDSN, "fh_abc")
	if !strings.Contains(got, "/fh_abc?") || !strings.Contains(got, "host=/pgsock") {
		t.Errorf("replaceDBName(runnerDSN) = %q, want the new path with host=/pgsock kept", got)
	}
}

// --- Docker-guarded integration test ---

// TestExternalURL_BootstrapsAgainstProvidedServer feeds the shared container's
// URL through the EXTERNAL branch with the production bootstrapExternalOnce
// (the real bootstrapTemplate), then creates a per-test database from it and
// checks it carries the migrated schema.
func TestExternalURL_BootstrapsAgainstProvidedServer(t *testing.T) {
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		t.Skip("FISHHAWK_SKIP_INTEGRATION set")
	}
	base, err := sharedContainerBaseURL()
	if err != nil {
		failStart(t, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	got := resolveBase(t, envOf(map[string]string{externalURLEnv: base}),
		func() (string, error) { t.Fatal("start must not be called on the external branch"); return "", nil },
		bootstrapExternalOnce)
	if got != base {
		t.Fatalf("resolveBase = %q, want the provided URL", got)
	}
	// A second call hits the once-cached result instead of re-bootstrapping.
	if err := bootstrapExternalOnce(base); err != nil {
		t.Fatalf("second bootstrapExternalOnce = %v, want the cached nil", err)
	}

	conn, err := pgx.Connect(ctx, newURLFrom(t, got))
	if err != nil {
		t.Fatalf("connect per-test db: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var version int64
	var dirty bool
	if err := conn.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
		t.Fatalf("per-test db is not migrated: %v", err)
	}
	if version == 0 || dirty {
		t.Errorf("per-test db schema_migrations = (%d, dirty=%v), want a clean head", version, dirty)
	}
}

// TestSharedContainer_SharesAndIsolates calls NewPool twice in one process
// and asserts both pools share the container (same host:port) yet are
// isolated (a table created via pool A is invisible via pool B).
func TestSharedContainer_SharesAndIsolates(t *testing.T) {
	poolA := NewPool(t)
	poolB := NewPool(t)

	cfgA := poolA.Config().ConnConfig
	cfgB := poolB.Config().ConnConfig
	if cfgA.Host != cfgB.Host || cfgA.Port != cfgB.Port {
		t.Errorf("pools not on the same container: A=%s:%d B=%s:%d", cfgA.Host, cfgA.Port, cfgB.Host, cfgB.Port)
	}
	if cfgA.Database == cfgB.Database {
		t.Errorf("pools share the same database %q; want distinct per-test databases", cfgA.Database)
	}

	ctx := context.Background()
	if _, err := poolA.Exec(ctx, "CREATE TABLE iso_check (id int)"); err != nil {
		t.Fatalf("create table in pool A: %v", err)
	}
	var n int
	if err := poolB.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'iso_check'`,
	).Scan(&n); err != nil {
		t.Fatalf("query table presence in pool B: %v", err)
	}
	if n != 0 {
		t.Errorf("iso_check visible in pool B (count=%d); databases are not isolated", n)
	}
}
