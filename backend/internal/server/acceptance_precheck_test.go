package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// specWithAcceptanceStage is a v1.1 feature_change workflow whose stages
// include an acceptance stage — the trigger resolveAcceptanceStage looks
// for. runAcceptancePrecheck evaluates a plan's acceptance_criteria only
// when such a stage is configured.
var specWithAcceptanceStage = []byte(`version: "1.1"
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
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
`)

// specNoAcceptanceStage is a workflow with no acceptance stage — the
// stage-conditional off switch: runAcceptancePrecheck writes no entry and
// returns nil.
var specNoAcceptanceStage = []byte(`version: "0.3"
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
`)

// newAcceptancePrecheckServer wires a Server with a run carrying the given
// workflow spec, returning the server, the audit fake, and the run row so
// callers can drive runAcceptancePrecheck and read back the appended
// plan_acceptance_precheck entry.
func newAcceptancePrecheckServer(t *testing.T, workflowSpec []byte) (*Server, *auditFake, *run.Run) {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newAuditFake()

	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = workflowSpec

	s := New(Config{
		Addr:      "127.0.0.1:0",
		AuditRepo: au,
		RunRepo:   rr,
	})
	return s, au, runRow
}

// acceptancePlanBody builds a standard_v1 plan body whose verification block
// carries the given acceptance_criteria and out_of_scope. It does NOT
// schema-validate: several tests deliberately craft bodies the schema would
// reject (missing rationale) or plan.Parse would reject (duplicate id), to
// exercise the raw-decode path independent of upload-order assumptions.
func acceptancePlanBody(t *testing.T, criteria []map[string]any, outOfScope []string) []byte {
	t.Helper()
	verification := map[string]any{
		"test_strategy": "Run the tests.",
		"rollback_plan": "Revert the PR.",
	}
	if criteria != nil {
		verification["acceptance_criteria"] = toAnySlice(criteria)
	}
	if outOfScope != nil {
		verification["out_of_scope"] = toStringAnySlice(outOfScope)
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["verification"] = verification
	})
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return body
}

func toAnySlice(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, m := range in {
		out = append(out, m)
	}
	return out
}

func toStringAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// lastAcceptancePrecheckEntry decodes the single plan_acceptance_precheck
// payload the audit fake captured, failing when none was written.
func lastAcceptancePrecheckEntry(t *testing.T, au *auditFake) AcceptancePrecheckPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var payloads []AcceptancePrecheckPayload
	for _, ap := range au.appended {
		if ap.Category != categoryPlanAcceptancePrecheck {
			continue
		}
		var p AcceptancePrecheckPayload
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("unmarshal acceptance precheck payload: %v", err)
		}
		payloads = append(payloads, p)
	}
	if len(payloads) != 1 {
		t.Fatalf("want exactly 1 plan_acceptance_precheck entry, got %d", len(payloads))
	}
	return payloads[0]
}

func countAcceptancePrecheckEntries(au *auditFake) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, ap := range au.appended {
		if ap.Category == categoryPlanAcceptancePrecheck {
			n++
		}
	}
	return n
}

func hasAcceptanceFinding(p AcceptancePrecheckPayload, rule string) *AcceptanceFinding {
	for i := range p.Findings {
		if p.Findings[i].Rule == rule {
			return &p.Findings[i]
		}
	}
	return nil
}

// (1) A workflow without an acceptance stage: stage-conditional off switch —
// nil result, NO audit entry.
func TestAcceptancePrecheck_NoAcceptanceStage_NoEntry(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specNoAcceptanceStage)
	// Even a plan with an obvious defect (no blocking criterion) must produce
	// nothing when the workflow configures no acceptance stage.
	body := acceptancePlanBody(t, nil, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	if got != nil {
		t.Fatalf("want nil result when no acceptance stage is configured; got %+v", got)
	}
	if n := countAcceptancePrecheckEntries(au); n != 0 {
		t.Fatalf("want no entry when no acceptance stage is configured; got %d", n)
	}
}

// (2) Acceptance stage + no criteria + no out_of_scope -> no_blocking_criterion
// finding persisted.
func TestAcceptancePrecheck_NoCriteriaNoOutOfScope_Flags(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, nil, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result when an acceptance stage is configured")
	}
	if got.AcceptanceStageID != "acceptance" {
		t.Errorf("AcceptanceStageID = %q, want acceptance", got.AcceptanceStageID)
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	if hasAcceptanceFinding(entry, acceptanceRuleNoBlockingCriterion) == nil {
		t.Fatalf("want a no_blocking_criterion finding; got %+v", entry.Findings)
	}
}

// (3) No criteria but a non-empty out_of_scope: justified absence -> clean
// entry (no no_blocking_criterion finding).
func TestAcceptancePrecheck_OutOfScopeSuppressesNoBlocking(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, nil, []string{"performance tuning deferred to a follow-up"})

	s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	entry := lastAcceptancePrecheckEntry(t, au)
	if f := hasAcceptanceFinding(entry, acceptanceRuleNoBlockingCriterion); f != nil {
		t.Fatalf("out_of_scope must suppress no_blocking_criterion; got %+v", entry.Findings)
	}
	if len(entry.Findings) != 0 {
		t.Fatalf("want zero findings; got %+v", entry.Findings)
	}
	if entry.OutOfScopeCount != 1 {
		t.Errorf("OutOfScopeCount = %d, want 1", entry.OutOfScopeCount)
	}
}

