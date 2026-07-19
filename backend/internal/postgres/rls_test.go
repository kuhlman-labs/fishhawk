package postgres_test

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

// pgInsufficientPrivilege is the SQLSTATE an RLS WITH CHECK violation
// raises ("new row violates row-level security policy").
const pgInsufficientPrivilege = "42501"

// rlsFixture is the shared setup for the RLS isolation proof: a migrated
// per-test database (via pgtest, so 0057's policies are live), seeded rows
// under two accounts plus untenanted NULL rows, and a purpose-created
// NON-superuser NOBYPASSRLS probe role. The probe role is the load-bearing
// part: the admin `fishhawk` role is a superuser AND the table owner, and
// superusers bypass RLS even under FORCE — run under it, every assertion
// below would spuriously pass with zero enforcement.
type rlsFixture struct {
	admin    *pgxpool.Pool // superuser fishhawk — seeds + verifies ground truth
	probe    *pgxpool.Pool // non-superuser NOBYPASSRLS — RLS engages here
	accountA uuid.UUID
	accountB uuid.UUID
	runA     uuid.UUID // account A's run
	runB     uuid.UUID // account B's run
	runNull  uuid.UUID // untenanted (account_id IS NULL) run
	stageA   uuid.UUID // stage under runA
	stageB   uuid.UUID // stage under runB
	stageN   uuid.UUID // stage under runNull
	sessA    uuid.UUID // session bound to account A
	sessB    uuid.UUID // session bound to account B
	sessN    uuid.UUID // untenanted session
}

