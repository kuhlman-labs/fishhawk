package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ---------------------------------------------------------------------------
// E72.1 / #3325: acceptance-stage omission at plan approval.
//
// Every fake here is FILE-LOCAL so approvals_test.go stays untouched: the
// omission hook is reachable only from an approved plan declaring
// acceptance_surface: none, which no fixture in that file ships.
// ---------------------------------------------------------------------------

// omissionRunRepo embeds the in-package orchestratorRepo (so the approve-advance
// CAS, TransitionRun and ListStagesForRun all behave) and adds the OPTIONAL
// DeletePendingAcceptanceStage capability the hook asserts. It removes the
// stage from both maps under the same predicates the SQL carries, records every
// call, and returns an injectable error.
type omissionRunRepo struct {
	*orchestratorRepo
	deleteErr   error
	deleteCalls []uuid.UUID
}

func (r *omissionRunRepo) DeletePendingAcceptanceStage(_ context.Context, id uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleteCalls = append(r.deleteCalls, id)
	if r.deleteErr != nil {
		return false, r.deleteErr
	}
	st, ok := r.stagesByID[id]
	if !ok || st.Type != run.StageTypeAcceptance || st.State != run.StageStatePending {
		return false, nil
	}
	delete(r.stagesByID, id)
	rows := r.stagesByRunID[st.RunID]
	kept := rows[:0]
	for _, s := range rows {
		if s.ID != id {
			kept = append(kept, s)
		}
	}
	r.stagesByRunID[st.RunID] = kept
	return true, nil
}

func (r *omissionRunRepo) deleteCallsSnapshot() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uuid.UUID(nil), r.deleteCalls...)
}

// readCountingAuditFake wraps auditFake and counts ListForRunByCategory calls
// per category, so the drivable-plan control can assert the hook performed NO
// omission-marker read (not merely that it wrote nothing).
type readCountingAuditFake struct {
	*auditFake
	rmu   sync.Mutex
	reads map[string]int
}

func (a *readCountingAuditFake) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.rmu.Lock()
	a.reads[category]++
	a.rmu.Unlock()
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

func (a *readCountingAuditFake) readsOf(category string) int {
	a.rmu.Lock()
	defer a.rmu.Unlock()
	return a.reads[category]
}

// omissionHarness is the approval-path fixture: a Server wired the way
// newBudgetCheckServer is, but with the omission-capable run repo and an audit
// fake whose ListForRunByCategory WORKS (served from its appended rows).
type omissionHarness struct {
	s      *Server
	rr     *omissionRunRepo
	au     *readCountingAuditFake
	art    *fakeArtifactRepo
	logBuf *bytes.Buffer
	run    *run.Run
	plan   *run.Stage
	impl   *run.Stage
	acc    *run.Stage
}

// newOmissionHarness seeds a run with plan(seq 0, awaiting_approval) +
// implement(seq 1, pending) + acceptance(seq 2, pending) and the given approved
// plan artifact on the plan stage. The Orchestrator carries Artifacts so the
// control case can drive TryShortCircuitAcceptance through the same wiring.
func newOmissionHarness(t *testing.T, p *plan.Plan) *omissionHarness {
	t.Helper()
	art := newFakeArtifactRepo()
	inner := newOrchestratorRepo()
	rr := &omissionRunRepo{orchestratorRepo: inner}
	au := &readCountingAuditFake{auditFake: newAuditFake(), reads: map[string]int{}}
	logBuf := &bytes.Buffer{}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		ApprovalRepo: newFakeApprovalRepo(),
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr, Artifacts: art},
		ArtifactRepo: art,
		Logger:       slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	r, planStage := seedBudgetRun(t, inner, art, p)
	impl := inner.seedStage(r.ID, 1, run.StageStatePending)
	impl.Type = run.StageTypeImplement
	acc := inner.seedStage(r.ID, 2, run.StageStatePending)
	acc.Type = run.StageTypeAcceptance
	return &omissionHarness{s: s, rr: rr, au: au, art: art, logBuf: logBuf, run: r, plan: planStage, impl: impl, acc: acc}
}

