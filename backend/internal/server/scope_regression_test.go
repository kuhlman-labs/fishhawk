package server

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
)

// newScopeRegressionServer wires a Server with only the AuditRepo
// runScopeRegression needs, returning the server + audit fake.
func newScopeRegressionServer(t *testing.T) (*Server, *auditFake) {
	t.Helper()
	au := newAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	return s, au
}

// scopeFile is the map shape a standard_v1 plan's scope.files entry takes.
func scopeFileMap(path string) map[string]any {
	return map[string]any{"path": path, "operation": "modify"}
}

// regressionPlanBody builds a schema-valid standard_v1 plan whose top-level
// scope.files are topFiles and whose decomposition (when subPlanScopes is
// non-empty) carries one sub-plan per element, each scoping its file list.
// The schema requires ≥2 sub-plans, so callers exercising the sub-plan union
// pass at least two. The body is parsed by runScopeRegression exactly as the
// production precondition (validation already passed in handleShipPlan).
func regressionPlanBody(t *testing.T, topFiles []string, subPlanScopes [][]string) []byte {
	t.Helper()
	top := make([]any, 0, len(topFiles))
	for _, f := range topFiles {
		top = append(top, scopeFileMap(f))
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": top}
	})
	if len(subPlanScopes) > 0 {
		subPlans := make([]any, 0, len(subPlanScopes))
		for i, files := range subPlanScopes {
			sub := make([]any, 0, len(files))
			for _, f := range files {
				sub = append(sub, scopeFileMap(f))
			}
			subPlans = append(subPlans, map[string]any{
				"title":                        "Slice " + string(rune('A'+i)),
				"scope_hint":                   "slice scope",
				"scope":                        map[string]any{"files": sub},
				"predicted_runtime_minutes":    10,
				"predicted_runtime_confidence": "medium",
			})
		}
		m["decomposition"] = map[string]any{
			"rationale": "scope exceeded single-stage budget",
			"sub_plans": subPlans,
		}
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture plan does not validate: %v", err)
	}
	return body
}

// basePlan constructs a *plan.Plan directly (no validation) with the given
// top-level and single-sub-plan scope files — the revision-base argument
// runScopeRegression diffs against.
func basePlan(topFiles, subFiles []string) *plan.Plan {
	p := &plan.Plan{}
	for _, f := range topFiles {
		p.Scope.Files = append(p.Scope.Files, plan.ScopeFile{Path: f, Operation: plan.FileOpModify})
	}
	if len(subFiles) > 0 {
		sp := plan.SubPlanSummary{Title: "Slice A", Scope: &plan.Scope{}}
		for _, f := range subFiles {
			sp.Scope.Files = append(sp.Scope.Files, plan.ScopeFile{Path: f, Operation: plan.FileOpModify})
		}
		p.Decomposition = &plan.Decomposition{Rationale: "x", SubPlans: []plan.SubPlanSummary{sp}}
	}
	return p
}

// lastScopeRegressionEntry decodes the single plan_scope_regression payload
// the audit fake captured, failing when none (or more than one) was written.
func lastScopeRegressionEntry(t *testing.T, au *auditFake) ScopeRegressionPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var payloads []ScopeRegressionPayload
	for _, ap := range au.appended {
		if ap.Category != categoryPlanScopeRegression {
			continue
		}
		var p ScopeRegressionPayload
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("unmarshal scope regression payload: %v", err)
		}
		payloads = append(payloads, p)
	}
	if len(payloads) != 1 {
		t.Fatalf("want exactly 1 plan_scope_regression entry, got %d", len(payloads))
	}
	return payloads[0]
}

func countScopeRegressionEntries(au *auditFake) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, ap := range au.appended {
		if ap.Category == categoryPlanScopeRegression {
			n++
		}
	}
	return n
}

// TestScopedPaths_UnionsTopLevelAndSubPlanScopes pins the pure helper: the
// returned set is the slash-normalized, sorted UNION of top-level
// scope.files and every sub-plan's scope.files, deduped.
func TestScopedPaths_UnionsTopLevelAndSubPlanScopes(t *testing.T) {
	p := basePlan([]string{"b/a.go", "b/b.go"}, []string{"b/c.go", "b/a.go"})
	got := scopedPaths(p)
	want := []string{"b/a.go", "b/b.go", "b/c.go"} // a.go deduped across top + sub
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scopedPaths = %v, want %v", got, want)
	}
}

