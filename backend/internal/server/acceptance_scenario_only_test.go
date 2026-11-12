package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// acceptance_scenario_only_test.go is the #4873 cross-boundary seam: a
// scenario-only replay regression (run d9862ab0, outcome seq 152809 / triage
// seq 152816) shipped through the REAL acceptance ingest pages class 4 without
// spending a fix-up pass, and both operator routes work on the SAME server and
// the SAME ordered audit chain — arbitration (the stale-expectation route) and
// a manual operator fix-up whose push re-opens acceptance (the genuine-
// regression route, admitted only while review is parked at awaiting_approval).

// scenarioOnlySpecYAML declares the feature_change order the manual fix-up
// precondition depends on: plan → implement → acceptance → review (approval
// gate), so acceptance settles BEFORE review opens.
const scenarioOnlySpecYAML = `version: "1.1"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
      - id: review
        type: review
        executor:
          human: true
        gates:
          - type: approval
            approvals:
              count: 1
`

const (
	scenarioOnlyPRHead   = "4873000000000000000000000000000000000001"
	scenarioOnlyFixupSHA = "4873000000000000000000000000000000000002"
	scenarioOnlyOriginPR = 3917
)

type scenarioOnlySeam struct {
	s                                   *Server
	rr                                  *promptRunRepo
	au                                  *orderedAuditFake
	runRow                              *run.Run
	plan, implement, acceptance, review *run.Stage
	priv                                ed25519.PrivateKey
}

// newScenarioOnlySeam wires ONE Server over promptRunRepo + orderedAuditFake
// (which sequences BOTH ListForRun and ListForRunByCategory off one store —
// acceptanceArbitrationDischarges reads ListForRun, which the plain auditFake
// leaves at sequence 0). plan, implement and acceptance succeeded; review is
// reviewState. A pull_request_opened head and a stage-scoped
// acceptance_dispatched anchor sit on the same ordered chain.
func newScenarioOnlySeam(t *testing.T, reviewState run.StageState) *scenarioOnlySeam {
	t.Helper()
	runID := uuid.New()
	rr := newPromptRunRepo()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newOrderedAuditFake()

	prURL := "https://github.com/kuhlman-labs/fishhawk/pull/4873"
	runRow := &run.Run{
		ID: runID, Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
		State: run.StateRunning, WorkflowSpec: []byte(scenarioOnlySpecYAML),
		RunnerKind: run.RunnerKindLocal, RunnerKindResolved: true,
		PullRequestURL: &prURL,
	}
	rr.getRuns[runID] = runRow
	mk := func(seq int, typ run.StageType, exec run.ExecutorKind, state run.StageState) *run.Stage {
		st := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: seq, Type: typ, ExecutorKind: exec, State: state}
		rr.getStages[st.ID] = st
		return st
	}
	planStage := mk(1, run.StageTypePlan, run.ExecutorAgent, run.StageStateSucceeded)
	implement := mk(2, run.StageTypeImplement, run.ExecutorAgent, run.StageStateSucceeded)
	acceptance := mk(3, run.StageTypeAcceptance, run.ExecutorAgent, run.StageStateSucceeded)
	review := mk(4, run.StageTypeReview, run.ExecutorHuman, reviewState)
	review.RequiresApproval = true
	review.Gate = &run.Gate{Kind: run.GateKindApproval}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {planStage, implement, acceptance, review}}

	v := "standard_v1"
	ar.all = append(ar.all, &artifact.Artifact{
		ID: uuid.New(), StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &v,
		Content: acceptancePlanArtifactContent(t), CreatedAt: time.Now().UTC(),
	})

	au.seedEntry(runID, "pull_request_opened", map[string]any{"head_sha": scenarioOnlyPRHead, "branch": "fishhawk/run-4873/stage-impl", "pr_number": 4873})
	anchor, _ := json.Marshal(map[string]any{"stage_id": acceptance.ID.String()})
	au.record(runID, &acceptance.ID, CategoryAcceptanceDispatched, anchor)

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr, SigningRepo: sf, ArtifactRepo: ar,
		AuditRepo: au, ConcernRepo: newFakeConcernRepo(),
	})
	priv, _ := sf.issue(t, runID)
	return &scenarioOnlySeam{s: s, rr: rr, au: au, runRow: runRow,
		plan: planStage, implement: implement, acceptance: acceptance, review: review, priv: priv}
}

