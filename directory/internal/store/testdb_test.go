package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Postgres routing for this package's tests (#4047). It mirrors
// backend/internal/pgtest.resolveBase (#2137), which the module boundary
// stops this module importing, so the two stay in step by convention.
//
// Inside the runner's --network=none gate container there is no Docker
// daemon, so the runner provisions a Postgres service and hands its
// unix-socket DSN in as FISHHAWK_TEST_PG_URL. resolveBase routes, in order:
//
//  1. FISHHAWK_SKIP_INTEGRATION set: skip (unchanged precedence).
//  2. FISHHAWK_TEST_PG_URL set: that server is the base. start is NEVER
//     called, and there is no skip path: the runner promised a database, so a
//     skip would silently drop the suite from the gate.
//  3. FISHHAWK_GATE_CONTAINER=1 without a URL: a fatal reason naming the
//     runner-side remedy, reported by every integration test through
//     Fatalf. start is never called (no daemon is reachable in there).
//  4. Otherwise the testcontainers start; an absent Docker daemon skips.
const (
	externalURLEnv   = "FISHHAWK_TEST_PG_URL"
	gateContainerEnv = "FISHHAWK_GATE_CONTAINER"
)

// containerStarts counts startContainer calls so a test can prove the
// provided-server path started no container (TestProvidedServerStartsNoContainer).
var containerStarts atomic.Int32

// fataler is the slice of *testing.T the routing report uses; a fake lets the
// pure tests observe which terminal call fired.
type fataler interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// baseResult is what resolveBase decided. Exactly one of url, skip and fatal
// is set on a nil error. terminate is set whenever start created a container,
// including when start then failed, so the caller can reap it before it
// panics.
type baseResult struct {
	url, skip, fatal string
	terminate        func()
}

// resolveBase is the pure routing seam under setup(). A non-nil error is a
// start failure that is neither a skip nor a fatal reason; the caller panics
// on it, as before.
func resolveBase(getenv func(string) string, start func() (string, func(), error)) (baseResult, error) {
	if getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		return baseResult{skip: "FISHHAWK_SKIP_INTEGRATION set"}, nil
	}
	if ext := getenv(externalURLEnv); ext != "" {
		return baseResult{url: ext}, nil
	}
	if getenv(gateContainerEnv) == "1" {
		return baseResult{fatal: fmt.Sprintf("directory/internal/store: inside the gate container (%s=1) with no provisioned Postgres (%s unset); "+
			"set FISHHAWK_GATE_SERVICES=postgres on the runner so it provisions one (#2137)", gateContainerEnv, externalURLEnv)}, nil
	}

	base, terminate, err := start()
	res := baseResult{terminate: terminate}
	if err != nil {
		if isDockerUnavailable(err) {
			res.skip = fmt.Sprintf("Docker not available: %v", err)
			return res, nil
		}
		return res, err
	}
	res.url = base
	return res, nil
}

// startContainer starts this package's own unshared Postgres container. It
// deliberately does NOT attach to the shared fishhawk-test-postgres by
// reuse+name: that would require replicating pgtest's whole hardening ladder
// (first-start name-conflict attach-retry, stale-reuse re-create,
// cross-process template bootstrap), and a naive WithReuse+WithName bootstrap
// is exactly the flake source #1174 removed. One package here needs Postgres,
// so one container per test binary is the cheap, hazard-free option — and
// because scripts/test disables ryuk and only reaps the shared container by
// name, this one is terminated explicitly in TestMain rather than leaked. It
// is unnamed (testcontainers assigns a random name), so it cannot collide with
// a concurrent invocation either.
//
// The returned terminate is non-nil whenever a container exists, including
// alongside an error, so a failure after tcpostgres.Run cannot leak it.
func startContainer() (string, func(), error) {
	containerStarts.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("fishhawk_directory"),
		tcpostgres.WithUsername("fishhawk"),
		tcpostgres.WithPassword("fishhawk"),
		tcpostgres.BasicWaitStrategies(),
	)
	var terminate func()
	if container != nil {
		terminate = func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			_ = container.Terminate(ctx)
		}
	}
	if err != nil {
		return "", terminate, err
	}

	base, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", terminate, fmt.Errorf("connection string: %w", err)
	}
	return base, terminate, nil
}