// TestRunScopeRegression_RegressingPass is the done-means behavioral test:
// base scopes {a,b,c} (c only in a sub-plan slice), the revision drops c and
// adds d/e, so RemovedFiles=[c], Regressed=true and exactly one entry is
// written. A no-op that returned an empty diff fails this. The dropped file c
// lives ONLY in a base sub-plan scope, pinning the sub-plan-union requirement.
func TestRunScopeRegression_RegressingPass(t *testing.T) {
	s, au := newScopeRegressionServer(t)
	base := basePlan([]string{"b/a.go", "b/b.go"}, []string{"b/c.go"})
	// New plan keeps a,b and adds d,e (in two sub-plans, ≥2 per schema),
	// dropping c entirely.
	newBody := regressionPlanBody(t, []string{"b/a.go", "b/b.go"}, [][]string{{"b/d.go"}, {"b/e.go"}})

	got := s.runScopeRegression(context.Background(), uuid.New(), uuid.New(), base, newBody)
	if got == nil {
		t.Fatal("runScopeRegression returned nil on a regressing pass")
	}
	if !got.Regressed {
		t.Errorf("Regressed = false, want true")
	}
	if !reflect.DeepEqual(got.RemovedFiles, []string{"b/c.go"}) {
		t.Errorf("RemovedFiles = %v, want [b/c.go] (the dropped sub-plan-scoped file)", got.RemovedFiles)
	}
	if !reflect.DeepEqual(got.AddedFiles, []string{"b/d.go", "b/e.go"}) {
		t.Errorf("AddedFiles = %v, want [b/d.go b/e.go]", got.AddedFiles)
	}

	entry := lastScopeRegressionEntry(t, au)
	if !entry.Regressed {
		t.Errorf("audit entry Regressed = false, want true")
	}
	if !reflect.DeepEqual(entry.RemovedFiles, []string{"b/c.go"}) {
		t.Errorf("audit RemovedFiles = %v, want [b/c.go]", entry.RemovedFiles)
	}
}

// TestRunScopeRegression_CleanReviseWritesEntry: a revise whose new scope is
// a superset of the base reports Regressed=false but STILL writes the entry
// (checked-and-clean, so a reader distinguishes it from never-checked).
func TestRunScopeRegression_CleanReviseWritesEntry(t *testing.T) {
	s, au := newScopeRegressionServer(t)
	base := basePlan([]string{"b/a.go"}, nil)
	newBody := regressionPlanBody(t, []string{"b/a.go", "b/b.go"}, nil)

	got := s.runScopeRegression(context.Background(), uuid.New(), uuid.New(), base, newBody)
	if got == nil {
		t.Fatal("runScopeRegression returned nil on a clean revise")
	}
	if got.Regressed {
		t.Errorf("Regressed = true, want false (new is a superset of base)")
	}
	if len(got.RemovedFiles) != 0 {
		t.Errorf("RemovedFiles = %v, want empty", got.RemovedFiles)
	}
	if !reflect.DeepEqual(got.AddedFiles, []string{"b/b.go"}) {
		t.Errorf("AddedFiles = %v, want [b/b.go]", got.AddedFiles)
	}
	if countScopeRegressionEntries(au) != 1 {
		t.Errorf("entries = %d, want 1 (checked-and-clean still records)", countScopeRegressionEntries(au))
	}
}

// TestRunScopeRegression_NonReviseSkips: base==nil (a non-revise ship) means
// the gate skips entirely — returns nil and writes no entry.
func TestRunScopeRegression_NonReviseSkips(t *testing.T) {
	s, au := newScopeRegressionServer(t)
	newBody := regressionPlanBody(t, []string{"b/a.go"}, nil)

	got := s.runScopeRegression(context.Background(), uuid.New(), uuid.New(), nil, newBody)
	if got != nil {
		t.Errorf("runScopeRegression = %+v, want nil on a non-revise ship", got)
	}
	if countScopeRegressionEntries(au) != 0 {
		t.Errorf("entries = %d, want 0 (non-revise ship writes nothing)", countScopeRegressionEntries(au))
	}
}

// TestRunScopeRegression_NilAuditRepoSkips: with no AuditRepo wired the
// gate cannot record its result, so it returns nil rather than computing a
// diff it can't persist (fail-open).
func TestRunScopeRegression_NilAuditRepoSkips(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no AuditRepo
	base := basePlan([]string{"b/a.go", "b/c.go"}, nil)
	newBody := regressionPlanBody(t, []string{"b/a.go"}, nil)

	if got := s.runScopeRegression(context.Background(), uuid.New(), uuid.New(), base, newBody); got != nil {
		t.Errorf("runScopeRegression = %+v, want nil with no AuditRepo", got)
	}
}

// TestRunScopeRegression_MalformedBodyFailsOpen: an unparseable new body
// returns nil and does NOT unwind the ship (no entry, no panic).
func TestRunScopeRegression_MalformedBodyFailsOpen(t *testing.T) {
	s, au := newScopeRegressionServer(t)
	base := basePlan([]string{"b/a.go"}, nil)

	got := s.runScopeRegression(context.Background(), uuid.New(), uuid.New(), base, []byte("{not valid json"))
	if got != nil {
		t.Errorf("runScopeRegression = %+v, want nil on a malformed body", got)
	}
	if countScopeRegressionEntries(au) != 0 {
		t.Errorf("entries = %d, want 0 on a parse failure", countScopeRegressionEntries(au))
	}
}

// TestRunScopeRegression_AppendErrorFailsOpen: an audit-append failure
// returns nil (fail-open) rather than unwinding the ship.
func TestRunScopeRegression_AppendErrorFailsOpen(t *testing.T) {
	s, au := newScopeRegressionServer(t)
	au.appendErr = errors.New("db down")
	base := basePlan([]string{"b/a.go", "b/c.go"}, nil)
	newBody := regressionPlanBody(t, []string{"b/a.go"}, nil) // drops c → would-be regression

	got := s.runScopeRegression(context.Background(), uuid.New(), uuid.New(), base, newBody)
	if got != nil {
		t.Errorf("runScopeRegression = %+v, want nil when the audit append fails", got)
	}
}
