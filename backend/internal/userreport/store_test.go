package userreport

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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

var (
	t1 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
)

func issuesKey(repo string) Key { return Key{Repo: repo, Source: SourceIssues} }

// rawCursor reads the row straight from the table, bypassing the Store, so a
// test asserts COMMITTED state rather than what a Store method returned.
func rawCursor(t *testing.T, pool *pgxpool.Pool, key Key) (time.Time, bool) {
	t.Helper()
	var at time.Time
	err := pool.QueryRow(context.Background(), `SELECT cursor_at FROM user_report_cursors
		WHERE repo = $1 AND source = $2 AND account_id IS NOT DISTINCT FROM $3`, key.Repo, string(key.Source), key.AccountID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("raw read: %v", err)
	}
	return at, true
}

// TestStore_GetAbsentThenInitThenAdvance walks one row's lifecycle: absent,
// initialised, a second Init returning the STORED value (insert-if-absent),
// then advanced.
func TestStore_GetAbsentThenInitThenAdvance(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	key := issuesKey("acme/widgets")

	if _, found, err := s.Get(ctx, key); err != nil || found {
		t.Fatalf("Get on empty = (found %v, %v), want absent", found, err)
	}
	if got, err := s.Init(ctx, key, t1); err != nil || !got.Equal(t1) {
		t.Fatalf("first Init = (%v, %v), want %v", got, err, t1)
	}
	if got, err := s.Init(ctx, key, t2); err != nil || !got.Equal(t1) {
		t.Fatalf("second Init = (%v, %v), want the STORED %v, not the new candidate", got, err, t1)
	}
	if at, _ := rawCursor(t, pool, key); !at.Equal(t1) {
		t.Fatalf("row after second Init = %v, want %v untouched", at, t1)
	}
	if got, found, err := s.Get(ctx, key); err != nil || !found || !got.Equal(t1) {
		t.Fatalf("Get = (%v, %v, %v), want %v", got, found, err, t1)
	}
	committed, advanced, err := s.Advance(ctx, key, t2)
	if err != nil || !advanced || !committed.Equal(t2) {
		t.Fatalf("Advance = (%v, %v, %v), want %v advanced", committed, advanced, err, t2)
	}
	// Advance on an absent row inserts it.
	other := issuesKey("acme/other")
	if committed, advanced, err := s.Advance(ctx, other, t1); err != nil || !advanced || !committed.Equal(t1) {
		t.Fatalf("Advance on absent row = (%v, %v, %v), want insert at %v", committed, advanced, err, t1)
	}
}

// TestStore_AdvanceIsMonotonic: advancing to an EARLIER value commits nothing
// and reports the stored later value. The row is READ BACK after the call —
// deleting the ON CONFLICT WHERE guard moves the row to t1, which this read
// catches even though the returned values could hide it.
func TestStore_AdvanceIsMonotonic(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	key := issuesKey("acme/widgets")
	if _, _, err := s.Advance(ctx, key, t2); err != nil {
		t.Fatal(err)
	}
	committed, advanced, err := s.Advance(ctx, key, t1)
	if err != nil {
		t.Fatalf("backwards Advance: %v", err)
	}
	if advanced || !committed.Equal(t2) {
		t.Errorf("backwards Advance = (%v, %v), want (%v, false)", committed, advanced, t2)
	}
	if at, _ := rawCursor(t, pool, key); !at.Equal(t2) {
		t.Errorf("row after backwards Advance = %v, want %v (never moves backwards)", at, t2)
	}
	if committed, advanced, err := s.Advance(ctx, key, t2); err != nil || advanced || !committed.Equal(t2) {
		t.Errorf("equal Advance = (%v, %v, %v), want (%v, false)", committed, advanced, err, t2)
	}
}

// TestStore_KeyedByRepoSourceAndAccount: rows are independent per repo and per
// account partition; the nil-account sentinel collapses a second untenanted
// row onto the first.
func TestStore_KeyedByRepoSourceAndAccount(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	acct := uuid.New()
	a, b := issuesKey("acme/a"), issuesKey("acme/b")
	tenanted := Key{AccountID: &acct, Repo: "acme/a", Source: SourceIssues}
	for _, step := range []struct {
		k  Key
		at time.Time
	}{{a, t1}, {b, t2}, {tenanted, t2}} {
		if _, _, err := s.Advance(ctx, step.k, step.at); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Advance(ctx, a, t2); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_report_cursors`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("row count = %d (%v), want 3 (no duplicate untenanted row)", n, err)
	}
	if at, _ := rawCursor(t, pool, b); !at.Equal(t2) {
		t.Errorf("acme/b = %v, want %v", at, t2)
	}
	if at, _ := rawCursor(t, pool, tenanted); !at.Equal(t2) {
		t.Errorf("tenanted acme/a = %v, want %v", at, t2)
	}
}

// TestStore_InvalidKeyRefusedBeforeDB: an empty repo, an unknown source and a
// zero cursor are refused with ErrInvalidKey and write nothing.
func TestStore_InvalidKeyRefusedBeforeDB(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	bad := []Key{{Source: SourceIssues}, {Repo: "r", Source: "discussions"}}
	for _, k := range bad {
		if _, _, err := s.Get(ctx, k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Get(%+v) err = %v, want ErrInvalidKey", k, err)
		}
		if _, err := s.Init(ctx, k, t1); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Init(%+v) err = %v, want ErrInvalidKey", k, err)
		}
		if _, _, err := s.Advance(ctx, k, t1); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Advance(%+v) err = %v, want ErrInvalidKey", k, err)
		}
	}
	if _, err := s.Init(ctx, issuesKey("r"), time.Time{}); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Init(zero) err = %v, want ErrInvalidKey", err)
	}
	if _, _, err := s.Advance(ctx, issuesKey("r"), time.Time{}); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Advance(zero) err = %v, want ErrInvalidKey", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_report_cursors`).Scan(&n); err != nil || n != 0 {
		t.Errorf("row count = %d (%v), want 0", n, err)
	}
}