// (4) Criteria all blocking:false -> no_blocking_criterion finding (the
// nil->true default must be applied only to omitted values, not explicit
// false).
func TestAcceptancePrecheck_AllNonBlocking_Flags(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{"id": "a1", "statement": "does a thing", "source": "explicit", "source_ref": "#1", "blocking": false},
		{"id": "a2", "statement": "does another", "source": "explicit", "source_ref": "#2", "blocking": false},
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result")
	}
	if got.BlockingCount != 0 {
		t.Errorf("BlockingCount = %d, want 0", got.BlockingCount)
	}
	if got.CriteriaCount != 2 {
		t.Errorf("CriteriaCount = %d, want 2", got.CriteriaCount)
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	if hasAcceptanceFinding(entry, acceptanceRuleNoBlockingCriterion) == nil {
		t.Fatalf("all-non-blocking criteria must flag no_blocking_criterion; got %+v", entry.Findings)
	}
}

// TestAcceptancePrecheck_OmittedBlockingCounts asserts the nil->true default:
// a criterion with no blocking key is effectively blocking, so it does NOT
// trip no_blocking_criterion.
func TestAcceptancePrecheck_OmittedBlockingCounts(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{"id": "a1", "statement": "does a thing", "source": "explicit", "source_ref": "#1"},
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got.BlockingCount != 1 {
		t.Errorf("BlockingCount = %d, want 1 (omitted blocking defaults to true)", got.BlockingCount)
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	if hasAcceptanceFinding(entry, acceptanceRuleNoBlockingCriterion) != nil {
		t.Fatalf("an omitted-blocking (effectively blocking) criterion must not flag; got %+v", entry.Findings)
	}
}

// The per-rule behavioral coverage of missing_source_ref, missing_rationale,
// and empty_id moved to backend/internal/plan/acceptance_check_test.go with the
// shared evaluator (#1596); the server tests below keep the integration
// coverage — that a finding flows into the PERSISTED audit payload, that the
// raw-body decode reaches the rule where plan.Parse would reject the plan, and
// the fail-open paths.

// (7) Duplicate id -> duplicate_id finding, proving the raw-body decode path
// works where plan.Parse (semanticCheck) would reject the plan outright.
func TestAcceptancePrecheck_DuplicateID_Flags(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{"id": "dup", "statement": "first", "source": "explicit", "source_ref": "#1", "blocking": true},
		{"id": "dup", "statement": "second", "source": "explicit", "source_ref": "#2", "blocking": true},
	}, nil)

	s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	entry := lastAcceptancePrecheckEntry(t, au)
	f := hasAcceptanceFinding(entry, acceptanceRuleDuplicateID)
	if f == nil {
		t.Fatalf("want a duplicate_id finding; got %+v", entry.Findings)
	}
	if f.CriterionID != "dup" {
		t.Errorf("finding CriterionID = %q, want dup", f.CriterionID)
	}
}

// (8) Fully clean criteria -> entry with findings: [] (checked-and-clean
// distinguishable from never-checked), and the [] not null contract.
func TestAcceptancePrecheck_CleanCriteria_EmptyFindings(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{"id": "a1", "statement": "does a thing", "source": "explicit", "source_ref": "#1", "blocking": true},
		{"id": "a2", "statement": "inferred one", "source": "inferred", "rationale": "derived from the issue", "blocking": false},
	}, nil)

	s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	entry := lastAcceptancePrecheckEntry(t, au)
	if len(entry.Findings) != 0 {
		t.Fatalf("want zero findings on a clean criteria set; got %+v", entry.Findings)
	}
	if entry.BlockingCount != 1 {
		t.Errorf("BlockingCount = %d, want 1", entry.BlockingCount)
	}
	// The payload must marshal findings as [] (not null) so a reader can tell
	// "checked and clean" from "never checked".
	au.mu.Lock()
	var raw map[string]json.RawMessage
	for _, ap := range au.appended {
		if ap.Category == categoryPlanAcceptancePrecheck {
			_ = json.Unmarshal(ap.Payload, &raw)
		}
	}
	au.mu.Unlock()
	if string(raw["findings"]) != "[]" {
		t.Errorf("findings marshaled as %s, want []", raw["findings"])
	}
}

// (9) Audit append failure -> WARN path still returns the computed payload.
func TestAcceptancePrecheck_AppendFailure_StillReturnsPayload(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	au.appendErr = errors.New("audit store down")
	body := acceptancePlanBody(t, nil, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want the computed payload even when the audit append fails")
	}
	if hasAcceptanceFinding(*got, acceptanceRuleNoBlockingCriterion) == nil {
		t.Fatalf("returned payload missing the no_blocking_criterion finding: %+v", got.Findings)
	}
}

// (10) Nil RunRepo/AuditRepo -> nil (fail-open, no panic).
func TestAcceptancePrecheck_NilRepos_ReturnsNil(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	body := acceptancePlanBody(t, nil, nil)
	if got := s.runAcceptancePrecheck(context.Background(), uuid.New(), uuid.New(), body); got != nil {
		t.Fatalf("want nil when repos are unconfigured; got %+v", got)
	}
}

