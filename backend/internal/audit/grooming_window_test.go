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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
	pool  *pgxpool.Pool
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
		pool: pool, audit: auditRepo, win: win, runID: r.ID,
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

// --- upkeep family (#3923) ---------------------------------------------------
//
// The same protocol, generalized over a second disposition family. Every
// upkeep test drives the PRODUCTION repo through audit.UpkeepWindowAppender.
// The upkeep batch additionally re-checks its BINDING under the run-row lock,
// so each fixture records an upkeep_report_recorded row naming the capture's
// artifact first (seeded by construction through AppendChained).

func (f *gwFixture) upkeepWin(t *testing.T) audit.UpkeepWindowAppender {
	t.Helper()
	win, ok := f.audit.(audit.UpkeepWindowAppender)
	if !ok {
		t.Fatal("the production audit repository must implement audit.UpkeepWindowAppender")
	}
	return win
}

func (f *gwFixture) appendRaw(t *testing.T, category string, payload map[string]any) *audit.Entry {
	t.Helper()
	raw, _ := json.Marshal(payload)
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now().UTC(), Category: category, Payload: raw,
	})
	if err != nil {
		t.Fatalf("seed %s: %v", category, err)
	}
	return e
}

// recordUpkeepReport seeds the upkeep_report_recorded row binding artifactID.
func (f *gwFixture) recordUpkeepReport(t *testing.T, artifactID string) *audit.Entry {
	t.Helper()
	return f.appendRaw(t, audit.UpkeepReportRecordedCategory, map[string]any{"run_id": f.runID.String(), "artifact_id": artifactID})
}

func (f *gwFixture) upkeepDisposition(artifactID, findingID, verdict string) audit.ChainAppendParams {
	payload, _ := json.Marshal(map[string]any{
		"run_id": f.runID.String(), "artifact_id": artifactID,
		"finding_id": findingID, "verdict": verdict,
	})
	return audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now().UTC(),
		Category: audit.UpkeepDispositionRecordedCategory, Payload: payload,
	}
}

func (f *gwFixture) upkeepWatermark(artifactID, settlement string) audit.ChainAppendParams {
	payload, _ := json.Marshal(map[string]any{
		"run_id": f.runID.String(), "artifact_id": artifactID, "settlement": settlement,
	})
	return audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now().UTC(),
		Category: audit.UpkeepApplyWindowClosedCategory, Payload: payload,
	}
}

func (f *gwFixture) rowsOf(t *testing.T, category string) []*audit.Entry {
	t.Helper()
	rows, err := f.audit.ListForRunByCategory(context.Background(), f.runID, category)
	if err != nil {
		t.Fatalf("list %s: %v", category, err)
	}
	return rows
}

// TestUpkeepWindow_BatchRefusedAtClosedWindow: an artifact-bound
// upkeep_apply_window_closed row (seeded DIRECTLY) refuses the batch with
// *UpkeepWindowClosedError and ZERO committed rows.
//
// COUNTERFACTUAL (watermark scan in windowDispositionBatchTx): make it never
// find the watermark — the rows land and the error is nil. The watermark
// exists only in the DB, so only the in-transaction scan sees it.
func TestUpkeepWindow_BatchRefusedAtClosedWindow(t *testing.T) {
	f := newGWFixture(t)
	win := f.upkeepWin(t)
	f.recordUpkeepReport(t, f.artA)
	wm := f.appendRaw(t, audit.UpkeepApplyWindowClosedCategory, map[string]any{"artifact_id": f.artA, "settlement": "rejected"})

	_, err := win.AppendChainedUpkeepDispositionBatch(context.Background(), f.artA, []audit.ChainAppendParams{
		f.upkeepDisposition(f.artA, "flake:a", "approved"),
		f.upkeepDisposition(f.artA, "flake:b", "rejected"),
	})
	var closed *audit.UpkeepWindowClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("err = %v (%T), want *audit.UpkeepWindowClosedError", err, err)
	}
	if closed.ArtifactID != f.artA || closed.Settlement != "rejected" || closed.Sequence != wm.Sequence {
		t.Errorf("closed = %+v, want artifact %s settlement rejected sequence %d", closed, f.artA, wm.Sequence)
	}
	if rows := f.rowsOf(t, audit.UpkeepDispositionRecordedCategory); len(rows) != 0 {
		t.Errorf("committed upkeep disposition rows = %d, want 0", len(rows))
	}
}

