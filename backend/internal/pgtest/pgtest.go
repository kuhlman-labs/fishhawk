// Package pgtest gives every backend integration test a freshly-migrated
// Postgres database while starting only ONE container for the whole
// `scripts/test` run, eliminating the base start-contention flake (#1174,
// #972). Instead of one testcontainers Postgres per package, a single
// process-singleton, WithReuseByName-keyed container (fishhawk-test-postgres)
// is shared across every test binary, and each test gets its own cheap
// ephemeral database via CREATE DATABASE ... TEMPLATE.
//
// Because `scripts/test` runs with TESTCONTAINERS_RYUK_DISABLED=true, the
// named container persists across package processes, so three concurrency
// hazards arise and are each handled with a pure, unit-testable classifier:
//
//   - First-start name-conflict race (isContainerNameConflict): two test
//     binaries can both try to create the named container; the loser gets a
//     docker name conflict. A bounded attach-retry re-resolves to the now
//     existing container instead of erroring or starting a second one.
//   - Cross-process template bootstrap (isDuplicateDatabase): each process
//     has its own sync.Once, so the 2nd+ processes re-attempt CREATE DATABASE
//     fishhawk_tmpl and hit SQLSTATE 42P04. The advisory-locked bootstrap
//     tolerates 42P04 and adopts the already-bootstrapped template.
//   - Per-test template contention (isTemplate1Contention): concurrent
//     CREATE DATABASE ... TEMPLATE can fail with SQLSTATE 55006; a bounded
//     retry rides it out.
//
// When Docker is unreachable (isDockerUnavailable) the helpers Skip so devs
// without Docker still pass the rest of the suite — preserving the old
// per-package skip behavior at the shared level.
package pgtest

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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

const (
	// containerName keys the shared, reused container. Slice 1's
	// scripts/test teardown removes it by this name after the run.
	containerName = "fishhawk-test-postgres"
	baseDB        = "fishhawk"
	baseUser      = "fishhawk"
	basePass      = "fishhawk"

	// templateDB is migrated once per container and used as the source
	// for each per-test CREATE DATABASE ... TEMPLATE.
	templateDB = "fishhawk_tmpl"

	// bootstrapLockKey serializes the template bootstrap across the
	// processes sharing the container. Advisory locks are cluster-wide,
	// so locking on the base database serializes every bootstrapper.
	bootstrapLockKey int64 = 1174

	attachAttempts     = 12
	attachRetryDelay   = 500 * time.Millisecond
	contentionAttempts = 12
	contentionDelay    = 250 * time.Millisecond
	startTimeout       = 120 * time.Second
)

var (
	sharedOnce sync.Once
	sharedBase string
	sharedErr  error
)

// NewURL returns the connection URL of a freshly-migrated, per-test
// ephemeral database on the single process-shared container. The database
// is dropped via t.Cleanup. Skips the test if Docker is unavailable.
func NewURL(t *testing.T) string {
	t.Helper()
	baseURL := sharedBaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect base db: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	dbName := "fh_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	create := func() error {
		_, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s",
			pgx.Identifier{dbName}.Sanitize(),
			pgx.Identifier{templateDB}.Sanitize()))
		return err
	}
	if err := createWithContentionRetry(contentionAttempts, contentionDelay, create); err != nil {
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

// NewPool returns a connected pgxpool against a freshly-migrated per-test
// database. The pool is closed (before the database is dropped) via
// t.Cleanup. Skips the test if Docker is unavailable.
func NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := NewURL(t)
	pool, err := postgres.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connect per-test pool: %v", err)
	}
	// Registered AFTER NewURL's drop cleanup, so it runs FIRST (LIFO):
	// the pool must close before the database can be dropped.
	t.Cleanup(pool.Close)
	return pool
}

// sharedBaseURL returns the base (admin) connection URL of the shared
// container, starting it on first use. Skips on Docker-unavailable or the
// FISHHAWK_SKIP_INTEGRATION override; fatals on any other start failure.
func sharedBaseURL(t *testing.T) string {
	t.Helper()
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		t.Skipf("FISHHAWK_SKIP_INTEGRATION set; skipping integration test")
	}
	base, err := sharedContainerBaseURL()
	if err != nil {
		failStart(t, err)
	}
	return base
}

func sharedContainerBaseURL() (string, error) {
	sharedOnce.Do(func() {
		sharedBase, sharedErr = startSharedContainer()
	})
	return sharedBase, sharedErr
}

func startSharedContainer() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	container, err := attachWithRetry(attachAttempts, attachRetryDelay, func() (*tcpostgres.PostgresContainer, error) {
		return tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase(baseDB),
			tcpostgres.WithUsername(baseUser),
			tcpostgres.WithPassword(basePass),
			tcpostgres.BasicWaitStrategies(),
			testcontainers.WithReuseByName(containerName),
		)
	})
	if err != nil {
		return "", err
	}
	// Intentionally NOT terminated: the container is shared across package
	// processes (ryuk disabled) and reaped by slice 1's scripts/test teardown.

	baseURL, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", fmt.Errorf("connection string: %w", err)
	}

	if err := bootstrapTemplate(ctx, baseURL); err != nil {
		return "", err
	}
	return baseURL, nil
}