func newRLSFixture(t *testing.T) *rlsFixture {
	t.Helper()
	dbURL := pgtest.NewURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	f := &rlsFixture{
		admin:    admin,
		accountA: uuid.New(),
		accountB: uuid.New(),
		runA:     uuid.New(),
		runB:     uuid.New(),
		runNull:  uuid.New(),
		stageA:   uuid.New(),
		stageB:   uuid.New(),
		stageN:   uuid.New(),
		sessA:    uuid.New(),
		sessB:    uuid.New(),
		sessN:    uuid.New(),
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO accounts (id, account_key) VALUES ($1, 'rls-account-a'), ($2, 'rls-account-b')`,
		f.accountA, f.accountB)
	seedRun := func(id uuid.UUID, account *uuid.UUID) {
		exec(`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, account_id)
		      VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'pending', 'local', $2)`, id, account)
	}
	seedRun(f.runA, &f.accountA)
	seedRun(f.runB, &f.accountB)
	seedRun(f.runNull, nil)
	seedStage := func(id, runID uuid.UUID) {
		exec(`INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		      VALUES ($1, $2, 0, 'implement', 'agent', 'claude-code', 'pending')`, id, runID)
	}
	seedStage(f.stageA, f.runA)
	seedStage(f.stageB, f.runB)
	seedStage(f.stageN, f.runNull)
	userID := uuid.New()
	exec(`INSERT INTO users (id, github_user_id, github_login, name) VALUES ($1, 909090, 'rls-user', 'RLS User')`, userID)
	seedSession := func(id uuid.UUID, account *uuid.UUID) {
		exec(`INSERT INTO sessions (id, user_id, token_hash, sliding_expires_at, absolute_expires_at, account_id)
		      VALUES ($1, $2, $3, now() + interval '1 day', now() + interval '7 days', $4)`,
			id, userID, "hash-"+id.String(), account)
	}
	seedSession(f.sessA, &f.accountA)
	seedSession(f.sessB, &f.accountB)
	seedSession(f.sessN, nil)

	// The probe role. CREATE ROLE is cluster-wide (not per-database), so the
	// name is uniqued per test and dropped via cleanup; DROP OWNED first
	// revokes its in-database grants so DROP ROLE cannot fail on dependency.
	role := "fh_rls_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	const probePassword = "fishhawk-rls-probe"
	exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS", role, probePassword))
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
	exec("GRANT USAGE ON SCHEMA public TO " + role)
	exec("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + role)

	probeURL, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse db url: %v", err)
	}
	probeURL.User = url.UserPassword(role, probePassword)
	probe, err := pgxpool.New(ctx, probeURL.String())
	if err != nil {
		t.Fatalf("connect probe pool: %v", err)
	}
	t.Cleanup(probe.Close)
	f.probe = probe

	// Premise guard: if the probe were superuser or BYPASSRLS, every
	// isolation assertion below would pass vacuously.
	var super, bypass bool
	if err := probe.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&super, &bypass); err != nil {
		t.Fatalf("read probe role attributes: %v", err)
	}
	if super || bypass {
		t.Fatalf("probe role rolsuper=%v rolbypassrls=%v, want false/false — RLS would not engage", super, bypass)
	}
	return f
}

// visibleIDs returns the id set a SELECT over table sees inside a
// WithTenant transaction on pool.
func visibleIDs(t *testing.T, pool *pgxpool.Pool, account, table string) map[uuid.UUID]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	got := map[uuid.UUID]bool{}
	if err := postgres.WithTenant(ctx, pool, account, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT id FROM "+table)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			got[id] = true
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("select %s under account %q: %v", table, account, err)
	}
	return got
}

// TestRLS_CrossAccountIsolation is the E44.6 headline proof (#1830): under a
// purpose-created non-superuser NOBYPASSRLS role, the 0057 policies refuse
// cross-account reads AND writes at the database, fail closed when no tenant
// is set, and keep untenanted (NULL-account) rows universally visible — while
// the superuser admin role demonstrably bypasses all of it (the documented
// reason this slice is inert in production until the runtime-role follow-up).
func TestRLS_CrossAccountIsolation(t *testing.T) {
	f := newRLSFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	accountA := f.accountA.String()
	accountB := f.accountB.String()

	t.Run("cross-account read refused, NULL rows visible under any account", func(t *testing.T) {
		underA := visibleIDs(t, f.probe, accountA, "runs")
		if underA[f.runB] {
			t.Error("account B's run visible under account A, want RLS-hidden")
		}
		if !underA[f.runA] || !underA[f.runNull] {
			t.Errorf("runs visible under account A = %v, want own run %s and untenanted run %s", underA, f.runA, f.runNull)
		}
		underB := visibleIDs(t, f.probe, accountB, "runs")
		if underB[f.runA] {
			t.Error("account A's run visible under account B, want RLS-hidden")
		}
		if !underB[f.runB] || !underB[f.runNull] {
			t.Errorf("runs visible under account B = %v, want own run %s and untenanted run %s", underB, f.runB, f.runNull)
		}
	})

	t.Run("unset GUC fails closed to untenanted rows only", func(t *testing.T) {
		got := visibleIDs(t, f.probe, "", "runs")
		if len(got) != 1 || !got[f.runNull] {
			t.Errorf("runs visible with no tenant set = %v, want exactly the untenanted run %s", got, f.runNull)
		}
	})

	t.Run("cross-account INSERT refused by WITH CHECK", func(t *testing.T) {
		// Positive control first: the same INSERT under the OWN account
		// succeeds, so the cross-account refusal below is the policy, not a
		// missing grant.
		ownID := uuid.New()
		if err := postgres.WithTenant(ctx, f.probe, accountA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, account_id)
				 VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'pending', 'local', $2)`, ownID, f.accountA)
			return err
		}); err != nil {
			t.Fatalf("own-account INSERT under account A failed (grants broken, refusal test would be vacuous): %v", err)
		}
		crossID := uuid.New()
		err := postgres.WithTenant(ctx, f.probe, accountA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, account_id)
				 VALUES ($1, 'r', 'feature_change', 'sha', 'cli', 'pending', 'local', $2)`, crossID, f.accountB)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgInsufficientPrivilege {
			t.Fatalf("cross-account INSERT error = %v, want SQLSTATE %s (row-level security WITH CHECK)", err, pgInsufficientPrivilege)
		}
		var n int
		if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM runs WHERE id = $1`, crossID).Scan(&n); err != nil {
			t.Fatalf("verify cross-account row absent: %v", err)
		}
		if n != 0 {
			t.Errorf("cross-account INSERT persisted %d row(s), want 0", n)
		}
	})

	t.Run("cross-account UPDATE refused", func(t *testing.T) {
		// Re-tenanting a visible row to another account trips WITH CHECK.
		err := postgres.WithTenant(ctx, f.probe, accountA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE runs SET account_id = $1 WHERE id = $2`, f.accountB, f.runA)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgInsufficientPrivilege {
			t.Fatalf("re-tenanting UPDATE error = %v, want SQLSTATE %s (row-level security WITH CHECK)", err, pgInsufficientPrivilege)
		}
		// An UPDATE targeting another account's row matches nothing: the row
		// is invisible under USING, so it is untouched rather than errored.
		if err := postgres.WithTenant(ctx, f.probe, accountA, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE runs SET repo = 'hijacked' WHERE id = $1`, f.runB)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("UPDATE of account B's run under account A affected %d row(s), want 0", tag.RowsAffected())
			}
			return nil
		}); err != nil {
			t.Fatalf("UPDATE targeting invisible row: %v", err)
		}
		var repo string
		if err := f.admin.QueryRow(ctx, `SELECT repo FROM runs WHERE id = $1`, f.runB).Scan(&repo); err != nil {
			t.Fatalf("verify account B's run unchanged: %v", err)
		}
		if repo != "r" {
			t.Errorf("account B's run repo after cross-account UPDATE = %q, want untouched \"r\"", repo)
		}
	})

	t.Run("stages scope via parent run subquery", func(t *testing.T) {
		underA := visibleIDs(t, f.probe, accountA, "stages")
		if underA[f.stageB] {
			t.Error("stage under account B's run visible under account A, want RLS-hidden")
		}
		if !underA[f.stageA] || !underA[f.stageN] {
			t.Errorf("stages visible under account A = %v, want own stage %s and untenanted-run stage %s", underA, f.stageA, f.stageN)
		}
		err := postgres.WithTenant(ctx, f.probe, accountA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
				 VALUES ($1, $2, 1, 'implement', 'agent', 'claude-code', 'pending')`, uuid.New(), f.runB)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgInsufficientPrivilege {
			t.Fatalf("INSERT stage under account B's run error = %v, want SQLSTATE %s (subquery WITH CHECK)", err, pgInsufficientPrivilege)
		}
	})

	t.Run("sessions scoped to admitting account", func(t *testing.T) {
		underA := visibleIDs(t, f.probe, accountA, "sessions")
		if underA[f.sessB] {
			t.Error("account B's session visible under account A, want RLS-hidden")
		}
		if !underA[f.sessA] || !underA[f.sessN] {
			t.Errorf("sessions visible under account A = %v, want own session %s and untenanted session %s", underA, f.sessA, f.sessN)
		}
	})

	t.Run("superuser bypasses RLS even under FORCE", func(t *testing.T) {
		// The documented production reality (#1830 follow-up): the runtime's
		// superuser role sees every row regardless of tenant, which is why
		// this slice is inert in production until the runtime-role switch.
		got := visibleIDs(t, f.admin, accountA, "runs")
		for _, id := range []uuid.UUID{f.runA, f.runB, f.runNull} {
			if !got[id] {
				t.Errorf("run %s invisible to superuser under account A — expected full bypass", id)
			}
		}
	})
}
