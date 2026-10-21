package mergequeue

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// Every time fixture here is set by SQL relative to the database clock; no Go
// time.Now() value is compared against a DB-stamped one.

const testRepo = "kuhlman-labs/fishhawk"

type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	repo  run.Repository
	store *PostgresStore
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	return &fixture{t: t, pool: pool, repo: run.NewPostgresRepository(pool), store: NewPostgresStore(pool)}
}

func (f *fixture) run() uuid.UUID {
	f.t.Helper()
	r, err := f.repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: testRepo, WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		f.t.Fatalf("create run: %v", err)
	}
	return r.ID
}

func mainBase() Base { return Base{Repo: testRepo, BaseRef: "main"} }

func (f *fixture) enqueue(runID uuid.UUID, baseRef, head string) EnqueueResult {
	f.t.Helper()
	res, err := f.store.Enqueue(context.Background(), EnqueueRequest{RunID: runID, BaseRef: baseRef, HeadSHA: head, BaseSHA: "base-" + head})
	if err != nil {
		f.t.Fatalf("Enqueue(%s, %s): %v", runID, baseRef, err)
	}
	return res
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

// entry reads a row back directly, bypassing the store's own paths.
func (f *fixture) entry(id uuid.UUID) Entry {
	f.t.Helper()
	e, err := scanEntry(f.pool.QueryRow(context.Background(), `SELECT `+entryCols+entryFrom+` WHERE e.id = $1`, id))
	if err != nil {
		f.t.Fatalf("read entry %s: %v", id, err)
	}
	return e
}

func (f *fixture) count(where string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM merge_candidate_queue_entries WHERE `+where, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", where, err)
	}
	return n
}

func (f *fixture) status(runID uuid.UUID) Status {
	f.t.Helper()
	st, ok, err := f.store.ActiveForRun(context.Background(), runID)
	if err != nil || !ok {
		f.t.Fatalf("ActiveForRun(%s) = found %v, err %v; want an active entry", runID, ok, err)
	}
	return st
}

func (f *fixture) admitNext(base Base) Admission {
	f.t.Helper()
	a, err := f.store.AdmitNext(context.Background(), base)
	if err != nil {
		f.t.Fatalf("AdmitNext: %v", err)
	}
	return a
}

// seed inserts a row BY CONSTRUCTION (never through the store), enqueued
// `ago` before the database clock.
func (f *fixture) seed(id, runID uuid.UUID, state State, ago time.Duration) {
	f.t.Helper()
	f.exec(`INSERT INTO merge_candidate_queue_entries (id, run_id, repo, base_ref, state, phase, enqueued_at, anchored_head_sha, admitted_at)
VALUES ($1::uuid, $2, $3, 'main', $4::text, CASE WHEN $4::text = 'live' THEN 'verifying' END,
  clock_timestamp() - make_interval(secs => $5::float8), 'h-' || $6, CASE WHEN $4::text = 'live' THEN clock_timestamp() END)`,
		id, runID, testRepo, string(state), ago.Seconds(), id.String())
}

func TestEnqueue_NewEntriesHeldAtTailWithPositions(t *testing.T) {
	f := newFixture(t)
	runs := []uuid.UUID{f.run(), f.run(), f.run()}
	for i, r := range runs {
		res := f.enqueue(r, "main", fmt.Sprintf("h%d", i))
		if res.Outcome != EnqueueInserted || res.Entry.State != StateHeld || res.Dropped != nil {
			t.Fatalf("enqueue %d = %+v, want a held insert", i, res)
		}
		if res.Entry.Repo != testRepo || res.Entry.AnchoredBaseSHA != fmt.Sprintf("base-h%d", i) {
			t.Fatalf("enqueue %d entry = %+v, want repo from the run and the base sha recorded", i, res.Entry)
		}
	}
	for i, r := range runs {
		st := f.status(r)
		if st.Position != i+1 || st.LiveRunID != nil {
			t.Fatalf("run %d status = position %d live %v, want position %d and no live run", i, st.Position, st.LiveRunID, i+1)
		}
	}
	if _, ok, err := f.store.ActiveForRun(context.Background(), f.run()); ok || err != nil {
		t.Fatalf("ActiveForRun(no entry) = found %v err %v, want not found", ok, err)
	}
}

func TestEnqueue_SameBaseReanchorsInPlace(t *testing.T) {
	f := newFixture(t)
	a, b := f.run(), f.run()
	first := f.enqueue(a, "main", "h1").Entry
	f.enqueue(b, "main", "x1")

	res := f.enqueue(a, "main", "h2")
	if res.Outcome != EnqueueReanchored || !res.AnchorChanged {
		t.Fatalf("re-enqueue = %+v, want reanchored with a changed anchor", res)
	}
	got := f.entry(first.ID)
	if got.AnchoredHeadSHA != "h2" || got.AnchoredBaseSHA != "base-h2" || !got.EnqueuedAt.Equal(first.EnqueuedAt) || got.State != StateHeld {
		t.Fatalf("re-anchored entry = %+v, want head h2, same enqueued_at %v, still held", got, first.EnqueuedAt)
	}
	if f.status(a).Position != 1 || f.count(`run_id = $1`, a) != 1 {
		t.Fatalf("re-anchor moved A's place or added a row")
	}

	same := f.enqueue(a, "main", "h2")
	if same.Outcome != EnqueueReanchored || same.AnchorChanged || !f.entry(first.ID).UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("same-anchor re-enqueue = %+v, want reanchored, unchanged, nothing written", same)
	}

	// A LIVE entry keeps its state and phase across a re-anchor.
	if adm := f.admitNext(mainBase()); !adm.Admitted || adm.Entry.ID != first.ID {
		t.Fatalf("admit = %+v, want A", adm)
	}
	live := f.enqueue(a, "main", "h3").Entry
	if live.State != StateLive || live.Phase != PhaseAdmitted || live.AnchoredHeadSHA != "h3" {
		t.Fatalf("live re-anchor = %+v, want live/admitted at h3", live)
	}
}

func TestEnqueue_DifferentBaseDropsOldAndInsertsAtTail(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.run(), f.run(), f.run()
	old := f.enqueue(a, "main", "h1").Entry
	f.enqueue(b, "main", "x1")
	f.enqueue(c, "release", "y1")

	res := f.enqueue(a, "release", "h2")
	if res.Outcome != EnqueueBaseChanged || res.Dropped == nil || res.Dropped.ID != old.ID {
		t.Fatalf("enqueue on a new base = %+v, want base_changed dropping %s", res, old.ID)
	}
	dropped := f.entry(old.ID)
	if dropped.State != StateDropped || dropped.EjectReason != ReasonBaseChanged || dropped.SettledAt == nil {
		t.Fatalf("old entry = %+v, want dropped base_changed with settled_at", dropped)
	}
	if res.Entry.BaseRef != "release" || res.Entry.State != StateHeld || f.status(a).Position != 2 {
		t.Fatalf("new entry = %+v position %d, want held on release at position 2", res.Entry, f.status(a).Position)
	}
	if f.status(b).Position != 1 {
		t.Fatalf("B position on main = %d, want 1 after A left", f.status(b).Position)
	}
}

func TestEnqueue_AfterTerminalInsertsFreshRowAtTail(t *testing.T) {
	f := newFixture(t)
	a, b := f.run(), f.run()
	first := f.enqueue(a, "main", "h1").Entry
	f.enqueue(b, "main", "x1")
	if _, err := f.store.Settle(context.Background(), first.ID, StateEjected, "verify_failed"); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	res := f.enqueue(a, "main", "h2")
	if res.Outcome != EnqueueInserted || res.Entry.ID == first.ID {
		t.Fatalf("re-entry = %+v, want a FRESH row (ADR-093 Q2)", res)
	}
	if f.status(a).Position != 2 || f.entry(first.ID).State != StateEjected {
		t.Fatalf("re-entry position %d / old state %s, want tail position 2 and the ejected row untouched", f.status(a).Position, f.entry(first.ID).State)
	}
}

// TestEnqueue_EntrySettledBetweenPeekAndLockGetsFreshRow isolates Enqueue's
// post-lock re-read: the peeked active entry is settled (by Settle, which
// takes only the base lock) while Enqueue holds just the run lock, so the
// re-read sees a terminal row and must insert a fresh one rather than
// re-anchor the terminal row.
func TestEnqueue_EntrySettledBetweenPeekAndLockGetsFreshRow(t *testing.T) {
	f := newFixture(t)
	a := f.run()
	first := f.enqueue(a, "main", "h1").Entry
	f.store.afterPeek = func(ctx context.Context) {
		f.store.afterPeek = nil
		if _, err := NewPostgresStore(f.pool).Settle(ctx, first.ID, StateDropped, "pr_closed"); err != nil {
			t.Errorf("settle between peek and lock: %v", err)
		}
	}
	res := f.enqueue(a, "main", "h2")
	if res.Outcome != EnqueueInserted || res.Entry.ID == first.ID || res.Entry.State != StateHeld {
		t.Fatalf("enqueue after a concurrent settle = %+v, want a fresh held row", res)
	}
	if old := f.entry(first.ID); old.State != StateDropped || old.AnchoredHeadSHA != "h1" {
		t.Fatalf("settled entry = %+v, want dropped and its anchor untouched", old)
	}
}

func TestEnqueue_RefusesInvalidAndUnknownRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.run()
	for name, req := range map[string]EnqueueRequest{
		"nil run":    {BaseRef: "main", HeadSHA: "h"},
		"no base":    {RunID: r, HeadSHA: "h"},
		"no head":    {RunID: r, BaseRef: "main"},
		"empty base": {RunID: r, BaseRef: "", HeadSHA: "h"},
	} {
		if _, err := f.store.Enqueue(ctx, req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	_, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: uuid.New(), BaseRef: "main", HeadSHA: "h"})
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("unknown run err = %v, want ErrNotFound wrapping run.ErrNotFound", err)
	}
	if f.count(`true`) != 0 {
		t.Fatal("a refused enqueue wrote a row")
	}
}

func TestAdmitNext_AdmitsOldestHeldAndListsLiveFirst(t *testing.T) {
	f := newFixture(t)
	a, b := f.run(), f.run()
	f.enqueue(a, "main", "h1")
	f.enqueue(b, "main", "x1")

	adm := f.admitNext(mainBase())
	if !adm.Admitted || adm.Readmitted || adm.Entry == nil || adm.Entry.RunID != a {
		t.Fatalf("AdmitNext = %+v, want A admitted", adm)
	}
	if adm.Entry.State != StateLive || adm.Entry.Phase != PhaseAdmitted || adm.Entry.AdmittedAt == nil {
		t.Fatalf("admitted entry = %+v, want live/admitted with admitted_at", adm.Entry)
	}
	if st := f.status(a); st.Position != 0 || st.LiveRunID == nil || *st.LiveRunID != a {
		t.Fatalf("A status = %+v, want live position 0 holding the line", st)
	}
	if st := f.status(b); st.Position != 1 || st.LiveRunID == nil || *st.LiveRunID != a {
		t.Fatalf("B status = %+v, want held position 1 behind live A", st)
	}

	// Enqueue C, then list: live first, then held FIFO.
	c := f.run()
	f.enqueue(c, "main", "z1")
	list, err := f.store.ListForBase(context.Background(), mainBase())
	if err != nil {
		t.Fatalf("ListForBase: %v", err)
	}
	if len(list) != 3 || list[0].RunID != a || list[1].RunID != b || list[2].RunID != c {
		t.Fatalf("ListForBase order = %v, want A(live), B, C", runIDs(list))
	}
	empty, err := f.store.ListForBase(context.Background(), Base{Repo: testRepo, BaseRef: "nope"})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("ListForBase(empty) = %v, %v; want a non-nil empty list", empty, err)
	}
}

func runIDs(es []Entry) []uuid.UUID {
	out := make([]uuid.UUID, len(es))
	for i, e := range es {
		out[i] = e.RunID
	}
	return out
}

// TestAdmitNext_LiveEntryBlocksTheBase is plan control (1): a base with a
// live entry admits nothing, so a second pass never goes live beside it.
func TestAdmitNext_LiveEntryBlocksTheBase(t *testing.T) {
	f := newFixture(t)
	liveRun, heldRun := f.run(), f.run()
	liveID := uuid.New()
	f.seed(liveID, liveRun, StateLive, time.Minute)
	f.seed(uuid.New(), heldRun, StateHeld, 30*time.Second)

	adm := f.admitNext(mainBase())
	if adm.Admitted || adm.Entry == nil || adm.Entry.ID != liveID {
		t.Fatalf("AdmitNext = %+v, want no admission naming the live entry %s", adm, liveID)
	}
	if n := f.count(`state = 'live'`); n != 1 {
		t.Fatalf("live entries = %d, want 1", n)
	}
	if st := f.status(heldRun); st.Position != 1 || st.LiveRunID == nil || *st.LiveRunID != liveRun {
		t.Fatalf("held status = %+v, want position 1 behind %s", st, liveRun)
	}
}

// TestAdmitNext_FIFOByEnqueuedAtNotID is plan control (2): the held rows'
// UUID order is the REVERSE of their enqueue order, and the later-enqueued
// row is inserted first (heap order), so neither an id order nor a missing
// ORDER BY admits the right run.
func TestAdmitNext_FIFOByEnqueuedAtNotID(t *testing.T) {
	f := newFixture(t)
	early, late := f.run(), f.run()
	lowID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	highID := uuid.MustParse("ffffffff-ffff-ffff-ffff-fffffffffffe")
	f.seed(lowID, late, StateHeld, 10*time.Second)
	f.seed(highID, early, StateHeld, 20*time.Second)

	adm := f.admitNext(mainBase())
	if !adm.Admitted || adm.Entry.ID != highID {
		t.Fatalf("AdmitNext admitted %+v, want the earliest-enqueued entry %s", adm.Entry, highID)
	}
	if st := f.status(late); st.Position != 1 {
		t.Fatalf("late position = %d, want 1", st.Position)
	}
}

// TestAdmitNext_OtherAccountsLiveEntryDoesNotBlock is plan control (3): the
// pgtest role is a superuser that BYPASSES RLS (the production posture), so
// the scoping under test is the store's explicit runs.account_id match.
func TestAdmitNext_OtherAccountsLiveEntryDoesNotBlock(t *testing.T) {
	f := newFixture(t)
	acct1, acct2 := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{acct1, acct2} {
		f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, id, "mq-"+id.String())
	}
	other, mine := f.run(), f.run()
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct1, other)
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct2, mine)
	f.seed(uuid.New(), other, StateLive, time.Minute)
	f.seed(uuid.New(), mine, StateHeld, 30*time.Second)

	if st := f.status(mine); st.LiveRunID != nil || st.Position != 1 {
		t.Fatalf("account-2 status = %+v, want no live run disclosed from account 1", st)
	}
	base2 := Base{AccountID: acct2.String(), Repo: testRepo, BaseRef: "main"}
	list, err := f.store.ListForBase(context.Background(), base2)
	if err != nil || len(list) != 1 || list[0].RunID != mine {
		t.Fatalf("account-2 list = %v, %v; want only its own entry", list, err)
	}
	adm := f.admitNext(base2)
	if !adm.Admitted || adm.Entry.RunID != mine || adm.Entry.AccountID != acct2.String() {
		t.Fatalf("account-2 AdmitNext = %+v, want its own held entry admitted", adm)
	}
	// The untenanted scope is its own account: nothing there.
	if adm := f.admitNext(mainBase()); adm.Admitted || adm.Entry != nil {
		t.Fatalf("untenanted AdmitNext = %+v, want nothing", adm)
	}
}

// TestActiveBases_ListsEveryBaseWithAHeldOrLiveEntry: one row per (account,
// repo, base ref) with an active entry; a base whose entries are all terminal
// is not listed, and an empty table answers an empty, non-nil list.
func TestActiveBases_ListsEveryBaseWithAHeldOrLiveEntry(t *testing.T) {
	f := newFixture(t)
	if got, err := f.store.ActiveBases(context.Background()); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("ActiveBases on an empty table = %v, %v; want an empty non-nil list", got, err)
	}
	acct := uuid.New()
	f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, acct, "mq-"+acct.String())
	live, held, tenanted, settled := f.run(), f.run(), f.run(), f.run()
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct, tenanted)
	f.seed(uuid.New(), live, StateLive, time.Minute)
	f.seed(uuid.New(), held, StateHeld, 30*time.Second)
	f.seed(uuid.New(), tenanted, StateHeld, 20*time.Second)
	gone := f.enqueue(settled, "release", "r1").Entry
	if _, err := f.store.Settle(context.Background(), gone.ID, StateDropped, "run_terminal"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := f.store.ActiveBases(context.Background())
	if err != nil {
		t.Fatalf("ActiveBases: %v", err)
	}
	want := []Base{mainBase(), {AccountID: acct.String(), Repo: testRepo, BaseRef: "main"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ActiveBases = %+v, want %+v (one per active base, settled release base absent)", got, want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.store.ActiveBases(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ActiveBases on a canceled context err = %v, want context.Canceled", err)
	}
}

// TestAdmitNext_ConcurrentAdmittersAdmitExactlyOne is plan control (4).
func TestAdmitNext_ConcurrentAdmittersAdmitExactlyOne(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 4; i++ {
		f.enqueue(f.run(), "main", fmt.Sprintf("h%d", i))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	errs := []error{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			adm, err := f.store.AdmitNext(context.Background(), mainBase())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if adm.Admitted {
				admitted++
			}
		}()
	}
	wg.Wait()
	if len(errs) != 0 || admitted != 1 || f.count(`state = 'live'`) != 1 {
		t.Fatalf("8 concurrent AdmitNext: admitted=%d live=%d errs=%v; want exactly one admission and one live entry", admitted, f.count(`state = 'live'`), errs)
	}
}

// TestAdmitNext_LockSerializesTheLiveCheck is the deterministic half of
// control (4): admitter A parks between its live check and its held select
// while B runs. Under the base lock B waits for A and then sees A's live
// entry; without it B admits the head and A admits the next one — two live.
func TestAdmitNext_LockSerializesTheLiveCheck(t *testing.T) {
	f := newFixture(t)
	f.enqueue(f.run(), "main", "h1")
	f.enqueue(f.run(), "main", "h2")
	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.store.afterLiveCheck = func(context.Context) {
		once.Do(func() {
			close(parked)
			<-release
		})
	}
	results := make(chan Admission, 2)
	errs := make(chan error, 2)
	admit := func() {
		adm, err := f.store.AdmitNext(context.Background(), mainBase())
		results <- adm
		errs <- err
	}
	go admit()
	<-parked
	bDone := make(chan struct{})
	go func() { admit(); close(bDone) }()
	select {
	case <-bDone:
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	close(release)
	admitted := 0
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("AdmitNext: %v", err)
		}
		if (<-results).Admitted {
			admitted++
		}
	}
	if live := f.count(`state = 'live'`); live != 1 || admitted != 1 {
		t.Fatalf("live entries = %d, admissions = %d; want 1 and 1 (the base lock serializes the live check)", live, admitted)
	}
}

func TestAdmitNext_EmptyBaseAndInvalidBase(t *testing.T) {
	f := newFixture(t)
	if adm := f.admitNext(mainBase()); adm.Admitted || adm.Entry != nil {
		t.Fatalf("empty base AdmitNext = %+v, want nothing", adm)
	}
	for name, b := range map[string]Base{
		"no repo":     {BaseRef: "main"},
		"no base":     {Repo: testRepo},
		"bad account": {AccountID: "not-a-uuid", Repo: testRepo, BaseRef: "main"},
	} {
		if _, err := f.store.AdmitNext(context.Background(), b); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("AdmitNext(%s) err = %v, want ErrInvalidRequest", name, err)
		}
		if _, err := f.store.ListForBase(context.Background(), b); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("ListForBase(%s) err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// TestAdmitNext_StaleAdmissionReclaimedOnce pins the crash-recovery arm: a
// live entry stuck in phase admitted past AdmissionStaleAfter (backdated by a
// DB-anchored offset) is re-claimed exactly once; a fresh admission and a
// stale entry in any other phase are not.
func TestAdmitNext_StaleAdmissionReclaimedOnce(t *testing.T) {
	f := newFixture(t)
	a, b := f.run(), f.run()
	f.enqueue(a, "main", "h1")
	f.enqueue(b, "main", "x1")
	first := f.admitNext(mainBase()).Entry
	backdate := func() {
		f.exec(`UPDATE merge_candidate_queue_entries SET admitted_at = clock_timestamp() - make_interval(secs => $2) WHERE id = $1`,
			first.ID, (AdmissionStaleAfter + time.Minute).Seconds())
	}
	backdate()
	stale := f.entry(first.ID).AdmittedAt

	adm := f.admitNext(mainBase())
	if !adm.Admitted || !adm.Readmitted || adm.Entry.ID != first.ID || !adm.Entry.AdmittedAt.After(*stale) {
		t.Fatalf("stale AdmitNext = %+v, want A re-claimed with a fresh admitted_at", adm)
	}
	if again := f.admitNext(mainBase()); again.Admitted || again.Entry.ID != first.ID {
		t.Fatalf("second AdmitNext = %+v, want no re-claim of a just-claimed admission", again)
	}
	if _, err := f.store.SetPhase(context.Background(), first.ID, PhaseVerifying); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	backdate()
	if adm := f.admitNext(mainBase()); adm.Admitted || adm.Entry.ID != first.ID {
		t.Fatalf("stale verifying AdmitNext = %+v, want no re-claim outside phase admitted", adm)
	}
	if f.status(b).Position != 1 || f.count(`state = 'live'`) != 1 {
		t.Fatal("re-claim changed the queue")
	}
}

func TestSettle_RequiresReasonAndTerminalState(t *testing.T) {
	f := newFixture(t)
	e := f.enqueue(f.run(), "main", "h1").Entry
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"ejected no reason": func() error { _, err := f.store.Settle(ctx, e.ID, StateEjected, ""); return err },
		"dropped no reason": func() error { _, err := f.store.Settle(ctx, e.ID, StateDropped, ""); return err },
		"held":              func() error { _, err := f.store.Settle(ctx, e.ID, StateHeld, "x"); return err },
		"nil entry":         func() error { _, err := f.store.Settle(ctx, uuid.Nil, StateMerged, ""); return err },
	} {
		if err := call(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if got := f.entry(e.ID); got.State != StateHeld || got.SettledAt != nil {
		t.Fatalf("entry after refused settles = %+v, want untouched held", got)
	}
	if _, err := f.store.Settle(ctx, uuid.New(), StateMerged, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown entry err = %v, want ErrNotFound", err)
	}
	res, err := f.store.Settle(ctx, e.ID, StateMerged, "")
	if err != nil || !res.Settled || res.Entry.State != StateMerged || res.Entry.EjectReason != "" {
		t.Fatalf("merged settle = %+v, %v; want settled merged with no reason", res, err)
	}
}

func TestSettle_IdempotentOnTerminalRow(t *testing.T) {
	f := newFixture(t)
	e := f.enqueue(f.run(), "main", "h1").Entry
	ctx := context.Background()
	first, err := f.store.Settle(ctx, e.ID, StateEjected, "verify_failed")
	if err != nil || !first.Settled {
		t.Fatalf("first settle = %+v, %v", first, err)
	}
	before := f.entry(e.ID)
	second, err := f.store.Settle(ctx, e.ID, StateDropped, "pr_closed")
	if err != nil || second.Settled || second.Entry.State != StateEjected || second.Entry.EjectReason != "verify_failed" {
		t.Fatalf("second settle = %+v, %v; want unsettled, carrying ejected/verify_failed", second, err)
	}
	after := f.entry(e.ID)
	if after.State != StateEjected || after.EjectReason != "verify_failed" ||
		!after.SettledAt.Equal(*before.SettledAt) || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("row after second settle = %+v, want byte-identical to %+v (nothing written)", after, before)
	}
	for name, call := range map[string]func() error{
		"Unadmit":  func() error { _, err := f.store.Unadmit(ctx, e.ID); return err },
		"SetPhase": func() error { _, err := f.store.SetPhase(ctx, e.ID, PhasePassed); return err },
		"Reanchor": func() error { _, err := f.store.Reanchor(ctx, e.ID, "h9", ""); return err },
	} {
		if err := call(); !errors.Is(err, ErrStateConflict) {
			t.Errorf("%s on a terminal entry: err = %v, want ErrStateConflict", name, err)
		}
	}
}

func TestUnadmit_ReturnsToHeldKeepingEnqueuedAt(t *testing.T) {
	f := newFixture(t)
	a, b := f.run(), f.run()
	orig := f.enqueue(a, "main", "h1").Entry
	f.enqueue(b, "main", "x1")
	if _, err := f.store.Unadmit(context.Background(), orig.ID); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("Unadmit(held) err = %v, want ErrStateConflict", err)
	}
	f.admitNext(mainBase())

	got, err := f.store.Unadmit(context.Background(), orig.ID)
	if err != nil {
		t.Fatalf("Unadmit: %v", err)
	}
	back := f.entry(orig.ID)
	if got.State != StateHeld || back.State != StateHeld || back.Phase != "" || back.AdmittedAt != nil || !back.EnqueuedAt.Equal(orig.EnqueuedAt) {
		t.Fatalf("unadmitted entry = %+v, want held, no phase/admitted_at, enqueued_at %v kept", back, orig.EnqueuedAt)
	}
	if f.status(a).Position != 1 || f.status(b).Position != 2 {
		t.Fatalf("positions A=%d B=%d, want A back at 1 ahead of B", f.status(a).Position, f.status(b).Position)
	}
	if adm := f.admitNext(mainBase()); !adm.Admitted || adm.Entry.RunID != a {
		t.Fatalf("re-admission = %+v, want A again", adm)
	}
}

func TestSetPhaseAndReanchor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	f.admitNext(mainBase())

	got, err := f.store.SetPhase(ctx, e.ID, PhasePassed)
	if err != nil || got.Phase != PhasePassed || f.entry(e.ID).Phase != PhasePassed {
		t.Fatalf("SetPhase = %+v, %v; want passed", got, err)
	}
	same, err := f.store.SetPhase(ctx, e.ID, PhasePassed)
	if err != nil || !same.UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("unchanged SetPhase = %+v, %v; want nothing written", same, err)
	}
	re, err := f.store.Reanchor(ctx, e.ID, "h2", "")
	if err != nil || re.AnchoredHeadSHA != "h2" || re.AnchoredBaseSHA != "" || re.State != StateLive || re.Phase != PhasePassed || !re.EnqueuedAt.Equal(e.EnqueuedAt) {
		t.Fatalf("Reanchor = %+v, %v; want head h2, NULL base, live/passed, same enqueued_at", re, err)
	}
	for name, call := range map[string]func() error{
		"SetPhase empty": func() error { _, err := f.store.SetPhase(ctx, e.ID, ""); return err },
		"Reanchor empty": func() error { _, err := f.store.Reanchor(ctx, e.ID, "", "b"); return err },
		"Unadmit nil":    func() error { _, err := f.store.Unadmit(ctx, uuid.Nil); return err },
	} {
		if err := call(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := f.store.Reanchor(ctx, uuid.New(), "h", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reanchor(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestLockKeyDomains(t *testing.T) {
	acct := uuid.New().String()
	if lockKey("", "r", "main") == lockKey(acct, "r", "main") ||
		lockKey(acct, "r", "main") == lockKey(acct, "r", "release") ||
		lockKey(acct, "r", "main") == lockKey(acct, "s", "main") {
		t.Fatal("lock keys collide across account, base or repo")
	}
	if k1, k2 := lockKey(acct, "r", "main"), lockKey(acct, "r", "main"); k1 != k2 {
		t.Fatal("lock key not deterministic")
	}
	if lockKey("", "ab", "c") == lockKey("", "a", "bc") {
		t.Fatal("repo/base separator missing")
	}
	id := uuid.New()
	if k1, k2 := runLockKey(id), runLockKey(id); k1 != k2 || k1 == runLockKey(uuid.New()) {
		t.Fatal("run lock key not per run")
	}
	if k1, k2 := intentLockKey(id), intentLockKey(id); k1 != k2 || k1 == intentLockKey(uuid.New()) || k1 == runLockKey(id) {
		t.Fatal("intent lock key not per run, or not disjoint from the run lock key")
	}
	if !StateHeld.Active() || !StateLive.Active() || StateMerged.Active() || StateHeld.Terminal() || !StateDropped.Terminal() {
		t.Fatal("state classification wrong")
	}
	if (Entry{AccountID: acct, Repo: "r", BaseRef: "main"}).Base() != (Base{AccountID: acct, Repo: "r", BaseRef: "main"}) {
		t.Fatal("Entry.Base mismatch")
	}
}

// failingTx is a pgx.Tx whose QueryRow and Exec fail with err. The embedded
// nil interface leaves every other method unimplemented; the tx helpers call
// only these two.
type failingTx struct {
	pgx.Tx
	err error
}

func (tx failingTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, tx.err
}

func (tx failingTx) QueryRow(context.Context, string, ...any) pgx.Row { return failingRow(tx) }

type failingRow failingTx

func (r failingRow) Scan(...any) error { return r.err }

func TestTxHelpers_WrapDatabaseErrors(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	if _, err := getEntryTx(ctx, failingTx{err: pgx.ErrNoRows}, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("getEntryTx(no rows) err = %v, want ErrNotFound", err)
	}
	boom := errors.New("boom")
	tx := failingTx{err: boom}
	for name, call := range map[string]func() error{
		"getEntryTx": func() error { _, err := getEntryTx(ctx, tx, id); return err },
		"lockTx":     func() error { return lockTx(ctx, tx, 2, 1) },
		"insertTx": func() error {
			_, err := insertTx(ctx, tx, EnqueueRequest{RunID: id, BaseRef: "main", HeadSHA: "h"}, testRepo)
			return err
		},
		"reanchorTx": func() error {
			_, changed, err := reanchorTx(ctx, tx, Entry{ID: id, AnchoredHeadSHA: "old"}, "new", "")
			if changed {
				t.Errorf("reanchorTx reported a change on a failed write")
			}
			return err
		},
		"settleTx": func() error { _, err := settleTx(ctx, tx, id, StateDropped, ReasonBaseChanged); return err },
		"markIntentTx": func() error {
			_, recorded, err := markIntentTx(ctx, tx, Entry{ID: id})
			if recorded {
				t.Errorf("markIntentTx reported a recorded intent on a failed write")
			}
			return err
		},
		"recordIntentTx": func() error {
			return recordIntentTx(ctx, tx, id, func(context.Context, Status) error {
				t.Errorf("recordIntentTx called the record on a failed status read")
				return nil
			})
		},
	} {
		if err := call(); !errors.Is(err, boom) || errors.Is(err, ErrNotFound) {
			t.Errorf("%s err = %v, want the wrapped database error and not ErrNotFound", name, err)
		}
	}
}

func TestStore_CanceledContextFailsAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	e := f.enqueue(r, "main", "h1").Entry
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, call := range map[string]func() error{
		"Enqueue": func() error {
			_, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "release", HeadSHA: "h2"})
			return err
		},
		"AdmitNext": func() error { _, err := f.store.AdmitNext(ctx, mainBase()); return err },
		"Unadmit":   func() error { _, err := f.store.Unadmit(ctx, e.ID); return err },
		"SetPhase":  func() error { _, err := f.store.SetPhase(ctx, e.ID, PhasePassed); return err },
		"Reanchor":  func() error { _, err := f.store.Reanchor(ctx, e.ID, "h2", ""); return err },
		"Settle":    func() error { _, err := f.store.Settle(ctx, e.ID, StateMerged, ""); return err },
		"ActiveForRun": func() error {
			_, found, err := f.store.ActiveForRun(ctx, r)
			if found {
				t.Errorf("ActiveForRun reported found on a failed read")
			}
			return err
		},
		"ListForBase": func() error { _, err := f.store.ListForBase(ctx, mainBase()); return err },
		"MarkMergeIntent": func() error {
			_, err := f.store.MarkMergeIntent(ctx, e.ID, nil)
			return err
		},
		"RecentLineTurns": func() error { _, err := f.store.RecentLineTurns(ctx, mainBase(), 1); return err },
	} {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s err = %v, want context.Canceled", name, err)
		}
	}
	if got := f.entry(e.ID); got.State != StateHeld || got.Phase != "" || got.AnchoredHeadSHA != "h1" || got.BaseRef != "main" || got.MergeIntentAt != nil {
		t.Fatalf("entry after failed calls = %+v, want it untouched", got)
	}
	if n := f.count(`true`); n != 1 {
		t.Fatalf("rows after failed calls = %d, want 1", n)
	}
}

func TestStore_MidTransactionFailureRollsBack(t *testing.T) {
	f := newFixture(t)
	r := f.run()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.store.afterPeek = func(context.Context) { cancel() }
	if _, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Enqueue with a failed base lock err = %v, want context.Canceled", err)
	}
	f.store.afterPeek = nil
	if n := f.count(`true`); n != 0 {
		t.Fatalf("rows after a failed enqueue = %d, want 0", n)
	}

	e := f.enqueue(r, "main", "h1").Entry
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	f.store.afterLiveCheck = func(context.Context) { cancel() }
	if _, err := f.store.AdmitNext(ctx, mainBase()); !errors.Is(err, context.Canceled) {
		t.Fatalf("AdmitNext with a failed held read err = %v, want context.Canceled", err)
	}
	if got := f.entry(e.ID); got.State != StateHeld || got.AdmittedAt != nil {
		t.Fatalf("entry after a failed admission = %+v, want still held", got)
	}
}

// TestEnqueue_MergeIntentStampsNewRow: a MergeIntent insert stamps
// merge_intent_at at the row's enqueue instant and reports IntentRecorded; a
// plain insert leaves it NULL; a base change carries the intent onto the NEW
// row only (the dropped episode keeps its own history).
func TestEnqueue_MergeIntentStampsNewRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plain := f.enqueue(f.run(), "main", "p1")
	if plain.IntentRecorded || plain.Entry.MergeIntentAt != nil || f.entry(plain.Entry.ID).MergeIntentAt != nil {
		t.Fatalf("plain enqueue = %+v, want no merge intent", plain)
	}
	r := f.run()
	res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h1", MergeIntent: true})
	if err != nil || res.Outcome != EnqueueInserted || !res.IntentRecorded {
		t.Fatalf("intent enqueue = %+v, %v; want an inserted row with IntentRecorded", res, err)
	}
	back := f.entry(res.Entry.ID)
	if back.MergeIntentAt == nil || !back.MergeIntentAt.Equal(back.EnqueuedAt) || !res.Entry.MergeIntentAt.Equal(*back.MergeIntentAt) {
		t.Fatalf("row merge_intent_at = %v, want set at enqueued_at %v", back.MergeIntentAt, back.EnqueuedAt)
	}
	list, err := f.store.ListForBase(ctx, mainBase())
	if err != nil || len(list) != 2 || list[0].MergeIntentAt != nil || list[1].MergeIntentAt == nil {
		t.Fatalf("ListForBase = %+v, %v; want the plain row without intent then the intent row with it", list, err)
	}
	if st := f.status(r); st.Entry.MergeIntentAt == nil || st.Position != 2 {
		t.Fatalf("ActiveForRun = %+v, want the intent selected at position 2", st)
	}

	moved, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "release", HeadSHA: "h2", MergeIntent: true})
	if err != nil || moved.Outcome != EnqueueBaseChanged || !moved.IntentRecorded || moved.Entry.MergeIntentAt == nil {
		t.Fatalf("base-change intent enqueue = %+v, %v; want a fresh row with IntentRecorded", moved, err)
	}
	if old := f.entry(res.Entry.ID); old.State != StateDropped || old.MergeIntentAt == nil || !old.MergeIntentAt.Equal(*back.MergeIntentAt) {
		t.Fatalf("dropped episode = %+v, want dropped keeping its own intent stamp", old)
	}
	again, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: f.run(), BaseRef: "release", HeadSHA: "h3"})
	if err != nil || again.IntentRecorded || again.Entry.MergeIntentAt != nil {
		t.Fatalf("plain enqueue on the release base = %+v, %v; want no intent", again, err)
	}
}

// TestEnqueue_ReanchorStampsIntentOnlyWhenNull: on a same-base active entry
// MergeIntent stamps merge_intent_at once (IntentRecorded true), a repeat
// leaves the stamp unchanged (IntentRecorded false), and a later plain
// re-anchor never clears it — each read back from the row.
func TestEnqueue_ReanchorStampsIntentOnlyWhenNull(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.run()
	orig := f.enqueue(r, "main", "h0").Entry
	if moved := f.enqueue(r, "main", "h1"); moved.IntentRecorded || !moved.AnchorChanged || f.entry(orig.ID).MergeIntentAt != nil {
		t.Fatalf("plain re-anchor = %+v, want the anchor moved and no intent stamped", moved)
	}
	orig = f.entry(orig.ID)
	first, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h2", MergeIntent: true})
	if err != nil || first.Outcome != EnqueueReanchored || !first.AnchorChanged || !first.IntentRecorded {
		t.Fatalf("first intent re-anchor = %+v, %v; want reanchored, anchor changed, IntentRecorded", first, err)
	}
	stamped := f.entry(orig.ID)
	if stamped.MergeIntentAt == nil || stamped.AnchoredHeadSHA != "h2" || !stamped.EnqueuedAt.Equal(orig.EnqueuedAt) {
		t.Fatalf("row after intent re-anchor = %+v, want intent set, head h2, enqueued_at kept", stamped)
	}
	repeat, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h2", MergeIntent: true})
	if err != nil || repeat.IntentRecorded || repeat.AnchorChanged {
		t.Fatalf("repeat intent enqueue = %+v, %v; want IntentRecorded false and no anchor change", repeat, err)
	}
	if got := f.entry(orig.ID); !got.MergeIntentAt.Equal(*stamped.MergeIntentAt) || !got.UpdatedAt.Equal(stamped.UpdatedAt) {
		t.Fatalf("row after repeat = %+v, want byte-identical to %+v (nothing written)", got, stamped)
	}
	plain, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h3"})
	if err != nil || plain.IntentRecorded || !plain.AnchorChanged {
		t.Fatalf("plain re-anchor = %+v, %v", plain, err)
	}
	if got := f.entry(orig.ID); got.MergeIntentAt == nil || !got.MergeIntentAt.Equal(*stamped.MergeIntentAt) || got.AnchoredHeadSHA != "h3" {
		t.Fatalf("row after plain re-anchor = %+v, want the intent stamp kept", got)
	}
}

// TestMarkMergeIntent_IdempotentAndRefusesTerminal: an active entry is
// stamped once; a repeat writes nothing (updated_at unchanged) and calls no
// record; a terminal entry is ErrStateConflict and is not stamped.
func TestMarkMergeIntent_IdempotentAndRefusesTerminal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	f.admitNext(mainBase())
	calls := 0
	record := func(context.Context, Status) error { calls++; return nil }

	got, err := f.store.MarkMergeIntent(ctx, e.ID, record)
	if err != nil || !got.IntentRecorded || got.Entry.MergeIntentAt == nil || got.Entry.State != StateLive || calls != 1 {
		t.Fatalf("MarkMergeIntent = %+v, %v (record calls %d); want the live entry stamped, recorded once", got, err, calls)
	}
	stamped := f.entry(e.ID)
	again, err := f.store.MarkMergeIntent(ctx, e.ID, record)
	if err != nil || again.IntentRecorded || calls != 1 {
		t.Fatalf("repeat MarkMergeIntent = %+v, %v (record calls %d); want IntentRecorded false, no record", again, err, calls)
	}
	if after := f.entry(e.ID); !after.UpdatedAt.Equal(stamped.UpdatedAt) || !after.MergeIntentAt.Equal(*stamped.MergeIntentAt) {
		t.Fatalf("row after repeat = %+v, want unchanged %+v", after, stamped)
	}

	gone := f.enqueue(f.run(), "release", "r1").Entry
	if _, err := f.store.Settle(ctx, gone.ID, StateDropped, "pr_closed"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	before := f.entry(gone.ID)
	// A nil record isolates the state guard: no record-time backstop can
	// roll a terminal stamp back.
	for _, rec := range []RecordIntentFunc{nil, record} {
		if _, err := f.store.MarkMergeIntent(ctx, gone.ID, rec); !errors.Is(err, ErrStateConflict) {
			t.Fatalf("MarkMergeIntent(terminal) err = %v, want ErrStateConflict", err)
		}
	}
	if after := f.entry(gone.ID); after.MergeIntentAt != nil || !after.UpdatedAt.Equal(before.UpdatedAt) || calls != 1 {
		t.Fatalf("terminal row after refusal = %+v (record calls %d), want untouched", after, calls)
	}
	if _, err := f.store.MarkMergeIntent(ctx, uuid.Nil, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("MarkMergeIntent(nil) err = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.store.MarkMergeIntent(ctx, uuid.New(), nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkMergeIntent(unknown) err = %v, want ErrNotFound", err)
	}
	held := f.enqueue(f.run(), "main", "h2").Entry
	if res, err := f.store.MarkMergeIntent(ctx, held.ID, nil); err != nil || !res.IntentRecorded || f.entry(held.ID).MergeIntentAt == nil {
		t.Fatalf("MarkMergeIntent(held, nil record) = %+v, %v; want stamped", res, err)
	}
}

// TestRecordIntent_FailureRollsBackAndRetryRecords pins the
// record-before-commit seam (C3): a failing RecordIntentFunc rolls the WHOLE
// call back — no inserted row, no re-anchor, no base_changed drop, no
// merge_intent_at — and returns ErrIntentUnrecorded wrapping its error; a
// retry records (the intent is still NULL, so IntentRecorded is true again)
// and the callback sees the call's own uncommitted writes.
func TestRecordIntent_FailureRollsBackAndRetryRecords(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	boom := errors.New("audit append failed")
	fail := func(context.Context, Status) error { return boom }
	var seen []Status
	ok := func(_ context.Context, st Status) error { seen = append(seen, st); return nil }
	wantUnrecorded := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrIntentUnrecorded) || !errors.Is(err, boom) {
			t.Fatalf("%s err = %v, want ErrIntentUnrecorded wrapping the record error", name, err)
		}
	}

	// Insert arm.
	live := f.run()
	f.enqueue(live, "main", "l1")
	f.admitNext(mainBase())
	r := f.run()
	_, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h1", MergeIntent: true, RecordIntent: fail})
	wantUnrecorded("insert", err)
	if n := f.count(`run_id = $1`, r); n != 0 {
		t.Fatalf("rows after an unrecorded insert = %d, want 0", n)
	}
	res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h1", MergeIntent: true, RecordIntent: ok})
	if err != nil || !res.IntentRecorded || len(seen) != 1 {
		t.Fatalf("retried insert = %+v, %v (records %d); want recorded once", res, err, len(seen))
	}
	if st := seen[0]; st.Entry.ID != res.Entry.ID || st.Entry.MergeIntentAt == nil || st.Position != 1 || st.LiveRunID == nil || *st.LiveRunID != live {
		t.Fatalf("recorded status = %+v, want the new entry with its intent, position 1, live run %s", st, live)
	}

	// Re-anchor arm.
	a := f.run()
	orig := f.enqueue(a, "main", "a1").Entry
	_, err = f.store.Enqueue(ctx, EnqueueRequest{RunID: a, BaseRef: "main", HeadSHA: "a2", MergeIntent: true, RecordIntent: fail})
	wantUnrecorded("re-anchor", err)
	if got := f.entry(orig.ID); got.MergeIntentAt != nil || got.AnchoredHeadSHA != "a1" || !got.UpdatedAt.Equal(orig.UpdatedAt) {
		t.Fatalf("row after an unrecorded re-anchor = %+v, want untouched (head a1, no intent)", got)
	}
	if res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: a, BaseRef: "main", HeadSHA: "a2", MergeIntent: true, RecordIntent: ok}); err != nil || !res.IntentRecorded || len(seen) != 2 {
		t.Fatalf("retried re-anchor = %+v, %v (records %d); want recorded", res, err, len(seen))
	}
	if res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: a, BaseRef: "main", HeadSHA: "a2", MergeIntent: true, RecordIntent: fail}); err != nil || res.IntentRecorded {
		t.Fatalf("repeat with intent already set = %+v, %v; want no record call (so no failure)", res, err)
	}

	// Base-change arm.
	b := f.run()
	old := f.enqueue(b, "main", "b1").Entry
	_, err = f.store.Enqueue(ctx, EnqueueRequest{RunID: b, BaseRef: "release", HeadSHA: "b2", MergeIntent: true, RecordIntent: fail})
	wantUnrecorded("base change", err)
	if got := f.entry(old.ID); got.State != StateHeld || got.MergeIntentAt != nil {
		t.Fatalf("old entry after an unrecorded base change = %+v, want still held, no intent", got)
	}
	if n := f.count(`run_id = $1`, b); n != 1 {
		t.Fatalf("rows after an unrecorded base change = %d, want 1 (no new row)", n)
	}

	// MarkMergeIntent.
	c := f.run()
	ce := f.enqueue(c, "main", "c1").Entry
	_, err = f.store.MarkMergeIntent(ctx, ce.ID, fail)
	wantUnrecorded("MarkMergeIntent", err)
	if got := f.entry(ce.ID); got.MergeIntentAt != nil {
		t.Fatalf("row after an unrecorded MarkMergeIntent = %+v, want no intent", got)
	}
	if res, err := f.store.MarkMergeIntent(ctx, ce.ID, ok); err != nil || !res.IntentRecorded || len(seen) != 3 || seen[2].Entry.ID != ce.ID {
		t.Fatalf("retried MarkMergeIntent = %+v, %v (records %d); want recorded on the entry", res, err, len(seen))
	}

	// A record for a run with no active entry is refused, never called.
	err = pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error { return recordIntentTx(ctx, tx, f.run(), ok) })
	if err == nil || errors.Is(err, ErrIntentUnrecorded) || len(seen) != 3 {
		t.Fatalf("recordIntentTx(no active entry) err = %v (records %d), want a refusal before the record", err, len(seen))
	}
}

// seedTurn inserts a terminal row BY CONSTRUCTION: admitted admittedAgo and
// settled settledAgo before ONE database-clock instant (admittedAgo 0: never
// admitted), so its line turn is exact.
func (f *fixture) seedTurn(runID uuid.UUID, baseRef string, state State, admittedAgo, settledAgo time.Duration) {
	f.t.Helper()
	f.exec(`INSERT INTO merge_candidate_queue_entries
  (run_id, repo, base_ref, state, eject_reason, enqueued_at, anchored_head_sha, admitted_at, settled_at)
SELECT $1, $2, $3, $4::text, CASE WHEN $4::text <> 'merged' THEN 'verify_failed' END, c.ts - interval '1 hour', 'h',
  CASE WHEN $5::float8 > 0 THEN c.ts - make_interval(secs => $5::float8) END, c.ts - make_interval(secs => $6::float8)
FROM (SELECT clock_timestamp() AS ts) c`, runID, testRepo, baseRef, string(state), admittedAgo.Seconds(), settledAgo.Seconds())
}

// TestRecentLineTurns_MergedOnlyNewestFirstAccountScoped: only the base's
// MERGED, admitted entries in the caller's account count, newest settle
// first, bounded by limit. The second account's merged row is the NEWEST
// settle on the same (repo, base), so it would lead the list if the account
// predicate were gone.
func TestRecentLineTurns_MergedOnlyNewestFirstAccountScoped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mine, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{mine, other} {
		f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, id, "mq-"+id.String())
	}
	base := Base{AccountID: mine.String(), Repo: testRepo, BaseRef: "main"}
	if got, err := f.store.RecentLineTurns(ctx, base, 10); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("RecentLineTurns on an empty base = %v, %v; want an empty non-nil list", got, err)
	}
	r, o := f.run(), f.run()
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, mine, r)
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, other, o)
	f.seedTurn(r, "main", StateMerged, 100*time.Second, 40*time.Second)  // 60s, newest of mine
	f.seedTurn(r, "main", StateMerged, 300*time.Second, 200*time.Second) // 100s
	f.seedTurn(r, "main", StateMerged, 500*time.Second, 480*time.Second) // 20s, oldest
	f.seedTurn(r, "main", StateEjected, 50*time.Second, 43*time.Second)  // not merged
	f.seedTurn(r, "main", StateDropped, 60*time.Second, 51*time.Second)  // not merged
	f.seedTurn(r, "main", StateMerged, 0, 30*time.Second)                // never admitted
	f.seedTurn(r, "release", StateMerged, 90*time.Second, 35*time.Second)
	f.seedTurn(o, "main", StateMerged, 43*time.Second, 10*time.Second) // other account, newest

	got, err := f.store.RecentLineTurns(ctx, base, 10)
	want := []time.Duration{60 * time.Second, 100 * time.Second, 20 * time.Second}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("RecentLineTurns(mine, 10) = %v, %v; want %v", got, err, want)
	}
	if got, err := f.store.RecentLineTurns(ctx, base, 2); err != nil || !slices.Equal(got, want[:2]) {
		t.Fatalf("RecentLineTurns(mine, 2) = %v, %v; want %v", got, err, want[:2])
	}
	if got, err := f.store.RecentLineTurns(ctx, Base{AccountID: other.String(), Repo: testRepo, BaseRef: "main"}, 10); err != nil || !slices.Equal(got, []time.Duration{33 * time.Second}) {
		t.Fatalf("RecentLineTurns(other) = %v, %v; want [33s]", got, err)
	}
	if got, err := f.store.RecentLineTurns(ctx, mainBase(), 10); err != nil || len(got) != 0 {
		t.Fatalf("RecentLineTurns(untenanted) = %v, %v; want none", got, err)
	}
	for name, call := range map[string]func() error{
		"limit 0": func() error { _, err := f.store.RecentLineTurns(ctx, base, 0); return err },
		"bad account": func() error {
			_, err := f.store.RecentLineTurns(ctx, Base{AccountID: "x", Repo: testRepo, BaseRef: "main"}, 1)
			return err
		},
		"empty base": func() error { _, err := f.store.RecentLineTurns(ctx, Base{Repo: testRepo}, 1); return err },
		"limit -1":   func() error { _, err := f.store.RecentLineTurns(ctx, base, -1); return err },
	} {
		if err := call(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// TestWithMergeIntentLock_SerializesOneRun: while A holds run r's intent lock
// B waits, and runs only after A returns; a different run's lock is
// independent. Inside the lock fn can Enqueue the same run (the intent key is
// disjoint from Enqueue's run key, so the holder never waits on itself).
// Counterfactual: a body that just calls fn lets B run while A holds, RED.
func TestWithMergeIntentLock_SerializesOneRun(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	ctx := context.Background()
	held, release := make(chan struct{}), make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- f.store.WithMergeIntentLock(ctx, r, func() error {
			if _, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h1", MergeIntent: true}); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	if err := f.store.WithMergeIntentLock(ctx, f.run(), func() error { return nil }); err != nil {
		t.Fatalf("another run's lock: %v", err)
	}
	var bRan atomic.Bool
	bDone := make(chan error, 1)
	go func() {
		bDone <- f.store.WithMergeIntentLock(ctx, r, func() error {
			bRan.Store(true)
			return nil
		})
	}()
	select {
	case <-bDone:
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	if bRan.Load() {
		t.Fatal("B ran while A held the run's intent lock")
	}
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("A: %v", err)
	}
	if err := <-bDone; err != nil || !bRan.Load() {
		t.Fatalf("B = %v ran=%v, want it to run once A released", err, bRan.Load())
	}
}

// TestWithMergeIntentLock_ErrorsAndCancel: fn's error is returned as is; a
// waiter whose context ends returns its error without running fn; a nil run
// id is refused before any SQL.
func TestWithMergeIntentLock_ErrorsAndCancel(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	errFn := errors.New("fn failed")
	if err := f.store.WithMergeIntentLock(context.Background(), r, func() error { return errFn }); !errors.Is(err, errFn) {
		t.Fatalf("fn error = %v, want it returned", err)
	}
	if err := f.store.WithMergeIntentLock(context.Background(), uuid.Nil, func() error { return nil }); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil run id = %v, want ErrInvalidRequest", err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- f.store.WithMergeIntentLock(context.Background(), r, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(100*time.Millisecond))
	defer cancel()
	ran := false
	err := f.store.WithMergeIntentLock(ctx, r, func() error { ran = true; return nil })
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) || ran {
		t.Fatalf("waiter past its deadline = %v ran=%v, want DeadlineExceeded and fn not run", err, ran)
	}
	if err := <-aDone; err != nil {
		t.Fatalf("holder: %v", err)
	}
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := f.store.WithMergeIntentLock(canceled, r, func() error { ran = true; return nil }); !errors.Is(err, context.Canceled) || ran {
		t.Fatalf("canceled context = %v ran=%v, want context.Canceled and fn not run", err, ran)
	}
}

func TestIntentHolderLimit_HalfThePoolAtLeastOne(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		maxConns int32
		want     int
	}{{10, 5}, {3, 1}, {2, 1}, {1, 1}} {
		cfg := f.pool.Config()
		cfg.MaxConns = tc.maxConns
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		if got := intentHolderLimit(pool); got != tc.want {
			t.Errorf("intentHolderLimit(MaxConns=%d) = %d, want %d", tc.maxConns, got, tc.want)
		}
		pool.Close()
	}
	if got := intentHolderLimit(nil); got != 1 {
		t.Errorf("intentHolderLimit(nil) = %d, want 1", got)
	}
}

// TestWithMergeIntentLock_DistinctRunsSaturatingThePoolProgress: six merges
// of six DISTINCT runs race on a 2-connection pool, and every fn needs a
// second connection (a store read) while its lock is held. Each fn first
// waits until as many holders as the pool has connections are inside (or a
// short bound passes), so without the holder bound two holders take both
// connections and every fn's read waits forever. With it at most one holder
// is inside and all six finish. The context deadline only bounds a RED run.
// Counterfactual: a holder limit equal to the pool size deadlocks, RED.
func TestWithMergeIntentLock_DistinctRunsSaturatingThePoolProgress(t *testing.T) {
	f := newFixture(t)
	cfg := f.pool.Config()
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	store := NewPostgresStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(5*time.Second))
	defer cancel()
	const runs = 6
	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for i := 0; i < runs; i++ {
		r := f.run()
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.WithMergeIntentLock(ctx, r, func() error {
				n := inside.Add(1)
				defer inside.Add(-1)
				for {
					m := maxInside.Load()
					if n <= m || maxInside.CompareAndSwap(m, n) {
						break
					}
				}
				deadline := time.Now().Add(timescale.D(100 * time.Millisecond))
				for inside.Load() < cfg.MaxConns && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				_, _, err := store.ActiveForRun(ctx, r)
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("WithMergeIntentLock on a saturated pool = %v, want every distinct-run holder to finish", err)
		}
	}
	if m := maxInside.Load(); m > 1 {
		t.Fatalf("concurrent holders = %d, want at most intentHolderLimit = 1 on a 2-connection pool", m)
	}
}

// TestWithMergeIntentLock_LapsedSessionSurfaces: the lock session dies while
// fn runs (an operator kill, an idle-session timeout). fn's nil is NOT
// reported as success: the call answers ErrIntentLockLapsed, and the lock is
// free again for the next holder. Counterfactual: ignoring the unlock result
// returns nil, RED.
func TestWithMergeIntentLock_LapsedSessionSurfaces(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	ctx := context.Background()
	key := uint64(intentLockKey(r))
	err := f.store.WithMergeIntentLock(ctx, r, func() error {
		var pid int32
		if err := f.pool.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE locktype = 'advisory'
  AND classid = $1::bigint::oid AND objid = $2::bigint::oid AND objsubid = 1 AND granted`,
			int64(key>>32), int64(key&0xffffffff)).Scan(&pid); err != nil {
			return fmt.Errorf("find the lock session: %w", err)
		}
		if _, err := f.pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
			return fmt.Errorf("terminate the lock session: %w", err)
		}
		deadline := time.Now().Add(timescale.D(5 * time.Second))
		for time.Now().Before(deadline) {
			var alive bool
			if err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive); err != nil {
				return err
			}
			if !alive {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return errors.New("the lock session never exited")
	})
	if !errors.Is(err, ErrIntentLockLapsed) {
		t.Fatalf("WithMergeIntentLock after its session died = %v, want ErrIntentLockLapsed", err)
	}
	next, cancel := context.WithTimeout(ctx, timescale.D(time.Second))
	defer cancel()
	ran := false
	if err := f.store.WithMergeIntentLock(next, r, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("next holder = %v ran=%v, want the lock free again", err, ran)
	}
}

