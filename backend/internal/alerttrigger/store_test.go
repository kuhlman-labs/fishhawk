package alerttrigger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// Every time fixture here is set by SQL relative to the DATABASE clock
// (claimed_at backdated with `now() - interval ...`), never seeded from a Go
// time.Now — the #3048 cross-clock trap. The stale cutoff Claim evaluates is
// itself a DB-clock comparison, so both sides share one clock.

const staleAfter = 10 * time.Minute

func testKey(fingerprint string) Key {
	return Key{SourceID: "pagerduty", Repo: "kuhlman-labs/fishhawk", Fingerprint: fingerprint}
}

// row is the persisted state of one alert_incidents row.
type row struct {
	token       uuid.UUID
	issueNumber *int
	issueURL    *string
	runID       *uuid.UUID
	occurrences int
}

func readRow(t *testing.T, pool *pgxpool.Pool, key Key) (row, bool) {
	t.Helper()
	var r row
	err := pool.QueryRow(context.Background(), `SELECT claim_token, issue_number, issue_url, run_id, occurrences
FROM alert_incidents WHERE source_id = $1 AND repo = $2 AND fingerprint = $3`,
		key.SourceID, key.Repo, key.Fingerprint).Scan(&r.token, &r.issueNumber, &r.issueURL, &r.runID, &r.occurrences)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return row{}, false
		}
		t.Fatalf("read alert_incidents row: %v", err)
	}
	return r, true
}

func mustClaim(t *testing.T, s *PostgresStore, key Key) Claim {
	t.Helper()
	c, err := s.Claim(context.Background(), key, staleAfter)
	if err != nil {
		t.Fatalf("Claim(%v): %v", key, err)
	}
	return c
}

// TestClaim_Lifecycle pins the three claim outcomes in order: the first alert
// claims (new), a second alert while the claim is live is in flight, and
// after Complete a third alert finds the filed issue (existing) — every alert
// counted.
func TestClaim_Lifecycle(t *testing.T) {
	t.Parallel()
	pool := pgtest.NewPool(t)
	s := NewPostgresStore(pool)
	ctx := context.Background()
	key := testKey("disk-full:db-1")

	first := mustClaim(t, s, key)
	if first.Kind != ClaimNew || first.Token == uuid.Nil || first.Occurrences != 1 {
		t.Fatalf("first Claim = %+v, want ClaimNew with a token and occurrences 1", first)
	}

	second := mustClaim(t, s, key)
	if second.Kind != ClaimInFlight || second.Token != uuid.Nil || second.Occurrences != 2 {
		t.Fatalf("second Claim = %+v, want ClaimInFlight, no token, occurrences 2", second)
	}

	const url = "https://github.com/kuhlman-labs/fishhawk/issues/42"
	if err := s.Complete(ctx, key, first.Token, 42, url); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	third := mustClaim(t, s, key)
	if third.Kind != ClaimExisting || third.IssueNumber != 42 || third.IssueURL != url || third.Occurrences != 3 {
		t.Fatalf("third Claim = %+v, want ClaimExisting #42 %s occurrences 3", third, url)
	}
	if third.Token != uuid.Nil {
		t.Errorf("ClaimExisting carried token %s, want uuid.Nil (the caller is not the claimant)", third.Token)
	}

	got, ok := readRow(t, pool, key)
	if !ok || got.issueNumber == nil || *got.issueNumber != 42 || got.occurrences != 3 || got.token != first.Token {
		t.Errorf("row after lifecycle = %+v (present=%v), want issue 42, occurrences 3, token %s", got, ok, first.Token)
	}

	// A different fingerprint, and the same fingerprint under another repo,
	// are independent incidents.
	if c := mustClaim(t, s, testKey("disk-full:db-2")); c.Kind != ClaimNew {
		t.Errorf("Claim(other fingerprint) = %s, want new", c.Kind)
	}
	other := key
	other.Repo = "kuhlman-labs/other"
	if c := mustClaim(t, s, other); c.Kind != ClaimNew {
		t.Errorf("Claim(same fingerprint, other repo) = %s, want new", c.Kind)
	}
}

