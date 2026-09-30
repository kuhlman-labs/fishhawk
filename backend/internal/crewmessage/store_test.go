package crewmessage

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// storeT0 is a microsecond-aligned UTC instant: TIMESTAMPTZ stores
// microseconds, so a fixture time with nanoseconds would not round-trip.
var storeT0 = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func int64Ptr(v int64) *int64 { return &v }

func timePtr(v time.Time) *time.Time { return &v }

// openIssueRow is an OPEN, issue-anchored thread-root row at seq.
func openIssueRow(seq int64) Row {
	return Row{
		SentSequence:        seq,
		SentEntryHash:       fmt.Sprintf("hash-%d", seq),
		IssueRef:            "issue:3736",
		MessageType:         TypeNotice,
		SenderRole:          RoleReviewer,
		RecipientRole:       RoleCaptain,
		ThreadRootSequence:  seq,
		State:               StateOpen,
		SentAt:              storeT0.Add(time.Duration(seq) * time.Second),
		LastAppliedSequence: seq,
	}
}

// seedStoreRun inserts a runs row so a run-anchored crew_messages row can
// satisfy its foreign key.
func seedStoreRun(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, account *uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, account_id)
		 VALUES ($1, 'acme/widgets', 'feature_change', 'sha', 'cli', 'pending', 'local', $2)`, id, account,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

func mustUpsert(t *testing.T, s *Store, rows ...Row) {
	t.Helper()
	for _, r := range rows {
		if err := s.Upsert(context.Background(), r); err != nil {
			t.Fatalf("upsert %d: %v", r.SentSequence, err)
		}
	}
}

func mustGet(t *testing.T, s *Store, seq int64) Row {
	t.Helper()
	r, err := s.Get(context.Background(), seq)
	if err != nil {
		t.Fatalf("get %d: %v", seq, err)
	}
	return r
}

func sentSeqsOf(rows []Row) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SentSequence)
	}
	return out
}

// TestStore_UpsertGetRoundTrip writes one row with every optional column set
// (run anchor, account, deadline, disposition, reason, round, disposed_at) and
// one with none set, and reads both back field-for-field: a column written
// but not read (or scanned into the wrong field) fails the equality.
func TestStore_UpsertGetRoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := NewStore(pool)
	account, runID := uuid.New(), uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, account_key) VALUES ($1, 'cm-store-account')`, account); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	seedStoreRun(t, pool, runID, &account)

	full := Row{
		SentSequence:        41,
		SentEntryHash:       "hash-41",
		AccountID:           &account,
		RunID:               &runID,
		MessageType:         TypeConsult,
		SenderRole:          RolePlanner,
		RecipientRole:       RoleArchitect,
		ResponseRequired:    true,
		Deadline:            timePtr(storeT0.Add(time.Hour)),
		ThreadRootSequence:  40,
		State:               StateRejected,
		DispositionSequence: int64Ptr(44),
		ReasonSequence:      int64Ptr(44),
		Round:               2,
		SentAt:              storeT0,
		DisposedAt:          timePtr(storeT0.Add(time.Minute)),
		LastAppliedSequence: 44,
	}
	bare := openIssueRow(50)
	bare.IssueRef = ""
	bare.DecisionRecordID = "dr-7"
	mustUpsert(t, s, full, bare)

	for _, want := range []Row{full, bare} {
		if got := mustGet(t, s, want.SentSequence); !reflect.DeepEqual(got, want) {
			t.Errorf("round trip of %d:\n got  %+v\n want %+v", want.SentSequence, got, want)
		}
	}
}

func TestStore_Get_MissingIsErrMessageNotFound(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	if _, err := s.Get(context.Background(), 999); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("Get(missing) err = %v, want ErrMessageNotFound", err)
	}
}

