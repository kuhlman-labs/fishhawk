package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres for these tests is routed by resolveBase (testdb_test.go), three
// ways: FISHHAWK_TEST_PG_URL set means that server is the base and no
// container is started (the runner's gate container, #2137);
// FISHHAWK_GATE_CONTAINER=1 without a URL is a Fatalf naming the runner-side
// remedy, never a skip; otherwise this package starts its own unshared
// testcontainers Postgres, terminated explicitly in TestMain. See
// testdb_test.go for why it cannot reuse backend/internal/pgtest.

var (
	baseURL     string
	skipReason  string
	fatalReason string
	terminateFn func()
)

func TestMain(m *testing.M) {
	code := func() int {
		defer func() {
			if terminateFn != nil {
				terminateFn()
			}
		}()
		setup()
		return m.Run()
	}()
	os.Exit(code)
}

func setup() {
	res, err := resolveBase(os.Getenv, startContainer)
	// Assigned BEFORE the panic: TestMain's deferred terminate runs while the
	// panic unwinds, so a container that started and then failed is reaped.
	terminateFn = res.terminate
	if err != nil {
		panic(fmt.Sprintf("start directory test postgres: %v", err))
	}
	baseURL, skipReason, fatalReason = res.url, res.skip, res.fatal
}

// newStore returns a Store over a freshly-migrated throwaway database.
func newStore(t *testing.T) *Store {
	t.Helper()
	dbURL := newTestDatabase(t)

	ctx := context.Background()
	if err := MigrateUp(dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect per-test pool: %v", err)
	}
	// Registered AFTER newTestDatabase's drop cleanup, so LIFO closes the
	// pool before the database is dropped.
	t.Cleanup(pool.Close)

	return New(pool)
}

func TestAssignRegionThenLookup(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	got, err := s.AssignRegion(ctx, "github", "acme", "us-east")
	if err != nil {
		t.Fatalf("AssignRegion: %v", err)
	}
	if got != "us-east" {
		t.Fatalf("assigned region: got %q want %q", got, "us-east")
	}

	looked, err := s.Lookup(ctx, "github", "acme")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if looked != "us-east" {
		t.Fatalf("looked up region: got %q want %q", looked, "us-east")
	}
}

// A second assignment naming a DIFFERENT region must not move the account:
// the first write wins and the caller is told who actually owns it.
func TestAssignRegionFirstWriteWins(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.AssignRegion(ctx, "github", "acme", "us-east"); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	got, err := s.AssignRegion(ctx, "github", "acme", "eu-west")
	if err != nil {
		t.Fatalf("second assign: %v", err)
	}
	if got != "us-east" {
		t.Fatalf("second assign returned %q, want the incumbent %q", got, "us-east")
	}

	looked, err := s.Lookup(ctx, "github", "acme")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if looked != "us-east" {
		t.Fatalf("persisted region moved to %q", looked)
	}
}

// Re-assigning the SAME region is idempotent — the routing path calls
// AssignRegion on every request, so this is the common case.
func TestAssignRegionIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		got, err := s.AssignRegion(ctx, "github", "acme", "us-east")
		if err != nil {
			t.Fatalf("assign %d: %v", i, err)
		}
		if got != "us-east" {
			t.Fatalf("assign %d returned %q", i, got)
		}
	}
}

// The load-bearing property: under genuine concurrency, with every caller
// proposing a DIFFERENT region, exactly one region is persisted and every
// caller observes that same winner. A sequential test does not discharge
// this — run with -race.
func TestAssignRegionConcurrentSingleWinner(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const n = 16
	results := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = s.AssignRegion(ctx, "github", "acme", fmt.Sprintf("region-%02d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	winner := ""
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if winner == "" {
			winner = results[i]
			continue
		}
		if results[i] != winner {
			t.Fatalf("callers disagree on the winner: %q vs %q", results[i], winner)
		}
	}
	if !strings.HasPrefix(winner, "region-") {
		t.Fatalf("winner %q is not one of the proposed regions", winner)
	}

	looked, err := s.Lookup(ctx, "github", "acme")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if looked != winner {
		t.Fatalf("persisted region %q differs from the region callers were told (%q)", looked, winner)
	}
}

// Accounts are keyed by (provider, account_key): the same key under a
// different provider is a different account.
func TestAssignRegionIsPerProviderAndKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.AssignRegion(ctx, "github", "acme", "us-east"); err != nil {
		t.Fatalf("assign github/acme: %v", err)
	}
	got, err := s.AssignRegion(ctx, "gitlab", "acme", "eu-west")
	if err != nil {
		t.Fatalf("assign gitlab/acme: %v", err)
	}
	if got != "eu-west" {
		t.Fatalf("gitlab/acme got %q, want eu-west", got)
	}
	got, err = s.AssignRegion(ctx, "github", "other", "eu-west")
	if err != nil {
		t.Fatalf("assign github/other: %v", err)
	}
	if got != "eu-west" {
		t.Fatalf("github/other got %q, want eu-west", got)
	}
}

func TestLookupUnknownAccount(t *testing.T) {
	s := newStore(t)
	_, err := s.Lookup(context.Background(), "github", "never-assigned")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestAssignRegionRejectsEmptyInput(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for _, tc := range []struct{ name, provider, key, region string }{
		{"provider", "", "acme", "us-east"},
		{"account_key", "github", "", "us-east"},
		{"region", "github", "acme", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.AssignRegion(ctx, tc.provider, tc.key, tc.region); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestLookupRejectsEmptyInput(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for _, tc := range []struct{ name, provider, key string }{
		{"provider", "", "acme"},
		{"account_key", "github", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Lookup(ctx, tc.provider, tc.key); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
		})
	}
}

// Pure, Docker-free: the scheme rewrite golang-migrate's pgx5 driver needs.
// A missed rewrite surfaces as "unknown driver postgres" at migrate time.
func TestNormalizeDatabaseURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"postgres://u:p@h:5432/db?sslmode=disable", "pgx5://u:p@h:5432/db?sslmode=disable"},
		{"postgresql://u:p@h:5432/db", "pgx5://u:p@h:5432/db"},
		{"pgx5://u:p@h:5432/db", "pgx5://u:p@h:5432/db"},
		{"", ""},
	} {
		if got := normalizeDatabaseURL(tc.in); got != tc.want {
			t.Errorf("normalizeDatabaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Pure, Docker-free: the embed directive must actually carry the SQL. A
// mis-rooted //go:embed yields an empty FS and migrations silently no-op.
func TestMigrationsAreEmbedded(t *testing.T) {
	entries, err := fs.ReadDir(Migrations(), ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	want := map[string]bool{
		"0001_directory.up.sql":   false,
		"0001_directory.down.sql": false,
	}
	for _, e := range entries {
		if _, ok := want[e.Name()]; ok {
			want[e.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("embedded migrations missing %s (got %v)", name, entries)
		}
	}
}

// The down migration must actually roll the schema back — an untested
// down leaves local dev with no way out of a bad head.
func TestMigrateDownDropsTable(t *testing.T) {
	ctx := context.Background()
	dbURL := newTestDatabase(t)

	if err := MigrateUp(dbURL); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	if err := MigrateDown(dbURL); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect per-test db: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('account_regions') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if exists {
		t.Fatal("account_regions still exists after MigrateDown")
	}
}
