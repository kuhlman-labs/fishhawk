package crewmessage

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// mbFixture is a real pgtest database with one run (in account) and a Mailbox
// over the pool.
type mbFixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	mb      *Mailbox
	runID   uuid.UUID
	account uuid.UUID
}

func newMailboxFixture(t *testing.T, bound int) *mbFixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &mbFixture{t: t, pool: pool, mb: NewMailbox(pool, bound), runID: uuid.New(), account: uuid.New()}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, f.account, "cm-mailbox-"+f.account.String()); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	seedStoreRun(t, pool, f.runID, &f.account)
	return f
}

// notice builds a valid notice (the notice.json fixture) on the given anchor.
func notice(t *testing.T, anchor map[string]any) []byte {
	t.Helper()
	return mutateFixture(t, "notice.json", func(m map[string]any) { m["anchor"] = anchor })
}

func (f *mbFixture) runNotice() []byte {
	return notice(f.t, map[string]any{"run_id": f.runID.String()})
}

func issueNotice(t *testing.T, ref string) []byte {
	return notice(t, map[string]any{"issue_ref": ref})
}

// sentShapedPayload is a VALID crew_message_sent payload. Seeded under a
// different category it isolates the category check: decoding alone would
// accept it.
func sentShapedPayload(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	p, err := json.Marshal(sentPayload{Message: raw})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return p
}

var mbActor = Actor{Kind: audit.ActorAgent, Subject: "reviewer"}

func (f *mbFixture) send(raw []byte, account *uuid.UUID, root *int64) Row {
	f.t.Helper()
	r, err := f.mb.Send(context.Background(), SendParams{RawMessage: raw, Actor: mbActor, AccountID: account, ThreadRootSequence: root})
	if err != nil {
		f.t.Fatalf("send: %v", err)
	}
	return *r
}

func (f *mbFixture) dispose(seq int64, d Disposition, reason string) (*Row, error) {
	return f.mb.Dispose(context.Background(), DisposeParams{SentSequence: seq, Disposition: d, Reason: reason, Actor: mbActor})
}

// chainCount counts committed chain entries of category, optionally narrowed
// to payload->>key = val.
func (f *mbFixture) chainCount(category, key string, val int64) int {
	f.t.Helper()
	q := `SELECT count(*) FROM audit_entries WHERE category = $1`
	args := []any{category}
	if key != "" {
		q += ` AND payload->>'` + key + `' = $2`
		args = append(args, strconv.FormatInt(val, 10))
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		f.t.Fatalf("chain count: %v", err)
	}
	return n
}