// TestStore_Upsert_MonotonicGuardRefusesStaleProjection is control C1, the
// ON CONFLICT ... WHERE last_applied_sequence < EXCLUDED.last_applied_sequence
// clause.
//
// MECHANISM: the row is seeded ALREADY terminal at a HIGHER
// last_applied_sequence (accepted, 20), then a send projection for the SAME
// sent_sequence carrying state open and the strictly lower sequence (10) is
// applied — the delayed-send-projection interleaving. Absent the guard the row
// regresses to open. The control's effect is committed state, so the test
// READS the row back. A same-sequence replay carrying a different value is the
// strict-< arm (a replay must be a no-op), and a strictly newer projection
// must still land (the guard is not a blanket refusal).
func TestStore_Upsert_MonotonicGuardRefusesStaleProjection(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	ctx := context.Background()

	terminal := openIssueRow(10)
	terminal.State = StateAccepted
	terminal.DispositionSequence = int64Ptr(20)
	terminal.DisposedAt = timePtr(storeT0.Add(time.Hour))
	terminal.LastAppliedSequence = 20
	mustUpsert(t, s, terminal)

	// The delayed send projection: must be a no-op, and not an error.
	if err := s.Upsert(ctx, openIssueRow(10)); err != nil {
		t.Fatalf("stale upsert: %v", err)
	}
	if got := mustGet(t, s, 10); got.State != StateAccepted || got.LastAppliedSequence != 20 {
		t.Errorf("after stale send projection: state=%q last_applied=%d, want accepted/20 (row regressed)", got.State, got.LastAppliedSequence)
	}

	// Same-sequence replay with a different value: still a no-op.
	replay := terminal
	replay.State = StateRejected
	mustUpsert(t, s, replay)
	if got := mustGet(t, s, 10); got.State != StateAccepted {
		t.Errorf("after same-sequence replay: state=%q, want accepted (replay must be a no-op)", got.State)
	}

	// A strictly newer projection lands.
	newer := terminal
	newer.State = StateExpired
	newer.LastAppliedSequence = 30
	mustUpsert(t, s, newer)
	if got := mustGet(t, s, 10); got.State != StateExpired || got.LastAppliedSequence != 30 {
		t.Errorf("after newer projection: state=%q last_applied=%d, want expired/30", got.State, got.LastAppliedSequence)
	}
}

// TestStore_Transition_AppliesAndRefusesSecond is control C2, the
// pending-only AND state = 'open' clause.
//
// MECHANISM: the message is disposed accepted FIRST, so the row is already
// terminal; the second transition carries a strictly NEWER disposition
// sequence (so the last_applied arm cannot mask the deletion) and the
// pending-only clause is the only thing that can refuse it. The test asserts
// BOTH the sentinel AND that the committed row still reads the first
// disposition.
func TestStore_Transition_AppliesAndRefusesSecond(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	ctx := context.Background()
	mustUpsert(t, s, openIssueRow(10))

	first := Transition{SentSequence: 10, State: StateAccepted, DispositionSequence: 20,
		ReasonSequence: int64Ptr(19), Round: 0, DisposedAt: storeT0.Add(time.Hour)}
	if err := s.Transition(ctx, first); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	got := mustGet(t, s, 10)
	want := openIssueRow(10)
	want.State = StateAccepted
	want.DispositionSequence = int64Ptr(20)
	want.ReasonSequence = int64Ptr(19)
	want.DisposedAt = timePtr(storeT0.Add(time.Hour))
	want.LastAppliedSequence = 20
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after first transition:\n got  %+v\n want %+v", got, want)
	}

	second := Transition{SentSequence: 10, State: StateRejected, DispositionSequence: 30,
		Round: 1, DisposedAt: storeT0.Add(2 * time.Hour)}
	if err := s.Transition(ctx, second); !errors.Is(err, ErrAlreadyDisposed) {
		t.Errorf("second transition err = %v, want ErrAlreadyDisposed", err)
	}
	if got := mustGet(t, s, 10); !reflect.DeepEqual(got, want) {
		t.Errorf("after refused second transition the row changed:\n got  %+v\n want %+v", got, want)
	}
}

