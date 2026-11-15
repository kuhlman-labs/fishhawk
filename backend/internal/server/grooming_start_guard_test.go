package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Handler coverage for the groom start guard (#5225, gap 2,
// grooming_start_guard.go). Parked state is seeded BY CONSTRUCTION: a run is
// created through the real handler (or inserted straight into the fake), and
// then a stage row is set to awaiting_approval in the fake. The guard itself is
// never called in setup. Refusal tests read COMMITTED state (run rows, global
// audit entries) after the call returns, not just the envelope.
//
// conventionsLoader is a process-wide package var (stubConventions), so no case
// here calls t.Parallel.

// groomGuardRepo is the repo every case seeds into.
const groomGuardRepo = "x/y"

// groomGuardWorkflow is groomingSpecYAML's workflow key.
const groomGuardWorkflow = "tidy_the_backlog"

// groomConfirmSpecYAML is a grooming workflow with a plan groom stage AND a
// human `type: review` confirm stage, mirroring
// devfixtures/scenarios/grooming-confirm-gate.yaml. groomingSpecYAML has no
// review stage, so the confirm-gate case needs its own spec.
const groomConfirmSpecYAML = `version: "2"
workflows:
  tidy_the_backlog:
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
        gates:
          - type: approval
            approvals:
              count: 1
              not: [author, agent]
      - id: confirm
        type: review
        executor:
          human: true
        gates:
          - type: approval
            approvals:
              count: 1
              not: [agent]
`

// groomScheduledSpecYAML is groomingSpecYAML admitting the scheduled and
// on-demand triggers, so the in-process scheduled path and an operator POST can
// both reach the guard under one workflow.
const groomScheduledSpecYAML = `version: "2"
workflows:
  tidy_the_backlog:
    applies_to:
      trigger: [scheduled]
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
      - id: apply
        type: implement
        executor:
          agent: claude-code
`

// groomBody is a create request for the grooming workflow under spec.
func groomBody(specYAML string) map[string]any {
	return charterRunBody(groomGuardWorkflow, specYAML)
}

// groomSeed describes a run inserted straight into the fake.
type groomSeed struct {
	repo, workflowID string
	state            run.State
	accountID        string
	createdAt        time.Time
	// parked, when non-empty, is the stage type seeded at awaiting_approval;
	// every other stage is pending.
	parked run.StageType
}

// seedGroomRun inserts a run with a plan and a review stage into the fake BY
// CONSTRUCTION, bypassing the handler (and so the guard).
func seedGroomRun(t *testing.T, repo *fakeRepo, sd groomSeed) *run.Run {
	t.Helper()
	if sd.repo == "" {
		sd.repo = groomGuardRepo
	}
	if sd.workflowID == "" {
		sd.workflowID = groomGuardWorkflow
	}
	if sd.state == "" {
		sd.state = run.StateRunning
	}
	if sd.createdAt.IsZero() {
		sd.createdAt = time.Now().UTC()
	}
	rn := &run.Run{
		ID: uuid.New(), Repo: sd.repo, WorkflowID: sd.workflowID, WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI, State: sd.state, AccountID: sd.accountID,
		CreatedAt: sd.createdAt, UpdatedAt: sd.createdAt,
	}
	var stages []*run.Stage
	for i, typ := range []run.StageType{run.StageTypePlan, run.StageTypeReview} {
		st := run.StageStatePending
		if typ == sd.parked {
			st = run.StageStateAwaitingApproval
		}
		stages = append(stages, &run.Stage{ID: uuid.New(), RunID: rn.ID, Sequence: i + 1, Type: typ, State: st})
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	repo.runs[rn.ID] = rn
	repo.stagesByRun[rn.ID] = stages
	return rn
}

// setStageState sets the first stage of typ on runID, by construction.
func setStageState(t *testing.T, repo *fakeRepo, runID uuid.UUID, typ run.StageType, to run.StageState) uuid.UUID {
	t.Helper()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, st := range repo.stagesByRun[runID] {
		if st.Type == typ {
			st.State = to
			return st.ID
		}
	}
	t.Fatalf("run %s has no %s stage (stages: %d)", runID, typ, len(repo.stagesByRun[runID]))
	return uuid.Nil
}

// setRunState sets a run's state in the fake, by construction.
func setRunState(repo *fakeRepo, runID uuid.UUID, to run.State) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	repo.runs[runID].State = to
}

// createdRunID decodes a 201/200 create response's run id.
func createdRunID(t *testing.T, w *httptest.ResponseRecorder) uuid.UUID {
	t.Helper()
	var rr runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &rr); err != nil || rr.ID == uuid.Nil {
		t.Fatalf("decode run response: %v\n%s", err, w.Body.String())
	}
	return rr.ID
}

