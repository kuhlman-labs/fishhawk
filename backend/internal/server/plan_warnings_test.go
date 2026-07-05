package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// nonTransitioningRunRepo embeds run.BaseFake (no-op ErrNotFound stubs) and
// records whether TransitionStage was ever invoked, pinning runPlanWarnings'
// non-blocking contract (binding condition 2): the gate must never
// transition or fail the plan stage. FailStage itself is built on top of
// TransitionStage (backend/internal/run/failure.go), so a false
// `transitioned` flag proves neither was called.
type nonTransitioningRunRepo struct {
	run.BaseFake
	transitioned bool
}

func (r *nonTransitioningRunRepo) TransitionStage(_ context.Context, _ uuid.UUID, _ run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	r.transitioned = true
	return nil, run.ErrNotFound
}

// newPlanWarningsServer wires a Server with only an AuditRepo fake and a
// non-transitioning RunRepo fake. runPlanWarnings guards solely on
// AuditRepo (unlike the sibling plan-gate advisories it needs no
// RunRepo/workflow spec/GitHub client to evaluate plan.Warnings()), so no
// run needs to be seeded; the RunRepo fake exists only to assert the
// non-blocking contract.
func newPlanWarningsServer(t *testing.T) (*Server, *auditFake, *nonTransitioningRunRepo) {
	t.Helper()
	au := newAuditFake()
	rr := &nonTransitioningRunRepo{}
	s := New(Config{
		Addr:      "127.0.0.1:0",
		AuditRepo: au,
		RunRepo:   rr,
	})
	return s, au, rr
}

// warningsSubPlan is one decomposition sub-plan for the plan-warnings test
// fixture: a title, its own disjoint scope.files, and optional depends_on
// indices.
type warningsSubPlan struct {
	title     string
	files     []plan.ScopeFile
	dependsOn []int
}

