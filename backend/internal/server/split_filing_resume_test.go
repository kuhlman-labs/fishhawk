package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/puddle/v2"
	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// newResumeHarness is the default resume fixture: a running run with an issue
// trigger_ref and repo o/r, its own plan stage succeeded with RequiresApproval
// and EndedAt after the split artifact's CreatedAt, one approve row, a 3-phase
// split artifact and no markers — so every rung except the one under test
// passes.
func newResumeHarness(t *testing.T, mutate func(*splitFilingConfig)) *splitFilingHarness {
	t.Helper()
	cfg := splitFilingConfig{withSplitProposal: true, approvalGate: true, approvedAlready: true, runState: run.StateRunning}
	if mutate != nil {
		mutate(&cfg)
	}
	return newSplitFilingHarness(t, cfg)
}

// resumeOperatorID is a non-run-bound bearer identity holding write:approvals.
func resumeOperatorID() Identity {
	return Identity{Subject: "github:op", TokenID: "tok-op", Scopes: []string{"write:approvals"}}
}

// resume POSTs the resume through the REAL route registration.
func (h *splitFilingHarness) resume(t *testing.T, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	return h.resumeRun(t, id, h.runID.String())
}

func (h *splitFilingHarness) resumeRun(t *testing.T, id Identity, runID string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.s.registerRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/split-filing/resume", nil)
	req = withIdentity(req, id)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// resumeEntries decodes every split_filing_resume_requested entry appended.
func (h *splitFilingHarness) resumeEntries(t *testing.T) ([]splitFilingResumeRequestedPayload, []audit.ChainAppendParams) {
	t.Helper()
	h.au.mu.Lock()
	defer h.au.mu.Unlock()
	var out []splitFilingResumeRequestedPayload
	var raw []audit.ChainAppendParams
	for _, e := range h.au.appended {
		if e.Category != splitFilingResumeRequestedCategory {
			continue
		}
		var p splitFilingResumeRequestedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", splitFilingResumeRequestedCategory, err)
		}
		out = append(out, p)
		raw = append(raw, e)
	}
	return out, raw
}

// seedPhaseMarker records phase idx as already filed, BY CONSTRUCTION.
func (h *splitFilingHarness) seedPhaseMarker(idx, number int) {
	marker, _ := json.Marshal(splitChildFiledMarker{SplitPhaseIndex: &idx, CreatedNumber: number, CreatedURL: "https://github.com/o/r/issues/" + strconv.Itoa(number)})
	rid := h.runID
	h.au.seeded = append(h.au.seeded, &audit.Entry{RunID: &rid, Category: categoryWorkItemFiled, Payload: marker})
}

// wantResumeRefused asserts status + code, zero File requests, zero attribution
// entries and a free registry. It returns the error body.
func wantResumeRefused(t *testing.T, h *splitFilingHarness, rec *httptest.ResponseRecorder, status int, code string) errorBody {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	eb := decodeErrorEnvelope(t, rec)
	if eb.Code != code {
		t.Fatalf("code = %q, want %q; body %s", eb.Code, code, rec.Body.String())
	}
	h.s.waitSplitFiling()
	if got := h.provider.requestCount(); got != 0 {
		t.Errorf("File requests = %d after a refusal, want 0", got)
	}
	if entries, _ := h.resumeEntries(t); len(entries) != 0 {
		t.Errorf("attribution entries = %d after a refusal, want 0 (refusals write nothing)", len(entries))
	}
	if n := h.s.splitFiling.holdersFor(h.runID); n != 0 {
		t.Errorf("holders = %d after a refusal, want 0", n)
	}
	return eb
}

func decodeResumeResponse(t *testing.T, rec *httptest.ResponseRecorder, status int) splitFilingResumeResponse {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	var out splitFilingResumeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode resume response: %v", err)
	}
	return out
}

