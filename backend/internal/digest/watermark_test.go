package digest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// recordingAppender records every digest_marked_read event and optionally
// fails. Guarded by a mutex (the #3226 fake rule).
type recordingAppender struct {
	mu     sync.Mutex
	events []MarkedReadEvent
	err    error
}

func (a *recordingAppender) AppendMarkedRead(_ context.Context, e MarkedReadEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.events = append(a.events, e)
	return nil
}

func (a *recordingAppender) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.events)
}

// failingUpsertDB fails the watermark advance exactly `failures` times and
// passes every other statement through — the injected DB error for the
// append-then-failed-advance path. MarkRead advances via QueryRow
// (advanceWatermarkSQL), so the failure is injected there.
type failingUpsertDB struct {
	DBTX
	failures int
}

func (d *failingUpsertDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql == advanceWatermarkSQL && d.failures > 0 {
		d.failures--
		return errRow{err: errors.New("injected upsert failure")}
	}
	return d.DBTX.QueryRow(ctx, sql, args...)
}

// errRow is a pgx.Row whose Scan always returns err.
type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

// valueRow is a pgx.Row scanning one int64 into its single destination.
type valueRow struct{ v int64 }

func (r valueRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("valueRow: want exactly one scan destination")
	}
	p, ok := dest[0].(*int64)
	if !ok {
		return errors.New("valueRow: destination is not *int64")
	}
	*p = r.v
	return nil
}

// refusedUpsertDB is a pure (no database) DBTX whose QueryRow answers per SQL
// text: rows[sql] when present, otherwise an error naming the unexpected
// statement. Exec and Query are never reached by advanceWatermark and fail
// loudly if they are.
type refusedUpsertDB struct {
	rows map[string]pgx.Row
}

func (d *refusedUpsertDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("refusedUpsertDB: unexpected Exec")
}

func (d *refusedUpsertDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("refusedUpsertDB: unexpected Query")
}

func (d *refusedUpsertDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if r, ok := d.rows[sql]; ok {
		return r
	}
	return errRow{err: errors.New("refusedUpsertDB: unexpected QueryRow " + sql)}
}

