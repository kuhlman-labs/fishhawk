package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantGUC is the per-transaction setting the 0057 row-level-security
// policies read via current_setting('app.account_id', true). SetTenant is
// the only writer; the policies fail closed to NULL-account rows when it
// is unset (or left as the empty string a reverted SET LOCAL leaves on a
// pooled session).
const TenantGUC = "app.account_id"

// Execer is the narrow slice of pgx.Tx that SetTenant needs, split out so
// tests can record the emitted SQL without a live transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SetTenant scopes the current transaction to accountID by issuing
// SET LOCAL app.account_id. SET LOCAL is transaction-scoped: it reverts at
// COMMIT/ROLLBACK, so every tenant-scoped query must run inside the same
// transaction — use WithTenant unless you already hold one.
//
// An empty accountID is a deliberate no-op: with the GUC unset the 0057
// policies fail closed to NULL-account (untenanted) rows only, which is the
// correct view for an unresolved identity. A non-empty accountID must be a
// UUID; anything else is rejected before touching the database (SET LOCAL
// cannot take bind parameters, so the value is inlined only after
// round-tripping through uuid.Parse).
func SetTenant(ctx context.Context, tx Execer, accountID string) error {
	if accountID == "" {
		return nil
	}
	id, err := uuid.Parse(accountID)
	if err != nil {
		return fmt.Errorf("set tenant: account id %q is not a UUID: %w", accountID, err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL "+TenantGUC+" = '"+id.String()+"'"); err != nil {
		return fmt.Errorf("set tenant: %w", err)
	}
	return nil
}

// WithTenant runs fn inside a transaction scoped to accountID: it begins a
// transaction on pool, applies SetTenant, runs fn, and commits — rolling
// back if SetTenant or fn fails. This is the per-request wrapper
// tenant-scoped read/write paths adopt as E44 proceeds.
//
// Cross-account system work (reconcilers, tickers) must NOT iterate
// WithTenant per account; it belongs on a BYPASSRLS system context, which is
// part of the deferred runtime-role follow-up to #1830 (today the runtime's
// superuser role bypasses RLS wholesale).
func WithTenant(ctx context.Context, pool *pgxpool.Pool, accountID string, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("with tenant: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := SetTenant(ctx, tx, accountID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("with tenant: commit: %w", err)
	}
	return nil
}
