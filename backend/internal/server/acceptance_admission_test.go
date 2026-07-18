package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// admissionSeam bundles the fakes + ids of a plan(succ)+implement(succ)+
// review(succ)+acceptance(state) run wired for the acceptance-admission
// endpoint: the server, the shared orchestratorRepo (functional transitions,
// used as BOTH the server RunRepo and the orchestrator Runs), the audit fake,
// and the wired orchestrator (nil GitHub, so a workflow_dispatch would be
// observable via the acceptance_dispatched emit — which the short-circuit path
// never fires).
type admissionSeam struct {
	s            *Server
	rr           *orchestratorRepo
	au           *auditFake
	o            *orchestrator.Orchestrator
	runID        uuid.UUID
	acceptanceID uuid.UUID
	implementID  uuid.UUID
}

// admissionPlanBytes builds a standard_v1 plan whose verification carries the
// given out_of_scope + acceptance_criteria — mirrors the orchestrator test's
// acceptanceSkipPlanBytes. No schema validation runs on artifact load, so a
// hand-built plan that decodes into plan.Plan is sufficient to drive the
// predicates.
func admissionPlanBytes(t *testing.T, outOfScope []string, criteria []map[string]any) json.RawMessage {
	t.Helper()
	verification := map[string]any{
		"test_strategy": "unit",
		"rollback_plan": "revert the PR",
	}
	if len(outOfScope) > 0 {
		verification["out_of_scope"] = outOfScope
	}
	if len(criteria) > 0 {
		verification["acceptance_criteria"] = criteria
	}
	b, err := json.Marshal(map[string]any{
		"plan_version": "standard_v1",
		"summary":      "ship the widget endpoint",
		"verification": verification,
	})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

func buildAdmissionSeam(t *testing.T, acceptanceState run.StageState, planBytes json.RawMessage) *admissionSeam {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newAuditFake()
	r := rr.seedRun()
	r.WorkflowID = "feature_change"
	planS := rr.seedStage(r.ID, 0, run.StageStateSucceeded)
	planS.Type = run.StageTypePlan
	impl := rr.seedStage(r.ID, 1, run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement
	rev := rr.seedStage(r.ID, 2, run.StageStateSucceeded)
	rev.Type = run.StageTypeReview
	acc := rr.seedStage(r.ID, 3, acceptanceState)
	acc.Type = run.StageTypeAcceptance

	ar := newFakeArtifactRepo()
	v := "standard_v1"
	ar.all = append(ar.all, &artifact.Artifact{
		ID: uuid.New(), StageID: planS.ID, Kind: artifact.KindPlan,
		SchemaVersion: &v, Content: planBytes, CreatedAt: time.Now().UTC(),
	})

	o := &orchestrator.Orchestrator{Runs: rr, Audit: au, Artifacts: ar}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au, ArtifactRepo: ar, Orchestrator: o})
	return &admissionSeam{s: s, rr: rr, au: au, o: o, runID: r.ID, acceptanceID: acc.ID, implementID: impl.ID}
}