// decodeEnvelope decodes an error response.
func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, w.Body.String())
	}
	return env.Error
}

// assertParkedRefusal asserts a 409 grooming_run_parked naming parkedRunID at
// the given stage type.
func assertParkedRefusal(t *testing.T, w *httptest.ResponseRecorder, parkedRunID uuid.UUID, stageType run.StageType) {
	t.Helper()
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 grooming_run_parked:\n%s", w.Code, w.Body.String())
	}
	e := decodeEnvelope(t, w)
	if e.Code != groomingRunParkedCode {
		t.Fatalf("code = %q, want %q", e.Code, groomingRunParkedCode)
	}
	if got := e.Details["parked_run_id"]; got != parkedRunID.String() {
		t.Errorf("details.parked_run_id = %v, want %s", got, parkedRunID)
	}
	if got := e.Details["parked_stage_type"]; got != string(stageType) {
		t.Errorf("details.parked_stage_type = %v, want %s", got, stageType)
	}
	if got := e.Details["workflow_id"]; got != groomGuardWorkflow {
		t.Errorf("details.workflow_id = %v, want %s", got, groomGuardWorkflow)
	}
	if _, ok := e.Details["parked_stage_id"].(string); !ok {
		t.Errorf("details.parked_stage_id missing: %v", e.Details)
	}
	if _, ok := e.Details["later_gates_not_evaluated"]; !ok {
		t.Errorf("details.later_gates_not_evaluated missing: %v", e.Details)
	}
	for _, verb := range []string{"fishhawk_approve_plan", "fishhawk_reject_plan", "fishhawk approve-review-gate", "fishhawk_cancel_run", parkedRunID.String()} {
		if !strings.Contains(e.Message, verb) {
			t.Errorf("message does not name %q (the remedy): %s", verb, e.Message)
		}
	}
}

// TestGroomStartGuard_RefusesWhileAGroomIsParkedAtItsGate is the headline
// (C13): a groom created through the handler, its plan stage then parked at
// awaiting_approval, refuses a second groom 409 with no second run row.
func TestGroomStartGuard_RefusesWhileAGroomIsParkedAtItsGate(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)

	first := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
	if first.Code != http.StatusCreated {
		t.Fatalf("first groom status = %d, want 201:\n%s", first.Code, first.Body.String())
	}
	firstID := createdRunID(t, first)
	setStageState(t, repo.fakeRepo, firstID, run.StageTypePlan, run.StageStateAwaitingApproval)

	second := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
	assertParkedRefusal(t, second, firstID, run.StageTypePlan)
	if n := runRowCount(repo.fakeRepo); n != 1 {
		t.Errorf("run rows after the refusal = %d, want 1 (a refusal must insert nothing)", n)
	}
}

// TestGroomStartGuard_RefusesAtTheConfirmGate: the groom stage succeeded and
// the human confirm (review) stage is parked.
func TestGroomStartGuard_RefusesAtTheConfirmGate(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)

	first := createRunViaHandler(t, s, groomBody(groomConfirmSpecYAML))
	if first.Code != http.StatusCreated {
		t.Fatalf("first groom status = %d, want 201:\n%s", first.Code, first.Body.String())
	}
	firstID := createdRunID(t, first)
	setStageState(t, repo.fakeRepo, firstID, run.StageTypePlan, run.StageStateSucceeded)
	setStageState(t, repo.fakeRepo, firstID, run.StageTypeReview, run.StageStateAwaitingApproval)

	second := createRunViaHandler(t, s, groomBody(groomConfirmSpecYAML))
	assertParkedRefusal(t, second, firstID, run.StageTypeReview)
	if !strings.Contains(decodeEnvelope(t, second).Message, "confirm gate") {
		t.Errorf("message does not name the confirm gate: %s", second.Body.String())
	}
	if n := runRowCount(repo.fakeRepo); n != 1 {
		t.Errorf("run rows after the refusal = %d, want 1", n)
	}
}

// TestGroomStartGuard_AdmitsWhenThePriorGroomIsTerminal: a cancelled run that
// still carries a stale awaiting_approval stage row does not refuse; the
// store's non-terminal filter drops it (C14).
func TestGroomStartGuard_AdmitsWhenThePriorGroomIsTerminal(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	seedGroomRun(t, repo.fakeRepo, groomSeed{state: run.StateCancelled, parked: run.StageTypePlan})

	w := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (a terminal prior groom never blocks):\n%s", w.Code, w.Body.String())
	}
}

