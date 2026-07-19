package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// recordingExecer records the SQL SetTenant emits, optionally failing every
// Exec, so the unit tests below run without a database.
type recordingExecer struct {
	sqls []string
	err  error
}

func (r *recordingExecer) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	r.sqls = append(r.sqls, sql)
	return pgconn.CommandTag{}, r.err
}

func TestSetTenant_EmitsSetLocal(t *testing.T) {
	rec := &recordingExecer{}
	accountID := uuid.New()
	if err := postgres.SetTenant(context.Background(), rec, accountID.String()); err != nil {
		t.Fatalf("SetTenant: %v", err)
	}
	if len(rec.sqls) != 1 {
		t.Fatalf("SetTenant emitted %d statements, want 1: %v", len(rec.sqls), rec.sqls)
	}
	want := "SET LOCAL app.account_id = '" + accountID.String() + "'"
	if rec.sqls[0] != want {
		t.Errorf("SetTenant emitted %q, want %q", rec.sqls[0], want)
	}
}

// An empty account id is the fail-closed no-op: nothing is emitted, leaving
// the GUC unset so the 0057 policies admit only NULL-account rows.
func TestSetTenant_EmptyAccountIsNoOp(t *testing.T) {
	rec := &recordingExecer{}
	if err := postgres.SetTenant(context.Background(), rec, ""); err != nil {
		t.Fatalf("SetTenant with empty account: %v", err)
	}
	if len(rec.sqls) != 0 {
		t.Errorf("SetTenant with empty account emitted %v, want no statements", rec.sqls)
	}
}

// A non-UUID account id is rejected BEFORE any SQL is emitted — the guard
// that makes inlining the value into SET LOCAL (which cannot take bind
// parameters) injection-safe.
func TestSetTenant_RejectsNonUUID(t *testing.T) {
	rec := &recordingExecer{}
	err := postgres.SetTenant(context.Background(), rec, "'; DROP TABLE runs; --")
	if err == nil {
		t.Fatal("SetTenant with non-UUID account id returned nil, want error")
	}
	if len(rec.sqls) != 0 {
		t.Errorf("SetTenant with non-UUID account id emitted %v, want no statements", rec.sqls)
	}
}

func TestSetTenant_PropagatesExecError(t *testing.T) {
	sentinel := errors.New("exec failed")
	rec := &recordingExecer{err: sentinel}
	err := postgres.SetTenant(context.Background(), rec, uuid.NewString())
	if !errors.Is(err, sentinel) {
		t.Errorf("SetTenant error = %v, want wrapped %v", err, sentinel)
	}
}

// TestWithTenant_ScopesTransactionAndCommits proves the wrapper contract
// end-to-end on a real database: inside fn the GUC carries the account id,
// fn's writes commit, and the SET LOCAL reverts with the transaction so the
// pooled connection does not leak the tenant into later queries.
func TestWithTenant_ScopesTransactionAndCommits(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	accountID := uuid.NewString()
	userID := uuid.New()
	if err := postgres.WithTenant(ctx, pool, accountID, func(tx pgx.Tx) error {
		var got string
		if err := tx.QueryRow(ctx, "SELECT current_setting('app.account_id', true)").Scan(&got); err != nil {
			return err
		}
		if got != accountID {
			t.Errorf("app.account_id inside WithTenant = %q, want %q", got, accountID)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO users (id, github_user_id, github_login, name) VALUES ($1, 424242, 'tenant-user', 'Tenant User')`,
			userID)
		return err
	}); err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	// fn's write committed.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count committed user: %v", err)
	}
	if n != 1 {
		t.Errorf("committed user count after WithTenant = %d, want 1", n)
	}
	// The SET LOCAL reverted with the transaction: outside it the GUC is
	// unset (NULL) or the empty placeholder — never the account id.
	var after *string
	if err := pool.QueryRow(ctx, "SELECT current_setting('app.account_id', true)").Scan(&after); err != nil {
		t.Fatalf("read app.account_id after WithTenant: %v", err)
	}
	if after != nil && *after == accountID {
		t.Errorf("app.account_id after WithTenant = %q, want reverted (SET LOCAL leaked past commit)", *after)
	}
}

// A failing fn rolls the transaction back: no partial write survives.
func TestWithTenant_RollsBackOnFnError(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sentinel := errors.New("fn failed")
	userID := uuid.New()
	err := postgres.WithTenant(ctx, pool, uuid.NewString(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, github_user_id, github_login, name) VALUES ($1, 424243, 'rollback-user', 'Rollback User')`,
			userID); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTenant error = %v, want %v", err, sentinel)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count rolled-back user: %v", err)
	}
	if n != 0 {
		t.Errorf("user count after failed WithTenant = %d, want 0 (rolled back)", n)
	}
}

// An invalid account id fails before fn runs — the transaction never
// executes tenant-scoped work under a garbage tenant.
func TestWithTenant_InvalidAccountSkipsFn(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fnCalled := false
	err := postgres.WithTenant(ctx, pool, "not-a-uuid", func(pgx.Tx) error {
		fnCalled = true
		return nil
	})
	if err == nil {
		t.Fatal("WithTenant with non-UUID account returned nil, want error")
	}
	if !strings.Contains(err.Error(), "not a UUID") {
		t.Errorf("WithTenant error = %v, want the SetTenant not-a-UUID rejection", err)
	}
	if fnCalled {
		t.Error("fn ran despite invalid account id, want skipped")
	}
}
