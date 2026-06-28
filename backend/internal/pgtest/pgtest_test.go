package pgtest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

// --- Docker-guarded integration test ---

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
