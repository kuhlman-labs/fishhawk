package mergequeue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pglock"
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
	if _, err := f.store.SetPhase(context.Background(), first.ID, f.entry(first.ID).AdmissionEpoch, PhaseVerifying); err != nil {
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
		"Unadmit":    func() error { _, err := f.store.Unadmit(ctx, e.ID, before.AdmissionEpoch); return err },
		"SetPhase":   func() error { _, err := f.store.SetPhase(ctx, e.ID, before.AdmissionEpoch, PhasePassed); return err },
		"Reanchor":   func() error { _, err := f.store.Reanchor(ctx, e.ID, "h9", ""); return err },
		"ReanchorAt": func() error { _, err := f.store.ReanchorAt(ctx, e.ID, before.AdmissionEpoch, "h9", ""); return err },
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
	if _, err := f.store.Unadmit(context.Background(), orig.ID, orig.AdmissionEpoch); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("Unadmit(held) err = %v, want ErrStateConflict", err)
	}
	adm := f.admitNext(mainBase())

	got, err := f.store.Unadmit(context.Background(), orig.ID, adm.Entry.AdmissionEpoch)
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
	epoch := f.admitNext(mainBase()).Entry.AdmissionEpoch

	got, err := f.store.SetPhase(ctx, e.ID, epoch, PhasePassed)
	if err != nil || got.Phase != PhasePassed || f.entry(e.ID).Phase != PhasePassed {
		t.Fatalf("SetPhase = %+v, %v; want passed", got, err)
	}
	same, err := f.store.SetPhase(ctx, e.ID, epoch, PhasePassed)
	if err != nil || !same.UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("unchanged SetPhase = %+v, %v; want nothing written", same, err)
	}
	re, err := f.store.Reanchor(ctx, e.ID, "h2", "")
	if err != nil || re.AnchoredHeadSHA != "h2" || re.AnchoredBaseSHA != "" || re.State != StateLive || re.Phase != PhasePassed || !re.EnqueuedAt.Equal(e.EnqueuedAt) {
		t.Fatalf("Reanchor = %+v, %v; want head h2, NULL base, live/passed, same enqueued_at", re, err)
	}
	for name, call := range map[string]func() error{
		"SetPhase empty":   func() error { _, err := f.store.SetPhase(ctx, e.ID, epoch, ""); return err },
		"Reanchor empty":   func() error { _, err := f.store.Reanchor(ctx, e.ID, "", "b"); return err },
		"ReanchorAt empty": func() error { _, err := f.store.ReanchorAt(ctx, e.ID, epoch, "", "b"); return err },
		"SettleAt held":    func() error { _, err := f.store.SettleAt(ctx, e.ID, epoch, StateHeld, "x"); return err },
		"Unadmit nil":      func() error { _, err := f.store.Unadmit(ctx, uuid.Nil, epoch); return err },
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
			_, recorded, err := markIntentTx(ctx, tx, Entry{ID: id}, 0)
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
		"AdmitNext":  func() error { _, err := f.store.AdmitNext(ctx, mainBase()); return err },
		"Unadmit":    func() error { _, err := f.store.Unadmit(ctx, e.ID, 0); return err },
		"SetPhase":   func() error { _, err := f.store.SetPhase(ctx, e.ID, 0, PhasePassed); return err },
		"Reanchor":   func() error { _, err := f.store.Reanchor(ctx, e.ID, "h2", ""); return err },
		"ReanchorAt": func() error { _, err := f.store.ReanchorAt(ctx, e.ID, 0, "h2", ""); return err },
		"Settle":     func() error { _, err := f.store.Settle(ctx, e.ID, StateMerged, ""); return err },
		"SettleAt":   func() error { _, err := f.store.SettleAt(ctx, e.ID, 0, StateMerged, ""); return err },
		"ClaimMergeDispatch": func() error {
			_, err := f.store.ClaimMergeDispatch(ctx, r, DispatchPathQueue, "h1")
			return err
		},
		"MarkMergeDispatchFailed": func() error { return f.store.MarkMergeDispatchFailed(ctx, r, "x") },
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
		"AdmitNextReclaiming": func() error {
			_, err := f.store.AdmitNextReclaiming(ctx, ReclaimRequest{Base: mainBase(), AdmittedBefore: f.databaseNow(),
				Record: func(context.Context, pgx.Tx, Entry) error { return nil }})
			return err
		},
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

	got, err := f.store.MarkMergeIntent(ctx, e.ID, &MergeIntentRequest{Record: record})
	if err != nil || !got.IntentRecorded || got.Entry.MergeIntentAt == nil || got.Entry.State != StateLive || calls != 1 {
		t.Fatalf("MarkMergeIntent = %+v, %v (record calls %d); want the live entry stamped, recorded once", got, err, calls)
	}
	stamped := f.entry(e.ID)
	again, err := f.store.MarkMergeIntent(ctx, e.ID, &MergeIntentRequest{Record: record})
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
		if _, err := f.store.MarkMergeIntent(ctx, gone.ID, &MergeIntentRequest{Record: rec}); !errors.Is(err, ErrStateConflict) {
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
	_, err = f.store.MarkMergeIntent(ctx, ce.ID, &MergeIntentRequest{Record: fail})
	wantUnrecorded("MarkMergeIntent", err)
	if got := f.entry(ce.ID); got.MergeIntentAt != nil {
		t.Fatalf("row after an unrecorded MarkMergeIntent = %+v, want no intent", got)
	}
	if res, err := f.store.MarkMergeIntent(ctx, ce.ID, &MergeIntentRequest{Record: ok}); err != nil || !res.IntentRecorded || len(seen) != 3 || seen[2].Entry.ID != ce.ID {
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
		aDone <- f.store.WithMergeIntentLock(ctx, r, func(context.Context) error {
			if _, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h1", MergeIntent: true}); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	if err := f.store.WithMergeIntentLock(ctx, f.run(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("another run's lock: %v", err)
	}
	var bRan atomic.Bool
	bDone := make(chan error, 1)
	go func() {
		bDone <- f.store.WithMergeIntentLock(ctx, r, func(context.Context) error {
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
	if err := f.store.WithMergeIntentLock(context.Background(), r, func(context.Context) error { return errFn }); !errors.Is(err, errFn) {
		t.Fatalf("fn error = %v, want it returned", err)
	}
	if err := f.store.WithMergeIntentLock(context.Background(), uuid.Nil, func(context.Context) error { return nil }); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil run id = %v, want ErrInvalidRequest", err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- f.store.WithMergeIntentLock(context.Background(), r, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(100*time.Millisecond))
	defer cancel()
	ran := false
	err := f.store.WithMergeIntentLock(ctx, r, func(context.Context) error { ran = true; return nil })
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) || ran {
		t.Fatalf("waiter past its deadline = %v ran=%v, want DeadlineExceeded and fn not run", err, ran)
	}
	if err := <-aDone; err != nil {
		t.Fatalf("holder: %v", err)
	}
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := f.store.WithMergeIntentLock(canceled, r, func(context.Context) error { ran = true; return nil }); !errors.Is(err, context.Canceled) || ran {
		t.Fatalf("canceled context = %v ran=%v, want context.Canceled and fn not run", err, ran)
	}
}

// TestWithMergeIntentLock_SharesThePoolBudget: the store's holder budget is
// the pool-wide pglock budget every other pglock helper on the pool shares,
// not a store-local one. Counterfactual: a store-local budget (today's
// pre-#4591 intentHolders shape) is a different pointer, RED.
func TestWithMergeIntentLock_SharesThePoolBudget(t *testing.T) {
	f := newFixture(t)
	if f.store.locks != pglock.BudgetFor(f.pool) {
		t.Fatal("store.locks is not pglock.BudgetFor(pool): the intent lock does not share the pool-wide holder budget")
	}
	if NewPostgresStore(f.pool).locks != f.store.locks {
		t.Fatal("two stores on one pool got different budgets")
	}
}

// TestWithMergeIntentLock_FnCarriesDeadline: a caller context with NO
// deadline still hands fn one, no later than intentFnTimeout from now, and fn
// runs marked as inside a pglock lock. Counterfactual: running fn on the
// caller's context, or multiplying intentFnTimeout, fails the bound, RED.
func TestWithMergeIntentLock_FnCarriesDeadline(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	before := time.Now()
	var deadline time.Time
	var ok, marked bool
	if err := f.store.WithMergeIntentLock(context.Background(), r, func(fctx context.Context) error {
		deadline, ok = fctx.Deadline()
		marked = pglock.InLock(fctx)
		return nil
	}); err != nil {
		t.Fatalf("WithMergeIntentLock: %v", err)
	}
	if !ok {
		t.Fatal("fn's context carries no deadline under a deadline-free caller context")
	}
	// The literal, not intentFnTimeout: a bound derived from the constant
	// would scale with it and could not catch a wrong value.
	if limit := before.Add(90*time.Second + timescale.D(5*time.Second)); deadline.After(limit) {
		t.Fatalf("fn deadline %s is later than now+90s (the intent fn deadline)+slack", deadline.Sub(before))
	}
	if !marked {
		t.Fatal("fn's context is not marked as inside a pglock lock")
	}
}

// TestWithMergeIntentLock_PanicReleasesLock: a panic in fn propagates, but
// the lock is released first: a SEPARATE, non-pooled session (advisory locks
// are re-entrant per session, so a pooled probe could reuse the leaked one)
// takes it, and the next WithMergeIntentLock on the run runs fn.
// Counterfactual: an unlock that is not deferred leaks the lock, RED.
func TestWithMergeIntentLock_PanicReleasesLock(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("fn's panic did not propagate")
			}
		}()
		_ = f.store.WithMergeIntentLock(context.Background(), r, func(context.Context) error { panic("fn panicked") })
	}()
	probe, err := pgx.Connect(context.Background(), f.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("probe session: %v", err)
	}
	defer func() { _ = probe.Close(context.Background()) }()
	var took bool
	if err := probe.QueryRow(context.Background(), `SELECT pg_try_advisory_lock($1)`, intentLockKey(r)).Scan(&took); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !took {
		t.Fatal("a separate session cannot take the run's intent lock after fn panicked: the lock leaked")
	}
	if _, err := probe.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, intentLockKey(r)); err != nil {
		t.Fatalf("probe unlock: %v", err)
	}
	next, cancel := context.WithTimeout(context.Background(), timescale.D(time.Second))
	defer cancel()
	ran := false
	if err := f.store.WithMergeIntentLock(next, r, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("next holder after the panic = %v ran=%v, want the lock free", err, ran)
	}
}

// TestWithMergeIntentLock_DistinctRunsSaturatingThePoolProgress: eight merges
// of eight DISTINCT runs race on a 6-connection pool, and every fn needs a
// second connection (a store read) while its lock is held. Each fn first
// waits until as many holders as the pool has connections are inside (or a
// short bound passes), so without the holder bound six holders take every
// connection and every fn's read waits forever. With it at most
// pglock.HolderLimit = 2 holders are inside and all eight finish. The context
// deadline only bounds a RED run. Counterfactual: a holder limit equal to the
// pool size deadlocks, RED.
func TestWithMergeIntentLock_DistinctRunsSaturatingThePoolProgress(t *testing.T) {
	f := newFixture(t)
	cfg := f.pool.Config()
	cfg.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	store := NewPostgresStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(5*time.Second))
	defer cancel()
	const runs = 8
	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for i := 0; i < runs; i++ {
		r := f.run()
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.WithMergeIntentLock(ctx, r, func(context.Context) error {
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
	if m, limit := maxInside.Load(), pglock.HolderLimit(pool); int(m) > limit {
		t.Fatalf("concurrent holders = %d, want at most pglock.HolderLimit = %d on a 6-connection pool", m, limit)
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
	err := f.store.WithMergeIntentLock(ctx, r, func(context.Context) error {
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
	if !errors.Is(err, ErrIntentLockLapsed) || !errors.Is(err, pglock.ErrLockLapsed) {
		t.Fatalf("WithMergeIntentLock after its session died = %v, want ErrIntentLockLapsed wrapping pglock.ErrLockLapsed", err)
	}
	next, cancel := context.WithTimeout(ctx, timescale.D(time.Second))
	defer cancel()
	ran := false
	if err := f.store.WithMergeIntentLock(next, r, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
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
	err = store.WithMergeIntentLock(ctx, r, func(context.Context) error {
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

// TestAdmitNext_BumpsEpochOnAdmitAndReclaim pins 0108's admission epoch: a
// fresh row reads 0, every admission (the admit AND the stale re-claim)
// bumps it by one, and a no-op AdmitNext (a live entry holds the line)
// leaves it.
func TestAdmitNext_BumpsEpochOnAdmitAndReclaim(t *testing.T) {
	f := newFixture(t)
	e := f.enqueue(f.run(), "main", "h1").Entry
	if e.AdmissionEpoch != 0 {
		t.Fatalf("fresh entry epoch = %d, want 0", e.AdmissionEpoch)
	}
	if adm := f.admitNext(mainBase()); !adm.Admitted || adm.Entry.AdmissionEpoch != 1 {
		t.Fatalf("admit = %+v, want admitted at epoch 1", adm)
	}
	if adm := f.admitNext(mainBase()); adm.Admitted || f.entry(e.ID).AdmissionEpoch != 1 {
		t.Fatalf("no-op AdmitNext = %+v, epoch %d; want not admitted, epoch 1", adm, f.entry(e.ID).AdmissionEpoch)
	}
	f.exec(`UPDATE merge_candidate_queue_entries SET admitted_at = clock_timestamp() - make_interval(secs => $2) WHERE id = $1`,
		e.ID, (AdmissionStaleAfter + time.Minute).Seconds())
	if adm := f.admitNext(mainBase()); !adm.Readmitted || adm.Entry.AdmissionEpoch != 2 || f.entry(e.ID).AdmissionEpoch != 2 {
		t.Fatalf("stale re-claim = %+v, want readmitted at epoch 2", adm)
	}
	if _, err := f.store.Unadmit(context.Background(), e.ID, 2); err != nil {
		t.Fatalf("Unadmit(2): %v", err)
	}
	if adm := f.admitNext(mainBase()); !adm.Admitted || adm.Entry.AdmissionEpoch != 3 {
		t.Fatalf("re-admission = %+v, want admitted at epoch 3", adm)
	}
}

// TestUnadmit_StaleEpochConflicts is the ADR-093 slice 2 stale-decision race
// at the store: admit (epoch 1), Unadmit(1), re-admit (epoch 2), then a
// stale Unadmit(1) is ErrStateConflict and the COMMITTED row is still live at
// epoch 2 with its admitted_at (read back, not inferred from the error).
func TestUnadmit_StaleEpochConflicts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	if adm := f.admitNext(mainBase()); adm.Entry.AdmissionEpoch != 1 {
		t.Fatalf("admit epoch = %d, want 1", adm.Entry.AdmissionEpoch)
	}
	if _, err := f.store.Unadmit(ctx, e.ID, 1); err != nil {
		t.Fatalf("Unadmit(1): %v", err)
	}
	f.admitNext(mainBase())
	before := f.entry(e.ID)
	if _, err := f.store.Unadmit(ctx, e.ID, 1); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale Unadmit(1) err = %v, want ErrStateConflict", err)
	}
	after := f.entry(e.ID)
	if after.State != StateLive || after.AdmissionEpoch != 2 || after.AdmittedAt == nil || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("entry after stale Unadmit = %+v, want untouched live at epoch 2", after)
	}
}

// TestSetPhase_StaleEpochConflicts: a stale epoch or a held entry is refused
// and the committed phase is unchanged.
func TestSetPhase_StaleEpochConflicts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	if _, err := f.store.SetPhase(ctx, e.ID, 0, PhasePassed); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("SetPhase(held) err = %v, want ErrStateConflict", err)
	}
	f.admitNext(mainBase())
	if _, err := f.store.SetPhase(ctx, e.ID, 0, PhasePassed); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("SetPhase(stale epoch 0) err = %v, want ErrStateConflict", err)
	}
	if got := f.entry(e.ID); got.Phase != PhaseAdmitted {
		t.Fatalf("phase after stale SetPhase = %q, want %q (untouched)", got.Phase, PhaseAdmitted)
	}
	if got, err := f.store.SetPhase(ctx, e.ID, 1, PhasePassed); err != nil || f.entry(e.ID).Phase != PhasePassed || got.Phase != PhasePassed {
		t.Fatalf("SetPhase(1) = %+v, %v; want passed", got, err)
	}
}

// TestSettleAt_StaleEpochConflicts: SettleAt settles only the live entry at
// its epoch; a stale epoch or a held entry is ErrStateConflict with the row
// still active, and a terminal row answers Settled=false like Settle.
func TestSettleAt_StaleEpochConflicts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	if _, err := f.store.SettleAt(ctx, e.ID, 0, StateDropped, "pr_closed"); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("SettleAt(held) err = %v, want ErrStateConflict", err)
	}
	if got := f.entry(e.ID); got.State != StateHeld || got.SettledAt != nil {
		t.Fatalf("held entry after SettleAt = %+v, want untouched", got)
	}
	f.admitNext(mainBase())
	if _, err := f.store.SettleAt(ctx, e.ID, 0, StateDropped, "pr_closed"); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("SettleAt(stale epoch) err = %v, want ErrStateConflict", err)
	}
	if got := f.entry(e.ID); got.State != StateLive || got.SettledAt != nil {
		t.Fatalf("live entry after stale SettleAt = %+v, want untouched", got)
	}
	res, err := f.store.SettleAt(ctx, e.ID, 1, StateEjected, "verify_failed")
	if err != nil || !res.Settled || f.entry(e.ID).State != StateEjected {
		t.Fatalf("SettleAt(1) = %+v, %v; want settled ejected", res, err)
	}
	again, err := f.store.SettleAt(ctx, e.ID, 1, StateDropped, "pr_closed")
	if err != nil || again.Settled || again.Entry.State != StateEjected || again.Entry.EjectReason != "verify_failed" {
		t.Fatalf("SettleAt(terminal) = %+v, %v; want unsettled, first state kept", again, err)
	}
	if _, err := f.store.SettleAt(ctx, e.ID, 1, StateDropped, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("SettleAt(no reason) err = %v, want ErrInvalidRequest", err)
	}
}

// TestReanchorAt_StaleEpochConflicts: ReanchorAt moves only the live entry
// at its epoch; a stale epoch is ErrStateConflict with the anchor unchanged.
func TestReanchorAt_StaleEpochConflicts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	if _, err := f.store.ReanchorAt(ctx, e.ID, 0, "h2", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("ReanchorAt(held) err = %v, want ErrStateConflict", err)
	}
	f.admitNext(mainBase())
	if _, err := f.store.ReanchorAt(ctx, e.ID, 0, "h2", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("ReanchorAt(stale) err = %v, want ErrStateConflict", err)
	}
	if got := f.entry(e.ID); got.AnchoredHeadSHA != "h1" {
		t.Fatalf("anchor after stale ReanchorAt = %q, want h1 (untouched)", got.AnchoredHeadSHA)
	}
	got, err := f.store.ReanchorAt(ctx, e.ID, 1, "h2", "")
	if err != nil || got.AnchoredHeadSHA != "h2" || f.entry(e.ID).AnchoredHeadSHA != "h2" || got.AdmissionEpoch != 1 {
		t.Fatalf("ReanchorAt(1) = %+v, %v; want head h2 at epoch 1", got, err)
	}
}

// TestEnqueue_StampsIntentSequenceOnce: a MergeIntent insert stamps the
// sequence with merge_intent_at; a non-intent insert stamps neither; a
// same-base repeat with another sequence never overwrites it.
func TestEnqueue_StampsIntentSequenceOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.run()
	res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h1", MergeIntent: true, IntentSequence: 41})
	if err != nil || !res.IntentRecorded || res.Entry.MergeIntentSequence != 41 || f.entry(res.Entry.ID).MergeIntentSequence != 41 {
		t.Fatalf("intent Enqueue = %+v, %v; want sequence 41", res, err)
	}
	again, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h2", MergeIntent: true, IntentSequence: 99})
	if err != nil || again.IntentRecorded || f.entry(res.Entry.ID).MergeIntentSequence != 41 {
		t.Fatalf("repeat Enqueue = %+v, %v; sequence %d, want 41 kept", again, err, f.entry(res.Entry.ID).MergeIntentSequence)
	}
	plain, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: f.run(), BaseRef: "main", HeadSHA: "x1", IntentSequence: 7})
	if err != nil || plain.Entry.MergeIntentAt != nil || plain.Entry.MergeIntentSequence != 0 {
		t.Fatalf("non-intent Enqueue = %+v, %v; want no intent, no sequence", plain, err)
	}
}