// postAdmission calls the admission endpoint under the given identity. A zero
// Identity is anonymous.
func postAdmission(t *testing.T, s *Server, stageID uuid.UUID, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	url := "/v0/stages/" + stageID.String() + "/acceptance-admission"
	req := httptest.NewRequest(http.MethodPost, url, nil)
	req.SetPathValue("stage_id", stageID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	w := httptest.NewRecorder()
	s.handleAcceptanceAdmission(w, req)
	return w
}

var allSkipWithBasisCriteria = []map[string]any{
	{"id": "webhook-fires", "statement": "webhook fires on close", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in webhook_integration_test.go with a fake"},
	{"id": "issue-closes", "statement": "issue auto-closes", "source": "inferred", "rationale": "external", "skip_expected": true, "expectation_basis": "validated in closer_e2e_test.go"},
}

// TestAcceptanceAdmission_AllSkipWithBasis_ShortCircuits is Mode 1 (the issue's
// done-means): POSTing admission on the acceptance stage of an all-skip-with-
// basis plan settles it succeeded, records an acceptance_outcome_recorded entry
// (accepted / passed / basis all-skip-with-basis), and fires NO acceptance
// dispatch — the no-runner-dispatch-evidence pin.
func TestAcceptanceAdmission_AllSkipWithBasis_ShortCircuits(t *testing.T) {
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))

	w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceAdmissionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.ShortCircuited {
		t.Fatalf("short_circuited = false, want true:\n%s", w.Body.String())
	}
	if resp.Kind != orchestrator.AcceptanceShortCircuitAllSkipWithBasis || resp.Basis != plan.AcceptanceBasisAllSkipWithBasis || resp.CriteriaTotal != 2 {
		t.Errorf("resp = %+v, want kind=%s basis=%s total=2", resp, orchestrator.AcceptanceShortCircuitAllSkipWithBasis, plan.AcceptanceBasisAllSkipWithBasis)
	}
	if resp.Stage == nil || resp.Stage.State != string(run.StageStateSucceeded) {
		t.Errorf("resp.Stage = %+v, want a succeeded stage", resp.Stage)
	}
	if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStateSucceeded {
		t.Errorf("acceptance stage = %q, want succeeded", got)
	}
	if got := seam.rr.runs[seam.runID].State; got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded (Advance re-entered)", got)
	}
	if n := countByCategory(seam.au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Fatalf("acceptance_outcome_recorded = %d, want 1", n)
	}
	if n := countByCategory(seam.au, CategoryAcceptanceDispatched); n != 0 {
		t.Errorf("acceptance_dispatched = %d, want 0 (no runner dispatch evidence)", n)
	}
	outcome := findAppendedByCategory(t, seam.au, CategoryAcceptanceOutcomeRecorded)
	for _, want := range []string{`"verdict":"passed"`, `"outcome":"accepted"`, `"basis":"all-skip-with-basis"`} {
		if !strings.Contains(string(outcome.Payload), want) {
			t.Errorf("outcome payload missing %s:\n%s", want, outcome.Payload)
		}
	}
}

// TestAcceptanceAdmission_OtherPredicates_ShortCircuit is Mode 8: the other two
// disjoint predicates (out-of-scope, empty-criteria) settle through the same
// admission path.
func TestAcceptanceAdmission_OtherPredicates_ShortCircuit(t *testing.T) {
	t.Run("out-of-scope", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, []string{"deletion deferred to a follow-up"}, nil))
		w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp.ShortCircuited || resp.Kind != orchestrator.AcceptanceShortCircuitOutOfScope {
			t.Fatalf("resp = %+v, want an out-of-scope short-circuit", resp)
		}
		if n := countByCategory(seam.au, CategoryAcceptanceSkippedOutOfScope); n != 1 {
			t.Errorf("acceptance_skipped_out_of_scope = %d, want 1", n)
		}
		if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStateSucceeded {
			t.Errorf("acceptance stage = %q, want succeeded", got)
		}
	})
	t.Run("empty-criteria", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, nil))
		w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp.ShortCircuited || resp.Kind != orchestrator.AcceptanceShortCircuitEmptyCriteria {
			t.Fatalf("resp = %+v, want an empty-criteria short-circuit", resp)
		}
		if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStateSucceeded {
			t.Errorf("acceptance stage = %q, want succeeded", got)
		}
	})
}

// TestAcceptanceAdmission_MixedCriteria_NoShortCircuit is Mode 2: a mixed plan
// (one drivable criterion) returns short_circuited:false and leaves the stage
// pending, no state change, no verdict.
func TestAcceptanceAdmission_MixedCriteria_NoShortCircuit(t *testing.T) {
	mixed := []map[string]any{
		allSkipWithBasisCriteria[0],
		{"id": "get-returns-200", "statement": "GET returns 200", "source": "explicit", "source_ref": "#1", "blocking": true},
	}
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, mixed))
	w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceAdmissionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ShortCircuited {
		t.Errorf("short_circuited = true, want false (mixed criteria)")
	}
	// The seam wires NO workflow spec, so the acceptance stage declares no egress
	// target hosts — needs_target stays absent even though the plan needs live
	// validation (the runner skips its target gate when no host is declared).
	if resp.NeedsTarget {
		t.Errorf("needs_target = true, want false (no declared egress target hosts)")
	}
	if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStatePending {
		t.Errorf("acceptance stage = %q, want pending (untouched)", got)
	}
	if n := countByCategory(seam.au, CategoryAcceptanceOutcomeRecorded); n != 0 {
		t.Errorf("acceptance_outcome_recorded = %d, want 0", n)
	}
}

