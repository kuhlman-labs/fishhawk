package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	// Side-effect import: registers the "pgx5" driver scheme with
	// golang-migrate. normalizeDatabaseURL maps "postgres://" onto
	// "pgx5://" so callers never need the driver-specific scheme.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns the embedded migration filesystem rooted at
// "migrations". Exported so tests can migrate a throwaway database
// without spawning the binary.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		// Unreachable: embed paths resolve at compile time.
		panic(err)
	}
	return sub
}

// MigrateUp applies all pending directory migrations. Idempotent.
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

// MigrateDown rolls back the most recent migration step (dev only).
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
// pgx5:// scheme the imported driver registers. Other schemes pass
// through unchanged.
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

// Connect opens a pool against the directory database and pings it.
// Closing the pool is the caller's responsibility.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	// The directory does one tiny read per redirect; a small pool is
	// plenty and keeps the global plane's footprint minimal.
	cfg.MaxConns = 5
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