// TestResumeSplitFiling_PartialFilesOnlyMissingPhases (T2, AC2) crosses route,
// handler, registry, detached hook, provider and audit persistence: a phase-0
// marker is recorded, so the resume files only phases 1 and 2.
func TestResumeSplitFiling_PartialFilesOnlyMissingPhases(t *testing.T) {
	h := newResumeHarness(t, nil)
	h.seedPhaseMarker(0, 4001)

	resp := decodeResumeResponse(t, h.resume(t, resumeOperatorID()), http.StatusAccepted)
	if resp.State != splitFilingResumeStateResuming || resp.PhaseCount != 3 || resp.AlreadyFiled != 1 {
		t.Errorf("response = %+v, want resuming / phase_count 3 / already_filed 1", resp)
	}
	if want := int(splitFilingBudgetFor(3) / time.Second); resp.BudgetSeconds != want {
		t.Errorf("budget_seconds = %d, want %d", resp.BudgetSeconds, want)
	}
	if resp.StageID != h.planStage.ID.String() || resp.RunID != h.runID.String() {
		t.Errorf("ids = %s/%s, want %s/%s", resp.RunID, resp.StageID, h.runID, h.planStage.ID)
	}
	h.s.waitSplitFiling()

	if got := h.provider.requestCount(); got != 2 {
		t.Errorf("File requests = %d, want 2 (phases 1 and 2 only)", got)
	}
	payload, n := h.completionEntry(t)
	if n != 1 {
		t.Fatalf("split_children_filed entries = %d, want 1", n)
	}
	var sawSeeded bool
	for _, c := range payload.Children {
		if c.Number == 4001 {
			sawSeeded = true
		}
	}
	if !sawSeeded {
		t.Errorf("completion children %+v omit the seeded phase-0 #4001", payload.Children)
	}
	entries, raw := h.resumeEntries(t)
	if len(entries) != 1 {
		t.Fatalf("attribution entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Actor != "github:op" || e.ArtifactID != h.planArtifact.ID.String() || e.Outcome != splitFilingResumeStateResuming ||
		e.PhaseCount != 3 || e.AlreadyFiled != 1 || e.StageID != h.planStage.ID.String() {
		t.Errorf("attribution payload = %+v", e)
	}
	if raw[0].ActorSubject == nil || *raw[0].ActorSubject != "github:op" {
		t.Errorf("attribution ActorSubject = %v, want github:op", raw[0].ActorSubject)
	}
	if n := h.s.splitFiling.holdersFor(h.runID); n != 0 {
		t.Errorf("holders after settle = %d, want 0", n)
	}
}

// TestResumeSplitFiling_InFlightRefused409 (T3, V7): while the first resume's
// filer is parked inside File, a second resume is refused 409
// split_filing_in_progress and writes no second attribution entry.
func TestResumeSplitFiling_InFlightRefused409(t *testing.T) {
	h := newResumeHarness(t, func(c *splitFilingConfig) { c.parkOnCall = 1 })
	h.seedPhaseMarker(0, 4001)
	// Registered after the harness's drain, so it runs FIRST: a failed
	// assertion before the release below can never deadlock the drain.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(h.provider.releasePark) }
	t.Cleanup(release)

	decodeResumeResponse(t, h.resume(t, resumeOperatorID()), http.StatusAccepted)
	<-h.provider.parked

	rec := h.resume(t, resumeOperatorID())
	if rec.Code != http.StatusConflict {
		release()
		h.s.waitSplitFiling()
		t.Fatalf("second resume status = %d, want 409; body %s; File requests after settle = %d (want 2)",
			rec.Code, rec.Body.String(), h.provider.requestCount())
	}
	eb := decodeErrorEnvelope(t, rec)
	if eb.Code != splitFilingResumeCodeInProgress {
		t.Fatalf("code = %q, want %s", eb.Code, splitFilingResumeCodeInProgress)
	}
	if !strings.Contains(eb.Message, "not proof of progress") || eb.Details["budget_seconds"] == nil {
		t.Errorf("in-progress refusal does not name the probe and budget_seconds: %+v", eb)
	}
	if entries, _ := h.resumeEntries(t); len(entries) != 1 {
		t.Errorf("attribution entries = %d, want 1 (the refused resume writes nothing)", len(entries))
	}

	h.s.splitFiling.waitHoldersAtMost(h.runID, 1)
	release()
	h.s.waitSplitFiling()

	if got := h.provider.requestCount(); got != 2 {
		t.Errorf("File requests = %d, want 2 (one per missing phase; no second filer)", got)
	}
}

// TestResumeSplitFiling_InCommitToLaunchWindowRefused409 (S2a, V7): between the
// approval's stage commit and its launch, a resume through the real route sees
// the approval's claim and is refused.
func TestResumeSplitFiling_InCommitToLaunchWindowRefused409(t *testing.T) {
	h := newResumeHarness(t, func(c *splitFilingConfig) { c.approvedAlready = false })
	var seamCodes []string
	h.s.splitFilingPreLaunch = func(*run.Stage) {
		rec := h.resume(t, resumeOperatorID())
		code := ""
		if rec.Code != http.StatusAccepted {
			code = decodeErrorEnvelope(t, rec).Code
		} else {
			code = "202"
		}
		seamCodes = append(seamCodes, code)
	}

	h.approveAs(t, context.Background(), "approver-one", *h.planStage)
	h.s.waitSplitFiling()

	if len(seamCodes) != 1 || seamCodes[0] != splitFilingResumeCodeInProgress {
		t.Fatalf("seam resume codes = %v, want [%s]", seamCodes, splitFilingResumeCodeInProgress)
	}
	if entries, _ := h.resumeEntries(t); len(entries) != 0 {
		t.Errorf("attribution entries = %d, want 0", len(entries))
	}
	if got := h.provider.requestCount(); got != 3 {
		t.Errorf("File requests = %d, want 3", got)
	}
	if _, n := h.completionEntry(t); n != 1 {
		t.Errorf("split_children_filed entries = %d, want 1", n)
	}
	if n := h.s.splitFiling.holdersFor(h.runID); n != 0 {
		t.Errorf("holders = %d, want 0", n)
	}
}

// TestResumeSplitFiling_RunTerminal409 (V4, V7): failed and cancelled runs are
// refused; a succeeded run stays admitted.
func TestResumeSplitFiling_RunTerminal409(t *testing.T) {
	for _, st := range []run.State{run.StateFailed, run.StateCancelled} {
		t.Run(string(st), func(t *testing.T) {
			h := newResumeHarness(t, func(c *splitFilingConfig) { c.runState = st })
			eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeRunTerminal)
			if eb.Details["run_state"] != string(st) {
				t.Errorf("details.run_state = %v, want %s", eb.Details["run_state"], st)
			}
			if !strings.Contains(eb.Message, "no idempotency marker") {
				t.Errorf("run_terminal remedy omits the hand-filed warning: %q", eb.Message)
			}
		})
	}
	t.Run("succeeded", func(t *testing.T) {
		h := newResumeHarness(t, func(c *splitFilingConfig) { c.runState = run.StateSucceeded })
		decodeResumeResponse(t, h.resume(t, resumeOperatorID()), http.StatusAccepted)
		h.s.waitSplitFiling()
		if got := h.provider.requestCount(); got != 3 {
			t.Errorf("File requests = %d, want 3", got)
		}
	})
}

// TestResumeSplitFiling_ParentSplitPlanOnly409 (V2a): the child's own plan
// stage is approved but carries no artifact; the parent's split is never filed.
func TestResumeSplitFiling_ParentSplitPlanOnly409(t *testing.T) {
	h := newResumeHarness(t, func(c *splitFilingConfig) { c.parentRun = true })
	eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeNoSplit)
	if eb.Details["reason"] != "no_plan_artifact" {
		t.Errorf("details.reason = %v, want no_plan_artifact", eb.Details["reason"])
	}
}

