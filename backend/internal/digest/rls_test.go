package digest

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

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

const pgInsufficientPrivilege = "42501"

// TestDigest_RLSIsolation proves 0089's captain_read_watermarks policy at the
// database, through the Store, under a purpose-created NOSUPERUSER NOBYPASSRLS
// probe role — the admin role is a superuser and bypasses RLS even under FORCE.
//
// MECHANISM: account B's watermark row exists BEFORE the probe reads (seeded
// through the admin pool). COUNTERFACTUAL, run IN the test (#3734 approval
// condition 5): FORCE RLS with NO policy denies everything, so dropping the
// policy would prove nothing about exposure. Instead the policy is replaced
// with USING (true) and the SAME probe read must then see account B's row —
// which is what makes the isolation assertion above it a statement about the
// policy rather than about an empty table.
func TestDigest_RLSIsolation(t *testing.T) {
	dbURL := pgtest.NewURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)
	accountA, accountB := uuid.New(), uuid.New()
	if _, err := admin.Exec(ctx, `INSERT INTO accounts (id, account_key) VALUES ($1, 'rls-dg-a'), ($2, 'rls-dg-b')`, accountA, accountB); err != nil {
		t.Fatal(err)
	}
	adminStore := NewStore(admin)
	if err := adminStore.UpsertWatermark(ctx, &accountA, "cap", testRepo, 11); err != nil {
		t.Fatal(err)
	}
	if err := adminStore.UpsertWatermark(ctx, &accountB, "cap", testRepo, 22); err != nil {
		t.Fatal(err)
	}
	probe := newProbePool(t, ctx, admin, dbURL)

	visible := func() (own, other bool, rows int) {
		t.Helper()
		if err := postgres.WithTenant(ctx, probe, accountA.String(), func(tx pgx.Tx) error {
			s := NewStore(tx)
			var err error
			if _, own, err = s.GetWatermark(ctx, &accountA, "cap", testRepo); err != nil {
				return err
			}
			if _, other, err = s.GetWatermark(ctx, &accountB, "cap", testRepo); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM captain_read_watermarks`).Scan(&rows)
		}); err != nil {
			t.Fatalf("probe read under account A: %v", err)
		}
		return own, other, rows
	}
	own, other, rows := visible()
	if !own || other || rows != 1 {
		t.Errorf("probe under account A: own=%v other=%v rows=%d, want own only (1 row)", own, other, rows)
	}

	// A write of account B's row from account A's session is refused.
	err = postgres.WithTenant(ctx, probe, accountA.String(), func(tx pgx.Tx) error {
		return NewStore(tx).UpsertWatermark(ctx, &accountB, "intruder", testRepo, 5)
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgInsufficientPrivilege {
		t.Errorf("cross-account insert err = %v, want SQLSTATE %s", err, pgInsufficientPrivilege)
	}

	// In-test counterfactual: a permissive policy exposes account B's row to
	// the same probe read.
	if _, err := admin.Exec(ctx, `ALTER POLICY captain_read_watermarks_tenant_isolation ON captain_read_watermarks
		USING (true) WITH CHECK (true)`); err != nil {
		t.Fatal(err)
	}
	if _, other, rows := visible(); !other || rows != 2 {
		t.Errorf("under USING (true) the probe sees other=%v rows=%d, want account B's row visible — the fixture does not discriminate", other, rows)
	}
}

// newProbePool creates a per-test NOSUPERUSER NOBYPASSRLS login role and
// returns a pool connected as it (the decisionindex rls_test shape).
func newProbePool(t *testing.T, ctx context.Context, admin *pgxpool.Pool, dbURL string) *pgxpool.Pool {
	t.Helper()
	role := "fh_dg_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	const password = "fishhawk-dg-probe"
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