// (11) Malformed raw plan body -> json.Unmarshal error -> fail-open: nil
// result and NO audit entry. The plan Risks section claimed this branch was
// covered by a malformed-body unit test; this pins it. The workflow DOES
// configure an acceptance stage (so resolveAcceptanceStage returns ok and the
// decode is reached), but the body is not valid JSON, so the raw-decode fails
// and the pre-check degrades exactly like the other fail-open paths.
func TestAcceptancePrecheck_MalformedBody_ReturnsNil(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := []byte("{not valid json")

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	if got != nil {
		t.Fatalf("want nil result on an unmarshal error; got %+v", got)
	}
	if n := countAcceptancePrecheckEntries(au); n != 0 {
		t.Fatalf("want no entry on the unmarshal fail-open path; got %d", n)
	}
}

// (12) RunRepo.GetRun error -> fail-open: nil result and NO audit entry. An
// unseeded run id makes the fake's GetRun return run.ErrNotFound, exercising
// the GetRun error branch (the FIRST fail-open path after the nil-repo guard).
func TestAcceptancePrecheck_GetRunError_ReturnsNil(t *testing.T) {
	s, au, _ := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, nil, nil)

	// A run id that was never seeded -> GetRun returns run.ErrNotFound.
	got := s.runAcceptancePrecheck(context.Background(), uuid.New(), uuid.New(), body)

	if got != nil {
		t.Fatalf("want nil result when GetRun fails; got %+v", got)
	}
	if n := countAcceptancePrecheckEntries(au); n != 0 {
		t.Fatalf("want no entry when GetRun fails; got %d", n)
	}
}

// (13) Unparseable workflow spec -> resolveAcceptanceStage's spec.ParseBytes
// error branch -> ok=false -> fail-open: nil result and NO audit entry. This
// pins the parse-error degradation independent of the "no acceptance stage"
// path (TestAcceptancePrecheck_NoAcceptanceStage_NoEntry), which reaches
// resolveAcceptanceStage with a spec that parses cleanly.
func TestAcceptancePrecheck_UnparseableSpec_ReturnsNil(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, []byte("version: \"1.1\"\nworkflows: [unterminated"))
	body := acceptancePlanBody(t, nil, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	if got != nil {
		t.Fatalf("want nil result when the workflow spec is unparseable; got %+v", got)
	}
	if n := countAcceptancePrecheckEntries(au); n != 0 {
		t.Fatalf("want no entry when the workflow spec is unparseable; got %d", n)
	}
}

// TestPlanGateEvidence_AcceptanceMapping asserts the server->prompt mapping:
// a nil acceptance payload leaves the prompt field absent, and a populated
// payload maps every field and finding through to the prompt evidence struct.
func TestPlanGateEvidence_AcceptanceMapping(t *testing.T) {
	// nil acceptance (and all other gates nil) -> nil evidence.
	if ev := planGateEvidence(nil, nil, nil, nil, nil); ev != nil {
		t.Fatalf("all-nil gates must map to nil evidence; got %+v", ev)
	}

	// Populated acceptance payload -> populated evidence, other fields absent.
	acc := &AcceptancePrecheckPayload{
		AcceptanceStageID: "acceptance",
		CriteriaCount:     2,
		BlockingCount:     1,
		OutOfScopeCount:   3,
		Findings: []AcceptanceFinding{
			{Rule: acceptanceRuleMissingSourceRef, CriterionID: "a1", Detail: "no source_ref"},
		},
	}
	ev := planGateEvidence(nil, nil, nil, nil, acc)
	if ev == nil || ev.AcceptancePrecheck == nil {
		t.Fatal("want a populated AcceptancePrecheck evidence")
	}
	if ev.ScopePrecheck != nil || ev.SurfaceSweep != nil || ev.TestSweep != nil || ev.ScopeRegression != nil {
		t.Errorf("only AcceptancePrecheck should be set; got %+v", ev)
	}
	ap := ev.AcceptancePrecheck
	if ap.AcceptanceStageID != "acceptance" || ap.CriteriaCount != 2 || ap.BlockingCount != 1 || ap.OutOfScopeCount != 3 {
		t.Errorf("counts/id not mapped: %+v", ap)
	}
	if len(ap.Findings) != 1 || ap.Findings[0].Rule != acceptanceRuleMissingSourceRef || ap.Findings[0].CriterionID != "a1" {
		t.Errorf("finding not mapped: %+v", ap.Findings)
	}
}

// TestAcceptancePrecheck_ReturnsComputedPayload pins the return contract: the
// returned payload equals the recorded audit payload, so handleShipPlan can
// thread it into the plan-review prompt without a read-back.
func TestAcceptancePrecheck_ReturnsComputedPayload(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{"id": "a1", "statement": "does a thing", "source": "explicit", "blocking": true},
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result")
	}
	recorded := lastAcceptancePrecheckEntry(t, au)
	gotJSON, _ := json.Marshal(got)
	recordedJSON, _ := json.Marshal(recorded)
	if string(gotJSON) != string(recordedJSON) {
		t.Errorf("returned result diverges from the recorded payload:\nreturned: %s\nrecorded: %s", gotJSON, recordedJSON)
	}
}

