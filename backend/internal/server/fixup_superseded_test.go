package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// E86.54 / #4391 slice 1: the fifth delivered-nothing signal in
// fixupRefundedPasses — a CONFIRMED stage_fixup_superseded marker.

// supersessionRefundServer wires a Server whose only dependency is the audit
// fake fixupRefundedPasses reads.
func supersessionRefundServer(t *testing.T) (*Server, *auditFake) {
	t.Helper()
	au := newAuditFake()
	return New(Config{Addr: "127.0.0.1:0", AuditRepo: au}), au
}

// seedSupersessionRow seeds one chain row on stage at an explicit sequence.
func seedSupersessionRow(au *auditFake, runID, stageID uuid.UUID, category string, seq int64, payload map[string]any) {
	sid := stageID
	au.mu.Lock()
	defer au.mu.Unlock()
	seedHeadEntry(au, runID, &sid, category, seq, payload)
}

// seedD6Trigger seeds the D6 route's stage_fixup_triggered at seq.
func seedD6Trigger(au *auditFake, runID, stageID uuid.UUID, seq int64) {
	seedSupersessionRow(au, runID, stageID, CategoryStageFixupTriggered, seq, map[string]any{
		"delegated": mergeCandidateDelegatedRule, "prior_state": string(run.StageStateSucceeded), "concern_ids": []string{}})
}

// seedSupersessionMarker seeds a stage_fixup_superseded row naming trigger tSeq.
func seedSupersessionMarker(au *auditFake, runID, stageID uuid.UUID, seq, tSeq int64) {
	seedSupersessionRow(au, runID, stageID, CategoryStageFixupSuperseded, seq, map[string]any{
		"trigger_sequence": tSeq, "superseded_by": fixupSupersededByVerifyRerun,
		"failed_verdict_sequence": tSeq - 1, "reason": mergeCandidateRerunReasonPrefix + "infra"})
}

// seedConversionTrigger seeds a stage_merge_candidate_verify_triggered row; a
// positive supersedes names the fix-up trigger it converts, zero omits the
// field (another producer's trigger).
func seedConversionTrigger(au *auditFake, runID, stageID uuid.UUID, seq, supersedes int64) {
	p := map[string]any{
		"branch": mcvBranch, "base_ref": mcvBaseRef, "expected_head_sha": mcvHead,
		"cause": mergeCandidateCauseBaseAdvance, "verify_command": mcvCommand,
		"prior_state": string(run.StageStateSucceeded),
	}
	if supersedes > 0 {
		p["supersedes_fixup_trigger_sequence"] = supersedes
	}
	seedSupersessionRow(au, runID, stageID, CategoryStageMergeCandidateVerifyTriggered, seq, p)
}

// TestFixupRefundedPasses_ConfirmedSupersession (R1): a D6 trigger, its
// marker and the conversion trigger naming it refund the pass, and the refund
// is not a crash. MECHANISM (CF): dropping the superseded signal (the
// markers/conversions block in fixupRefundedPasses) leaves no signal in the
// trigger's window, so refunded reads 0.
func TestFixupRefundedPasses_ConfirmedSupersession(t *testing.T) {
	s, au := supersessionRefundServer(t)
	runID, stageID := uuid.New(), uuid.New()
	seedD6Trigger(au, runID, stageID, 10)
	seedSupersessionMarker(au, runID, stageID, 11, 10)
	seedConversionTrigger(au, runID, stageID, 12, 10)

	refunded, crashed, err := s.fixupRefundedPasses(context.Background(), runID, stageID)
	if err != nil {
		t.Fatalf("fixupRefundedPasses: %v", err)
	}
	if refunded != 1 || crashed != 0 {
		t.Errorf("refunded = %d, crashedWithoutPush = %d; want 1, 0 (a confirmed supersession refunds, not a crash)", refunded, crashed)
	}
}

