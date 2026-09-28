package decisionindex

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

const pgInsufficientPrivilege = "42501"

// TestDecisionIndex_RLSIsolation proves 0088's tenant policy at the database,
// through the Store, under a purpose-created NOSUPERUSER NOBYPASSRLS probe
// role — the admin fishhawk role is a superuser and bypasses RLS even under
// FORCE, so a test run under it would pass with zero enforcement.
//
// MECHANISM: account B's row exists BEFORE the probe reads (seeded through the
// admin pool), so absent the USING predicate the same List returns both rows.
// COUNTERFACTUAL: replace the policy's USING/WITH CHECK with (true) → the
// read assertion and the 42501 assertion both go RED.
func TestDecisionIndex_RLSIsolation(t *testing.T) {
	dbURL := pgtest.NewURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)
	f := &chainFixture{pool: admin, repo: audit.NewPostgresRepository(admin),
		accountA: uuid.New(), accountB: uuid.New(), runA: uuid.New(), runB: uuid.New()}
	f.exec(t, `INSERT INTO accounts (id, account_key) VALUES ($1, 'rls-di-a'), ($2, 'rls-di-b')`, f.accountA, f.accountB)
	f.seedRun(t, f.runA, "acme/widgets", "sha-a", &f.accountA)
	f.seedRun(t, f.runB, "acme/gadgets", "sha-b", &f.accountB)

	rowA := sampleRow(f.runA, 11)
	rowA.AccountID = &f.accountA
	rowB := sampleRow(f.runB, 12)
	rowB.AccountID = &f.accountB
	adminStore := NewStore(admin)
	for _, r := range []Row{rowA, rowB} {
		if err := adminStore.Upsert(ctx, r); err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}

	probe := newProbePool(t, ctx, admin, dbURL)

	var got []Row
	if err := postgres.WithTenant(ctx, probe, f.accountA.String(), func(tx pgx.Tx) error {
		var err error
		got, err = NewStore(tx).List(ctx, ListFilter{})
		return err
	}); err != nil {
		t.Fatalf("list under account A: %v", err)
	}
	if len(got) != 1 || got[0].SourceSequence != rowA.SourceSequence {
		t.Errorf("probe under account A sees %v, want only [%d]", seqsOf(got), rowA.SourceSequence)
	}

	// A write of account B's row from account A's session is refused.
	intruder := sampleRow(f.runB, 13)
	intruder.AccountID = &f.accountB
	err = postgres.WithTenant(ctx, probe, f.accountA.String(), func(tx pgx.Tx) error {
		return NewStore(tx).Upsert(ctx, intruder)
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgInsufficientPrivilege {
		t.Errorf("cross-account insert err = %v, want SQLSTATE %s", err, pgInsufficientPrivilege)
	}
}

// newProbePool creates a per-test NOSUPERUSER NOBYPASSRLS login role and
// returns a pool connected as it (the internal/postgres newRLSFixture shape).
func newProbePool(t *testing.T, ctx context.Context, admin *pgxpool.Pool, dbURL string) *pgxpool.Pool {
	t.Helper()
	role := "fh_di_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	const password = "fishhawk-di-probe"
	for _, sql := range []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS", role, password),
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + role,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, dbURL)
		if err != nil {
			return // best-effort: the per-test database is dropped regardless
		}
		defer func() { _ = c.Close(ctx) }()
		_, _ = c.Exec(ctx, "DROP OWNED BY "+role)
		_, _ = c.Exec(ctx, "DROP ROLE IF EXISTS "+role)
	})
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	probe, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect probe: %v", err)
	}
	t.Cleanup(probe.Close)

	// Premise guard: a superuser or BYPASSRLS probe would pass vacuously.
	var super, bypass bool
	if err := probe.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("probe rolsuper=%v rolbypassrls=%v, want false/false", super, bypass)
	}
	return probe
}