// (#2512 layer 3) A criterion whose statement requires a capability the
// sandboxed acceptance executor lacks is flagged undecidable_criterion and
// counted in undecidable_count — surfaced through the EXISTING
// plan_acceptance_precheck entry, with no second evaluation call.
func TestAcceptancePrecheck_UndecidableCriterion_FlaggedAndCounted(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{
			"id":         "live-forge",
			"statement":  "a live GitHub round-trip closes the originating issue",
			"source":     "explicit",
			"source_ref": "#2512",
		},
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result when an acceptance stage is configured")
	}
	if got.UndecidableCount != 1 {
		t.Errorf("UndecidableCount = %d, want 1", got.UndecidableCount)
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	f := hasAcceptanceFinding(entry, acceptanceRuleUndecidableCriterion)
	if f == nil {
		t.Fatalf("want an undecidable_criterion finding in the persisted entry; got %+v", entry.Findings)
	}
	if f.CriterionID != "live-forge" {
		t.Errorf("CriterionID = %q, want live-forge", f.CriterionID)
	}
	if entry.UndecidableCount != 1 {
		t.Errorf("persisted undecidable_count = %d, want 1", entry.UndecidableCount)
	}
}

// TestShipPlan_UndecidableCriterion_IsAdvisoryNotARefusal drives the REAL
// plan-upload/admission seam (POST /v0/runs/{run_id}/plan -> handleShipPlan ->
// runAcceptancePrecheck), not the pre-check in isolation: the finding is
// advisory, so the plan carrying an undecidable criterion must still be
// ADMITTED to the operator gate.
//
// COUNTERFACTUAL: teaching handleShipPlan to refuse a plan whose acceptance
// pre-check reports undecidable_criterion (the shape the sibling
// tryScopeRetry / overCapSplitRejection gates use) makes this test go RED on
// the committed stage state. The admission assertion reads COMMITTED state
// back through the repo rather than the response, because a refusal that
// re-opens the stage still answers 201 (trap (a)) — a response-only assertion
// would stay green under the refusal.
func TestShipPlan_UndecidableCriterion_IsAdvisoryNotARefusal(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	// The pre-check is stage-conditional: give the run a workflow that
	// declares an acceptance stage so resolveAcceptanceStage returns ok.
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specWithAcceptanceStage
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := acceptancePlanBody(t, []map[string]any{
		{
			"id":         "live-forge",
			"statement":  "a live GitHub round-trip closes the originating issue",
			"source":     "explicit",
			"source_ref": "#2512",
		},
	}, nil)

	w := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// COMMITTED state first: the plan was ADMITTED — the stage walked to the
	// operator gate, was never re-opened or failed, and the run is alive.
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (an advisory finding must never refuse the plan)\ntransitions: %+v",
			got, rr.stageTransitions)
	}
	if got := rr.stagesByID[planStage.ID].FailureCategory; got != nil {
		t.Errorf("stage carries failure category %q; the undecidable_criterion rule must never fail a plan", *got)
	}
	if st := rr.runs[runRow.ID].State; st == run.StateFailed {
		t.Error("run state = failed; an advisory acceptance finding must never terminate a run")
	}

	// And the advisory finding genuinely fired on THIS upload, so the
	// admission above is not vacuously green on a plan that tripped no rule.
	if n := countAcceptancePrecheckEntries(au.auditFake); n != 1 {
		t.Fatalf("plan_acceptance_precheck entries = %d, want 1", n)
	}
	entry := lastAcceptancePrecheckEntry(t, au.auditFake)
	f := hasAcceptanceFinding(entry, acceptanceRuleUndecidableCriterion)
	if f == nil {
		t.Fatalf("want an undecidable_criterion finding on the admitted plan; got %+v", entry.Findings)
	}
	if f.CriterionID != "live-forge" {
		t.Errorf("CriterionID = %q, want live-forge", f.CriterionID)
	}
	if entry.UndecidableCount != 1 {
		t.Errorf("persisted undecidable_count = %d, want 1", entry.UndecidableCount)
	}
}

