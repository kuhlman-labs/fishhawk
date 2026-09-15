package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// cancelDropSeam is the fixture for the #3389 cancel-path drop writer: an
// orchestratorRepo carrying a plan stage (the one that recorded the approval)
// and an acceptance stage seeded in the state under test, an auditFake whose
// seeded approval_submitted row carries the approved retire_scenario entries,
// and a pageClassRecorder as the issue notifier so status refreshes are
// countable. The server's logger writes JSON into logBuf at DEBUG so a test
// can count a named WARN event.
type cancelDropSeam struct {
	s          *Server
	rr         *orchestratorRepo
	au         *auditFake
	rec        *pageClassRecorder
	logBuf     *bytes.Buffer
	runID      uuid.UUID
	planID     uuid.UUID
	acceptance *run.Stage // nil when the seam was built without one
}

// cancelDropRetired is the approved retirement every seam seeds; the
// status-comment test asserts its id renders verbatim.
var cancelDropRetired = []retiredScenarioEntry{{
	ID: "scenario:issue-101/crit-b", Reason: "behaviour replaced",
	RunID: "r1", PR: 742, RetiredAt: "2026-09-12T00:00:00Z",
}}

// newCancelDropSeam builds the seam. withAcceptance=false omits the
// acceptance stage entirely (the plan-stage fallback case). retired=nil seeds
// an approve row carrying NO retirements.
func newCancelDropSeam(t *testing.T, acceptanceState run.StageState, withAcceptance bool, retired []retiredScenarioEntry) *cancelDropSeam {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newAuditFake()
	runRow := rr.seedRun() // StateRunning
	runRow.IssueContext = &run.IssueContext{Number: 101, Title: "t", Body: "b", URL: "https://github.com/x/y/issues/101"}
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateSucceeded) // Type plan
	var acceptance *run.Stage
	if withAcceptance {
		acceptance = rr.seedStage(runRow.ID, 1, acceptanceState)
		acceptance.Type = run.StageTypeAcceptance
	}
	payload := map[string]any{"stage_id": planStage.ID.String(), "decision": "approve"}
	if retired != nil {
		payload["retired_scenarios"] = retired
	}
	seedHeadEntry(au, runRow.ID, &planStage.ID, "approval_submitted", 1, payload)

	rec := &pageClassRecorder{}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au, APITokenRepo: stubToken("write:runs")})
	s.issueNotifier = rec
	logBuf := &bytes.Buffer{}
	s.cfg.Logger = slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &cancelDropSeam{s: s, rr: rr, au: au, rec: rec, logBuf: logBuf,
		runID: runRow.ID, planID: planStage.ID, acceptance: acceptance}
}

// dropRows returns the appended acceptance_scenario_retirement_dropped rows.
func (c *cancelDropSeam) dropRows() []audit.ChainAppendParams {
	return appendedOfCategory(c.au, CategoryAcceptanceScenarioRetirementDropped)
}

// cancelDropPayload is the decoded payload shape the cancel writer stamps.
type cancelDropPayload struct {
	RunID                 string                 `json:"run_id"`
	StageID               string                 `json:"stage_id"`
	Retired               []retiredScenarioEntry `json:"retired"`
	ScenarioIDs           []string               `json:"scenario_ids"`
	Reason                string                 `json:"reason"`
	CancelSource          string                 `json:"cancel_source"`
	AcceptanceStageState  string                 `json:"acceptance_stage_state"`
	AcceptanceStageAbsent bool                   `json:"acceptance_stage_absent"`
	Error                 string                 `json:"error"`
}

func decodeCancelDrop(t *testing.T, p audit.ChainAppendParams) cancelDropPayload {
	t.Helper()
	var out cancelDropPayload
	if err := json.Unmarshal(p.Payload, &out); err != nil {
		t.Fatalf("decode drop payload: %v\n%s", err, p.Payload)
	}
	return out
}