// TestUpkeepWindow_BatchRefusedWhenReportSuperseded is the BINDING RE-CHECK
// (#3923 approval condition 1) at the audit layer: report A was recorded, then
// report B — a capture still bound to A is refused with
// *UpkeepReportSupersededError naming B, and NOTHING is written.
//
// COUNTERFACTUAL (checkReportBinding): make it return nil — the rows land
// against the superseded A and the error is nil.
func TestUpkeepWindow_BatchRefusedWhenReportSuperseded(t *testing.T) {
	f := newGWFixture(t)
	win := f.upkeepWin(t)
	f.recordUpkeepReport(t, f.artA)
	b := f.recordUpkeepReport(t, f.artB)

	_, err := win.AppendChainedUpkeepDispositionBatch(context.Background(), f.artA, []audit.ChainAppendParams{
		f.upkeepDisposition(f.artA, "flake:a", "approved"),
	})
	var sup *audit.UpkeepReportSupersededError
	if !errors.As(err, &sup) {
		t.Fatalf("err = %v (%T), want *audit.UpkeepReportSupersededError", err, err)
	}
	if sup.ArtifactID != f.artA || sup.CurrentArtifactID != f.artB || sup.CurrentSequence != b.Sequence {
		t.Errorf("superseded = %+v, want artifact %s current %s sequence %d", sup, f.artA, f.artB, b.Sequence)
	}
	if rows := f.rowsOf(t, audit.UpkeepDispositionRecordedCategory); len(rows) != 0 {
		t.Errorf("committed upkeep disposition rows = %d, want 0", len(rows))
	}

	// The CURRENT report still captures.
	if _, err := win.AppendChainedUpkeepDispositionBatch(context.Background(), f.artB, []audit.ChainAppendParams{
		f.upkeepDisposition(f.artB, "flake:a", "approved"),
	}); err != nil {
		t.Fatalf("capture against the current report: %v", err)
	}
}

// TestUpkeepWindow_BatchRefusedWithNoReportRow: no upkeep_report_recorded row
// at all — the binding cannot be confirmed, so the batch refuses (fail closed)
// with an empty CurrentArtifactID, writing nothing.
func TestUpkeepWindow_BatchRefusedWithNoReportRow(t *testing.T) {
	f := newGWFixture(t)
	_, err := f.upkeepWin(t).AppendChainedUpkeepDispositionBatch(context.Background(), f.artA, []audit.ChainAppendParams{
		f.upkeepDisposition(f.artA, "flake:a", "approved"),
	})
	var sup *audit.UpkeepReportSupersededError
	if !errors.As(err, &sup) || sup.CurrentArtifactID != "" {
		t.Fatalf("err = %v (%T), want *audit.UpkeepReportSupersededError with no current artifact", err, err)
	}
	if rows := f.rowsOf(t, audit.UpkeepDispositionRecordedCategory); len(rows) != 0 {
		t.Errorf("committed rows = %d, want 0", len(rows))
	}
}

// TestUpkeepWindow_RepeatedCloseReturnsFirstWatermark: PERMANENCE — a second
// close returns the FIRST watermark unchanged, appends nothing, and its
// consumed set stays bounded by the first watermark.
func TestUpkeepWindow_RepeatedCloseReturnsFirstWatermark(t *testing.T) {
	f := newGWFixture(t)
	win := f.upkeepWin(t)
	ctx := context.Background()
	f.recordUpkeepReport(t, f.artA)
	if _, err := win.AppendChainedUpkeepDispositionBatch(ctx, f.artA, []audit.ChainAppendParams{
		f.upkeepDisposition(f.artA, "flake:a", "approved"),
	}); err != nil {
		t.Fatalf("capture: %v", err)
	}
	first, consumed1, err := win.AppendChainedUpkeepWindowClose(ctx, f.upkeepWatermark(f.artA, "approved"), f.artA)
	if err != nil {
		t.Fatalf("first close: %v", err)
	}
	second, consumed2, err := win.AppendChainedUpkeepWindowClose(ctx, f.upkeepWatermark(f.artA, "rejected"), f.artA)
	if err != nil {
		t.Fatalf("second close: %v", err)
	}
	if second.Sequence != first.Sequence {
		t.Errorf("second close returned sequence %d, want the first watermark %d", second.Sequence, first.Sequence)
	}
	if n := len(f.rowsOf(t, audit.UpkeepApplyWindowClosedCategory)); n != 1 {
		t.Errorf("watermark rows = %d, want 1", n)
	}
	if len(consumed1) != 1 || len(consumed2) != 1 || consumed1[0].Sequence != consumed2[0].Sequence {
		t.Errorf("consumed sets differ: %d vs %d", len(consumed1), len(consumed2))
	}
}

// TestUpkeepWindow_ConsumedSetArtifactScoped: rows of a SECOND artifact and
// rows above the watermark are excluded from the consumed set.
func TestUpkeepWindow_ConsumedSetArtifactScoped(t *testing.T) {
	f := newGWFixture(t)
	ctx := context.Background()
	// Seeded DIRECTLY so both artifacts carry rows below A's watermark.
	f.appendRaw(t, audit.UpkeepDispositionRecordedCategory, map[string]any{"artifact_id": f.artA, "finding_id": "flake:a", "verdict": "approved"})
	f.appendRaw(t, audit.UpkeepDispositionRecordedCategory, map[string]any{"artifact_id": f.artB, "finding_id": "flake:b", "verdict": "approved"})
	w, consumed, err := f.upkeepWin(t).AppendChainedUpkeepWindowClose(ctx, f.upkeepWatermark(f.artA, "approved"), f.artA)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	// Above the watermark: seeded after the close.
	f.appendRaw(t, audit.UpkeepDispositionRecordedCategory, map[string]any{"artifact_id": f.artA, "finding_id": "flake:c", "verdict": "approved"})
	_, again, err := f.upkeepWin(t).AppendChainedUpkeepWindowClose(ctx, f.upkeepWatermark(f.artA, "approved"), f.artA)
	if err != nil {
		t.Fatalf("repeat close: %v", err)
	}
	for _, set := range [][]*audit.Entry{consumed, again} {
		if len(set) != 1 || groomingArtifactOf(t, set[0]) != f.artA || set[0].Sequence >= w.Sequence {
			t.Fatalf("consumed = %d entries, want exactly A's row below watermark %d", len(set), w.Sequence)
		}
	}
}