// (#2512 layer 3, exemption) A criterion already marked skip_expected with an
// expectation_basis is the sanctioned declaration for undecidable_criterion —
// no finding from THAT rule and undecidable_count stays 0.
//
// #2845 PAIRED HOLE CASE, at the server seam. This fixture is the operator's
// exact case: a criterion naming a LIVE forge round-trip, marked
// skip_expected-with-basis and NOT requires_live_validation. Because
// undecidable_criterion exempts it, the plan gate reported NOTHING and the
// criterion silently lost its auto-filed operator-validation walk.
// undecidable_criterion's behaviour is UNCHANGED (still exempt, still count 0);
// missing_live_validation_marker now fires and live_validation_marker_count is
// 1. The original comment's claim that the criterion draws no finding is no
// longer true and is corrected here.
func TestAcceptancePrecheck_UndecidableCriterion_ExemptWhenDeclared(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{
			"id":                "live-forge",
			"statement":         "a live GitHub round-trip closes the originating issue",
			"source":            "explicit",
			"source_ref":        "#2512",
			"skip_expected":     true,
			"expectation_basis": "pinned by the fake-forge integration test",
		},
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result")
	}
	if got.UndecidableCount != 0 {
		t.Errorf("UndecidableCount = %d, want 0 (declared criteria are exempt)", got.UndecidableCount)
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	if f := hasAcceptanceFinding(entry, acceptanceRuleUndecidableCriterion); f != nil {
		t.Fatalf("a declared criterion must not be flagged; got %+v", *f)
	}
	if entry.UndecidableCount != 0 {
		t.Errorf("persisted undecidable_count = %d, want 0", entry.UndecidableCount)
	}
	// #2845: the hole. The same criterion DOES draw the new rule.
	if got.LiveValidationMarkerCount != 1 {
		t.Errorf("LiveValidationMarkerCount = %d, want 1 (skip_expected-with-basis must not exempt a live target)", got.LiveValidationMarkerCount)
	}
	f := hasAcceptanceFinding(entry, acceptanceRuleMissingLiveValidationMarker)
	if f == nil {
		t.Fatalf("want a missing_live_validation_marker finding in the persisted entry; got %+v", entry.Findings)
	}
	if f.CriterionID != "live-forge" {
		t.Errorf("CriterionID = %q, want live-forge", f.CriterionID)
	}
	if entry.LiveValidationMarkerCount != 1 {
		t.Errorf("persisted live_validation_marker_count = %d, want 1", entry.LiveValidationMarkerCount)
	}
}

// (#2512 layer 3) An ordinary drivable criteria set leaves undecidable_count at
// zero, so the field is a real signal rather than a constant.
func TestAcceptancePrecheck_DrivableCriteria_ZeroUndecidableCount(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{
			"id":         "records-undecidable",
			"statement":  "posting a verdict carrying an undecidable row records verdict=undecidable",
			"source":     "explicit",
			"source_ref": "#2512",
		},
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result")
	}
	if got.UndecidableCount != 0 {
		t.Errorf("UndecidableCount = %d, want 0", got.UndecidableCount)
	}
	// #2845: the same for the live-validation-marker count — a drivable set
	// must leave it at zero so the field is a real signal, not a constant.
	if got.LiveValidationMarkerCount != 0 {
		t.Errorf("LiveValidationMarkerCount = %d, want 0", got.LiveValidationMarkerCount)
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	if entry.UndecidableCount != 0 {
		t.Errorf("persisted undecidable_count = %d, want 0", entry.UndecidableCount)
	}
	if entry.LiveValidationMarkerCount != 0 {
		t.Errorf("persisted live_validation_marker_count = %d, want 0", entry.LiveValidationMarkerCount)
	}
	if f := hasAcceptanceFinding(entry, acceptanceRuleMissingLiveValidationMarker); f != nil {
		t.Errorf("a drivable criteria set must draw no missing_live_validation_marker; got %+v", *f)
	}
}

// (#2845 E54.31 — THE HOLE TEST at the real seam) The #2822 true positive, one
// of the four statements the shipped detector missed, shipped through
// runAcceptancePrecheck: the finding lands in the PERSISTED entry and
// live_validation_marker_count is 1.
func TestAcceptancePrecheck_LiveValidationMarker_FlaggedAndCounted(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{
			"id":         "live-walk",
			"statement":  "A live walk is recorded: one real grooming run against this repo's backlog",
			"source":     "explicit",
			"source_ref": "#2845",
		},
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result when an acceptance stage is configured")
	}
	if got.LiveValidationMarkerCount != 1 {
		t.Errorf("LiveValidationMarkerCount = %d, want 1", got.LiveValidationMarkerCount)
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	f := hasAcceptanceFinding(entry, acceptanceRuleMissingLiveValidationMarker)
	if f == nil {
		t.Fatalf("want a missing_live_validation_marker finding in the persisted entry; got %+v", entry.Findings)
	}
	if f.CriterionID != "live-walk" {
		t.Errorf("CriterionID = %q, want live-walk", f.CriterionID)
	}
	if entry.LiveValidationMarkerCount != 1 {
		t.Errorf("persisted live_validation_marker_count = %d, want 1", entry.LiveValidationMarkerCount)
	}
}

// (#2845 E54.31 — the hole, marker-only exemption at the seam) A live-target
// criterion marked skip_expected with a basis and NO requires_live_validation
// is STILL flagged. This is the shape that silently lost its walk four times.
func TestAcceptancePrecheck_LiveValidationMarker_SkipExpectedOnlyStillFlagged(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{
			"id":                "live-github",
			"statement":         "Validate against live GitHub",
			"source":            "explicit",
			"source_ref":        "#2845",
			"skip_expected":     true,
			"expectation_basis": "pinned by the fake-forge integration test",
		},
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result")
	}
	if got.LiveValidationMarkerCount != 1 {
		t.Errorf("LiveValidationMarkerCount = %d, want 1", got.LiveValidationMarkerCount)
	}
	// The older rule stays exempt — its behaviour is unchanged by this PR.
	if got.UndecidableCount != 0 {
		t.Errorf("UndecidableCount = %d, want 0 (undecidable_criterion must still exempt)", got.UndecidableCount)
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	f := hasAcceptanceFinding(entry, acceptanceRuleMissingLiveValidationMarker)
	if f == nil {
		t.Fatalf("want a missing_live_validation_marker finding in the persisted entry; got %+v", entry.Findings)
	}
	if f.CriterionID != "live-github" {
		t.Errorf("CriterionID = %q, want live-github", f.CriterionID)
	}
	if entry.LiveValidationMarkerCount != 1 {
		t.Errorf("persisted live_validation_marker_count = %d, want 1", entry.LiveValidationMarkerCount)
	}
}