// TestRecordRetirementsDroppedOnCancel_AcceptanceStateGate is the spawn-gate
// table: every acceptance StageState the helper can observe after a cancel.
// pending / awaiting_host_dispatch / dispatched RECORD (no runner has fetched
// the retirements — or, for dispatched, may not have); running and every
// terminal state SKIP (the prompt fetch flipped the stage to running in the
// same handler that served the retirements, so the runner's deferred
// reporter owns the drop from there). Counterfactual (B): deleting the
// running/terminal early return in recordAcceptanceRetirementsDroppedOnCancel
// makes the `running` case fail on the row count. The bad state is seeded BY
// CONSTRUCTION (the stage is created in the state under test; the test never
// calls the gate in its own setup).
func TestRecordRetirementsDroppedOnCancel_AcceptanceStateGate(t *testing.T) {
	cases := []struct {
		state  run.StageState
		record bool
	}{
		{run.StageStatePending, true},
		{run.StageStateAwaitingHostDispatch, true},
		{run.StageStateDispatched, true},
		{run.StageStateRunning, false},
		{run.StageStateSucceeded, false},
		{run.StageStateFailed, false},
		{run.StageStateCancelled, false},
		{run.StageStateSuperseded, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			c := newCancelDropSeam(t, tc.state, true, cancelDropRetired)
			c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
			rows := c.dropRows()
			if !tc.record {
				if len(rows) != 0 {
					t.Fatalf("acceptance %s: rows = %d, want 0 (the runner's window owns the report)", tc.state, len(rows))
				}
				if len(c.rec.status) != 0 {
					t.Errorf("acceptance %s: status refreshes = %d, want 0", tc.state, len(c.rec.status))
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("acceptance %s: rows = %d, want exactly 1", tc.state, len(rows))
			}
			row := rows[0]
			if row.StageID == nil || *row.StageID != c.acceptance.ID {
				t.Errorf("row stage = %v, want the acceptance stage %s", row.StageID, c.acceptance.ID)
			}
			if row.ActorKind == nil || *row.ActorKind != audit.ActorSystem {
				t.Errorf("actor = %v, want system", row.ActorKind)
			}
			p := decodeCancelDrop(t, row)
			if p.Reason != acceptanceRetirementDropReasonRunCancelled || p.CancelSource != cancelSourceOperator {
				t.Errorf("reason/cancel_source = %q/%q, want run_cancelled_before_acceptance/operator_cancel", p.Reason, p.CancelSource)
			}
			if p.RunID != c.runID.String() || p.StageID != c.acceptance.ID.String() {
				t.Errorf("run_id/stage_id = %q/%q", p.RunID, p.StageID)
			}
			if len(p.Retired) != 1 || p.Retired[0] != cancelDropRetired[0] {
				t.Errorf("retired = %+v, want the FULL approved entry %+v", p.Retired, cancelDropRetired[0])
			}
			if len(p.ScenarioIDs) != 1 || p.ScenarioIDs[0] != cancelDropRetired[0].ID {
				t.Errorf("scenario_ids = %v", p.ScenarioIDs)
			}
			if p.AcceptanceStageState != string(tc.state) {
				t.Errorf("acceptance_stage_state = %q, want %q", p.AcceptanceStageState, tc.state)
			}
			if p.AcceptanceStageAbsent || p.Error != "" {
				t.Errorf("acceptance_stage_absent/error must be absent on the happy path: %s", row.Payload)
			}
			if len(c.rec.status) != 1 || c.rec.status[0] != c.runID {
				t.Errorf("status refreshes = %v, want exactly one for the run", c.rec.status)
			}
		})
	}
}

// TestRecordRetirementsDroppedOnCancel_NoRetirements_NoRow: an approve row
// carrying no retire_scenario entries costs one audit read and appends
// nothing. Counterfactual (H): deleting the len(retired)==0 early return
// makes an empty-retired row appear here.
func TestRecordRetirementsDroppedOnCancel_NoRetirements_NoRow(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, nil)
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
	if n := len(c.dropRows()); n != 0 {
		t.Fatalf("rows = %d, want 0 (no approved retirement, nothing dropped)", n)
	}
	if n := len(c.rec.status); n != 0 {
		t.Errorf("status refreshes = %d, want 0", n)
	}
}

// TestRecordRetirementsDroppedOnCancel_Idempotent: a second invocation, from
// ANY cancel source, appends nothing — the key is (stage, reason).
// Counterfactual (C): deleting the retirementDropAlreadyRecorded check
// yields two rows.
func TestRecordRetirementsDroppedOnCancel_Idempotent(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, cancelDropRetired)
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceStageCancelled)
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
	if n := len(c.dropRows()); n != 1 {
		t.Fatalf("rows after three invocations = %d, want 1 (idempotent per stage+reason)", n)
	}
	if n := len(c.rec.status); n != 1 {
		t.Errorf("status refreshes = %d, want 1", n)
	}
}

