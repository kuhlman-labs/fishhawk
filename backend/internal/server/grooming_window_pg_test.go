package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// grooming_window_pg_test.go drives the #2991 capture/apply window END TO END
// against REAL Postgres — the run-row lock, the atomic batch, the settlement
// watermark and the on-approval consumption all live in the store and the hook.

// gwPGFixture is a real run + plan (groom) stage + grooming_report artifact, with
// an approval repo and the provider seams stubbed to recording fakes.
type gwPGFixture struct {
	s       *Server
	runID   uuid.UUID
	stage   *run.Stage
	artID   uuid.UUID
	ids     groomingApplyEntryIDs
	audit   audit.Repository
	appr    approval.Repository
	mutator *groomingApplyMutator
}

func newGWPGFixture(t *testing.T) *gwPGFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	apprRepo := approval.NewPostgresRepository(pool)

	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: groomingApplyRepo, WorkflowID: "backlog_grooming", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI, WorkflowSpec: []byte(groomingApplySpec),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, rn.ID, run.StateRunning); err != nil {
		t.Fatalf("run -> running: %v", err)
	}
	stage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: rn.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("create groom stage: %v", err)
	}
	report, ids := groomingApplyFullReport()
	body := groomingApplyReportJSON(t, report)
	sv := plan.GroomingReportVersion
	art, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: stage.ID, Kind: artifact.KindGroomingReport,
		SchemaVersion: &sv, Content: body, ContentHash: sha256Hex(body),
	})
	if err != nil {
		t.Fatalf("create grooming_report artifact: %v", err)
	}

	s := New(Config{RunRepo: runRepo, ArtifactRepo: artRepo, AuditRepo: auditRepo, ApprovalRepo: apprRepo})

	mut := &groomingApplyMutator{}
	prevConv := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		return workmgmt.Conventions{Provider: "github", States: map[string]string{
			workmgmt.CanonicalStateBacklog: "Backlog", workmgmt.CanonicalStateUpNext: "Up Next",
		}}, nil
	}
	t.Cleanup(func() { conventionsLoader = prevConv })
	prevMut := groomingMutatorFor
	groomingMutatorFor = func(string) (workmgmt.GroomingMutator, error) { return mut, nil }
	t.Cleanup(func() { groomingMutatorFor = prevMut })
	prevRdr := groomingReaderFor
	groomingReaderFor = func(string) (workmgmt.WorkItemReader, error) { return &groomingApplyReader{}, nil }
	t.Cleanup(func() { groomingReaderFor = prevRdr })

	return &gwPGFixture{
		s: s, runID: rn.ID, stage: stage, artID: art.ID, ids: ids,
		audit: auditRepo, appr: apprRepo, mutator: mut,
	}
}

func (f *gwPGFixture) rowsOf(t *testing.T, category string) []*audit.Entry {
	t.Helper()
	rows, err := f.audit.ListForRunByCategory(context.Background(), f.runID, category)
	if err != nil {
		t.Fatalf("list %s: %v", category, err)
	}
	return rows
}

// mutationOutcomes indexes the grooming_mutation_applied chain by entry id. It is
// how a race test observes what the settlement actually CONSUMED: a consumed
// disposition drives its entry's decision (and thus its outcome), whereas an
// UNconsumed entry falls to its undispositioned default — so the outcome, not the
// mere row count, discriminates the consumed set.
func (f *gwPGFixture) mutationOutcomes(t *testing.T) map[string]workmgmt.GroomingMutationRecord {
	t.Helper()
	out := map[string]workmgmt.GroomingMutationRecord{}
	for _, e := range f.rowsOf(t, workmgmt.GroomingMutationAppliedCategory) {
		var rec workmgmt.GroomingMutationRecord
		if err := json.Unmarshal(e.Payload, &rec); err != nil {
			t.Fatalf("decode mutation: %v", err)
		}
		out[rec.EntryID] = rec
	}
	return out
}

// postGDNoT posts a disposition batch WITHOUT touching *testing.T, so it is safe
// to call from a spawned goroutine (a t-method from a non-test goroutine misroutes
// FailNow/Goexit). The caller records the response and asserts after wg.Wait.
func postGDNoT(s *Server, runID uuid.UUID, raw string,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID.String()+"/grooming-dispositions", strings.NewReader(raw))
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleRecordGroomingDispositions(w, withID(req))
	return w
}