// isDockerUnavailable reports the daemon-absent shape, so a dev without
// Docker skips rather than fails (mirrors backend/internal/pgtest).
func isDockerUnavailable(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
		"is the docker daemon running",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// requireBase reports the routing decision from TestMain's package state.
func requireBase(tb fataler) {
	tb.Helper()
	requireBaseFrom(tb, skipReason, fatalReason)
}

// requireBaseFrom skips on a skip reason and FAILS on a fatal reason; the
// fatal arm never skips. Fake fatalers return from Skipf/Fatalf, so each
// terminal call is followed by an explicit return.
func requireBaseFrom(tb fataler, skip, fatal string) {
	tb.Helper()
	if skip != "" {
		tb.Skipf("skipping integration test: %s", skip)
		return
	}
	if fatal != "" {
		tb.Fatalf("%s", fatal)
		return
	}
}

// newTestDatabase creates a throwaway database on the routed base server and
// returns its URL. It is dropped WITH (FORCE) in t.Cleanup; a caller that
// opens a pool on it must register the pool's Close AFTER this call so the
// LIFO cleanup order closes the pool first. TEMPLATE template0 keeps the
// create independent of any session on template1.
func newTestDatabase(t *testing.T) string {
	t.Helper()
	requireBase(t)

	ctx := context.Background()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random db name: %v", err)
	}
	dbName := "fh_dir_" + hex.EncodeToString(buf)

	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect base db: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()+" TEMPLATE template0"); err != nil {
		t.Fatalf("create per-test db: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, baseURL)
		if err != nil {
			return // best-effort drop
		}
		defer func() { _ = c.Close(ctx) }()
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)")
	})

	return replaceDBName(baseURL, dbName)
}

func replaceDBName(base, dbName string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Path = "/" + dbName
	return u.String()
}

// --- routing units: pure, no container, no t.Setenv ---

// runnerDSN is the exact unix-socket DSN shape the runner pins (#2137).
const runnerDSN = "postgres://fishhawk:fishhawk@/fishhawk?host=/pgsock&sslmode=disable"

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// recordingStart returns a start fake that counts its calls and returns the
// given results.
func recordingStart(calls *int, base string, terminate func(), err error) func() (string, func(), error) {
	return func() (string, func(), error) {
		*calls++
		return base, terminate, err
	}
}

// With FISHHAWK_TEST_PG_URL set the start fake SUCCEEDS, so routing that
// falls through to start would record a call and return "postgres://container".
func TestResolveBase_ProvidedURLStartsNoContainer(t *testing.T) {
	var calls int
	res, err := resolveBase(envOf(map[string]string{externalURLEnv: runnerDSN}),
		recordingStart(&calls, "postgres://container", nil, nil))
	if err != nil {
		t.Fatalf("resolveBase: %v", err)
	}
	if calls != 0 {
		t.Errorf("start was called %d time(s); a provided server must start no container", calls)
	}
	if res.url != runnerDSN {
		t.Errorf("url = %q, want the provided DSN %q", res.url, runnerDSN)
	}
	if res.skip != "" || res.fatal != "" {
		t.Errorf("skip=%q fatal=%q, want both empty", res.skip, res.fatal)
	}
}

// The start fake returns a Docker-UNAVAILABLE error, so routing that falls
// through to start would turn the gate-container case into a silent skip.
func TestResolveBase_GateContainerWithoutURLFatals(t *testing.T) {
	var calls int
	res, err := resolveBase(envOf(map[string]string{gateContainerEnv: "1"}),
		recordingStart(&calls, "", nil, errors.New("Cannot connect to the Docker daemon")))
	if err != nil {
		t.Fatalf("resolveBase: %v", err)
	}
	if calls != 0 {
		t.Errorf("start was called %d time(s); there is no daemon inside the gate container", calls)
	}
	if !strings.Contains(res.fatal, "FISHHAWK_GATE_SERVICES=postgres") {
		t.Errorf("fatal = %q, want it to name FISHHAWK_GATE_SERVICES=postgres", res.fatal)
	}
	if res.skip != "" {
		t.Errorf("skip = %q, want empty: a gate-container run must never skip", res.skip)
	}
	if res.url != "" {
		t.Errorf("url = %q, want empty", res.url)
	}
}