// TestUpkeepWindow_FamilyIsolation: a grooming disposition row and a grooming
// watermark carrying the SAME artifact_id as the upkeep artifact neither close
// the upkeep window nor enter the upkeep consumed set.
//
// COUNTERFACTUAL: point upkeepFamily at the grooming categories — the capture
// is refused (grooming watermark seen) or the consumed set includes the
// grooming row.
func TestUpkeepWindow_FamilyIsolation(t *testing.T) {
	f := newGWFixture(t)
	ctx := context.Background()
	win := f.upkeepWin(t)
	f.recordUpkeepReport(t, f.artA)
	if _, err := f.audit.AppendChained(ctx, f.dispositionParams(f.artA, "ordering:a", "approved")); err != nil {
		t.Fatalf("seed grooming disposition: %v", err)
	}
	if _, err := f.audit.AppendChained(ctx, f.watermarkParams(f.artA, "approved")); err != nil {
		t.Fatalf("seed grooming watermark: %v", err)
	}
	es, err := win.AppendChainedUpkeepDispositionBatch(ctx, f.artA, []audit.ChainAppendParams{
		f.upkeepDisposition(f.artA, "flake:a", "approved"),
	})
	if err != nil {
		t.Fatalf("upkeep capture refused by a GROOMING watermark: %v", err)
	}
	_, consumed, err := win.AppendChainedUpkeepWindowClose(ctx, f.upkeepWatermark(f.artA, "approved"), f.artA)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(consumed) != 1 || consumed[0].Sequence != es[0].Sequence || consumed[0].Category != audit.UpkeepDispositionRecordedCategory {
		t.Fatalf("consumed = %d entries, want exactly the upkeep row %d", len(consumed), es[0].Sequence)
	}
}

// TestUpkeepWindow_ConcurrentCaptureAndSettlementSerialize mirrors the grooming
// race test: every capture either lands below the watermark (and is consumed)
// or is refused whole — none lands above it, none is partial.
func TestUpkeepWindow_ConcurrentCaptureAndSettlementSerialize(t *testing.T) {
	const rounds = 8
	stagger := timescale.D(3 * time.Millisecond)
	for i := 0; i < rounds; i++ {
		f := newGWFixture(t)
		win := f.upkeepWin(t)
		f.recordUpkeepReport(t, f.artA)
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
			_, captureErr = win.AppendChainedUpkeepDispositionBatch(context.Background(), f.artA, []audit.ChainAppendParams{
				f.upkeepDisposition(f.artA, "flake:a", "approved"),
				f.upkeepDisposition(f.artA, "flake:b", "rejected"),
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			if captureFirst {
				time.Sleep(stagger)
			}
			_, consumed, settleErr = win.AppendChainedUpkeepWindowClose(context.Background(), f.upkeepWatermark(f.artA, "approved"), f.artA)
		}()
		close(start)
		wg.Wait()
		if settleErr != nil {
			t.Fatalf("round %d: settle: %v", i, settleErr)
		}
		rows := f.rowsOf(t, audit.UpkeepDispositionRecordedCategory)
		var closed *audit.UpkeepWindowClosedError
		switch {
		case captureErr == nil:
			if len(rows) != 2 || len(consumed) != 2 {
				t.Errorf("round %d: capture landed: rows=%d consumed=%d, want 2/2", i, len(rows), len(consumed))
			}
		case errors.As(captureErr, &closed):
			if len(rows) != 0 || len(consumed) != 0 {
				t.Errorf("round %d: capture refused: rows=%d consumed=%d, want 0/0", i, len(rows), len(consumed))
			}
		default:
			t.Errorf("round %d: capture err = %v, want nil or UpkeepWindowClosedError", i, captureErr)
		}
	}
}

// TestUpkeepWindow_ErrorStrings pins the two upkeep refusals' text: each names
// the facts the 409 handler renders.
func TestUpkeepWindow_ErrorStrings(t *testing.T) {
	closed := (&audit.UpkeepWindowClosedError{ArtifactID: "art-1", Settlement: "rejected", Sequence: 9}).Error()
	for _, want := range []string{"upkeep", "art-1", "rejected", "9"} {
		if !strings.Contains(closed, want) {
			t.Errorf("UpkeepWindowClosedError = %q, want %q", closed, want)
		}
	}
	sup := (&audit.UpkeepReportSupersededError{ArtifactID: "art-1", CurrentArtifactID: "art-2", CurrentSequence: 11}).Error()
	for _, want := range []string{"art-1", "art-2", "11", "superseded"} {
		if !strings.Contains(sup, want) {
			t.Errorf("UpkeepReportSupersededError = %q, want %q", sup, want)
		}
	}
}

// TestUpkeepWindow_RunNotFound: both upkeep cores fail at the run-row lock for
// an unknown run, and an empty batch is a no-op.
func TestUpkeepWindow_RunNotFound(t *testing.T) {
	f := newGWFixture(t)
	win := f.upkeepWin(t)
	p := f.upkeepDisposition(f.artA, "flake:a", "approved")
	p.RunID = uuid.New()
	if _, err := win.AppendChainedUpkeepDispositionBatch(context.Background(), f.artA, []audit.ChainAppendParams{p}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("batch err = %v, want run not found", err)
	}
	w := f.upkeepWatermark(f.artA, "approved")
	w.RunID = uuid.New()
	if _, _, err := win.AppendChainedUpkeepWindowClose(context.Background(), w, f.artA); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("close err = %v, want run not found", err)
	}
	if es, err := win.AppendChainedUpkeepDispositionBatch(context.Background(), f.artA, nil); err != nil || len(es) != 0 {
		t.Errorf("empty batch = %v, %v; want no-op", es, err)
	}
}