// (#2845 E54.31, exemption) requires_live_validation is the ONE exemption, and
// it holds at the seam: no finding, and the count stays 0.
func TestAcceptancePrecheck_LiveValidationMarker_ExemptWhenMarked(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{
			"id":                       "live-walk",
			"statement":                "A live walk is recorded: one real grooming run against this repo's backlog",
			"source":                   "explicit",
			"source_ref":               "#2845",
			"requires_live_validation": true,
			"skip_expected":            true,
			"expectation_basis":        "pinned by the fake-tracker grooming integration test",
		},
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result")
	}
	if got.LiveValidationMarkerCount != 0 {
		t.Errorf("LiveValidationMarkerCount = %d, want 0 (requires_live_validation is the exemption)", got.LiveValidationMarkerCount)
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	if f := hasAcceptanceFinding(entry, acceptanceRuleMissingLiveValidationMarker); f != nil {
		t.Fatalf("a correctly-marked criterion must not be flagged; got %+v", *f)
	}
	if entry.LiveValidationMarkerCount != 0 {
		t.Errorf("persisted live_validation_marker_count = %d, want 0", entry.LiveValidationMarkerCount)
	}
}

// TestShipPlan_MissingLiveValidationMarker_IsAdvisoryNotARefusal drives the REAL
// plan-upload/admission seam (POST /v0/runs/{run_id}/plan -> handleShipPlan ->
// runAcceptancePrecheck), not the pre-check in isolation: the new finding is
// advisory, so the plan carrying it must still be ADMITTED to the operator gate.
//
// COUNTERFACTUAL: teaching handleShipPlan to refuse a plan whose acceptance
// pre-check reports missing_live_validation_marker makes this test go RED on the
// COMMITTED stage state. The admission assertions read committed state back
// through the repo rather than the response, because a refusal that re-opens the
// stage still answers 201 (trap (a)) — a response-only assertion would stay
// green under the refusal.
func TestShipPlan_MissingLiveValidationMarker_IsAdvisoryNotARefusal(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specWithAcceptanceStage
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := acceptancePlanBody(t, []map[string]any{
		{
			"id":         "grooming-run",
			"statement":  "A real backlog_grooming run against this repository reaches its approval gate",
			"source":     "explicit",
			"source_ref": "#2845",
		},
	}, nil)

	w := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// COMMITTED state first: the plan was ADMITTED.
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (an advisory finding must never refuse the plan)\ntransitions: %+v",
			got, rr.stageTransitions)
	}
	if got := rr.stagesByID[planStage.ID].FailureCategory; got != nil {
		t.Errorf("stage carries failure category %q; the missing_live_validation_marker rule must never fail a plan", *got)
	}
	if st := rr.runs[runRow.ID].State; st == run.StateFailed {
		t.Error("run state = failed; an advisory acceptance finding must never terminate a run")
	}

	// And the advisory finding genuinely fired on THIS upload, so the admission
	// above is not vacuously green on a plan that tripped no rule.
	if n := countAcceptancePrecheckEntries(au.auditFake); n != 1 {
		t.Fatalf("plan_acceptance_precheck entries = %d, want 1", n)
	}
	entry := lastAcceptancePrecheckEntry(t, au.auditFake)
	f := hasAcceptanceFinding(entry, acceptanceRuleMissingLiveValidationMarker)
	if f == nil {
		t.Fatalf("want a missing_live_validation_marker finding on the admitted plan; got %+v", entry.Findings)
	}
	if f.CriterionID != "grooming-run" {
		t.Errorf("CriterionID = %q, want grooming-run", f.CriterionID)
	}
	if entry.LiveValidationMarkerCount != 1 {
		t.Errorf("persisted live_validation_marker_count = %d, want 1", entry.LiveValidationMarkerCount)
	}
}

// ---------------------------------------------------------------------------
// all_criteria_skip_expected / all_skip_short_circuit (#3026, E32.50)
// ---------------------------------------------------------------------------

// allSkipCriterion builds a criterion marked skip_expected with a basis — the
// shape whose whole-plan repetition is the all-skip short-circuit condition.
func allSkipCriterion(id, statement, basis string) map[string]any {
	return map[string]any{
		"id":                id,
		"statement":         statement,
		"source":            "explicit",
		"source_ref":        "#3026",
		"skip_expected":     true,
		"expectation_basis": basis,
	}
}

