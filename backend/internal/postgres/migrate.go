package postgres

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	// Side-effect import: registers the "pgx5" driver scheme with
	// golang-migrate. normalizeDatabaseURL maps the standard
	// "postgres://" prefix onto "pgx5://" before handing it off so
	// callers don't need to know about the driver-specific scheme.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns the embedded migration filesystem rooted at
// "migrations". Exported so tests can drive migrations against a
// throwaway database without spawning a binary.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		// Unreachable: embed paths are resolved at compile time.
		panic(err)
	}
	return sub
}

// MigrateUp applies all pending migrations against the given Postgres
// URL. Idempotent: when no pending migrations exist it returns nil.
//
// Accepts the standard postgres:// URL scheme; rewritten internally
// to the pgx5:// scheme that the driver registers.
func MigrateUp(databaseURL string) error {
	return MigrateUpFS(Migrations(), databaseURL)
}

// MigrateUpFS is MigrateUp against an explicit migration filesystem —
// the seam that lets a test drive the REAL migration path (including
// the dirty-state enrichment below) against a synthetic migration set,
// rather than re-implementing it. Production callers use MigrateUp,
// which passes the embedded Migrations().
//
// It is exported rather than unexported because the package's
// migration tests live in the EXTERNAL postgres_test package (they
// need a raw, un-migrated throwaway database, the AGENTS.md-recorded
// exemption to the shared pgtest container), which cannot reach an
// unexported identifier.
func MigrateUpFS(fsys fs.FS, databaseURL string) error {
	m, err := openMigratorFS(fsys, databaseURL)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", enrichDirtyError(err))
	}
	return nil
}

// enrichDirtyError replaces golang-migrate's bare "Dirty database
// version N. Fix and force version." with an actionable message naming
// the dirty version AND the recovery, so an operator reading a failed
// migrate Job's logs is not left to infer the procedure. Any other
// error passes through unchanged.
//
// The dirty FLAG itself is golang-migrate's behaviour, not this
// package's: before running a migration it records (version, dirty) and
// clears the flag only on success, so a failed migration leaves the
// marker that makes the NEXT run refuse rather than proceed over a
// half-migrated schema. This function is the only part of that path
// this package owns.
func enrichDirtyError(err error) error {
	var dirty migrate.ErrDirty
	if !errors.As(err, &dirty) {
		return err
	}
	return fmt.Errorf(
		"database is marked dirty at version %d: a previous migration failed part-way and the schema was left unverified. "+
			"Migrations will REFUSE to run until this is resolved — no release proceeds over a half-migrated schema. "+
			"Recovery: inspect the failed migration, repair the schema by hand if needed, then clear the marker with "+
			"`migrate -path <migrations> -database <url> force %d` (force the version you have actually applied) and re-run migrate up: %w",
		dirty.Version, dirty.Version, err)
}

// MigrateDown rolls back the most recent migration step. Intended
// only for local dev (production uses forward-only migrations per
// ADR-006).
func MigrateDown(databaseURL string) error {
	m, err := openMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Steps(-1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// MigrateVersion reports the migration version golang-migrate has
// recorded against the given Postgres URL, and whether that version is
// dirty (a previous migration failed part-way — see enrichDirtyError).
// It is the read primitive that lets a rollback test NAME its target
// migration instead of counting steps from the current tip (#2815): a
// test reads the live version, computes how many steps down its target
// is, and asserts it landed there — so a new migration landing above
// the target shifts the tip without editing the test.
//
// A fresh database with no migration applied reports (0, false, nil):
// golang-migrate returns migrate.ErrNilVersion in that case, which this
// maps to version 0. That mapping is unambiguous ONLY because the
// migration series starts at 0001, so version 0 can never be a real
// applied migration; TestMigrations_EmbeddedFiles asserts that invariant.
//
// Exported for the same reason as MigrateUpFS: the package's migration
// tests live in the external postgres_test package and cannot reach an
// unexported identifier.
func MigrateVersion(databaseURL string) (uint, bool, error) {
	m, err := openMigrator(databaseURL)
	if err != nil {
		return 0, false, err
	}
	defer func() { _, _ = m.Close() }()
	version, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migrate version: %w", err)
	}
	return version, dirty, nil
}

func openMigrator(databaseURL string) (*migrate.Migrate, error) {
	return openMigratorFS(Migrations(), databaseURL)
}

func openMigratorFS(fsys fs.FS, databaseURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("init source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, normalizeDatabaseURL(databaseURL))
	if err != nil {
		return nil, fmt.Errorf("open migrate: %w", err)
	}
	return m, nil
}

// normalizeDatabaseURL maps the conventional postgres:// scheme onto
// the pgx5:// scheme that the imported database driver registers.
// Other schemes pass through unchanged.
func normalizeDatabaseURL(url string) string {
	const (
		std = "postgres://"
		alt = "postgresql://"
	)
	switch {
	case strings.HasPrefix(url, std):
		return "pgx5://" + url[len(std):]
	case strings.HasPrefix(url, alt):
		return "pgx5://" + url[len(alt):]
	default:
		return url
	}
}