// warningsPlanBody builds a schema-valid standard_v1 plan body. With no
// sub-plans it is a plain single-slice plan (no decomposition); with
// sub-plans it builds a decomposition whose slices carry the given
// scope.files and optional depends_on edges — the shape runPlanWarnings
// evaluates via plan.Warnings() (#1684). The parent's
// predicted_runtime_minutes is kept equal to the sub-plan runtime sum so
// the (unrelated) runtime-compression advisory never fires here,
// isolating the depends_on assertion.
func warningsPlanBody(t *testing.T, subs []warningsSubPlan) []byte {
	t.Helper()
	m := planfixture.Valid()
	if len(subs) > 0 {
		subMaps := make([]any, 0, len(subs))
		sum := 0
		for _, sp := range subs {
			fileMaps := make([]any, 0, len(sp.files))
			for _, f := range sp.files {
				fileMaps = append(fileMaps, map[string]any{"path": f.Path, "operation": string(f.Operation)})
			}
			subMap := map[string]any{
				"title":                        sp.title,
				"scope_hint":                   sp.title + " slice",
				"scope":                        map[string]any{"files": fileMaps},
				"predicted_runtime_minutes":    10,
				"predicted_runtime_confidence": "medium",
			}
			if len(sp.dependsOn) > 0 {
				deps := make([]any, len(sp.dependsOn))
				for i, d := range sp.dependsOn {
					deps[i] = d
				}
				subMap["depends_on"] = deps
			}
			subMaps = append(subMaps, subMap)
			sum += 10
		}
		m["decomposition"] = map[string]any{
			"rationale": "scope exceeded single-stage budget",
			"sub_plans": subMaps,
		}
		m["predicted_runtime_minutes"] = sum
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

// planWarningsEntries decodes every plan_warnings payload the audit fake
// captured.
func planWarningsEntries(t *testing.T, au *auditFake) []PlanWarningsPayload {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []PlanWarningsPayload
	for _, ap := range au.appended {
		if ap.Category != categoryPlanWarnings {
			continue
		}
		var p PlanWarningsPayload
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("unmarshal plan warnings payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// TestRunPlanWarnings_AllEmptyDependsOn_Fires is the FIRE case: a
// >=2-slice decomposition whose sub_plans ALL omit depends_on causes
// runPlanWarnings to append exactly one plan_warnings entry whose
// payload.warnings contains the all-empty-depends_on advisory substring.
func TestRunPlanWarnings_AllEmptyDependsOn_Fires(t *testing.T) {
	s, au, rr := newPlanWarningsServer(t)
	body := warningsPlanBody(t, []warningsSubPlan{
		{title: "Part A", files: []plan.ScopeFile{{Path: "a.go", Operation: plan.FileOpCreate}}},
		{title: "Part B", files: []plan.ScopeFile{{Path: "b.go", Operation: plan.FileOpCreate}}},
	})

	got := s.runPlanWarnings(context.Background(), uuid.New(), uuid.New(), body)

	if got == nil {
		t.Fatal("want a non-nil result when a warning fires")
	}
	entries := planWarningsEntries(t, au)
	if len(entries) != 1 {
		t.Fatalf("plan_warnings entries = %d, want 1", len(entries))
	}
	found := false
	for _, w := range entries[0].Warnings {
		if strings.Contains(w, "none declares depends_on") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one containing %q", entries[0].Warnings, "none declares depends_on")
	}
	if rr.transitioned {
		t.Error("runPlanWarnings must never transition the stage")
	}
}

// TestRunPlanWarnings_DependsOnDeclared_NoFire is the edge-declared
// NO-FIRE case: a >=2-slice decomposition with at least one sub_plan
// declaring depends_on appends NO plan_warnings entry.
func TestRunPlanWarnings_DependsOnDeclared_NoFire(t *testing.T) {
	s, au, rr := newPlanWarningsServer(t)
	body := warningsPlanBody(t, []warningsSubPlan{
		{title: "Part A", files: []plan.ScopeFile{{Path: "a.go", Operation: plan.FileOpCreate}}},
		{title: "Part B", files: []plan.ScopeFile{{Path: "b.go", Operation: plan.FileOpCreate}}, dependsOn: []int{0}},
	})

	got := s.runPlanWarnings(context.Background(), uuid.New(), uuid.New(), body)

	if got != nil {
		t.Fatalf("want nil result when no warning fires; got %+v", got)
	}
	if entries := planWarningsEntries(t, au); len(entries) != 0 {
		t.Fatalf("plan_warnings entries = %d, want 0", len(entries))
	}
	if rr.transitioned {
		t.Error("runPlanWarnings must never transition the stage")
	}
}

// TestRunPlanWarnings_SingleSlice_NoFire is the single-slice / no-
// decomposition NO-FIRE case: a plan with no decomposition appends no
// entry.
func TestRunPlanWarnings_SingleSlice_NoFire(t *testing.T) {
	s, au, rr := newPlanWarningsServer(t)
	body := warningsPlanBody(t, nil)

	got := s.runPlanWarnings(context.Background(), uuid.New(), uuid.New(), body)

	if got != nil {
		t.Fatalf("want nil result for a non-decomposed plan; got %+v", got)
	}
	if entries := planWarningsEntries(t, au); len(entries) != 0 {
		t.Fatalf("plan_warnings entries = %d, want 0", len(entries))
	}
	if rr.transitioned {
		t.Error("runPlanWarnings must never transition the stage")
	}
}

// TestRunPlanWarnings_AppendError_FailsOpen pins the fail-open contract on
// the audit-append leg (binding condition 2 / fix-up concern): a fire case
// (a >=2-slice all-empty-depends_on decomposition) whose AuditRepo.
// AppendChained call fails still returns the computed non-nil payload and
// never transitions the stage — the append error is WARN-logged and
// swallowed, not propagated.
func TestRunPlanWarnings_AppendError_FailsOpen(t *testing.T) {
	s, au, rr := newPlanWarningsServer(t)
	au.appendErr = errors.New("plan warnings: append boom")
	body := warningsPlanBody(t, []warningsSubPlan{
		{title: "Part A", files: []plan.ScopeFile{{Path: "a.go", Operation: plan.FileOpCreate}}},
		{title: "Part B", files: []plan.ScopeFile{{Path: "b.go", Operation: plan.FileOpCreate}}},
	})

	got := s.runPlanWarnings(context.Background(), uuid.New(), uuid.New(), body)

	if got == nil {
		t.Fatal("want a non-nil result despite the append failure (fail-open)")
	}
	found := false
	for _, w := range got.Warnings {
		if strings.Contains(w, "none declares depends_on") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one containing %q", got.Warnings, "none declares depends_on")
	}
	if entries := planWarningsEntries(t, au); len(entries) != 0 {
		t.Fatalf("plan_warnings entries = %d, want 0 (append failed, nothing recorded)", len(entries))
	}
	if rr.transitioned {
		t.Error("runPlanWarnings must never transition the stage")
	}
}

// TestRunPlanWarnings_ParseFailure_FailsOpen pins the fail-open contract:
// an unparseable plan body writes no entry, returns nil, and never
// transitions the stage.
func TestRunPlanWarnings_ParseFailure_FailsOpen(t *testing.T) {
	s, au, rr := newPlanWarningsServer(t)

	got := s.runPlanWarnings(context.Background(), uuid.New(), uuid.New(), []byte(`not json`))

	if got != nil {
		t.Fatalf("want nil result on parse failure; got %+v", got)
	}
	if entries := planWarningsEntries(t, au); len(entries) != 0 {
		t.Fatalf("plan_warnings entries = %d, want 0", len(entries))
	}
	if rr.transitioned {
		t.Error("runPlanWarnings must never transition the stage")
	}
}

// TestRunPlanWarnings_NilAuditRepo_FailsOpen pins the guard-only-on-
// AuditRepo contract: a Server with no AuditRepo returns nil and never
// panics.
func TestRunPlanWarnings_NilAuditRepo_FailsOpen(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	body := warningsPlanBody(t, []warningsSubPlan{
		{title: "Part A", files: []plan.ScopeFile{{Path: "a.go", Operation: plan.FileOpCreate}}},
		{title: "Part B", files: []plan.ScopeFile{{Path: "b.go", Operation: plan.FileOpCreate}}},
	})

	got := s.runPlanWarnings(context.Background(), uuid.New(), uuid.New(), body)

	if got != nil {
		t.Fatalf("want nil result with no AuditRepo; got %+v", got)
	}
}
