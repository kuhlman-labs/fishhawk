package crewmessage

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

const crewPgInsufficientPrivilege = "42501"

// TestCrewMessages_RLSIsolation is control C6: 0090's tenant policy, proven at
// the database through the Store under a purpose-created NOSUPERUSER
// NOBYPASSRLS probe role — the admin fishhawk role is a superuser and bypasses
// RLS even under FORCE, so a test run under it would pass with zero
// enforcement.
//
// MECHANISM: account B's row exists BEFORE the probe reads (seeded through the
// admin pool), so absent the USING predicate the same ListByRecipient returns
// both rows. An untenanted (NULL-account) row is seeded too: the predicate
// keeps it visible (the #1829 window) and an unset app.account_id sees ONLY
// it. COUNTERFACTUAL: replace the policy's USING/WITH CHECK with (true) → the
// read assertions and the 42501 assertion all go RED.
func TestCrewMessages_RLSIsolation(t *testing.T) {
	dbURL := pgtest.NewURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)

	accountA, accountB := uuid.New(), uuid.New()
	rowA, rowB, rowNull := openIssueRow(11), openIssueRow(12), openIssueRow(13)
	rowA.AccountID = &accountA
	rowB.AccountID = &accountB
	mustUpsert(t, NewStore(admin), rowA, rowB, rowNull)

	probe := newCrewProbePool(t, ctx, admin, dbURL)

	read := func(account string) []int64 {
		t.Helper()
		var got []Row
		if err := postgres.WithTenant(ctx, probe, account, func(tx pgx.Tx) error {
			var err error
			got, err = NewStore(tx).ListByRecipient(ctx, ListFilter{})
			return err
		}); err != nil {
			t.Fatalf("list under account %q: %v", account, err)
		}
		return sentSeqsOf(got)
	}
	if got := read(accountA.String()); fmt.Sprint(got) != "[11 13]" {
		t.Errorf("probe under account A sees %v, want [11 13] (own row + untenanted)", got)
	}
	if got := read(accountB.String()); fmt.Sprint(got) != "[12 13]" {
		t.Errorf("probe under account B sees %v, want [12 13]", got)
	}
	if got := read(""); fmt.Sprint(got) != "[13]" {
		t.Errorf("probe with no account sees %v, want [13] (fail closed to untenanted rows)", got)
	}

	// Get is filtered too: A cannot read B's row by its key.
	if err := postgres.WithTenant(ctx, probe, accountA.String(), func(tx pgx.Tx) error {
		_, err := NewStore(tx).Get(ctx, rowB.SentSequence)
		return err
	}); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("Get(B's row) under account A err = %v, want ErrMessageNotFound", err)
	}

	// A write of account B's row from account A's session is refused.
	intruder := openIssueRow(14)
	intruder.AccountID = &accountB
	err = postgres.WithTenant(ctx, probe, accountA.String(), func(tx pgx.Tx) error {
		return NewStore(tx).Upsert(ctx, intruder)
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != crewPgInsufficientPrivilege {
		t.Errorf("cross-account insert err = %v, want SQLSTATE %s", err, crewPgInsufficientPrivilege)
	}
}

// newCrewProbePool creates a per-test NOSUPERUSER NOBYPASSRLS login role and
// returns a pool connected as it (the decisionindex newProbePool shape).
func newCrewProbePool(t *testing.T, ctx context.Context, admin *pgxpool.Pool, dbURL string) *pgxpool.Pool {
	t.Helper()
	role := "fh_cm_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	const password = "fishhawk-cm-probe"
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