// scenarioOnly152809Body is the 152809 verdict on the wire: assertion_fail,
// 1 passed + 6 skipped plan criteria (each skip with expectation_basis), and 25
// replayed scenario rows (16 passed, 8 skipped, 1 failed) under a runner replay
// header serving 25 of a 246-scenario corpus; the failed scenario's origin_pr
// is 3917.
func scenarioOnly152809Body(t *testing.T) []byte {
	t.Helper()
	rows := shape152809Rows()
	var served []acceptanceReplayedScenario
	for _, r := range rows {
		if !isScenarioRow(r.ID) {
			continue
		}
		e := acceptanceReplayedScenario{ScenarioID: r.ID, OriginPR: 3000, OriginIssue: 3000, OriginRunID: uuid.NewString(), Path: "acceptance/scenarios/x.yaml"}
		if r.ID == staleScenarioID {
			e.OriginPR, e.OriginIssue, e.Path = scenarioOnlyOriginPR, 3722, "acceptance/scenarios/issue-3722/accepted-requires-acceptance-statement.yaml"
		}
		served = append(served, e)
	}
	if len(served) != 25 {
		t.Fatalf("fixture: %d scenario rows, want 25", len(served))
	}
	body, err := json.Marshal(acceptanceBody{
		Verdict: acceptanceVerdictFailed, FailureMode: acceptanceFailureAssertionFail,
		Criteria: critRaw(rows...), TargetURL: "http://127.0.0.1:8080",
		Replay: &acceptanceReplay{Cap: 25, CorpusSize: 246, Served: 25, SampledOut: 221, Seed: "seed-152809", Scenarios: served},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// rows returns the run's chain entries of one category, in sequence order.
func (f *scenarioOnlySeam) rows(category string) []*audit.Entry {
	return f.au.snapshot(f.runRow.ID, category)
}

func (f *scenarioOnlySeam) gate(t *testing.T) string {
	t.Helper()
	stages, err := f.rr.ListStagesForRun(context.Background(), f.runRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.s.acceptanceGateState(context.Background(), f.runRow, stages)
	if err != nil {
		t.Fatalf("acceptanceGateState: %v", err)
	}
	return g
}

func payloadOf(t *testing.T, e *audit.Entry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("unmarshal %s payload: %v", e.Category, err)
	}
	return m
}

func assertIDs(t *testing.T, what string, raw any, want ...string) {
	t.Helper()
	got, _ := raw.([]any)
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, raw, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %v, want %q", what, i, got[i], want[i])
		}
	}
}

// shipScenarioOnly is the shared prefix: the 152809 verdict through the real
// handleShipAcceptance pages class 4 keyed on the scenario id, spends no fix-up,
// and parks the gate at acceptance_triage. Returns the shipped outcome entry.
func (f *scenarioOnlySeam) shipScenarioOnly(t *testing.T) *audit.Entry {
	t.Helper()
	w := shipAcceptanceRequest(t, f.s, f.runRow.ID, f.acceptance.ID, f.priv, scenarioOnly152809Body(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if n := len(f.rows(CategoryAcceptanceScenarioRegression)); n != 1 {
		t.Fatalf("acceptance_scenario_regression rows = %d, want 1", n)
	}
	outcomes := f.rows(CategoryAcceptanceOutcomeRecorded)
	if len(outcomes) != 1 {
		t.Fatalf("acceptance_outcome_recorded rows = %d, want 1", len(outcomes))
	}
	op := payloadOf(t, outcomes[0])
	if op["verdict"] != acceptanceVerdictFailed || op["criteria_failed"] != float64(0) {
		t.Errorf("outcome verdict/criteria_failed = %v/%v, want failed/0", op["verdict"], op["criteria_failed"])
	}
	if replay, _ := op["replay"].(map[string]any); replay["scenarios_failed"] != float64(1) {
		t.Errorf("outcome replay.scenarios_failed = %v, want 1", replay["scenarios_failed"])
	}
	assertIDs(t, "outcome failed_scenario_ids", op["failed_scenario_ids"], staleScenarioID)

	triage := f.rows(CategoryAcceptanceTriageDecided)
	if len(triage) != 1 {
		t.Fatalf("acceptance_triage_decided rows = %d, want 1", len(triage))
	}
	tp := payloadOf(t, triage[0])
	if tp["class"] != acceptanceClass4 || tp["disposition"] != acceptanceDispositionPaged {
		t.Fatalf("triage class/disposition = %v/%v, want 4/paged (152816 recorded 1/fixup_dispatched)", tp["class"], tp["disposition"])
	}
	assertIDs(t, "triage criterion_ids", tp["criterion_ids"], staleScenarioID)
	if n := len(f.rows(CategoryStageFixupTriggered)); n != 0 {
		t.Fatalf("stage_fixup_triggered rows = %d, want 0 (a scenario-only page spends no fix-up)", n)
	}
	if f.implement.State != run.StageStateSucceeded || f.acceptance.State != run.StageStateSucceeded {
		t.Errorf("implement/acceptance = %s/%s, want succeeded/succeeded (class 4 takes no transition)", f.implement.State, f.acceptance.State)
	}
	if g := f.gate(t); g != acceptanceGateTriage {
		t.Fatalf("gate = %q, want %q", g, acceptanceGateTriage)
	}
	return outcomes[0]
}

// TestAcceptanceSeam_ScenarioOnlyReplay is the #4873 done-means seam (R3, R4,
// R5): wire body → validate partition → classifier → triage router → outcome /
// audit payloads → acceptanceGateState → arbitration handler / fix-up handler →
// PR fixup_pushed report → reopenAcceptanceOnFixupPush, on one Server and one
// ordered chain.
func TestAcceptanceSeam_ScenarioOnlyReplay(t *testing.T) {
	t.Run("arbitrate_discharges_page", func(t *testing.T) {
		f := newScenarioOnlySeam(t, run.StageStateAwaitingApproval)
		outcome := f.shipScenarioOnly(t)

		w := postArbitration(t, f.s, f.runRow.ID, acceptanceArbitrationRequest{
			Reason: "scenario expectation superseded by a later ruling; retiring it on main",
		}, withMergeOperator)
		if w.Code != http.StatusOK {
			t.Fatalf("arbitration status = %d, want 200 (reason-only discharge of a scenario-only page):\n%s", w.Code, w.Body.String())
		}
		arb := f.rows(CategoryAcceptanceTriageArbitrated)
		if len(arb) != 1 {
			t.Fatalf("acceptance_triage_arbitrated rows = %d, want 1", len(arb))
		}
		ap := payloadOf(t, arb[0])
		if ap["outcome_sequence"] != float64(outcome.Sequence) {
			t.Errorf("outcome_sequence = %v, want the shipped outcome's sequence %d", ap["outcome_sequence"], outcome.Sequence)
		}
		if ap["triage_class"] != acceptanceClass4 || ap["triage_disposition"] != acceptanceDispositionPaged {
			t.Errorf("arbitration triage class/disposition = %v/%v, want 4/paged", ap["triage_class"], ap["triage_disposition"])
		}
		assertIDs(t, "arbitration failed_scenario_ids", ap["failed_scenario_ids"], staleScenarioID)
		if g := f.gate(t); g != acceptanceGateArbitrated {
			t.Errorf("gate after arbitration = %q, want %q", g, acceptanceGateArbitrated)
		}
	})

	t.Run("operator_fixup_reopens_acceptance", func(t *testing.T) {
		f := newScenarioOnlySeam(t, run.StageStateAwaitingApproval)
		f.shipScenarioOnly(t)

		const concern = "the replayed scenario issue-3722/accepted-requires-acceptance-statement is a genuine regression; restore the acceptance-statement check"
		w := postFixup(t, f.s, f.implement.ID, fixupRequest{OperatorConcern: concern, Reason: "genuine scenario regression"})
		if w.Code != http.StatusOK {
			t.Fatalf("operator fix-up status = %d, want 200 (review is awaiting_approval):\n%s", w.Code, w.Body.String())
		}
		if f.implement.State != run.StageStatePending {
			t.Errorf("implement = %s, want pending", f.implement.State)
		}
		if f.review.State != run.StageStatePending {
			t.Errorf("review = %s, want re-parked to pending", f.review.State)
		}
		fix := f.rows(CategoryStageFixupTriggered)
		if len(fix) != 1 {
			t.Fatalf("stage_fixup_triggered rows = %d, want 1", len(fix))
		}
		if !strings.Contains(string(fix[0].Payload), "genuine regression; restore the acceptance-statement check") {
			t.Errorf("stage_fixup_triggered payload does not carry the operator concern:\n%s", fix[0].Payload)
		}
		if fp := payloadOf(t, fix[0]); fp["reparked_review_stage_id"] != f.review.ID.String() {
			t.Errorf("reparked_review_stage_id = %v, want %s", fp["reparked_review_stage_id"], f.review.ID)
		}
		if f.acceptance.State != run.StageStateSucceeded {
			t.Errorf("acceptance = %s before the push, want succeeded", f.acceptance.State)
		}

		// The runner claims the pass, then reports the fix-up push at a NEW head.
		f.implement.State = run.StageStateRunning
		body, _ := json.Marshal(map[string]any{
			"outcome": "fixup_pushed", "branch": "fishhawk/run-4873/stage-impl",
			"head_sha": scenarioOnlyFixupSHA, "base_sha": scenarioOnlyPRHead, "files_changed_count": 1,
		})
		w = shipPRRequest(t, f.s, f.runRow.ID, f.implement.ID, f.priv, body, "")
		if w.Code != http.StatusOK {
			t.Fatalf("fixup_pushed status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		pushed := f.rows(CategoryFixupPushed)
		if len(pushed) != 1 {
			t.Fatalf("fixup_pushed rows = %d, want 1", len(pushed))
		}
		reopened := f.rows(CategoryAcceptanceReopened)
		if len(reopened) != 1 {
			t.Fatalf("acceptance_reopened rows = %d, want 1 (the fix-up push re-opens acceptance)", len(reopened))
		}
		if reopened[0].Sequence <= pushed[0].Sequence {
			t.Errorf("acceptance_reopened seq %d not above fixup_pushed seq %d", reopened[0].Sequence, pushed[0].Sequence)
		}
		if f.acceptance.State == run.StageStateSucceeded {
			t.Errorf("acceptance still succeeded after the fix-up push, want re-opened")
		}
	})

	t.Run("review_resolved_refuses_operator_fixup", func(t *testing.T) {
		// The review-first resolved shape: pins the PRECONDITION, not the route.
		f := newScenarioOnlySeam(t, run.StageStateSucceeded)
		f.shipScenarioOnly(t)

		w := postFixup(t, f.s, f.implement.ID, fixupRequest{OperatorConcern: "genuine regression", Reason: "r"})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("operator fix-up status = %d, want 422 once review succeeded:\n%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "fixup_not_applicable") || !strings.Contains(w.Body.String(), `review stage is already resolved (state \"succeeded\")`) {
			t.Errorf("refusal body = %s, want fixup_not_applicable naming the resolved (succeeded) review stage", w.Body.String())
		}
		if n := len(f.rows(CategoryStageFixupTriggered)); n != 0 {
			t.Errorf("stage_fixup_triggered rows = %d, want 0 on the refusal", n)
		}
		if f.implement.State != run.StageStateSucceeded {
			t.Errorf("implement = %s, want still succeeded", f.implement.State)
		}
	})
}