// TestComplete_RefusesFiledRow pins the issue_number IS NULL predicate:
// completing an already-filed row (even with its own token) is ErrClaimLost
// and the first filing survives.
func TestComplete_RefusesFiledRow(t *testing.T) {
	t.Parallel()
	pool := pgtest.NewPool(t)
	s := NewPostgresStore(pool)
	ctx := context.Background()
	key := testKey("cpu-high")

	c := mustClaim(t, s, key)
	if err := s.Complete(ctx, key, c.Token, 7, "https://example.test/7"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := s.Complete(ctx, key, c.Token, 8, "https://example.test/8"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("second Complete = %v, want ErrClaimLost", err)
	}
	got, _ := readRow(t, pool, key)
	if got.issueNumber == nil || *got.issueNumber != 7 {
		t.Errorf("issue_number after refused re-Complete = %v, want 7 (the first filing survives)", got.issueNumber)
	}
}

// TestRelease pins Release: it deletes only an UNFILED row holding the
// caller's token, so the next alert files afresh; a wrong token and a filed
// row are both ErrClaimLost and leave the row in place.
func TestRelease(t *testing.T) {
	t.Parallel()
	pool := pgtest.NewPool(t)
	s := NewPostgresStore(pool)
	ctx := context.Background()
	key := testKey("oom")

	c := mustClaim(t, s, key)
	if err := s.Release(ctx, key, uuid.New()); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("Release(wrong token) = %v, want ErrClaimLost", err)
	}
	if _, ok := readRow(t, pool, key); !ok {
		t.Fatal("row gone after a wrong-token Release, want it kept")
	}
	if err := s.Release(ctx, key, c.Token); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok := readRow(t, pool, key); ok {
		t.Fatal("row present after Release, want deleted")
	}
	again := mustClaim(t, s, key)
	if again.Kind != ClaimNew || again.Occurrences != 1 {
		t.Fatalf("Claim after Release = %+v, want ClaimNew occurrences 1", again)
	}

	// A filed row is never released, even by its own claimant.
	if err := s.Complete(ctx, key, again.Token, 9, "https://example.test/9"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := s.Release(ctx, key, again.Token); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("Release(filed row) = %v, want ErrClaimLost", err)
	}
	if got, ok := readRow(t, pool, key); !ok || got.issueNumber == nil || *got.issueNumber != 9 {
		t.Errorf("filed row after Release = %+v (present=%v), want kept with issue 9", got, ok)
	}
}

// TestClaim_StaleReclaimTokenGuard pins the crashed-filer recovery and the
// claim-token guard that makes it safe. A claim whose filer never completed is
// backdated past staleAfter ON THE DB CLOCK; the next alert reclaims it with
// a DIFFERENT token (ClaimNew); the original filer's late Complete is then
// ErrClaimLost and the row keeps the new claimant — so a late filer can never
// overwrite the claim it lost.
func TestClaim_StaleReclaimTokenGuard(t *testing.T) {
	t.Parallel()
	pool := pgtest.NewPool(t)
	s := NewPostgresStore(pool)
	ctx := context.Background()
	key := testKey("latency-p99")

	original := mustClaim(t, s, key)
	if original.Kind != ClaimNew {
		t.Fatalf("first Claim = %s, want new", original.Kind)
	}
	if _, err := pool.Exec(ctx, `UPDATE alert_incidents SET claimed_at = now() - interval '1 hour'
WHERE source_id = $1 AND repo = $2 AND fingerprint = $3`, key.SourceID, key.Repo, key.Fingerprint); err != nil {
		t.Fatalf("backdate claimed_at: %v", err)
	}

	reclaim := mustClaim(t, s, key)
	if reclaim.Kind != ClaimNew {
		t.Fatalf("Claim of a stale unfiled row = %s, want new (stale reclaim)", reclaim.Kind)
	}
	if reclaim.Token == uuid.Nil || reclaim.Token == original.Token {
		t.Fatalf("reclaim token = %s, want a fresh token distinct from the original %s", reclaim.Token, original.Token)
	}
	if reclaim.Occurrences != 2 {
		t.Errorf("reclaim occurrences = %d, want 2 (the reclaiming alert is counted)", reclaim.Occurrences)
	}

	// The reclaim refreshed claimed_at, so the claim is live again.
	if c := mustClaim(t, s, key); c.Kind != ClaimInFlight {
		t.Errorf("Claim right after a reclaim = %s, want in_flight (claimed_at refreshed)", c.Kind)
	}

	if err := s.Complete(ctx, key, original.Token, 11, "https://example.test/11"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("late Complete with the ORIGINAL token = %v, want ErrClaimLost", err)
	}
	got, _ := readRow(t, pool, key)
	if got.token != reclaim.Token || got.issueNumber != nil {
		t.Fatalf("row after the late original Complete = token %s issue %v, want token %s and still unfiled", got.token, got.issueNumber, reclaim.Token)
	}

	if err := s.Complete(ctx, key, reclaim.Token, 12, "https://example.test/12"); err != nil {
		t.Fatalf("Complete with the reclaim token: %v", err)
	}
	if got, _ := readRow(t, pool, key); got.issueNumber == nil || *got.issueNumber != 12 {
		t.Errorf("issue_number after the reclaimant's Complete = %v, want 12", got.issueNumber)
	}
}