// TestRecordRetirementsDroppedOnCancel_ChainReadError_RecordsEmptyRow: the
// #3396 shape — the approval-chain read fails, so the row carries retired:
// [] (an EMPTY LIST, never null), the read error, and the named WARN event
// exactly once. Counterfactual (G): returning on the read error instead of
// falling through leaves zero rows.
func TestRecordRetirementsDroppedOnCancel_ChainReadError_RecordsEmptyRow(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, cancelDropRetired)
	c.au.listByCategoryErrCategory = "approval_submitted"
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceRunBudget)
	rows := c.dropRows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (the failed read is the only source of the entries it would name)", len(rows))
	}
	p := decodeCancelDrop(t, rows[0])
	if p.Retired == nil || len(p.Retired) != 0 || !strings.Contains(string(rows[0].Payload), `"retired":[]`) {
		t.Errorf("retired must be an EMPTY LIST (not null): %s", rows[0].Payload)
	}
	if p.Error == "" || p.Reason != acceptanceRetirementDropReasonRunCancelled || p.CancelSource != cancelSourceRunBudget {
		t.Errorf("payload = %s, want error + reason run_cancelled_before_acceptance + cancel_source run_budget_exceeded", rows[0].Payload)
	}
	if n := strings.Count(c.logBuf.String(), `"event":"`+acceptanceRetirementCancelUnreadableEvent+`"`); n != 1 {
		t.Errorf("%s log lines = %d, want 1:\n%s", acceptanceRetirementCancelUnreadableEvent, n, c.logBuf.String())
	}
	if n := len(c.rec.status); n != 1 {
		t.Errorf("status refreshes = %d, want 1", n)
	}
}

// TestRecordRetirementsDroppedOnCancel_AppendError_BestEffort: an append
// failure is WARN-logged, refreshes nothing, and never panics or unwinds.
func TestRecordRetirementsDroppedOnCancel_AppendError_BestEffort(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, cancelDropRetired)
	c.au.appendErrCategory = CategoryAcceptanceScenarioRetirementDropped
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
	if n := len(c.dropRows()); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
	if n := len(c.rec.status); n != 0 {
		t.Errorf("status refreshes = %d, want 0 (nothing landed to render)", n)
	}
	if !strings.Contains(c.logBuf.String(), "append audit entry failed") {
		t.Errorf("append failure must be WARN-logged:\n%s", c.logBuf.String())
	}
}

// TestRecordRetirementsDroppedOnCancel_IdempotencyReadError_StillAppends: a
// failed drop-category list is WARN + proceed (the row still lands), the
// same posture the other two writers take.
func TestRecordRetirementsDroppedOnCancel_IdempotencyReadError_StillAppends(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, cancelDropRetired)
	c.au.listByCategoryErrCategory = CategoryAcceptanceScenarioRetirementDropped
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
	if n := len(c.dropRows()); n != 1 {
		t.Fatalf("rows = %d, want 1 (list error is WARN + proceed)", n)
	}
	if !strings.Contains(c.logBuf.String(), "proceeding without idempotency guard") {
		t.Errorf("list failure must be WARN-logged:\n%s", c.logBuf.String())
	}
}

// TestRecordRetirementsDroppedOnCancel_NoAcceptanceStage_StampsPlanStage: a
// run with no acceptance stage still gets the record, on the plan stage that
// recorded the approval, flagged acceptance_stage_absent.
func TestRecordRetirementsDroppedOnCancel_NoAcceptanceStage_StampsPlanStage(t *testing.T) {
	c := newCancelDropSeam(t, "", false, cancelDropRetired)
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
	rows := c.dropRows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (never drop the record for lack of a stage)", len(rows))
	}
	if rows[0].StageID == nil || *rows[0].StageID != c.planID {
		t.Errorf("row stage = %v, want the plan stage %s", rows[0].StageID, c.planID)
	}
	p := decodeCancelDrop(t, rows[0])
	if !p.AcceptanceStageAbsent || p.AcceptanceStageState != "" || p.StageID != c.planID.String() {
		t.Errorf("payload = %s, want acceptance_stage_absent:true on the plan stage and no acceptance_stage_state", rows[0].Payload)
	}
	if len(p.Retired) != 1 {
		t.Errorf("retired = %+v, want the full entry", p.Retired)
	}
}