// TestMarkMergeIntent_StampsSequenceOnlyOnTransition: the sequence is
// stamped on the NULL-to-set transition and a repeat (another sequence)
// writes nothing.
func TestMarkMergeIntent_StampsSequenceOnlyOnTransition(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	got, err := f.store.MarkMergeIntent(ctx, e.ID, &MergeIntentRequest{IntentSequence: 12})
	if err != nil || !got.IntentRecorded || f.entry(e.ID).MergeIntentSequence != 12 {
		t.Fatalf("MarkMergeIntent = %+v, %v; want sequence 12", got, err)
	}
	again, err := f.store.MarkMergeIntent(ctx, e.ID, &MergeIntentRequest{IntentSequence: 13})
	if err != nil || again.IntentRecorded || f.entry(e.ID).MergeIntentSequence != 12 {
		t.Fatalf("repeat MarkMergeIntent = %+v, %v; want 12 kept", again, err)
	}
}

// TestMarkMergeIntent_NilRequestStampsNothing: a nil request records the
// intent with no sequence (NULL, read as 0) and no callback.
func TestMarkMergeIntent_NilRequestStampsNothing(t *testing.T) {
	f := newFixture(t)
	e := f.enqueue(f.run(), "main", "h1").Entry
	got, err := f.store.MarkMergeIntent(context.Background(), e.ID, nil)
	if err != nil || !got.IntentRecorded || f.entry(e.ID).MergeIntentAt == nil || f.entry(e.ID).MergeIntentSequence != 0 {
		t.Fatalf("MarkMergeIntent(nil) = %+v, %v; want intent, sequence 0", got, err)
	}
	if n := f.count(`id = $1 AND merge_intent_sequence IS NULL`, e.ID); n != 1 {
		t.Fatalf("merge_intent_sequence NULL rows = %d, want 1", n)
	}
}

// TestClaimMergeDispatch_OnePerRunConcurrently is approval condition D1 at
// the store, with NO lock held: N concurrent claims for one run across the
// three paths admit exactly one; every other is ErrAlreadyDispatched carrying
// the winning claim. The UNIQUE run_id constraint alone decides it.
func TestClaimMergeDispatch_OnePerRunConcurrently(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	paths := []string{DispatchPathQueue, DispatchPathInline, DispatchPathDelegated}
	const n = 9
	var wg sync.WaitGroup
	var mu sync.Mutex
	var won []DispatchClaim
	var refused int
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			c, err := f.store.ClaimMergeDispatch(context.Background(), r, paths[i%3], "h1")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won = append(won, c)
			case errors.Is(err, ErrAlreadyDispatched):
				refused++
			default:
				t.Errorf("claim %d: unexpected err %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if len(won) != 1 || refused != n-1 {
		t.Fatalf("claims won = %d, refused = %d; want exactly 1 and %d (D1 UNIQUE run_id)", len(won), refused, n-1)
	}
	if rows := f.countClaims(r); rows != 1 {
		t.Fatalf("merge_dispatch_claims rows = %d, want 1", rows)
	}
	existing, err := f.store.ClaimMergeDispatch(context.Background(), r, DispatchPathInline, "h2")
	if !errors.Is(err, ErrAlreadyDispatched) || existing.Path != won[0].Path || existing.State != DispatchDispatched || existing.HeadSHA != "h1" {
		t.Fatalf("repeat claim = %+v, %v; want ErrAlreadyDispatched carrying the winning claim", existing, err)
	}
}

// TestMarkMergeDispatchFailed_UpdatesNeverDeletes: a failed dispatch moves
// the claim to dispatch_failed (the row stays), the first cause is kept, and
// a later claim is still refused until the operator re-arm deletes the row.
func TestMarkMergeDispatchFailed_UpdatesNeverDeletes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.run()
	if err := f.store.MarkMergeDispatchFailed(ctx, r, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkMergeDispatchFailed(no claim) err = %v, want ErrNotFound", err)
	}
	if _, err := f.store.ClaimMergeDispatch(ctx, r, DispatchPathQueue, "h1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := f.store.MarkMergeDispatchFailed(ctx, r, "405 not mergeable"); err != nil {
		t.Fatalf("MarkMergeDispatchFailed: %v", err)
	}
	if err := f.store.MarkMergeDispatchFailed(ctx, r, "second"); err != nil {
		t.Fatalf("repeat MarkMergeDispatchFailed: %v", err)
	}
	c, err := f.store.ClaimMergeDispatch(ctx, r, DispatchPathInline, "h1")
	if !errors.Is(err, ErrAlreadyDispatched) || c.State != DispatchFailed || c.FailureCause != "405 not mergeable" || c.FailedAt == nil {
		t.Fatalf("claim after failure = %+v, %v; want ErrAlreadyDispatched on dispatch_failed with the first cause", c, err)
	}
	if rows := f.countClaims(r); rows != 1 {
		t.Fatalf("claim rows after failure = %d, want 1 (updated, never deleted)", rows)
	}
	// The operator re-arm (README.md § "Merge-dispatch ledger").
	f.exec(`DELETE FROM merge_dispatch_claims WHERE run_id = $1 AND state = 'dispatch_failed'`, r)
	if _, err := f.store.ClaimMergeDispatch(ctx, r, DispatchPathInline, "h2"); err != nil {
		t.Fatalf("claim after the operator re-arm: %v", err)
	}
}

// TestClaimMergeDispatch_RefusesBeforeSQL: malformed claims and an unknown
// run.
func TestClaimMergeDispatch_RefusesBeforeSQL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.run()
	for name, call := range map[string]func() error{
		"nil run":  func() error { _, err := f.store.ClaimMergeDispatch(ctx, uuid.Nil, DispatchPathQueue, "h"); return err },
		"no head":  func() error { _, err := f.store.ClaimMergeDispatch(ctx, r, DispatchPathQueue, ""); return err },
		"bad path": func() error { _, err := f.store.ClaimMergeDispatch(ctx, r, "auto", "h"); return err },
		"mark nil": func() error { return f.store.MarkMergeDispatchFailed(ctx, uuid.Nil, "x") },
	} {
		if err := call(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if f.countClaims(r) != 0 {
		t.Fatal("a refused claim wrote a row")
	}
	if _, err := f.store.ClaimMergeDispatch(ctx, uuid.New(), DispatchPathQueue, "h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run err = %v, want ErrNotFound", err)
	}
}

func (f *fixture) countClaims(runID uuid.UUID) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM merge_dispatch_claims WHERE run_id = $1`, runID).Scan(&n); err != nil {
		f.t.Fatalf("count claims: %v", err)
	}
	return n
}

// failClaimReadTracer cancels the context of ClaimMergeDispatch's claim
// read-back (the SELECT after a 23505) and of nothing else, so only that
// follow-up read fails.
type failClaimReadTracer struct{}

func (failClaimReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(d.SQL, "SELECT run_id, state, path, head_sha") && strings.Contains(d.SQL, "FROM merge_dispatch_claims") {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return c
	}
	return ctx
}

func (failClaimReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestClaimMergeDispatch_DuplicateWhoseReadBackFailsNamesUnknown (#53abff93):
// a 23505 whose follow-up claim read fails is still ErrAlreadyDispatched
// (fail closed), returned with the run id and an explicit unknown state, path
// and head rather than a zero claim, and the existing claim is untouched.
func TestClaimMergeDispatch_DuplicateWhoseReadBackFailsNamesUnknown(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.NewURL(t)
	plain, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(plain.Close)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Tracer = failClaimReadTracer{}
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	f := &fixture{t: t, pool: plain, repo: run.NewPostgresRepository(plain), store: NewPostgresStore(plain)}
	runID := f.run()
	if _, err := f.store.ClaimMergeDispatch(ctx, runID, DispatchPathQueue, "h1"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	c, err := NewPostgresStore(traced).ClaimMergeDispatch(ctx, runID, DispatchPathInline, "h2")
	if !errors.Is(err, ErrAlreadyDispatched) {
		t.Fatalf("duplicate claim err = %v, want ErrAlreadyDispatched", err)
	}
	if c.RunID != runID || c.State != DispatchUnknown || c.Path != DispatchUnknownField || c.HeadSHA != DispatchUnknownField {
		t.Fatalf("claim = %+v, want the run id with an unknown state, path and head", c)
	}
	existing, found, err := f.store.MergeDispatch(ctx, runID)
	if err != nil || !found || existing.State != DispatchDispatched || existing.Path != DispatchPathQueue || existing.HeadSHA != "h1" {
		t.Fatalf("existing claim = %+v found=%v err=%v, want the queue claim intact", existing, found, err)
	}
}

// --- ADR-112 R3: approval wait, yield marker (0109) ---

// markYielded marks a held row BY CONSTRUCTION (never through the store),
// yielded `ago` before the database clock, and returns the stored yielded_at.
func (f *fixture) markYielded(id uuid.UUID, reason string, ago time.Duration) time.Time {
	f.t.Helper()
	f.exec(`UPDATE merge_candidate_queue_entries
SET yield_reason = $2, yield_detail = 'seeded', yielded_at = clock_timestamp() - make_interval(secs => $3::float8)
WHERE id = $1`, id, reason, ago.Seconds())
	e := f.entry(id)
	if e.YieldedAt == nil {
		f.t.Fatalf("seeded marker on %s did not land", id)
	}
	return *e.YieldedAt
}

// admitAt admits the base's next entry and returns its epoch, failing unless
// want was the one admitted.
func (f *fixture) admitAt(want uuid.UUID) int64 {
	f.t.Helper()
	adm := f.admitNext(mainBase())
	if !adm.Admitted || adm.Entry.ID != want {
		f.t.Fatalf("AdmitNext = %+v, want %s admitted", adm, want)
	}
	return adm.Entry.AdmissionEpoch
}

func (f *fixture) approvalWait(id uuid.UUID, epoch int64, window time.Duration) ApprovalWaitResult {
	f.t.Helper()
	res, err := f.store.ApprovalWait(context.Background(), id, epoch, window)
	if err != nil {
		f.t.Fatalf("ApprovalWait(%s, %d): %v", id, epoch, err)
	}
	return res
}

// TestApprovalWait_AnchorSurvivesUnadmitAndReadmission (CF1 vehicle): one
// row's wait anchor is stamped by the first ApprovalWait and survives an
// Unadmit (merge-on-pass readvance), the re-admission and a stale re-claim,
// so every later ApprovalWait returns the SAME start; expiry is judged in SQL
// against that anchor. With the anchor reset by Unadmit, or stamped without
// COALESCE, the second start is a later database instant: RED.
func TestApprovalWait_AnchorSurvivesUnadmitAndReadmission(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	if _, err := f.store.ApprovalWait(ctx, e.ID, 0, time.Hour); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("ApprovalWait(held) err = %v, want ErrStateConflict", err)
	}
	if f.entry(e.ID).ApprovalWaitStartedAt != nil {
		t.Fatal("a refused ApprovalWait stamped the wait anchor")
	}
	epoch := f.admitAt(e.ID)
	first := f.approvalWait(e.ID, epoch, time.Hour)
	if first.Entry.ApprovalWaitStartedAt == nil || first.Expired ||
		first.Remaining <= 59*time.Minute || first.Remaining > time.Hour {
		t.Fatalf("first ApprovalWait = %+v, want an anchor, not expired, ~1h remaining", first)
	}
	start := *first.Entry.ApprovalWaitStartedAt

	if _, err := f.store.Unadmit(ctx, e.ID, epoch); err != nil {
		t.Fatalf("Unadmit: %v", err)
	}
	if got := f.entry(e.ID).ApprovalWaitStartedAt; got == nil || !got.Equal(start) {
		t.Fatalf("anchor after Unadmit = %v, want %v (never reset on readvance)", got, start)
	}
	epoch = f.admitAt(e.ID)
	second := f.approvalWait(e.ID, epoch, time.Hour)
	if second.Entry.ApprovalWaitStartedAt == nil || !second.Entry.ApprovalWaitStartedAt.Equal(start) {
		t.Fatalf("anchor after re-admission = %v, want the first start %v", second.Entry.ApprovalWaitStartedAt, start)
	}

	// A stale re-claim keeps it too.
	f.exec(`UPDATE merge_candidate_queue_entries SET admitted_at = clock_timestamp() - make_interval(secs => $2) WHERE id = $1`,
		e.ID, (AdmissionStaleAfter + time.Minute).Seconds())
	adm := f.admitNext(mainBase())
	if !adm.Readmitted {
		t.Fatalf("stale re-claim = %+v, want readmitted", adm)
	}
	if got := f.approvalWait(e.ID, adm.Entry.AdmissionEpoch, time.Hour).Entry.ApprovalWaitStartedAt; got == nil || !got.Equal(start) {
		t.Fatalf("anchor after stale re-claim = %v, want %v", got, start)
	}

	// A stale epoch is refused and changes nothing.
	before := f.entry(e.ID)
	if _, err := f.store.ApprovalWait(ctx, e.ID, epoch, time.Hour); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale ApprovalWait err = %v, want ErrStateConflict", err)
	}
	if after := f.entry(e.ID); !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("stale ApprovalWait wrote the row: %+v", after)
	}

	// Expiry: the anchor backdated past the window (database clock).
	f.exec(`UPDATE merge_candidate_queue_entries SET approval_wait_started_at = clock_timestamp() - interval '2 hours' WHERE id = $1`, e.ID)
	if res := f.approvalWait(e.ID, adm.Entry.AdmissionEpoch, time.Hour); !res.Expired || res.Remaining != 0 {
		t.Fatalf("ApprovalWait past the window = %+v, want expired with 0 remaining", res)
	}
	if res := f.approvalWait(e.ID, adm.Entry.AdmissionEpoch, 3*time.Hour); res.Expired || res.Remaining <= 59*time.Minute {
		t.Fatalf("ApprovalWait inside a 3h window = %+v, want not expired, ~1h remaining", res)
	}

	for _, w := range []time.Duration{0, -time.Minute} {
		if _, err := f.store.ApprovalWait(ctx, e.ID, adm.Entry.AdmissionEpoch, w); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("ApprovalWait(window %s) err = %v, want ErrInvalidRequest", w, err)
		}
	}
}

// TestAdmitNext_SkipsYieldedRows (CF2 vehicle): the yielded row is the OLDEST
// by enqueued_at, so plain FIFO would admit it; AdmitNext admits the younger
// unmarked row instead, and a base holding only yielded rows admits nothing.
func TestAdmitNext_SkipsYieldedRows(t *testing.T) {
	f := newFixture(t)
	yielded, younger := uuid.New(), uuid.New()
	f.seed(yielded, f.run(), StateHeld, 2*time.Hour)
	f.seed(younger, f.run(), StateHeld, time.Hour)
	f.markYielded(yielded, "forge_approval_absent", time.Minute)

	adm := f.admitNext(mainBase())
	if !adm.Admitted || adm.Entry.ID != younger {
		t.Fatalf("AdmitNext = %+v, want the younger unmarked row %s (the yielded %s skipped)", adm, younger, yielded)
	}
	if got := f.entry(yielded); got.State != StateHeld || !got.Yielded() {
		t.Fatalf("yielded row after admission = %+v, want still held and marked", got)
	}

	// A base with only yielded rows admits nothing.
	other := Base{Repo: testRepo, BaseRef: "release"}
	only := f.enqueue(f.run(), "release", "r1").Entry
	f.markYielded(only.ID, "forge_approval_vetoed", time.Minute)
	if adm := f.admitNext(other); adm.Admitted || adm.Entry != nil {
		t.Fatalf("AdmitNext(only yielded) = %+v, want no admission and no entry", adm)
	}
	if got := f.entry(only.ID); got.State != StateHeld {
		t.Fatalf("only-yielded row = %+v, want held", got)
	}
}

// TestYield_OneTransactionUnderEpochCAS: a stale epoch is refused and the
// COMMITTED row is still live and unmarked (read back); the current epoch
// moves the row to held with the marker in one statement, keeping enqueued_at
// and the wait anchor.
func TestYield_OneTransactionUnderEpochCAS(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	marker := YieldMarker{Reason: "forge_approval_dismissed", Detail: "approval dismissed by a push"}
	if _, err := f.store.Yield(ctx, e.ID, 0, marker); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("Yield(held) err = %v, want ErrStateConflict", err)
	}
	if f.entry(e.ID).Yielded() {
		t.Fatal("a refused Yield on a held row marked it")
	}
	stale := f.admitAt(e.ID)
	if _, err := f.store.Unadmit(ctx, e.ID, stale); err != nil {
		t.Fatalf("Unadmit: %v", err)
	}
	epoch := f.admitAt(e.ID)
	wait := f.approvalWait(e.ID, epoch, time.Hour)

	if _, err := f.store.Yield(ctx, e.ID, stale, marker); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale Yield err = %v, want ErrStateConflict", err)
	}
	if got := f.entry(e.ID); got.State != StateLive || got.Yielded() || got.YieldReason != "" || got.AdmissionEpoch != epoch {
		t.Fatalf("entry after stale Yield = %+v, want untouched live, unmarked, epoch %d", got, epoch)
	}
	if _, err := f.store.Yield(ctx, e.ID, epoch, YieldMarker{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Yield(empty reason) err = %v, want ErrInvalidRequest", err)
	}

	got, err := f.store.Yield(ctx, e.ID, epoch, marker)
	if err != nil {
		t.Fatalf("Yield: %v", err)
	}
	read := f.entry(e.ID)
	for _, g := range []Entry{got, read} {
		if g.State != StateHeld || g.Phase != "" || g.AdmittedAt != nil || !g.EnqueuedAt.Equal(e.EnqueuedAt) ||
			g.YieldReason != marker.Reason || g.YieldDetail != marker.Detail || g.YieldedAt == nil ||
			g.ApprovalWaitStartedAt == nil || !g.ApprovalWaitStartedAt.Equal(*wait.Entry.ApprovalWaitStartedAt) {
			t.Fatalf("yielded entry = %+v, want held, no phase/admitted_at, same enqueued_at, the marker, the wait anchor kept", g)
		}
	}
	if _, err := f.store.Yield(ctx, e.ID, epoch, marker); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("second Yield err = %v, want ErrStateConflict (no longer live)", err)
	}

	// YieldAge is computed on the database clock; 0 when unmarked.
	f.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = clock_timestamp() - interval '1 hour' WHERE id = $1`, e.ID)
	if age := f.entry(e.ID).YieldAge; age < time.Hour || age > time.Hour+time.Minute {
		t.Fatalf("YieldAge = %s, want ~1h", age)
	}
	if age := f.enqueue(f.run(), "main", "u1").Entry.YieldAge; age != 0 {
		t.Fatalf("unmarked YieldAge = %s, want 0", age)
	}
}