// TestResumeSplitFiling_UndecodableOwnArtifact409 is a response-mode check.
// It pins the reason too: an own artifact that exists but is not plan JSON is
// plan_artifact_undecodable, never conflated with no_plan_artifact.
func TestResumeSplitFiling_UndecodableOwnArtifact409(t *testing.T) {
	h := newResumeHarness(t, func(c *splitFilingConfig) { c.planOverride = []byte("not json") })
	eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeNoSplit)
	if eb.Details["reason"] != "plan_artifact_undecodable" {
		t.Errorf("details.reason = %v, want plan_artifact_undecodable", eb.Details["reason"])
	}
}

// TestResumeSplitFiling_ArtifactAfterFirstApproveOnDBClock409: the artifact
// predates EndedAt on the daemon clock but postdates the earliest approve row
// on the database clock (the clock artifacts.created_at uses). A later
// re-approval row (base+2m) cannot widen the bound: the EARLIEST row decides.
// Refused naming the approve-row time; nothing filed.
func TestResumeSplitFiling_ArtifactAfterFirstApproveOnDBClock409(t *testing.T) {
	h := newResumeHarness(t, func(c *splitFilingConfig) {
		c.artifactAt = splitFixtureBase.Add(30 * time.Second)
		c.endedAt = splitFixtureBase.Add(time.Minute)
		c.approvedAt = splitFixtureBase.Add(10 * time.Second)
	})
	h.ar.all = append(h.ar.all, &approval.Approval{
		ID: uuid.New(), StageID: h.planStage.ID, ApproverSubject: "late-reapprover",
		Decision: approval.DecisionApprove, SubmittedAt: splitFixtureBase.Add(2 * time.Minute),
	})
	eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodePlanChanged)
	if eb.Details["first_approve_submitted_at"] != "2026-11-01T12:00:10Z" {
		t.Errorf("details.first_approve_submitted_at = %v, want 2026-11-01T12:00:10Z", eb.Details["first_approve_submitted_at"])
	}
	if eb.Details["artifact_id"] != h.planArtifact.ID.String() {
		t.Errorf("details.artifact_id = %v, want %s", eb.Details["artifact_id"], h.planArtifact.ID)
	}
}

