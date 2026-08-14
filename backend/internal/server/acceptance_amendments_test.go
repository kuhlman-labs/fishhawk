package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// amendAuditFake is approvalAuditFake with a working ListForRunByCategory, so
// the #2581 seam can read PRIOR approval_submitted rows. The embedded fake still
// captures AppendChained, so a refusal test can assert no approval_submitted row
// was written. listErr drives the audit-read fail-closed branch.
type amendAuditFake struct {
	*approvalAuditFake
	byRunCategory map[string][]*audit.Entry
	listErr       error
}

func newAmendAuditFake() *amendAuditFake {
	return &amendAuditFake{
		approvalAuditFake: newApprovalAuditFake(),
		byRunCategory:     map[string][]*audit.Entry{},
	}
}

func (a *amendAuditFake) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if a.listErr != nil {
		return nil, a.listErr
	}
	return a.byRunCategory[runID.String()+":"+category], nil
}

// seedApprovalEntry records a synthetic approval_submitted row BY CONSTRUCTION
// (never by calling the gate that would write it), so a counterfactual RED lands
// on the behavioral assertion rather than on fixture setup.
func (a *amendAuditFake) seedApprovalEntry(runID, stageID uuid.UUID, seq int64, decision string, amendments []acceptanceCriteriaAmendment) {
	payload := map[string]any{"stage_id": stageID.String(), "decision": decision}
	if amendments != nil {
		payload["amend_acceptance_criteria"] = amendments
	}
	raw, _ := json.Marshal(payload)
	key := runID.String() + ":approval_submitted"
	rid := runID
	sid := stageID
	a.byRunCategory[key] = append(a.byRunCategory[key], &audit.Entry{
		ID: uuid.New(), Sequence: seq, RunID: &rid, StageID: &sid,
		Category: "approval_submitted", Payload: raw,
	})
}

// amendCriteria is the standard three-criterion fixture the seam and gate tests
// amend. crit-2 is the one usually retired.
func amendCriteria() []plan.AcceptanceCriterion {
	no := false
	return []plan.AcceptanceCriterion{
		{ID: "crit-1", Statement: "the gate refuses an unknown id", Source: plan.CriterionSourceExplicit},
		{ID: "crit-2", Statement: "healthz reports the server budget", Source: plan.CriterionSourceExplicit},
		{ID: "crit-3", Statement: "the advisory line is rendered", Source: plan.CriterionSourceExplicit, Blocking: &no},
	}
}

// newAmendServer wires a Server with every repo the #2581 gate + seam read: run,
// plan artifact (carrying the acceptance criteria), approvals, and an audit fake
// whose ListForRunByCategory actually serves seeded rows. It returns the plan
// stage in awaiting_approval so a test can drive the real approve handler.
func newAmendServer(t *testing.T, criteria []plan.AcceptanceCriterion) (*Server, *orchestratorRepo, *amendAuditFake, *fakeApprovalRepo, *run.Run, *run.Stage) {
	t.Helper()
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	au := newAmendAuditFake()
	app := newFakeApprovalRepo()
	runRow := rr.seedRun()
	stage := rr.seedStage(runRow.ID, 0, run.StageStateAwaitingApproval)
	seedBudgetPlanArtifact(t, art, stage.ID, &plan.Plan{
		PlanVersion:  "standard_v1",
		Verification: plan.Verification{AcceptanceCriteria: criteria},
	})
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ArtifactRepo: art,
		AuditRepo:    au,
		ApprovalRepo: app,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})
	return s, rr, au, app, runRow, stage
}

// amendBody renders an approve body carrying the given raw amendment JSON.
func amendBody(amendments string) string {
	return `{"decision":"approve","amend_acceptance_criteria":` + amendments + `}`
}

// assertRefused asserts the exact status + error id + details.rule AND that no
// approval row and no approval_submitted audit entry were written — a refusal
// must leave the operator's submission slot free for a corrected retry.
func assertAmendRefused(t *testing.T, w *httptest.ResponseRecorder, app *fakeApprovalRepo, au *amendAuditFake, status int, code, rule string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d:\n%s", w.Code, status, w.Body.String())
	}
	if !bodyHasCode(w, code) {
		t.Errorf("want error code %s, got %s", code, w.Body.String())
	}
	if rule != "" && !strings.Contains(w.Body.String(), `"rule":"`+rule+`"`) {
		t.Errorf("want details.rule=%s, got %s", rule, w.Body.String())
	}
	if code == "validation_failed" && !strings.Contains(w.Body.String(), `"field":"amend_acceptance_criteria"`) {
		t.Errorf("want details.field=amend_acceptance_criteria, got %s", w.Body.String())
	}
	assertNoApprovalRecorded(t, app, au.approvalAuditFake)
}