// (#3026) An all-skip plan body sets AllSkipShortCircuit true and carries the
// all_criteria_skip_expected finding.
func TestAcceptancePrecheck_AllSkip_HeadlineTrue(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		allSkipCriterion("a1", "the renderer emits the consequence line", "covered by the prompt render unit test"),
		allSkipCriterion("a2", "the payload records the headline", "covered by the server payload unit test"),
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result")
	}
	if !got.AllSkipShortCircuit {
		t.Errorf("AllSkipShortCircuit = false, want true on an all-skip plan")
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	f := hasAcceptanceFinding(entry, acceptanceRuleAllCriteriaSkipExpected)
	if f == nil {
		t.Fatalf("want an all_criteria_skip_expected finding; got %+v", entry.Findings)
	}
	if f.CriterionID != "" {
		t.Errorf("CriterionID = %q, want empty (plan-level finding)", f.CriterionID)
	}
	if !entry.AllSkipShortCircuit {
		t.Errorf("persisted all_skip_short_circuit = false, want true")
	}
}

// (#3026) A MIXED-criteria body: one drivable criterion means acceptance
// dispatches normally, so the headline is false and no finding fires.
func TestAcceptancePrecheck_MixedCriteria_HeadlineFalse(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		allSkipCriterion("a1", "the renderer emits the consequence line", "covered by the prompt render unit test"),
		{"id": "a2", "statement": "the plan gate admits the plan", "source": "explicit", "source_ref": "#3026"},
	}, nil)

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result")
	}
	if got.AllSkipShortCircuit {
		t.Errorf("AllSkipShortCircuit = true, want false when a drivable criterion exists")
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	if hasAcceptanceFinding(entry, acceptanceRuleAllCriteriaSkipExpected) != nil {
		t.Errorf("want no all_criteria_skip_expected finding on a mixed plan; got %+v", entry.Findings)
	}
}

// (#3026) A ZERO-criteria body takes the disjoint empty-criteria short-circuit,
// not this one: headline false, no finding.
func TestAcceptancePrecheck_ZeroCriteria_HeadlineFalse(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, nil, []string{"a doc-only change authors no criteria"})

	got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result")
	}
	if got.AllSkipShortCircuit {
		t.Errorf("AllSkipShortCircuit = true, want false on a zero-criteria plan")
	}
	entry := lastAcceptancePrecheckEntry(t, au)
	if hasAcceptanceFinding(entry, acceptanceRuleAllCriteriaSkipExpected) != nil {
		t.Errorf("want no all_criteria_skip_expected finding on a zero-criteria plan; got %+v", entry.Findings)
	}
}

// (#3026, CONDITION 2) The audit record is a MACHINE-READ surface, so the wire
// key is part of the contract. all_skip_short_circuit carries NO omitempty
// deliberately: it is PRESENT on every plan_acceptance_precheck payload —
// present-and-true on an all-skip plan, present-and-FALSE on a mixed one. That
// is the additive audit-wire change the rollback note states, and this test is
// what would go red if an omitempty were added behind the prose's back.
func TestAcceptancePrecheck_AllSkipWireKey_PresentOnEveryPayload(t *testing.T) {
	marshalled := func(t *testing.T, criteria []map[string]any) map[string]any {
		t.Helper()
		s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
		body := acceptancePlanBody(t, criteria, nil)
		if got := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body); got == nil {
			t.Fatal("want a non-nil result")
		}
		au.mu.Lock()
		defer au.mu.Unlock()
		for _, ap := range au.appended {
			if ap.Category != categoryPlanAcceptancePrecheck {
				continue
			}
			var raw map[string]any
			if err := json.Unmarshal(ap.Payload, &raw); err != nil {
				t.Fatalf("unmarshal raw payload: %v", err)
			}
			return raw
		}
		t.Fatal("no plan_acceptance_precheck entry appended")
		return nil
	}

	allSkip := marshalled(t, []map[string]any{
		allSkipCriterion("a1", "the renderer emits the consequence line", "covered by a unit test"),
	})
	v, ok := allSkip["all_skip_short_circuit"]
	if !ok {
		t.Fatalf("all-skip payload is missing the all_skip_short_circuit key: %v", allSkip)
	}
	if v != true {
		t.Errorf("all_skip_short_circuit = %v, want true", v)
	}

	mixed := marshalled(t, []map[string]any{
		allSkipCriterion("a1", "the renderer emits the consequence line", "covered by a unit test"),
		{"id": "a2", "statement": "the plan gate admits the plan", "source": "explicit", "source_ref": "#3026"},
	})
	mv, ok := mixed["all_skip_short_circuit"]
	if !ok {
		t.Fatalf("mixed-criteria payload is missing the all_skip_short_circuit key — the tag must carry NO omitempty: %v", mixed)
	}
	if mv != false {
		t.Errorf("all_skip_short_circuit = %v, want false (present-and-false, not absent)", mv)
	}
}