// TestGroomingDispositionsConsumedEndToEnd is the CROSS-BOUNDARY done-means: a
// real capture of an approved DEDUP entry plus a rejected HYGIENE entry, a real
// approval, and assertions on the PERSISTED grooming_mutation_applied chain —
// the approved dedup dispatched a close while the rejected hygiene did not, and
// the difference is readable in the chain. A comment-only touch fails it.
func TestGroomingDispositionsConsumedEndToEnd(t *testing.T) {
	f := newGWPGFixture(t)

	body := fmt.Sprintf(`{"dispositions":[
	  {"entry_id":%q,"verdict":"approved","close_target":%q},
	  {"entry_id":%q,"verdict":"rejected"}
	]}`, f.ids.duplicate, groomingApplyRepo+"#16", f.ids.hygiene)
	if w := postGD(t, f.s, f.runID, body, gdOperator); w.Code != http.StatusOK {
		t.Fatalf("capture status = %d: %s", w.Code, w.Body.String())
	}

	// Approve the gate, then run the on-approval hook.
	if _, err := f.appr.Submit(context.Background(), approval.SubmitParams{
		StageID: f.stage.ID, ApproverSubject: "kuhlman-labs", Decision: approval.DecisionApprove, Surface: approval.SurfaceAPI,
	}); err != nil {
		t.Fatalf("submit approval: %v", err)
	}
	f.s.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)

	// The approved dedup entry dispatched a close; the rejected hygiene did not.
	mutations := map[string]workmgmt.GroomingMutationRecord{}
	for _, e := range f.rowsOf(t, workmgmt.GroomingMutationAppliedCategory) {
		var rec workmgmt.GroomingMutationRecord
		if err := json.Unmarshal(e.Payload, &rec); err != nil {
			t.Fatalf("decode mutation: %v", err)
		}
		mutations[rec.EntryID] = rec
	}
	dup, ok := mutations[f.ids.duplicate]
	if !ok {
		t.Fatalf("no grooming_mutation_applied row for the approved dedup entry %q; chain=%v", f.ids.duplicate, mutations)
	}
	if dup.Outcome != workmgmt.GroomingOutcomeApplied {
		t.Errorf("approved dedup outcome = %q, want applied", dup.Outcome)
	}
	if hy, ok := mutations[f.ids.hygiene]; !ok || hy.Outcome != workmgmt.GroomingOutcomeSkipped || hy.SkipReason != workmgmt.GroomingSkipNotApproved {
		t.Errorf("rejected hygiene row = %+v (present=%v), want skipped/not_approved", hy, ok)
	}

	// The mutator was dialed for the dedup close, and NOT with a hygiene entry it
	// was told to reject.
	dialedDup := false
	for _, id := range f.mutator.dialedEntryIDs() {
		if id == f.ids.duplicate {
			dialedDup = true
		}
		if id == f.ids.hygiene {
			t.Errorf("mutator dialed the REJECTED hygiene entry %q", id)
		}
	}
	if !dialedDup {
		t.Errorf("mutator was never dialed for the approved dedup entry %q", f.ids.duplicate)
	}

	// The window is now closed: a further capture is refused.
	if w := postGD(t, f.s, f.runID, gdBatch(f.ids.ordering, "approved"), gdOperator); w.Code != http.StatusConflict {
		t.Errorf("post-settlement capture status = %d, want 409 grooming_window_closed: %s", w.Code, w.Body.String())
	}
}

// TestRecordGroomingDispositions_RefusedAfterWindowClosed pins the handler's 409
// branch AND the batch core's refusal through the real capability: with a
// watermark seeded BY CONSTRUCTION, a capture records nothing and returns 409.
//
// COUNTERFACTUAL (409 branch in the capture handler / watermark scan in the batch
// core): reads committed state after the call — zero rows — so the assertion
// lands on behaviour, not on the error bytes.
func TestRecordGroomingDispositions_RefusedAfterWindowClosed(t *testing.T) {
	f := newGWPGFixture(t)
	// Seed a watermark for the report artifact directly.
	wp, _ := json.Marshal(groomingWindowPayload{
		RunID: f.runID.String(), StageID: f.stage.ID.String(),
		ArtifactID: f.artID.String(), Settlement: "approved",
		ClosedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, Timestamp: time.Now().UTC(),
		Category: audit.GroomingApplyWindowClosedCategory, Payload: wp,
	}); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	w := postGD(t, f.s, f.runID, gdBatch(f.ids.ordering, "approved"), gdOperator)
	requireGDError(t, w, http.StatusConflict, "grooming_window_closed")

	if rows := f.rowsOf(t, CategoryGroomingDispositionRecorded); len(rows) != 0 {
		t.Errorf("committed disposition rows = %d, want 0 — a closed window records nothing", len(rows))
	}
}