// ---------------------------------------------------------------------------
// Seam tests: resolveEffectiveAcceptanceCriteria is the SINGLE computation.
// ---------------------------------------------------------------------------

// TestResolveEffectiveCriteria_NoAmendments_PlanSetVerbatim: with nothing
// recorded and nothing pending, the effective set is the plan's criteria in plan
// order with an EMPTY retired set, and AllIDs is the full plan id list.
func TestResolveEffectiveCriteria_NoAmendments_PlanSetVerbatim(t *testing.T) {
	s, _, _, _, runRow, _ := newAmendServer(t, amendCriteria())
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	eff, err := s.resolveEffectiveAcceptanceCriteria(context.Background(), runRow.ID, p, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !reflect.DeepEqual(eff.Live, amendCriteria()) {
		t.Errorf("Live = %+v, want the plan set verbatim", eff.Live)
	}
	if len(eff.Retired) != 0 {
		t.Errorf("Retired = %+v, want empty", eff.Retired)
	}
	if want := []string{"crit-1", "crit-2", "crit-3"}; !reflect.DeepEqual(eff.AllIDs, want) {
		t.Errorf("AllIDs = %v, want %v", eff.AllIDs, want)
	}
}

// TestResolveEffectiveCriteria_RecordedRetire_MovesToRetired: one recorded
// retirement moves the criterion out of Live and into Retired with its reason,
// the operator source tag, and the recording entry's audit sequence — while
// AllIDs still carries every plan id (the superset invariant the served
// acceptance_criteria_ids depends on).
func TestResolveEffectiveCriteria_RecordedRetire_MovesToRetired(t *testing.T) {
	s, _, au, _, runRow, stage := newAmendServer(t, amendCriteria())
	au.seedApprovalEntry(runRow.ID, stage.ID, 7, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "retire", Reason: "condition 1 dropped its surface"},
	})
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	eff, err := s.resolveEffectiveAcceptanceCriteria(context.Background(), runRow.ID, p, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(eff.Live) != 2 || eff.Live[0].ID != "crit-1" || eff.Live[1].ID != "crit-3" {
		t.Errorf("Live = %+v, want crit-1 + crit-3", eff.Live)
	}
	want := []retiredCriterion{{
		ID: "crit-2", Reason: "condition 1 dropped its surface",
		Source: acceptanceRetirementSourceOperator, ApprovalSequence: 7,
	}}
	if !reflect.DeepEqual(eff.Retired, want) {
		t.Errorf("Retired = %+v, want %+v", eff.Retired, want)
	}
	if len(eff.AllIDs) != 3 {
		t.Errorf("AllIDs = %v, want all three plan ids (never narrowed)", eff.AllIDs)
	}
}

// TestResolveEffectiveCriteria_Restate_StaysLive: a restatement replaces only
// the statement and leaves the criterion LIVE — restatement is deliberately NOT
// a silencing channel, so a restated criterion still fails if it genuinely
// fails.
func TestResolveEffectiveCriteria_Restate_StaysLive(t *testing.T) {
	s, _, au, _, runRow, stage := newAmendServer(t, amendCriteria())
	au.seedApprovalEntry(runRow.ID, stage.ID, 3, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "restate", Reason: "narrowed at the gate", Statement: "healthz reports the STAGE budget"},
	})
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	eff, err := s.resolveEffectiveAcceptanceCriteria(context.Background(), runRow.ID, p, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(eff.Live) != 3 {
		t.Fatalf("Live = %+v, want all three still live", eff.Live)
	}
	if eff.Live[1].Statement != "healthz reports the STAGE budget" {
		t.Errorf("restated statement = %q, want the replacement", eff.Live[1].Statement)
	}
	if len(eff.Retired) != 0 {
		t.Errorf("Retired = %+v, want empty (a restate never retires)", eff.Retired)
	}
	if want := []string{"crit-2"}; !reflect.DeepEqual(eff.Restated, want) {
		t.Errorf("Restated = %v, want %v — Live alone cannot signal a restate-only amendment", eff.Restated, want)
	}
	if !eff.amended() {
		t.Error("amended() = false on a restate-only history; the prompt would fall back to the plan's original statements")
	}
}