// TestClearYield_CompareAndSwap (CF3 vehicle): a stale yield revision and a
// stale head (after a marker-preserving reanchor) are both refused, and the
// marker is still set on READ-BACK (error identity alone is not the
// assertion); the matching keys clear the marker and close the approval wait
// while RETAINING its anchor (docs/adr/112 A3.7), and the row is admitted
// ahead of a younger held row (FIFO at enqueued_at).
func TestClearYield_CompareAndSwap(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.run(), f.run()
	ea := f.enqueue(a, "main", "h1").Entry
	eb := f.enqueue(b, "main", "x1").Entry
	epoch := f.admitAt(ea.ID)
	f.approvalWait(ea.ID, epoch, time.Hour)
	y, err := f.store.Yield(ctx, ea.ID, epoch, YieldMarker{Reason: "forge_approval_absent"})
	if err != nil {
		t.Fatalf("Yield: %v", err)
	}
	yieldedAt, rev := *y.YieldedAt, y.YieldRevision

	stillMarked := func(why string) {
		t.Helper()
		if got := f.entry(ea.ID); !got.Yielded() || !got.YieldedAt.Equal(yieldedAt) || got.ApprovalWaitStartedAt == nil {
			t.Fatalf("%s: entry = %+v, want the marker (yielded at %v) and wait anchor still set", why, got, yieldedAt)
		}
	}
	if _, err := f.store.ClearYield(ctx, ea.ID, "h1", rev-1, false); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("ClearYield(prior revision) err = %v, want ErrStateConflict", err)
	}
	stillMarked("stale revision")

	// A marker-less re-enqueue at a new head moves the anchor and keeps the
	// marker, so a probe judged at the OLD head is stale.
	if res := f.enqueue(a, "main", "h2"); res.Outcome != EnqueueReanchored || res.Marked {
		t.Fatalf("marker-less re-enqueue = %+v, want reanchored, not marked by this call", res)
	}
	stillMarked("marker-less reanchor")
	if _, err := f.store.ClearYield(ctx, ea.ID, "h1", rev, false); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("ClearYield(stale head) err = %v, want ErrStateConflict", err)
	}
	stillMarked("stale head")
	if _, err := f.store.ClearYield(ctx, eb.ID, "x1", 0, false); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("ClearYield(unmarked row) err = %v, want ErrStateConflict", err)
	}
	if _, err := f.store.ClearYield(ctx, ea.ID, "", rev, false); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ClearYield(no head) err = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.store.ClearYield(ctx, ea.ID, "h2", -1, false); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ClearYield(negative revision) err = %v, want ErrInvalidRequest", err)
	}

	cleared, err := f.store.ClearYield(ctx, ea.ID, "h2", rev, false)
	if err != nil {
		t.Fatalf("ClearYield: %v", err)
	}
	if cleared.Yielded() || cleared.YieldReason != "" || cleared.ApprovalWaitStartedAt == nil || cleared.ApprovalWaitOpen ||
		!cleared.EnqueuedAt.Equal(ea.EnqueuedAt) {
		t.Fatalf("cleared entry = %+v, want no marker, the wait anchor retained and closed, same enqueued_at", cleared)
	}
	f.admitAt(ea.ID) // ahead of the younger B: FIFO at its original enqueued_at
}

// TestReanchorYielded_KeepsMarkerAndEpisode (ADR-112 approval condition C2):
// the head moves after the yield; ReanchorYielded on the OLD head moves the
// anchor and KEEPS the marker, yielded_at and the wait anchor (never a clear,
// so a push buys no fresh window); a stale probe on the old head is refused;
// the new head's qualifying approval then clears it at the NEW head and the
// row rejoins FIFO. With ReanchorYielded a no-op, the clear at the new head
// is refused and the row stays marked: RED.
func TestReanchorYielded_KeepsMarkerAndEpisode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.run(), f.run()
	ea := f.enqueue(a, "main", "h1").Entry
	f.enqueue(b, "main", "x1")
	epoch := f.admitAt(ea.ID)
	wait := f.approvalWait(ea.ID, epoch, time.Hour)
	y, err := f.store.Yield(ctx, ea.ID, epoch, YieldMarker{Reason: "forge_approval_dismissed", Detail: "d"})
	if err != nil {
		t.Fatalf("Yield: %v", err)
	}
	yieldedAt := *y.YieldedAt

	moved, err := f.store.ReanchorYielded(ctx, ea.ID, "h1", y.YieldRevision, "h2", "b2")
	if err != nil {
		t.Fatalf("ReanchorYielded: %v", err)
	}
	read := f.entry(ea.ID)
	for _, g := range []Entry{moved, read} {
		if g.AnchoredHeadSHA != "h2" || g.AnchoredBaseSHA != "b2" || g.State != StateHeld ||
			!g.Yielded() || !g.YieldedAt.Equal(yieldedAt) || g.YieldReason != "forge_approval_dismissed" ||
			g.ApprovalWaitStartedAt == nil || !g.ApprovalWaitStartedAt.Equal(*wait.Entry.ApprovalWaitStartedAt) {
			t.Fatalf("re-anchored yielded entry = %+v, want head h2, the same marker, yielded_at and wait anchor", g)
		}
	}
	if _, err := f.store.ReanchorYielded(ctx, ea.ID, "h1", moved.YieldRevision, "h3", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale ReanchorYielded err = %v, want ErrStateConflict", err)
	}
	if got := f.entry(ea.ID); got.AnchoredHeadSHA != "h2" {
		t.Fatalf("stale ReanchorYielded moved the head to %q", got.AnchoredHeadSHA)
	}
	if _, err := f.store.ReanchorYielded(ctx, ea.ID, "h2", moved.YieldRevision, "", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ReanchorYielded(no new head) err = %v, want ErrInvalidRequest", err)
	}
	if adm := f.admitNext(mainBase()); !adm.Admitted || adm.Entry.ID == ea.ID {
		t.Fatalf("AdmitNext with A re-anchored but marked = %+v, want B admitted (A still skipped)", adm)
	}

	// The new head's approval clears at the NEW head; A rejoins at its place.
	if _, err := f.store.ClearYield(ctx, ea.ID, "h2", moved.YieldRevision, false); err != nil {
		t.Fatalf("ClearYield at the new head: %v", err)
	}
	if got := f.entry(ea.ID); got.Yielded() {
		t.Fatalf("entry after the new head's clear = %+v, want unmarked", got)
	}
	if st := f.status(a); st.Position != 1 {
		t.Fatalf("cleared A position = %d, want 1 (FIFO at its enqueued_at)", st.Position)
	}
}

// TestEnqueue_MarkerlessReanchorPreservesMarker (approval condition C2(c)): an
// Enqueue or a by-entry Reanchor WITHOUT a Yield marker (the ADR-092
// rebase-path re-enqueue) keeps an existing marker, its yielded_at and the
// wait anchor: it never clears or moves one.
func TestEnqueue_MarkerlessReanchorPreservesMarker(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.run()
	e := f.enqueue(a, "main", "h1").Entry
	yieldedAt := f.markYielded(e.ID, "forge_approval_absent", time.Minute)
	f.exec(`UPDATE merge_candidate_queue_entries SET approval_wait_started_at = clock_timestamp() - interval '40 minutes' WHERE id = $1`, e.ID)
	anchor := *f.entry(e.ID).ApprovalWaitStartedAt

	check := func(why, head string) {
		t.Helper()
		got := f.entry(e.ID)
		if got.AnchoredHeadSHA != head || !got.Yielded() || !got.YieldedAt.Equal(yieldedAt) ||
			got.YieldReason != "forge_approval_absent" || got.YieldDetail != "seeded" ||
			got.ApprovalWaitStartedAt == nil || !got.ApprovalWaitStartedAt.Equal(anchor) {
			t.Fatalf("%s: entry = %+v, want head %s with the marker and wait anchor untouched", why, got, head)
		}
	}
	if res := f.enqueue(a, "main", "h2"); res.Marked || !res.AnchorChanged {
		t.Fatalf("marker-less re-enqueue = %+v, want re-anchored, Marked false", res)
	}
	check("marker-less Enqueue", "h2")
	if _, err := f.store.Reanchor(ctx, e.ID, "h3", ""); err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	check("by-entry Reanchor", "h3")
	if _, err := f.store.MarkMergeIntent(ctx, e.ID, nil); err != nil {
		t.Fatalf("MarkMergeIntent: %v", err)
	}
	check("MarkMergeIntent", "h3")
}

// TestEnqueue_BaseChangeStartsNewWaitEpisode (first-officer ruling D,
// #4632): a base change ENDS the approval-wait episode. A YIELDED row on main
// (marker set, wait anchor backdated an hour) is re-enqueued on release
// WITHOUT a Yield marker (the ADR-092 rebase path): the old row is dropped
// base_changed, and the run's new row lands at release's TAIL (behind a row
// already held there), UNMARKED, with no approval-wait anchor. R3's no-reset
// rule applies within one base (TestEnqueue_MarkerlessReanchorPreservesMarker
// keeps the marker on a same-base head move). Counterfactuals: carrying the
// marker, or the old approval_wait_started_at, onto the new row, RED.
func TestEnqueue_BaseChangeStartsNewWaitEpisode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.run()
	e := f.enqueue(a, "main", "h1").Entry
	ahead := f.enqueue(f.run(), "release", "r1").Entry
	epoch := f.admitAt(e.ID)
	f.approvalWait(e.ID, epoch, time.Hour)
	if _, err := f.store.Yield(ctx, e.ID, epoch, YieldMarker{Reason: "forge_approval_absent", Detail: "no approval"}); err != nil {
		t.Fatalf("Yield: %v", err)
	}
	f.exec(`UPDATE merge_candidate_queue_entries SET approval_wait_started_at = approval_wait_started_at - interval '1 hour' WHERE id = $1`, e.ID)
	old := f.entry(e.ID)
	if !old.Yielded() || old.ApprovalWaitStartedAt == nil {
		t.Fatalf("precondition: %+v, want yielded with a wait anchor", old)
	}

	res := f.enqueue(a, "release", "h1")

	if res.Outcome != EnqueueBaseChanged || res.Dropped == nil || res.Dropped.ID != e.ID || res.Dropped.State != StateDropped || res.Marked {
		t.Fatalf("base-change Enqueue = %+v, want EnqueueBaseChanged dropping %s, not marked", res, e.ID)
	}
	got := f.entry(res.Entry.ID)
	if got.ID == e.ID || got.BaseRef != "release" || got.State != StateHeld {
		t.Fatalf("new row = %+v, want a fresh held row on release", got)
	}
	if got.Yielded() || got.YieldReason != "" || got.YieldDetail != "" {
		t.Fatalf("new row marker = %q/%q yielded_at %v, want unmarked (a new episode)", got.YieldReason, got.YieldDetail, got.YieldedAt)
	}
	if got.ApprovalWaitStartedAt != nil && !got.ApprovalWaitStartedAt.After(*old.ApprovalWaitStartedAt) {
		t.Fatalf("new row wait anchor = %v, want NULL or newer than the old %v", got.ApprovalWaitStartedAt, old.ApprovalWaitStartedAt)
	}
	list, err := f.store.ListForBase(ctx, Base{Repo: testRepo, BaseRef: "release"})
	if err != nil {
		t.Fatalf("ListForBase(release): %v", err)
	}
	if len(list) != 2 || list[0].ID != ahead.ID || list[1].ID != got.ID {
		t.Fatalf("release line = %+v, want [%s, %s] (the moved row at the tail)", list, ahead.ID, got.ID)
	}
}

// TestEnqueue_YieldMarkerNeverOnLiveRow (approval condition C10): seed a LIVE
// same-base entry, Enqueue it with a Yield marker (and merge intent), and READ
// BACK yielded_at IS NULL: merge_run never marks the entry holding the line,
// while the intent is still recorded and Marked is false.
func TestEnqueue_YieldMarkerNeverOnLiveRow(t *testing.T) {
	f := newFixture(t)
	a := f.run()
	e := f.enqueue(a, "main", "h1").Entry
	f.admitAt(e.ID)
	res, err := f.store.Enqueue(context.Background(), EnqueueRequest{RunID: a, BaseRef: "main", HeadSHA: "h1",
		MergeIntent: true, Yield: &YieldMarker{Reason: "forge_approval_absent", Detail: "no approval"}})
	if err != nil {
		t.Fatalf("Enqueue(live, marker): %v", err)
	}
	if res.Marked || !res.IntentRecorded || res.Entry.State != StateLive {
		t.Fatalf("Enqueue(live, marker) = %+v, want Marked false, intent recorded, still live", res)
	}
	if f.count(`id = $1 AND yielded_at IS NULL AND yield_reason IS NULL AND state = 'live'`, e.ID) != 1 {
		t.Fatalf("live entry after a marked Enqueue = %+v, want live with yielded_at NULL", f.entry(e.ID))
	}
}

// TestEnqueue_YieldMarkerArms: a new row is inserted held and marked; a
// same-base HELD entry is re-anchored and marked, a repeat keeping yielded_at
// (the same episode) while taking the new reason; a base change inserts the
// new row marked; an empty reason is refused before any SQL.
func TestEnqueue_YieldMarkerArms(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	enq := func(runID uuid.UUID, base, head, reason string) EnqueueResult {
		t.Helper()
		res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: runID, BaseRef: base, HeadSHA: head,
			MergeIntent: true, Yield: &YieldMarker{Reason: reason, Detail: "d-" + head}})
		if err != nil {
			t.Fatalf("Enqueue(%s, %s, marker): %v", base, head, err)
		}
		return res
	}

	a := f.run()
	ins := enq(a, "main", "h1", "forge_approval_absent")
	got := f.entry(ins.Entry.ID)
	if ins.Outcome != EnqueueInserted || !ins.Marked || got.State != StateHeld || !got.Yielded() ||
		got.YieldReason != "forge_approval_absent" || got.YieldDetail != "d-h1" || got.MergeIntentAt == nil ||
		!got.YieldedAt.Equal(got.EnqueuedAt) {
		t.Fatalf("marked insert = %+v / %+v, want a held marked row with intent, yielded at enqueue", ins, got)
	}

	b := f.run()
	plain := f.enqueue(b, "main", "x1").Entry
	re := enq(b, "main", "x2", "forge_approval_vetoed")
	first := f.entry(plain.ID)
	if re.Outcome != EnqueueReanchored || !re.Marked || !first.Yielded() || first.AnchoredHeadSHA != "x2" ||
		first.YieldReason != "forge_approval_vetoed" || !first.EnqueuedAt.Equal(plain.EnqueuedAt) {
		t.Fatalf("marked held reanchor = %+v / %+v, want re-anchored at x2, marked vetoed, same enqueued_at", re, first)
	}
	again := enq(b, "main", "x3", "forge_approval_absent")
	second := f.entry(plain.ID)
	if !again.Marked || !second.YieldedAt.Equal(*first.YieldedAt) || second.YieldReason != "forge_approval_absent" || second.AnchoredHeadSHA != "x3" {
		t.Fatalf("repeat marked reanchor = %+v, want yielded_at kept %v, reason absent, head x3", second, first.YieldedAt)
	}

	moved := enq(b, "release", "x4", "forge_approval_absent")
	if moved.Outcome != EnqueueBaseChanged || !moved.Marked || !f.entry(moved.Entry.ID).Yielded() {
		t.Fatalf("marked base change = %+v, want the new row marked", moved)
	}

	before := f.count(`true`)
	if _, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: f.run(), BaseRef: "main", HeadSHA: "z1", Yield: &YieldMarker{}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Enqueue(empty reason) err = %v, want ErrInvalidRequest", err)
	}
	if f.count(`true`) != before {
		t.Fatal("a refused marked Enqueue wrote a row")
	}
}

// TestYield_ContinuesLastClearedEpisodeOnSameBaseRef (docs/adr/112 A3.7,
// T15): a re-yield whose marker carries the entry's cleared episode keeps
// that episode's yielded_at (backdated an hour, so a fresh stamp is told
// apart) when the entry's base ref equals the episode's, WHATEVER the head or
// reason: an episode cleared at head h1 for one reason continues on a
// re-yield at h2 for another, through Yield and through Enqueue marking an
// unmarked held entry; an episode on another base ref, or no Continue, stamps
// the database clock (a new episode). Counterfactual CF16: restoring the
// head/base-SHA condition in yieldContinueSQL makes the moved-head re-yield a
// fresh episode, RED (the heads differ).
func TestYield_ContinuesLastClearedEpisodeOnSameBaseRef(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.run()
	e := f.enqueue(a, "main", "h1").Entry
	// episode yields e (live) at the anchor's own epoch for reason, backdates
	// the stamp an hour, clears it, and returns the cleared episode.
	episode := func(reason string, cont *YieldEpisode) (Entry, YieldEpisode) {
		t.Helper()
		got, err := f.store.Yield(ctx, e.ID, f.admitAt(e.ID), YieldMarker{Reason: reason, Continue: cont})
		if err != nil {
			t.Fatalf("Yield: %v", err)
		}
		f.exec(`UPDATE merge_candidate_queue_entries SET yielded_at = yielded_at - interval '1 hour' WHERE id = $1`, e.ID)
		y := f.entry(e.ID)
		if _, err := f.store.ClearYield(ctx, e.ID, y.AnchoredHeadSHA, y.YieldRevision, false); err != nil {
			t.Fatalf("ClearYield: %v", err)
		}
		return got, YieldEpisode{YieldedAt: *y.YieldedAt, BaseRef: y.BaseRef}
	}
	fresh := func(why string, g Entry, ep YieldEpisode) {
		t.Helper()
		if g.YieldedAt == nil || g.YieldedAt.Equal(ep.YieldedAt) || g.YieldAge >= time.Minute {
			t.Fatalf("%s: yielded_at %v age %s, want a fresh episode (not %v)", why, g.YieldedAt, g.YieldAge, ep.YieldedAt)
		}
	}

	_, ep := episode("forge_approval_absent", nil)
	got, ep2 := episode("forge_approval_absent", &ep)
	if got.YieldedAt == nil || !got.YieldedAt.Equal(ep.YieldedAt) || got.YieldAge < time.Hour {
		t.Fatalf("same-base Yield yielded_at %v age %s, want the continued %v (age >= 1h)", got.YieldedAt, got.YieldAge, ep.YieldedAt)
	}

	// The head moved (a marker-less re-enqueue of the cleared held row) and
	// the reason changed: the episode still continues (A3.7).
	f.enqueue(a, "main", "h2")
	got, ep3 := episode("merge_checks_red", &ep2)
	if got.YieldedAt == nil || !got.YieldedAt.Equal(ep2.YieldedAt) || got.AnchoredHeadSHA != "h2" {
		t.Fatalf("moved-head, other-reason Yield yielded_at %v head %s, want the continued %v at h2", got.YieldedAt, got.AnchoredHeadSHA, ep2.YieldedAt)
	}

	// An episode cleared on another base ref is a new episode.
	other := ep3
	other.BaseRef = "release"
	got, _ = episode("merge_checks_red", &other)
	fresh("another base ref", got, other)

	// Enqueue marking the unmarked held row: the same base continues even at
	// a moved head (the re-anchor in the same transaction).
	_, ep4 := episode("merge_checks_pending", nil)
	res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: a, BaseRef: "main", HeadSHA: "h3", BaseSHA: "base-h3",
		MergeIntent: true, Yield: &YieldMarker{Reason: "forge_approval_absent", Continue: &ep4}})
	if err != nil || !res.Marked {
		t.Fatalf("Enqueue(h3, marker) = %+v, %v; want marked", res, err)
	}
	if g := f.entry(e.ID); g.YieldedAt == nil || !g.YieldedAt.Equal(ep4.YieldedAt) || g.AnchoredHeadSHA != "h3" {
		t.Fatalf("Enqueue mark at a moved head: yielded_at %v head %s, want the continued %v at h3", g.YieldedAt, g.AnchoredHeadSHA, ep4.YieldedAt)
	}
}