// TestResumeSplitFiling_StoreReadErrors (77a8e926 a-c): each read before the
// claim surfaces its error as 500 internal_error with nothing recorded,
// launched or held.
func TestResumeSplitFiling_StoreReadErrors(t *testing.T) {
	t.Run("get_run", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		h.rr.runErr = errors.New("run store down")
		wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusInternalServerError, "internal_error")
	})
	t.Run("list_stages", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		h.rr.stagesByRunID = nil // promptRunRepo answers ListStagesForRun with an error
		wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusInternalServerError, "internal_error")
	})
	t.Run("list_approvals", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		h.s.cfg.ApprovalRepo = &listErrApprovalRepo{fakeApprovalRepo: h.ar, err: errors.New("approval store down")}
		wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusInternalServerError, "internal_error")
	})
}

// listErrApprovalRepo fails ListForStage; every other method is the fake's.
type listErrApprovalRepo struct {
	*fakeApprovalRepo
	err error
}

func (l *listErrApprovalRepo) ListForStage(context.Context, uuid.UUID) ([]*approval.Approval, error) {
	return nil, l.err
}

// TestResumeSplitFiling_PlanStageSelection (af3bb039, 77a8e926 d): the run's
// NEWEST plan stage decides. An older succeeded, approved stage carries the
// split artifact; a newer plan stage (listed second, so a first-match pick
// would take the older one) is still awaiting approval. Refused with nothing
// filed. A run with no plan stage at all is refused no_plan_stage.
func TestResumeSplitFiling_PlanStageSelection(t *testing.T) {
	t.Run("newest_not_succeeded", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		h.planStage.CreatedAt = splitFixtureBase
		newer := &run.Stage{
			ID: uuid.New(), RunID: h.runID, Type: run.StageTypePlan, State: run.StageStateAwaitingApproval,
			RequiresApproval: true, CreatedAt: splitFixtureBase.Add(time.Hour),
		}
		h.rr.getStages[newer.ID] = newer
		h.rr.stagesByRunID[h.runID] = []*run.Stage{h.planStage, newer}
		eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeNotApproved)
		if eb.Details["reason"] != "plan_stage_not_succeeded" || eb.Details["stage_id"] != newer.ID.String() {
			t.Errorf("details = %v, want reason plan_stage_not_succeeded on the newer stage %s", eb.Details, newer.ID)
		}
	})
	t.Run("no_plan_stage", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		h.rr.stagesByRunID[h.runID] = []*run.Stage{{ID: uuid.New(), RunID: h.runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded}}
		eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeNotApproved)
		if eb.Details["reason"] != "no_plan_stage" {
			t.Errorf("details.reason = %v, want no_plan_stage", eb.Details["reason"])
		}
	})
}

// TestResumeSplitFiling_AdmittedPrincipals (77a8e926 e, c3d3633f): pins who
// the verb ADMITS beyond a plain operator token. A cookie session (no
// TokenID) passes the scope rung without write:approvals, bounded only by the
// memberWrite account tier; a delegated operator-agent token holding
// write:approvals is admitted — the verb applies no approver-eligibility gate
// and files only what a recorded approval authorized.
func TestResumeSplitFiling_AdmittedPrincipals(t *testing.T) {
	for name, id := range map[string]Identity{
		"cookie_session":           {Subject: "github:op", UserID: "user-op", SessionID: "sess-op", AccountID: "acct-op"},
		"delegated_operator_agent": {Subject: "operator-agent/operator-role-v0", TokenID: "tok-agent", Scopes: []string{"write:approvals"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newResumeHarness(t, nil)
			resp := decodeResumeResponse(t, h.resume(t, id), http.StatusAccepted)
			if resp.State != splitFilingResumeStateResuming {
				t.Errorf("state = %q, want resuming", resp.State)
			}
			h.s.waitSplitFiling()
			entries, _ := h.resumeEntries(t)
			if len(entries) != 1 || entries[0].Actor != id.Subject {
				t.Errorf("attribution entries = %+v, want one by %s", entries, id.Subject)
			}
		})
	}
}