// TestStore_Transition_RoundColumnPinned asserts the round a rejection closes
// is what the row carries — the column the per-thread bound's readers use.
func TestStore_Transition_RoundColumnPinned(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	ctx := context.Background()
	root := openIssueRow(10)
	reply1, reply2 := openIssueRow(11), openIssueRow(12)
	reply1.ThreadRootSequence, reply2.ThreadRootSequence = 10, 10
	mustUpsert(t, s, root, reply1, reply2)
	for i, seq := range []int64{10, 11, 12} {
		if err := s.Transition(ctx, Transition{SentSequence: seq, State: StateRejected,
			DispositionSequence: 100 + seq, Round: i + 1, DisposedAt: storeT0}); err != nil {
			t.Fatalf("reject %d: %v", seq, err)
		}
	}
	for i, seq := range []int64{10, 11, 12} {
		if got := mustGet(t, s, seq); got.Round != i+1 {
			t.Errorf("row %d round = %d, want %d", seq, got.Round, i+1)
		}
	}
	if n, err := s.CountThreadRejections(ctx, 10); err != nil || n != 3 {
		t.Errorf("CountThreadRejections(10) = %d, %v; want 3, nil", n, err)
	}
}

// TestStore_Transition_NonTerminalTargetRefused: a transition whose target is
// not terminal (open, empty, out of set) is refused with ErrInvalidDisposition
// and the committed row is untouched. With the Terminal check deleted, the
// target open satisfies the pending-only UPDATE, which then stamps a
// disposition sequence onto a still-open row — so the test READS the row.
func TestStore_Transition_NonTerminalTargetRefused(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	ctx := context.Background()
	mustUpsert(t, s, openIssueRow(10))
	for _, target := range []State{StateOpen, "", "closed"} {
		err := s.Transition(ctx, Transition{SentSequence: 10, State: target, DispositionSequence: 20, DisposedAt: storeT0})
		if !errors.Is(err, ErrInvalidDisposition) {
			t.Errorf("Transition(target %q) err = %v, want ErrInvalidDisposition", target, err)
		}
		if got := mustGet(t, s, 10); !reflect.DeepEqual(got, openIssueRow(10)) {
			t.Errorf("Transition(target %q) changed the row: %+v", target, got)
		}
	}
}

// TestStore_Transition_NotNewerThanRowRefused: the last_applied_sequence arm.
// MECHANISM: an OPEN row whose last_applied_sequence (50) is ABOVE the
// transition's disposition sequence (40), so state = 'open' passes and the
// monotonic arm is the only refusal. The row must stay open.
func TestStore_Transition_NotNewerThanRowRefused(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	ctx := context.Background()
	mustUpsert(t, s, openIssueRow(50))
	err := s.Transition(ctx, Transition{SentSequence: 50, State: StateAccepted, DispositionSequence: 40, DisposedAt: storeT0})
	if !errors.Is(err, ErrAlreadyDisposed) {
		t.Errorf("stale transition err = %v, want ErrAlreadyDisposed", err)
	}
	if got := mustGet(t, s, 50); !reflect.DeepEqual(got, openIssueRow(50)) {
		t.Errorf("stale transition changed the row: %+v", got)
	}
}

func TestStore_Transition_MissingRowIsErrMessageNotFound(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	err := s.Transition(context.Background(), Transition{SentSequence: 77, State: StateAccepted, DispositionSequence: 78, DisposedAt: storeT0})
	if !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("Transition(missing) err = %v, want ErrMessageNotFound", err)
	}
}