// --- comms family + the generic FamilyWindowAppender (#4012) ----------------
//
// The comms family has no typed entry points: every comms test drives the
// PRODUCTION repo through audit.FamilyWindowAppender with family "comms". Like
// upkeep it re-checks its binding under the run-row lock, so each fixture
// records a comms_report_recorded row naming the capture's artifact first
// (seeded by construction through AppendChained).

func (f *gwFixture) familyWin(t *testing.T) audit.FamilyWindowAppender {
	t.Helper()
	win, ok := f.audit.(audit.FamilyWindowAppender)
	if !ok {
		t.Fatal("the production audit repository must implement audit.FamilyWindowAppender")
	}
	return win
}

func (f *gwFixture) recordCommsReport(t *testing.T, artifactID string) *audit.Entry {
	t.Helper()
	return f.appendRaw(t, audit.CommsReportRecordedCategory, map[string]any{"run_id": f.runID.String(), "artifact_id": artifactID})
}

func (f *gwFixture) commsDisposition(artifactID, entryID, verdict string) audit.ChainAppendParams {
	payload, _ := json.Marshal(map[string]any{
		"run_id": f.runID.String(), "artifact_id": artifactID,
		"entry_id": entryID, "verdict": verdict,
	})
	return audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now().UTC(),
		Category: audit.CommsDispositionRecordedCategory, Payload: payload,
	}
}

func (f *gwFixture) commsWatermark(artifactID, settlement string) audit.ChainAppendParams {
	payload, _ := json.Marshal(map[string]any{
		"run_id": f.runID.String(), "artifact_id": artifactID, "settlement": settlement,
	})
	return audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now().UTC(),
		Category: audit.CommsApplyWindowClosedCategory, Payload: payload,
	}
}

// runRowCount reads the run's TOTAL committed audit row count.
func (f *gwFixture) runRowCount(t *testing.T) int {
	t.Helper()
	rows, err := f.audit.ListForRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("list run rows: %v", err)
	}
	return len(rows)
}

// TestCommsWindow_BatchRefusedAtClosedWindow: an artifact-bound
// comms_apply_window_closed row (seeded DIRECTLY, by construction) refuses the
// generic comms batch with the GENERIC *WindowClosedError naming the family,
// and ZERO comms disposition rows commit (read after the call — an
// error-identity assertion alone cannot tell a refusal from a
// fired-then-rolled-back append).
//
// COUNTERFACTUAL (commsFamily.watermarkCategory): blank it. Fixture state that
// makes it observable: the binding is valid (report row names A), so the
// watermark scan is the ONLY refusal in the path, and the watermark exists
// only as a DB row of the comms category — with the scan pointed at "" it
// finds nothing, the batch commits both rows and the zero-rows read goes RED.
func TestCommsWindow_BatchRefusedAtClosedWindow(t *testing.T) {
	f := newGWFixture(t)
	f.recordCommsReport(t, f.artA)
	wm := f.appendRaw(t, audit.CommsApplyWindowClosedCategory, map[string]any{"artifact_id": f.artA, "settlement": "rejected"})

	_, err := f.familyWin(t).AppendChainedFamilyDispositionBatch(context.Background(), audit.WindowFamilyComms, f.artA, []audit.ChainAppendParams{
		f.commsDisposition(f.artA, "report:a", "approved"),
		f.commsDisposition(f.artA, "report:b", "rejected"),
	})
	var closed *audit.WindowClosedError
	if !errors.As(err, &closed) {
		t.Errorf("err = %v (%T), want *audit.WindowClosedError", err, err)
	} else if closed.Family != audit.WindowFamilyComms || closed.ArtifactID != f.artA || closed.Settlement != "rejected" || closed.Sequence != wm.Sequence {
		t.Errorf("closed = %+v, want family comms artifact %s settlement rejected sequence %d", closed, f.artA, wm.Sequence)
	}
	if rows := f.rowsOf(t, audit.CommsDispositionRecordedCategory); len(rows) != 0 {
		t.Errorf("committed comms disposition rows = %d, want 0 — a closed window records nothing", len(rows))
	}
}