// TestResolveAcceptancePromptCriteria_RestateOnly_RendersReplacement pins the
// PROMPT caller's composition for a restate-only history: the live set (carrying
// the replacement statement) is returned so buildAcceptance renders it, and the
// retired list stays nil so the retired block does not render.
func TestResolveAcceptancePromptCriteria_RestateOnly_RendersReplacement(t *testing.T) {
	s, _, au, _, runRow, stage := newAmendServer(t, amendCriteria())
	au.seedApprovalEntry(runRow.ID, stage.ID, 6, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "restate", Reason: "narrowed at the gate", Statement: "healthz reports the STAGE budget"},
	})
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	live, retired := s.resolveAcceptancePromptCriteria(context.Background(), runRow.ID, p)
	if len(live) != 3 {
		t.Fatalf("live = %+v, want the three-criterion effective set (a restate-only history must not fall back to the plan set)", live)
	}
	if live[1].Statement != "healthz reports the STAGE budget" {
		t.Errorf("live[1].Statement = %q, want the replacement statement", live[1].Statement)
	}
	if retired != nil {
		t.Errorf("retired = %+v, want nil (nothing was retired)", retired)
	}
}

// TestResolveAcceptancePromptCriteria_NoAmendments_NilNil is the inert control
// paired with the test above: with nothing recorded both trigger fields stay nil,
// which is what keeps an unamended run's prompt byte-identical.
func TestResolveAcceptancePromptCriteria_NoAmendments_NilNil(t *testing.T) {
	s, _, _, _, runRow, _ := newAmendServer(t, amendCriteria())
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	live, retired := s.resolveAcceptancePromptCriteria(context.Background(), runRow.ID, p)
	if live != nil || retired != nil {
		t.Errorf("unamended resolve = (%+v, %+v), want nil/nil", live, retired)
	}
}

// TestResolveEffectiveCriteria_CumulativeAcrossEntries_AscendingSequence: two
// approve entries each retiring one criterion are applied in ASCENDING audit
// sequence, and both retirements survive with their own provenance.
func TestResolveEffectiveCriteria_CumulativeAcrossEntries_AscendingSequence(t *testing.T) {
	s, _, au, _, runRow, stage := newAmendServer(t, amendCriteria())
	// Seeded out of order on purpose: the seam sorts, the fixture does not.
	au.seedApprovalEntry(runRow.ID, stage.ID, 9, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-3", Action: "retire", Reason: "second approval"},
	})
	au.seedApprovalEntry(runRow.ID, stage.ID, 4, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "retire", Reason: "first approval"},
	})
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	eff, err := s.resolveEffectiveAcceptanceCriteria(context.Background(), runRow.ID, p, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(eff.Live) != 1 || eff.Live[0].ID != "crit-1" {
		t.Errorf("Live = %+v, want only crit-1", eff.Live)
	}
	if len(eff.Retired) != 2 || eff.Retired[0].ID != "crit-2" || eff.Retired[1].ID != "crit-3" {
		t.Fatalf("Retired = %+v, want crit-2 then crit-3 (plan order)", eff.Retired)
	}
	if eff.Retired[0].ApprovalSequence != 4 || eff.Retired[1].ApprovalSequence != 9 {
		t.Errorf("retirement sequences = %d/%d, want 4/9", eff.Retired[0].ApprovalSequence, eff.Retired[1].ApprovalSequence)
	}
}