// TestResumeSplitFiling_PlanChangedSinceApproval409 (AA1): the stage succeeded
// at EndedAt t0; A1 (split) predates it, A2 (a different valid split) was
// uploaded after it. The resume is refused naming A2, and files nothing.
func TestResumeSplitFiling_PlanChangedSinceApproval409(t *testing.T) {
	h := newResumeHarness(t, nil)
	a2 := h.seedLateArtifact(t)

	eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodePlanChanged)
	if eb.Details["artifact_id"] != a2.ID.String() {
		t.Errorf("details.artifact_id = %v, want A2 %s", eb.Details["artifact_id"], a2.ID)
	}
	if eb.Details["stage_ended_at"] == nil || eb.Details["artifact_created_at"] == nil {
		t.Errorf("details omit the binding times: %v", eb.Details)
	}
	if !strings.Contains(eb.Message, "start a new run, because the artifact changed after the plan gate closed") {
		t.Errorf("remedy = %q, want the start-a-new-run text", eb.Message)
	}
}

// TestResumeSplitFiling_ArtifactAfterSuccessThenSecondSubjectApprove409 (AA1,
// replaces _ReapprovedAfterNewArtifactAdmitted): an artifact uploaded after the
// stage succeeded, then a second-subject re-approval. The re-approval launches
// nothing (a same-state re-approval does not move EndedAt) and the resume is
// still refused.
func TestResumeSplitFiling_ArtifactAfterSuccessThenSecondSubjectApprove409(t *testing.T) {
	h := newResumeHarness(t, nil)
	a2 := h.seedLateArtifact(t)

	h.approveAs(t, context.Background(), "approver-two", *h.planStage)
	h.s.waitSplitFiling()
	if got := h.launches(); got != 0 {
		t.Fatalf("re-approval launches = %d, want 0", got)
	}

	eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodePlanChanged)
	if eb.Details["artifact_id"] != a2.ID.String() {
		t.Errorf("details.artifact_id = %v, want A2 %s", eb.Details["artifact_id"], a2.ID)
	}
}

// seedLateArtifact adds a valid split artifact created one minute AFTER the
// plan stage's EndedAt, BY CONSTRUCTION.
func (h *splitFilingHarness) seedLateArtifact(t *testing.T) *artifact.Artifact {
	t.Helper()
	sv := "standard_v1"
	a2, err := h.art.Create(context.Background(), artifact.CreateParams{
		StageID: h.planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: splitPlanBytes(t, true),
	})
	if err != nil {
		t.Fatalf("seed A2: %v", err)
	}
	a2.CreatedAt = h.planStage.EndedAt.Add(time.Minute)
	return a2
}

// TestResumeSplitFiling_HookFiledReadFails_FilesNothing (V1, resume path): the
// handler's own work_item_filed read (call 1) passes; the hook's read (call 2)
// fails, so the hook aborts before filing anything.
func TestResumeSplitFiling_HookFiledReadFails_FilesNothing(t *testing.T) {
	h := newResumeHarness(t, nil)
	h.seedPhaseMarker(0, 4001)
	h.s.cfg.AuditRepo = &categoryListErrAuditRepo{auditFake: h.au, failCategory: categoryWorkItemFiled, failOnCall: 2}

	resp := decodeResumeResponse(t, h.resume(t, resumeOperatorID()), http.StatusAccepted)
	if resp.AlreadyFiled != 1 {
		t.Errorf("already_filed = %d, want 1", resp.AlreadyFiled)
	}
	h.s.waitSplitFiling()

	if got := h.provider.requestCount(); got != 0 {
		t.Errorf("File requests = %d, want 0 (the hook's failed read must abort)", got)
	}
	if _, n := h.completionEntry(t); n != 0 {
		t.Errorf("split_children_filed entries = %d, want 0", n)
	}
	if !strings.Contains(h.logs.String(), "list work_item_filed failed") {
		t.Errorf("no 'list work_item_filed failed' WARN; logs:\n%s", h.logs.String())
	}
	if entries, _ := h.resumeEntries(t); len(entries) != 1 || entries[0].Outcome != splitFilingResumeStateResuming {
		t.Errorf("attribution entries = %+v, want one with outcome resuming", entries)
	}
}