// TestFixupRefundedPasses_InertMarkerNotRefunded (R2): a marker no conversion
// trigger confirms is never a supersession. MECHANISM (CF): counting every
// marker without confirmedFixupSupersessions refunds subtest (a) — its only
// other row, a category-B recovery, is no refund signal, so the confirmation
// is the only control in the path. Subtest (b) stays masked by the push veto,
// which is why (a) isolates the control. Subtest (d) isolates the per-stage
// filter: a confirmed supersession on ANOTHER stage sits inside this stage's
// window.
func TestFixupRefundedPasses_InertMarkerNotRefunded(t *testing.T) {
	cases := map[string]func(au *auditFake, runID, stageID uuid.UUID){
		"a marker then dispatch then category-B recovery": func(au *auditFake, runID, stageID uuid.UUID) {
			seedSupersessionMarker(au, runID, stageID, 11, 10)
			seedSupersessionRow(au, runID, stageID, CategoryStageConcurrencyAdmitted, 12, map[string]any{"group": "local-implement:h1"})
			seedSupersessionRow(au, runID, stageID, CategoryStageFixupRecovered, 13, map[string]any{"source_failure_category": string(run.FailureB)})
		},
		"b marker then fixup_pushed": func(au *auditFake, runID, stageID uuid.UUID) {
			seedSupersessionMarker(au, runID, stageID, 11, 10)
			seedSupersessionRow(au, runID, stageID, CategoryFixupPushed, 12, map[string]any{"head_sha": "abc"})
		},
		"c marker then dispatch then push then a fieldless trigger": func(au *auditFake, runID, stageID uuid.UUID) {
			seedSupersessionMarker(au, runID, stageID, 11, 10)
			seedSupersessionRow(au, runID, stageID, CategoryStageConcurrencyAdmitted, 12, map[string]any{"group": "local-implement:h1"})
			seedSupersessionRow(au, runID, stageID, CategoryFixupPushed, 13, map[string]any{"head_sha": "abc"})
			seedConversionTrigger(au, runID, stageID, 14, 0)
		},
		"d confirmed on another stage": func(au *auditFake, runID, _ uuid.UUID) {
			other := uuid.New()
			seedSupersessionMarker(au, runID, other, 11, 10)
			seedConversionTrigger(au, runID, other, 12, 10)
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			s, au := supersessionRefundServer(t)
			runID, stageID := uuid.New(), uuid.New()
			seedD6Trigger(au, runID, stageID, 10)
			seed(au, runID, stageID)

			refunded, crashed, err := s.fixupRefundedPasses(context.Background(), runID, stageID)
			if err != nil {
				t.Fatalf("fixupRefundedPasses: %v", err)
			}
			if refunded != 0 || crashed != 0 {
				t.Errorf("refunded = %d, crashedWithoutPush = %d; want 0, 0 (an inert marker is never a supersession)", refunded, crashed)
			}
		})
	}
}

// TestFixupRefundedPasses_SupersededReadErrorFailsClosed (R3): a read failure
// on either new category is returned, never a smaller refund. MECHANISM (CF):
// treating the read error as "no markers" (or "no conversions") returns nil,
// and the error assertion fails. The conversion read is reached ONLY when a
// marker exists, so a run without markers is untouched by its failure.
func TestFixupRefundedPasses_SupersededReadErrorFailsClosed(t *testing.T) {
	for _, cat := range []string{CategoryStageFixupSuperseded, CategoryStageMergeCandidateVerifyTriggered} {
		t.Run(cat, func(t *testing.T) {
			s, au := supersessionRefundServer(t)
			runID, stageID := uuid.New(), uuid.New()
			seedD6Trigger(au, runID, stageID, 10)
			seedSupersessionMarker(au, runID, stageID, 11, 10)
			seedConversionTrigger(au, runID, stageID, 12, 10)
			au.listByCategoryErrCategory = cat

			refunded, _, err := s.fixupRefundedPasses(context.Background(), runID, stageID)
			if err == nil {
				t.Fatalf("err = nil (refunded %d), want the %s read error returned", refunded, cat)
			}
			if !strings.Contains(err.Error(), cat) {
				t.Errorf("err = %v, want it to name %s", err, cat)
			}
		})
	}
	t.Run("no marker skips the conversion read", func(t *testing.T) {
		s, au := supersessionRefundServer(t)
		runID, stageID := uuid.New(), uuid.New()
		seedD6Trigger(au, runID, stageID, 10)
		seedSupersessionRow(au, runID, stageID, CategoryFixupNoChanges, 11, map[string]any{})
		au.listByCategoryErrCategory = CategoryStageMergeCandidateVerifyTriggered

		refunded, _, err := s.fixupRefundedPasses(context.Background(), runID, stageID)
		if err != nil || refunded != 1 {
			t.Fatalf("refunded = %d, err = %v; want 1, nil (no marker, so the conversion read is never made)", refunded, err)
		}
	})
}