// TestClaim_ConcurrentSingleWinner fires 16 concurrent Claims at one key and
// requires EXACTLY one ClaimNew: the single-writer property that stops a
// burst of identical alerts double-filing.
func TestClaim_ConcurrentSingleWinner(t *testing.T) {
	t.Parallel()
	pool := pgtest.NewPool(t)
	s := NewPostgresStore(pool)
	key := testKey("burst")

	const n = 16
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		kinds = map[ClaimKind]int{}
		errs  []error
		start = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, err := s.Claim(context.Background(), key, staleAfter)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			kinds[c.Kind]++
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d concurrent Claims errored: %v", len(errs), errs)
	}
	if kinds[ClaimNew] != 1 || kinds[ClaimInFlight] != n-1 {
		t.Fatalf("concurrent Claim outcomes = %v, want exactly 1 new and %d in_flight", kinds, n-1)
	}
	if got, _ := readRow(t, pool, key); got.occurrences != n {
		t.Errorf("occurrences after %d concurrent Claims = %d, want %d", n, got.occurrences, n)
	}
}

// TestRecordRun pins RecordRun: refused on an unfiled claim
// (ErrIncidentNotFiled), recorded on a filed row.
func TestRecordRun(t *testing.T) {
	t.Parallel()
	pool := pgtest.NewPool(t)
	s := NewPostgresStore(pool)
	ctx := context.Background()
	key := testKey("error-rate")

	var runID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state)
