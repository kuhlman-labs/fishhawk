package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// grooming_window_test.go drives the #2991 capture/apply concurrency protocol
// against REAL Postgres: a fake repository cannot exercise the run-row lock, the
// in-transaction watermark scan, and the whole-batch rollback the protocol lives
// on. So the seam is real — pgtest Postgres, the production run + audit repos.

type gwFixture struct {
	audit audit.Repository
	win   audit.GroomingWindowAppender
	runID uuid.UUID
	artA  string
	artB  string
}

func newGWFixture(t *testing.T) *gwFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "backlog_grooming", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	win, ok := auditRepo.(audit.GroomingWindowAppender)
	if !ok {
		t.Fatal("the production audit repository must implement audit.GroomingWindowAppender")
	}
	return &gwFixture{
		audit: auditRepo, win: win, runID: r.ID,
		artA: uuid.NewString(), artB: uuid.NewString(),
	}
}

// dispositionParams builds a valid disposition ChainAppendParams for artifactID.
func (f *gwFixture) dispositionParams(artifactID, entryID, verdict string) audit.ChainAppendParams {
	payload, _ := json.Marshal(map[string]any{
		"run_id": f.runID.String(), "artifact_id": artifactID,
		"entry_id": entryID, "verdict": verdict,
	})
	return audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now().UTC(),
		Category: audit.GroomingDispositionRecordedCategory, Payload: payload,
	}
}

// watermarkParams builds a settlement watermark ChainAppendParams for artifactID.
func (f *gwFixture) watermarkParams(artifactID, settlement string) audit.ChainAppendParams {
	payload, _ := json.Marshal(map[string]any{
		"run_id": f.runID.String(), "artifact_id": artifactID, "settlement": settlement,
	})
	return audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now().UTC(),
		Category: audit.GroomingApplyWindowClosedCategory, Payload: payload,
	}
}

func (f *gwFixture) dispositionRows(t *testing.T) []*audit.Entry {
	t.Helper()
	rows, err := f.audit.ListForRunByCategory(context.Background(), f.runID, audit.GroomingDispositionRecordedCategory)
	if err != nil {
		t.Fatalf("list disposition rows: %v", err)
	}
	return rows
}

func (f *gwFixture) watermarkRows(t *testing.T) []*audit.Entry {
	t.Helper()
	rows, err := f.audit.ListForRunByCategory(context.Background(), f.runID, audit.GroomingApplyWindowClosedCategory)
	if err != nil {
		t.Fatalf("list watermark rows: %v", err)
	}
	return rows
}

// TestGroomingWindow_BatchRefusedAtClosedWindow: the batch path refuses at a
// PRE-EXISTING watermark (seeded by construction) and writes NOTHING. Reads
// committed state after the call — an error-identity assertion alone would be
// byte-identical to a fired-then-rolled-back control.
//
// COUNTERFACTUAL (watermark scan in AppendChainedGroomingDispositionBatchTx):
// delete the scan and the batch appends the rows, so the zero-rows assertion
// reddens.
func TestGroomingWindow_BatchRefusedAtClosedWindow(t *testing.T) {
	f := newGWFixture(t)
	// Seed the watermark DIRECTLY (by construction), not via the settlement core.
	if _, err := f.audit.AppendChained(context.Background(), f.watermarkParams(f.artA, "approved")); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	_, err := f.win.AppendChainedGroomingDispositionBatch(context.Background(), f.artA,
		[]audit.ChainAppendParams{
			f.dispositionParams(f.artA, "ordering:a", "approved"),
			f.dispositionParams(f.artA, "ordering:b", "approved"),
		})
	var closed *audit.GroomingWindowClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("err = %v (%T), want *audit.GroomingWindowClosedError", err, err)
	}
	if closed.ArtifactID != f.artA || closed.Settlement != "approved" {
		t.Errorf("closed = %+v, want artifact %s settlement approved", closed, f.artA)
	}
	if rows := f.dispositionRows(t); len(rows) != 0 {
		t.Errorf("committed disposition rows = %d, want 0 — a closed window records nothing", len(rows))
	}
}