// TestStore_Transition_ConcurrentExactlyOneWins races 8 transitions of one
// open row, each with a DISTINCT disposition sequence, so the last_applied arm
// alone would admit every transition newer than the current winner: only the
// pending-only clause makes the count exactly one.
func TestStore_Transition_ConcurrentExactlyOneWins(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	ctx := context.Background()
	mustUpsert(t, s, openIssueRow(10))

	const n = 8
	states := []State{StateAccepted, StateRejected, StateExpired}
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = s.Transition(ctx, Transition{SentSequence: 10, State: states[i%len(states)],
				DispositionSequence: int64(100 + i), DisposedAt: storeT0})
		}(i)
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Errorf("transitions %d and %d both succeeded, want exactly one", winner, i)
			}
			winner = i
		case !errors.Is(err, ErrAlreadyDisposed):
			t.Errorf("transition %d err = %v, want nil or ErrAlreadyDisposed", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no transition succeeded, want exactly one")
	}
	got := mustGet(t, s, 10)
	if got.DispositionSequence == nil || *got.DispositionSequence != int64(100+winner) || got.State != states[winner%len(states)] {
		t.Errorf("committed row = %+v, want the winner's (#%d) disposition", got, winner)
	}
}

func TestStore_ListByRecipient(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	ctx := context.Background()
	a, b, c, d := openIssueRow(1), openIssueRow(2), openIssueRow(3), openIssueRow(4)
	b.RecipientRole = RoleArchitect
	c.State = StateRejected
	c.LastAppliedSequence = 9
	mustUpsert(t, s, d, c, b, a) // out of order: the read orders by sequence

	cases := []struct {
		name string
		f    ListFilter
		want []int64
	}{
		{"all", ListFilter{}, []int64{1, 2, 3, 4}},
		{"captain", ListFilter{RecipientRole: RoleCaptain}, []int64{1, 3, 4}},
		{"captain open", ListFilter{RecipientRole: RoleCaptain, State: StateOpen}, []int64{1, 4}},
		{"rejected", ListFilter{State: StateRejected}, []int64{3}},
		{"limit", ListFilter{RecipientRole: RoleCaptain, Limit: 2}, []int64{1, 3}},
		{"none", ListFilter{RecipientRole: RoleHistorian}, []int64{}},
	}
	for _, tc := range cases {
		got, err := s.ListByRecipient(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if seqs := sentSeqsOf(got); !reflect.DeepEqual(seqs, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, seqs, tc.want)
		}
	}
}

// TestStore_ListByAnchor pins the EXACT anchor match: each anchor kind reads
// only its own rows, and a filter naming no anchor matches nothing rather than
// everything.
func TestStore_ListByAnchor(t *testing.T) {
	pool := pgtest.NewPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	runA, runB := uuid.New(), uuid.New()
	seedStoreRun(t, pool, runA, nil)
	seedStoreRun(t, pool, runB, nil)

	onRun := func(seq int64, run uuid.UUID) Row {
		r := openIssueRow(seq)
		r.IssueRef = ""
		r.RunID = &run
		return r
	}
	onDecision := func(seq int64, id string) Row {
		r := openIssueRow(seq)
		r.IssueRef = ""
		r.DecisionRecordID = id
		return r
	}
	onIssue := func(seq int64, ref string) Row {
		r := openIssueRow(seq)
		r.IssueRef = ref
		return r
	}
	mustUpsert(t, s, onRun(1, runA), onRun(2, runB), onRun(3, runA),
		onIssue(4, "issue:1"), onIssue(5, "issue:2"), onDecision(6, "dr-1"), onDecision(7, "dr-2"))

	cases := []struct {
		name string
		f    AnchorFilter
		want []int64
	}{
		{"run A", AnchorFilter{RunID: &runA}, []int64{1, 3}},
		{"run B", AnchorFilter{RunID: &runB}, []int64{2}},
		{"issue 1", AnchorFilter{IssueRef: "issue:1"}, []int64{4}},
		{"decision 2", AnchorFilter{DecisionRecordID: "dr-2"}, []int64{7}},
		{"no anchor", AnchorFilter{}, []int64{}},
	}
	for _, tc := range cases {
		got, err := s.ListByAnchor(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if seqs := sentSeqsOf(got); !reflect.DeepEqual(seqs, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, seqs, tc.want)
		}
	}
}