// TestResolveEffectiveCriteria_IgnoredEntries: a retirement recorded under a
// REJECT decision, and one recorded off the PLAN stage, are both ignored — the
// same filter loadApprovalAddScopeFiles applies (#2598). A decomposed child
// therefore inherits no parent retirement, which is the conservative direction.
func TestResolveEffectiveCriteria_IgnoredEntries(t *testing.T) {
	s, rr, au, _, runRow, stage := newAmendServer(t, amendCriteria())
	reviewStage := rr.seedStage(runRow.ID, 1, run.StageStateAwaitingApproval)
	reviewStage.Type = run.StageTypeReview

	au.seedApprovalEntry(runRow.ID, stage.ID, 2, "reject", []acceptanceCriteriaAmendment{
		{ID: "crit-1", Action: "retire", Reason: "recorded under a reject"},
	})
	au.seedApprovalEntry(runRow.ID, reviewStage.ID, 3, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "retire", Reason: "recorded off the plan stage"},
	})
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	eff, err := s.resolveEffectiveAcceptanceCriteria(context.Background(), runRow.ID, p, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(eff.Live) != 3 || len(eff.Retired) != 0 {
		t.Errorf("Live/Retired = %d/%d, want 3/0 — both entries must be ignored", len(eff.Live), len(eff.Retired))
	}
}

// TestResolveEffectiveCriteria_NoAuditRepo_PlanSetVerbatim: with no AuditRepo
// configured there is no chain to read, so the seam returns the plan's criteria
// verbatim with nothing retired — the same direction as every other degrade
// (toward MORE validation, never toward silence).
func TestResolveEffectiveCriteria_NoAuditRepo_PlanSetVerbatim(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	eff, err := s.resolveEffectiveAcceptanceCriteria(context.Background(), uuid.New(), p, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(eff.Live) != 3 || len(eff.Retired) != 0 {
		t.Errorf("Live/Retired = %d/%d, want 3/0 with no AuditRepo", len(eff.Live), len(eff.Retired))
	}
}

// TestResolveEffectiveCriteria_UndecodablePayload_Skipped: an approval_submitted
// row whose payload does not decode is SKIPPED rather than failing the read, and
// a decodable retirement on the same chain still applies. Skipping toward more
// validation is the documented direction; the row is seeded BY CONSTRUCTION.
func TestResolveEffectiveCriteria_UndecodablePayload_Skipped(t *testing.T) {
	s, _, au, _, runRow, stage := newAmendServer(t, amendCriteria())
	key := runRow.ID.String() + ":approval_submitted"
	rid, sid := runRow.ID, stage.ID
	au.byRunCategory[key] = append(au.byRunCategory[key], &audit.Entry{
		ID: uuid.New(), Sequence: 2, RunID: &rid, StageID: &sid,
		Category: "approval_submitted", Payload: []byte(`{"decision":`),
	})
	au.seedApprovalEntry(runRow.ID, stage.ID, 3, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "retire", Reason: "superseded at the gate"},
	})
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	eff, err := s.resolveEffectiveAcceptanceCriteria(context.Background(), runRow.ID, p, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(eff.Retired) != 1 || eff.Retired[0].ID != "crit-2" {
		t.Errorf("Retired = %+v, want only crit-2 (the undecodable row is skipped)", eff.Retired)
	}
	if len(eff.Live) != 2 {
		t.Errorf("Live = %d, want 2", len(eff.Live))
	}
}

// TestRecordedAcceptanceEffectiveVerdict covers the replay-echo reader the
// idempotent ship branch consults: a recorded DOWNGRADE reports its effective
// verdict, a recorded plain outcome reports none, and every unreadable case
// (unknown artifact, audit read error, no AuditRepo) reports "not found" so the
// caller falls back to its freshly computed value instead of silently claiming
// the verdict was not downgraded.
func TestRecordedAcceptanceEffectiveVerdict(t *testing.T) {
	s, _, au, _, runRow, stage := newAmendServer(t, amendCriteria())
	key := runRow.ID.String() + ":" + CategoryAcceptanceOutcomeRecorded
	rid, sid := runRow.ID, stage.ID
	seedOutcome := func(payload map[string]any) {
		raw, _ := json.Marshal(payload)
		au.byRunCategory[key] = append(au.byRunCategory[key], &audit.Entry{
			ID: uuid.New(), RunID: &rid, StageID: &sid,
			Category: CategoryAcceptanceOutcomeRecorded, Payload: raw,
		})
	}
	seedOutcome(map[string]any{"artifact_id": "art-plain", "verdict": "failed"})
	seedOutcome(map[string]any{
		"artifact_id": "art-downgraded", "verdict": "passed", "verdict_reported": "failed",
	})
	au.byRunCategory[key] = append(au.byRunCategory[key], &audit.Entry{
		ID: uuid.New(), RunID: &rid, Category: CategoryAcceptanceOutcomeRecorded,
		Payload: []byte(`{"artifact_id":`),
	})

	for _, tc := range []struct {
		name       string
		artifactID string
		wantVerd   string
		wantFound  bool
	}{
		{"downgrade recorded", "art-downgraded", acceptanceVerdictPassed, true},
		{"no downgrade recorded", "art-plain", "", true},
		{"no entry for this artifact", "art-missing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found := s.recordedAcceptanceEffectiveVerdict(context.Background(), runRow.ID, tc.artifactID)
			if got != tc.wantVerd || found != tc.wantFound {
				t.Errorf("= (%q, %v), want (%q, %v)", got, found, tc.wantVerd, tc.wantFound)
			}
		})
	}

	au.listErr = errors.New("audit store outage")
	if got, found := s.recordedAcceptanceEffectiveVerdict(context.Background(), runRow.ID, "art-downgraded"); got != "" || found {
		t.Errorf("on a read error = (%q, %v), want (\"\", false)", got, found)
	}

	noAudit := New(Config{Addr: "127.0.0.1:0"})
	if got, found := noAudit.recordedAcceptanceEffectiveVerdict(context.Background(), runRow.ID, "art-downgraded"); got != "" || found {
		t.Errorf("with no AuditRepo = (%q, %v), want (\"\", false)", got, found)
	}
}

