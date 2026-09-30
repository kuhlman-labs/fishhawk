package crewmessage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// seedMixedCorpus drives the live Mailbox through all three anchor kinds, all
// four states, an exhausted thread with its escalation, and a
// response_required consult with a deadline.
func seedMixedCorpus(t *testing.T, f *mbFixture) {
	t.Helper()
	run := f.send(f.runNotice(), nil, nil)
	issue := f.send(issueNotice(t, "x#1"), &f.account, nil)
	decision := f.send(notice(t, map[string]any{"decision_record_id": "ADR-081"}), &f.account, nil)
	consult := f.send(mutateFixture(t, "consult.json", func(m map[string]any) {
		m["anchor"] = map[string]any{"run_id": f.runID.String()}
	}), nil, nil)
	_ = consult // stays open
	reply := f.send(issueNotice(t, "x#1"), &f.account, &issue.SentSequence)
	extra := f.send(issueNotice(t, "x#1"), &f.account, &issue.SentSequence)
	for _, d := range []struct {
		seq  int64
		disp Disposition
	}{
		{run.SentSequence, DispositionAccepted},
		{decision.SentSequence, DispositionExpired},
		{issue.SentSequence, DispositionRejected},
		{reply.SentSequence, DispositionRejected},
	} {
		if _, err := f.dispose(d.seq, d.disp, "because"); err != nil {
			t.Fatalf("dispose %d: %v", d.seq, err)
		}
	}
	if _, err := f.dispose(extra.SentSequence, DispositionRejected, ""); !errors.Is(err, ErrRoundBoundExhausted) {
		t.Fatalf("third rejection err = %v, want ErrRoundBoundExhausted", err)
	}
}

// TestRebuild_AfterTruncateYieldsIdenticalRows is the rebuild-identity
// criterion over a mixed corpus.
func TestRebuild_AfterTruncateYieldsIdenticalRows(t *testing.T) {
	f := newMailboxFixture(t, 2)
	seedMixedCorpus(t, f)
	states := map[State]int{}
	for _, r := range f.rows() {
		states[r.State]++
	}
	if len(states) != 4 {
		t.Fatalf("corpus covers states %v, want all four", states)
	}
	stats := f.assertRebuildIdentity()
	if stats.EscalationsSeen != 1 || stats.RowsSkippedUndecodable != 0 || stats.DispositionsSkippedSuperseded != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

// TestRebuild_AfterInterleavedProjectionYieldsIdenticalRows repeats the
// identity check on the C1 interleaving fixture.
func TestRebuild_AfterInterleavedProjectionYieldsIdenticalRows(t *testing.T) {
	f := newMailboxFixture(t, 0)
	f.mb.afterChainAppend = func(ctx context.Context, sent *audit.Entry) {
		if _, err := f.mb.Dispose(ctx, DisposeParams{SentSequence: sent.Sequence, Disposition: DispositionRejected, Reason: "r"}); err != nil {
			t.Errorf("interleaved dispose: %v", err)
		}
	}
	f.send(f.runNotice(), nil, nil)
	f.send(issueNotice(t, "x#4"), &f.account, nil)
	for _, r := range f.rows() {
		if r.State != StateRejected {
			t.Errorf("row %d = %s, want rejected", r.SentSequence, r.State)
		}
	}
	f.assertRebuildIdentity()
}

// TestRebuild_WithoutTruncateIsIdempotent: replaying over a populated index
// changes nothing — every projection is at or behind its row.
func TestRebuild_WithoutTruncateIsIdempotent(t *testing.T) {
	f := newMailboxFixture(t, 2)
	seedMixedCorpus(t, f)
	before := f.rows()
	if _, err := Rebuild(context.Background(), f.pool, RebuildOptions{Logger: quietLogger}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if after := f.rows(); !reflect.DeepEqual(before, after) {
		t.Errorf("rows changed on a no-truncate replay:\n %+v\n %+v", before, after)
	}
}

// TestRebuild_FirstDispositionWins (approval condition 2): a hand-seeded chain
// carries TWO dispositions for one message (a pre-fix history the live path
// can no longer produce). Replay applies the FIRST in chain order, as the live
// pending-only update does.
func TestRebuild_FirstDispositionWins(t *testing.T) {
	f := newMailboxFixture(t, 0)
	sent := f.send(f.runNotice(), nil, nil)
	repo := audit.NewPostgresRepository(f.pool)
	var first *audit.Entry
	for i, d := range []Disposition{DispositionAccepted, DispositionRejected} {
		payload, _ := json.Marshal(disposedPayload{SentSequence: sent.SentSequence, ThreadRootSequence: sent.SentSequence, Disposition: d})
		e, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: f.runID, Timestamp: time.Now(), Category: CategoryDisposed, Payload: payload,
		})
		if err != nil {
			t.Fatalf("seed disposition %d: %v", i, err)
		}
		if i == 0 {
			first = e
		}
	}
	stats, err := Rebuild(context.Background(), f.pool, RebuildOptions{Truncate: true, Logger: quietLogger})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	got := mustGet(t, f.mb.Store(), sent.SentSequence)
	if got.State != StateAccepted || got.DispositionSequence == nil || *got.DispositionSequence != first.Sequence {
		t.Errorf("rebuilt row state/disposition = %s/%v, want accepted/%d (the FIRST disposition)", got.State, got.DispositionSequence, first.Sequence)
	}
	if stats.DispositionsSkippedSuperseded != 1 {
		t.Errorf("DispositionsSkippedSuperseded = %d, want 1", stats.DispositionsSkippedSuperseded)
	}
	// Live agrees: the chain already holds a disposition, so Dispose refuses.
	if _, err := f.dispose(sent.SentSequence, DispositionExpired, ""); !errors.Is(err, ErrAlreadyDisposed) {
		t.Errorf("live dispose err = %v, want ErrAlreadyDisposed", err)
	}
}