// TestUpdateYield_RewritesReasonOnly: the matching (head, revision) rewrites
// reason and detail and keeps yielded_at, the head and the wait anchor; a
// stale key is refused and the stored reason is unchanged on read-back.
func TestUpdateYield_RewritesReasonOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	yieldedAt := f.markYielded(e.ID, "forge_approval_absent", time.Minute)
	rev := f.entry(e.ID).YieldRevision

	if _, err := f.store.UpdateYield(ctx, e.ID, "h0", rev, YieldMarker{Reason: "forge_approval_vetoed"}); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("UpdateYield(stale head) err = %v, want ErrStateConflict", err)
	}
	if got := f.entry(e.ID); got.YieldReason != "forge_approval_absent" {
		t.Fatalf("reason after a stale UpdateYield = %q, want unchanged", got.YieldReason)
	}
	if _, err := f.store.UpdateYield(ctx, e.ID, "h1", rev, YieldMarker{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("UpdateYield(empty reason) err = %v, want ErrInvalidRequest", err)
	}
	got, err := f.store.UpdateYield(ctx, e.ID, "h1", rev, YieldMarker{Reason: "forge_approval_vetoed", Detail: "changes requested"})
	if err != nil {
		t.Fatalf("UpdateYield: %v", err)
	}
	if got.YieldReason != "forge_approval_vetoed" || got.YieldDetail != "changes requested" ||
		!got.YieldedAt.Equal(yieldedAt) || got.AnchoredHeadSHA != "h1" || got.State != StateHeld {
		t.Fatalf("updated entry = %+v, want the new reason/detail, yielded_at and head kept", got)
	}
}

// TestListForBaseAndPosition_YieldedOrderedLast: the yielded row is the
// OLDEST held row, yet it lists after every unmarked held row and its
// position counts after them; the live row stays first.
func TestListForBaseAndPosition_YieldedOrderedLast(t *testing.T) {
	f := newFixture(t)
	live, yielded, u1, u2 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	runs := map[uuid.UUID]uuid.UUID{}
	for i, id := range []uuid.UUID{live, yielded, u1, u2} {
		state := StateHeld
		if id == live {
			state = StateLive
		}
		runs[id] = f.run()
		f.seed(id, runs[id], state, time.Duration(4-i)*time.Hour)
	}
	f.markYielded(yielded, "forge_approval_absent", time.Minute)

	es, err := f.store.ListForBase(context.Background(), mainBase())
	if err != nil {
		t.Fatalf("ListForBase: %v", err)
	}
	got := make([]uuid.UUID, len(es))
	for i, e := range es {
		got[i] = e.ID
	}
	if want := []uuid.UUID{live, u1, u2, yielded}; !slices.Equal(got, want) {
		t.Fatalf("ListForBase order = %v, want live, u1, u2, yielded %v", got, want)
	}
	for id, want := range map[uuid.UUID]int{live: 0, u1: 1, u2: 2, yielded: 3} {
		if st := f.status(runs[id]); st.Position != want {
			t.Errorf("position of %s = %d, want %d", id, st.Position, want)
		}
	}
}

// promoteRecorder is a Promote Record callback that records every call and
// returns err. It also reads the entry's order key on the callback's own tx,
// pinning that Record runs inside the promotion transaction after the write.
type promoteRecorder struct {
	calls     []Promotion
	keyOnTx   []bool
	err       error
	readOnTxE error
}

func (r *promoteRecorder) record(ctx context.Context, tx pgx.Tx, p Promotion) error {
	r.calls = append(r.calls, p)
	var set bool
	if err := tx.QueryRow(ctx, `SELECT order_key IS NOT NULL FROM merge_candidate_queue_entries WHERE id = $1`, p.Entry.ID).Scan(&set); err != nil {
		r.readOnTxE = err
	}
	r.keyOnTx = append(r.keyOnTx, set)
	return r.err
}

func (f *fixture) listRuns(base Base) []uuid.UUID {
	f.t.Helper()
	es, err := f.store.ListForBase(context.Background(), base)
	if err != nil {
		f.t.Fatalf("ListForBase: %v", err)
	}
	return runIDs(es)
}

func (f *fixture) orderKeyNull(id uuid.UUID) bool {
	f.t.Helper()
	return f.count(`id = $1 AND order_key IS NOT NULL`, id) == 0
}

// TestPromote_MovesHeldEntryToHead: with A, B, C held, promoting C puts it
// first in ListForBase, at position 1, and makes it the entry AdmitNext
// admits; Record is called once, inside the transaction, with the verdict.
// Deleting the promote UPDATE leaves C third; reverting AdmitNext's ORDER BY
// to (enqueued_at, id) admits A; reverting activeForRun's position tuple
// reads C at 3: each RED.
func TestPromote_MovesHeldEntryToHead(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.run(), f.run(), f.run()
	for i, r := range []uuid.UUID{a, b, c} {
		f.enqueue(r, "main", fmt.Sprintf("h%d", i))
	}
	rec := &promoteRecorder{}
	p, err := f.store.Promote(context.Background(), PromoteRequest{RunID: c, Record: rec.record})
	if err != nil {
		t.Fatalf("Promote(C): %v", err)
	}
	if !p.Changed || p.OldPosition != 3 || p.NewPosition != 1 || p.Entry.RunID != c || !p.Entry.Promoted() || p.LiveRunID != nil {
		t.Fatalf("Promotion = %+v, want Changed 3 -> 1 for C, promoted, no live run", p)
	}
	if len(rec.calls) != 1 || rec.calls[0].NewPosition != 1 || rec.readOnTxE != nil || !rec.keyOnTx[0] {
		t.Fatalf("Record calls = %+v (order key on tx %v, err %v), want one call seeing the written key", rec.calls, rec.keyOnTx, rec.readOnTxE)
	}
	if got, want := f.listRuns(mainBase()), []uuid.UUID{c, a, b}; !slices.Equal(got, want) {
		t.Fatalf("ListForBase after promote = %v, want C, A, B %v", got, want)
	}
	if st := f.status(c); st.Position != 1 || !st.Entry.Promoted() {
		t.Fatalf("C status = position %d promoted %v, want 1 and promoted", st.Position, st.Entry.Promoted())
	}
	for _, r := range []uuid.UUID{a, b} {
		if st := f.status(r); st.Entry.Promoted() || st.Entry.OrderKey != nil {
			t.Fatalf("unpromoted entry %s carries an order key %v", r, st.Entry.OrderKey)
		}
	}
	if st := f.status(a); st.Position != 2 {
		t.Fatalf("A position = %d, want 2", st.Position)
	}
	if adm := f.admitNext(mainBase()); !adm.Admitted || adm.Entry.RunID != c {
		t.Fatalf("AdmitNext after promote = %+v, want C admitted (admission follows the effective key)", adm)
	}
}

// TestPromote_RefusalsWriteNothing: one arm per refusal; every arm reads the
// target's order_key back NULL. live → ErrEntryLive, yielded →
// ErrEntryYielded, only a terminal row → ErrNotQueued, settled between the
// peek and the lock → ErrNotQueued, unknown run → ErrNotFound, nil Record or
// nil run id → ErrInvalidRequest.
func TestPromote_RefusalsWriteNothing(t *testing.T) {
	ctx := context.Background()
	rec := &promoteRecorder{}

	t.Run("live", func(t *testing.T) {
		f := newFixture(t)
		live, held := f.run(), f.run()
		f.enqueue(live, "main", "h1")
		f.enqueue(held, "main", "h2")
		f.admitNext(mainBase())
		_, err := f.store.Promote(ctx, PromoteRequest{RunID: live, Record: rec.record})
		if !errors.Is(err, ErrEntryLive) {
			t.Fatalf("Promote(live) err = %v, want ErrEntryLive", err)
		}
		if !f.orderKeyNull(f.status(live).Entry.ID) {
			t.Fatal("live entry carries an order key after a refused promote")
		}
	})
	t.Run("yielded", func(t *testing.T) {
		f := newFixture(t)
		ahead, y := f.run(), f.run()
		f.enqueue(ahead, "main", "h1")
		e := f.enqueue(y, "main", "h2").Entry
		f.markYielded(e.ID, "forge_approval_absent", time.Minute)
		_, err := f.store.Promote(ctx, PromoteRequest{RunID: y, Record: rec.record})
		if !errors.Is(err, ErrEntryYielded) {
			t.Fatalf("Promote(yielded) err = %v, want ErrEntryYielded", err)
		}
		if !f.orderKeyNull(e.ID) {
			t.Fatal("yielded entry carries an order key after a refused promote")
		}
	})
	t.Run("terminal only", func(t *testing.T) {
		f := newFixture(t)
		r := f.run()
		e := f.enqueue(r, "main", "h1").Entry
		if _, err := f.store.Settle(ctx, e.ID, StateEjected, "verify_failed"); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		_, err := f.store.Promote(ctx, PromoteRequest{RunID: r, Record: rec.record})
		if !errors.Is(err, ErrNotQueued) {
			t.Fatalf("Promote(terminal only) err = %v, want ErrNotQueued", err)
		}
		if !f.orderKeyNull(e.ID) {
			t.Fatal("terminal row carries an order key")
		}
	})
	t.Run("settled between peek and lock", func(t *testing.T) {
		f := newFixture(t)
		ahead, r := f.run(), f.run()
		f.enqueue(ahead, "main", "h1")
		e := f.enqueue(r, "main", "h2").Entry
		f.store.afterPromotePeek = func(context.Context) {
			if _, err := NewPostgresStore(f.pool).Settle(ctx, e.ID, StateDropped, "pr_closed"); err != nil {
				t.Errorf("settle in hook: %v", err)
			}
		}
		_, err := f.store.Promote(ctx, PromoteRequest{RunID: r, Record: rec.record})
		if !errors.Is(err, ErrNotQueued) {
			t.Fatalf("Promote(settled after peek) err = %v, want ErrNotQueued", err)
		}
		if !f.orderKeyNull(e.ID) {
			t.Fatal("settled row carries an order key")
		}
	})
	t.Run("unknown run", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.store.Promote(ctx, PromoteRequest{RunID: uuid.New(), Record: rec.record}); !errors.Is(err, ErrNotFound) || !errors.Is(err, run.ErrNotFound) {
			t.Fatalf("Promote(unknown run) err = %v, want ErrNotFound", err)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		f := newFixture(t)
		ahead, r := f.run(), f.run()
		f.enqueue(ahead, "main", "h1")
		e := f.enqueue(r, "main", "h2").Entry
		if _, err := f.store.Promote(ctx, PromoteRequest{RunID: r}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Promote(nil Record) err = %v, want ErrInvalidRequest", err)
		}
		if _, err := f.store.Promote(ctx, PromoteRequest{Record: rec.record}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Promote(nil run id) err = %v, want ErrInvalidRequest", err)
		}
		if !f.orderKeyNull(e.ID) {
			t.Fatal("a refused-before-SQL promote wrote an order key")
		}
	})
	if len(rec.calls) != 0 {
		t.Fatalf("Record called %d times across refusals, want 0", len(rec.calls))
	}
}

// TestPromote_RecordErrorRollsBack: a Record error is returned wrapped and the
// whole promotion rolls back, read as COMMITTED state after the call: the
// order key is NULL and the order is the original FIFO. A store that ignored
// Record's error would commit the key: RED.
func TestPromote_RecordErrorRollsBack(t *testing.T) {
	f := newFixture(t)
	a, b := f.run(), f.run()
	f.enqueue(a, "main", "h1")
	e := f.enqueue(b, "main", "h2").Entry
	boom := errors.New("audit append failed")
	rec := &promoteRecorder{err: boom}
	_, err := f.store.Promote(context.Background(), PromoteRequest{RunID: b, Record: rec.record})
	if !errors.Is(err, boom) {
		t.Fatalf("Promote err = %v, want the Record error wrapped", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("Record calls = %d, want 1", len(rec.calls))
	}
	if !f.orderKeyNull(e.ID) {
		t.Fatal("order key committed despite the Record error; want the promotion rolled back")
	}
	if got, want := f.listRuns(mainBase()), []uuid.UUID{a, b}; !slices.Equal(got, want) {
		t.Fatalf("ListForBase after the rolled-back promote = %v, want the original %v", got, want)
	}
}

// TestPromote_AlreadyFirstNoop: the first held entry answers Changed=false,
// positions 1/1, writes nothing and never calls Record. So does the guarded
// write's miss: the only entry ahead vanishes (its run is deleted, the
// cascade the base lock does not cover) between the position read and the
// write.
func TestPromote_AlreadyFirstNoop(t *testing.T) {
	ctx := context.Background()
	t.Run("first", func(t *testing.T) {
		f := newFixture(t)
		a, b := f.run(), f.run()
		e := f.enqueue(a, "main", "h1").Entry
		f.enqueue(b, "main", "h2")
		rec := &promoteRecorder{}
		p, err := f.store.Promote(ctx, PromoteRequest{RunID: a, Record: rec.record})
		if err != nil || p.Changed || p.OldPosition != 1 || p.NewPosition != 1 {
			t.Fatalf("Promote(first) = %+v, %v; want a 1/1 no-op", p, err)
		}
		if len(rec.calls) != 0 || !f.orderKeyNull(e.ID) {
			t.Fatalf("no-op promote called Record %d times or wrote a key", len(rec.calls))
		}
	})
	t.Run("guard misses", func(t *testing.T) {
		f := newFixture(t)
		a, b := f.run(), f.run()
		f.enqueue(a, "main", "h1")
		e := f.enqueue(b, "main", "h2").Entry
		f.store.afterPromotePosition = func(context.Context) {
			if _, err := f.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, a); err != nil {
				t.Errorf("delete run in hook: %v", err)
			}
		}
		rec := &promoteRecorder{}
		p, err := f.store.Promote(ctx, PromoteRequest{RunID: b, Record: rec.record})
		if err != nil || p.Changed || p.OldPosition != 1 || p.NewPosition != 1 {
			t.Fatalf("Promote(guard misses) = %+v, %v; want a 1/1 no-op", p, err)
		}
		if len(rec.calls) != 0 || !f.orderKeyNull(e.ID) {
			t.Fatalf("guard-miss promote called Record %d times or wrote a key", len(rec.calls))
		}
	})
}

// parkPromote installs an afterPromotePosition hook that parks the FIRST
// Promote to reach it (after its base lock and position read, before its
// order-key write) until release closes; parked closes when it is parked.
// Every later caller passes straight through: a sync.Once would block them
// inside the hook until the first returns, so a second Promote would wait
// there whether or not the base lock held it.
func (f *fixture) parkPromote() (parked, release chan struct{}) {
	parked, release = make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	f.store.afterPromotePosition = func(context.Context) {
		if first.CompareAndSwap(false, true) {
			close(parked)
			<-release
		}
	}
	return parked, release
}

// promoteResult is one goroutine's Promote answer.
type promoteResult struct {
	p   Promotion
	err error
}

// TestPromote_ConcurrentPromotesSerializeOnTheBaseLock: A promotes r3 and
// parks holding the base lock; B promotes r2 and must wait for A. B then reads
// A's committed key (OldPosition 3) and goes ahead of it, so the order is r2,
// r3, r1. Each goroutine records into its OWN promoteRecorder, so a -race RED
// cannot come from shared fake state. MECHANISM: with Promote's base lock
// removed, B completes while A is parked (RED), reads OldPosition 2 because A
// has written nothing, and A then keys ahead of B (order r3, r2, r1: RED).
func TestPromote_ConcurrentPromotesSerializeOnTheBaseLock(t *testing.T) {
	f := newFixture(t)
	r1, r2, r3 := f.run(), f.run(), f.run()
	for i, r := range []uuid.UUID{r1, r2, r3} {
		f.enqueue(r, "main", fmt.Sprintf("h%d", i))
	}
	parked, release := f.parkPromote()
	recA, recB := &promoteRecorder{}, &promoteRecorder{}
	aDone, bDone := make(chan promoteResult, 1), make(chan promoteResult, 1)
	go func() {
		p, err := f.store.Promote(context.Background(), PromoteRequest{RunID: r3, Record: recA.record})
		aDone <- promoteResult{p, err}
	}()
	<-parked
	go func() {
		p, err := f.store.Promote(context.Background(), PromoteRequest{RunID: r2, Record: recB.record})
		bDone <- promoteResult{p, err}
	}()
	var early *promoteResult
	select {
	case res := <-bDone:
		early = &res
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	close(release)
	a := <-aDone
	if early != nil {
		t.Fatalf("Promote(r2) completed while Promote(r3) was parked holding the base lock: %+v, %v", early.p, early.err)
	}
	b := <-bDone
	if a.err != nil || b.err != nil {
		t.Fatalf("Promote errors: A %v, B %v", a.err, b.err)
	}
	if !a.p.Changed || a.p.OldPosition != 3 || a.p.NewPosition != 1 || len(recA.calls) != 1 {
		t.Fatalf("A = %+v (records %d), want Changed 3 -> 1 recorded once", a.p, len(recA.calls))
	}
	if !b.p.Changed || b.p.OldPosition != 3 || b.p.NewPosition != 1 || len(recB.calls) != 1 {
		t.Fatalf("B = %+v (records %d), want Changed 3 -> 1 (it saw A's committed key) recorded once", b.p, len(recB.calls))
	}
	if got, want := f.listRuns(mainBase()), []uuid.UUID{r2, r3, r1}; !slices.Equal(got, want) {
		t.Fatalf("ListForBase = %v, want r2, r3, r1 %v", got, want)
	}
	ka, kb := f.status(r3).Entry.OrderKey, f.status(r2).Entry.OrderKey
	if ka == nil || kb == nil || !kb.Before(*ka) {
		t.Fatalf("order keys A %v B %v, want B's strictly before A's", ka, kb)
	}
}

// TestPromote_ConcurrentAdmitNextWaitsForThePromotion: A promotes r3 and
// parks holding the base lock; a concurrent AdmitNext must wait for A and
// then admit the promoted head r3, not the FIFO head r1. MECHANISM: with
// Promote's base lock removed, AdmitNext completes while A is parked and
// admits r1: RED.
func TestPromote_ConcurrentAdmitNextWaitsForThePromotion(t *testing.T) {
	f := newFixture(t)
	r1, r2, r3 := f.run(), f.run(), f.run()
	for i, r := range []uuid.UUID{r1, r2, r3} {
		f.enqueue(r, "main", fmt.Sprintf("h%d", i))
	}
	parked, release := f.parkPromote()
	rec := &promoteRecorder{}
	aDone := make(chan promoteResult, 1)
	go func() {
		p, err := f.store.Promote(context.Background(), PromoteRequest{RunID: r3, Record: rec.record})
		aDone <- promoteResult{p, err}
	}()
	<-parked
	type admitResult struct {
		adm Admission
		err error
	}
	admDone := make(chan admitResult, 1)
	go func() {
		adm, err := f.store.AdmitNext(context.Background(), mainBase())
		admDone <- admitResult{adm, err}
	}()
	var early *admitResult
	select {
	case res := <-admDone:
		early = &res
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	close(release)
	a := <-aDone
	if early != nil {
		admitted := uuid.Nil
		if early.adm.Entry != nil {
			admitted = early.adm.Entry.RunID
		}
		t.Fatalf("AdmitNext completed while Promote(r3) was parked holding the base lock: admitted run %s (r1 %s, r3 %s), err %v", admitted, r1, r3, early.err)
	}
	adm := <-admDone
	if a.err != nil || adm.err != nil {
		t.Fatalf("errors: Promote %v, AdmitNext %v", a.err, adm.err)
	}
	if !a.p.Changed || a.p.OldPosition != 3 || a.p.NewPosition != 1 {
		t.Fatalf("Promote(r3) = %+v, want Changed 3 -> 1", a.p)
	}
	if !adm.adm.Admitted || adm.adm.Entry == nil || adm.adm.Entry.RunID != r3 {
		t.Fatalf("AdmitNext = %+v, want r3 (the promoted head) admitted, not the FIFO head r1", adm.adm)
	}
	if live := f.count(`state = 'live'`); live != 1 || f.status(r3).Entry.State != StateLive {
		t.Fatalf("live entries = %d (r3 %s), want r3 the only live entry", live, f.status(r3).Entry.State)
	}
}

// TestPromote_NewestPromoteWinsAndSurvivesUnadmitYieldClear: a second promote
// goes ahead of the first; the promoted entry keeps its order key across an
// Unadmit, a Yield (it sorts last while marked) and a ClearYield (it rejoins
// at its promoted place).
func TestPromote_NewestPromoteWinsAndSurvivesUnadmitYieldClear(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b, c, d := f.run(), f.run(), f.run(), f.run()
	for i, r := range []uuid.UUID{a, b, c, d} {
		f.enqueue(r, "main", fmt.Sprintf("h%d", i))
	}
	rec := &promoteRecorder{}
	for _, r := range []uuid.UUID{c, d} {
		if _, err := f.store.Promote(ctx, PromoteRequest{RunID: r, Record: rec.record}); err != nil {
			t.Fatalf("Promote(%s): %v", r, err)
		}
	}
	if got, want := f.listRuns(mainBase()), []uuid.UUID{d, c, a, b}; !slices.Equal(got, want) {
		t.Fatalf("after two promotes = %v, want D, C, A, B %v", got, want)
	}
	dEntry := f.status(d).Entry
	key := *dEntry.OrderKey

	epoch := f.admitAt(dEntry.ID)
	if _, err := f.store.Unadmit(ctx, dEntry.ID, epoch); err != nil {
		t.Fatalf("Unadmit: %v", err)
	}
	if got, want := f.listRuns(mainBase()), []uuid.UUID{d, c, a, b}; !slices.Equal(got, want) {
		t.Fatalf("after Unadmit = %v, want D back first %v", got, want)
	}
	epoch = f.admitAt(dEntry.ID)
	y, err := f.store.Yield(ctx, dEntry.ID, epoch, YieldMarker{Reason: "forge_approval_absent"})
	if err != nil {
		t.Fatalf("Yield: %v", err)
	}
	if got, want := f.listRuns(mainBase()), []uuid.UUID{c, a, b, d}; !slices.Equal(got, want) {
		t.Fatalf("while yielded = %v, want D last %v", got, want)
	}
	if _, err := f.store.ClearYield(ctx, dEntry.ID, y.AnchoredHeadSHA, y.YieldRevision, false); err != nil {
		t.Fatalf("ClearYield: %v", err)
	}
	if got, want := f.listRuns(mainBase()), []uuid.UUID{d, c, a, b}; !slices.Equal(got, want) {
		t.Fatalf("after ClearYield = %v, want D at its promoted place %v", got, want)
	}
	if got := f.entry(dEntry.ID).OrderKey; got == nil || !got.Equal(key) {
		t.Fatalf("D order key after Unadmit/Yield/ClearYield = %v, want kept %v", got, key)
	}
}

// TestPromote_RepromoteUsesExistingOrderKey: an entry promoted once and then
// overtaken by a later promote is promoted again from its existing order key,
// not its enqueued_at, and goes back to the head.
func TestPromote_RepromoteUsesExistingOrderKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b, c, d := f.run(), f.run(), f.run(), f.run()
	for i, r := range []uuid.UUID{a, b, c, d} {
		f.enqueue(r, "main", fmt.Sprintf("h%d", i))
	}
	rec := &promoteRecorder{}
	for _, r := range []uuid.UUID{c, d} {
		if _, err := f.store.Promote(ctx, PromoteRequest{RunID: r, Record: rec.record}); err != nil {
			t.Fatalf("Promote(%s): %v", r, err)
		}
	}
	p, err := f.store.Promote(ctx, PromoteRequest{RunID: c, Record: rec.record})
	if err != nil || !p.Changed || p.OldPosition != 2 || p.NewPosition != 1 {
		t.Fatalf("re-Promote(C) = %+v, %v; want Changed 2 -> 1", p, err)
	}
	if got, want := f.listRuns(mainBase()), []uuid.UUID{c, d, a, b}; !slices.Equal(got, want) {
		t.Fatalf("after re-promote = %v, want C, D, A, B %v", got, want)
	}
}

// TestPromote_MidTransactionFailureRollsBack: a statement failing after the
// peek (the base lock) or after the position read (the order-key write)
// returns the error and commits nothing.
func TestPromote_MidTransactionFailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(f *fixture, cancel context.CancelFunc)
	}{
		{"base lock", func(f *fixture, cancel context.CancelFunc) {
			f.store.afterPromotePeek = func(context.Context) { cancel() }
		}},
		{"order key write", func(f *fixture, cancel context.CancelFunc) {
			f.store.afterPromotePosition = func(context.Context) { cancel() }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			a, b := f.run(), f.run()
			f.enqueue(a, "main", "h1")
			e := f.enqueue(b, "main", "h2").Entry
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.hook(f, cancel)
			rec := &promoteRecorder{}
			if _, err := f.store.Promote(ctx, PromoteRequest{RunID: b, Record: rec.record}); !errors.Is(err, context.Canceled) {
				t.Fatalf("Promote err = %v, want context.Canceled", err)
			}
			if len(rec.calls) != 0 || !f.orderKeyNull(e.ID) {
				t.Fatalf("failed promote called Record %d times or committed a key", len(rec.calls))
			}
		})
	}
}