// TestGroomingWindow_MultiEntryCaptureVsApprovalSettlement races a 3-entry HTTP
// capture against the on-approval settlement and asserts the capture either
// landed all three (and they were CONSUMED) or was refused 409 having landed
// ZERO — never a partial. Multi-entry is required to observe a split.
//
// The consumed half is observed through the mutation outcomes, not the row count:
// the raced dispositions carry verdicts that DIFFER from each entry's
// undispositioned default (hygiene/dependency default to a synthesized `approved`;
// an undispositioned gated `ordering` gets no decision at all), so a consumed
// disposition yields a distinct skip reason. An off-by-one that committed the
// rows but left them inert would fall to the defaults and redden the outcome
// assertions while the never-partial row count stayed green.
func TestGroomingWindow_MultiEntryCaptureVsApprovalSettlement(t *testing.T) {
	const rounds = 10
	stagger := timescale.D(3 * time.Millisecond)
	for i := 0; i < rounds; i++ {
		f := newGWPGFixture(t)
		if _, err := f.appr.Submit(context.Background(), approval.SubmitParams{
			StageID: f.stage.ID, ApproverSubject: "kuhlman-labs", Decision: approval.DecisionApprove, Surface: approval.SurfaceAPI,
		}); err != nil {
			t.Fatalf("round %d: submit approval: %v", i, err)
		}
		body := fmt.Sprintf(`{"dispositions":[
		  {"entry_id":%q,"verdict":"rejected"},
		  {"entry_id":%q,"verdict":"rejected"},
		  {"entry_id":%q,"verdict":"amended"}
		]}`, f.ids.hygiene, f.ids.dependency, f.ids.ordering)

		// Both operations are always in flight and contend on the run-row lock; the
		// alternating stagger only decides which acquires it first, so the
		// capture-wins (200) arm — where the consumed set is asserted — is exercised
		// deterministically every other round instead of at the scheduler's mercy.
		captureFirst := i%2 == 0
		var wg sync.WaitGroup
		var w *httptest.ResponseRecorder
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if !captureFirst {
				time.Sleep(stagger)
			}
			w = postGDNoT(f.s, f.runID, body, gdOperator)
		}()
		go func() {
			defer wg.Done()
			<-start
			if captureFirst {
				time.Sleep(stagger)
			}
			f.s.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionApprove)
		}()
		close(start)
		wg.Wait()

		rows := f.rowsOf(t, CategoryGroomingDispositionRecorded)
		switch w.Code {
		case http.StatusOK:
			if len(rows) != 3 {
				t.Errorf("round %d: capture 200 but %d disposition rows committed, want 3 — never a partial", i, len(rows))
			}
			// The capture landed before the settlement, so all three dispositions
			// were consumed: each entry's outcome reflects its recorded verdict, not
			// its undispositioned default.
			out := f.mutationOutcomes(t)
			wantSkip := map[string]string{
				f.ids.hygiene:    workmgmt.GroomingSkipNotApproved,
				f.ids.dependency: workmgmt.GroomingSkipNotApproved,
				f.ids.ordering:   workmgmt.GroomingSkipAmended,
			}
			for id, reason := range wantSkip {
				rec, ok := out[id]
				if !ok {
					t.Errorf("round %d: entry %q captured but absent from mutation outcomes — its disposition was not consumed", i, id)
					continue
				}
				if rec.Outcome != workmgmt.GroomingOutcomeSkipped || rec.SkipReason != reason {
					t.Errorf("round %d: entry %q outcome = %s/%s, want skipped/%s — its disposition was not consumed (fell to the undispositioned default)",
						i, id, rec.Outcome, rec.SkipReason, reason)
				}
			}
		case http.StatusConflict:
			if len(rows) != 0 {
				t.Errorf("round %d: capture 409 but %d disposition rows committed, want 0 — never a partial", i, len(rows))
			}
		default:
			t.Errorf("round %d: capture status = %d, want 200 or 409", i, w.Code)
		}
	}
}