func (f *mbFixture) allChainEntries() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_entries`).Scan(&n); err != nil {
		f.t.Fatalf("count: %v", err)
	}
	return n
}

func (f *mbFixture) rows() []Row {
	f.t.Helper()
	rs, err := f.mb.Store().ListByRecipient(context.Background(), ListFilter{})
	if err != nil {
		f.t.Fatalf("list: %v", err)
	}
	return rs
}

// assertRebuildIdentity captures every row, truncates, rebuilds, and requires
// full-Row equality field by field.
func (f *mbFixture) assertRebuildIdentity() RebuildStats {
	f.t.Helper()
	before := f.rows()
	stats, err := Rebuild(context.Background(), f.pool, RebuildOptions{Truncate: true, PageSize: 2})
	if err != nil {
		f.t.Fatalf("rebuild: %v", err)
	}
	after := f.rows()
	if len(before) != len(after) {
		f.t.Fatalf("rebuild row count = %d, want %d", len(after), len(before))
	}
	for i := range before {
		if !reflect.DeepEqual(before[i], after[i]) {
			f.t.Errorf("rebuilt row %d differs:\n live    %+v\n rebuilt %+v", before[i].SentSequence, before[i], after[i])
		}
	}
	return stats
}

// TestMailbox_SendDisposeRebuild_EndToEnd crosses every layer: Validate, the
// real audit chain (run AND global partitions), the derived table, the reads,
// truncate and Rebuild.
func TestMailbox_SendDisposeRebuild_EndToEnd(t *testing.T) {
	f := newMailboxFixture(t, 0)
	ctx := context.Background()
	if f.mb.roundBound != DefaultRoundBound {
		t.Errorf("roundBound = %d, want DefaultRoundBound", f.mb.roundBound)
	}

	runRow := f.send(f.runNotice(), nil, nil)
	issueRow := f.send(issueNotice(t, "kuhlman-labs/fishhawk#3736"), &f.account, nil)
	decision := f.send(notice(t, map[string]any{"decision_record_id": "ADR-081"}), &f.account, nil)
	consult := mutateFixture(t, "consult.json", func(m map[string]any) {
		m["anchor"] = map[string]any{"run_id": f.runID.String()}
		m["deadline"] = "2026-09-29t17:30:00.5z" // RFC 3339 lowercase form
	})
	consultRow := f.send(consult, nil, nil)
	reply := f.send(f.runNotice(), nil, &runRow.SentSequence)

	if runRow.RunID == nil || *runRow.RunID != f.runID || runRow.AccountID == nil || *runRow.AccountID != f.account {
		t.Errorf("run row anchor/account = %v/%v, want %s/%s", runRow.RunID, runRow.AccountID, f.runID, f.account)
	}
	if issueRow.IssueRef != "kuhlman-labs/fishhawk#3736" || issueRow.RunID != nil || decision.DecisionRecordID != "ADR-081" {
		t.Errorf("run-less anchors not projected: %+v / %+v", issueRow, decision)
	}
	if consultRow.Deadline == nil || !consultRow.Deadline.Equal(time.Date(2026, 9, 29, 17, 30, 0, 5e8, time.UTC)) || !consultRow.ResponseRequired {
		t.Errorf("consult deadline/response_required = %v/%v", consultRow.Deadline, consultRow.ResponseRequired)
	}
	if reply.ThreadRootSequence != runRow.SentSequence || runRow.ThreadRootSequence != runRow.SentSequence {
		t.Errorf("thread roots = %d/%d, want %d", runRow.ThreadRootSequence, reply.ThreadRootSequence, runRow.SentSequence)
	}
	if got := mustGet(t, f.mb.Store(), runRow.SentSequence); !reflect.DeepEqual(got, runRow) {
		t.Errorf("stored row != returned row:\n %+v\n %+v", got, runRow)
	}

	acc, err := f.dispose(runRow.SentSequence, DispositionAccepted, "")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if acc.State != StateAccepted || acc.ReasonSequence != nil || acc.Round != 0 {
		t.Errorf("accepted row = %+v", acc)
	}
	rej, err := f.dispose(issueRow.SentSequence, DispositionRejected, "not this way")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rej.State != StateRejected || rej.ReasonSequence == nil || *rej.ReasonSequence != *rej.DispositionSequence || rej.Round != 1 {
		t.Errorf("rejected row = %+v", rej)
	}
	if _, err := f.dispose(decision.SentSequence, DispositionExpired, ""); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if got := mustGet(t, f.mb.Store(), rej.SentSequence); !reflect.DeepEqual(got, *rej) {
		t.Errorf("stored disposed row != returned:\n %+v\n %+v", got, *rej)
	}

	open, err := f.mb.Store().ListByRecipient(ctx, ListFilter{RecipientRole: RolePlanner, State: StateOpen})
	if err != nil || !reflect.DeepEqual(sentSeqsOf(open), []int64{reply.SentSequence}) {
		t.Errorf("open planner rows = %v (%v), want [%d]", sentSeqsOf(open), err, reply.SentSequence)
	}
	onRun, err := f.mb.Store().ListByAnchor(ctx, AnchorFilter{RunID: &f.runID})
	if err != nil || len(onRun) != 3 {
		t.Errorf("rows on run = %v (%v), want 3", sentSeqsOf(onRun), err)
	}
	onIssue, err := f.mb.Store().ListByAnchor(ctx, AnchorFilter{IssueRef: "kuhlman-labs/fishhawk#3736"})
	if err != nil || len(onIssue) != 1 || onIssue[0].State != StateRejected {
		t.Errorf("rows on issue = %+v (%v)", onIssue, err)
	}

	stats := f.assertRebuildIdentity()
	if stats.EntriesRead != 8 || stats.RowsUpserted != 8 || stats.RowsSkippedUndecodable != 0 {
		t.Errorf("rebuild stats = %+v, want 8 read / 8 applied", stats)
	}
}

// TestMailbox_Send_DelayedProjectionCannotRegressTerminalRow (C1, end to
// end): a FULL Dispose runs between Send's chain append and Send's
// projection. The send projection then lands late, and the monotonic guard
// must make it a no-op.
func TestMailbox_Send_DelayedProjectionCannotRegressTerminalRow(t *testing.T) {
	f := newMailboxFixture(t, 0)
	var disposeErr error
	f.mb.afterChainAppend = func(ctx context.Context, sent *audit.Entry) {
		_, disposeErr = f.mb.Dispose(ctx, DisposeParams{SentSequence: sent.Sequence, Disposition: DispositionAccepted})
	}
	sent := f.send(issueNotice(t, "x#1"), &f.account, nil)
	if disposeErr != nil {
		t.Fatalf("interleaved dispose: %v", disposeErr)
	}
	got := mustGet(t, f.mb.Store(), sent.SentSequence)
	if got.State != StateAccepted || got.DispositionSequence == nil {
		t.Fatalf("row after delayed send projection = %+v, want accepted (regressed to open)", got)
	}
	f.mb.afterChainAppend = nil
	f.assertRebuildIdentity()
}

// TestMailbox_Dispose_SecondDispositionRefused: the message is disposed
// first, so the refuse-before-append check is the only gate on the second.
func TestMailbox_Dispose_SecondDispositionRefused(t *testing.T) {
	f := newMailboxFixture(t, 0)
	sent := f.send(f.runNotice(), nil, nil)
	if _, err := f.dispose(sent.SentSequence, DispositionAccepted, ""); err != nil {
		t.Fatalf("first dispose: %v", err)
	}
	_, err := f.dispose(sent.SentSequence, DispositionRejected, "changed my mind")
	if !errors.Is(err, ErrAlreadyDisposed) {
		t.Errorf("second dispose err = %v, want ErrAlreadyDisposed", err)
	}
	if n := f.chainCount(CategoryDisposed, "sent_sequence", sent.SentSequence); n != 1 {
		t.Errorf("disposed entries on the chain = %d, want 1 (the refusal must append nothing)", n)
	}
	if got := mustGet(t, f.mb.Store(), sent.SentSequence); got.State != StateAccepted {
		t.Errorf("row state = %s, want accepted", got.State)
	}
}

// TestMailbox_Dispose_ConcurrentExactlyOneWins (approval condition 1): 8
// racing dispositions, exactly one nil, seven ErrAlreadyDisposed, and exactly
// ONE crew_message_disposed entry on the committed chain.
func TestMailbox_Dispose_ConcurrentExactlyOneWins(t *testing.T) {
	f := newMailboxFixture(t, 0)
	sent := f.send(f.runNotice(), nil, nil)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.dispose(sent.SentSequence, DispositionAccepted, "")
		}(i)
	}
	wg.Wait()
	wins, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrAlreadyDisposed):
			refused++
		default:
			t.Errorf("unexpected err: %v", err)
		}
	}
	if wins != 1 || refused != n-1 {
		t.Errorf("wins/refused = %d/%d, want 1/%d", wins, refused, n-1)
	}
	if c := f.chainCount(CategoryDisposed, "sent_sequence", sent.SentSequence); c != 1 {
		t.Errorf("disposed entries on the chain = %d, want exactly 1", c)
	}
	f.assertRebuildIdentity()
}

// TestMailbox_Dispose_UnknownDispositionRefusedBeforeAppend (C3): the only
// defect is the disposition value.
func TestMailbox_Dispose_UnknownDispositionRefusedBeforeAppend(t *testing.T) {
	f := newMailboxFixture(t, 0)
	sent := f.send(f.runNotice(), nil, nil)
	before := f.allChainEntries()
	_, err := f.dispose(sent.SentSequence, Disposition("maybe"), "")
	if !errors.Is(err, ErrInvalidDisposition) {
		t.Errorf("err = %v, want ErrInvalidDisposition", err)
	}
	if after := f.allChainEntries(); after != before {
		t.Errorf("chain entries %d -> %d, want unchanged", before, after)
	}
	if got := mustGet(t, f.mb.Store(), sent.SentSequence); got.State != StateOpen {
		t.Errorf("row state = %s, want open", got.State)
	}
}

// TestMailbox_Dispose_UnknownSentSequenceRefusedBeforeAppend (C4): one case
// names a sequence that EXISTS on the chain but is not a crew_message_sent
// entry, so a bare existence check would pass it; one names nothing.
func TestMailbox_Dispose_UnknownSentSequenceRefusedBeforeAppend(t *testing.T) {
	f := newMailboxFixture(t, 0)
	other, err := audit.NewPostgresRepository(f.pool).AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now(), Category: "run_started", Payload: sentShapedPayload(t, f.runNotice()),
	})
	if err != nil {
		t.Fatalf("seed unrelated entry: %v", err)
	}
	undecodable, err := audit.NewPostgresRepository(f.pool).AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now(), Category: CategorySent, Payload: json.RawMessage(`{"message":{"type":"notice"}}`),
	})
	if err != nil {
		t.Fatalf("seed undecodable sent entry: %v", err)
	}
	for name, seq := range map[string]int64{
		"other category":    other.Sequence,
		"nonexistent":       other.Sequence + 1000,
		"undecodable sent":  undecodable.Sequence,
		"negative sequence": -1,
	} {
		before := f.allChainEntries()
		_, err := f.dispose(seq, DispositionAccepted, "")
		if !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("%s: err = %v, want ErrMessageNotFound", name, err)
		}
		if after := f.allChainEntries(); after != before {
			t.Errorf("%s: chain entries %d -> %d, want unchanged", name, before, after)
		}
	}
}

// TestMailbox_Reject_RoundBoundRefusesAndEscalates (C5 + approval condition
// 5): with bound 2 the thread already holds two rejections, so the third is
// refused and ONE escalation is recorded instead. The round column is pinned
// live and after rebuild.
func TestMailbox_Reject_RoundBoundRefusesAndEscalates(t *testing.T) {
	f := newMailboxFixture(t, 2)
	root := f.send(issueNotice(t, "x#9"), &f.account, nil)
	r1 := f.send(issueNotice(t, "x#9"), &f.account, &root.SentSequence)
	r2 := f.send(issueNotice(t, "x#9"), &f.account, &root.SentSequence)
	for _, s := range []int64{root.SentSequence, r1.SentSequence} {
		if _, err := f.dispose(s, DispositionRejected, "no"); err != nil {
			t.Fatalf("reject %d: %v", s, err)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		_, err := f.dispose(r2.SentSequence, DispositionRejected, "still no")
		if !errors.Is(err, ErrRoundBoundExhausted) {
			t.Fatalf("attempt %d: err = %v, want ErrRoundBoundExhausted", attempt, err)
		}
	}
	if n := f.chainCount(CategoryEscalated, "thread_root_sequence", root.SentSequence); n != 1 {
		t.Errorf("escalation entries = %d, want exactly 1 (a later refusal does not re-escalate)", n)
	}
	if n := f.chainCount(CategoryDisposed, "thread_root_sequence", root.SentSequence); n != 2 {
		t.Errorf("disposed entries in the thread = %d, want 2 (no third rejection)", n)
	}
	check := func(when string) {
		t.Helper()
		want := map[int64]struct {
			state State
			round int
		}{root.SentSequence: {StateRejected, 1}, r1.SentSequence: {StateRejected, 2}, r2.SentSequence: {StateOpen, 0}}
		for seq, w := range want {
			got := mustGet(t, f.mb.Store(), seq)
			if got.State != w.state || got.Round != w.round {
				t.Errorf("%s: row %d state/round = %s/%d, want %s/%d", when, seq, got.State, got.Round, w.state, w.round)
			}
		}
	}
	check("live")
	stats := f.assertRebuildIdentity()
	check("after rebuild")
	if stats.EscalationsSeen != 1 {
		t.Errorf("EscalationsSeen = %d, want 1", stats.EscalationsSeen)
	}
	// An acceptance is not a rejection: it is not bounded.
	if _, err := f.dispose(r2.SentSequence, DispositionAccepted, ""); err != nil {
		t.Errorf("accept after exhaustion: %v", err)
	}
}

// TestMailbox_Reject_BoundIsPerThreadNotPerRun is the isolation arm: a bound
// built on audit.AppendChainedUnderBudget's per-(run, category) scope fails it.
func TestMailbox_Reject_BoundIsPerThreadNotPerRun(t *testing.T) {
	f := newMailboxFixture(t, 1)
	a := f.send(f.runNotice(), nil, nil)
	a1 := f.send(f.runNotice(), nil, &a.SentSequence)
	if _, err := f.dispose(a.SentSequence, DispositionRejected, ""); err != nil {
		t.Fatalf("reject a: %v", err)
	}
	if _, err := f.dispose(a1.SentSequence, DispositionRejected, ""); !errors.Is(err, ErrRoundBoundExhausted) {
		t.Fatalf("thread A second rejection err = %v, want ErrRoundBoundExhausted", err)
	}
	b := f.send(f.runNotice(), nil, nil)
	if _, err := f.dispose(b.SentSequence, DispositionRejected, ""); err != nil {
		t.Errorf("second thread on the SAME run: first rejection err = %v, want nil", err)
	}
	c := f.send(issueNotice(t, "x#2"), &f.account, nil)
	if _, err := f.dispose(c.SentSequence, DispositionRejected, ""); err != nil {
		t.Errorf("run-less thread: first rejection err = %v, want nil", err)
	}
}

// TestMailbox_Reject_ConcurrentRoundsRespectBound: bound+4 concurrent
// rejections on one thread, exactly bound succeed — the lock-then-count
// ordering. Run on a run anchor AND a run-less anchor, whose chain helper
// takes its own advisory lock after ours (a lock inversion would hang).
func TestMailbox_Reject_ConcurrentRoundsRespectBound(t *testing.T) {
	const bound = 2
	for _, runless := range []bool{false, true} {
		t.Run("runless="+strconv.FormatBool(runless), func(t *testing.T) {
			f := newMailboxFixture(t, bound)
			msg := f.runNotice()
			var acct *uuid.UUID
			if runless {
				msg, acct = issueNotice(t, "x#7"), &f.account
			}
			root := f.send(msg, acct, nil)
			seqs := []int64{root.SentSequence}
			for i := 0; i < bound+3; i++ {
				seqs = append(seqs, f.send(msg, acct, &root.SentSequence).SentSequence)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			errs := make([]error, len(seqs))
			var wg sync.WaitGroup
			for i, s := range seqs {
				wg.Add(1)
				go func(i int, s int64) {
					defer wg.Done()
					_, errs[i] = f.mb.Dispose(ctx, DisposeParams{SentSequence: s, Disposition: DispositionRejected})
				}(i, s)
			}
			wg.Wait()
			ok, exhausted := 0, 0
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, ErrRoundBoundExhausted):
					exhausted++
				default:
					t.Errorf("unexpected err: %v", err)
				}
			}
			if ok != bound || exhausted != len(seqs)-bound {
				t.Errorf("succeeded/exhausted = %d/%d, want %d/%d", ok, exhausted, bound, len(seqs)-bound)
			}
			if n := f.chainCount(CategoryDisposed, "thread_root_sequence", root.SentSequence); n != bound {
				t.Errorf("rejections on the chain = %d, want %d", n, bound)
			}
			if n := f.chainCount(CategoryEscalated, "thread_root_sequence", root.SentSequence); n != 1 {
				t.Errorf("escalations = %d, want 1", n)
			}
		})
	}
}

// TestMailbox_Reject_BoundCountedFromChainNotTable: truncating the derived
// table on an exhausted thread must not reset the bound.
func TestMailbox_Reject_BoundCountedFromChainNotTable(t *testing.T) {
	f := newMailboxFixture(t, 1)
	root := f.send(f.runNotice(), nil, nil)
	reply := f.send(f.runNotice(), nil, &root.SentSequence)
	if _, err := f.dispose(root.SentSequence, DispositionRejected, ""); err != nil {
		t.Fatalf("reject root: %v", err)
	}
	if err := f.mb.Store().Truncate(context.Background()); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := f.dispose(reply.SentSequence, DispositionRejected, ""); !errors.Is(err, ErrRoundBoundExhausted) {
		t.Errorf("rejection after truncate err = %v, want ErrRoundBoundExhausted", err)
	}
}

// TestMailbox_Send_RejectsImplementRecipient (C7): the schema admits
// implementer, so Validate's invariant-#8 rule is the only gate.
func TestMailbox_Send_RejectsImplementRecipient(t *testing.T) {
	f := newMailboxFixture(t, 0)
	raw := mutateFixture(t, "notice.json", func(m map[string]any) {
		m["anchor"] = map[string]any{"run_id": f.runID.String()}
		m["recipient_role"] = string(RoleImplementer)
	})
	_, err := f.mb.Send(context.Background(), SendParams{RawMessage: raw})
	if !errors.Is(err, ErrRecipientNotAddressable) {
		t.Errorf("err = %v, want ErrRecipientNotAddressable", err)
	}
	if n := f.chainCount(CategorySent, "", 0); n != 0 {
		t.Errorf("sent entries = %d, want 0", n)
	}
	if rs := f.rows(); len(rs) != 0 {
		t.Errorf("rows = %d, want 0", len(rs))
	}
}

// TestMailbox_Send_ThreadMembership (approval condition 3): each refusal is
// named and appends nothing.
func TestMailbox_Send_ThreadMembership(t *testing.T) {
	f := newMailboxFixture(t, 0)
	root := f.send(issueNotice(t, "x#5"), &f.account, nil)
	reply := f.send(issueNotice(t, "x#5"), &f.account, &root.SentSequence)
	unrelated, err := audit.NewPostgresRepository(f.pool).AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now(), Category: "run_started", Payload: sentShapedPayload(t, issueNotice(t, "x#5")), AccountID: &f.account,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	undecodable, err := audit.NewPostgresRepository(f.pool).AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now(), Category: CategorySent, Payload: json.RawMessage(`{"message":"nope"}`), AccountID: &f.account,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	other := uuid.New()
	cases := []struct {
		name    string
		raw     []byte
		account *uuid.UUID
		root    int64
		want    error
	}{
		{"nonexistent root", issueNotice(t, "x#5"), &f.account, root.SentSequence + 1000, ErrThreadRootNotFound},
		{"root is another category", issueNotice(t, "x#5"), &f.account, unrelated.Sequence, ErrThreadRootNotFound},
		{"root undecodable", issueNotice(t, "x#5"), &f.account, undecodable.Sequence, ErrThreadRootNotFound},
		{"root is a reply", issueNotice(t, "x#5"), &f.account, reply.SentSequence, ErrThreadRootNotRoot},
		{"cross account", issueNotice(t, "x#5"), &other, root.SentSequence, ErrThreadCrossAccount},
		{"untenanted vs tenanted", issueNotice(t, "x#5"), nil, root.SentSequence, ErrThreadCrossAccount},
		{"anchor mismatch", issueNotice(t, "x#6"), &f.account, root.SentSequence, ErrThreadAnchorMismatch},
		{"run reply to run-less root", f.runNotice(), nil, root.SentSequence, ErrThreadAnchorMismatch},
	}
	for _, tc := range cases {
		before := f.allChainEntries()
		seq := tc.root
		_, err := f.mb.Send(context.Background(), SendParams{RawMessage: tc.raw, AccountID: tc.account, ThreadRootSequence: &seq})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if after := f.allChainEntries(); after != before {
			t.Errorf("%s: chain entries %d -> %d, want unchanged", tc.name, before, after)
		}
	}
}

// ---- fault injection ----

// faultTx wraps a real pgx.Tx and fails the failAt-th Exec / QueryRow / Query
// (by failOp) inside the transaction, so the in-transaction error returns are
// reachable.
type faultTx struct {
	pgx.Tx
	failOp string
	failAt int
	rows   pgx.Rows
	calls  map[string]int
}

func (t *faultTx) hit(op string) bool {
	t.calls[op]++
	return op == t.failOp && t.calls[op] == t.failAt
}

func (t *faultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if t.hit("Exec") {
		return pgconn.CommandTag{}, errInjected
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func (t *faultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if t.hit("QueryRow") {
		return faultRow{err: errInjected}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

func (t *faultTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if t.hit("Query") {
		if t.rows != nil {
			return t.rows, nil
		}
		return nil, errInjected
	}
	return t.Tx.Query(ctx, sql, args...)
}

// txFaultDB hands out faultTx-wrapped transactions over a real pool.
type txFaultDB struct {
	DBTX
	failOp string
	failAt int
	rows   pgx.Rows
}

func (d *txFaultDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.DBTX.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, failOp: d.failOp, failAt: d.failAt, rows: d.rows, calls: map[string]int{}}, nil
}

// withDB returns a copy of f's mailbox over db (store included).
func (f *mbFixture) withDB(db DBTX) *Mailbox {
	m := *f.mb
	m.db = db
	m.store = NewStore(db)
	return &m
}

// TestMailbox_FaultInjection reaches every mailbox error return. A fault
// never surfaces as a state-machine sentinel, and a fault BEFORE the append
// leaves the chain unchanged.
func TestMailbox_FaultInjection(t *testing.T) {
	f := newMailboxFixture(t, 1)
	ctx := context.Background()
	open := f.send(f.runNotice(), nil, nil)
	root := f.send(f.runNotice(), nil, nil)
	exhaust := f.send(f.runNotice(), nil, &root.SentSequence)
	if _, err := f.dispose(root.SentSequence, DispositionRejected, ""); err != nil {
		t.Fatalf("reject root: %v", err)
	}

	disposeOpen := func(m *Mailbox, d Disposition) error {
		_, err := m.Dispose(ctx, DisposeParams{SentSequence: open.SentSequence, Disposition: d})
		return err
	}
	cases := []struct {
		name string
		call func() error
	}{
		{"send begin", func() error {
			_, err := f.withDB(&faultDB{inner: f.pool, failOp: "Begin", failAt: 1, err: errInjected}).
				Send(ctx, SendParams{RawMessage: f.runNotice()})
			return err
		}},
		{"send thread-root read", func() error {
			seq := root.SentSequence
			_, err := f.withDB(&faultDB{inner: f.pool, failOp: "QueryRow", failAt: 1, err: errInjected}).
				Send(ctx, SendParams{RawMessage: f.runNotice(), ThreadRootSequence: &seq})
			return err
		}},
		{"send run append", func() error {
			m := f.withDB(f.pool)
			m.appendRun = func(context.Context, pgx.Tx, audit.ChainAppendParams) (*audit.Entry, error) { return nil, errInjected }
			_, err := m.Send(ctx, SendParams{RawMessage: f.runNotice()})
			return err
		}},
		{"send global append", func() error {
			m := f.withDB(f.pool)
			m.appendGlobal = func(context.Context, pgx.Tx, audit.GlobalChainAppendParams) (*audit.Entry, error) {
				return nil, errInjected
			}
			_, err := m.Send(ctx, SendParams{RawMessage: issueNotice(t, "x#3"), AccountID: &f.account})
			return err
		}},
		{"dispose sent read", func() error {
			return disposeOpen(f.withDB(&faultDB{inner: f.pool, failOp: "QueryRow", failAt: 1, err: errInjected}), DispositionAccepted)
		}},
		{"dispose begin", func() error {
			return disposeOpen(f.withDB(&faultDB{inner: f.pool, failOp: "Begin", failAt: 1, err: errInjected}), DispositionAccepted)
		}},
		{"dispose thread lock", func() error {
			return disposeOpen(f.withDB(&txFaultDB{DBTX: f.pool, failOp: "Exec", failAt: 1}), DispositionAccepted)
		}},
		{"dispose disposition read", func() error {
			return disposeOpen(f.withDB(&txFaultDB{DBTX: f.pool, failOp: "QueryRow", failAt: 1}), DispositionAccepted)
		}},
		{"dispose rejection count", func() error {
			return disposeOpen(f.withDB(&txFaultDB{DBTX: f.pool, failOp: "QueryRow", failAt: 2}), DispositionRejected)
		}},
		{"dispose append", func() error {
			m := f.withDB(f.pool)
			m.appendRun = func(context.Context, pgx.Tx, audit.ChainAppendParams) (*audit.Entry, error) { return nil, errInjected }
			return disposeOpen(m, DispositionAccepted)
		}},
		{"escalation read", func() error {
			_, err := f.withDB(&txFaultDB{DBTX: f.pool, failOp: "QueryRow", failAt: 3}).
				Dispose(ctx, DisposeParams{SentSequence: exhaust.SentSequence, Disposition: DispositionRejected})
			return err
		}},
		{"escalation append", func() error {
			m := f.withDB(f.pool)
			m.appendRun = func(context.Context, pgx.Tx, audit.ChainAppendParams) (*audit.Entry, error) { return nil, errInjected }
			_, err := m.Dispose(ctx, DisposeParams{SentSequence: exhaust.SentSequence, Disposition: DispositionRejected})
			return err
		}},
	}
	for _, tc := range cases {
		before := f.allChainEntries()
		rowsBefore := len(f.rows())
		err := tc.call()
		if !errors.Is(err, errInjected) {
			t.Errorf("%s: err = %v, want wrapping the injected fault", tc.name, err)
		}
		var pe *ProjectionError
		if errors.As(err, &pe) {
			t.Errorf("%s: a pre-commit fault reported as a ProjectionError", tc.name)
		}
		for _, s := range []error{ErrMessageNotFound, ErrAlreadyDisposed, ErrRoundBoundExhausted, ErrInvalidDisposition} {
			if errors.Is(err, s) {
				t.Errorf("%s: fault surfaced as sentinel %v", tc.name, s)
			}
		}
		if after := f.allChainEntries(); after != before {
			t.Errorf("%s: chain entries %d -> %d, want unchanged", tc.name, before, after)
		}
		if n := len(f.rows()); n != rowsBefore {
			t.Errorf("%s: rows %d -> %d, want unchanged (no projection without a committed entry)", tc.name, rowsBefore, n)
		}
	}
}

// TestMailbox_ProjectionAfterCommitFailure: the chain entry committed, the
// projection failed. The operation happened — the built row is returned with
// a *ProjectionError — and Rebuild closes the gap.
func TestMailbox_ProjectionAfterCommitFailure(t *testing.T) {
	f := newMailboxFixture(t, 0)
	ctx := context.Background()

	// Send: the first pool-level Exec is the projection upsert.
	row, err := f.withDB(&faultDB{inner: f.pool, failOp: "Exec", failAt: 1, err: errInjected}).
		Send(ctx, SendParams{RawMessage: f.runNotice()})
	var pe *ProjectionError
	if !errors.As(err, &pe) || !errors.Is(err, errInjected) || row == nil || pe.Sequence != row.SentSequence || pe.Error() == "" {
		t.Fatalf("send projection failure = %v / %+v, want a ProjectionError carrying the committed row", err, row)
	}
	if n := f.chainCount(CategorySent, "", 0); n != 1 {
		t.Errorf("sent entries = %d, want 1 (the chain is authoritative)", n)
	}
	if _, err := f.mb.Store().Get(ctx, row.SentSequence); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("row projected despite the fault: %v", err)
	}

	// Dispose, Transition fails (second pool-level QueryRow).
	disp, err := f.withDB(&faultDB{inner: f.pool, failOp: "QueryRow", failAt: 2, err: errInjected}).
		Dispose(ctx, DisposeParams{SentSequence: row.SentSequence, Disposition: DispositionAccepted})
	if !errors.As(err, &pe) || disp == nil || disp.State != StateAccepted {
		t.Fatalf("dispose transition failure = %v / %+v, want a ProjectionError carrying the terminal row", err, disp)
	}

	// Dispose with no send row (never projected), fallback upsert fails.
	second := f.send(f.runNotice(), nil, nil)
	if err := f.mb.Store().Truncate(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, err = f.withDB(&faultDB{inner: f.pool, failOp: "Exec", failAt: 1, err: errInjected}).
		Dispose(ctx, DisposeParams{SentSequence: second.SentSequence, Disposition: DispositionAccepted})
	if !errors.As(err, &pe) || !errors.Is(err, errInjected) {
		t.Fatalf("dispose fallback-upsert failure = %v, want a ProjectionError", err)
	}

	// Rebuild closes every gap.
	if _, err := Rebuild(ctx, f.pool, RebuildOptions{Truncate: true}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	for _, s := range []int64{row.SentSequence, second.SentSequence} {
		if got := mustGet(t, f.mb.Store(), s); got.State != StateAccepted {
			t.Errorf("row %d after rebuild = %s, want accepted", s, got.State)
		}
	}
}

// TestMailbox_Dispose_FallbackUpsertWhenSendNotProjected: a Dispose whose
// send projection never landed upserts the whole terminal row, identical to
// what Rebuild produces.
func TestMailbox_Dispose_FallbackUpsertWhenSendNotProjected(t *testing.T) {
	f := newMailboxFixture(t, 0)
	sent := f.send(f.runNotice(), nil, nil)
	if err := f.mb.Store().Truncate(context.Background()); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	disp, err := f.dispose(sent.SentSequence, DispositionExpired, "")
	if err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if got := mustGet(t, f.mb.Store(), sent.SentSequence); !reflect.DeepEqual(got, *disp) {
		t.Errorf("fallback row:\n %+v\nwant\n %+v", got, *disp)
	}
	f.assertRebuildIdentity()
}

// TestProjectSentRow_Defensive covers the builder's refusals a Parse-validated
// message cannot reach (the schema asserts the uuid and date-time formats).
func TestProjectSentRow_Defensive(t *testing.T) {
	if _, err := projectSentRow(nil, &Message{}, nil); !errors.Is(err, ErrNilEntry) {
		t.Errorf("nil entry err = %v, want ErrNilEntry", err)
	}
	e := &audit.Entry{Sequence: 1}
	if _, err := projectSentRow(e, &Message{Anchor: Anchor{RunID: "not-a-uuid"}}, nil); err == nil {
		t.Error("bad run_id accepted")
	}
	if _, err := projectSentRow(e, &Message{Anchor: Anchor{IssueRef: "i"}, Deadline: "tomorrow"}, nil); err == nil {
		t.Error("bad deadline accepted")
	}
	if !sameAccount(nil, nil) || sameAccount(nil, &uuid.UUID{}) || sameAccount(&uuid.UUID{}, nil) {
		t.Error("sameAccount nil handling wrong")
	}
	if (Actor{}).kindPtr() != nil || (Actor{}).subjectPtr() != nil {
		t.Error("empty actor must record NULL kind and subject")
	}
}

// TestMailbox_Send_StampsStageID pins E77.5's SendParams.StageID threading: a
// run-anchored send carrying a StageID stamps the crew_message_sent entry's
// stage_id (what makes the per-stage consult budget countable from the
// chain), one without leaves it NULL, and the stamped entry's hash still
// re-verifies from its stored columns — stage_id is a hash input the chain
// already carries, so stamping it is additive.
func TestMailbox_Send_StampsStageID(t *testing.T) {
	f := newMailboxFixture(t, 0)
	ctx := context.Background()
	stageID := uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		VALUES ($1, $2, 1, 'plan', 'agent', 'claude-code', 'running')`, stageID, f.runID); err != nil {
		t.Fatalf("seed stage: %v", err)
	}
	stamped, err := f.mb.Send(ctx, SendParams{RawMessage: f.runNotice(), Actor: mbActor, StageID: &stageID})
	if err != nil {
		t.Fatalf("send with stage: %v", err)
	}
	bare := f.send(f.runNotice(), nil, nil)

	type stored struct {
		runID, stageID *uuid.UUID
		ts             time.Time
		category       string
		actorKind      *audit.ActorKind
		actorSubject   *string
		payload        []byte
		prevHash       *string
		entryHash      string
	}
	read := func(seq int64) stored {
		t.Helper()
		var s stored
		if err := f.pool.QueryRow(ctx, `SELECT run_id, stage_id, ts, category, actor_kind, actor_subject, payload, prev_hash, entry_hash
			FROM audit_entries WHERE sequence = $1`, seq).Scan(&s.runID, &s.stageID, &s.ts, &s.category,
			&s.actorKind, &s.actorSubject, &s.payload, &s.prevHash, &s.entryHash); err != nil {
			t.Fatalf("read entry %d: %v", seq, err)
		}
		return s
	}
	got := read(stamped.SentSequence)
	if got.stageID == nil || *got.stageID != stageID {
		t.Fatalf("stamped entry stage_id = %v, want %s", got.stageID, stageID)
	}
	if b := read(bare.SentSequence); b.stageID != nil {
		t.Fatalf("send without StageID stamped stage_id = %s, want NULL", *b.stageID)
	}
	h, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID: got.runID, StageID: got.stageID, Timestamp: got.ts, Category: got.category,
		ActorKind: got.actorKind, ActorSubject: got.actorSubject, Payload: got.payload, PrevHash: got.prevHash,
	})
	if err != nil {
		t.Fatalf("recompute hash: %v", err)
	}
	if h != got.entryHash {
		t.Fatalf("stamped entry hash does not re-verify: recomputed %s, stored %s", h, got.entryHash)
	}
}