// TestGroomStartGuard_AdmitsWhenNoStageIsParked: an open groom with no stage
// at awaiting_approval does not refuse.
func TestGroomStartGuard_AdmitsWhenNoStageIsParked(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	seedGroomRun(t, repo.fakeRepo, groomSeed{})

	w := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
}

// TestGroomStartGuard_NonGroomingWorkflowIsNotGuarded (C15): a feature_change
// with a parked stage never blocks a second feature_change.
func TestGroomStartGuard_NonGroomingWorkflowIsNotGuarded(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)

	first := createRunViaHandler(t, s, charterRunBody("feature_change", nonGroomingSpecYAML))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201:\n%s", first.Code, first.Body.String())
	}
	setStageState(t, repo.fakeRepo, createdRunID(t, first), run.StageTypePlan, run.StageStateAwaitingApproval)

	second := createRunViaHandler(t, s, charterRunBody("feature_change", nonGroomingSpecYAML))
	if second.Code != http.StatusCreated {
		t.Fatalf("second status = %d, want 201 (only grooming workflows are guarded):\n%s", second.Code, second.Body.String())
	}
}

// TestGroomStartGuard_OtherRepoIsNotGuarded: a parked groom of ANOTHER repo
// does not refuse.
func TestGroomStartGuard_OtherRepoIsNotGuarded(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	seedGroomRun(t, repo.fakeRepo, groomSeed{repo: "other/repo", parked: run.StageTypePlan})

	w := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
}

// TestGroomStartGuard_RunListFaultFailsOpen (C17, ListRuns arm): a ListRuns
// error admits. The parked run would refuse if the list succeeded, so the
// admit is the fail-open arm's verdict.
func TestGroomStartGuard_RunListFaultFailsOpen(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	seedGroomRun(t, repo.fakeRepo, groomSeed{parked: run.StageTypePlan})
	repo.listErr = errors.New("injected list fault")

	w := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (a ListRuns fault fails open):\n%s", w.Code, w.Body.String())
	}
}

// stageListFaultRepo wraps the delegation repo so ListStagesForRun fails for
// ONE run id. newDelegationServer's driveE2ERepo overrides ListStagesForRun, so
// an injector on fakeRepo would never fire (N4).
type stageListFaultRepo struct {
	*driveE2ERepo
	faultRunID uuid.UUID
}

func (r *stageListFaultRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if runID == r.faultRunID {
		return nil, errors.New("injected stage-list fault")
	}
	return r.driveE2ERepo.ListStagesForRun(ctx, runID)
}

// TestGroomStartGuard_StageListFaultFailsOpen (C17, stage-list arm): the only
// parked run's stage list fails, so the guard skips it and admits.
func TestGroomStartGuard_StageListFaultFailsOpen(t *testing.T) {
	base := &driveE2ERepo{fakeRepo: newFakeRepo()}
	parked := seedGroomRun(t, base.fakeRepo, groomSeed{parked: run.StageTypePlan})
	repo := &stageListFaultRepo{driveE2ERepo: base, faultRunID: parked.ID}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: newAuditFake(), ConcernRepo: newFakeConcernRepo()})
	stubConventions(t, conventionsWithCharter(t), nil)

	w := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (a stage-list fault fails open):\n%s", w.Code, w.Body.String())
	}
}

// TestGroomStartGuard_FullScanWindowFailsOpen (C17, full-window arm): one
// OLDER parked groom behind groomStartGuardScanLimit newer open, unparked
// grooms is outside the window, so the guard admits with a Warn. This pins the
// bound's direction: newest first, oldest unseen.
func TestGroomStartGuard_FullScanWindowFailsOpen(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	base := time.Now().UTC().Add(-time.Hour)
	seedGroomRun(t, repo.fakeRepo, groomSeed{parked: run.StageTypePlan, createdAt: base})
	for i := 1; i <= groomStartGuardScanLimit; i++ {
		seedGroomRun(t, repo.fakeRepo, groomSeed{createdAt: base.Add(time.Duration(i) * time.Second)})
	}

	w := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (a full scan window fails open):\n%s", w.Code, w.Body.String())
	}
}