// TestAcceptanceAdmission_NeedsTarget covers the E48.6 (#1953) augmentation on the
// short_circuited:false path: a mixed-criteria plan (live validation required)
// whose workflow spec DECLARES an egress target host returns needs_target:true +
// the verbatim host list + the resolved merge-candidate head SHA, so a dispatch
// verb probes the target before spawning a doomed runner. Two legs pin the
// SHA-resolved and SHA-empty-but-still-needs-target branches; a third pins that a
// short-circuit hit never carries needs_target.
func TestAcceptanceAdmission_NeedsTarget(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	mixed := []map[string]any{
		allSkipWithBasisCriteria[0],
		{"id": "get-returns-200", "statement": "GET returns 200", "source": "explicit", "source_ref": "#1", "blocking": true},
	}

	t.Run("declared hosts + resolvable head SHA -> needs_target with hosts+sha", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, mixed))
		// The committed example spec declares egress target host localhost:8080.
		seam.rr.runs[seam.runID].WorkflowSpec = exampleBytes
		// Seed a reported-head ledger entry so the expected head SHA resolves.
		headSHA := "abc1234def567890abc1234def567890abc12345"
		seam.au.seeded = append(seam.au.seeded,
			makeReportedHeadEntry(seam.runID, seam.implementID, "pull_request_opened", headSHA, time.Now().UTC()))

		w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.ShortCircuited {
			t.Fatalf("short_circuited = true, want false")
		}
		if !resp.NeedsTarget {
			t.Errorf("needs_target = false, want true (live validation + declared hosts)")
		}
		if len(resp.TargetHosts) != 1 || resp.TargetHosts[0] != "localhost:8080" {
			t.Errorf("target_hosts = %v, want [localhost:8080] verbatim", resp.TargetHosts)
		}
		if resp.ExpectedHeadSHA != headSHA {
			t.Errorf("expected_head_sha = %q, want %q (newest reported head)", resp.ExpectedHeadSHA, headSHA)
		}
		// Wire-key assertion: the dispatch verb decodes these exact keys.
		body := w.Body.String()
		for _, key := range []string{`"needs_target":true`, `"target_hosts":["localhost:8080"]`, `"expected_head_sha":"` + headSHA + `"`} {
			if !strings.Contains(body, key) {
				t.Errorf("response missing wire key %s:\n%s", key, body)
			}
		}
		// No state change and no verdict — this is metadata-only.
		if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStatePending {
			t.Errorf("acceptance stage = %q, want pending (untouched)", got)
		}
	})

	t.Run("declared hosts + unresolvable head SHA -> needs_target, empty sha", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, mixed))
		seam.rr.runs[seam.runID].WorkflowSpec = exampleBytes
		// No reported-head ledger entry seeded -> resolveAcceptanceExpectedHeadSHA
		// returns "". needs_target must still be present (the verb degrades to a
		// proceed-with-warning on an empty SHA).
		w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp.NeedsTarget {
			t.Errorf("needs_target = false, want true even with an unresolvable head SHA")
		}
		if len(resp.TargetHosts) != 1 || resp.TargetHosts[0] != "localhost:8080" {
			t.Errorf("target_hosts = %v, want [localhost:8080]", resp.TargetHosts)
		}
		if resp.ExpectedHeadSHA != "" {
			t.Errorf("expected_head_sha = %q, want empty (no ledger entry)", resp.ExpectedHeadSHA)
		}
	})

	t.Run("short-circuit hit never carries needs_target", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
		// Even with declared hosts, an all-skip short-circuit settles the stage and
		// needs_target stays absent (no live target is needed).
		seam.rr.runs[seam.runID].WorkflowSpec = exampleBytes
		w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp.ShortCircuited {
			t.Fatalf("short_circuited = false, want true (all-skip)")
		}
		if resp.NeedsTarget {
			t.Errorf("needs_target = true, want false on a short-circuit hit")
		}
	})
}