// TestShipPlan_AllCriteriaSkipExpected_IsAdvisoryNotARefusal is CONDITION 1's
// server-side half: the plan gate ADMITS an all-skip plan. The whole design
// rests on all_criteria_skip_expected never refusing anything, and this asserts
// the ADMIT/non-blocked OUTCOME directly — the committed stage state reaches
// its normal approval state, no failure category is stamped, and the run is
// alive — rather than merely that a findings slice contains the entry.
//
// The finding assertions at the end keep the admission from being vacuously
// green on a plan that tripped no rule.
func TestShipPlan_AllCriteriaSkipExpected_IsAdvisoryNotARefusal(t *testing.T) {
	s, rr, _, sf, au := newPlanSequenceServer(t)
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specWithAcceptanceStage
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateRunning)
	planStage.RequiresApproval = true
	priv, _ := sf.issue(t, runRow.ID)

	body := acceptancePlanBody(t, []map[string]any{
		allSkipCriterion("a1", "the payload records the headline", "covered by the server payload unit test"),
		allSkipCriterion("a2", "the renderer emits the consequence line", "covered by the prompt render unit test"),
	}, nil)

	w := shipPlanRequest(t, s, runRow.ID, planStage.ID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201 (an advisory finding must never refuse the upload):\n%s", w.Code, w.Body.String())
	}

	// COMMITTED state: the plan was ADMITTED — the stage walked to the operator
	// gate, was never re-opened or failed, and the run is alive.
	if got := rr.stagesByID[planStage.ID].State; got != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (all_criteria_skip_expected must never refuse the plan)\ntransitions: %+v",
			got, rr.stageTransitions)
	}
	if got := rr.stagesByID[planStage.ID].FailureCategory; got != nil {
		t.Errorf("stage carries failure category %q; the all_criteria_skip_expected rule must never fail a plan", *got)
	}
	if st := rr.runs[runRow.ID].State; st == run.StateFailed {
		t.Error("run state = failed; an advisory acceptance finding must never terminate a run")
	}

	// The advisory genuinely fired on THIS upload.
	if n := countAcceptancePrecheckEntries(au.auditFake); n != 1 {
		t.Fatalf("plan_acceptance_precheck entries = %d, want 1", n)
	}
	entry := lastAcceptancePrecheckEntry(t, au.auditFake)
	if hasAcceptanceFinding(entry, acceptanceRuleAllCriteriaSkipExpected) == nil {
		t.Fatalf("want an all_criteria_skip_expected finding on the admitted plan; got %+v", entry.Findings)
	}
	if !entry.AllSkipShortCircuit {
		t.Errorf("persisted all_skip_short_circuit = false, want true")
	}
}

// TestAcceptancePrecheck_AllSkip_JoinToRenderedPrompt is the producer-to-
// consumer JOIN test: starting from a REAL all-skip plan BODY, it runs the
// ACTUAL runAcceptancePrecheck -> planGateEvidence -> prompt.Build("plan_review")
// path with NO hand-constructed AcceptancePrecheckPayload and NO hand-
// constructed prompt.AcceptancePrecheckEvidence anywhere, and asserts the
// consequence line in the FINAL rendered bytes.
//
// A mis-wire at EITHER seam (payload -> evidence, or evidence -> render) fails
// this test while the per-layer server and prompt tests both stay green — which
// is exactly what it exists to cover.
func TestAcceptancePrecheck_AllSkip_JoinToRenderedPrompt(t *testing.T) {
	render := func(t *testing.T, criteria []map[string]any) string {
		t.Helper()
		s, _, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
		body := acceptancePlanBody(t, criteria, nil)

		result := s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)
		if result == nil {
			t.Fatal("want a non-nil pre-check result")
		}
		gateEv := planGateEvidence(nil, nil, nil, nil, result)
		if gateEv == nil {
			t.Fatal("planGateEvidence produced no evidence")
		}
		got, err := prompt.Build("plan_review", prompt.Trigger{
			Repo:             "kuhlman-labs/example",
			PlanGateEvidence: gateEv,
		})
		if err != nil {
			t.Fatalf("prompt.Build: %v", err)
		}
		return got
	}

	const consequence = "CONSEQUENCE: every acceptance criterion is marked skip_expected with an expectation_basis"

	allSkip := render(t, []map[string]any{
		allSkipCriterion("a1", "the payload records the headline", "covered by the server payload unit test"),
		allSkipCriterion("a2", "the renderer emits the consequence line", "covered by the prompt render unit test"),
	})
	for _, want := range []string{
		"Acceptance pre-check (verification.acceptance_criteria evaluated against the configured acceptance stage)",
		consequence,
		"ZERO",
		"#2347",
		"FINDING all_criteria_skip_expected",
	} {
		if !strings.Contains(allSkip, want) {
			t.Errorf("rendered plan_review prompt missing %q — a seam between pre-check, evidence mapping and render is broken:\n%s", want, allSkip)
		}
	}

	// NEGATIVE TWIN from a real mixed-criteria body: the line is absent.
	mixed := render(t, []map[string]any{
		allSkipCriterion("a1", "the payload records the headline", "covered by a unit test"),
		{"id": "a2", "statement": "the plan gate admits the plan", "source": "explicit", "source_ref": "#3026"},
	})
	if strings.Contains(mixed, consequence) {
		t.Errorf("mixed-criteria plan must not render the all-skip consequence line:\n%s", mixed)
	}
	if strings.Contains(mixed, "FINDING all_criteria_skip_expected") {
		t.Errorf("mixed-criteria plan must not render the all_criteria_skip_expected finding:\n%s", mixed)
	}
}