// TestEntryReads_CanceledContext: the pool reads surface a canceled context.
func TestEntryReads_CanceledContext(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := f.store.LatestForRun(ctx, f.run()); !errors.Is(err, context.Canceled) {
		t.Fatalf("LatestForRun err = %v, want context.Canceled", err)
	}
	if _, err := f.store.UnresolvedEjections(ctx, mainBase(), 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("UnresolvedEjections err = %v, want context.Canceled", err)
	}
	if _, err := f.store.ReentryCandidates(ctx, nil, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReentryCandidates err = %v, want context.Canceled", err)
	}
}

// TestPromote_AccountScoped: another account's EARLIER held row on the same
// repo and base changes neither the promoted key (1 ms ahead of this
// account's first entry, not the other account's) nor anyone's position.
func TestPromote_AccountScoped(t *testing.T) {
	f := newFixture(t)
	acct := uuid.New()
	f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, acct, "mq-"+acct.String())
	other := f.run()
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct, other)
	otherID := uuid.New()
	f.seed(otherID, other, StateHeld, time.Hour)
	a, b := f.run(), f.run()
	aEntry := f.enqueue(a, "main", "h1").Entry
	bEntry := f.enqueue(b, "main", "h2").Entry
	p, err := f.store.Promote(context.Background(), PromoteRequest{RunID: b, Record: (&promoteRecorder{}).record})
	if err != nil || !p.Changed || p.OldPosition != 2 || p.NewPosition != 1 {
		t.Fatalf("Promote(B) = %+v, %v; want 2 -> 1 counted in its own account", p, err)
	}
	got := f.entry(bEntry.ID).OrderKey
	if want := aEntry.EnqueuedAt.Add(-time.Millisecond); got == nil || !got.Equal(want) {
		t.Fatalf("B order key = %v, want A's enqueued_at - 1ms %v (the other account's row must not count)", got, want)
	}
	if st := f.status(other); st.Position != 1 || st.Entry.Promoted() {
		t.Fatalf("other account's entry = position %d promoted %v, want 1 and untouched", st.Position, st.Entry.Promoted())
	}
}

// TestEnqueue_ReentryAfterEjectionJoinsTail: an ejected run that re-enters
// gets a FRESH row at the TAIL (ADR-093 Q2), behind every entry that was
// behind it. Stamping the fresh row with the run's earliest prior enqueued_at
// gives it back its original place (position 1): RED.
func TestEnqueue_ReentryAfterEjectionJoinsTail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b, c := f.run(), f.run(), f.run()
	for i, r := range []uuid.UUID{a, b, c} {
		f.enqueue(r, "main", fmt.Sprintf("h%d", i))
	}
	adm := f.admitNext(mainBase())
	if adm.Entry.RunID != a {
		t.Fatalf("AdmitNext = %+v, want A", adm)
	}
	if _, err := f.store.SettleAt(ctx, adm.Entry.ID, adm.Entry.AdmissionEpoch, StateEjected, "verify_failed"); err != nil {
		t.Fatalf("eject A: %v", err)
	}
	res := f.enqueue(a, "main", "h0-fixed")
	if res.Outcome != EnqueueInserted || res.Entry.ID == adm.Entry.ID || res.Entry.Promoted() {
		t.Fatalf("re-enqueue = %+v, want a fresh unpromoted row", res)
	}
	if got, want := f.listRuns(mainBase()), []uuid.UUID{b, c, a}; !slices.Equal(got, want) {
		t.Fatalf("ListForBase after re-entry = %v, want B, C, A %v", got, want)
	}
	if st := f.status(a); st.Position != 3 {
		t.Fatalf("re-entered A position = %d, want 3 (the tail)", st.Position)
	}
}

// TestLatestForRun: the run's newest row in any state; none for a run that
// never queued; a nil run id is ErrInvalidRequest.
func TestLatestForRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.run()
	if _, found, err := f.store.LatestForRun(ctx, r); found || err != nil {
		t.Fatalf("LatestForRun(never queued) = found %v, %v; want not found", found, err)
	}
	first := f.enqueue(r, "main", "h1").Entry
	if e, found, err := f.store.LatestForRun(ctx, r); !found || err != nil || e.ID != first.ID || e.State != StateHeld {
		t.Fatalf("LatestForRun(held) = %+v, %v, %v; want the held row", e, found, err)
	}
	if _, err := f.store.Settle(ctx, first.ID, StateEjected, "verify_failed"); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if e, found, err := f.store.LatestForRun(ctx, r); !found || err != nil || e.ID != first.ID || e.State != StateEjected || e.SettledAge < 0 {
		t.Fatalf("LatestForRun(ejected) = %+v, %v, %v; want the ejected row", e, found, err)
	}
	second := f.enqueue(r, "main", "h2").Entry
	if e, found, err := f.store.LatestForRun(ctx, r); !found || err != nil || e.ID != second.ID {
		t.Fatalf("LatestForRun(re-entered) = %+v, %v, %v; want the fresh row %s", e, found, err, second.ID)
	}
	if _, _, err := f.store.LatestForRun(ctx, uuid.Nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("LatestForRun(nil) err = %v, want ErrInvalidRequest", err)
	}
}

// seedEjected inserts a terminal row BY CONSTRUCTION, settled settledAgo
// before the database clock and enqueued an hour before that, with reason
// and (intent) merge intent.
func (f *fixture) seedEjected(runID uuid.UUID, state State, reason string, intent bool, settledAgo time.Duration) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO merge_candidate_queue_entries
  (run_id, repo, base_ref, state, eject_reason, enqueued_at, anchored_head_sha, settled_at, merge_intent_at, merge_intent_sequence)
SELECT $1, $2, 'main', $3::text, $4, c.ts - make_interval(secs => $5::float8) - interval '1 hour', 'h-ej',
  c.ts - make_interval(secs => $5::float8), CASE WHEN $6::bool THEN c.ts - interval '2 hours' END, CASE WHEN $6::bool THEN 7 END