// TestRebuild_UndecodablePayloadCountedNotGuessed: malformed entries are
// counted and skipped; every other row is intact.
func TestRebuild_UndecodablePayloadCountedNotGuessed(t *testing.T) {
	f := newMailboxFixture(t, 0)
	good := f.send(f.runNotice(), nil, nil)
	repo := audit.NewPostgresRepository(f.pool)
	seed := func(category, payload string) {
		t.Helper()
		if _, err := repo.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: f.runID, Timestamp: time.Now(), Category: category, Payload: json.RawMessage(payload),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(CategorySent, `{"message":{"schema_version":"crew-message-v1"}}`)                               // schema-invalid message
	seed(CategorySent, `[1]`)                                                                            // payload not an object                                                    // wrong payload shape
	seed(CategoryDisposed, `{"sent_sequence":"x"}`)                                                      // wrong field type
	seed(CategoryDisposed, `{"sent_sequence":1,"disposition":"maybe"}`)                                  // out-of-set disposition
	seed(CategoryDisposed, `{"sent_sequence":999999,"thread_root_sequence":1,"disposition":"accepted"}`) // orphan
	before := f.rows()
	stats, err := Rebuild(context.Background(), f.pool, RebuildOptions{Truncate: true})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.RowsSkippedUndecodable != 4 || stats.DispositionsSkippedOrphan != 1 || stats.RowsUpserted != 1 || stats.EntriesRead != 6 {
		t.Errorf("stats = %+v, want 4 undecodable / 1 orphan / 1 upserted / 6 read", stats)
	}
	if after := f.rows(); !reflect.DeepEqual(before, after) || len(after) != 1 || after[0].SentSequence != good.SentSequence {
		t.Errorf("rows after rebuild = %+v, want only the good row, unchanged", after)
	}
}

// TestRebuild_FaultInjection reaches every Rebuild error return, and proves a
// failed truncating rebuild rolls back rather than leaving the index empty.
func TestRebuild_FaultInjection(t *testing.T) {
	f := newMailboxFixture(t, 0)
	sent := f.send(f.runNotice(), nil, nil)
	if _, err := f.dispose(sent.SentSequence, DispositionAccepted, ""); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	before := f.rows()
	scanErr := errors.New("scan fault")
	cases := []struct {
		name     string
		db       DBTX
		truncate bool
		want     error
	}{
		{"begin", &faultDB{inner: f.pool, failOp: "Begin", failAt: 1, err: errInjected}, true, errInjected},
		{"truncate", &txFaultDB{DBTX: f.pool, failOp: "Exec", failAt: 1}, true, errInjected},
		{"page query", &txFaultDB{DBTX: f.pool, failOp: "Query", failAt: 1}, true, errInjected},
		{"page scan", &txFaultDB{DBTX: f.pool, failOp: "Query", failAt: 1, rows: &faultRows{next: 1, scanErr: scanErr}}, true, scanErr},
		{"sent upsert", &txFaultDB{DBTX: f.pool, failOp: "Exec", failAt: 2}, true, errInjected},
		{"disposition projection", &txFaultDB{DBTX: f.pool, failOp: "QueryRow", failAt: 1}, true, errInjected},
	}
	for _, tc := range cases {
		_, err := Rebuild(context.Background(), tc.db, RebuildOptions{Truncate: tc.truncate, Logger: quietLogger})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want wrapping %v", tc.name, err, tc.want)
		}
		if after := f.rows(); !reflect.DeepEqual(before, after) {
			t.Errorf("%s: index changed by a failed rebuild (not rolled back): %+v", tc.name, after)
		}
	}
}