// TestGroomingWindow_MultiEntryCaptureVsRejectionSettlement is the sibling of the
// approval-settlement race: rejection settles the window just as decisively as
// approval (defect 2), so a 3-entry capture racing a REJECT decision must still
// land all three or be refused 409 having landed ZERO — never a partial. The
// reject path settles in C1 before any ratification, so no approval is submitted.
func TestGroomingWindow_MultiEntryCaptureVsRejectionSettlement(t *testing.T) {
	const rounds = 10
	stagger := timescale.D(3 * time.Millisecond)
	for i := 0; i < rounds; i++ {
		f := newGWPGFixture(t)
		body := fmt.Sprintf(`{"dispositions":[
		  {"entry_id":%q,"verdict":"approved"},
		  {"entry_id":%q,"verdict":"approved"},
		  {"entry_id":%q,"verdict":"amended"}
		]}`, f.ids.hygiene, f.ids.dependency, f.ids.ordering)

		// Deterministic alternating bias (see the approval sibling): both operations
		// contend on the run-row lock; the stagger only picks the winner so the
		// capture-wins (200) arm and its below-watermark re-derivation run every
		// other round.
		captureFirst := i%2 == 0
		var wg sync.WaitGroup
		var w *httptest.ResponseRecorder
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if !captureFirst {
				time.Sleep(stagger)
			}
			w = postGDNoT(f.s, f.runID, body, gdOperator)
		}()
		go func() {
			defer wg.Done()
			<-start
			if captureFirst {
				time.Sleep(stagger)
			}
			f.s.applyApprovedGrooming(context.Background(), f.stage, approval.DecisionReject)
		}()
		close(start)
		wg.Wait()

		rows := f.rowsOf(t, CategoryGroomingDispositionRecorded)
		switch w.Code {
		case http.StatusOK:
			if len(rows) != 3 {
				t.Errorf("round %d: capture 200 but %d disposition rows committed, want 3 — never a partial", i, len(rows))
			}
			// A reject dispatches nothing, so the consumed set is NOT surfaced
			// through mutation outcomes as it is on the approval path; re-derive it
			// instead. The settlement wrote exactly one watermark, and all three
			// committed rows must sit strictly BELOW it — i.e. inside the range the
			// settlement swept — with every raced entry id accounted for. An
			// off-by-one that committed a boundary row outside the swept range
			// reddens here.
			wms := f.rowsOf(t, audit.GroomingApplyWindowClosedCategory)
			if len(wms) != 1 {
				t.Fatalf("round %d: window rows = %d, want exactly 1", i, len(wms))
			}
			wmSeq := wms[0].Sequence
			seen := map[string]bool{}
			for _, r := range rows {
				var dp groomingDispositionPayload
				if err := json.Unmarshal(r.Payload, &dp); err != nil {
					t.Fatalf("round %d: decode disposition: %v", i, err)
				}
				if r.Sequence >= wmSeq {
					t.Errorf("round %d: disposition %q at seq %d is not below the watermark seq %d — outside the consumed set",
						i, dp.EntryID, r.Sequence, wmSeq)
				}
				seen[dp.EntryID] = true
			}
			for _, id := range []string{f.ids.hygiene, f.ids.dependency, f.ids.ordering} {
				if !seen[id] {
					t.Errorf("round %d: entry %q captured but absent from the swept (below-watermark) set", i, id)
				}
			}
		case http.StatusConflict:
			if len(rows) != 0 {
				t.Errorf("round %d: capture 409 but %d disposition rows committed, want 0 — never a partial", i, len(rows))
			}
		default:
			t.Errorf("round %d: capture status = %d, want 200 or 409", i, w.Code)
		}
		// A reject dispatches nothing regardless of the interleaving.
		if dialed := f.mutator.dialedEntryIDs(); len(dialed) != 0 {
			t.Errorf("round %d: mutator dialed %v on a REJECT settlement", i, dialed)
		}
	}
}
