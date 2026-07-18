package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
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

// planWarningsCapSpec is a feature_change workflow whose implement stage
// declares max_files_changed = 2, the resolved cap the over-cap advisory
// (#2053) checks len(scope.files) against.
var planWarningsCapSpec = []byte(`version: "0.3"
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
          - max_files_changed: 2
`)

// overCapPlanBody builds a schema-valid standard_v1 plan whose top-level
// scope.files has numFiles entries and whose over_cap flag is set per overCap
// (nil = omit the field entirely). It drives the #2053 over-cap advisory matrix:
// the advisory must be derived from numFiles vs the resolved cap ALONE, never
// from the over_cap value.
func overCapPlanBody(t *testing.T, numFiles int, overCap *bool) []byte {
	t.Helper()
	fileMaps := make([]any, 0, numFiles)
	for i := 0; i < numFiles; i++ {
		fileMaps = append(fileMaps, map[string]any{
			"path":      fmt.Sprintf("backend/internal/foo/f%d.go", i),
			"operation": "modify",
		})
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": fileMaps}
	})
	if overCap != nil {
		m["over_cap"] = *overCap
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

// hasOverCapWarning reports whether any warning names the scanned count and the
// cap in the #2053 over-cap advisory shape.
func hasOverCapWarning(warnings []string, count, capLimit int) bool {
	for _, w := range warnings {
		if strings.Contains(w, fmt.Sprintf("declares %d files", count)) &&
			strings.Contains(w, fmt.Sprintf("cap of %d", capLimit)) {
			return true
		}
	}
	return false
}

// TestRunPlanWarnings_OverCap_FlagMatrix is the condition-1 matrix (#2053):
// an over-cap plan (len(scope.files)=3 > resolved cap 2) fires the deterministic
// over-cap advisory in ALL THREE flag states — over_cap omitted, false, AND true
// — while an under-cap plan (1 file) never fires regardless of the flag. Together
// these prove the advisory is derived from the file count ALONE and never reads
// parsedPlan.OverCap; in particular the over_cap:true + over-cap cell catches a
// flag-reading regression like `if !plan.OverCap && count > cap { warn }` that a
// {omitted,false} × over matrix alone would miss.
func TestRunPlanWarnings_OverCap_FlagMatrix(t *testing.T) {
	const capLimit = 2
	for _, tc := range []struct {
		name     string
		numFiles int
		overCap  *bool
		wantFire bool
	}{
		{name: "over-cap, flag omitted -> fire", numFiles: 3, overCap: nil, wantFire: true},
		{name: "over-cap, flag false -> fire", numFiles: 3, overCap: boolPtr(false), wantFire: true},
		{name: "over-cap, flag true -> fire", numFiles: 3, overCap: boolPtr(true), wantFire: true},
		{name: "under-cap, flag true -> no fire", numFiles: 1, overCap: boolPtr(true), wantFire: false},
		{name: "under-cap, no flag -> no fire", numFiles: 1, overCap: nil, wantFire: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, au, runRow := newScopePrecheckServer(t, planWarningsCapSpec)
			body := overCapPlanBody(t, tc.numFiles, tc.overCap)

			got := s.runPlanWarnings(context.Background(), runRow.ID, uuid.New(), body)

			entries := planWarningsEntries(t, au)
			if tc.wantFire {
				if got == nil {
					t.Fatal("want a non-nil result when the over-cap advisory fires")
				}
				if !hasOverCapWarning(got.Warnings, tc.numFiles, capLimit) {
					t.Errorf("returned warnings = %v, want one naming count=%d and cap=%d", got.Warnings, tc.numFiles, capLimit)
				}
				if len(entries) != 1 {
					t.Fatalf("plan_warnings entries = %d, want 1", len(entries))
				}
				if !hasOverCapWarning(entries[0].Warnings, tc.numFiles, capLimit) {
					t.Errorf("recorded warnings = %v, want one naming count=%d and cap=%d", entries[0].Warnings, tc.numFiles, capLimit)
				}
			} else {
				if got != nil {
					t.Fatalf("want nil result for an under-cap plan; got %+v", got)
				}
				if len(entries) != 0 {
					t.Fatalf("plan_warnings entries = %d, want 0 for an under-cap plan", len(entries))
				}
			}
		})
	}
}

// TestRunPlanWarnings_OverCap_NilRunRepo_FailsOpen pins the fail-open leg for a
// nil RunRepo (#2053): the over-cap advisory is skipped (the cap cannot be
// resolved without the run) and the plan settle is never blocked. An over-cap
// single-slice plan therefore records NO entry.
func TestRunPlanWarnings_OverCap_NilRunRepo_FailsOpen(t *testing.T) {
	au := newAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au}) // RunRepo intentionally nil.
	body := overCapPlanBody(t, 3, nil)

	got := s.runPlanWarnings(context.Background(), uuid.New(), uuid.New(), body)

	if got != nil {
		t.Fatalf("want nil result with no RunRepo (over-cap check skipped); got %+v", got)
	}
	if entries := planWarningsEntries(t, au); len(entries) != 0 {
		t.Fatalf("plan_warnings entries = %d, want 0 (cap unresolvable, fail-open)", len(entries))
	}
}