FROM (SELECT clock_timestamp() AS ts) c
RETURNING id`, runID, testRepo, string(state), reason, settledAgo.Seconds(), intent).Scan(&id); err != nil {
		f.t.Fatalf("seed ejected row: %v", err)
	}
	return id
}

func entryIDs(es []Entry) []uuid.UUID {
	out := make([]uuid.UUID, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

// TestUnresolvedEjections: the base's ejected rows still newest for their
// run, newest settle first, account-scoped, bounded by limit. A re-entered
// run's ejection (a fresh row behind it), a dropped row and another
// account's ejection on the same base are excluded.
func TestUnresolvedEjections(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	older := f.seedEjected(f.run(), StateEjected, "verify_failed", true, 2*time.Hour)
	newer := f.seedEjected(f.run(), StateEjected, "advance_conflict", false, time.Hour)
	reentered := f.run()
	f.seedEjected(reentered, StateEjected, "verify_failed", true, 30*time.Minute)
	f.enqueue(reentered, "main", "h-fixed")
	f.seedEjected(f.run(), StateDropped, "pr_closed", false, 10*time.Minute)
	acct := uuid.New()
	f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, acct, "mq-"+acct.String())
	foreign := f.run()
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct, foreign)
	f.seedEjected(foreign, StateEjected, "verify_failed", true, time.Minute)

	got, err := f.store.UnresolvedEjections(ctx, mainBase(), 10)
	if err != nil {
		t.Fatalf("UnresolvedEjections: %v", err)
	}
	if want := []uuid.UUID{newer, older}; !slices.Equal(entryIDs(got), want) {
		t.Fatalf("UnresolvedEjections = %v, want newest-first unresolved %v", entryIDs(got), want)
	}
	if got[0].EjectReason != "advance_conflict" || got[0].State != StateEjected {
		t.Fatalf("first ejection = %+v, want the advance_conflict row", got[0])
	}
	got, err = f.store.UnresolvedEjections(ctx, mainBase(), 1)
	if err != nil || !slices.Equal(entryIDs(got), []uuid.UUID{newer}) {
		t.Fatalf("UnresolvedEjections(limit 1) = %v, %v; want only the newest", entryIDs(got), err)
	}
	empty, err := f.store.UnresolvedEjections(ctx, Base{Repo: testRepo, BaseRef: "release"}, 10)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("UnresolvedEjections(empty base) = %v, %v; want an empty non-nil list", empty, err)
	}
	if _, err := f.store.UnresolvedEjections(ctx, mainBase(), 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("limit 0 err = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.store.UnresolvedEjections(ctx, Base{Repo: testRepo}, 10); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid base err = %v, want ErrInvalidRequest", err)
	}
}

// TestReentryCandidates: ejected verify_failed rows with merge intent, newest
// for their run, on a non-terminal run, oldest settle first, across accounts,
// bounded by limit; SettledAge is the database-clock age of the settle. No
// intent, another reason, a re-entered run and a terminal run are excluded.
// Deleting the terminal-run predicate lists the terminal fixture: RED.
func TestReentryCandidates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	oldest := f.seedEjected(f.run(), StateEjected, ReentryReason, true, 3*time.Hour)
	acct := uuid.New()
	f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, acct, "mq-"+acct.String())
	foreignRun := f.run()
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acct, foreignRun)
	foreign := f.seedEjected(foreignRun, StateEjected, ReentryReason, true, 2*time.Hour)
	f.seedEjected(f.run(), StateEjected, ReentryReason, false, 90*time.Minute)     // no intent
	f.seedEjected(f.run(), StateEjected, "advance_conflict", true, 80*time.Minute) // another reason
	f.seedEjected(f.run(), StateDropped, ReentryReason, true, 70*time.Minute)      // not ejected
	reentered := f.run()
	f.seedEjected(reentered, StateEjected, ReentryReason, true, time.Hour)
	f.enqueue(reentered, "main", "h-fixed")
	terminal := f.run()
	f.seedEjected(terminal, StateEjected, ReentryReason, true, 50*time.Minute)
	f.exec(`UPDATE runs SET state = 'cancelled' WHERE id = $1`, terminal)

	got, err := f.store.ReentryCandidates(ctx, nil, 10)
	if err != nil {
		t.Fatalf("ReentryCandidates: %v", err)
	}
	if want := []uuid.UUID{oldest, foreign}; !slices.Equal(entryIDs(got), want) {
		t.Fatalf("ReentryCandidates = %v, want oldest-first %v", entryIDs(got), want)
	}
	if age := got[0].SettledAge; age < 3*time.Hour || age > 3*time.Hour+timescale.D(time.Minute) {
		t.Fatalf("SettledAge = %v, want ~3h (database clock)", age)
	}
	if got[0].MergeIntentAt == nil || got[0].MergeIntentSequence != 7 {
		t.Fatalf("candidate = %+v, want its merge intent and sequence", got[0])
	}
	got, err = f.store.ReentryCandidates(ctx, nil, 1)
	if err != nil || !slices.Equal(entryIDs(got), []uuid.UUID{oldest}) {
		t.Fatalf("ReentryCandidates(limit 1) = %v, %v; want only the oldest", entryIDs(got), err)
	}
	if _, err := f.store.ReentryCandidates(ctx, nil, 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("limit 0 err = %v, want ErrInvalidRequest", err)
	}
}

// TestReentryCandidates_KeysetCursorPages: the exclusive (settled_at, id)
// cursor pages the oldest-first order without skipping or repeating a row,
// breaking a settled_at tie on id; nil starts at the oldest (#4297: the
// sweep pages past never-passing candidates). The b/c settled_at tie
// STRADDLES the 2-row page boundary (#4705): page 1 ends on the lower-id
// tied row, so a settled_at-only cursor skips the higher-id tied row (>) or
// repeats it (>=), and only the (settled_at, id) row comparison passes. A
// tie inside one page (the earlier c/d fixture) stays GREEN under a
// settled_at-only cursor. Ignoring the cursor re-reads the first page
// forever: RED.
func TestReentryCandidates_KeysetCursorPages(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.seedEjected(f.run(), StateEjected, ReentryReason, true, 4*time.Hour)
	b := f.seedEjected(f.run(), StateEjected, ReentryReason, true, 3*time.Hour)
	c := f.seedEjected(f.run(), StateEjected, ReentryReason, true, 2*time.Hour)
	d := f.seedEjected(f.run(), StateEjected, ReentryReason, true, time.Hour)
	f.exec(`UPDATE merge_candidate_queue_entries SET settled_at = (SELECT settled_at FROM merge_candidate_queue_entries WHERE id = $1) WHERE id = $2`, b, c)
	lo, hi := b, c
	if bytes.Compare(c[:], b[:]) < 0 {
		lo, hi = c, b
	}
	want := []uuid.UUID{a, lo, hi, d}
	var got []uuid.UUID
	var after *ReentryCursor
	for pages := 0; ; pages++ {
		if pages > len(want) {
			t.Fatalf("paging did not terminate after %d pages: %v", pages, got)
		}
		page, err := f.store.ReentryCandidates(ctx, after, 2)
		if err != nil {
			t.Fatalf("ReentryCandidates(after %+v): %v", after, err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, entryIDs(page)...)
		after = ReentryCursorAt(page[len(page)-1])
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged candidates = %v, want %v (every row once, oldest first, ties on id)", got, want)
	}
	if ReentryCursorAt(Entry{ID: a}) != nil {
		t.Fatal("ReentryCursorAt(unsettled) != nil")
	}
}

// --- ADR-093 D8: the restart resume's store seam (#4298) ---

// databaseNow reads the database clock through the store.
func (f *fixture) databaseNow() time.Time {
	f.t.Helper()
	now, err := f.store.DatabaseNow(context.Background())
	if err != nil || now.IsZero() {
		f.t.Fatalf("DatabaseNow = %v, %v; want a database instant", now, err)
	}
	return now
}

// setAdmittedAt moves a live row's admitted_at to `offset` from at, an
// instant READ from the database (never a Go clock).
func (f *fixture) setAdmittedAt(id uuid.UUID, at time.Time, offset time.Duration) {
	f.t.Helper()
	f.exec(`UPDATE merge_candidate_queue_entries SET admitted_at = $2::timestamptz + make_interval(secs => $3::float8) WHERE id = $1`,
		id, at, offset.Seconds())
}

// backdateStale moves a live row's admitted_at past AdmissionStaleAfter on the
// database clock.
func (f *fixture) backdateStale(id uuid.UUID) {
	f.t.Helper()
	f.exec(`UPDATE merge_candidate_queue_entries SET admitted_at = clock_timestamp() - make_interval(secs => $2::float8) WHERE id = $1`,
		id, (AdmissionStaleAfter + time.Minute).Seconds())
}

// reclaimRecorder is a ReclaimRecordFunc that counts its calls, keeps the
// entries it was handed, reads the row's epoch INSIDE the call's transaction
// and on the pool (the committed value), and fails with err when set.
type reclaimRecorder struct {
	pool      *pgxpool.Pool
	err       error
	calls     []Entry
	txEpoch   []int64
	poolEpoch []int64
}

func (r *reclaimRecorder) record(ctx context.Context, tx pgx.Tx, e Entry) error {
	r.calls = append(r.calls, e)
	var inTx, committed int64
	if err := tx.QueryRow(ctx, `SELECT admission_epoch FROM merge_candidate_queue_entries WHERE id = $1`, e.ID).Scan(&inTx); err != nil {
		return err
	}
	if err := r.pool.QueryRow(ctx, `SELECT admission_epoch FROM merge_candidate_queue_entries WHERE id = $1`, e.ID).Scan(&committed); err != nil {
		return err
	}
	r.txEpoch, r.poolEpoch = append(r.txEpoch, inTx), append(r.poolEpoch, committed)
	return r.err
}

func (f *fixture) reclaiming(base Base, before time.Time, rec *reclaimRecorder) Admission {
	f.t.Helper()
	adm, err := f.store.AdmitNextReclaiming(context.Background(), ReclaimRequest{Base: base, AdmittedBefore: before, Record: rec.record})
	if err != nil {
		f.t.Fatalf("AdmitNextReclaiming: %v", err)
	}
	return adm
}

// TestAdmitNext_StaleReclaimCoversAdvancing (counterfactual A1): a live entry
// backdated past AdmissionStaleAfter in phase advancing is re-claimed (epoch
// +1, read back) exactly like phase admitted; the same backdate in verifying,
// unverified, passed, merging or settling is never re-claimed.
func TestAdmitNext_StaleReclaimCoversAdvancing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	epoch := f.admitAt(e.ID)
	if _, err := f.store.SetPhase(ctx, e.ID, epoch, PhaseAdvancing); err != nil {
		t.Fatalf("SetPhase(advancing): %v", err)
	}
	f.backdateStale(e.ID)
	adm := f.admitNext(mainBase())
	if !adm.Admitted || !adm.Readmitted || adm.Entry.ID != e.ID {
		t.Fatalf("stale advancing AdmitNext = %+v, want re-claimed", adm)
	}
	got := f.entry(e.ID)
	if got.AdmissionEpoch != epoch+1 || got.Phase != PhaseAdvancing || got.State != StateLive {
		t.Fatalf("re-claimed advancing entry = epoch %d phase %q state %s, want epoch %d, phase advancing kept, live",
			got.AdmissionEpoch, got.Phase, got.State, epoch+1)
	}
	epoch = got.AdmissionEpoch
	for _, ph := range []Phase{PhaseVerifying, PhaseUnverified, PhasePassed, PhaseMerging, PhaseSettling} {
		if _, err := f.store.SetPhase(ctx, e.ID, epoch, ph); err != nil {
			t.Fatalf("SetPhase(%s): %v", ph, err)
		}
		f.backdateStale(e.ID)
		if adm := f.admitNext(mainBase()); adm.Admitted || adm.Readmitted || adm.Entry == nil || adm.Entry.ID != e.ID {
			t.Errorf("stale %s AdmitNext = %+v, want no re-claim (the live entry returned)", ph, adm)
		}
		if got := f.entry(e.ID); got.AdmissionEpoch != epoch {
			t.Errorf("stale %s entry epoch = %d, want %d unchanged", ph, got.AdmissionEpoch, epoch)
		}
	}
}

// TestAdmitNextReclaiming_BootInstant (counterfactuals A2, A3): a live entry
// admitted 1 s before the boot instant and NOT stale is re-claimed and
// recorded once; one admitted after it is untouched and unrecorded; no other
// phase is ever re-claimed before the instant; a stale entry admitted after
// the instant is re-claimed AND recorded; an empty live slot admits the first
// held row like AdmitNext without a record; and the request is validated
// before any SQL, with nothing written.
func TestAdmitNextReclaiming_BootInstant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Empty live slot: admits the first held row like AdmitNext, no record.
	a := f.enqueue(f.run(), "main", "h1").Entry
	f.enqueue(f.run(), "main", "x1")
	rec := &reclaimRecorder{pool: f.pool}
	adm := f.reclaiming(mainBase(), f.databaseNow(), rec)
	if !adm.Admitted || adm.Readmitted || adm.Entry.ID != a.ID || adm.Entry.Phase != PhaseAdmitted || adm.Entry.AdmissionEpoch != 1 {
		t.Fatalf("empty-slot AdmitNextReclaiming = %+v, want A admitted (not readmitted) at epoch 1", adm)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("an admission of a held row called Record %d times, want 0", len(rec.calls))
	}

	// Admitted AFTER the instant (captured first): untouched, unrecorded.
	b2 := f.enqueue(f.run(), "release", "r1").Entry
	release := Base{Repo: testRepo, BaseRef: "release"}
	bootAt := f.databaseNow()
	epochB := f.admitAt2(release, b2.ID)
	beforeB := f.entry(b2.ID)
	if adm := f.reclaiming(release, bootAt, rec); adm.Admitted || adm.Entry == nil || adm.Entry.ID != b2.ID {
		t.Fatalf("post-instant AdmitNextReclaiming = %+v, want no re-claim", adm)
	}
	if got := f.entry(b2.ID); got.AdmissionEpoch != epochB || !got.AdmittedAt.Equal(*beforeB.AdmittedAt) || len(rec.calls) != 0 {
		t.Fatalf("post-instant entry = epoch %d admitted_at %v (records %d), want epoch %d at %v and no record",
			got.AdmissionEpoch, got.AdmittedAt, len(rec.calls), epochB, beforeB.AdmittedAt)
	}

	// Admitted 1 s BEFORE the instant, fresh (not stale): re-claimed, recorded.
	bootAt = f.databaseNow()
	f.setAdmittedAt(a.ID, bootAt, -time.Second)
	stamped := f.entry(a.ID).AdmittedAt
	adm = f.reclaiming(mainBase(), bootAt, rec)
	if !adm.Admitted || !adm.Readmitted || adm.Entry.ID != a.ID || adm.Entry.AdmissionEpoch != 2 {
		t.Fatalf("pre-instant AdmitNextReclaiming = %+v, want A re-claimed at epoch 2", adm)
	}
	got := f.entry(a.ID)
	if got.AdmissionEpoch != 2 || !got.AdmittedAt.After(*stamped) || got.Phase != PhaseAdmitted {
		t.Fatalf("re-claimed entry = %+v, want epoch 2, a fresh admitted_at, phase kept", got)
	}
	if len(rec.calls) != 1 || rec.calls[0].ID != a.ID || rec.calls[0].AdmissionEpoch != 2 ||
		rec.txEpoch[0] != 2 || rec.poolEpoch[0] != 1 {
		t.Fatalf("Record calls = %d (tx epoch %v, committed %v), want one for A at epoch 2 seen in-tx, uncommitted (1) outside",
			len(rec.calls), rec.txEpoch, rec.poolEpoch)
	}
	// Re-asked at the same instant: A's admitted_at is now after it.
	if adm := f.reclaiming(mainBase(), bootAt, rec); adm.Admitted || len(rec.calls) != 1 {
		t.Fatalf("second AdmitNextReclaiming = %+v (records %d), want no second re-claim", adm, len(rec.calls))
	}

	// Every non-admission phase before the instant: never re-claimed.
	epoch := int64(2)
	for _, ph := range []Phase{PhaseVerifying, PhaseUnverified, PhasePassed, PhaseMerging, PhaseSettling} {
		if _, err := f.store.SetPhase(ctx, a.ID, epoch, ph); err != nil {
			t.Fatalf("SetPhase(%s): %v", ph, err)
		}
		at := f.databaseNow()
		f.setAdmittedAt(a.ID, at, -time.Second)
		if adm := f.reclaiming(mainBase(), at, rec); adm.Admitted || adm.Readmitted {
			t.Errorf("pre-instant %s AdmitNextReclaiming = %+v, want no re-claim", ph, adm)
		}
		if got := f.entry(a.ID); got.AdmissionEpoch != epoch || len(rec.calls) != 1 {
			t.Errorf("pre-instant %s entry epoch = %d (records %d), want %d and no record", ph, got.AdmissionEpoch, len(rec.calls), epoch)
		}
	}

	// A STALE advancing entry admitted after an old instant: the stale
	// re-claim runs in the reclaiming variant too, and records.
	if _, err := f.store.SetPhase(ctx, a.ID, epoch, PhaseAdvancing); err != nil {
		t.Fatalf("SetPhase(advancing): %v", err)
	}
	old := f.databaseNow().Add(-time.Hour) // a database instant, offset
	f.backdateStale(a.ID)
	if adm := f.reclaiming(mainBase(), old, rec); !adm.Readmitted || adm.Entry.AdmissionEpoch != epoch+1 || len(rec.calls) != 2 {
		t.Fatalf("stale AdmitNextReclaiming = %+v (records %d), want re-claimed at epoch %d and recorded", adm, len(rec.calls), epoch+1)
	}

	// Validation: refused before any SQL, with a held row that WOULD be
	// admitted left held (committed state, not only the error).
	held := f.enqueue(f.run(), "other", "o1").Entry
	other := Base{Repo: testRepo, BaseRef: "other"}
	for name, req := range map[string]ReclaimRequest{
		"zero instant": {Base: other, Record: rec.record},
		"nil record":   {Base: other, AdmittedBefore: f.databaseNow()},
		"invalid base": {Base: Base{Repo: testRepo}, AdmittedBefore: f.databaseNow(), Record: rec.record},
	} {
		if _, err := f.store.AdmitNextReclaiming(ctx, req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if got := f.entry(held.ID); got.State != StateHeld || got.AdmittedAt != nil {
		t.Fatalf("held row after refused calls = %+v, want still held", got)
	}
}

// admitAt2 is admitAt on any base.
func (f *fixture) admitAt2(base Base, want uuid.UUID) int64 {
	f.t.Helper()
	adm := f.admitNext(base)
	if !adm.Admitted || adm.Entry.ID != want {
		f.t.Fatalf("AdmitNext(%s) = %+v, want %s admitted", base.BaseRef, adm, want)
	}
	return adm.Entry.AdmissionEpoch
}

// TestAdmitNextReclaiming_RecordFailureRollsBack (counterfactual A4): a Record
// error is ErrReclaimUnrecorded wrapping it, and the COMMITTED row (read
// back) keeps its admission epoch and admitted_at; a retry with a working
// Record then re-claims.
func TestAdmitNextReclaiming_RecordFailureRollsBack(t *testing.T) {
	f := newFixture(t)
	e := f.enqueue(f.run(), "main", "h1").Entry
	epoch := f.admitAt(e.ID)
	bootAt := f.databaseNow()
	f.setAdmittedAt(e.ID, bootAt, -time.Second)
	before := f.entry(e.ID)

	boom := errors.New("audit append failed")
	rec := &reclaimRecorder{pool: f.pool, err: boom}
	_, err := f.store.AdmitNextReclaiming(context.Background(), ReclaimRequest{Base: mainBase(), AdmittedBefore: bootAt, Record: rec.record})
	if !errors.Is(err, ErrReclaimUnrecorded) || !errors.Is(err, boom) {
		t.Fatalf("AdmitNextReclaiming with a failing Record err = %v, want ErrReclaimUnrecorded wrapping the record error", err)
	}
	after := f.entry(e.ID)
	if after.AdmissionEpoch != epoch || !after.AdmittedAt.Equal(*before.AdmittedAt) || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("entry after a failed record = epoch %d admitted_at %v, want epoch %d at %v (rolled back)",
			after.AdmissionEpoch, after.AdmittedAt, epoch, before.AdmittedAt)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("Record calls = %d, want 1", len(rec.calls))
	}
	rec.err = nil
	if adm := f.reclaiming(mainBase(), bootAt, rec); !adm.Readmitted || f.entry(e.ID).AdmissionEpoch != epoch+1 {
		t.Fatalf("retry = %+v, want re-claimed at epoch %d", adm, epoch+1)
	}
}

// TestAdmitNextReclaiming_SkipsYieldedAndPreservesWaitState (R6;
// counterfactuals A5, A6). (a) No live row: an OLDER yielded held row Y ahead
// of a younger unmarked held row U: U is admitted, and Y keeps its marker,
// yielded_at and approval_wait_started_at. (b) A pre-instant live admitted
// row L carrying an approval-wait anchor (ApprovalWait), a promote order_key
// and an anchored head/base is re-claimed (epoch +1) with every one of those,
// enqueued_at and the NULL yield columns read back unchanged.
func TestAdmitNextReclaiming_SkipsYieldedAndPreservesWaitState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := &reclaimRecorder{pool: f.pool}

	y, u := uuid.New(), uuid.New()
	f.seed(y, f.run(), StateHeld, 2*time.Hour)
	f.seed(u, f.run(), StateHeld, time.Hour)
	f.markYielded(y, "forge_approval_absent", time.Minute)
	f.exec(`UPDATE merge_candidate_queue_entries SET approval_wait_started_at = clock_timestamp() - interval '5 minutes' WHERE id = $1`, y)
	yBefore := f.entry(y)
	adm := f.reclaiming(mainBase(), f.databaseNow(), rec)
	if !adm.Admitted || adm.Readmitted || adm.Entry.ID != u {
		t.Fatalf("AdmitNextReclaiming = %+v, want the younger unmarked %s admitted (the yielded %s skipped)", adm, u, y)
	}
	yAfter := f.entry(y)
	if yAfter.State != StateHeld || yAfter.YieldReason != yBefore.YieldReason || yAfter.YieldedAt == nil ||
		!yAfter.YieldedAt.Equal(*yBefore.YieldedAt) || yAfter.ApprovalWaitStartedAt == nil ||
		!yAfter.ApprovalWaitStartedAt.Equal(*yBefore.ApprovalWaitStartedAt) || !yAfter.UpdatedAt.Equal(yBefore.UpdatedAt) {
		t.Fatalf("yielded row after admission = %+v, want held with marker, yielded_at and wait anchor unchanged (%+v)", yAfter, yBefore)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("an admission called Record %d times, want 0", len(rec.calls))
	}

	// (b) on another base: X ahead by enqueue time, L promoted past it.
	release := Base{Repo: testRepo, BaseRef: "release"}
	f.enqueue(f.run(), "release", "x1")
	lRun := f.run()
	l := f.enqueue(lRun, "release", "l1").Entry
	if _, err := f.store.Promote(ctx, PromoteRequest{RunID: lRun, Record: func(context.Context, pgx.Tx, Promotion) error { return nil }}); err != nil {
		t.Fatalf("Promote(L): %v", err)
	}
	epoch := f.admitAt2(release, l.ID)
	f.approvalWait(l.ID, epoch, time.Hour)
	bootAt := f.databaseNow()
	f.setAdmittedAt(l.ID, bootAt, -time.Second)
	before := f.entry(l.ID)
	if before.OrderKey == nil || before.ApprovalWaitStartedAt == nil || before.AnchoredBaseSHA == "" {
		t.Fatalf("seeded L = %+v, want an order key, a wait anchor and an anchored base", before)
	}
	adm = f.reclaiming(release, bootAt, rec)
	if !adm.Readmitted || adm.Entry.ID != l.ID || adm.Entry.AdmissionEpoch != epoch+1 {
		t.Fatalf("AdmitNextReclaiming(L) = %+v, want L re-claimed at epoch %d", adm, epoch+1)
	}
	after := f.entry(l.ID)
	switch {
	case after.AdmissionEpoch != epoch+1 || after.State != StateLive || after.Phase != before.Phase:
		t.Fatalf("re-claimed L = %+v, want live at epoch %d, phase %q kept", after, epoch+1, before.Phase)
	case after.ApprovalWaitStartedAt == nil || !after.ApprovalWaitStartedAt.Equal(*before.ApprovalWaitStartedAt):
		t.Fatalf("L's approval_wait_started_at = %v, want %v kept (R6: no new approval-wait episode)", after.ApprovalWaitStartedAt, before.ApprovalWaitStartedAt)
	case after.AnchoredHeadSHA != before.AnchoredHeadSHA || after.AnchoredBaseSHA != before.AnchoredBaseSHA || !after.AnchoredAt.Equal(before.AnchoredAt):
		t.Fatalf("L's anchor = %q/%q at %v, want %q/%q at %v kept", after.AnchoredHeadSHA, after.AnchoredBaseSHA, after.AnchoredAt,
			before.AnchoredHeadSHA, before.AnchoredBaseSHA, before.AnchoredAt)
	case after.OrderKey == nil || !after.OrderKey.Equal(*before.OrderKey) || !after.EnqueuedAt.Equal(before.EnqueuedAt):
		t.Fatalf("L's order key %v / enqueued_at %v, want %v / %v kept", after.OrderKey, after.EnqueuedAt, before.OrderKey, before.EnqueuedAt)
	case after.YieldReason != "" || after.YieldDetail != "" || after.YieldedAt != nil:
		t.Fatalf("L's yield columns = %q/%q/%v, want NULL", after.YieldReason, after.YieldDetail, after.YieldedAt)
	}
	if len(rec.calls) != 1 || rec.calls[0].ID != l.ID {
		t.Fatalf("Record calls = %d, want one for L", len(rec.calls))
	}
}

// TestDatabaseNow: the store reads the database clock (an admission made
// after it stamps a later admitted_at), and a canceled context fails.
func TestDatabaseNow(t *testing.T) {
	f := newFixture(t)
	now := f.databaseNow()
	e := f.enqueue(f.run(), "main", "h1").Entry
	f.admitAt(e.ID)
	if got := f.entry(e.ID).AdmittedAt; got == nil || !got.After(now) {
		t.Fatalf("admitted_at %v after DatabaseNow %v, want later on the same clock", got, now)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := f.store.DatabaseNow(ctx); !errors.Is(err, context.Canceled) || !got.IsZero() {
		t.Fatalf("DatabaseNow(canceled) = %v, %v; want the zero time and context.Canceled", got, err)
	}
}

// TestSetPhase_D8Steps: the ADR-093 D8 phases round-trip under the epoch CAS
// (a stale epoch is refused and the phase read back is unchanged).
func TestSetPhase_D8Steps(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	epoch := f.admitAt(e.ID)
	for _, ph := range []Phase{PhaseAdvancing, PhaseVerifying, PhasePassed, PhaseMerging, PhaseSettling} {
		got, err := f.store.SetPhase(ctx, e.ID, epoch, ph)
		if err != nil || got.Phase != ph || f.entry(e.ID).Phase != ph {
			t.Fatalf("SetPhase(%s) = %+v, %v; want phase %s read back", ph, got, err, ph)
		}
		if _, err := f.store.SetPhase(ctx, e.ID, epoch+1, PhaseAdmitted); !errors.Is(err, ErrStateConflict) {
			t.Fatalf("SetPhase at a stale epoch err = %v, want ErrStateConflict", err)
		}
		if got := f.entry(e.ID).Phase; got != ph {
			t.Fatalf("phase after a refused SetPhase = %q, want %q", got, ph)
		}
	}
	if string(PhaseAdvancing) != "advancing" || string(PhaseMerging) != "merging" || string(PhaseSettling) != "settling" {
		t.Fatalf("D8 phase values = %q/%q/%q, want advancing/merging/settling", PhaseAdvancing, PhaseMerging, PhaseSettling)
	}
	if IntentFnTimeout != intentFnTimeout {
		t.Fatalf("IntentFnTimeout = %s, want the lock's fn deadline %s", IntentFnTimeout, intentFnTimeout)
	}
}

// --- docs/adr/112 Amendment A3 (#4780): the yield revision, the row-4 flag,
// the retained approval anchor and the merge-gate window with its budget.
// Every time offset is set in SQL relative to clock_timestamp().

// gateWait calls GateWait, failing on an error.
func (f *fixture) gateWait(id uuid.UUID, epoch int64, window time.Duration) GateWaitResult {
	f.t.Helper()
	res, err := f.store.GateWait(context.Background(), id, epoch, window)
	if err != nil {
		f.t.Fatalf("GateWait(%s, %d): %v", id, epoch, err)
	}
	return res
}

// readvance unadmits e at epoch and re-admits it, returning the new epoch.
func (f *fixture) readvance(id uuid.UUID, epoch int64) int64 {
	f.t.Helper()
	if _, err := f.store.Unadmit(context.Background(), id, epoch); err != nil {
		f.t.Fatalf("Unadmit: %v", err)
	}
	return f.admitAt(id)
}

// TestGateWait_OpensOncePerEpoch (T5, CF8 vehicle): the first call in an
// admission epoch spends one token (3 to 2) and stamps the window's start and
// epoch; a second call in the SAME epoch (a (b) hold, then a (c) hold) spends
// nothing and continues the same start; a call in a NEW epoch (after a
// readvance) opens a FRESH window. MECHANISM: the old window's start is
// backdated a minute, so only the epoch comparison tells the re-advanced
// epoch's fresh start from the kept one.
func TestGateWait_OpensOncePerEpoch(t *testing.T) {
	f := newFixture(t)
	e := f.enqueue(f.run(), "main", "h1").Entry
	epoch := f.admitAt(e.ID)

	first := f.gateWait(e.ID, epoch, time.Hour)
	if !first.Opened || first.Expired || first.NoToken || first.Entry.GateHoldTokens != 2 ||
		first.Entry.GateWaitEpoch == nil || *first.Entry.GateWaitEpoch != epoch || first.Entry.GateWaitStartedAt == nil {
		t.Fatalf("first GateWait = %+v, want opened, one token spent (2 left), window stamped at epoch %d", first, epoch)
	}
	second := f.gateWait(e.ID, epoch, time.Hour)
	if second.Opened || second.Entry.GateHoldTokens != 2 || !second.Entry.GateWaitStartedAt.Equal(*first.Entry.GateWaitStartedAt) {
		t.Fatalf("same-epoch GateWait = %+v, want the window continued with no spend", second)
	}

	f.exec(`UPDATE merge_candidate_queue_entries SET gate_wait_started_at = gate_wait_started_at - interval '1 minute' WHERE id = $1`, e.ID)
	old := *f.entry(e.ID).GateWaitStartedAt
	epoch2 := f.readvance(e.ID, epoch)
	third := f.gateWait(e.ID, epoch2, time.Hour)
	if !third.Opened || third.Entry.GateHoldTokens != 1 || *third.Entry.GateWaitEpoch != epoch2 || !third.Entry.GateWaitStartedAt.After(old) {
		t.Fatalf("new-epoch GateWait = %+v, want a fresh window at epoch %d (start after %v), one more token spent", third, epoch2, old)
	}
}

// TestGateWait_BudgetExhaustedReturnsNoToken (T6, CF9 vehicle): windows
// opened in three distinct epochs with no elapsed refill time empty the
// budget; the fourth epoch answers NoToken and writes NOTHING (re-read: the
// third window and its epoch stand, zero tokens). MECHANISM: no refill time
// passes, so only the decrement empties the bucket.
func TestGateWait_BudgetExhaustedReturnsNoToken(t *testing.T) {
	f := newFixture(t)
	e := f.enqueue(f.run(), "main", "h1").Entry
	epoch := f.admitAt(e.ID)
	for i := 0; i < GateHoldBudget; i++ {
		if i > 0 {
			epoch = f.readvance(e.ID, epoch)
		}
		if res := f.gateWait(e.ID, epoch, time.Hour); !res.Opened {
			t.Fatalf("window %d = %+v, want opened", i+1, res)
		}
	}
	before := f.entry(e.ID)
	epoch4 := f.readvance(e.ID, epoch)
	res := f.gateWait(e.ID, epoch4, time.Hour)
	if !res.NoToken || res.Opened || res.Expired {
		t.Fatalf("fourth-epoch GateWait = %+v, want NoToken", res)
	}
	after := f.entry(e.ID)
	if after.GateHoldTokens != 0 || *after.GateWaitEpoch != epoch || !after.GateWaitStartedAt.Equal(*before.GateWaitStartedAt) ||
		!after.GateTokensRefilledAt.Equal(*before.GateTokensRefilledAt) {
		t.Fatalf("after NoToken = tokens %d epoch %v start %v, want nothing written (0, %d, %v)",
			after.GateHoldTokens, after.GateWaitEpoch, after.GateWaitStartedAt, epoch, before.GateWaitStartedAt)
	}
}

// TestGateWait_RefillRestoresOneTokenPerHour (T7, CF10 vehicle): an empty
// bucket whose refill clock is 61 minutes old accrues one token and the open
// spends it (0 left); one 181 minutes old accrues three, capped at the
// budget before the spend (2 left), and its refill clock restarts at now.
func TestGateWait_RefillRestoresOneTokenPerHour(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ago     string
		left    int
		restart bool
	}{
		{"one hour accrues one token", "61 minutes", 0, false},
		{"three hours cap at the budget", "181 minutes", GateHoldBudget - 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			e := f.enqueue(f.run(), "main", "h1").Entry
			epoch := f.admitAt(e.ID)
			f.exec(`UPDATE merge_candidate_queue_entries SET gate_hold_tokens = 0,
  gate_tokens_refilled_at = clock_timestamp() - $2::interval WHERE id = $1`, e.ID, tc.ago)
			res := f.gateWait(e.ID, epoch, time.Hour)
			if !res.Opened || res.Entry.GateHoldTokens != tc.left {
				t.Fatalf("GateWait after %s = %+v, want opened with %d tokens left", tc.ago, res, tc.left)
			}
			var restarted, kept bool
			if err := f.pool.QueryRow(context.Background(), `SELECT gate_tokens_refilled_at > clock_timestamp() - interval '1 minute',
  gate_tokens_refilled_at <= clock_timestamp() FROM merge_candidate_queue_entries WHERE id = $1`, e.ID).Scan(&restarted, &kept); err != nil {
				t.Fatalf("read the refill clock: %v", err)
			}
			if restarted != tc.restart || !kept {
				t.Fatalf("refill clock restarted=%v (want %v), not past now=%v", restarted, tc.restart, kept)
			}
		})
	}
}

// TestGateWait_FutureRefillClockAccruesNothing (approval condition "GateWait
// refill"): a refill clock two hours in the FUTURE yields k clamped to 0 (a
// negative k is never carried), so a bucket of one token opens and leaves 0,
// and the clock is pulled back to now (it never moves past now).
// Counterfactual: without the clamp at 0 the negative k empties the bucket
// and the open answers NoToken, RED.
func TestGateWait_FutureRefillClockAccruesNothing(t *testing.T) {
	f := newFixture(t)
	e := f.enqueue(f.run(), "main", "h1").Entry
	epoch := f.admitAt(e.ID)
	f.exec(`UPDATE merge_candidate_queue_entries SET gate_hold_tokens = 1,
  gate_tokens_refilled_at = clock_timestamp() + interval '2 hours' WHERE id = $1`, e.ID)
	res := f.gateWait(e.ID, epoch, time.Hour)
	if !res.Opened || res.NoToken || res.Entry.GateHoldTokens != 0 {
		t.Fatalf("GateWait with a future refill clock = %+v, want opened with 0 tokens left", res)
	}
	var notPastNow bool
	if err := f.pool.QueryRow(context.Background(), `SELECT gate_tokens_refilled_at <= clock_timestamp()
