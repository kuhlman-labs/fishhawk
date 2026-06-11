package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// specImplementPathConstraints is a feature_change workflow whose
// implement stage carries forbidden_paths, allowed_paths,
// max_files_changed, AND a required_outcomes constraint. The precheck
// must evaluate only the path/max_files constraints and drop
// required_outcomes (binding condition 1).
var specImplementPathConstraints = []byte(`version: "0.3"
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
        constraints:
          - max_files_changed: 3
          - forbidden_paths:
              - ".github/workflows/**"
          - allowed_paths:
              - "backend/**"
          - required_outcomes:
              - tests_added_or_updated
              - ci_green
`)

// newScopePrecheckServer wires a Server with a run carrying the given
// workflow spec, plus a plan stage. It returns the server, the audit
// fake, and the run row so callers can drive runScopePrecheck and read
// back the appended plan_scope_precheck entry.
func newScopePrecheckServer(t *testing.T, workflowSpec []byte) (*Server, *auditFake, *run.Run) {
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

// scopePlanBody builds a schema-valid standard_v1 plan body carrying the
// given scope files via the centralized planfixture so it tracks any new
// required schema fields. runScopePrecheck's parse mirrors the production
// precondition (validation already passed in handleShipPlan).
func scopePlanBody(t *testing.T, files []plan.ScopeFile) []byte {
	t.Helper()
	fileMaps := make([]any, 0, len(files))
	for _, f := range files {
		fileMaps = append(fileMaps, map[string]any{
			"path":      f.Path,
			"operation": string(f.Operation),
		})
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": fileMaps}
	})
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture plan does not validate: %v", err)
	}
	return body
}

// lastScopePrecheckEntry decodes the single plan_scope_precheck payload
// the audit fake captured, failing the test when none was written.
func lastScopePrecheckEntry(t *testing.T, au *auditFake) ScopePrecheckPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var payloads []ScopePrecheckPayload
	for _, ap := range au.appended {
		if ap.Category != categoryPlanScopePrecheck {
			continue
		}
		var p ScopePrecheckPayload
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("unmarshal scope precheck payload: %v", err)
		}
		payloads = append(payloads, p)
	}
	if len(payloads) != 1 {
		t.Fatalf("want exactly 1 plan_scope_precheck entry, got %d", len(payloads))
	}
	return payloads[0]
}

// countScopePrecheckEntries counts the plan_scope_precheck entries the
// audit fake captured.
func countScopePrecheckEntries(au *auditFake) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, ap := range au.appended {
		if ap.Category == categoryPlanScopePrecheck {
			n++
		}
	}
	return n
}

func TestScopePrecheck_ForbiddenMatchFlagged(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: ".github/workflows/ci.yml", Operation: plan.FileOpModify},
	})

	s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	got := lastScopePrecheckEntry(t, au)
	if got.WorkflowID != "feature_change" {
		t.Errorf("WorkflowID = %q, want feature_change", got.WorkflowID)
	}
	if got.ImplementStageID != "implement" {
		t.Errorf("ImplementStageID = %q, want implement", got.ImplementStageID)
	}
	if got.ScannedFiles != 1 {
		t.Errorf("ScannedFiles = %d, want 1", got.ScannedFiles)
	}
	if !hasViolation(got, "forbidden_paths") {
		t.Fatalf("want a forbidden_paths violation; got %+v", got.Violations)
	}
}

func TestScopePrecheck_AllowedViolationFlagged(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	// outside-of-allowlist file (allowed_paths is backend/**).
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "frontend/src/app.ts", Operation: plan.FileOpModify},
	})

	s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	got := lastScopePrecheckEntry(t, au)
	if !hasViolation(got, "allowed_paths") {
		t.Fatalf("want an allowed_paths violation; got %+v", got.Violations)
	}
}

func TestScopePrecheck_MaxFilesOverCapFlagged(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	// 4 files exceeds max_files_changed: 3. Keep them inside the allowlist
	// so the only violation is max_files_changed.
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/a.go", Operation: plan.FileOpModify},
		{Path: "backend/b.go", Operation: plan.FileOpModify},
		{Path: "backend/c.go", Operation: plan.FileOpModify},
		{Path: "backend/d.go", Operation: plan.FileOpModify},
	})

	s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	got := lastScopePrecheckEntry(t, au)
	if !hasViolation(got, "max_files_changed") {
		t.Fatalf("want a max_files_changed violation; got %+v", got.Violations)
	}
}

func TestScopePrecheck_CleanScopeWritesEmptyViolations(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify},
	})

	s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	got := lastScopePrecheckEntry(t, au)
	if len(got.Violations) != 0 {
		t.Fatalf("want zero violations on a clean scope; got %+v", got.Violations)
	}
	if got.ScannedFiles != 1 {
		t.Errorf("ScannedFiles = %d, want 1", got.ScannedFiles)
	}
}

// TestScopePrecheck_NoTestFileNoRequiredOutcomesViolation is the binding
// condition 1 assertion: a plan whose scope.files lists no _test.go must
// NOT produce a required_outcomes (tests_added_or_updated) violation — the
// precheck evaluates only the path/max_files constraints.
func TestScopePrecheck_NoTestFileNoRequiredOutcomesViolation(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	// A single non-test source file inside the allowlist, under the cap:
	// the only constraint it could trip is the (excluded) required_outcomes.
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify},
	})

	s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	got := lastScopePrecheckEntry(t, au)
	if hasViolation(got, "required_outcomes") {
		t.Fatalf("required_outcomes must be excluded from the precheck; got %+v", got.Violations)
	}
	if len(got.Violations) != 0 {
		t.Fatalf("want zero violations; got %+v", got.Violations)
	}
}