// TestGroomingWindow_MidBatchFailureLeavesNothingCommitted: a mid-batch failure
// (an invalid-JSON payload that fails inside AppendChainedTx AFTER a valid
// append) rolls the WHOLE batch back. Every param uses the run's OWN id so the
// preflight cannot reject the batch first (the issue's process note). Reads
// committed state after the call.
//
// COUNTERFACTUAL (one-transaction batch): a per-row implementation would leave
// the first row durable; the zero-rows assertion reddens.
func TestGroomingWindow_MidBatchFailureLeavesNothingCommitted(t *testing.T) {
	f := newGWFixture(t)
	bad := f.dispositionParams(f.artA, "ordering:b", "approved")
	bad.Payload = []byte("this is not json") // fails ComputeEntryHash inside the tx

	_, err := f.win.AppendChainedGroomingDispositionBatch(context.Background(), f.artA,
		[]audit.ChainAppendParams{
			f.dispositionParams(f.artA, "ordering:a", "approved"), // valid, appended first
			bad, // fails mid-batch
		})
	if err == nil {
		t.Fatal("batch with an invalid mid-batch payload returned nil error")
	}
	if rows := f.dispositionRows(t); len(rows) != 0 {
		t.Errorf("committed disposition rows = %d, want 0 — a mid-batch failure rolls the WHOLE batch back", len(rows))
	}
}

// TestGroomingWindow_SettlementAppendsOneWatermark: the first settlement appends
// exactly one watermark and returns the consumed dispositions.
func TestGroomingWindow_SettlementAppendsOneWatermark(t *testing.T) {
	f := newGWFixture(t)
	if _, err := f.win.AppendChainedGroomingDispositionBatch(context.Background(), f.artA,
		[]audit.ChainAppendParams{
			f.dispositionParams(f.artA, "ordering:a", "approved"),
			f.dispositionParams(f.artA, "duplicate:x", "rejected"),
		}); err != nil {
		t.Fatalf("capture: %v", err)
	}

	wm, consumed, err := f.win.AppendChainedGroomingWindowClose(context.Background(), f.watermarkParams(f.artA, "approved"), f.artA)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if wm == nil {
		t.Fatal("settlement returned no watermark")
	}
	if len(consumed) != 2 {
		t.Errorf("consumed = %d, want 2", len(consumed))
	}
	if rows := f.watermarkRows(t); len(rows) != 1 {
		t.Errorf("watermark rows = %d, want 1", len(rows))
	}
}

// TestGroomingWindow_RepeatedSettlementDoesNotReopen: a second settlement returns
// the EXISTING watermark unchanged (permanence) and appends nothing, and a
// disposition recorded BETWEEN the two settlements is still refused.
//
// COUNTERFACTUAL (permanence branch in AppendChainedGroomingWindowCloseTx):
// delete it and the second settlement appends a second watermark; the exactly-one
// assertion reddens.
func TestGroomingWindow_RepeatedSettlementDoesNotReopen(t *testing.T) {
	f := newGWFixture(t)
	wm1, _, err := f.win.AppendChainedGroomingWindowClose(context.Background(), f.watermarkParams(f.artA, "approved"), f.artA)
	if err != nil {
		t.Fatalf("first settle: %v", err)
	}

	// A capture recorded AFTER the window closed is refused.
	_, err = f.win.AppendChainedGroomingDispositionBatch(context.Background(), f.artA,
		[]audit.ChainAppendParams{f.dispositionParams(f.artA, "ordering:a", "approved")})
	var closed *audit.GroomingWindowClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("post-settlement capture err = %v, want GroomingWindowClosedError", err)
	}

	wm2, _, err := f.win.AppendChainedGroomingWindowClose(context.Background(), f.watermarkParams(f.artA, "rejected"), f.artA)
	if err != nil {
		t.Fatalf("second settle: %v", err)
	}
	if wm2.Sequence != wm1.Sequence {
		t.Errorf("second settlement sequence = %d, want the FIRST watermark's %d (permanence)", wm2.Sequence, wm1.Sequence)
	}
	if rows := f.watermarkRows(t); len(rows) != 1 {
		t.Errorf("watermark rows = %d, want exactly 1 (a repeat settlement appends nothing)", len(rows))
	}
}