// TestRecordRetirementsDroppedOnCancel_ListStagesError_NoRow: a failed stage
// list leaves no row (there is no stage to stamp) and WARN-logs.
func TestRecordRetirementsDroppedOnCancel_ListStagesError_NoRow(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, cancelDropRetired)
	rr := &cancelDropListStagesErrRepo{orchestratorRepo: c.rr, err: errors.New("stages down")}
	c.s.cfg.RunRepo = rr
	c.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
	if n := len(c.dropRows()); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
	if !strings.Contains(c.logBuf.String(), "list stages failed") {
		t.Errorf("list-stages failure must be WARN-logged:\n%s", c.logBuf.String())
	}
}

// cancelDropListStagesErrRepo makes ListStagesForRun fail while every other
// orchestratorRepo method (GetStage for approvalEntryStageIsPlan) works.
type cancelDropListStagesErrRepo struct {
	*orchestratorRepo
	err error
}

func (r *cancelDropListStagesErrRepo) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, r.err
}

// TestRecordRetirementsDroppedOnCancel_NilRepos_NoOp: an unconfigured audit
// or run repository makes the helper a no-op (CLI/dev posture).
func TestRecordRetirementsDroppedOnCancel_NilRepos_NoOp(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, cancelDropRetired)
	noAudit := New(Config{Addr: "127.0.0.1:0", RunRepo: c.rr})
	noAudit.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
	noRun := New(Config{Addr: "127.0.0.1:0", AuditRepo: c.au})
	noRun.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), c.runID, cancelSourceOperator)
	if n := len(c.dropRows()); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
}

// TestRecordRetirementsDroppedOnCancel_OnRunCancelled_Delegates: the exported
// orchestrator-observer entry point reaches the helper with the source it
// was given.
func TestRecordRetirementsDroppedOnCancel_OnRunCancelled_Delegates(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, cancelDropRetired)
	c.s.OnRunCancelled(context.Background(), c.runID, cancelSourceStageCancelled)
	rows := c.dropRows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if p := decodeCancelDrop(t, rows[0]); p.CancelSource != cancelSourceStageCancelled {
		t.Errorf("cancel_source = %q, want stage_cancelled", p.CancelSource)
	}
}

// TestCancelRun_DroppedRetirement_RendersOnStatusComment is the done-means
// test asserting SHIPPED output over the REAL routes: a bearer
// POST /v0/runs/{id}/cancel through s.Handler(), then
// GET /v0/runs/{id}/status-comment through handleGetStatusComment, whose
// body must carry the #3392 renderer's line naming the dropped scenario id.
// No new render code: the row lands in the category the renderer already
// admits. Counterfactual (A): deleting the helper call in handleCancelRun
// leaves the line out of the body.
func TestCancelRun_DroppedRetirement_RendersOnStatusComment(t *testing.T) {
	c := newCancelDropSeam(t, run.StageStatePending, true, cancelDropRetired)

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/cancel", c.runID), nil)
	req.Header.Set("Authorization", "Bearer "+c.s.cfg.APITokenRepo.(*stubAPITokenRepo).tok.PlainText)
	w := httptest.NewRecorder()
	c.s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.State != string(run.StateCancelled) {
		t.Fatalf("State = %q, want cancelled", got.State)
	}

	greq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/runs/%s/status-comment", c.runID), nil)
	gw := httptest.NewRecorder()
	c.s.Handler().ServeHTTP(gw, greq)
	if gw.Code != http.StatusOK {
		t.Fatalf("status-comment GET status = %d, want 200:\n%s", gw.Code, gw.Body.String())
	}
	var resp statusCommentResponse
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode status-comment response: %v", err)
	}
	const want = "Acceptance scenario retirement dropped (run_cancelled_before_acceptance): 1 scenario still replayed — `scenario:issue-101/crit-b`"
	if !strings.Contains(resp.Body, want) {
		t.Fatalf("status-comment body missing %q:\n%s", want, resp.Body)
	}
}