// TestGroomStartGuard_ParkedGroomBehindSixtyTerminalRunsIsRefused (C14): one
// OLDEST parked groom behind 60 NEWER cancelled grooms is still found, because
// the terminal filter runs in the store before the LIMIT. The fake honours
// newest-first ordering and Limit, so dropping NonTerminal leaves a 50-row
// window of cancelled runs and admits.
func TestGroomStartGuard_ParkedGroomBehindSixtyTerminalRunsIsRefused(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	base := time.Now().UTC().Add(-time.Hour)
	oldest := seedGroomRun(t, repo.fakeRepo, groomSeed{parked: run.StageTypePlan, createdAt: base})
	for i := 1; i <= 60; i++ {
		seedGroomRun(t, repo.fakeRepo, groomSeed{state: run.StateCancelled, createdAt: base.Add(time.Duration(i) * time.Second)})
	}

	w := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
	assertParkedRefusal(t, w, oldest.ID, run.StageTypePlan)
	if !repo.lastListFilter.NonTerminal {
		t.Errorf("guard ListRuns filter = %+v, want NonTerminal true", repo.lastListFilter)
	}
}

// TestGroomStartGuard_OtherTenantsParkedGroomIsInvisible (C16, M6f): tenant B's
// parked groom never blocks tenant A's POST or an untenanted scheduled start,
// and neither answer names B's run. An UNTENANTED parked groom does refuse
// tenant A, which pins the visibility rule (own tenant plus untenanted).
func TestGroomStartGuard_OtherTenantsParkedGroomIsInvisible(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	tenantB := uuid.New().String()
	if tenantB == testOperatorAccountID {
		t.Fatal("tenant B collides with the test operator's account")
	}
	bRun := seedGroomRun(t, repo.fakeRepo, groomSeed{repo: scheduledParams("").Repo, parked: run.StageTypePlan, accountID: tenantB})

	// Tenant A (the test operator identity) POSTs.
	body := groomBody(groomScheduledSpecYAML)
	body["repo"] = scheduledParams("").Repo
	body["trigger_source"] = "cli"
	body["applies_to_override"] = true
	body["applies_to_override_reason"] = "tenancy fixture: an on-demand start of a scheduled groom"
	w := createRunViaHandler(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("tenant A status = %d, want 201 (B's parked groom is invisible):\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), bRun.ID.String()) {
		t.Errorf("tenant A's response names tenant B's run %s", bRun.ID)
	}

	// The untenanted scheduler: its query carries no account constraint, so
	// only groomGuardTenantVisible keeps B's row out.
	p := scheduledParams(groomScheduledSpecYAML)
	p.WorkflowID = groomGuardWorkflow
	out, err := s.StartScheduledRun(context.Background(), p)
	if err != nil {
		t.Fatalf("StartScheduledRun: %v", err)
	}
	if out.Kind != ScheduledStartStarted {
		t.Fatalf("scheduled outcome = %+v, want started (B's parked groom is invisible to the untenanted scheduler)", out)
	}
	if strings.Contains(out.Message, bRun.ID.String()) {
		t.Errorf("scheduled outcome names tenant B's run %s", bRun.ID)
	}

	// An untenanted parked groom IS visible to tenant A.
	uRun := seedGroomRun(t, repo.fakeRepo, groomSeed{repo: scheduledParams("").Repo, parked: run.StageTypePlan})
	w = createRunViaHandler(t, s, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("tenant A status = %d, want 409 (an untenanted parked groom is visible):\n%s", w.Code, w.Body.String())
	}
	if got := decodeEnvelope(t, w).Details["parked_run_id"]; got != uRun.ID.String() {
		t.Errorf("parked_run_id = %v, want the untenanted run %s", got, uRun.ID)
	}
}

// TestGroomStartGuard_ReplayShortCircuitsBeforeGuard (C18, replay half): a
// replay of the parked run's own Idempotency-Key answers 200 with the same id.
func TestGroomStartGuard_ReplayShortCircuitsBeforeGuard(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	const key = "groom-guard-replay"

	first := createRunViaHandlerWithKey(t, s, groomBody(groomingSpecYAML), key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201:\n%s", first.Code, first.Body.String())
	}
	firstID := createdRunID(t, first)
	setStageState(t, repo.fakeRepo, firstID, run.StageTypePlan, run.StageStateAwaitingApproval)

	replay := createRunViaHandlerWithKey(t, s, groomBody(groomingSpecYAML), key)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 (the replay short-circuits before the guard):\n%s", replay.Code, replay.Body.String())
	}
	if got := createdRunID(t, replay); got != firstID {
		t.Errorf("replay id = %s, want %s", got, firstID)
	}
}