// TestGroomingWindow_ConcurrentCaptureAndSettlementSerialize runs one 3-entry
// capture concurrently with one settlement and asserts one of the two legal
// interleavings over committed state — never a partial, AND (the consumed half of
// the invariant) that a capture that WON was actually swept into the settlement's
// consumed set rather than committed-but-inert (an off-by-one on the below-
// watermark bound). Both goroutines are always in flight and contend on the same
// run-row lock; a small alternating stagger only decides which acquires it first,
// so BOTH arms — and thus the consumed-set assertion — are exercised every run
// rather than at the mercy of the scheduler (a pure race lands capture-first only
// ~1 round in 40 here, which would leave the consumed half all but untested).
func TestGroomingWindow_ConcurrentCaptureAndSettlementSerialize(t *testing.T) {
	const rounds = 12
	const (
		idA = "ordering:a"
		idB = "ordering:b"
		idC = "ordering:c"
	)
	stagger := timescale.D(3 * time.Millisecond)
	for i := 0; i < rounds; i++ {
		f := newGWFixture(t)
		captureFirst := i%2 == 0
		var wg sync.WaitGroup
		var captureErr, settleErr error
		var consumed []*audit.Entry
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if !captureFirst {
				time.Sleep(stagger)
			}
			_, captureErr = f.win.AppendChainedGroomingDispositionBatch(context.Background(), f.artA,
				[]audit.ChainAppendParams{
					f.dispositionParams(f.artA, idA, "approved"),
					f.dispositionParams(f.artA, idB, "approved"),
					f.dispositionParams(f.artA, idC, "approved"),
				})
		}()
		go func() {
			defer wg.Done()
			<-start
			if captureFirst {
				time.Sleep(stagger)
			}
			// Keep the settlement's returned consumed set: the never-partial half of
			// the invariant is the row count, but the consumed half — that a landed
			// capture is actually SWEPT INTO the settlement, not left inert — is only
			// observable here.
			_, consumed, settleErr = f.win.AppendChainedGroomingWindowClose(context.Background(), f.watermarkParams(f.artA, "approved"), f.artA)
		}()
		close(start)
		wg.Wait()
		if settleErr != nil {
			t.Fatalf("round %d: settle: %v", i, settleErr)
		}

		rows := f.dispositionRows(t)
		var closed *audit.GroomingWindowClosedError
		switch {
		case captureErr == nil:
			// Capture landed first: all three rows present AND all three appear in
			// the settlement's consumed set.
			if len(rows) != 3 {
				t.Errorf("round %d: capture succeeded but %d rows committed, want 3 — never a partial", i, len(rows))
			}
			got := groomingEntryIDSet(t, consumed)
			for _, id := range []string{idA, idB, idC} {
				if !got[id] {
					t.Errorf("round %d: capture landed but entry %q is absent from the consumed set %v — its committed row is inert", i, id, got)
				}
			}
		case errors.As(captureErr, &closed):
			// Settlement landed first: capture refused, ZERO rows, and the
			// settlement consumed nothing.
			if len(rows) != 0 {
				t.Errorf("round %d: capture refused but %d rows committed, want 0 — never a partial", i, len(rows))
			}
			if len(consumed) != 0 {
				t.Errorf("round %d: settlement landed first but consumed %d dispositions, want 0", i, len(consumed))
			}
		default:
			t.Errorf("round %d: capture err = %v, want nil or GroomingWindowClosedError", i, captureErr)
		}
	}
}