// TestAcceptanceAdmission_RecoveryChild_NeedsTargetWithAncestorHead is the #2028
// cross-layer regression proof: a plan-stageless recovery child (ParentRunID set,
// own ledger EMPTY) whose ancestor carries a mixed-criteria plan + a reported-head
// ledger entry must produce the SAME needs_target refusal as its plan-bearing
// parent — NON-EMPTY expected_head_sha resolved via the parent walk — not a weaker
// one. The shared orchestratorRepo backs BOTH the server RunRepo and the
// orchestrator Runs, so a single seeded parent+child exercises BOTH parent walks:
// the orchestrator plan walk (liveValidationRequired) AND the server head walk
// (expected_head_sha). On the pre-fix tree the head walk is absent and
// expected_head_sha resolves "", so the NON-EMPTY assertion is the RED regression
// proof. A ledger-empty companion pins that needs_target stays true with an empty
// SHA (the walk exhausts).
func TestAcceptanceAdmission_RecoveryChild_NeedsTargetWithAncestorHead(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	mixed := []map[string]any{
		allSkipWithBasisCriteria[0],
		{"id": "get-returns-200", "statement": "GET returns 200", "source": "explicit", "source_ref": "#1", "blocking": true},
	}
	const headSHA = "abc1234def567890abc1234def567890abc12345"

	// newRecoveryChildSeam seeds a PARENT run (plan+implement+review, plan artifact
	// on its plan stage) and a plan-stageless recovery CHILD (ParentRunID=parent.ID,
	// declared egress hosts, admissible pending acceptance stage). When seedHead is
	// true the reported-head ledger entry is scoped to the PARENT runID only (the
	// child's own ledger stays empty). Returns the server, the audit fake, and the
	// child's run+acceptance ids.
	newRecoveryChildSeam := func(t *testing.T, seedHead bool) (*Server, *auditFake, *orchestratorRepo, uuid.UUID, uuid.UUID) {
		t.Helper()
		rr := newOrchestratorRepo()
		au := newAuditFake()

		parent := rr.seedRun()
		parent.WorkflowID = "feature_change"
		parentPlan := rr.seedStage(parent.ID, 0, run.StageStateSucceeded)
		parentPlan.Type = run.StageTypePlan
		parentImpl := rr.seedStage(parent.ID, 1, run.StageStateSucceeded)
		parentImpl.Type = run.StageTypeImplement
		parentRev := rr.seedStage(parent.ID, 2, run.StageStateSucceeded)
		parentRev.Type = run.StageTypeReview

		child := rr.seedRun()
		child.WorkflowID = "feature_change"
		child.ParentRunID = &parent.ID
		child.WorkflowSpec = exampleBytes
		childImpl := rr.seedStage(child.ID, 0, run.StageStateSucceeded)
		childImpl.Type = run.StageTypeImplement
		childRev := rr.seedStage(child.ID, 1, run.StageStateSucceeded)
		childRev.Type = run.StageTypeReview
		childAcc := rr.seedStage(child.ID, 2, run.StageStatePending)
		childAcc.Type = run.StageTypeAcceptance

		ar := newFakeArtifactRepo()
		v := "standard_v1"
		ar.all = append(ar.all, &artifact.Artifact{
			ID: uuid.New(), StageID: parentPlan.ID, Kind: artifact.KindPlan,
			SchemaVersion: &v, Content: admissionPlanBytes(t, nil, mixed), CreatedAt: time.Now().UTC(),
		})
		if seedHead {
			au.seeded = append(au.seeded,
				makeReportedHeadEntry(parent.ID, parentImpl.ID, "pull_request_opened", headSHA, time.Now().UTC()))
		}

		o := &orchestrator.Orchestrator{Runs: rr, Audit: au, Artifacts: ar}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au, ArtifactRepo: ar, Orchestrator: o})
		return s, au, rr, child.ID, childAcc.ID
	}

	t.Run("ancestor head -> needs_target with the walked non-empty sha", func(t *testing.T) {
		s, au, rr, _, childAccID := newRecoveryChildSeam(t, true)

		w := postAdmission(t, s, childAccID, testOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.ShortCircuited {
			t.Fatalf("short_circuited = true, want false (mixed-criteria ancestor plan)")
		}
		if !resp.NeedsTarget {
			t.Errorf("needs_target = false, want true (live validation via the ancestor plan walk)")
		}
		if len(resp.TargetHosts) != 1 || resp.TargetHosts[0] != "localhost:8080" {
			t.Errorf("target_hosts = %v, want [localhost:8080] verbatim", resp.TargetHosts)
		}
		// THE RED REGRESSION PROOF: the head resolves NON-EMPTY via the parent walk.
		// On the pre-step-5 tree the own-run-scoped resolver reads the child's empty
		// ledger and this fails.
		if resp.ExpectedHeadSHA != headSHA {
			t.Errorf("expected_head_sha = %q, want %q (ancestor head via the parent walk)", resp.ExpectedHeadSHA, headSHA)
		}
		body := w.Body.String()
		if !strings.Contains(body, `"expected_head_sha":"`+headSHA+`"`) {
			t.Errorf("response missing wire key expected_head_sha=%s:\n%s", headSHA, body)
		}
		// No-spawn pin: no runner dispatched, no verdict recorded, stage still pending.
		if n := countByCategory(au, CategoryAcceptanceDispatched); n != 0 {
			t.Errorf("acceptance_dispatched = %d, want 0 (no runner spawned)", n)
		}
		if n := countByCategory(au, CategoryAcceptanceOutcomeRecorded); n != 0 {
			t.Errorf("acceptance_outcome_recorded = %d, want 0 (no verdict)", n)
		}
		if got := rr.stagesByID[childAccID].State; got != run.StageStatePending {
			t.Errorf("acceptance stage = %q, want pending (untouched)", got)
		}
	})

	t.Run("no ancestor head -> needs_target, empty sha", func(t *testing.T) {
		s, _, _, _, childAccID := newRecoveryChildSeam(t, false)

		w := postAdmission(t, s, childAccID, testOperatorIdentity())
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp.NeedsTarget {
			t.Errorf("needs_target = false, want true even with no resolvable ancestor head")
		}
		if len(resp.TargetHosts) != 1 || resp.TargetHosts[0] != "localhost:8080" {
			t.Errorf("target_hosts = %v, want [localhost:8080]", resp.TargetHosts)
		}
		if resp.ExpectedHeadSHA != "" {
			t.Errorf("expected_head_sha = %q, want empty (walk exhausts with no ledger entry)", resp.ExpectedHeadSHA)
		}
	})
}