// TestResolveEffectiveCriteria_AuditError_NoPartialSet: an audit read failure
// returns an error and NO partial set, so each caller picks its own fail
// direction explicitly.
func TestResolveEffectiveCriteria_AuditError_NoPartialSet(t *testing.T) {
	s, _, au, _, runRow, _ := newAmendServer(t, amendCriteria())
	au.listErr = errors.New("audit store outage")
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	eff, err := s.resolveEffectiveAcceptanceCriteria(context.Background(), runRow.ID, p, nil)
	if err == nil {
		t.Fatal("resolve returned nil error on an audit read failure, want an error")
	}
	if len(eff.Live) != 0 || len(eff.Retired) != 0 || len(eff.AllIDs) != 0 {
		t.Errorf("resolve returned a PARTIAL set on error: %+v", eff)
	}
}

// TestResolveAcceptancePromptCriteria_AuditError_RendersFullSet pins the PROMPT
// caller's documented fail direction: on an unreadable chain both trigger fields
// come back nil, which makes buildAcceptance render the FULL plan criteria set.
// The degrade can never silence a criterion.
func TestResolveAcceptancePromptCriteria_AuditError_RendersFullSet(t *testing.T) {
	s, _, au, _, runRow, stage := newAmendServer(t, amendCriteria())
	au.seedApprovalEntry(runRow.ID, stage.ID, 5, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "retire", Reason: "would otherwise be retired"},
	})
	au.listErr = errors.New("audit store outage")
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: amendCriteria()}}

	live, retired := s.resolveAcceptancePromptCriteria(context.Background(), runRow.ID, p)
	if live != nil || retired != nil {
		t.Errorf("degraded resolve returned live=%v retired=%v, want nil/nil (render the full plan set)", live, retired)
	}
}

// ---------------------------------------------------------------------------
// Gate refusals R1-R9 (each: exact status + error id + details.rule, and NO
// approval row inserted).
// ---------------------------------------------------------------------------

// R1.
func TestAmendCriteria_UnknownID_Refused(t *testing.T) {
	s, _, au, app, _, stage := newAmendServer(t, amendCriteria())
	w := submitApproval(t, s, stage.ID,
		amendBody(`[{"id":"crit-nope","action":"retire","reason":"typo"}]`))
	assertAmendRefused(t, w, app, au, http.StatusBadRequest, "validation_failed", "unknown_criterion_id")
}

// R2.
func TestAmendCriteria_BlankReason_Refused(t *testing.T) {
	s, _, au, app, _, stage := newAmendServer(t, amendCriteria())
	w := submitApproval(t, s, stage.ID,
		amendBody(`[{"id":"crit-2","action":"retire","reason":"   "}]`))
	assertAmendRefused(t, w, app, au, http.StatusBadRequest, "validation_failed", "reason_required")
}

// R3.
func TestAmendCriteria_RestateWithoutStatement_Refused(t *testing.T) {
	s, _, au, app, _, stage := newAmendServer(t, amendCriteria())
	w := submitApproval(t, s, stage.ID,
		amendBody(`[{"id":"crit-2","action":"restate","reason":"narrowed"}]`))
	assertAmendRefused(t, w, app, au, http.StatusBadRequest, "validation_failed", "statement_required")
}