// groomingEntryIDSet decodes the entry_id of each consumed disposition into a set.
func groomingEntryIDSet(t *testing.T, entries []*audit.Entry) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, e := range entries {
		var p struct {
			EntryID string `json:"entry_id"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode consumed payload: %v", err)
		}
		out[p.EntryID] = true
	}
	return out
}

// TestGroomingWindow_TwoArtifacts is the CONDITION 1 pin: dispositions captured
// against artifact A must not appear in artifact B's consumed set, and settling B
// must not close A's window.
func TestGroomingWindow_TwoArtifacts(t *testing.T) {
	f := newGWFixture(t)
	// Capture against A and against B — same run, distinct artifacts.
	if _, err := f.win.AppendChainedGroomingDispositionBatch(context.Background(), f.artA,
		[]audit.ChainAppendParams{f.dispositionParams(f.artA, "ordering:a", "approved")}); err != nil {
		t.Fatalf("capture A: %v", err)
	}
	if _, err := f.win.AppendChainedGroomingDispositionBatch(context.Background(), f.artB,
		[]audit.ChainAppendParams{f.dispositionParams(f.artB, "ordering:b", "approved")}); err != nil {
		t.Fatalf("capture B: %v", err)
	}

	// Settling B consumes ONLY B's disposition.
	_, consumedB, err := f.win.AppendChainedGroomingWindowClose(context.Background(), f.watermarkParams(f.artB, "approved"), f.artB)
	if err != nil {
		t.Fatalf("settle B: %v", err)
	}
	if len(consumedB) != 1 {
		t.Fatalf("B consumed %d, want 1 (B's own disposition only)", len(consumedB))
	}
	if got := groomingArtifactOf(t, consumedB[0]); got != f.artB {
		t.Errorf("B consumed a disposition for artifact %s, want B (%s) — A's disposition leaked", got, f.artB)
	}

	// Settling B did NOT close A's window: a capture for A is still accepted.
	if _, err := f.win.AppendChainedGroomingDispositionBatch(context.Background(), f.artA,
		[]audit.ChainAppendParams{f.dispositionParams(f.artA, "ordering:a2", "approved")}); err != nil {
		t.Errorf("capture for A after settling B was refused (%v); settling B must not close A's window", err)
	}

	// And settling A consumes ONLY A's dispositions.
	_, consumedA, err := f.win.AppendChainedGroomingWindowClose(context.Background(), f.watermarkParams(f.artA, "approved"), f.artA)
	if err != nil {
		t.Fatalf("settle A: %v", err)
	}
	for _, e := range consumedA {
		if got := groomingArtifactOf(t, e); got != f.artA {
			t.Errorf("A consumed a disposition for artifact %s, want A (%s)", got, f.artA)
		}
	}
}

func groomingArtifactOf(t *testing.T, e *audit.Entry) string {
	t.Helper()
	var p struct {
		ArtifactID string `json:"artifact_id"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decode consumed payload: %v", err)
	}
	return p.ArtifactID
}

// TestGroomingWindowClosedError_Error pins the error string the 409 handler
// renders from — it names the artifact, the settlement and the watermark sequence.
func TestGroomingWindowClosedError_Error(t *testing.T) {
	e := &audit.GroomingWindowClosedError{
		ArtifactID: "art-1", Settlement: "approved", Sequence: 7, ClosedAt: time.Now().UTC(),
	}
	msg := e.Error()
	for _, want := range []string{"art-1", "approved", "7"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}
}

// TestGroomingWindow_EmptyBatchNoop: an empty capture batch is a no-op that
// writes nothing and returns no error (the len(ps)==0 guard).
func TestGroomingWindow_EmptyBatchNoop(t *testing.T) {
	f := newGWFixture(t)
	entries, err := f.win.AppendChainedGroomingDispositionBatch(context.Background(), f.artA, nil)
	if err != nil {
		t.Fatalf("empty batch returned error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("empty batch returned %d entries, want 0", len(entries))
	}
	if rows := f.dispositionRows(t); len(rows) != 0 {
		t.Errorf("empty batch committed %d rows, want 0", len(rows))
	}
}

// TestGroomingWindow_BatchRunNotFound: a capture batch whose params name a
// nonexistent run fails at the run-row lock (pgx.ErrNoRows) writing nothing.
func TestGroomingWindow_BatchRunNotFound(t *testing.T) {
	f := newGWFixture(t)
	p := f.dispositionParams(f.artA, "ordering:a", "approved")
	p.RunID = uuid.New() // a run that does not exist
	_, err := f.win.AppendChainedGroomingDispositionBatch(context.Background(), f.artA,
		[]audit.ChainAppendParams{p})
	if err == nil {
		t.Fatal("batch against a nonexistent run returned nil error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want it to name the missing run", err)
	}
}

// TestGroomingWindow_SettlementRunNotFound: a settlement whose param names a
// nonexistent run fails at the run-row lock writing nothing.
func TestGroomingWindow_SettlementRunNotFound(t *testing.T) {
	f := newGWFixture(t)
	p := f.watermarkParams(f.artA, "approved")
	p.RunID = uuid.New() // a run that does not exist
	_, _, err := f.win.AppendChainedGroomingWindowClose(context.Background(), p, f.artA)
	if err == nil {
		t.Fatal("settlement against a nonexistent run returned nil error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want it to name the missing run", err)
	}
}