// getRunErrRepo wraps an orchestratorRepo and forces GetRun to fail while every
// other method (GetStage, transitions, …) still works, so the handler's GetRun
// call in the needs_target augmentation can be driven into its fail-open branch
// without disturbing the orchestrator's own repo access.
type getRunErrRepo struct {
	*orchestratorRepo
}

func (r *getRunErrRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("simulated run-repo transport failure")
}

// TestAcceptanceAdmission_NeedsTarget_GetRunError_DropsSilently pins the
// deliberate fail-open branch (#1953): when live validation is required but the
// handler's GetRun lookup fails, needs_target is dropped silently
// (short_circuited:false, no hosts) rather than erroring the dispatch — the
// caller's own spawn path still applies. The orchestrator keeps the working repo;
// only the server's RunRepo GetRun fails.
func TestAcceptanceAdmission_NeedsTarget_GetRunError_DropsSilently(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	mixed := []map[string]any{
		allSkipWithBasisCriteria[0],
		{"id": "get-returns-200", "statement": "GET returns 200", "source": "explicit", "source_ref": "#1", "blocking": true},
	}
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, mixed))
	seam.rr.runs[seam.runID].WorkflowSpec = exampleBytes

	// Rebuild the server with a RunRepo that errors on GetRun; the orchestrator
	// keeps seam.rr, so the short-circuit evaluation still runs and reports
	// liveValidationRequired before the failing GetRun drops needs_target.
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: &getRunErrRepo{seam.rr}, AuditRepo: seam.au, Orchestrator: seam.o})
	w := postAdmission(t, s, seam.acceptanceID, testOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open):\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceAdmissionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ShortCircuited {
		t.Errorf("short_circuited = true, want false")
	}
	if resp.NeedsTarget {
		t.Errorf("needs_target = true, want false (GetRun error drops it silently)")
	}
	if len(resp.TargetHosts) != 0 {
		t.Errorf("target_hosts = %v, want empty (dropped)", resp.TargetHosts)
	}
}