// bootstrapTemplate creates and migrates the shared template database under
// a cluster-wide advisory lock, tolerating the cross-process duplicate.
func bootstrapTemplate(ctx context.Context, baseURL string) error {
	conn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("connect base db: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", bootstrapLockKey); err != nil {
		return fmt.Errorf("acquire bootstrap lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", bootstrapLockKey) }()

	createTemplate := func() error {
		_, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{templateDB}.Sanitize())
		return err
	}
	migrateTemplate := func() error {
		return postgres.MigrateUp(replaceDBName(baseURL, templateDB))
	}
	return bootstrapWith(createTemplate, migrateTemplate)
}

// bootstrapWith is the pure, injectable core of bootstrapTemplate: create
// the template, TOLERATING SQLSTATE 42P04 (duplicate_database) so a process
// that lost the cross-process race ADOPTS the already-created template,
// then run migrations (golang-migrate no-ops at head, so re-verifying an
// adopted template is safe).
func bootstrapWith(createTemplate, migrate func() error) error {
	if err := createTemplate(); err != nil {
		if !isDuplicateDatabase(err) {
			return fmt.Errorf("create template db: %w", err)
		}
		// Adopted an already-bootstrapped template (cross-process race).
	}
	if err := migrate(); err != nil {
		return fmt.Errorf("migrate template db: %w", err)
	}
	return nil
}

// attachWithRetry runs the container start, retrying ONLY on a docker
// name conflict (isContainerNameConflict) — the first-start race where a
// sibling process won the create. On retry, WithReuseByName re-resolves to
// the now-existing container, converging to a single shared handle. Any
// other error returns immediately.
func attachWithRetry[T any](attempts int, delay time.Duration, run func() (T, error)) (T, error) {
	var zero T
	var lastErr error
	for i := 0; i < attempts; i++ {
		v, err := run()
		if err == nil {
			return v, nil
		}
		if !isContainerNameConflict(err) {
			return zero, err
		}
		lastErr = err
		time.Sleep(delay)
	}
	return zero, fmt.Errorf("attach to shared container: name conflict persisted after %d attempts: %w", attempts, lastErr)
}

// createWithContentionRetry runs the per-test CREATE DATABASE ... TEMPLATE,
// retrying ONLY on SQLSTATE 55006 (isTemplate1Contention) — transient
// template-in-use contention under concurrency. Any other error returns
// immediately; exhausting the attempts returns a wrapped error.
func createWithContentionRetry(attempts int, delay time.Duration, create func() error) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		err := create()
		if err == nil {
			return nil
		}
		if !isTemplate1Contention(err) {
			return err
		}
		lastErr = err
		time.Sleep(delay)
	}
	return fmt.Errorf("create from template: contention (55006) persisted after %d attempts: %w", attempts, lastErr)
}

// fataler is the subset of *testing.T that failStart needs, so the skip vs
// fatal routing is unit-testable with a fake.
type fataler interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// failStart routes a shared-container start failure: Docker-unavailable
// skips (preserving the old per-package behavior), anything else fatals.
func failStart(tb fataler, err error) {
	tb.Helper()
	if isDockerUnavailable(err) {
		tb.Skipf("Docker not available; skipping integration test: %v", err)
		return
	}
	tb.Fatalf("start shared postgres: %v", err)
}

// replaceDBName rewrites the database path of a Postgres URL, preserving
// host, credentials, and query parameters.
func replaceDBName(baseURL, dbName string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	u.Path = "/" + dbName
	return u.String()
}

// isContainerNameConflict reports whether err is the docker "container
// name is already in use" conflict (HTTP 409) raised when a sibling test
// process won the concurrent create of the shared named container.
func isContainerNameConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "container name") && strings.Contains(msg, "already in use") {
		return true
	}
	return strings.Contains(msg, "status code 409") && strings.Contains(msg, "conflict")
}

// isDuplicateDatabase reports whether err carries SQLSTATE 42P04
// (duplicate_database) — the cross-process template bootstrap race.
func isDuplicateDatabase(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "42P04"
	}
	return false
}

// isTemplate1Contention reports whether err carries SQLSTATE 55006
// (object_in_use) — the template "is being accessed by other users"
// contention raised by concurrent CREATE DATABASE ... TEMPLATE.
func isTemplate1Contention(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "55006"
	}
	return false
}

// isDockerUnavailable reports whether err is the daemon-absent / cannot
// connect shape, so the helpers can Skip rather than Fatal.
func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
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