// TestCommsWindow_BatchRefusedWhenReportSuperseded is the comms BINDING
// RE-CHECK: report A, then report B — a capture still bound to A is refused
// with the generic *ReportSupersededError naming B, and NOTHING is written.
//
// COUNTERFACTUAL (commsFamily.reportCategory): blank it. Fixture state that
// makes it observable: NO comms watermark exists, so with the binding re-check
// disabled nothing else refuses — the batch commits a row against the
// superseded A and the zero-rows read goes RED.
func TestCommsWindow_BatchRefusedWhenReportSuperseded(t *testing.T) {
	f := newGWFixture(t)
	win := f.familyWin(t)
	f.recordCommsReport(t, f.artA)
	b := f.recordCommsReport(t, f.artB)

	_, err := win.AppendChainedFamilyDispositionBatch(context.Background(), audit.WindowFamilyComms, f.artA, []audit.ChainAppendParams{
		f.commsDisposition(f.artA, "report:a", "approved"),
	})
	var sup *audit.ReportSupersededError
	if !errors.As(err, &sup) {
		t.Errorf("err = %v (%T), want *audit.ReportSupersededError", err, err)
	} else if sup.Family != audit.WindowFamilyComms || sup.ArtifactID != f.artA || sup.CurrentArtifactID != f.artB || sup.CurrentSequence != b.Sequence {
		t.Errorf("superseded = %+v, want family comms artifact %s current %s sequence %d", sup, f.artA, f.artB, b.Sequence)
	}
	if rows := f.rowsOf(t, audit.CommsDispositionRecordedCategory); len(rows) != 0 {
		t.Fatalf("committed comms disposition rows = %d, want 0 — the capture landed against the superseded report", len(rows))
	}

	// The CURRENT report still captures.
	if _, err := win.AppendChainedFamilyDispositionBatch(context.Background(), audit.WindowFamilyComms, f.artB, []audit.ChainAppendParams{
		f.commsDisposition(f.artB, "report:a", "approved"),
	}); err != nil {
		t.Fatalf("capture against the current report: %v", err)
	}
}

// TestCommsWindow_BatchRefusedWithNoReportRow: no comms_report_recorded row at
// all — the binding cannot be confirmed, so the batch refuses (fail closed)
// with an empty CurrentArtifactID, writing nothing. An UPKEEP report row naming
// the same artifact is seeded so the refusal is also proof the re-check reads
// the comms family's own report category.
func TestCommsWindow_BatchRefusedWithNoReportRow(t *testing.T) {
	f := newGWFixture(t)
	f.recordUpkeepReport(t, f.artA)
	_, err := f.familyWin(t).AppendChainedFamilyDispositionBatch(context.Background(), audit.WindowFamilyComms, f.artA, []audit.ChainAppendParams{
		f.commsDisposition(f.artA, "report:a", "approved"),
	})
	var sup *audit.ReportSupersededError
	if !errors.As(err, &sup) || sup.CurrentArtifactID != "" || sup.CurrentSequence != 0 || sup.Family != audit.WindowFamilyComms {
		t.Errorf("err = %v (%T), want comms *audit.ReportSupersededError with no current artifact", err, err)
	}
	if rows := f.rowsOf(t, audit.CommsDispositionRecordedCategory); len(rows) != 0 {
		t.Errorf("committed rows = %d, want 0", len(rows))
	}
}