// TestRunPlanWarnings_OverCap_GetRunError_FailsOpen pins the fail-open leg when
// GetRun errors (#2053): an unseeded run id means the cap cannot be resolved, so
// the over-cap advisory is skipped and the plan settle is never blocked.
func TestRunPlanWarnings_OverCap_GetRunError_FailsOpen(t *testing.T) {
	s, au, _ := newScopePrecheckServer(t, planWarningsCapSpec)
	body := overCapPlanBody(t, 3, nil)

	// A random run id the orchestrator repo never seeded -> GetRun ErrNotFound.
	got := s.runPlanWarnings(context.Background(), uuid.New(), uuid.New(), body)

	if got != nil {
		t.Fatalf("want nil result when GetRun errors (over-cap check skipped); got %+v", got)
	}
	if entries := planWarningsEntries(t, au); len(entries) != 0 {
		t.Fatalf("plan_warnings entries = %d, want 0 (GetRun error, fail-open)", len(entries))
	}
}

// TestRunPlanWarnings_OverCap_NoImplementStage_FailsOpen pins the fail-open leg
// when resolveImplementConstraints returns ok=false because the workflow has no
// implement stage (#2053): no cap to check against, so the over-cap advisory is
// skipped and the plan settle is never blocked.
func TestRunPlanWarnings_OverCap_NoImplementStage_FailsOpen(t *testing.T) {
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
	body := overCapPlanBody(t, 3, nil)

	got := s.runPlanWarnings(context.Background(), runRow.ID, uuid.New(), body)

	if got != nil {
		t.Fatalf("want nil result when the workflow has no implement stage; got %+v", got)
	}
	if entries := planWarningsEntries(t, au); len(entries) != 0 {
		t.Fatalf("plan_warnings entries = %d, want 0 (no implement stage, fail-open)", len(entries))
	}
}

// TestRunPlanWarnings_OverCap_NoCapConstraint_FailsOpen pins the fail-open leg
// when the implement stage resolves but declares no max_files_changed (#2053):
// MaxFilesChanged is 0, so there is no cap to exceed and the over-cap advisory is
// skipped — an over-cap-by-count plan against an uncapped workflow records no
// entry.
func TestRunPlanWarnings_OverCap_NoCapConstraint_FailsOpen(t *testing.T) {
	specNoCap := []byte(`version: "0.3"
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
`)
	s, au, runRow := newScopePrecheckServer(t, specNoCap)
	body := overCapPlanBody(t, 5, nil)

	got := s.runPlanWarnings(context.Background(), runRow.ID, uuid.New(), body)

	if got != nil {
		t.Fatalf("want nil result when no max_files_changed cap is configured; got %+v", got)
	}
	if entries := planWarningsEntries(t, au); len(entries) != 0 {
		t.Fatalf("plan_warnings entries = %d, want 0 (no cap, fail-open)", len(entries))
	}
}

// TestShipPlan_OverCapAdvisory_ReachesGetPlanField is the condition-2 end-to-end
// test (#2053): an over-cap plan ingested at ship/upload flows through
// handleShipPlan -> runPlanWarnings -> a plan_warnings audit entry, and the
// over-cap advisory text (naming the scanned count and the cap) reaches the
// serialized-out form the fishhawk_get_plan PlanWarnings field is built from.
//
// The get_plan resolver (backend/cmd/fishhawk-mcp/tools.go::loadPlanWarnings)
// selects the NEWEST plan_warnings entry and decodes its payload's `warnings`
// array — the exact contract replicated here via getPlanWarningsField — so this
// asserts the operator-visible cross-boundary contract, not merely the internal
// warnings slice.
func TestShipPlan_OverCapAdvisory_ReachesGetPlanField(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	// specGatingReviewersWithConstraints declares max_files_changed: 3.
	s, sf, _, au, _ := newPlanServerWithReviewer(t, runID, stageID, reviewer, specGatingReviewersWithConstraints)
	priv, _ := sf.issue(t, runID)
	// 4 scope files exceeds the cap of 3 -> the over-cap advisory must fire.
	body := overCapPlanBody(t, 4, nil)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	warnings := getPlanWarningsField(t, au, runID)
	if !hasOverCapWarning(warnings, 4, 3) {
		t.Errorf("get_plan plan_warnings field = %v, want one naming count=4 and cap=3", warnings)
	}
}

// getPlanWarningsField replicates the fishhawk_get_plan PlanWarnings serialization
// (backend/cmd/fishhawk-mcp/tools.go::loadPlanWarnings), which cannot be imported
// across the module boundary: it selects the NEWEST plan_warnings audit entry for
// the run and returns its payload's decoded `warnings` array. Returns nil when no
// entry exists — the field-omitted case the get_plan resolver produces.
func getPlanWarningsField(t *testing.T, au *auditFake, _ uuid.UUID) []string {
	t.Helper()
	entries := planWarningsEntries(t, au)
	if len(entries) == 0 {
		return nil
	}
	return entries[len(entries)-1].Warnings
}