// TestWithMergeIntentLock_SurvivesIdleInTransactionTimeout: the lock session
// runs no transaction while fn runs, so a server-side
// idle_in_transaction_session_timeout far shorter than fn cannot end it and
// release the exclusion early. Counterfactual: the transaction-scoped lock
// this replaced (pg_try_advisory_xact_lock in an idle transaction) is killed
// at 50 ms, the probe takes the lock mid-fn, RED.
func TestWithMergeIntentLock_SurvivesIdleInTransactionTimeout(t *testing.T) {
	f := newFixture(t)
	cfg := f.pool.Config()
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "50"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	store := NewPostgresStore(pool)
	r := f.run()
	ctx := context.Background()
	var probeTook bool
	err = store.WithMergeIntentLock(ctx, r, func() error {
		time.Sleep(timescale.D(300 * time.Millisecond))
		if err := f.pool.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, intentLockKey(r)).Scan(&probeTook); err != nil {
			return err
		}
		if probeTook {
			_, _ = f.pool.Exec(ctx, `SELECT pg_advisory_unlock($1)`, intentLockKey(r))
		}
		return nil
	})
	if probeTook {
		t.Fatal("another session took the run's intent lock while fn ran: the exclusion lapsed under idle_in_transaction_session_timeout")
	}
	if err != nil {
		t.Fatalf("WithMergeIntentLock = %v, want nil", err)
	}
}