func (f *fixture) watermark(t *testing.T, account *uuid.UUID, subject string) (int64, bool) {
	t.Helper()
	seq, ok, err := f.st.GetWatermark(context.Background(), account, subject, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	return seq, ok
}

func (f *fixture) watermarkRows(t *testing.T, subject string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM captain_read_watermarks WHERE captain_subject = $1 AND repo = $2`, subject, testRepo).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedHead appends n entries and returns the repository's chain head.
func (f *fixture) seedHead(t *testing.T, n int) int64 {
	t.Helper()
	var last int64
	for i := 0; i < n; i++ {
		last = f.appendEntry(t, f.run, nil, "run_started", map[string]any{}).Sequence
	}
	return last
}

// TestMarkRead_AdvancesWatermark: the row's sequence equals to_sequence after
// the call, and exactly one event was appended carrying the transition.
func TestMarkRead_AdvancesWatermark(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 2)
	app := &recordingAppender{}
	res, err := f.st.MarkRead(context.Background(), app, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: head})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Advanced || res.Sequence != head || res.HadPrevious {
		t.Errorf("result = %+v, want advanced to %d from nothing", res, head)
	}
	if got, ok := f.watermark(t, nil, "cap"); !ok || got != head {
		t.Errorf("watermark = %d/%v, want %d", got, ok, head)
	}
	if app.count() != 1 || app.events[0].ToSequence != head || app.events[0].Repo != testRepo {
		t.Errorf("events = %+v, want one to %d", app.events, head)
	}
	payload, err := app.events[0].Payload()
	if err != nil || !strings.Contains(string(payload), `"to_sequence":`) {
		t.Errorf("payload = %s (%v), want a to_sequence key", payload, err)
	}
}

// TestMarkRead_AppendFailureLeavesWatermarkUnmoved (failure mode 1): the
// appender ALWAYS errors; the call errors AND a follow-up read returns the
// PRIOR sequence — a committed-state assertion, so swapping the append and
// the upsert (the advance committing before the failed append) reddens it.
func TestMarkRead_AppendFailureLeavesWatermarkUnmoved(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 3)
	ctx := context.Background()
	if err := f.st.UpsertWatermark(ctx, nil, "cap", testRepo, head-2); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("chain unavailable")
	_, err := f.st.MarkRead(ctx, &recordingAppender{err: boom}, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: head})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the append error", err)
	}
	if got, _ := f.watermark(t, nil, "cap"); got != head-2 {
		t.Errorf("watermark after failed append = %d, want the prior %d (unmoved)", got, head-2)
	}
}

// TestMarkRead_RetryAfterFailedAdvanceIsIdempotent (failure mode 2): the first
// attempt appends and then fails the upsert; the retry appends a SECOND
// (honest) entry and converges the watermark to to_sequence, leaving exactly
// one row for the (subject, repo).
func TestMarkRead_RetryAfterFailedAdvanceIsIdempotent(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 2)
	ctx := context.Background()
	app := &recordingAppender{}
	flaky := NewStore(&failingUpsertDB{DBTX: f.pool, failures: 1})
	if _, err := flaky.MarkRead(ctx, app, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: head}); err == nil {
		t.Fatal("first attempt succeeded, want the injected upsert failure")
	}
	if _, ok := f.watermark(t, nil, "cap"); ok || app.count() != 1 {
		t.Fatalf("after failed advance: watermark present=%v events=%d, want absent and 1", ok, app.count())
	}
	res, err := flaky.MarkRead(ctx, app, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: head})
	if err != nil || !res.Advanced {
		t.Fatalf("retry = %+v, %v; want advanced", res, err)
	}
	if got, _ := f.watermark(t, nil, "cap"); got != head {
		t.Errorf("watermark after retry = %d, want %d", got, head)
	}
	if n := f.watermarkRows(t, "cap"); n != 1 {
		t.Errorf("watermark rows = %d, want 1", n)
	}
	if app.count() != 2 {
		t.Errorf("events = %d, want 2 (the chain records both attempts)", app.count())
	}
	// A third call at the same sequence is the no-op: no further entry.
	if res, err := flaky.MarkRead(ctx, app, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: head}); err != nil || res.Advanced || app.count() != 2 {
		t.Errorf("repeat = %+v, %v, events %d; want a no-op", res, err, app.count())
	}
}

// TestMarkRead_BeyondChainHeadRefused (failure mode 3): refused with the head
// named, nothing appended, nothing written.
func TestMarkRead_BeyondChainHeadRefused(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 1)
	app := &recordingAppender{}
	_, err := f.st.MarkRead(context.Background(), app, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: head + 999})
	var beyond *BeyondChainHeadError
	if !errors.As(err, &beyond) || beyond.Head != head || !strings.Contains(err.Error(), "chain head") {
		t.Fatalf("err = %v, want *BeyondChainHeadError naming head %d", err, head)
	}
	if _, ok := f.watermark(t, nil, "cap"); ok || app.count() != 0 {
		t.Errorf("refusal wrote: watermark present=%v events=%d", ok, app.count())
	}
}

// TestMarkRead_AtOrBelowWatermarkIsNoOp (failure mode 4): with the watermark
// at the head, a mark at head-1 (still within the chain, so every downstream
// check would pass) is a no-op. The assertion is on the ENTRY COUNT, because
// the upsert's monotonic WHERE would mask a missing no-op branch on the stored
// sequence alone.
func TestMarkRead_AtOrBelowWatermarkIsNoOp(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 3)
	ctx := context.Background()
	app := &recordingAppender{}
	if _, err := f.st.MarkRead(ctx, app, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: head}); err != nil {
		t.Fatal(err)
	}
	for _, to := range []int64{head - 1, head} {
		res, err := f.st.MarkRead(ctx, app, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: to})
		if err != nil || res.Advanced || res.Sequence != head {
			t.Errorf("mark at %d = %+v, %v; want a no-op at %d", to, res, err, head)
		}
	}
	if app.count() != 1 {
		t.Errorf("digest_marked_read events = %d, want 1 (no-ops append nothing)", app.count())
	}
	if got, _ := f.watermark(t, nil, "cap"); got != head {
		t.Errorf("watermark = %d, want %d", got, head)
	}
}

// TestMarkRead_RejectsInvalidParams covers the validation branches.
func TestMarkRead_RejectsInvalidParams(t *testing.T) {
	f := newFixture(t)
	for name, p := range map[string]MarkReadParams{
		"no repo":     {CaptainSubject: "cap", ToSequence: 1},
		"no subject":  {Repo: testRepo, ToSequence: 1},
		"zero to":     {CaptainSubject: "cap", Repo: testRepo},
		"negative to": {CaptainSubject: "cap", Repo: testRepo, ToSequence: -3},
	} {
		if _, err := f.st.MarkRead(context.Background(), &recordingAppender{}, p); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// TestWatermark_ConcurrentLowerUpsertDoesNotRegress: the upsert is called
// DIRECTLY (bypassing MarkRead's no-op branch, which would otherwise mask the
// control) with a lower sequence after a higher one; the row stays higher.
func TestWatermark_ConcurrentLowerUpsertDoesNotRegress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.st.UpsertWatermark(ctx, nil, "cap", testRepo, 40); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertWatermark(ctx, nil, "cap", testRepo, 30); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.watermark(t, nil, "cap"); got != 40 {
		t.Errorf("watermark after lower upsert = %d, want 40 (monotonic)", got)
	}
}

// TestMarkRead_ConcurrentAdvanceReportsCommitted (#3734 fix-up condition 4): a
// lower mark-read that read the OLD watermark before a higher one committed
// must report the higher COMMITTED sequence with advanced=false, not the value
// it requested. The interleave is deterministic — the lower call's appender
// fires AFTER it reads the watermark and BEFORE its own advance, and inside it
// the higher MarkRead runs to completion — so there is no goroutine or sleep to
// flake. Old code returned p.ToSequence/advanced=true unconditionally, so it
// reddens this assertion.
func TestMarkRead_ConcurrentAdvanceReportsCommitted(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 3)
	ctx := context.Background()
	lower, higher := head-1, head

	var once sync.Once
	interleave := AppenderFunc(func(ctx context.Context, _ MarkedReadEvent) error {
		once.Do(func() {
			if _, err := f.st.MarkRead(ctx, &recordingAppender{}, MarkReadParams{
				CaptainSubject: "cap", Repo: testRepo, ToSequence: higher,
			}); err != nil {
				t.Errorf("interleaved higher mark-read: %v", err)
			}
		})
		return nil
	})
	res, err := f.st.MarkRead(ctx, interleave, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: lower})
	if err != nil {
		t.Fatal(err)
	}
	if res.Advanced || res.Sequence != higher {
		t.Errorf("lower mark-read = %+v, want committed %d with advanced=false", res, higher)
	}
	if got, _ := f.watermark(t, nil, "cap"); got != higher {
		t.Errorf("watermark = %d, want %d (the higher commit stands)", got, higher)
	}
}

// overlapMarkRead overlaps two REAL transactions on two dedicated pool
// connections through the public MarkRead (#3852). It optionally seeds the
// watermark at *seed, then:
//
//  1. tx1 (conn1) upserts `higher` and stays UNCOMMITTED, holding the row lock
//     (or, with no seed, the uncommitted first insert's unique-index entry);
//  2. MarkRead(lower) runs on conn2 on a goroutine — its GetWatermark read does
//     not block, but its advance statement must wait on tx1;
//  3. pg_stat_activity is polled (via a third pool connection) until conn2's
//     backend is in a Lock wait — POSITIVE blocked-state evidence, so the
//     advance statement's snapshot provably predates tx1's commit. If MarkRead
//     returns first, or no Lock wait appears by the deadline, the helper fails
//     LOUDLY: a non-overlapping run can never pass vacuously;
//  4. tx1 commits and MarkRead's result is returned.
//
// Three connections are in use at once (conn1, conn2, the poller). The fixture
// pool comes from pgtest.NewPool -> postgres.Connect, which sets MaxConns = 10
// (backend/internal/postgres/postgres.go:24), so all three are served without
// waiting on the pool. The poll deadline is timescale.D-scaled; the 20ms poll
// interval stays unscaled (the timescale trap). Precedent:
// repoacl TestRepoACLPostgres_PurgeOverlapRejectsInFlightGuardedInsert.
//
// The fail-loud guard was run: committing tx1 BEFORE MarkRead starts (no
// overlap) reddens both overlap tests with "MarkRead returned ({... Sequence:3
// Advanced:false}, err=<nil>) before tx1 committed" — a result that would
// otherwise have passed their assertions.
func overlapMarkRead(t *testing.T, f *fixture, seed *int64, higher, lower int64) (MarkReadResult, error) {
	t.Helper()
	ctx := context.Background()
	if seed != nil {
		if err := f.st.UpsertWatermark(ctx, nil, "cap", testRepo, *seed); err != nil {
			t.Fatalf("seed watermark: %v", err)
		}
	}

	conn1, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn1: %v", err)
	}
	defer conn1.Release()
	tx1, err := conn1.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }() // no-op after commit
	if err := NewStore(tx1).UpsertWatermark(ctx, nil, "cap", testRepo, higher); err != nil {
		t.Fatalf("uncommitted higher upsert in tx1: %v", err)
	}

	conn2, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn2: %v", err)
	}
	defer conn2.Release()
	var conn2pid int
	if err := conn2.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&conn2pid); err != nil {
		t.Fatalf("conn2 backend pid: %v", err)
	}

	type outcome struct {
		res MarkReadResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := NewStore(conn2).MarkRead(ctx, &recordingAppender{}, MarkReadParams{
			CaptainSubject: "cap", Repo: testRepo, ToSequence: lower,
		})
		done <- outcome{res, err}
	}()
	// On any early t.Fatalf below, unblock conn2 (roll tx1 back) and drain the
	// goroutine BEFORE conn2.Release runs — defers are LIFO, so this runs first.
	received := false
	defer func() {
		if received {
			return
		}
		_ = tx1.Rollback(ctx)
		select {
		case <-done:
		case <-time.After(timescale.D(5 * time.Second)):
		}
	}()

	deadline := timescale.D(5 * time.Second)
	blockDeadline := time.Now().Add(deadline)
	blocked := false
	for time.Now().Before(blockDeadline) {
		select {
		case out := <-done:
			received = true
			t.Fatalf("MarkRead returned (%+v, err=%v) before tx1 committed — its advance did NOT wait on tx1, so the overlap this test exists to exercise did not happen", out.res, out.err)
		default:
		}
		var waitEventType string
		if err := f.pool.QueryRow(ctx,
			`SELECT coalesce(wait_event_type, '') FROM pg_stat_activity WHERE pid = $1`, conn2pid,
		).Scan(&waitEventType); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if waitEventType == "Lock" {
			blocked = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !blocked {
		t.Fatalf("conn2 never entered a Lock wait within %v — MarkRead's advance did not block on tx1, so the overlap was not exercised", deadline)
	}

	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}
	select {
	case out := <-done:
		received = true
		return out.res, out.err
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("MarkRead did not return after tx1 committed")
	}
	return MarkReadResult{}, nil // unreachable: t.Fatal stops the goroutine
}

// TestMarkRead_OverlappingHigherCommitReportsLatest (#3852): the watermark is
// at head-2; a concurrent higher mark (head) holds the row uncommitted while
// MarkRead(head-1) blocks on it, then commits. MarkRead must report the LATEST
// committed sequence (head) with advanced=false — not the stale head-2 its own
// statement snapshot held, and not its requested head-1.
//
// Counterfactuals run (each RED, then restored): pre-fix store.go (the CTE +
// UNION ALL same-statement fallback, from HEAD) -> "result = {PreviousSequence:
// 1 HadPrevious:true Sequence:1 Advanced:false}, want committed 3" (the stale
// head-2); the refused branch returning the requested sequence without a
// re-read -> "Sequence:2 ... want committed 3" (head-1).
func TestMarkRead_OverlappingHigherCommitReportsLatest(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 3)
	seed := head - 2
	res, err := overlapMarkRead(t, f, &seed, head, head-1)
	if err != nil {
		t.Fatalf("overlapped MarkRead: %v", err)
	}
	if res.Advanced || res.Sequence != head {
		t.Errorf("result = %+v, want committed %d with advanced=false (not the stale %d, not the requested %d)", res, head, head-2, head-1)
	}
	if got, ok := f.watermark(t, nil, "cap"); !ok || got != head {
		t.Errorf("stored watermark = %d/%v, want %d", got, ok, head)
	}
}

// TestMarkRead_OverlappingFirstInsertReportsLatest (#3852): NO watermark row
// exists; a concurrent FIRST mark (head) inserts it uncommitted while
// MarkRead(head-1) waits on the unique-index conflict, then commits. MarkRead
// must succeed and report head with advanced=false and had_previous=false.
//
// Counterfactual run (RED, then restored): pre-fix store.go -> "overlapped
// MarkRead: digest: mark read: entry appended but watermark not advanced (a
// retry converges it): digest: advance watermark: no rows in result set".
func TestMarkRead_OverlappingFirstInsertReportsLatest(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 3)
	res, err := overlapMarkRead(t, f, nil, head, head-1)
	if err != nil {
		t.Fatalf("overlapped MarkRead: %v", err)
	}
	if res.Advanced || res.HadPrevious || res.Sequence != head {
		t.Errorf("result = %+v, want committed %d, advanced=false, had_previous=false", res, head)
	}
	if got, ok := f.watermark(t, nil, "cap"); !ok || got != head {
		t.Errorf("stored watermark = %d/%v, want %d", got, ok, head)
	}
	if n := f.watermarkRows(t, "cap"); n != 1 {
		t.Errorf("watermark rows = %d, want 1", n)
	}
}

// TestAdvanceWatermark_RefusedUpsertBranches drives advanceWatermark over a
// pure fake DBTX whose upsert is REFUSED (pgx.ErrNoRows), one subtest per
// branch of the separate-statement re-read.
//
// Counterfactuals run (each RED, then restored): the re-read-error branch
// returning (sequence, false, nil) -> "reread error propagates" RED ("err =
// <nil>, want errors.Is re-read unavailable"); the no-row branch returning
// (0, false, nil) -> "reread finds no row fails closed" RED ("err = <nil>, want
// errors.Is digest: watermark row vanished between the refused advance and its
// re-read").
func TestAdvanceWatermark_RefusedUpsertBranches(t *testing.T) {
	boom := errors.New("re-read unavailable")
	refused := errRow{err: pgx.ErrNoRows}
	for _, tc := range []struct {
		name        string
		reread      pgx.Row
		wantErr     error
		wantCommitd int64
	}{
		{"reread error propagates", errRow{err: boom}, boom, 0},
		{"reread finds no row fails closed", errRow{err: pgx.ErrNoRows}, ErrWatermarkRowVanished, 0},
		{"reread value reported", valueRow{v: 42}, nil, 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &refusedUpsertDB{rows: map[string]pgx.Row{advanceWatermarkSQL: refused, getWatermarkSQL: tc.reread}}
			committed, changed, err := NewStore(db).advanceWatermark(context.Background(), nil, "cap", testRepo, 7)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if changed || committed != tc.wantCommitd {
				t.Errorf("advanceWatermark = (%d, %v), want (%d, false)", committed, changed, tc.wantCommitd)
			}
		})
	}
}

// TestMarkRead_GenericAdvanceErrorIsNotARefusal: an advance failure that is
// NOT pgx.ErrNoRows must surface as an error, never be mistaken for a refused
// upsert. The PRIOR head-2 row is what makes a mis-classification observable:
// a generic error that fell into the re-read branch would find head-2 and
// return a nil error with advanced=false — a silent success, watermark unmoved.
//
// Counterfactual run (RED, then restored): classifying ANY upsert error as a
// refusal -> "MarkRead = {... Sequence:1 Advanced:false}, <nil>; want the
// injected advance error".
func TestMarkRead_GenericAdvanceErrorIsNotARefusal(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 3)
	ctx := context.Background()
	if err := f.st.UpsertWatermark(ctx, nil, "cap", testRepo, head-2); err != nil {
		t.Fatal(err)
	}
	flaky := NewStore(&failingUpsertDB{DBTX: f.pool, failures: 1})
	res, err := flaky.MarkRead(ctx, &recordingAppender{}, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: head})
	if err == nil {
		t.Fatalf("MarkRead = %+v, %v; want the injected advance error", res, err)
	}
	if got, _ := f.watermark(t, nil, "cap"); got != head-2 {
		t.Errorf("watermark after failed advance = %d, want the prior %d", got, head-2)
	}
}

// TestWatermark_UntenantedUpsertIsSingleRow: two advancing MarkRead calls with
// a nil account leave exactly one row (the COALESCE-sentinel unique index), and
// a tenanted row for the same (subject, repo) is a distinct watermark.
func TestWatermark_UntenantedUpsertIsSingleRow(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 2)
	ctx := context.Background()
	for _, to := range []int64{head - 1, head} {
		if _, err := f.st.MarkRead(ctx, &recordingAppender{}, MarkReadParams{CaptainSubject: "cap", Repo: testRepo, ToSequence: to}); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.watermarkRows(t, "cap"); n != 1 {
		t.Errorf("untenanted rows = %d, want 1", n)
	}
	acct := uuid.New()
	f.exec(t, `INSERT INTO accounts (id, account_key) VALUES ($1, 'digest-wm')`, acct)
	if err := f.st.UpsertWatermark(ctx, &acct, "cap", testRepo, head-1); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.watermark(t, &acct, "cap"); got != head-1 {
		t.Errorf("tenanted watermark = %d, want %d", got, head-1)
	}
	if got, _ := f.watermark(t, nil, "cap"); got != head {
		t.Errorf("untenanted watermark = %d, want %d (independent of the tenanted row)", got, head)
	}
}

// TestMarkedReadEvent_PayloadRecordsMarkedBy (E76.3 / #3766): the chain entry
// names WHO marked alongside WHICH key moved, and carries the key's basis.
// A caller that leaves MarkedBy empty still gets the key (falling back to the
// watermark subject) rather than a silent omission; an empty basis is omitted.
func TestMarkedReadEvent_PayloadRecordsMarkedBy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ev        MarkedReadEvent
		wantBy    string
		wantBasis any
	}{
		{"explicit", MarkedReadEvent{CaptainSubject: "github:bob", MarkedBy: "github:bob", CaptainSubjectBasis: "caller", Repo: testRepo, ToSequence: 3}, "github:bob", "caller"},
		{"fallback", MarkedReadEvent{CaptainSubject: "cap", Repo: testRepo, ToSequence: 3}, "cap", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := tc.ev.Payload()
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			if m["marked_by"] != tc.wantBy {
				t.Errorf("marked_by = %v, want %q (payload %s)", m["marked_by"], tc.wantBy, raw)
			}
			if m["captain_subject_basis"] != tc.wantBasis {
				t.Errorf("captain_subject_basis = %v, want %v (payload %s)", m["captain_subject_basis"], tc.wantBasis, raw)
			}
		})
	}
}

// TestMarkRead_ThreadsMarkedByAndBasis: MarkRead carries MarkedBy and the
// basis from its params onto the appended event (the handler's only path to
// the chain entry).
func TestMarkRead_ThreadsMarkedByAndBasis(t *testing.T) {
	f := newFixture(t)
	head := f.seedHead(t, 1)
	app := &recordingAppender{}
	if _, err := f.st.MarkRead(context.Background(), app, MarkReadParams{
		CaptainSubject: "github:bob", MarkedBy: "github:bob", CaptainSubjectBasis: "caller", Repo: testRepo, ToSequence: head,
	}); err != nil {
		t.Fatal(err)
	}
	if app.count() != 1 || app.events[0].MarkedBy != "github:bob" || app.events[0].CaptainSubjectBasis != "caller" {
		t.Errorf("events = %+v, want one carrying marked_by github:bob basis caller", app.events)
	}
}