// TestAcceptanceAdmission_NonAdmissibleState_False is the already-settled no-op:
// a succeeded acceptance stage is a non-admissible state → short_circuited:false,
// no re-settle.
func TestAcceptanceAdmission_NonAdmissibleState_False(t *testing.T) {
	seam := buildAdmissionSeam(t, run.StageStateSucceeded, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	w := postAdmission(t, seam.s, seam.acceptanceID, testOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceAdmissionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ShortCircuited {
		t.Errorf("short_circuited = true, want false (already-settled acceptance stage)")
	}
	// A non-admissible (settled) stage is never live-validation-required, so
	// needs_target stays absent even if hosts were declared.
	if resp.NeedsTarget {
		t.Errorf("needs_target = true, want false (already-settled acceptance stage)")
	}
	if n := countByCategory(seam.au, CategoryAcceptanceOutcomeRecorded); n != 0 {
		t.Errorf("acceptance_outcome_recorded = %d, want 0", n)
	}
}

// TestAcceptanceAdmission_NilOrchestrator_FailsOpen is Mode 5: a server with no
// orchestrator wired returns short_circuited:false (fail-open — a degraded
// server never blocks a legitimate dispatch).
func TestAcceptanceAdmission_NilOrchestrator_FailsOpen(t *testing.T) {
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	// Rebuild the server with no orchestrator but the same repo.
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: seam.rr, AuditRepo: seam.au})
	w := postAdmission(t, s, seam.acceptanceID, testOperatorIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceAdmissionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ShortCircuited {
		t.Errorf("short_circuited = true, want false (nil orchestrator)")
	}
	if got := seam.rr.stagesByID[seam.acceptanceID].State; got != run.StageStatePending {
		t.Errorf("acceptance stage = %q, want pending (untouched)", got)
	}
}

// TestAcceptanceAdmission_Auth is Mode 6: 401 unauthenticated, 403 missing
// write:stages, 403 cross-run MCP subject binding; plus the MCP-subject
// same-run happy path.
func TestAcceptanceAdmission_Auth(t *testing.T) {
	t.Run("anonymous -> 401", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
		w := postAdmission(t, seam.s, seam.acceptanceID, Identity{})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
		}
	})
	t.Run("token missing write:stages -> 403", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
		id := Identity{Subject: "github:agent", TokenID: "tok", Scopes: []string{"read:runs"}}
		w := postAdmission(t, seam.s, seam.acceptanceID, id)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "insufficient_scope") {
			t.Errorf("body missing insufficient_scope: %s", w.Body.String())
		}
	})
	t.Run("cross-run MCP subject -> 403", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
		id := Identity{Subject: "mcp:run:" + uuid.NewString(), TokenID: "tok", Scopes: []string{"write:stages"}}
		w := postAdmission(t, seam.s, seam.acceptanceID, id)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "cross_run_admission") {
			t.Errorf("body missing cross_run_admission: %s", w.Body.String())
		}
	})
	t.Run("same-run MCP subject -> short-circuits", func(t *testing.T) {
		seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
		id := Identity{Subject: "mcp:run:" + seam.runID.String(), TokenID: "tok", Scopes: []string{"write:stages"}}
		w := postAdmission(t, seam.s, seam.acceptanceID, id)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		var resp acceptanceAdmissionResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp.ShortCircuited {
			t.Errorf("short_circuited = false, want true for the run's own agent token")
		}
	})
}

// TestAcceptanceAdmission_UnknownStage_404 is Mode: an unknown stage id 404s.
func TestAcceptanceAdmission_UnknownStage_404(t *testing.T) {
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	w := postAdmission(t, seam.s, uuid.New(), testOperatorIdentity())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stage_not_found") {
		t.Errorf("body missing stage_not_found: %s", w.Body.String())
	}
}