// TestResumeSplitFiling_PlanNotSucceeded409 (T4).
func TestResumeSplitFiling_PlanNotSucceeded409(t *testing.T) {
	h := newResumeHarness(t, nil)
	h.planStage.State = run.StageStateAwaitingApproval
	eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeNotApproved)
	if eb.Details["reason"] != "plan_stage_not_succeeded" {
		t.Errorf("details.reason = %v", eb.Details["reason"])
	}
}

// TestResumeSplitFiling_GatelessPlanStage409 (S3): an approve row is seeded, so
// the decision check cannot mask the gate check.
func TestResumeSplitFiling_GatelessPlanStage409(t *testing.T) {
	h := newResumeHarness(t, nil)
	h.planStage.RequiresApproval = false
	eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeNotApproved)
	if eb.Details["reason"] != "gateless_plan_stage" {
		t.Errorf("details.reason = %v, want gateless_plan_stage", eb.Details["reason"])
	}
}

// TestResumeSplitFiling_NoApproveDecision409 (S3): only a reject row.
func TestResumeSplitFiling_NoApproveDecision409(t *testing.T) {
	h := newResumeHarness(t, nil)
	h.ar.all[0].Decision = approval.DecisionReject
	eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeNotApproved)
	if eb.Details["reason"] != "no_approve_decision" {
		t.Errorf("details.reason = %v, want no_approve_decision", eb.Details["reason"])
	}
}

// TestResumeSplitFiling_NoParentIssue409 and _MalformedRepo409 (S5).
func TestResumeSplitFiling_NoParentIssue409(t *testing.T) {
	h := newResumeHarness(t, nil)
	h.rr.getRuns[h.runID].TriggerRef = nil
	wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeNoParentIssue)
}

func TestResumeSplitFiling_MalformedRepo409(t *testing.T) {
	h := newResumeHarness(t, nil)
	h.rr.getRuns[h.runID].Repo = "not-a-repo"
	wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeRepoMalformed)
}

// TestResumeSplitFiling_PlanLoadError500: never reported as in_progress.
func TestResumeSplitFiling_PlanLoadError500(t *testing.T) {
	h := newResumeHarness(t, func(c *splitFilingConfig) { c.planLoadErr = errors.New("artifact store down") })
	wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusInternalServerError, splitFilingResumeCodeFailed)
}

// TestResumeSplitFiling_AuditReadErrors (S4): each marker read error refuses
// with nothing launched; an unavailable store answers the 503 envelope.
func TestResumeSplitFiling_AuditReadErrors(t *testing.T) {
	for _, cat := range []string{splitChildrenFiledCategory, splitFilingRefusedCategory, categoryWorkItemFiled} {
		t.Run(cat, func(t *testing.T) {
			h := newResumeHarness(t, nil)
			h.s.cfg.AuditRepo = &categoryListErrAuditRepo{auditFake: h.au, failCategory: cat}
			wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusInternalServerError, "internal_error")
		})
	}
	t.Run("unavailable", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		h.s.cfg.AuditRepo = &categoryListErrAuditRepo{auditFake: h.au, failCategory: splitChildrenFiledCategory,
			err: fmt.Errorf("list audit: %w", puddle.ErrClosedPool)}
		wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusServiceUnavailable, "service_unavailable")
	})
}

// TestResumeSplitFiling_AttributionAppendFails500 (S7): an append failure
// launches nothing.
func TestResumeSplitFiling_AttributionAppendFails500(t *testing.T) {
	h := newResumeHarness(t, nil)
	h.au.appendErrCategory = splitFilingResumeRequestedCategory
	wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusInternalServerError, splitFilingResumeCodeFailed)
}

// TestResumeSplitFiling_FilesNothingStillAttributed: the provider fails on its
// first call, so nothing is filed, but the admitted resume is still attributed.
func TestResumeSplitFiling_FilesNothingStillAttributed(t *testing.T) {
	h := newResumeHarness(t, func(c *splitFilingConfig) { c.providerFailOnCall = 1 })
	decodeResumeResponse(t, h.resume(t, resumeOperatorID()), http.StatusAccepted)
	h.s.waitSplitFiling()
	if n := h.childMarkerCount(t); n != 0 {
		t.Errorf("child markers = %d, want 0", n)
	}
	entries, _ := h.resumeEntries(t)
	if len(entries) != 1 || entries[0].Actor != "github:op" || entries[0].Outcome != splitFilingResumeStateResuming {
		t.Errorf("attribution entries = %+v, want one by github:op with outcome resuming", entries)
	}
}