// R4.
func TestAmendCriteria_DuplicateID_Refused(t *testing.T) {
	s, _, au, app, _, stage := newAmendServer(t, amendCriteria())
	w := submitApproval(t, s, stage.ID,
		amendBody(`[{"id":"crit-2","action":"retire","reason":"first"},{"id":"crit-2","action":"retire","reason":"second"}]`))
	assertAmendRefused(t, w, app, au, http.StatusBadRequest, "validation_failed", "duplicate_id")
}

// R5: an amendment supplied on a REJECT — and on a NON-PLAN stage — is REFUSED,
// never silently dropped. A drop would diverge what the operator believes they
// retired from what the chain records.
func TestAmendCriteria_OnRejectOrNonPlanStage_Refused(t *testing.T) {
	t.Run("reject decision", func(t *testing.T) {
		s, _, au, app, _, stage := newAmendServer(t, amendCriteria())
		w := submitApproval(t, s, stage.ID,
			`{"decision":"reject","amend_acceptance_criteria":[{"id":"crit-2","action":"retire","reason":"r"}]}`)
		assertAmendRefused(t, w, app, au, http.StatusBadRequest, "validation_failed", "amendment_not_approve_plan_stage")
	})
	t.Run("non-plan stage", func(t *testing.T) {
		s, rr, au, app, runRow, _ := newAmendServer(t, amendCriteria())
		implStage := rr.seedStage(runRow.ID, 1, run.StageStateAwaitingApproval)
		implStage.Type = run.StageTypeImplement
		w := submitApproval(t, s, implStage.ID,
			amendBody(`[{"id":"crit-2","action":"retire","reason":"r"}]`))
		assertAmendRefused(t, w, app, au, http.StatusBadRequest, "validation_failed", "amendment_not_approve_plan_stage")
	})
}

// R6: the anti-silencing gate in a SINGLE call — retiring every criterion is
// refused 422, because a plan with no acceptance contract left validates
// nothing.
func TestAmendCriteria_AllRetiredSingleCall_Refused(t *testing.T) {
	s, _, au, app, _, stage := newAmendServer(t, amendCriteria())
	w := submitApproval(t, s, stage.ID, amendBody(
		`[{"id":"crit-1","action":"retire","reason":"a"},`+
			`{"id":"crit-2","action":"retire","reason":"b"},`+
			`{"id":"crit-3","action":"retire","reason":"c"}]`))
	assertAmendRefused(t, w, app, au, http.StatusUnprocessableEntity, "acceptance_criteria_all_retired", "")
}

// R7: the SAME gate reached CUMULATIVELY — two prior approvals already retired
// two of three criteria, so retiring the last one empties the contract and is
// refused. This is the test that proves the gate reads the UNION the seam
// computes, not this request alone.
func TestAmendCriteria_AllRetiredCumulative_Refused(t *testing.T) {
	s, _, au, app, runRow, stage := newAmendServer(t, amendCriteria())
	au.seedApprovalEntry(runRow.ID, stage.ID, 2, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-1", Action: "retire", Reason: "prior a"},
	})
	au.seedApprovalEntry(runRow.ID, stage.ID, 3, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "retire", Reason: "prior b"},
	})
	w := submitApproval(t, s, stage.ID,
		amendBody(`[{"id":"crit-3","action":"retire","reason":"the last one"}]`))
	assertAmendRefused(t, w, app, au, http.StatusUnprocessableEntity, "acceptance_criteria_all_retired", "")
}

// R8: an id a PRIOR approval already retired cannot be re-reasoned or undone.
func TestAmendCriteria_AlreadyRetiredID_Refused(t *testing.T) {
	s, _, au, app, runRow, stage := newAmendServer(t, amendCriteria())
	au.seedApprovalEntry(runRow.ID, stage.ID, 2, "approve", []acceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "retire", Reason: "prior retirement"},
	})
	w := submitApproval(t, s, stage.ID,
		amendBody(`[{"id":"crit-2","action":"retire","reason":"a different reason"}]`))
	assertAmendRefused(t, w, app, au, http.StatusBadRequest, "validation_failed", "already_retired")
}