// TestScopePrecheck_DeleteUnderForbiddenStillFlags proves Status is not
// load-bearing: a delete-operation file under a forbidden glob still
// produces a forbidden_paths violation (path checks match on Path only).
func TestScopePrecheck_DeleteUnderForbiddenStillFlags(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: ".github/workflows/old.yml", Operation: plan.FileOpDelete},
	})

	s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	got := lastScopePrecheckEntry(t, au)
	if !hasViolation(got, "forbidden_paths") {
		t.Fatalf("a delete under a forbidden glob must still flag; got %+v", got.Violations)
	}
}

// TestScopePrecheck_NilSpecFailOpen verifies the fail-open contract: a run
// with no workflow spec writes no entry and never errors.
func TestScopePrecheck_NilSpecFailOpen(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, nil)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: ".github/workflows/ci.yml", Operation: plan.FileOpModify},
	})

	got := s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	if n := countScopePrecheckEntries(au); n != 0 {
		t.Fatalf("want no entry written for a nil-spec run; got %d", n)
	}
	if got != nil {
		t.Fatalf("fail-open must return a nil result (#963); got %+v", got)
	}
}

// TestScopePrecheck_NoImplementStageFailOpen verifies fail-open when the
// workflow has no implement stage.
func TestScopePrecheck_NoImplementStageFailOpen(t *testing.T) {
	specNoImplement := []byte(`version: "0.3"
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
	s, au, runRow := newScopePrecheckServer(t, specNoImplement)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: ".github/workflows/ci.yml", Operation: plan.FileOpModify},
	})

	got := s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	if n := countScopePrecheckEntries(au); n != 0 {
		t.Fatalf("want no entry written when there is no implement stage; got %d", n)
	}
	if got != nil {
		t.Fatalf("fail-open must return a nil result (#963); got %+v", got)
	}
}

// TestScopePrecheck_ReturnsComputedPayload pins the #963 return contract:
// the function returns the same result payload it records in the audit
// entry, so handleShipPlan can thread it into the plan-review prompt
// without a read-back.
func TestScopePrecheck_ReturnsComputedPayload(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: ".github/workflows/ci.yml", Operation: plan.FileOpModify},
	})

	got := s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)
	if got == nil {
		t.Fatal("want a non-nil result when the precheck ran")
	}

	recorded := lastScopePrecheckEntry(t, au)
	gotJSON, _ := json.Marshal(got)
	recordedJSON, _ := json.Marshal(recorded)
	if string(gotJSON) != string(recordedJSON) {
		t.Errorf("returned result diverges from the recorded audit payload:\nreturned: %s\nrecorded: %s", gotJSON, recordedJSON)
	}
	if !hasViolation(*got, "forbidden_paths") {
		t.Errorf("returned result missing the forbidden_paths violation: %+v", got.Violations)
	}
}

// TestScopePrecheck_PayloadCarriesCapWhenClean is the #983 headroom
// assertion: even a clean (no-violations) payload records the resolved
// max_files_changed so downstream surfaces can render the 29/30
// near-miss a violations-only payload hides.
func TestScopePrecheck_PayloadCarriesCapWhenClean(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify},
	})

	s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	got := lastScopePrecheckEntry(t, au)
	if len(got.Violations) != 0 {
		t.Fatalf("want zero violations; got %+v", got.Violations)
	}
	if got.MaxFilesChanged != 3 {
		t.Errorf("MaxFilesChanged = %d, want 3", got.MaxFilesChanged)
	}
}

// TestScopePrecheck_PayloadCarriesCapWhenOverCap asserts the cap rides
// the payload alongside the max_files_changed violation.
func TestScopePrecheck_PayloadCarriesCapWhenOverCap(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
	body := scopePlanBody(t, []plan.ScopeFile{
		{Path: "backend/a.go", Operation: plan.FileOpModify},
		{Path: "backend/b.go", Operation: plan.FileOpModify},
		{Path: "backend/c.go", Operation: plan.FileOpModify},
		{Path: "backend/d.go", Operation: plan.FileOpModify},
	})

	s.runScopePrecheck(context.Background(), runRow.ID, runRow.ID, body)

	got := lastScopePrecheckEntry(t, au)
	if !hasViolation(got, "max_files_changed") {
		t.Fatalf("want a max_files_changed violation; got %+v", got.Violations)
	}
	if got.MaxFilesChanged != 3 {
		t.Errorf("MaxFilesChanged = %d, want 3", got.MaxFilesChanged)
	}
}

// TestFlattenPathConstraints_MaxFilesMinWins asserts the flatten keeps
// the MINIMUM when two constraints set max_files_changed, matching the
// post-implement gate's mergeConstraints (trace.go) — the previous
// last-wins behavior was a latent divergence (#983).
func TestFlattenPathConstraints_MaxFilesMinWins(t *testing.T) {
	got := flattenPathConstraints([]spec.Constraint{
		{MaxFilesChanged: 5},
		{MaxFilesChanged: 8},
	})
	if got.MaxFilesChanged != 5 {
		t.Errorf("MaxFilesChanged = %d, want 5 (min-wins)", got.MaxFilesChanged)
	}
	got = flattenPathConstraints([]spec.Constraint{
		{MaxFilesChanged: 8},
		{MaxFilesChanged: 5},
	})
	if got.MaxFilesChanged != 5 {
		t.Errorf("MaxFilesChanged = %d, want 5 (min-wins regardless of order)", got.MaxFilesChanged)
	}
}

func hasViolation(p ScopePrecheckPayload, constraint string) bool {
	for _, v := range p.Violations {
		if v.Constraint == constraint {
			return true
		}
	}
	return false
}