FROM merge_candidate_queue_entries WHERE id = $1`, e.ID).Scan(&notPastNow); err != nil || !notPastNow {
		t.Fatalf("refill clock after the open: not past now=%v (%v), want true", notPastNow, err)
	}
}

// TestGateWait_NeverTouchesApprovalAnchor (T8, CF11 vehicle): across an
// open, a continue and an expiry the approval anchor stays NULL and no
// approval wait opens.
func TestGateWait_NeverTouchesApprovalAnchor(t *testing.T) {
	f := newFixture(t)
	e := f.enqueue(f.run(), "main", "h1").Entry
	epoch := f.admitAt(e.ID)
	check := func(why string) {
		t.Helper()
		if g := f.entry(e.ID); g.ApprovalWaitStartedAt != nil || g.ApprovalWaitOpen {
			t.Fatalf("%s: approval anchor %v open=%v, want NULL and closed", why, g.ApprovalWaitStartedAt, g.ApprovalWaitOpen)
		}
	}
	f.gateWait(e.ID, epoch, time.Hour)
	check("open")
	f.gateWait(e.ID, epoch, time.Hour)
	check("continue")
	f.exec(`UPDATE merge_candidate_queue_entries SET gate_wait_started_at = clock_timestamp() - interval '2 hours' WHERE id = $1`, e.ID)
	if res := f.gateWait(e.ID, epoch, time.Hour); !res.Expired {
		t.Fatalf("backdated window = %+v, want expired", res)
	}
	check("expiry")
}

// TestGateWait_CASAndExpiry (T9): a held row and a stale epoch are
// ErrStateConflict with NOTHING written (re-read: full budget, no window); a
// non-positive window is ErrInvalidRequest; expiry is judged on the database
// clock against the window's start (a fresh window has time remaining, a
// backdated one is expired with none).
func TestGateWait_CASAndExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	untouched := func(why string) {
		t.Helper()
		if g := f.entry(e.ID); g.GateHoldTokens != GateHoldBudget || g.GateWaitStartedAt != nil || g.GateWaitEpoch != nil {
			t.Fatalf("%s: entry = tokens %d window %v/%v, want nothing written", why, g.GateHoldTokens, g.GateWaitStartedAt, g.GateWaitEpoch)
		}
	}
	if _, err := f.store.GateWait(ctx, e.ID, 0, time.Hour); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("GateWait(held) err = %v, want ErrStateConflict", err)
	}
	untouched("held")
	epoch := f.admitAt(e.ID)
	if _, err := f.store.GateWait(ctx, e.ID, epoch-1, time.Hour); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("GateWait(stale epoch) err = %v, want ErrStateConflict", err)
	}
	untouched("stale epoch")
	for _, w := range []time.Duration{0, -time.Minute} {
		if _, err := f.store.GateWait(ctx, e.ID, epoch, w); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("GateWait(window %s) err = %v, want ErrInvalidRequest", w, err)
		}
	}
	untouched("invalid window")
	if res := f.gateWait(e.ID, epoch, time.Hour); res.Expired || res.Remaining <= 0 || res.Remaining > time.Hour {
		t.Fatalf("fresh window = %+v, want unexpired with (0, 1h] remaining", res)
	}
	f.exec(`UPDATE merge_candidate_queue_entries SET gate_wait_started_at = clock_timestamp() - interval '61 minutes' WHERE id = $1`, e.ID)
	if res := f.gateWait(e.ID, epoch, time.Hour); !res.Expired || res.Remaining != 0 || res.Opened {
		t.Fatalf("backdated window = %+v, want expired with nothing remaining, not re-opened", res)
	}
}

// yieldLive admits e and yields it with m, returning the yielded entry.
func (f *fixture) yieldLive(id uuid.UUID, m YieldMarker) Entry {
	f.t.Helper()
	y, err := f.store.Yield(context.Background(), id, f.admitAt(id), m)
	if err != nil {
		f.t.Fatalf("Yield: %v", err)
	}
	return y
}

// TestYieldRevision_EveryMarkerWriteBumps (T10, CF12 vehicle): a marked
// insert starts at revision 1, and Yield, UpdateYield, ReanchorYielded,
// ClearYield and Enqueue's mark of an unmarked held row each bump the
// revision by exactly one; a marker-less re-anchor does not.
func TestYieldRevision_EveryMarkerWriteBumps(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ins, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: f.run(), BaseRef: "main", HeadSHA: "m1", Yield: &YieldMarker{Reason: "forge_approval_absent"}})
	if err != nil || ins.Entry.YieldRevision != 1 {
		t.Fatalf("marked insert revision = %d (%v), want 1", ins.Entry.YieldRevision, err)
	}
	a := f.run()
	e := f.enqueue(a, "main", "h1").Entry
	if e.YieldRevision != 0 {
		t.Fatalf("unmarked insert revision = %d, want 0", e.YieldRevision)
	}
	want := int64(0)
	step := func(op string, got Entry, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		want++
		if got.YieldRevision != want || f.entry(e.ID).YieldRevision != want {
			t.Fatalf("%s: revision = %d (read %d), want %d", op, got.YieldRevision, f.entry(e.ID).YieldRevision, want)
		}
	}
	y, err := f.store.Yield(ctx, e.ID, f.admitAt(e.ID), YieldMarker{Reason: "forge_approval_absent"})
	step("Yield", y, err)
	u, err := f.store.UpdateYield(ctx, e.ID, "h1", y.YieldRevision, YieldMarker{Reason: "merge_checks_red"})
	step("UpdateYield", u, err)
	r, err := f.store.ReanchorYielded(ctx, e.ID, "h1", u.YieldRevision, "h2", "")
	step("ReanchorYielded", r, err)
	c, err := f.store.ClearYield(ctx, e.ID, "h2", r.YieldRevision, false)
	step("ClearYield", c, err)
	res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: a, BaseRef: "main", HeadSHA: "h2", Yield: &YieldMarker{Reason: "forge_approval_absent"}})
	step("Enqueue mark", res.Entry, err)
	plain := f.enqueue(a, "main", "h3")
	if plain.Entry.YieldRevision != want {
		t.Fatalf("marker-less re-anchor revision = %d, want unchanged %d", plain.Entry.YieldRevision, want)
	}
}

// TestClearYield_StaleRevisionRefused (T11, CF13 vehicle): a probe reads the
// entry at revision r; the marker clears, then re-yields CONTINUING the same
// yielded_at at the same head; the delayed probe's ClearYield at r is
// ErrStateConflict and a READ shows the marker still set. MECHANISM: the
// continued episode reuses (head, yielded_at), so only the revision separates
// the old marker from the new.
func TestClearYield_StaleRevisionRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	read := f.yieldLive(e.ID, YieldMarker{Reason: "merge_checks_red"})
	if _, err := f.store.ClearYield(ctx, e.ID, "h1", read.YieldRevision, false); err != nil {
		t.Fatalf("ClearYield: %v", err)
	}
	again := f.yieldLive(e.ID, YieldMarker{Reason: "merge_checks_red", Continue: &YieldEpisode{YieldedAt: *read.YieldedAt, BaseRef: "main"}})
	if !again.YieldedAt.Equal(*read.YieldedAt) || again.AnchoredHeadSHA != read.AnchoredHeadSHA {
		t.Fatalf("re-yield = %v at %s, want the continued %v at the same head", again.YieldedAt, again.AnchoredHeadSHA, read.YieldedAt)
	}
	if _, err := f.store.ClearYield(ctx, e.ID, "h1", read.YieldRevision, false); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale ClearYield err = %v, want ErrStateConflict", err)
	}
	if g := f.entry(e.ID); !g.Yielded() || g.YieldReason != "merge_checks_red" {
		t.Fatalf("after the stale clear = %+v, want the newer marker standing", g)
	}
}

// TestUpdateYield_StaleRevisionCannotOverwriteNewerReason (T12): a probe
// reads revision r; a newer probe transitions the reason (same head and
// yielded_at); the delayed probe's UpdateYield at r is ErrStateConflict and a
// READ shows the newer reason intact.
func TestUpdateYield_StaleRevisionCannotOverwriteNewerReason(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	read := f.yieldLive(e.ID, YieldMarker{Reason: "merge_checks_pending"})
	if _, err := f.store.UpdateYield(ctx, e.ID, "h1", read.YieldRevision, YieldMarker{Reason: "merge_checks_red"}); err != nil {
		t.Fatalf("newer UpdateYield: %v", err)
	}
	if _, err := f.store.UpdateYield(ctx, e.ID, "h1", read.YieldRevision, YieldMarker{Reason: "merge_review_pending"}); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("stale UpdateYield err = %v, want ErrStateConflict", err)
	}
	if g := f.entry(e.ID); g.YieldReason != "merge_checks_red" || !g.YieldedAt.Equal(*read.YieldedAt) {
		t.Fatalf("after the stale update = %q at %v, want merge_checks_red in the episode %v", g.YieldReason, g.YieldedAt, read.YieldedAt)
	}
}

// TestRow4Flag_Lifecycle (T13, CF14 vehicle): a Row4 Yield sets row4_flag;
// an UpdateYield to merge_checks_red (no Row4) keeps it; ClearYield(clearRow4
// false) keeps it; a later ClearYield(clearRow4 true) clears it.
func TestRow4Flag_Lifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	y := f.yieldLive(e.ID, YieldMarker{Reason: "forge_review_required", Row4: true})
	if !y.Row4Flag {
		t.Fatal("a Row4 Yield did not set row4_flag")
	}
	u, err := f.store.UpdateYield(ctx, e.ID, "h1", y.YieldRevision, YieldMarker{Reason: "merge_checks_red"})
	if err != nil || !u.Row4Flag || !f.entry(e.ID).Row4Flag {
		t.Fatalf("UpdateYield to merge_checks_red: flag=%v (%v), want kept", u.Row4Flag, err)
	}
	c, err := f.store.ClearYield(ctx, e.ID, "h1", u.YieldRevision, false)
	if err != nil || !c.Row4Flag || !f.entry(e.ID).Row4Flag {
		t.Fatalf("ClearYield(clearRow4=false): flag=%v (%v), want kept", c.Row4Flag, err)
	}
	y2 := f.yieldLive(e.ID, YieldMarker{Reason: "merge_checks_red"})
	if !y2.Row4Flag {
		t.Fatal("a non-Row4 Yield cleared row4_flag")
	}
	c2, err := f.store.ClearYield(ctx, e.ID, "h1", y2.YieldRevision, true)
	if err != nil || c2.Row4Flag || f.entry(e.ID).Row4Flag {
		t.Fatalf("ClearYield(clearRow4=true): flag=%v (%v), want cleared", c2.Row4Flag, err)
	}
}

// TestClearYield_RetainsApprovalAnchorAndClosesWait (T14, CF15 vehicle):
// ApprovalWait stamps the anchor and opens the wait; a yield keeps both; a
// clear RETAINS the anchor (docs/adr/112 A3.7) and closes the wait.
func TestClearYield_RetainsApprovalAnchorAndClosesWait(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	epoch := f.admitAt(e.ID)
	wait := f.approvalWait(e.ID, epoch, time.Hour)
	if !wait.Entry.ApprovalWaitOpen {
		t.Fatal("ApprovalWait did not open the wait")
	}
	y, err := f.store.Yield(ctx, e.ID, epoch, YieldMarker{Reason: "forge_approval_absent"})
	if err != nil || !y.ApprovalWaitOpen {
		t.Fatalf("Yield: open=%v (%v), want the wait kept open", y.ApprovalWaitOpen, err)
	}
	c, err := f.store.ClearYield(ctx, e.ID, "h1", y.YieldRevision, false)
	if err != nil {
		t.Fatalf("ClearYield: %v", err)
	}
	for _, g := range []Entry{c, f.entry(e.ID)} {
		if g.ApprovalWaitStartedAt == nil || !g.ApprovalWaitStartedAt.Equal(*wait.Entry.ApprovalWaitStartedAt) || g.ApprovalWaitOpen {
			t.Fatalf("cleared entry anchor %v open=%v, want the anchor %v retained and the wait closed",
				g.ApprovalWaitStartedAt, g.ApprovalWaitOpen, wait.Entry.ApprovalWaitStartedAt)
		}
	}
}

// TestApprovalWait_RetainedAnchorAfterClear (approval condition "A3.7
// approval anchor, rule (i)", the STORE pair): (i) an approval wait whose
// anchor is past the window, then a yield and a clear, then a re-admission
// in a LATER epoch: ApprovalWait reports EXPIRED at once (the anchor is
// continued, no fresh window) and re-opens the wait; (ii) a readvance after
// the clear leaves the retained anchor with the wait CLOSED, so a
// non-approval hold is never expired by it. Counterfactuals: restoring
// ClearYield's `approval_wait_started_at = NULL` stamps a fresh anchor in
// (i), not expired, RED; dropping ClearYield's `approval_wait_open = false`
// leaves the wait open in (ii), RED.
func TestApprovalWait_RetainedAnchorAfterClear(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.run(), "main", "h1").Entry
	epoch := f.admitAt(e.ID)
	f.approvalWait(e.ID, epoch, time.Hour)
	f.exec(`UPDATE merge_candidate_queue_entries SET approval_wait_started_at = clock_timestamp() - interval '2 hours' WHERE id = $1`, e.ID)
	anchor := *f.entry(e.ID).ApprovalWaitStartedAt
	y, err := f.store.Yield(ctx, e.ID, epoch, YieldMarker{Reason: "forge_approval_absent"})
	if err != nil {
		t.Fatalf("Yield: %v", err)
	}
	if _, err := f.store.ClearYield(ctx, e.ID, "h1", y.YieldRevision, false); err != nil {
		t.Fatalf("ClearYield: %v", err)
	}

	// (ii) a readvance after the clear: the anchor is retained, the wait closed.
	epoch2 := f.admitAt(e.ID)
	epoch3 := f.readvance(e.ID, epoch2)
	if g := f.entry(e.ID); g.ApprovalWaitStartedAt == nil || !g.ApprovalWaitStartedAt.Equal(anchor) || g.ApprovalWaitOpen {
		t.Fatalf("(ii) after a readvance = anchor %v open=%v, want %v retained and the wait closed", g.ApprovalWaitStartedAt, g.ApprovalWaitOpen, anchor)
	}

	// (i) an approval-fold block in the later epoch: expired at once.
	res := f.approvalWait(e.ID, epoch3, time.Hour)
	if !res.Expired || res.Remaining != 0 || !res.Entry.ApprovalWaitStartedAt.Equal(anchor) || !res.Entry.ApprovalWaitOpen {
		t.Fatalf("(i) ApprovalWait in a later epoch = %+v, want expired at once on the retained anchor %v, the wait re-opened", res, anchor)
	}
}

// --- ADR-112 R3 withdraw intent (#4685) ---

// withdrawRecorder records every WithdrawIntent Record call and the entry's
// state as the callback reads it ON tx (the settle visible, nothing
// committed).
type withdrawRecorder struct {
	calls     []Withdrawal
	stateOnTx []string
	readOnTxE error
	err       error
}

func (r *withdrawRecorder) record(ctx context.Context, tx pgx.Tx, w Withdrawal) error {
	r.calls = append(r.calls, w)
	var st string
	if err := tx.QueryRow(ctx, `SELECT state FROM merge_candidate_queue_entries WHERE id = $1`, w.Entry.ID).Scan(&st); err != nil {
		r.readOnTxE = err
	}
	r.stateOnTx = append(r.stateOnTx, st)
	return r.err
}

// yieldedIntent enqueues runID held on main at head, YIELDED by construction
// (the merge_run queue arm's Enqueue{MergeIntent, Yield}) with intent
// sequence seq, and returns the row read back.
func (f *fixture) yieldedIntent(runID uuid.UUID, head string, seq int64) Entry {
	f.t.Helper()
	res, err := f.store.Enqueue(context.Background(), EnqueueRequest{RunID: runID, BaseRef: "main", HeadSHA: head,
		MergeIntent: true, IntentSequence: seq, Yield: &YieldMarker{Reason: "forge_approval_absent", Detail: "no approving review"}})
	if err != nil {
		f.t.Fatalf("Enqueue(%s yielded intent): %v", runID, err)
	}
	e := f.entry(res.Entry.ID)
	if e.State != StateHeld || !e.Yielded() || e.MergeIntentAt == nil {
		f.t.Fatalf("seeded entry = %+v, want held, yielded, with intent", e)
	}
	return e
}

// claimRow reads runID's merge_dispatch_claims row back (found=false: none).
func (f *fixture) claimRow(runID uuid.UUID) (DispatchClaim, bool) {
	f.t.Helper()
	c, found, err := f.store.MergeDispatch(context.Background(), runID)
	if err != nil {
		f.t.Fatalf("MergeDispatch(%s): %v", runID, err)
	}
	return c, found
}

// sameEntryState reports whether two reads of one row agree on everything a
// withdrawal could write: state, reason, settle, intent and marker.
func sameEntryState(a, b Entry) bool {
	eqT := func(x, y *time.Time) bool { return (x == nil) == (y == nil) && (x == nil || x.Equal(*y)) }
	return a.ID == b.ID && a.State == b.State && a.EjectReason == b.EjectReason && eqT(a.SettledAt, b.SettledAt) &&
		eqT(a.MergeIntentAt, b.MergeIntentAt) && a.MergeIntentSequence == b.MergeIntentSequence &&
		eqT(a.YieldedAt, b.YieldedAt) && a.YieldReason == b.YieldReason && a.YieldRevision == b.YieldRevision
}

// TestWithdrawIntent_SettlesYieldedEntryDropped: with A live, B held and Y
// held YIELDED with intent, withdrawing Y settles it dropped intent_withdrawn
// in one transaction, read back as COMMITTED state: ActiveForRun(Y) finds
// nothing, LatestForRun(Y) is the dropped row with its merge_intent_at,
// merge_intent_sequence and yield marker retained, Record ran exactly once
// on tx seeing the settle, and A and B are untouched. Replacing the settle
// with a re-read (CF1's store half) leaves Y held: RED.
func TestWithdrawIntent_SettlesYieldedEntryDropped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, b, y := f.run(), f.run(), f.run()
	f.enqueue(a, "main", "ha")
	f.enqueue(b, "main", "hb")
	f.admitNext(mainBase())
	before := f.yieldedIntent(y, "hy", 41)

	rec := &withdrawRecorder{}
	w, err := f.store.WithdrawIntent(ctx, WithdrawIntentRequest{RunID: y, Record: rec.record})
	if err != nil {
		t.Fatalf("WithdrawIntent(Y): %v", err)
	}
	if w.Entry.ID != before.ID || w.Entry.State != StateDropped || w.Entry.EjectReason != ReasonIntentWithdrawn ||
		w.LiveRunID == nil || *w.LiveRunID != a {
		t.Fatalf("Withdrawal = %+v (live %v), want Y's entry dropped %s behind live A", w.Entry, w.LiveRunID, ReasonIntentWithdrawn)
	}
	if len(rec.calls) != 1 || rec.readOnTxE != nil || rec.stateOnTx[0] != string(StateDropped) {
		t.Fatalf("Record calls = %d (state on tx %v, err %v), want one call seeing the settle", len(rec.calls), rec.stateOnTx, rec.readOnTxE)
	}

	if _, found, err := f.store.ActiveForRun(ctx, y); err != nil || found {
		t.Fatalf("ActiveForRun(Y) after withdraw = found %v, err %v; want no active entry", found, err)
	}
	got, found, err := f.store.LatestForRun(ctx, y)
	if err != nil || !found {
		t.Fatalf("LatestForRun(Y) = found %v, err %v", found, err)
	}
	if got.ID != before.ID || got.State != StateDropped || got.EjectReason != ReasonIntentWithdrawn || got.SettledAt == nil {
		t.Fatalf("Y's row after withdraw = %+v, want the same row dropped %s", got, ReasonIntentWithdrawn)
	}
	if got.MergeIntentAt == nil || !got.MergeIntentAt.Equal(*before.MergeIntentAt) || got.MergeIntentSequence != 41 ||
		!got.Yielded() || got.YieldReason != before.YieldReason || !got.EnqueuedAt.Equal(before.EnqueuedAt) {
		t.Fatalf("Y's row after withdraw = %+v, want merge_intent_at, sequence 41, marker and enqueued_at retained (%+v)", got, before)
	}
	if order := f.listRuns(mainBase()); !slices.Equal(order, []uuid.UUID{a, b}) {
		t.Fatalf("ListForBase after withdraw = %v, want A, B", order)
	}
	if st := f.status(a); st.Entry.State != StateLive {
		t.Fatalf("A = %s, want still live", st.Entry.State)
	}
	if st := f.status(b); st.Entry.State != StateHeld || st.Entry.Yielded() || st.Position != 1 {
		t.Fatalf("B = %+v (position %d), want still held first, unmarked", st.Entry, st.Position)
	}
}

// TestWithdrawIntent_RefusalsWriteNothing: one row per refusal, each seeded
// BY CONSTRUCTION so the refusal under test is the only guard in the path,
// and each read back as COMMITTED state unchanged (the entry's state, reason,
// intent and marker, and the run's dispatch claim) with Record never called.
// Deleting the no-intent, unmarked-entry or dispatch-claim guard settles that
// row's entry dropped: RED on the state read-back. The live-entry refusal is
// pinned by error identity only: without it the row falls through to the
// not-held guard (ErrNotQueued), which also writes nothing (a masking
// downstream guard, declared).
func TestWithdrawIntent_RefusalsWriteNothing(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// seed returns the run and the entry id to read back (uuid.Nil: none).
		seed      func(f *fixture) (uuid.UUID, uuid.UUID)
		want      error
		wantClaim DispatchState // "" = no claim
	}{
		{name: "no entry", want: ErrNotQueued, seed: func(f *fixture) (uuid.UUID, uuid.UUID) { return f.run(), uuid.Nil }},
		{name: "terminal only", want: ErrNotQueued, seed: func(f *fixture) (uuid.UUID, uuid.UUID) {
			r := f.run()
			e := f.yieldedIntent(r, "h1", 3)
			if _, err := f.store.Settle(ctx, e.ID, StateEjected, ReentryReason); err != nil {
				f.t.Fatalf("Settle: %v", err)
			}
			return r, e.ID
		}},
		{name: "settled between peek and lock", want: ErrNotQueued, seed: func(f *fixture) (uuid.UUID, uuid.UUID) {
			r := f.run()
			e := f.yieldedIntent(r, "h1", 3)
			f.store.afterWithdrawPeek = func(context.Context) {
				if _, err := NewPostgresStore(f.pool).Settle(ctx, e.ID, StateDropped, "pr_closed"); err != nil {
					f.t.Errorf("settle in hook: %v", err)
				}
			}
			return r, e.ID
		}},
		{name: "live", want: ErrEntryLive, seed: func(f *fixture) (uuid.UUID, uuid.UUID) {
			r := f.run()
			res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h1", MergeIntent: true})
			if err != nil {
				f.t.Fatalf("Enqueue: %v", err)
			}
			f.admitAt(res.Entry.ID)
			return r, res.Entry.ID
		}},
		{name: "yielded without intent", want: ErrNoIntent, seed: func(f *fixture) (uuid.UUID, uuid.UUID) {
			r := f.run()
			res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h1",
				Yield: &YieldMarker{Reason: "forge_approval_absent"}})
			if err != nil {
				f.t.Fatalf("Enqueue: %v", err)
			}
			if e := f.entry(res.Entry.ID); !e.Yielded() || e.MergeIntentAt != nil {
				f.t.Fatalf("seed = %+v, want yielded with no intent", e)
			}
			return r, res.Entry.ID
		}},
		{name: "unmarked held with intent", want: ErrEntryNotYielded, seed: func(f *fixture) (uuid.UUID, uuid.UUID) {
			r := f.run()
			res, err := f.store.Enqueue(ctx, EnqueueRequest{RunID: r, BaseRef: "main", HeadSHA: "h1", MergeIntent: true})
			if err != nil {
				f.t.Fatalf("Enqueue: %v", err)
			}
			if e := f.entry(res.Entry.ID); e.Yielded() || e.MergeIntentAt == nil || e.State != StateHeld {
				f.t.Fatalf("seed = %+v, want held, unmarked, with intent", e)
			}
			return r, res.Entry.ID
		}},
		{name: "dispatch_failed claim", want: ErrDispatchClaimed, wantClaim: DispatchFailed, seed: func(f *fixture) (uuid.UUID, uuid.UUID) {
			r := f.run()
			e := f.yieldedIntent(r, "h1", 3)
			if _, err := f.store.ClaimMergeDispatch(ctx, r, DispatchPathInline, "h0"); err != nil {
				f.t.Fatalf("claim: %v", err)
			}
			if err := f.store.MarkMergeDispatchFailed(ctx, r, "forge 500"); err != nil {
				f.t.Fatalf("mark failed: %v", err)
			}
			return r, e.ID
		}},
		{name: "dispatched claim", want: ErrDispatchClaimed, wantClaim: DispatchDispatched, seed: func(f *fixture) (uuid.UUID, uuid.UUID) {
			r := f.run()
			e := f.yieldedIntent(r, "h1", 3)
			if _, err := f.store.ClaimMergeDispatch(ctx, r, DispatchPathQueue, "h1"); err != nil {
				f.t.Fatalf("claim: %v", err)
			}
			return r, e.ID
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r, id := tc.seed(f)
			var before Entry
			if id != uuid.Nil && tc.name != "settled between peek and lock" {
				before = f.entry(id)
			}
			claimBefore, claimedBefore := f.claimRow(r)
			rec := &withdrawRecorder{}
			_, err := f.store.WithdrawIntent(ctx, WithdrawIntentRequest{RunID: r, Record: rec.record})
			// COMMITTED state first (the claim, then the entry), so a deleted
			// guard turns RED on what it protects, not only on the error.
			claimAfter, claimedAfter := f.claimRow(r)
			if claimedAfter != claimedBefore || claimAfter.State != claimBefore.State || claimAfter.Path != claimBefore.Path ||
				claimAfter.FailureCause != claimBefore.FailureCause || !claimAfter.ClaimedAt.Equal(claimBefore.ClaimedAt) {
				t.Fatalf("claim changed by a refused withdraw: before %+v (%v), after %+v (%v)", claimBefore, claimedBefore, claimAfter, claimedAfter)
			}
			switch {
			case tc.name == "settled between peek and lock":
				if e := f.entry(id); e.State != StateDropped || e.EjectReason != "pr_closed" {
					t.Fatalf("entry = %s/%s, want the hook's dropped pr_closed (not re-settled)", e.State, e.EjectReason)
				}
			case id != uuid.Nil:
				if after := f.entry(id); !sameEntryState(before, after) {
					t.Fatalf("entry changed by a refused withdraw:\nbefore %+v\nafter  %+v", before, after)
				}
			default:
				if n := f.count(`run_id = $1`, r); n != 0 {
					t.Fatalf("rows for an unqueued run = %d, want 0", n)
				}
			}
			if len(rec.calls) != 0 {
				t.Fatalf("Record called %d times on a refusal, want 0", len(rec.calls))
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("WithdrawIntent err = %v, want %v", err, tc.want)
			}
			if tc.wantClaim != "" && (!claimedAfter || claimAfter.State != tc.wantClaim) {
				t.Fatalf("claim = %+v (found %v), want %s still standing", claimAfter, claimedAfter, tc.wantClaim)
			}
			if tc.wantClaim == DispatchFailed && claimAfter.FailureCause != "forge 500" {
				t.Fatalf("dispatch_failed cause = %q, want the original 'forge 500' (never cleared or re-armed)", claimAfter.FailureCause)
			}
			if tc.wantClaim != "" && !strings.Contains(err.Error(), string(tc.wantClaim)) {
				t.Fatalf("err = %v, want it to name the claim state %s", err, tc.wantClaim)
			}
		})
	}
}

// TestWithdrawIntent_RecordErrorRollsBack: a Record error is returned wrapped
// and the whole withdrawal rolls back, read as COMMITTED state: the entry is
// still the run's active held, yielded, intent-carrying row. A store that
// ignored Record's error would commit the drop: RED.
func TestWithdrawIntent_RecordErrorRollsBack(t *testing.T) {
	f := newFixture(t)
	r := f.run()
	before := f.yieldedIntent(r, "h1", 9)
	boom := errors.New("audit append failed")
	rec := &withdrawRecorder{err: boom}
	_, err := f.store.WithdrawIntent(context.Background(), WithdrawIntentRequest{RunID: r, Record: rec.record})
	// COMMITTED state first: a store that ignored Record's error commits the
	// drop, and this read turns RED on it.
	if after := f.entry(before.ID); !sameEntryState(before, after) || after.State != StateHeld || !after.Yielded() {
		t.Fatalf("entry after a failed Record = %+v, want the original held yielded row %+v", after, before)
	}
	if st := f.status(r); st.Entry.ID != before.ID {
		t.Fatalf("active entry after a failed Record = %s, want %s", st.Entry.ID, before.ID)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("Record calls = %d, want 1", len(rec.calls))
	}
	if !errors.Is(err, boom) {
		t.Fatalf("WithdrawIntent err = %v, want the Record error wrapped", err)
	}
}

// TestWithdrawIntent_InvalidAndUnknown: a nil run id and a nil Record are
// refused before any SQL (the entry read back unchanged); an unknown run is
// ErrNotFound, which also matches run.ErrNotFound.
func TestWithdrawIntent_InvalidAndUnknown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.run()
	before := f.yieldedIntent(r, "h1", 2)
	rec := &withdrawRecorder{}
	if _, err := f.store.WithdrawIntent(ctx, WithdrawIntentRequest{Record: rec.record}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("WithdrawIntent(nil run id) err = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.store.WithdrawIntent(ctx, WithdrawIntentRequest{RunID: r}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("WithdrawIntent(nil Record) err = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.store.WithdrawIntent(ctx, WithdrawIntentRequest{RunID: uuid.New(), Record: rec.record}); !errors.Is(err, ErrNotFound) || !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("WithdrawIntent(unknown run) err = %v, want ErrNotFound", err)
	}
	if !sameEntryState(before, f.entry(before.ID)) || len(rec.calls) != 0 {
		t.Fatalf("entry changed or Record called (%d) by a refused withdraw", len(rec.calls))
	}
}

// TestWithdrawIntent_SuppressesAutoReentry (approval condition 3): a
// withdrawn intent stays absent from the ADR-093 D4 re-entry read. Runs W and
// C have the same shape: an OLDER row ejected verify_failed with intent, and
// a newer re-entered row held YIELDED with intent. PRE-withdraw, W's newest
// row is yielded with intent, and neither run is a ReentryCandidates row
// (the held re-entry is newest). W is withdrawn; C is the control. Then both
// newest rows meet the event that makes a yielded entry a re-entry candidate
// — a red pass ejecting it verify_failed (Settle) — and the read is taken
// again: C's row IS a candidate (the path reaches candidacy, so the
// assertion below is not vacuous), while W's dropped row is terminal, so the
// eject settles nothing and no W row is ever a candidate. Skipping the
// withdraw's settle (CF1's store half) leaves W held, the eject lands, and W
// is listed: RED.
func TestWithdrawIntent_SuppressesAutoReentry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w, c := f.run(), f.run()
	f.seedEjected(w, StateEjected, ReentryReason, true, 2*time.Hour)
	f.seedEjected(c, StateEjected, ReentryReason, true, 2*time.Hour)
	wEntry := f.yieldedIntent(w, "hw", 5)
	cEntry := f.yieldedIntent(c, "hc", 6)
	candidates := func() map[uuid.UUID][]uuid.UUID {
		t.Helper()
		es, err := f.store.ReentryCandidates(ctx, nil, 100)
		if err != nil {
			t.Fatalf("ReentryCandidates: %v", err)
		}
		out := map[uuid.UUID][]uuid.UUID{}
		for _, e := range es {
			out[e.RunID] = append(out[e.RunID], e.ID)
		}
		return out
	}

	if st := f.status(w); st.Entry.ID != wEntry.ID || !st.Entry.Yielded() || st.Entry.MergeIntentAt == nil {
		t.Fatalf("pre-withdraw W = %+v, want its yielded intent entry active", st.Entry)
	}
	if got := candidates(); len(got[w]) != 0 || len(got[c]) != 0 {
		t.Fatalf("pre-withdraw re-entry candidates = %v, want neither run (each newest row is held)", got)
	}

	if _, err := f.store.WithdrawIntent(ctx, WithdrawIntentRequest{RunID: w, Record: (&withdrawRecorder{}).record}); err != nil {
		t.Fatalf("WithdrawIntent(W): %v", err)
	}

	for _, e := range []Entry{wEntry, cEntry} {
		if _, err := f.store.Settle(ctx, e.ID, StateEjected, ReentryReason); err != nil {
			t.Fatalf("red-pass eject of %s: %v", e.ID, err)
		}
	}
	got := candidates()
	if !slices.Equal(got[c], []uuid.UUID{cEntry.ID}) {
		t.Fatalf("control C candidates = %v, want its ejected row %s (the fixture path must reach candidacy)", got[c], cEntry.ID)
	}
	if len(got[w]) != 0 {
		t.Fatalf("withdrawn W candidates = %v, want none: a withdrawn intent is never re-entered", got[w])
	}
	if latest, _, err := f.store.LatestForRun(ctx, w); err != nil || latest.ID != wEntry.ID || latest.State != StateDropped || latest.EjectReason != ReasonIntentWithdrawn {
		t.Fatalf("W newest row = %+v (%v), want its withdrawn row still dropped %s", latest, err, ReasonIntentWithdrawn)
	}
}