// TestStore_DatabaseErrorsAreWrapped: a closed pool surfaces each method's
// error rather than a zero value read as "absent".
func TestStore_DatabaseErrorsAreWrapped(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := NewStore(pool)
	pool.Close()
	ctx := context.Background()
	k := issuesKey("r")
	if _, found, err := s.Get(ctx, k); err == nil || found {
		t.Errorf("Get on a closed pool = (found %v, %v), want an error", found, err)
	}
	if _, err := s.Init(ctx, k, t1); err == nil {
		t.Error("Init on a closed pool succeeded")
	}
	if _, _, err := s.Advance(ctx, k, t1); err == nil {
		t.Error("Advance on a closed pool succeeded")
	}
}

// TestStore_ConcurrentWriterCommittedAfterSnapshot pins Init's and Advance's
// re-read fallback. A second session inserts the row and holds it UNCOMMITTED;
// the Store's statement starts (taking its snapshot), blocks on the conflict,
// and resumes after the commit. Its ON CONFLICT arm sees the committed row,
// its snapshot read does not, so the CTE returns no row — and the Store must
// re-read rather than fail or report the value it requested.
func TestStore_ConcurrentWriterCommittedAfterSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(s *Store, key Key) (time.Time, bool, error)
	}{
		{"init", func(s *Store, key Key) (time.Time, bool, error) {
			at, err := s.Init(context.Background(), key, t1)
			return at, false, err
		}},
		{"advance backwards", func(s *Store, key Key) (time.Time, bool, error) {
			return s.Advance(context.Background(), key, t1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			ctx := context.Background()
			key := issuesKey("acme/race")
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `INSERT INTO user_report_cursors (repo, source, cursor_at) VALUES ($1, 'issues', $2)`, key.Repo, t2); err != nil {
				t.Fatal(err)
			}
			type result struct {
				at  time.Time
				adv bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				at, adv, err := tc.call(NewStore(pool), key)
				done <- result{at, adv, err}
			}()
			deadline := time.Now().Add(30 * time.Second)
			for {
				var waiting int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
					WHERE wait_event_type = 'Lock' AND query LIKE '%user_report_cursors%'`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the Store's statement never blocked on the uncommitted conflicting row")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			r := <-done
			if r.err != nil || !r.at.Equal(t2) || r.adv {
				t.Errorf("= (%v, advanced %v, %v), want the concurrently committed %v, not advanced", r.at, r.adv, r.err, t2)
			}
		})
	}
}

// TestStore_TenantIsolation proves 0096's policy through the Store under a
// NOSUPERUSER NOBYPASSRLS probe role (the admin role bypasses RLS even under
// FORCE). Account B's row exists BEFORE the probe reads. In-test
// counterfactual: with the policy replaced by USING (true) the SAME probe
// read sees account B's row, so the isolation assertion is about the policy,
// not an empty table.
func TestStore_TenantIsolation(t *testing.T) {
	dbURL := pgtest.NewURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)
	accountA, accountB := uuid.New(), uuid.New()
	adminStore := NewStore(admin)
	keyA := Key{AccountID: &accountA, Repo: "acme/widgets", Source: SourceIssues}
	keyB := Key{AccountID: &accountB, Repo: "acme/widgets", Source: SourceIssues}
	if _, _, err := adminStore.Advance(ctx, keyA, t1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := adminStore.Advance(ctx, keyB, t2); err != nil {
		t.Fatal(err)
	}
	probe := newProbePool(t, ctx, admin, dbURL)
	ps := NewStore(probe)

	visible := func() (own, other bool, rows int) {
		t.Helper()
		var err error
		if _, own, err = ps.Get(ctx, keyA); err != nil {
			t.Fatal(err)
		}
		// keyB read under account B's tenant would see it; reading B's key
		// under A's tenant is the isolation question, so query directly.
		if err := postgres.WithTenant(ctx, probe, accountA.String(), func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM user_report_cursors WHERE account_id = $1`, accountB).Scan(&n); err != nil {
				return err
			}
			other = n > 0
			return tx.QueryRow(ctx, `SELECT count(*) FROM user_report_cursors`).Scan(&rows)
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
		_, err := tx.Exec(ctx, `INSERT INTO user_report_cursors (account_id, repo, source, cursor_at) VALUES ($1, 'intruder', 'issues', now())`, accountB)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("cross-account insert err = %v, want a row-level security refusal", err)
	}

	if _, err := admin.Exec(ctx, `ALTER POLICY user_report_cursors_tenant_isolation ON user_report_cursors
		USING (true) WITH CHECK (true)`); err != nil {
		t.Fatal(err)
	}
	if _, other, rows := visible(); !other || rows != 2 {
		t.Errorf("under USING (true) the probe sees other=%v rows=%d, want account B's row visible — the fixture does not discriminate", other, rows)
	}
}

// newProbePool creates a per-test NOSUPERUSER NOBYPASSRLS login role and
// returns a pool connected as it (the digest rls_test shape).
func newProbePool(t *testing.T, ctx context.Context, admin *pgxpool.Pool, dbURL string) *pgxpool.Pool {
	t.Helper()
	role := "fh_ur_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	const password = "fishhawk-ur-probe"
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