// R9: fail CLOSED when the amendment can anchor to nothing — a plan carrying no
// acceptance_criteria, and (second arm) a plan that cannot be loaded at all.
func TestAmendCriteria_PlanUnavailableOrNoCriteria_Refused(t *testing.T) {
	t.Run("plan with zero criteria", func(t *testing.T) {
		s, _, au, app, _, stage := newAmendServer(t, nil)
		w := submitApproval(t, s, stage.ID,
			amendBody(`[{"id":"crit-2","action":"retire","reason":"r"}]`))
		assertAmendRefused(t, w, app, au, http.StatusUnprocessableEntity, "acceptance_criteria_unavailable", "")
	})
	t.Run("plan artifact unreadable", func(t *testing.T) {
		s, _, au, app, _, stage := newAmendServer(t, amendCriteria())
		s.cfg.ArtifactRepo.(*fakeArtifactRepo).listErr = errors.New("artifact store outage")
		w := submitApproval(t, s, stage.ID,
			amendBody(`[{"id":"crit-2","action":"retire","reason":"r"}]`))
		assertAmendRefused(t, w, app, au, http.StatusUnprocessableEntity, "acceptance_criteria_unavailable", "")
	})
}

// TestAmendCriteria_UnknownAction_Refused pins the action-validity refusal: an
// action outside {retire, restate} is refused rather than silently ignored (an
// ignored action would record an amendment that does nothing).
func TestAmendCriteria_UnknownAction_Refused(t *testing.T) {
	s, _, au, app, _, stage := newAmendServer(t, amendCriteria())
	w := submitApproval(t, s, stage.ID,
		amendBody(`[{"id":"crit-2","action":"delete","reason":"r"}]`))
	assertAmendRefused(t, w, app, au, http.StatusBadRequest, "validation_failed", "unknown_action")
}

// TestAmendCriteria_AuditReadError_FailsClosed pins the gate's degrade: an
// unreadable approval chain means the anti-silencing refusals cannot be
// evaluated, so the amendment is REFUSED (422) rather than recorded unverified.
func TestAmendCriteria_AuditReadError_FailsClosed(t *testing.T) {
	s, _, au, app, _, stage := newAmendServer(t, amendCriteria())
	au.listErr = errors.New("audit store outage")
	w := submitApproval(t, s, stage.ID,
		amendBody(`[{"id":"crit-2","action":"retire","reason":"r"}]`))
	assertAmendRefused(t, w, app, au, http.StatusUnprocessableEntity, "acceptance_criteria_unavailable", "")
}

// TestAmendCriteria_OversizedText_Capped pins the truncation branch: an
// oversized reason is CAPPED (not refused), so the operator's decision still
// lands but cannot bloat the hashed audit payload.
func TestAmendCriteria_OversizedText_Capped(t *testing.T) {
	s, _, _, _, _, stage := newAmendServer(t, amendCriteria())
	huge := strings.Repeat("x", prompt.MaxApprovalConditionBytes+500)
	body, _ := json.Marshal(map[string]any{
		"decision": "approve",
		"amend_acceptance_criteria": []map[string]string{
			{"id": "crit-2", "action": "retire", "reason": huge},
		},
	})
	w := submitApproval(t, s, stage.ID, string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an oversized reason is capped, not refused):\n%s", w.Code, w.Body.String())
	}
	au := s.cfg.AuditRepo.(*amendAuditFake)
	payload := findApprovalSubmittedPayload(t, au.appended)
	entries, _ := payload["amend_acceptance_criteria"].([]any)
	if len(entries) != 1 {
		t.Fatalf("amend_acceptance_criteria = %v, want one entry", payload["amend_acceptance_criteria"])
	}
	first, _ := entries[0].(map[string]any)
	reason, _ := first["reason"].(string)
	// CapText appends its byte-identical "...[truncated]" marker, so the capped
	// text is the cap plus that marker — the point being it is BOUNDED, not that
	// the oversized input rode onto the hash chain whole.
	if len(reason) >= len(huge) {
		t.Errorf("recorded reason length = %d, want it capped below the %d-byte input", len(reason), len(huge))
	}
	if !strings.HasSuffix(reason, "...[truncated]") {
		t.Errorf("recorded reason is not the capped form: ...%q", reason[max(0, len(reason)-40):])
	}
}