// TestResumeSplitFiling_RunBoundTokenForbidden (T5, V7, AA3): every mcp:run:
// subject — own run, other run, malformed — HOLDS write:approvals, so only the
// run-bound prefix check stops it.
func TestResumeSplitFiling_RunBoundTokenForbidden(t *testing.T) {
	for name, subject := range map[string]func(uuid.UUID) string{
		"own_run":   func(id uuid.UUID) string { return "mcp:run:" + id.String() },
		"other_run": func(uuid.UUID) string { return "mcp:run:" + uuid.NewString() },
		"malformed": func(uuid.UUID) string { return "mcp:run:not-a-uuid" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newResumeHarness(t, nil)
			id := Identity{Subject: subject(h.runID), TokenID: "tok-run", Scopes: []string{"write:approvals", "write:runs"}}
			wantResumeRefused(t, h, h.resume(t, id), http.StatusForbidden, "run_token_forbidden")
		})
	}
}

// TestResumeSplitFiling_MissingWriteApprovalsForbidden (T6, AA2): a
// write:runs-only token is refused insufficient_scope.
func TestResumeSplitFiling_MissingWriteApprovalsForbidden(t *testing.T) {
	h := newResumeHarness(t, nil)
	id := Identity{Subject: "github:op", TokenID: "tok-op", Scopes: []string{"write:runs"}}
	eb := wantResumeRefused(t, h, h.resume(t, id), http.StatusForbidden, "insufficient_scope")
	if eb.Details["required_scope"] != "write:approvals" {
		t.Errorf("details.required_scope = %v, want write:approvals", eb.Details["required_scope"])
	}
}

// TestResumeSplitFiling_AnonymousUnauthorized401 (T7, V3).
func TestResumeSplitFiling_AnonymousUnauthorized401(t *testing.T) {
	anon := Identity{Subject: "anonymous"}
	t.Run("unknown_run", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		eb := wantResumeRefused(t, h, h.resumeRun(t, anon, uuid.Nil.String()), http.StatusUnauthorized, "authentication_required")
		if !strings.Contains(eb.Message, "write:approvals") {
			t.Errorf("401 message %q does not name write:approvals", eb.Message)
		}
	})
	t.Run("tenanted_run", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		h.rr.getRuns[h.runID].AccountID = testOperatorAccountID
		wantResumeRefused(t, h, h.resume(t, anon), http.StatusUnauthorized, "authentication_required")
	})
	t.Run("direct", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		req := withIdentity(httptest.NewRequest(http.MethodPost, "/", nil), anon)
		req.SetPathValue("run_id", h.runID.String())
		rec := httptest.NewRecorder()
		h.s.handleResumeSplitFiling(rec, req)
		wantResumeRefused(t, h, rec, http.StatusUnauthorized, "authentication_required")
	})
}

