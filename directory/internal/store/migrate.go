package store

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	// Side-effect import: registers the "pgx5" driver scheme with
	// golang-migrate. normalizeDatabaseURL maps the standard
	// "postgres://" prefix onto "pgx5://" before handing it off, so
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

// MigrateUp applies all pending migrations against the given Postgres URL.
// Idempotent: when no pending migrations exist it returns nil.
func MigrateUp(databaseURL string) error {
	m, err := openMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// MigrateDown rolls back the most recent migration step. Intended only for
// local dev (production uses forward-only migrations per ADR-006).
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

func openMigrator(databaseURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(Migrations(), ".")
	if err != nil {
		return nil, fmt.Errorf("init source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, normalizeDatabaseURL(databaseURL))
	if err != nil {
		return nil, fmt.Errorf("open migrate: %w", err)
	}
	return m, nil
}

// normalizeDatabaseURL maps the conventional postgres:// scheme onto the
// pgx5:// scheme that the imported database driver registers. Other schemes
// pass through unchanged.
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