func TestResolveBase_SkipIntegrationWinsAndHostPathUnchanged(t *testing.T) {
	t.Run("skip integration wins over URL and gate container", func(t *testing.T) {
		var calls int
		res, err := resolveBase(envOf(map[string]string{
			"FISHHAWK_SKIP_INTEGRATION": "1",
			externalURLEnv:              runnerDSN,
			gateContainerEnv:            "1",
		}), recordingStart(&calls, "postgres://container", nil, nil))
		if err != nil {
			t.Fatalf("resolveBase: %v", err)
		}
		if calls != 0 {
			t.Errorf("start was called %d time(s)", calls)
		}
		if res.skip == "" || res.url != "" || res.fatal != "" {
			t.Errorf("res = %+v, want only skip set", res)
		}
	})

	t.Run("empty env starts the container and carries its url and terminate", func(t *testing.T) {
		var calls, terminated int
		res, err := resolveBase(envOf(nil),
			recordingStart(&calls, "postgres://container", func() { terminated++ }, nil))
		if err != nil {
			t.Fatalf("resolveBase: %v", err)
		}
		if calls != 1 {
			t.Errorf("start calls = %d, want 1", calls)
		}
		if res.url != "postgres://container" || res.skip != "" || res.fatal != "" {
			t.Errorf("res = %+v, want only url=postgres://container", res)
		}
		if res.terminate == nil {
			t.Fatal("terminate not carried through")
		}
		res.terminate()
		if terminated != 1 {
			t.Errorf("terminate invocations = %d, want 1", terminated)
		}
	})

	t.Run("docker unavailable skips", func(t *testing.T) {
		var calls int
		res, err := resolveBase(envOf(nil),
			recordingStart(&calls, "", nil, errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")))
		if err != nil {
			t.Fatalf("resolveBase: %v", err)
		}
		if !strings.HasPrefix(res.skip, "Docker not available") || res.fatal != "" {
			t.Errorf("res = %+v, want a Docker-unavailable skip", res)
		}
	})

	t.Run("any other start error is returned, not skipped", func(t *testing.T) {
		boom := errors.New("pull access denied")
		var calls int
		res, err := resolveBase(envOf(nil), recordingStart(&calls, "", nil, boom))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		if res.skip != "" || res.fatal != "" || res.url != "" {
			t.Errorf("res = %+v, want no routing decision on a start error", res)
		}
	})
}

// A failure AFTER the container exists (a ConnectionString error) returns the
// terminate alongside the error; resolveBase must carry it so setup() can
// assign terminateFn before it panics, instead of leaking the container.
func TestResolveBase_StartErrorCarriesTerminate(t *testing.T) {
	boom := errors.New("connection string: boom")
	var terminated int
	var calls int
	res, err := resolveBase(envOf(nil),
		recordingStart(&calls, "", func() { terminated++ }, boom))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if res.terminate == nil {
		t.Fatal("terminate dropped on the start-error path: the container would leak")
	}
	res.terminate()
	if terminated != 1 {
		t.Errorf("terminate invocations = %d, want 1", terminated)
	}
}

// msgTB is a fake fataler that records which terminal call fired and its
// message. Like *testing.T's, Skipf/Fatalf record then return (the real ones
// halt the goroutine), so requireBaseFrom returns explicitly after each.
type msgTB struct {
	skipped, fataled bool
	msg              string
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

func TestRequireBase_FatalReasonFatalsNeverSkips(t *testing.T) {
	fatal := "set FISHHAWK_GATE_SERVICES=postgres on the runner"
	tb := &msgTB{}
	requireBaseFrom(tb, "", fatal)
	if tb.skipped || !tb.fataled {
		t.Fatalf("fatal reason: skipped=%v fataled=%v, want skipped=false fataled=true", tb.skipped, tb.fataled)
	}
	if !strings.Contains(tb.msg, "FISHHAWK_GATE_SERVICES") {
		t.Errorf("fatal message %q does not name FISHHAWK_GATE_SERVICES", tb.msg)
	}

	tb = &msgTB{}
	requireBaseFrom(tb, "Docker not available", "")
	if !tb.skipped || tb.fataled {
		t.Errorf("skip reason: skipped=%v fataled=%v, want skipped=true fataled=false", tb.skipped, tb.fataled)
	}

	tb = &msgTB{}
	requireBaseFrom(tb, "", "")
	if tb.skipped || tb.fataled {
		t.Errorf("no reason: skipped=%v fataled=%v, want neither", tb.skipped, tb.fataled)
	}
}

// The runner's unix-socket DSN carries the socket directory in the query
// (`host=`), so the per-test rename must change only the database.
func TestExternalURL_UnixSocketDSNSurvivesRename(t *testing.T) {
	cfg, err := pgconn.ParseConfig(replaceDBName(runnerDSN, "fh_dir_x"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Host != "/pgsock" {
		t.Errorf("Host = %q, want /pgsock", cfg.Host)
	}
	if cfg.Database != "fh_dir_x" {
		t.Errorf("Database = %q, want fh_dir_x", cfg.Database)
	}
}

// In-situ check against the real TestMain: when the provided-server env var
// is set, no testcontainers start happened and the base is that server. On the
// host path (var unset) it logs and returns rather than skipping, so the host
// suite gains no skip lines.
func TestProvidedServerStartsNoContainer(t *testing.T) {
	ext := os.Getenv(externalURLEnv)
	if ext == "" || os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		t.Logf("%s unset (host path) or integration skipped: provided-server arm not exercised", externalURLEnv)
		return
	}
	if n := containerStarts.Load(); n != 0 {
		t.Errorf("startContainer ran %d time(s) with %s set", n, externalURLEnv)
	}
	if baseURL != ext {
		t.Errorf("baseURL = %q, want the provided %q", baseURL, ext)
	}
}