// TestStore_CountThreadRejections counts only REJECTED rows on the named
// thread: an accepted and an open row on the same thread, and a rejected row
// on another thread, are all excluded.
func TestStore_CountThreadRejections(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	ctx := context.Background()
	mk := func(seq, root int64, st State) Row {
		r := openIssueRow(seq)
		r.ThreadRootSequence = root
		r.State = st
		r.LastAppliedSequence = seq + 100
		return r
	}
	mustUpsert(t, s, mk(1, 1, StateRejected), mk(2, 1, StateRejected), mk(3, 1, StateAccepted),
		mk(4, 1, StateOpen), mk(5, 5, StateRejected))
	for root, want := range map[int64]int{1: 2, 5: 1, 99: 0} {
		if n, err := s.CountThreadRejections(ctx, root); err != nil || n != want {
			t.Errorf("CountThreadRejections(%d) = %d, %v; want %d, nil", root, n, err, want)
		}
	}
}

func TestStore_Truncate(t *testing.T) {
	s := NewStore(pgtest.NewPool(t))
	ctx := context.Background()
	mustUpsert(t, s, openIssueRow(1), openIssueRow(2))
	if err := s.Truncate(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	got, err := s.ListByRecipient(ctx, ListFilter{})
	if err != nil || len(got) != 0 {
		t.Errorf("after truncate ListByRecipient = %v, %v; want empty", sentSeqsOf(got), err)
	}
}

// ---- fault injection ----

// faultDB wraps a DBTX and fails the failAt-th call (1-based) of the method
// named failOp with err — or, for a Query, hands back rows instead when rows is
// set, so a scan / rows.Err failure is reachable. Every other call delegates
// to inner. The counters are mutex-guarded: a store or mailbox path may call
// it from several goroutines.
type faultDB struct {
	inner  DBTX
	failOp string
	failAt int
	err    error
	rows   pgx.Rows

	mu    sync.Mutex
	calls map[string]int
}

var errInjected = errors.New("injected fault")

func (f *faultDB) hit(op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[op]++
	return op == f.failOp && f.calls[op] == f.failAt
}

func (f *faultDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.hit("Exec") || f.inner == nil {
		return pgconn.CommandTag{}, f.err
	}
	return f.inner.Exec(ctx, sql, args...)
}

func (f *faultDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.hit("Query") || f.inner == nil {
		if f.rows != nil {
			return f.rows, nil
		}
		return nil, f.err
	}
	return f.inner.Query(ctx, sql, args...)
}

func (f *faultDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.hit("QueryRow") || f.inner == nil {
		return faultRow{err: f.err}
	}
	return f.inner.QueryRow(ctx, sql, args...)
}

func (f *faultDB) Begin(ctx context.Context) (pgx.Tx, error) {
	if f.hit("Begin") || f.inner == nil {
		return nil, f.err
	}
	return f.inner.Begin(ctx)
}

type faultRow struct{ err error }

func (r faultRow) Scan(...any) error { return r.err }

// faultRows yields `next` rows whose Scan fails with scanErr, then reports
// iterErr from Err. The embedded nil pgx.Rows supplies any method the
// collectors never call (it panics if one is reached).
type faultRows struct {
	pgx.Rows
	next    int
	scanErr error
	iterErr error
}

func (r *faultRows) Close()            {}
func (r *faultRows) Err() error        { return r.iterErr }
func (r *faultRows) Scan(...any) error { return r.scanErr }
func (r *faultRows) Next() bool {
	if r.next <= 0 {
		return false
	}
	r.next--
	return true
}

// TestStore_FaultInjection reaches every store error return and asserts the
// injected error surfaces wrapped (errors.Is), never swallowed or replaced by
// a sentinel.
func TestStore_FaultInjection(t *testing.T) {
	ctx := context.Background()
	scanErr := errors.New("scan fault")
	iterErr := errors.New("iteration fault")
	cases := []struct {
		name string
		db   *faultDB
		call func(*Store) error
		want error
	}{
		{"upsert exec", &faultDB{failOp: "Exec", failAt: 1, err: errInjected},
			func(s *Store) error { return s.Upsert(ctx, openIssueRow(1)) }, errInjected},
		{"transition scan", &faultDB{failOp: "QueryRow", failAt: 1, err: errInjected},
			func(s *Store) error {
				return s.Transition(ctx, Transition{SentSequence: 1, State: StateAccepted, DispositionSequence: 2})
			}, errInjected},
		{"get query", &faultDB{failOp: "Query", failAt: 1, err: errInjected},
			func(s *Store) error { _, err := s.Get(ctx, 1); return err }, errInjected},
		{"get scan", &faultDB{failOp: "Query", failAt: 1, rows: &faultRows{next: 1, scanErr: scanErr}},
			func(s *Store) error { _, err := s.Get(ctx, 1); return err }, scanErr},
		{"list by recipient query", &faultDB{failOp: "Query", failAt: 1, err: errInjected},
			func(s *Store) error { _, err := s.ListByRecipient(ctx, ListFilter{Limit: 1}); return err }, errInjected},
		{"list by recipient scan", &faultDB{failOp: "Query", failAt: 1, rows: &faultRows{next: 1, scanErr: scanErr}},
			func(s *Store) error { _, err := s.ListByRecipient(ctx, ListFilter{}); return err }, scanErr},
		{"list by recipient rows.Err", &faultDB{failOp: "Query", failAt: 1, rows: &faultRows{iterErr: iterErr}},
			func(s *Store) error { _, err := s.ListByRecipient(ctx, ListFilter{}); return err }, iterErr},
		{"list by anchor query", &faultDB{failOp: "Query", failAt: 1, err: errInjected},
			func(s *Store) error { _, err := s.ListByAnchor(ctx, AnchorFilter{IssueRef: "i"}); return err }, errInjected},
		{"list by anchor scan", &faultDB{failOp: "Query", failAt: 1, rows: &faultRows{next: 1, scanErr: scanErr}},
			func(s *Store) error { _, err := s.ListByAnchor(ctx, AnchorFilter{IssueRef: "i"}); return err }, scanErr},
		{"count scan", &faultDB{failOp: "QueryRow", failAt: 1, err: errInjected},
			func(s *Store) error { _, err := s.CountThreadRejections(ctx, 1); return err }, errInjected},
		{"truncate exec", &faultDB{failOp: "Exec", failAt: 1, err: errInjected},
			func(s *Store) error { return s.Truncate(ctx) }, errInjected},
	}
	for _, tc := range cases {
		err := tc.call(NewStore(tc.db))
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want wrapping %v", tc.name, err, tc.want)
		}
		if errors.Is(err, ErrMessageNotFound) || errors.Is(err, ErrAlreadyDisposed) {
			t.Errorf("%s: err = %v, a fault must not surface as a state-machine sentinel", tc.name, err)
		}
	}
}

// TestStore_ClosedPool is the closed-pool arm: a real pool closed before use
// fails every call with a wrapped error rather than panicking or reporting a
// sentinel.
func TestStore_ClosedPool(t *testing.T) {
	pool := pgtest.NewPool(t)
	pool.Close()
	s := NewStore(pool)
	ctx := context.Background()
	if err := s.Upsert(ctx, openIssueRow(1)); err == nil {
		t.Error("Upsert on a closed pool = nil, want an error")
	}
	if _, err := s.Get(ctx, 1); err == nil || errors.Is(err, ErrMessageNotFound) {
		t.Errorf("Get on a closed pool = %v, want a non-sentinel error", err)
	}
}