// TestGroomStartGuard_RefusalAppendsNoOverrideGrant (C18, pre-admission half):
// a refused groom whose request carries an applies_to override appends no
// run_admitted_applies_to_override grant. Non-vacuity: once the parked groom is
// cancelled, the SAME request is admitted AND granted.
func TestGroomStartGuard_RefusalAppendsNoOverrideGrant(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	parked := seedGroomRun(t, repo.fakeRepo, groomSeed{parked: run.StageTypePlan})

	body := groomBody(groomScheduledSpecYAML) // admits only scheduled
	body["trigger_source"] = "cli"
	body["applies_to_override"] = true
	body["applies_to_override_reason"] = "override fixture"

	w := createRunViaHandler(t, s, body)
	assertParkedRefusal(t, w, parked.ID, run.StageTypePlan)
	if n := countGlobalAudits(au, "run_admitted_applies_to_override"); n != 0 {
		t.Fatalf("run_admitted_applies_to_override grants after the refusal = %d, want 0", n)
	}

	setRunState(repo.fakeRepo, parked.ID, run.StateCancelled)
	w = createRunViaHandler(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status after cancel = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if n := countGlobalAudits(au, "run_admitted_applies_to_override"); n != 1 {
		t.Errorf("grants after the admitted request = %d, want 1 (the fixture must reach the override grant)", n)
	}
}

// TestGroomStartGuard_ScheduledStartIsRefused is the CROSS-BOUNDARY case:
// Server.StartScheduledRun → the real handleCreateRun → classifyScheduledStart,
// with an untenanted parked groom, yields ScheduledStartRefused carrying
// grooming_run_parked and 409, the outcome the scheduler records as
// scheduled_run_refused.
func TestGroomStartGuard_ScheduledStartIsRefused(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	p := scheduledParams(groomScheduledSpecYAML)
	p.WorkflowID = groomGuardWorkflow
	parked := seedGroomRun(t, repo.fakeRepo, groomSeed{repo: p.Repo, parked: run.StageTypeReview})

	out, err := s.StartScheduledRun(context.Background(), p)
	if err != nil {
		t.Fatalf("StartScheduledRun: %v", err)
	}
	if out.Kind != ScheduledStartRefused || out.Code != groomingRunParkedCode || out.Status != http.StatusConflict {
		t.Fatalf("outcome = %+v, want refused/409 %s", out, groomingRunParkedCode)
	}
	if !strings.Contains(out.Message, parked.ID.String()) {
		t.Errorf("refusal message does not name the parked run %s: %s", parked.ID, out.Message)
	}
	if n := runRowCount(repo.fakeRepo); n != 1 {
		t.Errorf("run rows = %d, want 1 (the refused window minted nothing)", n)
	}
}

// TestGroomStartGuard_ClearedGroomIsNoLongerRefused is the operator remedy
// loop (N5b): cancel every parked run named by successive 409s, and the start
// is then admitted (201) or meets the next gate (422 charter_required), never
// grooming_run_parked again.
func TestGroomStartGuard_ClearedGroomIsNoLongerRefused(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	for i := 0; i < 3; i++ {
		seedGroomRun(t, repo.fakeRepo, groomSeed{parked: run.StageTypePlan, createdAt: time.Now().UTC().Add(time.Duration(i) * time.Second)})
	}

	cancelled := map[string]bool{}
	for attempt := 0; attempt < 10; attempt++ {
		w := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
		if w.Code != http.StatusConflict {
			charterRefused := w.Code == http.StatusUnprocessableEntity && decodeEnvelope(t, w).Code == "charter_required"
			if w.Code != http.StatusCreated && !charterRefused {
				t.Fatalf("status = %d, want 201 or 422 charter_required once cleared:\n%s", w.Code, w.Body.String())
			}
			if len(cancelled) != 3 {
				t.Errorf("cleared after cancelling %d parked runs, want 3", len(cancelled))
			}
			return
		}
		e := decodeEnvelope(t, w)
		if e.Code != groomingRunParkedCode {
			t.Fatalf("409 code = %q, want %q", e.Code, groomingRunParkedCode)
		}
		id, _ := e.Details["parked_run_id"].(string)
		if cancelled[id] {
			t.Fatalf("run %s named again after it was cancelled", id)
		}
		cancelled[id] = true
		setRunState(repo.fakeRepo, uuid.MustParse(id), run.StateCancelled)
	}
	t.Fatal("still refused after 10 attempts")
}