// TestSupersededFixup_RefundAdmitsNextRouteAfterRealConversion (R4,
// cross-layer): a real failed report through the mux routes the first D6
// fix-up; slice 0's verb arm converts it in place; a FAILED report for the
// converted pass, again through the mux, routes a SECOND D6 fix-up because
// the superseded pass is refunded against the one-pass budget
// (defaultMaxFixupPasses = 1). MECHANISM (CF): dropping the superseded signal
// from fixupRefundedPasses leaves prior 1, refunded 0, and the second route is
// refused "fixup budget exhausted".
func TestSupersededFixup_RefundAdmitsNextRouteAfterRealConversion(t *testing.T) {
	f := newMCVShipFixture(t, mcvDelegatedV0SpecYAML)
	// A locked-local run: the D6 route's own Orchestrator.Advance parks the
	// re-opened implement stage at awaiting_host_dispatch instead of
	// dispatching it, which is the queued-fix-up shape a re-run converts.
	f.repo.mu.Lock()
	f.repo.runs[f.runID].RunnerKind = run.RunnerKindLocal
	f.repo.runs[f.runID].RunnerKindResolved = true
	f.repo.mu.Unlock()
	ctx := context.Background()

	f.startRunning(t, mcvHead)
	w, resp := f.ship(t, mcvReportBody(mcvHead, "failed", "", "FAIL: TestX (load-starved)"))
	if w.Code != http.StatusOK || !resp.FixupRouted {
		t.Fatalf("first failed report: status %d routed %v refusal %q; want the first D6 fix-up routed", w.Code, resp.FixupRouted, resp.FixupRefusal)
	}
	if st := f.stageState(t, f.impl.ID); st != run.StageStatePending && st != run.StageStateAwaitingHostDispatch {
		t.Fatalf("fixture: implement stage = %q after the D6 route, want pending or awaiting_host_dispatch (a queued fix-up)", st)
	}

	impl, err := f.repo.GetStage(ctx, f.impl.ID)
	if err != nil {
		t.Fatal(err)
	}
	conv := f.s.rerunQueuedMergeCandidateFixup(ctx, f.runRow(t), impl,
		mcvBranch, mcvBaseRef, mcvHead, mergeCandidateCauseBaseAdvance, "infra")
	if !conv.FixupSuperseded || conv.State != mergeCandidateStateInFlight {
		t.Fatalf("conversion: state %q superseded %v refusal %q; want the fix-up converted in place", conv.State, conv.FixupSuperseded, conv.Refusal)
	}

	// The converted pass runs and fails again.
	f.repo.mu.Lock()
	f.impl.State = run.StageStateRunning
	f.repo.mu.Unlock()
	w, resp = f.ship(t, mcvReportBody(mcvHead, "failed", "", "FAIL: TestX (load-starved again)"))
	if w.Code != http.StatusOK {
		t.Fatalf("second failed report: status %d:\n%s", w.Code, w.Body.String())
	}
	if !resp.FixupRouted {
		t.Fatalf("second failed report routed no fix-up (refusal %q); want a second D6 fix-up admitted by the supersession refund", resp.FixupRefusal)
	}
	rows := auditEntries(f.au, CategoryStageFixupTriggered)
	if len(rows) != 2 {
		t.Fatalf("stage_fixup_triggered rows = %d, want 2", len(rows))
	}
	payload := string(rows[1].Payload)
	if !strings.Contains(payload, `"pass_ordinal":2`) || !strings.Contains(payload, `"refunded_passes":1`) {
		t.Errorf("second D6 trigger payload = %s, want pass_ordinal 2 (prior 1) and refunded_passes 1", payload)
	}
}