// TestResumeSplitFiling_UnconfiguredAfterAuth (V3 ordering): with nil repos an
// anonymous direct call is 401, not 503; an authorized one is 503.
func TestResumeSplitFiling_UnconfiguredAfterAuth(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	call := func(id Identity) *httptest.ResponseRecorder {
		req := withIdentity(httptest.NewRequest(http.MethodPost, "/", nil), id)
		req.SetPathValue("run_id", uuid.NewString())
		rec := httptest.NewRecorder()
		s.handleResumeSplitFiling(rec, req)
		return rec
	}
	if rec := call(Identity{Subject: "anonymous"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous with nil repos: %d %s, want 401", rec.Code, rec.Body.String())
	}
	rec := call(resumeOperatorID())
	if rec.Code != http.StatusServiceUnavailable || decodeErrorEnvelope(t, rec).Code != splitFilingResumeCodeUnconfigured {
		t.Errorf("authorized with nil repos: %d %s, want 503 %s", rec.Code, rec.Body.String(), splitFilingResumeCodeUnconfigured)
	}
}

// TestResumeSplitFiling_ResponseModes (T8): response-mode checks, not
// committed-state counterfactual vehicles (the hook's own gates mask them).
func TestResumeSplitFiling_ResponseModes(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		rid := h.runID
		h.au.seeded = append(h.au.seeded, &audit.Entry{RunID: &rid, Category: splitChildrenFiledCategory, Payload: []byte(`{}`)})
		resp := decodeResumeResponse(t, h.resume(t, resumeOperatorID()), http.StatusOK)
		if resp.State != splitFilingResumeStateCompleted || resp.BudgetSeconds != 0 {
			t.Errorf("response = %+v, want completed with no budget", resp)
		}
		h.s.waitSplitFiling()
		if got := h.provider.requestCount(); got != 0 {
			t.Errorf("File requests = %d, want 0", got)
		}
		if entries, _ := h.resumeEntries(t); len(entries) != 1 || entries[0].Outcome != splitFilingResumeStateCompleted {
			t.Errorf("attribution entries = %+v, want one with outcome completed", entries)
		}
		if n := h.s.splitFiling.holdersFor(h.runID); n != 0 {
			t.Errorf("holders = %d, want 0", n)
		}
	})
	t.Run("refused_marker", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		rid := h.runID
		h.au.seeded = append(h.au.seeded, &audit.Entry{RunID: &rid, Category: splitFilingRefusedCategory, Payload: []byte(`{}`)})
		wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeFilingRefused)
	})
	t.Run("no_split_proposal", func(t *testing.T) {
		h := newResumeHarness(t, func(c *splitFilingConfig) { c.withSplitProposal = false })
		eb := wantResumeRefused(t, h, h.resume(t, resumeOperatorID()), http.StatusConflict, splitFilingResumeCodeNoSplit)
		if eb.Details["reason"] != "no_split_proposal" {
			t.Errorf("details.reason = %v", eb.Details["reason"])
		}
	})
	t.Run("unknown_run", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		wantResumeRefused(t, h, h.resumeRun(t, resumeOperatorID(), uuid.NewString()), http.StatusNotFound, "run_not_found")
	})
	t.Run("bad_uuid", func(t *testing.T) {
		h := newResumeHarness(t, nil)
		wantResumeRefused(t, h, h.resumeRun(t, resumeOperatorID(), "not-a-uuid"), http.StatusBadRequest, "validation_failed")
	})
}

// splitFilingResumeDocumentedCodes is every refusal code the OpenAPI operation
// must name.
var splitFilingResumeDocumentedCodes = []string{
	splitFilingResumeCodeNotApproved, splitFilingResumeCodeNoSplit, splitFilingResumeCodePlanChanged,
	splitFilingResumeCodeRunTerminal, splitFilingResumeCodeRepoMalformed, splitFilingResumeCodeNoParentIssue,
	splitFilingResumeCodeInProgress, splitFilingResumeCodeFilingRefused, splitFilingResumeCodeUnconfigured,
	splitFilingResumeCodeFailed, "run_token_forbidden", "insufficient_scope", "authentication_required",
}

// TestOpenAPI_SplitFilingResumeRouteDocumented pins the OpenAPI path, its
// operationId, the response schema fields and every refusal code.
func TestOpenAPI_SplitFilingResumeRouteDocumented(t *testing.T) {
	doc, _ := escapeOpenAPIDoc(t)
	op := escapeMapAt(t, doc, "paths", "/v0/runs/{run_id}/split-filing/resume", "post")
	if op["operationId"] != "resumeSplitFiling" {
		t.Errorf("docs/api/v0.openapi.yaml: split-filing resume operationId = %v, want resumeSplitFiling", op["operationId"])
	}
	opYAML, err := yaml.Marshal(op)
	if err != nil {
		t.Fatalf("marshal operation: %v", err)
	}
	for _, code := range splitFilingResumeDocumentedCodes {
		if !strings.Contains(string(opYAML), code) {
			t.Errorf("docs/api/v0.openapi.yaml POST /v0/runs/{run_id}/split-filing/resume does not name code %q", code)
		}
	}
	for _, phrase := range []string{"write:approvals", "budget_seconds", "start a new run", "filed by hand"} {
		if !strings.Contains(string(opYAML), phrase) {
			t.Errorf("docs/api/v0.openapi.yaml POST /v0/runs/{run_id}/split-filing/resume does not state %q", phrase)
		}
	}
	props := escapeMapAt(t, doc, "components", "schemas", "SplitFilingResumeResponse", "properties")
	for _, field := range []string{"run_id", "stage_id", "state", "phase_count", "already_filed", "budget_seconds"} {
		if _, ok := props[field]; !ok {
			t.Errorf("docs/api/v0.openapi.yaml SplitFilingResumeResponse is missing %q", field)
		}
	}
}