// TestCommsWindow_RepeatedCloseReturnsFirstWatermark: PERMANENCE through the
// generic entry — a second close returns the FIRST watermark unchanged,
// appends nothing, and its consumed set stays bounded by the first watermark
// (a disposition seeded after the first close is not consumed).
func TestCommsWindow_RepeatedCloseReturnsFirstWatermark(t *testing.T) {
	f := newGWFixture(t)
	win := f.familyWin(t)
	ctx := context.Background()
	f.recordCommsReport(t, f.artA)
	es, err := win.AppendChainedFamilyDispositionBatch(ctx, audit.WindowFamilyComms, f.artA, []audit.ChainAppendParams{
		f.commsDisposition(f.artA, "report:a", "approved"),
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	first, consumed1, err := win.AppendChainedFamilyWindowClose(ctx, audit.WindowFamilyComms, f.commsWatermark(f.artA, "approved"), f.artA)
	if err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Above the first watermark, seeded DIRECTLY (the batch would refuse it).
	f.appendRaw(t, audit.CommsDispositionRecordedCategory, map[string]any{"artifact_id": f.artA, "entry_id": "report:late", "verdict": "approved"})
	second, consumed2, err := win.AppendChainedFamilyWindowClose(ctx, audit.WindowFamilyComms, f.commsWatermark(f.artA, "rejected"), f.artA)
	if err != nil {
		t.Fatalf("second close: %v", err)
	}
	if second.Sequence != first.Sequence {
		t.Errorf("second close returned sequence %d, want the first watermark %d", second.Sequence, first.Sequence)
	}
	if n := len(f.rowsOf(t, audit.CommsApplyWindowClosedCategory)); n != 1 {
		t.Errorf("comms watermark rows = %d, want 1", n)
	}
	for _, set := range [][]*audit.Entry{consumed1, consumed2} {
		if len(set) != 1 || set[0].Sequence != es[0].Sequence {
			t.Errorf("consumed = %d entries, want exactly the captured row %d", len(set), es[0].Sequence)
		}
	}
}

// TestCommsWindow_FamilyIsolation: an upkeep watermark and a comms watermark
// on the SAME artifact_id string each refuse only their own family's capture,
// and an upkeep disposition on that artifact never enters the comms consumed
// set.
//
// COUNTERFACTUAL (commsFamily.watermarkCategory): point it at
// UpkeepApplyWindowClosedCategory. Fixture state that makes it observable: the
// UPKEEP watermark for A is seeded before the first comms capture while no
// comms watermark exists, so the first comms batch is refused by the foreign
// watermark and the "comms capture refused by an UPKEEP watermark" assertion
// goes RED.
func TestCommsWindow_FamilyIsolation(t *testing.T) {
	f := newGWFixture(t)
	ctx := context.Background()
	win := f.familyWin(t)
	f.recordCommsReport(t, f.artA)
	f.recordUpkeepReport(t, f.artA)
	f.appendRaw(t, audit.UpkeepDispositionRecordedCategory, map[string]any{"artifact_id": f.artA, "finding_id": "flake:a", "verdict": "approved"})
	upWM := f.appendRaw(t, audit.UpkeepApplyWindowClosedCategory, map[string]any{"artifact_id": f.artA, "settlement": "approved"})

	es, err := win.AppendChainedFamilyDispositionBatch(ctx, audit.WindowFamilyComms, f.artA, []audit.ChainAppendParams{
		f.commsDisposition(f.artA, "report:a", "approved"),
	})
	if err != nil {
		t.Fatalf("comms capture refused by an UPKEEP watermark: %v", err)
	}
	// The upkeep window IS closed for its own family.
	var upClosed *audit.UpkeepWindowClosedError
	if _, err := win.AppendChainedFamilyDispositionBatch(ctx, audit.WindowFamilyUpkeep, f.artA, []audit.ChainAppendParams{
		f.upkeepDisposition(f.artA, "flake:b", "approved"),
	}); !errors.As(err, &upClosed) || upClosed.Sequence != upWM.Sequence {
		t.Fatalf("upkeep capture err = %v, want *UpkeepWindowClosedError at its own watermark %d", err, upWM.Sequence)
	}
	w, consumed, err := win.AppendChainedFamilyWindowClose(ctx, audit.WindowFamilyComms, f.commsWatermark(f.artA, "approved"), f.artA)
	if err != nil {
		t.Fatalf("comms close: %v", err)
	}
	if w.Sequence == upWM.Sequence || w.Category != audit.CommsApplyWindowClosedCategory {
		t.Fatalf("comms close returned %s seq %d — the upkeep watermark was taken as the comms one", w.Category, w.Sequence)
	}
	if len(consumed) != 1 || consumed[0].Sequence != es[0].Sequence || consumed[0].Category != audit.CommsDispositionRecordedCategory {
		t.Fatalf("comms consumed = %d entries, want exactly the comms row %d", len(consumed), es[0].Sequence)
	}
	// And now the comms window refuses for comms.
	var closed *audit.WindowClosedError
	if _, err := win.AppendChainedFamilyDispositionBatch(ctx, audit.WindowFamilyComms, f.artA, []audit.ChainAppendParams{
		f.commsDisposition(f.artA, "report:b", "approved"),
	}); !errors.As(err, &closed) || closed.Sequence != w.Sequence {
		t.Fatalf("second comms capture err = %v, want *WindowClosedError at comms watermark %d", err, w.Sequence)
	}
}

// TestFamilyWindow_UnknownFamilyRefusedWritesNothing: an unregistered family
// name is refused with ErrUnknownWindowFamily on BOTH generic entries — through
// the repository AND through the exported Tx cores directly — and the run's
// TOTAL committed row count is unchanged.
//
// COUNTERFACTUAL (resolveWindowFamily): make its body
// `return windowFamilies[name], nil`. Fixture state that makes it observable:
// the params are VALID appends carrying the run's own id, and the zero-value
// family has empty categories — no binding re-check, a watermark scan over ""
// that finds nothing — so the batch commits its row and the close commits p;
// the Tx-core leg COMMITS its transaction after the call, so a core that
// wrote would persist. Every row-count read goes RED (not a compile error).
// Deleting only the postgresRepo pre-check is masked by the Tx core's own
// check (same error, still nothing written); that pre-check's job is to not
// OPEN a transaction, which no row count can observe.
func TestFamilyWindow_UnknownFamilyRefusedWritesNothing(t *testing.T) {
	f := newGWFixture(t)
	ctx := context.Background()
	win := f.familyWin(t)
	f.recordCommsReport(t, f.artA)
	before := f.runRowCount(t)

	// Committed state is read FIRST, so a control that is absent reddens on
	// the row count rather than only on the error identity.
	assertUnknown := func(leg string, err error) {
		t.Helper()
		if n := f.runRowCount(t); n != before {
			t.Fatalf("%s: run audit rows %d -> %d, want unchanged — an unknown family must write nothing", leg, before, n)
		}
		if !errors.Is(err, audit.ErrUnknownWindowFamily) {
			t.Fatalf("%s: err = %v (%T), want ErrUnknownWindowFamily", leg, err, err)
		}
		var uf *audit.UnknownWindowFamilyError
		if !errors.As(err, &uf) || uf.Family != "nope" {
			t.Fatalf("%s: err = %v, want *UnknownWindowFamilyError{Family: nope}", leg, err)
		}
	}

	es, err := win.AppendChainedFamilyDispositionBatch(ctx, "nope", f.artA, []audit.ChainAppendParams{
		f.commsDisposition(f.artA, "report:a", "approved"),
	})
	if es != nil {
		t.Errorf("batch returned entries %v on refusal", es)
	}
	assertUnknown("repo batch", err)
	w, consumed, err := win.AppendChainedFamilyWindowClose(ctx, "nope", f.commsWatermark(f.artA, "approved"), f.artA)
	if w != nil || consumed != nil {
		t.Errorf("close returned %v / %v on refusal", w, consumed)
	}
	assertUnknown("repo close", err)

	// The Tx cores, driven directly inside a transaction that is COMMITTED
	// after the call, so a core that wrote anything would persist it.
	runTx := func(fn func(tx pgx.Tx) error) error {
		tx, berr := f.pool.Begin(ctx)
		if berr != nil {
			t.Fatalf("begin: %v", berr)
		}
		callErr := fn(tx)
		if cerr := tx.Commit(ctx); cerr != nil {
			t.Fatalf("commit: %v", cerr)
		}
		return callErr
	}
	assertUnknown("tx batch", runTx(func(tx pgx.Tx) error {
		_, e := audit.AppendChainedFamilyDispositionBatchTx(ctx, tx, "nope", f.artA, []audit.ChainAppendParams{
			f.commsDisposition(f.artA, "report:a", "approved"),
		})
		return e
	}))
	assertUnknown("tx close", runTx(func(tx pgx.Tx) error {
		_, _, e := audit.AppendChainedFamilyWindowCloseTx(ctx, tx, "nope", f.commsWatermark(f.artA, "approved"), f.artA)
		return e
	}))

	// The Tx cores still serve a KNOWN family (the resolve is not a blanket
	// refusal).
	if err := runTx(func(tx pgx.Tx) error {
		_, e := audit.AppendChainedFamilyDispositionBatchTx(ctx, tx, audit.WindowFamilyComms, f.artA, []audit.ChainAppendParams{
			f.commsDisposition(f.artA, "report:a", "approved"),
		})
		return e
	}); err != nil {
		t.Fatalf("tx batch for comms: %v", err)
	}
	if err := runTx(func(tx pgx.Tx) error {
		_, c, e := audit.AppendChainedFamilyWindowCloseTx(ctx, tx, audit.WindowFamilyComms, f.commsWatermark(f.artA, "approved"), f.artA)
		if e == nil && len(c) != 1 {
			t.Errorf("tx close consumed %d, want 1", len(c))
		}
		return e
	}); err != nil {
		t.Fatalf("tx close for comms: %v", err)
	}
}

// TestFamilyWindow_GenericEntryKeepsTypedErrorsForUpkeep: grooming and upkeep
// reached through the GENERIC entry return THEIR pre-existing typed refusals,
// never the generic ones, so the typed and generic entry points cannot
// disagree about one family.
//
// COUNTERFACTUAL (upkeepFamily.superseded / .closed): swap either constructor
// for the generic error type. Fixture state that makes it observable: each leg
// seeds exactly the one refusal condition (a closed upkeep window with a valid
// binding; a superseded upkeep binding with no watermark; a closed grooming
// window), so the error that comes back is that constructor's output alone and
// the errors.As on the typed error goes RED.
func TestFamilyWindow_GenericEntryKeepsTypedErrorsForUpkeep(t *testing.T) {
	ctx := context.Background()

	t.Run("upkeep closed", func(t *testing.T) {
		f := newGWFixture(t)
		f.recordUpkeepReport(t, f.artA)
		f.appendRaw(t, audit.UpkeepApplyWindowClosedCategory, map[string]any{"artifact_id": f.artA, "settlement": "approved"})
		_, err := f.familyWin(t).AppendChainedFamilyDispositionBatch(ctx, audit.WindowFamilyUpkeep, f.artA, []audit.ChainAppendParams{
			f.upkeepDisposition(f.artA, "flake:a", "approved"),
		})
		var typed *audit.UpkeepWindowClosedError
		var generic *audit.WindowClosedError
		if !errors.As(err, &typed) || errors.As(err, &generic) {
			t.Fatalf("err = %v (%T), want *audit.UpkeepWindowClosedError and NOT the generic type", err, err)
		}
	})
	t.Run("upkeep superseded", func(t *testing.T) {
		f := newGWFixture(t)
		f.recordUpkeepReport(t, f.artA)
		f.recordUpkeepReport(t, f.artB)
		_, err := f.familyWin(t).AppendChainedFamilyDispositionBatch(ctx, audit.WindowFamilyUpkeep, f.artA, []audit.ChainAppendParams{
			f.upkeepDisposition(f.artA, "flake:a", "approved"),
		})
		var typed *audit.UpkeepReportSupersededError
		var generic *audit.ReportSupersededError
		if !errors.As(err, &typed) || errors.As(err, &generic) || typed.CurrentArtifactID != f.artB {
			t.Fatalf("err = %v (%T), want *audit.UpkeepReportSupersededError naming %s and NOT the generic type", err, err, f.artB)
		}
	})
	t.Run("grooming closed", func(t *testing.T) {
		f := newGWFixture(t)
		if _, err := f.audit.AppendChained(ctx, f.watermarkParams(f.artA, "approved")); err != nil {
			t.Fatalf("seed grooming watermark: %v", err)
		}
		_, err := f.familyWin(t).AppendChainedFamilyDispositionBatch(ctx, audit.WindowFamilyGrooming, f.artA, []audit.ChainAppendParams{
			f.dispositionParams(f.artA, "ordering:a", "approved"),
		})
		var typed *audit.GroomingWindowClosedError
		var generic *audit.WindowClosedError
		if !errors.As(err, &typed) || errors.As(err, &generic) {
			t.Fatalf("err = %v (%T), want *audit.GroomingWindowClosedError and NOT the generic type", err, err)
		}
		if rows := f.dispositionRows(t); len(rows) != 0 {
			t.Errorf("committed grooming rows = %d, want 0", len(rows))
		}
	})
}

// TestLookupWindowFamily: the exported registry view returns each known
// family's categories from the same source the Tx cores use, and ok=false for
// an unknown name.
func TestLookupWindowFamily(t *testing.T) {
	cases := []audit.WindowFamilyInfo{
		{Name: audit.WindowFamilyGrooming, DispositionCategory: audit.GroomingDispositionRecordedCategory, WatermarkCategory: audit.GroomingApplyWindowClosedCategory},
		{Name: audit.WindowFamilyUpkeep, DispositionCategory: audit.UpkeepDispositionRecordedCategory, WatermarkCategory: audit.UpkeepApplyWindowClosedCategory, ReportCategory: audit.UpkeepReportRecordedCategory},
		{Name: audit.WindowFamilyComms, DispositionCategory: audit.CommsDispositionRecordedCategory, WatermarkCategory: audit.CommsApplyWindowClosedCategory, ReportCategory: audit.CommsReportRecordedCategory},
	}
	for _, want := range cases {
		got, ok := audit.LookupWindowFamily(want.Name)
		if !ok || got != want {
			t.Errorf("LookupWindowFamily(%q) = %+v, %v; want %+v, true", want.Name, got, ok, want)
		}
	}
	if got, ok := audit.LookupWindowFamily("nope"); ok || got != (audit.WindowFamilyInfo{}) {
		t.Errorf("LookupWindowFamily(nope) = %+v, %v; want zero, false", got, ok)
	}
}

// TestWindowClosedError_Error pins the generic closed refusal's text: it names
// the family and the watermark's facts the 409 handler renders.
func TestWindowClosedError_Error(t *testing.T) {
	got := (&audit.WindowClosedError{Family: "comms", ArtifactID: "art-1", Settlement: "rejected", Sequence: 9}).Error()
	want := "audit: comms capture window for artifact art-1 is closed (settlement=rejected, watermark sequence 9)"
	if got != want {
		t.Errorf("WindowClosedError = %q, want %q", got, want)
	}
}

// TestReportSupersededError_Error pins the generic superseded refusal's text,
// and the unknown-family refusal's.
func TestReportSupersededError_Error(t *testing.T) {
	got := (&audit.ReportSupersededError{Family: "comms", ArtifactID: "art-1", CurrentArtifactID: "art-2", CurrentSequence: 11}).Error()
	want := `audit: comms report art-1 is superseded by "art-2" (report sequence 11); re-capture against the current report`
	if got != want {
		t.Errorf("ReportSupersededError = %q, want %q", got, want)
	}
	uf := (&audit.UnknownWindowFamilyError{Family: "nope"}).Error()
	for _, w := range []string{`"nope"`, "grooming", "upkeep", "comms"} {
		if !strings.Contains(uf, w) {
			t.Errorf("UnknownWindowFamilyError = %q, want %q", uf, w)
		}
	}
}