VALUES (gen_random_uuid(), 'kuhlman-labs/fishhawk', 'hotfix_change', 'sha', 'alert', 'pending') RETURNING id`).Scan(&runID); err != nil {
		t.Fatalf("seed alert run: %v", err)
	}

	c := mustClaim(t, s, key)
	if err := s.RecordRun(ctx, key, runID); !errors.Is(err, ErrIncidentNotFiled) {
		t.Fatalf("RecordRun on an unfiled claim = %v, want ErrIncidentNotFiled", err)
	}
	if err := s.RecordRun(ctx, testKey("never-seen"), runID); !errors.Is(err, ErrIncidentNotFiled) {
		t.Fatalf("RecordRun on an absent key = %v, want ErrIncidentNotFiled", err)
	}
	if err := s.Complete(ctx, key, c.Token, 5, "https://example.test/5"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := s.RecordRun(ctx, key, runID); err != nil {
		t.Fatalf("RecordRun on a filed row: %v", err)
	}
	if got, _ := readRow(t, pool, key); got.runID == nil || *got.runID != runID {
		t.Errorf("run_id after RecordRun = %v, want %s", got.runID, runID)
	}
}

// TestStore_RefusesInvalidInput pins the argument guards: an incomplete key
// on every method, a non-positive staleAfter, and a Complete without a URL.
// Each refusal is read back as COMMITTED STATE — no row may be written — not
// only as an error.
func TestStore_RefusesInvalidInput(t *testing.T) {
	t.Parallel()
	pool := pgtest.NewPool(t)
	s := NewPostgresStore(pool)
	ctx := context.Background()

	countRows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM alert_incidents`).Scan(&n); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		return n
	}

	for name, key := range map[string]Key{
		"no source":      {Repo: "o/r", Fingerprint: "f"},
		"no repo":        {SourceID: "s", Fingerprint: "f"},
		"no fingerprint": {SourceID: "s", Repo: "o/r"},
	} {
		if _, err := s.Claim(ctx, key, staleAfter); err == nil {
			t.Errorf("Claim(%s) = nil error, want refused", name)
		}
		if err := s.Complete(ctx, key, uuid.New(), 1, "https://example.test/1"); err == nil || errors.Is(err, ErrClaimLost) {
			t.Errorf("Complete(%s) = %v, want an incomplete-key refusal", name, err)
		}
		if err := s.Release(ctx, key, uuid.New()); err == nil || errors.Is(err, ErrClaimLost) {
			t.Errorf("Release(%s) = %v, want an incomplete-key refusal", name, err)
		}
		if err := s.RecordRun(ctx, key, uuid.New()); err == nil || errors.Is(err, ErrIncidentNotFiled) {
			t.Errorf("RecordRun(%s) = %v, want an incomplete-key refusal", name, err)
		}
	}
	if n := countRows(); n != 0 {
		t.Fatalf("rows after incomplete-key Claims = %d, want 0", n)
	}

	for _, d := range []time.Duration{0, -time.Minute} {
		if _, err := s.Claim(ctx, testKey("stale-guard"), d); err == nil {
			t.Errorf("Claim(staleAfter=%s) = nil error, want refused", d)
		}
	}
	if n := countRows(); n != 0 {
		t.Fatalf("rows after non-positive staleAfter Claims = %d, want 0", n)
	}

	key := testKey("complete-guard")
	c := mustClaim(t, s, key)
	if err := s.Complete(ctx, key, c.Token, 3, ""); err == nil {
		t.Error("Complete with an empty URL = nil error, want refused")
	}
	if err := s.Complete(ctx, key, c.Token, 0, "https://example.test/0"); err == nil {
		t.Error("Complete with issue number 0 = nil error, want refused")
	}
	if got, _ := readRow(t, pool, key); got.issueNumber != nil || got.issueURL != nil {
		t.Errorf("row after refused Completes = issue %v url %v, want still unfiled", got.issueNumber, got.issueURL)
	}
}

// TestStore_SurfacesDatabaseErrors pins that a database failure is returned
// as an error — never mistaken for a claim outcome or for the ErrClaimLost /
// ErrIncidentNotFiled sentinels a caller branches on. A missing table fails
// every statement; a closed pool fails before any statement runs.
func TestStore_SurfacesDatabaseErrors(t *testing.T) {
	t.Parallel()
	pool := pgtest.NewPool(t)
	s := NewPostgresStore(pool)
	ctx := context.Background()
	key := testKey("db-error")

	if _, err := pool.Exec(ctx, `ALTER TABLE alert_incidents RENAME TO alert_incidents_gone`); err != nil {
		t.Fatalf("rename table: %v", err)
	}
	if c, err := s.Claim(ctx, key, staleAfter); err == nil {
		t.Errorf("Claim with no table = %+v, nil error; want an error", c)
	}
	sentinel := func(err error) bool { return errors.Is(err, ErrClaimLost) || errors.Is(err, ErrIncidentNotFiled) }
	for name, err := range map[string]error{
		"Complete":  s.Complete(ctx, key, uuid.New(), 1, "https://example.test/1"),
		"Release":   s.Release(ctx, key, uuid.New()),
		"RecordRun": s.RecordRun(ctx, key, uuid.New()),
	} {
		if err == nil || sentinel(err) {
			t.Errorf("%s with no table = %v, want a database error (not a sentinel)", name, err)
		}
	}

	pool.Close()
	if c, err := s.Claim(ctx, key, staleAfter); err == nil {
		t.Errorf("Claim on a closed pool = %+v, nil error; want an error", c)
	}
}