func (h *omissionHarness) approve(t *testing.T) {
	t.Helper()
	w := submitApproval(t, h.s, h.plan.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// acceptanceRow returns the run's acceptance stage row as ListStagesForRun
// reports it (committed state), or nil when no such row exists.
func (h *omissionHarness) acceptanceRow(t *testing.T) *run.Stage {
	t.Helper()
	stages, err := h.rr.ListStagesForRun(context.Background(), h.run.ID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	return acceptanceStageOf(stages)
}

// omissionMarkers returns the acceptance_stage_omitted rows the audit fake
// captured for the run, decoded.
func (h *omissionHarness) omissionMarkers(t *testing.T) []acceptanceStageOmittedPayload {
	t.Helper()
	h.au.mu.Lock()
	defer h.au.mu.Unlock()
	var out []acceptanceStageOmittedPayload
	for _, ap := range h.au.appended {
		if ap.RunID != h.run.ID || ap.Category != CategoryAcceptanceStageOmitted {
			continue
		}
		if ap.StageID == nil || *ap.StageID != h.plan.ID {
			t.Errorf("acceptance_stage_omitted row scoped to stage %v, want the PLAN stage %s (audit_entries.stage_id is ON DELETE RESTRICT; the marker must outlive the acceptance stage)", ap.StageID, h.plan.ID)
		}
		if ap.ActorKind == nil || *ap.ActorKind != audit.ActorKind("system") {
			t.Errorf("acceptance_stage_omitted actor_kind = %v, want system", ap.ActorKind)
		}
		var p acceptanceStageOmittedPayload
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("unmarshal acceptance_stage_omitted payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// surfaceNonePlan is the legal none shape: zero criteria, an out_of_scope
// justification, and the declaration.
func surfaceNonePlan() *plan.Plan {
	return &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 5,
		Verification: plan.Verification{
			TestStrategy:      "go test ./...",
			RollbackPlan:      "revert",
			AcceptanceSurface: plan.AcceptanceSurfaceValueNone,
			OutOfScope:        []string{"a comment-only change has no observable surface", "second"},
		},
	}
}

// drivablePlan is the control: one drivable criterion naming an HTTP surface
// and NO acceptance_surface declaration.
func drivablePlan() *plan.Plan {
	return &plan.Plan{
		PlanVersion:             "standard_v1",
		PredictedRuntimeMinutes: 5,
		Verification: plan.Verification{
			TestStrategy: "go test ./...",
			RollbackPlan: "revert",
			AcceptanceCriteria: []plan.AcceptanceCriterion{{
				ID: "c1", Statement: "the run status reports the state", Source: "explicit", SourceRef: "#3325",
				VerifyHint: "GET /v0/runs/{run_id} returns 200",
			}},
		},
	}
}

// (a) TestApprovePlan_AcceptanceSurfaceNone_OmitsAcceptanceStage is the
// done-means, asserted on COMMITTED state after the handler returns: the
// acceptance stage row is gone and exactly one acceptance_stage_omitted marker
// scoped to the plan stage exists with the expected payload. Counterfactual:
// delete the omitAcceptanceStageForSurfaceNone call in finishApprovalAdvance →
// the row survives and no marker exists.
func TestApprovePlan_AcceptanceSurfaceNone_OmitsAcceptanceStage(t *testing.T) {
	h := newOmissionHarness(t, surfaceNonePlan())
	h.approve(t)

	if row := h.acceptanceRow(t); row != nil {
		t.Fatalf("acceptance stage %s still present (state %s) after approving a none plan; want it deleted", row.ID, row.State)
	}
	markers := h.omissionMarkers(t)
	if len(markers) != 1 {
		t.Fatalf("acceptance_stage_omitted rows = %d, want exactly 1", len(markers))
	}
	m := markers[0]
	if m.OmittedStageID != h.acc.ID || m.OmittedSequence != 2 || m.Basis != acceptanceStageOmissionBasis ||
		m.CriteriaTotal != 0 || m.OutOfScopeCount != 2 {
		t.Errorf("payload = %+v, want omitted_stage_id=%s omitted_sequence=2 basis=%s criteria_total=0 out_of_scope_count=2",
			m, h.acc.ID, acceptanceStageOmissionBasis)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 1 || calls[0] != h.acc.ID {
		t.Errorf("DeletePendingAcceptanceStage calls = %v, want exactly [%s]", calls, h.acc.ID)
	}
	// The plan stage itself advanced as usual — the hook never unwinds the
	// approval.
	if got, _ := h.rr.GetStage(context.Background(), h.plan.ID); got.State != run.StageStateSucceeded {
		t.Errorf("plan stage state = %s, want succeeded", got.State)
	}
}

// (a, second approval) TestApprovePlan_AcceptanceSurfaceNone_Idempotent: after
// a fully successful pass a re-approval finds no pending acceptance stage and
// writes nothing — no second marker, no second delete.
func TestApprovePlan_AcceptanceSurfaceNone_Idempotent(t *testing.T) {
	h := newOmissionHarness(t, surfaceNonePlan())
	h.approve(t)
	if len(h.omissionMarkers(t)) != 1 {
		t.Fatal("fixture: first approval did not record exactly one marker")
	}
	// A re-approval needs the plan stage back at its gate; drive the hook
	// directly the way a second finishApprovalAdvance would.
	h.s.omitAcceptanceStageForSurfaceNone(context.Background(), h.plan)

	if got := len(h.omissionMarkers(t)); got != 1 {
		t.Errorf("acceptance_stage_omitted rows after re-approval = %d, want 1 (no double append)", got)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 1 {
		t.Errorf("DeletePendingAcceptanceStage calls after re-approval = %d, want 1 (no second delete)", len(calls))
	}
}

// (b) TestApprovePlan_AcceptanceSurfaceNone_AppendFails_StageRetainedNoMarker
// is the DURABLE-RECORD-FIRST proof: when the marker append fails the stage is
// STILL PRESENT and pending, no marker exists, and the delete was never called.
// Swap the hook to delete-then-append and this goes RED (stage gone, no marker
// — the unrecoverable state the ordering exists to make unreachable).
func TestApprovePlan_AcceptanceSurfaceNone_AppendFails_StageRetainedNoMarker(t *testing.T) {
	h := newOmissionHarness(t, surfaceNonePlan())
	h.au.appendErrCategory = CategoryAcceptanceStageOmitted
	h.approve(t)

	row := h.acceptanceRow(t)
	if row == nil {
		t.Fatal("acceptance stage DELETED although the marker append failed: the hook is not marker-first")
	}
	if row.State != run.StageStatePending {
		t.Errorf("acceptance stage state = %s, want pending (untouched)", row.State)
	}
	if got := len(h.omissionMarkers(t)); got != 0 {
		t.Errorf("acceptance_stage_omitted rows = %d, want 0", got)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 0 {
		t.Errorf("DeletePendingAcceptanceStage calls = %v, want none when the append failed", calls)
	}
	if !strings.Contains(h.logBuf.String(), "append acceptance_stage_omitted failed") {
		t.Errorf("want a WARN naming the failed append; logs:\n%s", h.logBuf.String())
	}
}

// (c) TestApprovePlan_AcceptanceSurfaceNone_DeleteFails_MarkerPresent_ReapprovalCompletesWithoutDoubleAppend:
// a delete failure after a successful append leaves marker + stage (partial
// state ii); clearing the fault and re-approving completes the delete and
// leaves exactly ONE marker — the marker, not the stage's absence, anchors
// idempotency.
func TestApprovePlan_AcceptanceSurfaceNone_DeleteFails_MarkerPresent_ReapprovalCompletesWithoutDoubleAppend(t *testing.T) {
	h := newOmissionHarness(t, surfaceNonePlan())
	h.rr.deleteErr = errors.New("injected: RESTRICT violation")
	h.approve(t)

	if row := h.acceptanceRow(t); row == nil || row.State != run.StageStatePending {
		t.Fatalf("acceptance row after failed delete = %+v, want present and pending", row)
	}
	if got := len(h.omissionMarkers(t)); got != 1 {
		t.Fatalf("acceptance_stage_omitted rows after failed delete = %d, want 1 (append landed first)", got)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 1 {
		t.Fatalf("DeletePendingAcceptanceStage calls = %d, want 1", len(calls))
	}
	if !strings.Contains(h.logBuf.String(), "delete pending acceptance stage failed") {
		t.Errorf("want a WARN naming the failed delete; logs:\n%s", h.logBuf.String())
	}

	// Repair: clear the fault and re-run the hook as the next approval would.
	h.rr.deleteErr = nil
	h.s.omitAcceptanceStageForSurfaceNone(context.Background(), h.plan)

	if row := h.acceptanceRow(t); row != nil {
		t.Errorf("acceptance stage still present after the repairing re-approval: %+v", row)
	}
	if got := len(h.omissionMarkers(t)); got != 1 {
		t.Errorf("acceptance_stage_omitted rows after re-approval = %d, want exactly 1 (no double append)", got)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 2 {
		t.Errorf("DeletePendingAcceptanceStage calls = %d, want 2 (the retried idempotent delete)", len(calls))
	}
}

// (d) TestApprovePlan_AcceptanceSurfaceNone_MarkerReadError_FailsOpen: an
// omission-marker read error retains the stage, appends nothing and deletes
// nothing.
func TestApprovePlan_AcceptanceSurfaceNone_MarkerReadError_FailsOpen(t *testing.T) {
	h := newOmissionHarness(t, surfaceNonePlan())
	h.au.listByCategoryErrCategory = CategoryAcceptanceStageOmitted
	h.approve(t)

	if row := h.acceptanceRow(t); row == nil || row.State != run.StageStatePending {
		t.Fatalf("acceptance row = %+v, want present and pending on a marker read error", row)
	}
	if got := len(h.omissionMarkers(t)); got != 0 {
		t.Errorf("acceptance_stage_omitted rows = %d, want 0", got)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 0 {
		t.Errorf("DeletePendingAcceptanceStage calls = %v, want none", calls)
	}
	if !strings.Contains(h.logBuf.String(), "list omission markers failed") {
		t.Errorf("want a WARN naming the failed marker read; logs:\n%s", h.logBuf.String())
	}
}

// noOmitterRunRepo hides DeletePendingAcceptanceStage from the wrapped repo:
// embedding the run.Repository INTERFACE promotes only its own methods, so the
// optional capability genuinely does not resolve (mirrors noPredictionRunRepo).
type noOmitterRunRepo struct{ run.Repository }

// (e) TestApprovePlan_AcceptanceSurfaceNone_RepoWithoutOmitter_FailsOpen: a
// RunRepo lacking the capability retains the stage, writes no marker and WARNs.
func TestApprovePlan_AcceptanceSurfaceNone_RepoWithoutOmitter_FailsOpen(t *testing.T) {
	art := newFakeArtifactRepo()
	inner := newOrchestratorRepo()
	hidden := noOmitterRunRepo{Repository: &omissionRunRepo{orchestratorRepo: inner}}
	if _, ok := run.Repository(hidden).(acceptanceStageOmitter); ok {
		t.Fatal("noOmitterRunRepo unexpectedly satisfies acceptanceStageOmitter")
	}
	au := newAuditFake()
	logBuf := &bytes.Buffer{}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		ApprovalRepo: newFakeApprovalRepo(),
		RunRepo:      hidden,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: hidden},
		ArtifactRepo: art,
		Logger:       slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	r, planStage := seedBudgetRun(t, inner, art, surfaceNonePlan())
	impl := inner.seedStage(r.ID, 1, run.StageStatePending)
	impl.Type = run.StageTypeImplement
	acc := inner.seedStage(r.ID, 2, run.StageStatePending)
	acc.Type = run.StageTypeAcceptance

	w := submitApproval(t, s, planStage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	stages, _ := inner.ListStagesForRun(context.Background(), r.ID)
	if row := acceptanceStageOf(stages); row == nil || row.State != run.StageStatePending {
		t.Fatalf("acceptance row = %+v, want present and pending when the repo lacks the capability", row)
	}
	au.mu.Lock()
	for _, ap := range au.appended {
		if ap.Category == CategoryAcceptanceStageOmitted {
			t.Error("acceptance_stage_omitted appended although no delete capability exists")
		}
	}
	au.mu.Unlock()
	if !strings.Contains(logBuf.String(), "repo lacks DeletePendingAcceptanceStage") {
		t.Errorf("want a WARN naming the missing capability; logs:\n%s", logBuf.String())
	}
}

// (f) TestApprovePlan_AcceptanceSurfaceNone_NonPendingAcceptance_Untouched: an
// acceptance stage that already settled is not an omission candidate — no
// marker, no delete.
func TestApprovePlan_AcceptanceSurfaceNone_NonPendingAcceptance_Untouched(t *testing.T) {
	h := newOmissionHarness(t, surfaceNonePlan())
	h.acc.State = run.StageStateSucceeded // seeded BY CONSTRUCTION, not via the transition table
	h.approve(t)

	if row := h.acceptanceRow(t); row == nil || row.State != run.StageStateSucceeded {
		t.Fatalf("acceptance row = %+v, want present and succeeded (untouched)", row)
	}
	if got := len(h.omissionMarkers(t)); got != 0 {
		t.Errorf("acceptance_stage_omitted rows = %d, want 0", got)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 0 {
		t.Errorf("DeletePendingAcceptanceStage calls = %v, want none", calls)
	}
}

// (g) TestApprovePlan_DrivableCriterion_RetainsAcceptanceStage is the CONTROL:
// a plan with one drivable criterion and no declaration keeps its pending
// acceptance stage, records no marker, performs NO omission-marker audit read,
// and the orchestrator's TryShortCircuitAcceptance on that stage reports
// liveValidationRequired=true with no short-circuit — the stage 'actually
// spawns' path is untouched.
func TestApprovePlan_DrivableCriterion_RetainsAcceptanceStage(t *testing.T) {
	h := newOmissionHarness(t, drivablePlan())
	h.approve(t)

	row := h.acceptanceRow(t)
	if row == nil || row.State != run.StageStatePending {
		t.Fatalf("acceptance row = %+v, want present and pending for a drivable plan", row)
	}
	if got := len(h.omissionMarkers(t)); got != 0 {
		t.Errorf("acceptance_stage_omitted rows = %d, want 0", got)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 0 {
		t.Errorf("DeletePendingAcceptanceStage calls = %v, want none", calls)
	}
	if n := h.au.readsOf(CategoryAcceptanceStageOmitted); n != 0 {
		t.Errorf("omission-marker ListForRunByCategory reads = %d, want 0 (the hook must return before any audit read on a non-none plan)", n)
	}

	sc, liveValidationRequired, err := h.s.cfg.Orchestrator.TryShortCircuitAcceptance(context.Background(), h.run.ID, row.ID)
	if err != nil {
		t.Fatalf("TryShortCircuitAcceptance: %v", err)
	}
	if sc != nil || !liveValidationRequired {
		t.Errorf("TryShortCircuitAcceptance = (%+v, live=%v), want (nil, true): the drivable stage must spawn, not short-circuit", sc, liveValidationRequired)
	}
}

// (h) TestApprovePlan_Reject_DoesNotOmit: a reject decision on a none plan
// never reaches the hook.
func TestApprovePlan_Reject_DoesNotOmit(t *testing.T) {
	h := newOmissionHarness(t, surfaceNonePlan())
	w := submitApproval(t, h.s, h.plan.ID, `{"decision":"reject","comment":"wrong fork"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reject status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := len(h.omissionMarkers(t)); got != 0 {
		t.Errorf("acceptance_stage_omitted rows = %d, want 0 on a reject", got)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 0 {
		t.Errorf("DeletePendingAcceptanceStage calls = %v, want none on a reject", calls)
	}
	if n := h.au.readsOf(CategoryAcceptanceStageOmitted); n != 0 {
		t.Errorf("omission-marker reads = %d, want 0 on a reject", n)
	}
}

// TestAcceptanceSeam_SurfaceNone_OmittedRunClearsMergeGate crosses
// approval-write → gate-read on the SAME stores: after (a), acceptanceGateState
// for a run whose workflow spec declares an acceptance stage resolves the
// merge-eligible acceptanceGateOmitted disposition — not acceptance_pending —
// and acceptanceGateAdmitsMerge admits it.
func TestAcceptanceSeam_SurfaceNone_OmittedRunClearsMergeGate(t *testing.T) {
	h := newOmissionHarness(t, surfaceNonePlan())
	h.approve(t)
	if h.acceptanceRow(t) != nil || len(h.omissionMarkers(t)) != 1 {
		t.Fatal("fixture: approval did not omit the stage with one marker")
	}

	stages, err := h.rr.ListStagesForRun(context.Background(), h.run.ID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	runRow := acceptanceGateRun(h.run.ID, specWithAcceptanceStage)
	got, err := h.s.acceptanceGateState(context.Background(), runRow, stages)
	if err != nil {
		t.Fatalf("acceptanceGateState: %v", err)
	}
	if got != acceptanceGateOmitted {
		t.Errorf("acceptanceGateState = %q, want %q after an approval-driven omission", got, acceptanceGateOmitted)
	}
	if !acceptanceGateAdmitsMerge(got) {
		t.Errorf("acceptanceGateAdmitsMerge(%q) = false, want true", got)
	}
}

// (i) TestApprovePlan_AcceptanceSurfaceNone_PlanLoadError_FailsOpen: a store
// fault while loading the approved plan retains the stage, writes no marker,
// performs no omission-marker read, and WARNs naming the failed load. The hook
// is driven directly (an artifact-list fault set before the HTTP approval would
// fail the budget check first, never reaching the hook). Counterfactual: delete
// the loadApprovedPlanForRun error branch → the nil plan falls through the
// "not a none plan" return silently and the WARN assertion goes RED.
func TestApprovePlan_AcceptanceSurfaceNone_PlanLoadError_FailsOpen(t *testing.T) {
	h := newOmissionHarness(t, surfaceNonePlan())
	h.art.listErr = errors.New("injected: artifact store unavailable")
	h.s.omitAcceptanceStageForSurfaceNone(context.Background(), h.plan)

	if row := h.acceptanceRow(t); row == nil || row.State != run.StageStatePending {
		t.Fatalf("acceptance row = %+v, want present and pending on a plan load error", row)
	}
	if got := len(h.omissionMarkers(t)); got != 0 {
		t.Errorf("acceptance_stage_omitted rows = %d, want 0", got)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 0 {
		t.Errorf("DeletePendingAcceptanceStage calls = %v, want none", calls)
	}
	if n := h.au.readsOf(CategoryAcceptanceStageOmitted); n != 0 {
		t.Errorf("omission-marker reads = %d, want 0 (the hook returns before any audit read when the plan cannot be loaded)", n)
	}
	if !strings.Contains(h.logBuf.String(), "load approved plan failed") {
		t.Errorf("want a WARN naming the failed plan load; logs:\n%s", h.logBuf.String())
	}
}

// (j) TestApprovePlan_AcceptanceSurfaceNone_CorruptMarkers_DoNotAnchorIdempotency:
// pre-existing acceptance_stage_omitted rows whose payload is undecodable or
// names uuid.Nil are seeded BY CONSTRUCTION into the audit fake's history. They
// cannot name a stage, so they must not anchor idempotency: the hook still
// appends exactly ONE well-formed marker for the pending stage and deletes it.
// Counterfactual: make acceptanceOmissionMarkers fail the whole read on an
// undecodable row (return the unmarshal error) → nothing is written, the stage
// survives, and the marker-count assertion goes RED.
func TestApprovePlan_AcceptanceSurfaceNone_CorruptMarkers_DoNotAnchorIdempotency(t *testing.T) {
	h := newOmissionHarness(t, surfaceNonePlan())
	rid := h.run.ID
	pid := h.plan.ID
	h.au.seeded = append(h.au.seeded,
		&audit.Entry{RunID: &rid, StageID: &pid, Category: CategoryAcceptanceStageOmitted, Payload: json.RawMessage(`{not json`)},
		&audit.Entry{RunID: &rid, StageID: &pid, Category: CategoryAcceptanceStageOmitted, Payload: json.RawMessage(`{"omitted_stage_id":"00000000-0000-0000-0000-000000000000","basis":"acceptance_surface_none"}`)},
	)
	marked, err := h.s.acceptanceOmissionMarkers(context.Background(), h.run.ID)
	if err != nil {
		t.Fatalf("acceptanceOmissionMarkers: %v (a corrupt row must be skipped, not fail the read)", err)
	}
	if len(marked) != 0 {
		t.Fatalf("marker set from corrupt rows = %v, want empty", marked)
	}

	h.approve(t)

	if row := h.acceptanceRow(t); row != nil {
		t.Fatalf("acceptance stage %s still present after approval with only corrupt markers; want it deleted", row.ID)
	}
	markers := h.omissionMarkers(t)
	if len(markers) != 1 || markers[0].OmittedStageID != h.acc.ID {
		t.Fatalf("well-formed acceptance_stage_omitted rows = %+v, want exactly one naming %s", markers, h.acc.ID)
	}
	if calls := h.rr.deleteCallsSnapshot(); len(calls) != 1 || calls[0] != h.acc.ID {
		t.Errorf("DeletePendingAcceptanceStage calls = %v, want exactly [%s]", calls, h.acc.ID)
	}
	// The well-formed marker now anchors idempotency alongside the corrupt
	// rows: a re-approval writes nothing further.
	h.s.omitAcceptanceStageForSurfaceNone(context.Background(), h.plan)
	if got := len(h.omissionMarkers(t)); got != 1 {
		t.Errorf("acceptance_stage_omitted rows after re-approval = %d, want 1", got)
	}
}
