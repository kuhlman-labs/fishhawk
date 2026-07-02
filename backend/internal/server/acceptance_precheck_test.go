package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
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

// (5) Explicit criterion with empty source_ref -> missing_source_ref finding
// naming the criterion id.
func TestAcceptancePrecheck_ExplicitMissingSourceRef_Flags(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{"id": "a1", "statement": "does a thing", "source": "explicit", "blocking": true},
	}, nil)

	s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	entry := lastAcceptancePrecheckEntry(t, au)
	f := hasAcceptanceFinding(entry, acceptanceRuleMissingSourceRef)
	if f == nil {
		t.Fatalf("want a missing_source_ref finding; got %+v", entry.Findings)
	}
	if f.CriterionID != "a1" {
		t.Errorf("finding CriterionID = %q, want a1", f.CriterionID)
	}
}

// (6) Inferred criterion with empty rationale -> missing_rationale finding.
// The body is crafted directly (bypassing schema validation, which the
// if/then conditional would reject) so the defense-in-depth rule is exercised
// independent of upload order.
func TestAcceptancePrecheck_InferredMissingRationale_Flags(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{"id": "a1", "statement": "does a thing", "source": "inferred", "blocking": true},
	}, nil)

	s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	entry := lastAcceptancePrecheckEntry(t, au)
	f := hasAcceptanceFinding(entry, acceptanceRuleMissingRationale)
	if f == nil {
		t.Fatalf("want a missing_rationale finding; got %+v", entry.Findings)
	}
	if f.CriterionID != "a1" {
		t.Errorf("finding CriterionID = %q, want a1", f.CriterionID)
	}
}

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

// TestAcceptancePrecheck_EmptyID_Flags exercises the empty_id branch.
func TestAcceptancePrecheck_EmptyID_Flags(t *testing.T) {
	s, au, runRow := newAcceptancePrecheckServer(t, specWithAcceptanceStage)
	body := acceptancePlanBody(t, []map[string]any{
		{"id": "", "statement": "does a thing", "source": "explicit", "source_ref": "#1", "blocking": true},
	}, nil)

	s.runAcceptancePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	entry := lastAcceptancePrecheckEntry(t, au)
	if hasAcceptanceFinding(entry, acceptanceRuleEmptyID) == nil {
		t.Fatalf("want an empty_id finding; got %+v", entry.Findings)
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