// TestAcceptanceAdmission_NonAcceptanceStage_422 is Mode: a non-acceptance stage
// 422s (a misrouted call is diagnosable, not a silent false).
func TestAcceptanceAdmission_NonAcceptanceStage_422(t *testing.T) {
	seam := buildAdmissionSeam(t, run.StageStatePending, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	w := postAdmission(t, seam.s, seam.implementID, testOperatorIdentity())
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "validation_failed") {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
}

// blockingTransitionRepo wraps an orchestratorRepo and gates the FIRST
// TransitionStage call on a channel (#1936). The gate signals `started` and then
// either proceeds when `release` closes OR aborts with ctx.Err() if the caller's
// context is cancelled first — so a walk running under a cancellable/deadlined
// context aborts, while one running under the detached no-deadline mutation
// context waits for the release and completes. Every non-first transition passes
// through unblocked. Embedding promotes GetRun/GetStage/TransitionStageFrom/… so
// the wrapper satisfies run.Repository AND the StageCASTransitioner capability.
type blockingTransitionRepo struct {
	*orchestratorRepo
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (b *blockingTransitionRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	var gateErr error
	b.once.Do(func() {
		close(b.started)
		select {
		case <-b.release:
		case <-ctx.Done():
			gateErr = ctx.Err()
		}
	})
	if gateErr != nil {
		return nil, gateErr
	}
	return b.orchestratorRepo.TransitionStage(ctx, id, to, c)
}

// seedAdmissionRun seeds a plan(succ)+implement(succ)+review(succ)+
// acceptance(pending) run into base and registers the plan artifact, returning
// the run/acceptance/implement ids and the artifact repo — the shared scaffold
// for the #1936 concurrency tests that need a custom (blocking) Runs repo rather
// than the plain buildAdmissionSeam wiring.
func seedAdmissionRun(t *testing.T, base *orchestratorRepo, planBytes json.RawMessage) (runID, acceptanceID, implementID uuid.UUID, ar *fakeArtifactRepo) {
	t.Helper()
	r := base.seedRun()
	r.WorkflowID = "feature_change"
	planS := base.seedStage(r.ID, 0, run.StageStateSucceeded)
	planS.Type = run.StageTypePlan
	impl := base.seedStage(r.ID, 1, run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement
	rev := base.seedStage(r.ID, 2, run.StageStateSucceeded)
	rev.Type = run.StageTypeReview
	acc := base.seedStage(r.ID, 3, run.StageStatePending)
	acc.Type = run.StageTypeAcceptance

	ar = newFakeArtifactRepo()
	v := "standard_v1"
	ar.all = append(ar.all, &artifact.Artifact{
		ID: uuid.New(), StageID: planS.ID, Kind: artifact.KindPlan,
		SchemaVersion: &v, Content: planBytes, CreatedAt: time.Now().UTC(),
	})
	return r.ID, acc.ID, impl.ID, ar
}

// TestAcceptanceAdmission_DetachedWalk_SettlesDespiteSlowTransition is failure
// mode d AND binding condition 1 (#1936): a state transition slower than the
// handler-level walk bound must NOT abort the walk. The mutation phase runs on
// context.WithoutCancel with NO deadline, so even after the client disconnects
// AND the handler's own walk timeout fires, the stage still fully settles to
// succeeded with the acceptance_outcome_recorded verdict. This is the test that
// fails on the rejected 30s-abort design (the walk would be cancelled mid-flight).
func TestAcceptanceAdmission_DetachedWalk_SettlesDespiteSlowTransition(t *testing.T) {
	// Shrink the pre-mutation bound so the timeout demonstrably fires during the
	// (blocked) transition — proving the mutation phase re-detached from it.
	prev := acceptanceAdmissionWalkTimeout
	acceptanceAdmissionWalkTimeout = timescale.D(100 * time.Millisecond)
	defer func() { acceptanceAdmissionWalkTimeout = prev }()

	base := newOrchestratorRepo()
	_, acceptanceID, _, ar := seedAdmissionRun(t, base, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	blk := &blockingTransitionRepo{orchestratorRepo: base, started: make(chan struct{}), release: make(chan struct{})}
	au := newAuditFake()
	o := &orchestrator.Orchestrator{Runs: blk, Audit: au, Artifacts: ar}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: blk, AuditRepo: au, ArtifactRepo: ar, Orchestrator: o})

	// A cancellable request context — a client that will disconnect mid-walk.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+acceptanceID.String()+"/acceptance-admission", nil)
		req.SetPathValue("stage_id", acceptanceID.String())
		req = req.WithContext(context.WithValue(reqCtx, ctxKeyIdentity, testOperatorIdentity()))
		w := httptest.NewRecorder()
		s.handleAcceptanceAdmission(w, req)
		done <- w
	}()

	// Wait until the walk reaches its first (gated) transition — past the
	// pre-mutation reads, inside the mutation phase.
	<-blk.started
	// Disconnect the client AND wait well past the handler walk timeout so a
	// deadline, if the mutation phase carried one, would already have fired.
	cancelReq()
	time.Sleep(2 * acceptanceAdmissionWalkTimeout)
	// Release the slow transition; the walk must run to completion regardless.
	close(blk.release)

	w := <-done
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (walk settled despite disconnect + timeout):\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceAdmissionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.ShortCircuited {
		t.Fatalf("short_circuited = false, want true (walk completed):\n%s", w.Body.String())
	}
	if got := base.stagesByID[acceptanceID].State; got != run.StageStateSucceeded {
		t.Errorf("acceptance stage = %q, want succeeded (fully settled)", got)
	}
	if n := countByCategory(au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Errorf("acceptance_outcome_recorded = %d, want 1 (verdict recorded on the detached walk)", n)
	}
}

// TestAcceptanceAdmission_HostDispatch_SingleWriter is the cross-boundary
// interleaving test (#1936): with an admission short-circuit walk paused
// mid-transition (holding the per-stage lock), a concurrent host-dispatch marker
// on the SAME stage serializes behind the lock. Exactly one writer wins — the
// admission settles the stage (short_circuited:true) and the marker 409s — never
// a spawned-marker success AND a short-circuit settle on the same stage.
func TestAcceptanceAdmission_HostDispatch_SingleWriter(t *testing.T) {
	base := newOrchestratorRepo()
	runID, acceptanceID, _, ar := seedAdmissionRun(t, base, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	blk := &blockingTransitionRepo{orchestratorRepo: base, started: make(chan struct{}), release: make(chan struct{})}
	au := newAuditFake()
	o := &orchestrator.Orchestrator{Runs: blk, Audit: au, Artifacts: ar}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: blk, AuditRepo: au, ArtifactRepo: ar, Orchestrator: o})

	// Fire the admission; it acquires the per-stage lock and blocks in the first
	// (gated) transition, mid-walk.
	admDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { admDone <- postAdmission(t, s, acceptanceID, testOperatorIdentity()) }()
	<-blk.started

	// Fire the host-dispatch marker concurrently; it must block on the same lock.
	hdDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { hdDone <- postHostDispatch(t, s, runID, acceptanceID, withHostDispatchOperator) }()

	// Let the walk finish; the marker unblocks and observes the settled stage.
	close(blk.release)

	admW := <-admDone
	hdW := <-hdDone

	var admResp acceptanceAdmissionResponse
	if err := json.Unmarshal(admW.Body.Bytes(), &admResp); err != nil {
		t.Fatalf("unmarshal admission: %v", err)
	}
	if admW.Code != http.StatusOK || !admResp.ShortCircuited {
		t.Fatalf("admission = %d short_circuited=%v, want 200 short_circuited:true:\n%s", admW.Code, admResp.ShortCircuited, admW.Body.String())
	}
	if hdW.Code != http.StatusConflict {
		t.Fatalf("host-dispatch = %d, want 409 dispatch_not_admissible:\n%s", hdW.Code, hdW.Body.String())
	}

	// Single-writer invariant: the marker must NOT report a successful transition
	// alongside the admission's short-circuit settle.
	var hdResp hostDispatchResponse
	_ = json.Unmarshal(hdW.Body.Bytes(), &hdResp)
	if hdResp.Transitioned && admResp.ShortCircuited {
		t.Errorf("both writers mutated the stage: marker transitioned AND admission short-circuited")
	}
	if got := base.stagesByID[acceptanceID].State; got != run.StageStateSucceeded {
		t.Errorf("acceptance stage = %q, want succeeded (the single winning writer)", got)
	}
}
