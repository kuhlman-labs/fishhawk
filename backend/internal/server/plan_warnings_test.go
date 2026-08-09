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
// This half proves the PRODUCTION leg: the real handleShipPlan ->
// runPlanWarnings writes an audit payload carrying the over-cap advisory. The
// SELECTION leg — the real get_plan resolver
// (backend/cmd/fishhawk-mcp/tools.go::loadPlanWarnings) picking that payload out
// of the audit log and decoding its `warnings` array — is asserted against the
// GENUINE resolver by TestGetPlan_OverCapAdvisory_ReachesPlanWarningsField in
// the fishhawk-mcp package (loadPlanWarnings lives in package main and is
// unimportable here). getPlanWarningsField below only REPLICATES that selection
// so this server-package test can assert the payload text without the
// cross-module hop; the fishhawk-mcp seam test is what catches a resolver
// divergence.
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
// entry exists — the field-omitted case the get_plan resolver produces. Because
// this REPLICATES rather than invokes the resolver, the genuine selection path is
// pinned separately by TestGetPlan_OverCapAdvisory_ReachesPlanWarningsField in the
// fishhawk-mcp package, which exercises the real loadPlanWarnings.
func getPlanWarningsField(t *testing.T, au *auditFake, _ uuid.UUID) []string {
	t.Helper()
	entries := planWarningsEntries(t, au)
	if len(entries) == 0 {
		return nil
	}
	return entries[len(entries)-1].Warnings
}

// splitProposalMap returns a valid two-phase expand->contract split_proposal
// map (each phase carrying its own disjoint scope.files, depends_on edge
// expand(0) <- contract(1)) for the over-cap reject accept-path tests (#2055).
func splitProposalMap() map[string]any {
	return map[string]any{
		"rationale": "split expand->migrate->contract; each phase at or under the cap",
		"phases": []any{
			map[string]any{
				"title": "Expand",
				"scope": map[string]any{"files": []any{
					map[string]any{"path": "backend/internal/foo/expand.go", "operation": "modify"},
				}},
			},
			map[string]any{
				"title":      "Contract",
				"depends_on": []any{0},
				"scope": map[string]any{"files": []any{
					map[string]any{"path": "backend/internal/foo/contract.go", "operation": "modify"},
				}},
			},
		},
	}
}

// overCapPlanBodyWithSplit builds a schema-valid standard_v1 plan whose
// scope.files has numFiles entries, sets over_cap per overCap (nil = omit), and
// attaches a valid split_proposal when withSplit is true — the shapes the
// count-derived overCapSplitRejection matrix (#2055) exercises.
func overCapPlanBodyWithSplit(t *testing.T, numFiles int, overCap *bool, withSplit bool) []byte {
	t.Helper()
	fileMaps := make([]any, 0, numFiles)
	for i := 0; i < numFiles; i++ {
		fileMaps = append(fileMaps, map[string]any{
			"path":      fmt.Sprintf("backend/internal/bar/f%d.go", i),
			"operation": "modify",
		})
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": fileMaps}
	})
	if overCap != nil {
		m["over_cap"] = *overCap
	}
	if withSplit {
		m["split_proposal"] = splitProposalMap()
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	// Validate is schema-only (no semanticCheck); the authoritative over-cap gate
	// decodes with json.Unmarshal exactly like handleShipPlan, so an over_cap:true
	// + no-split body is admitted for the count-derived reject to judge.
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture plan does not validate: %v", err)
	}
	return body
}

// unmarshalPlan decodes a plan body WITHOUT semanticCheck, mirroring the
// json.Unmarshal handleShipPlan uses for the authoritative over-cap gate — the
// gate judges an over-cap-by-count monolith on the file count alone, never on
// over_cap or any in-artifact semantic coupling, which is the whole point of the
// flag-independence keystone.
func unmarshalPlan(t *testing.T, body []byte) *plan.Plan {
	t.Helper()
	var p plan.Plan
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal plan body: %v", err)
	}
	return &p
}

// TestOverCapSplitRejection_FlagMatrix is the E50 keystone (#2055 constraint
// #1): the count-derived server reject fires for an over-cap-BY-COUNT monolith
// lacking a split_proposal in ALL THREE over_cap states (omitted, false, true),
// accepts an over-cap plan carrying a valid split_proposal, and leaves an
// under-cap plan untouched. The over_cap:true + no-split cell is what catches a
// flag-reading regression like `if plan.OverCap && count > cap` — the reject
// must be derived from the file count ALONE and never read over_cap.
func TestOverCapSplitRejection_FlagMatrix(t *testing.T) {
	const capLimit = 2
	for _, tc := range []struct {
		name       string
		numFiles   int
		overCap    *bool
		withSplit  bool
		wantReject bool
	}{
		{name: "over-cap, flag omitted, no split -> reject", numFiles: 3, overCap: nil, withSplit: false, wantReject: true},
		{name: "over-cap, flag false, no split -> reject", numFiles: 3, overCap: boolPtr(false), withSplit: false, wantReject: true},
		{name: "over-cap, flag true, no split -> reject", numFiles: 3, overCap: boolPtr(true), withSplit: false, wantReject: true},
		{name: "over-cap, flag omitted, WITH split -> accept", numFiles: 3, overCap: nil, withSplit: true, wantReject: false},
		{name: "over-cap, flag true, WITH split -> accept", numFiles: 3, overCap: boolPtr(true), withSplit: true, wantReject: false},
		{name: "under-cap, flag true, no split -> accept", numFiles: 1, overCap: boolPtr(true), withSplit: false, wantReject: false},
		{name: "under-cap, flag omitted -> accept", numFiles: 1, overCap: nil, withSplit: false, wantReject: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, runRow := newScopePrecheckServer(t, planWarningsCapSpec)
			p := unmarshalPlan(t, overCapPlanBodyWithSplit(t, tc.numFiles, tc.overCap, tc.withSplit))

			reason := s.overCapSplitRejection(context.Background(), runRow.ID, p)

			if tc.wantReject {
				if reason == "" {
					t.Fatal("want a non-empty reject reason for an over-cap-by-count plan without split_proposal")
				}
				if !strings.Contains(reason, fmt.Sprintf("declares %d files", tc.numFiles)) ||
					!strings.Contains(reason, fmt.Sprintf("cap of %d", capLimit)) {
					t.Errorf("reason = %q, want it to name count=%d and cap=%d", reason, tc.numFiles, capLimit)
				}
				if !strings.Contains(reason, "split_proposal") {
					t.Errorf("reason = %q, want it to name split_proposal as the remedy", reason)
				}
			} else if reason != "" {
				t.Errorf("want no reject reason; got %q", reason)
			}
		})
	}
}

// TestOverCapSplitRejection_NilRunRepo_FailsOpen pins the fail-open leg for a
// nil RunRepo (#2055): the cap cannot be resolved, so overCapByCount returns
// ok=false and the reject is skipped — an over-cap-by-count monolith is NOT
// blocked when the cap is unresolvable.
func TestOverCapSplitRejection_NilRunRepo_FailsOpen(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // RunRepo intentionally nil.
	p := unmarshalPlan(t, overCapPlanBodyWithSplit(t, 3, nil, false))

	if reason := s.overCapSplitRejection(context.Background(), uuid.New(), p); reason != "" {
		t.Errorf("want no reject with a nil RunRepo (cap unresolvable, fail-open); got %q", reason)
	}
}

// TestOverCapSplitRejection_UnresolvedCap_FailsOpen pins the fail-open leg when
// the workflow has no implement stage (#2055): resolveImplementConstraints
// returns ok=false, so there is no cap to exceed and the reject is skipped even
// for an over-cap-by-count monolith without a split.
func TestOverCapSplitRejection_UnresolvedCap_FailsOpen(t *testing.T) {
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
	s, _, runRow := newScopePrecheckServer(t, specNoImplement)
	p := unmarshalPlan(t, overCapPlanBodyWithSplit(t, 3, nil, false))

	if reason := s.overCapSplitRejection(context.Background(), runRow.ID, p); reason != "" {
		t.Errorf("want no reject when the workflow has no implement stage (fail-open); got %q", reason)
	}
}

// --- irreducible (#2412) ---

// overCapIrreducibleBody builds a schema-valid standard_v1 plan whose scope.files
// has numFiles entries and which carries an irreducible declaration with the
// given rationale (and optional atomicity_basis). It deliberately does NOT run
// semanticCheck (Validate is schema-only), so a blank/whitespace-only rationale
// — rejected by plan.Parse but admitted by the schema's minLength:1 for a space —
// is still produced for the gate to judge (mirroring handleShipPlan's
// json.Unmarshal decode path).
func overCapIrreducibleBody(t *testing.T, numFiles int, rationale, basis string) []byte {
	t.Helper()
	fileMaps := make([]any, 0, numFiles)
	for i := 0; i < numFiles; i++ {
		fileMaps = append(fileMaps, map[string]any{
			"path":      fmt.Sprintf("backend/internal/baz/f%d.go", i),
			"operation": "modify",
		})
	}
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": fileMaps}
	})
	irr := map[string]any{"rationale": rationale}
	if basis != "" {
		irr["atomicity_basis"] = basis
	}
	m["irreducible"] = irr
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture plan does not validate: %v", err)
	}
	return body
}

// TestOverCapSplitRejection_IrreducibleAccepted is the counterfactual vehicle for
// the Irreducible.Declared() short-circuit in overCapSplitRejection (#2412): an
// over-cap-by-count plan carrying a well-formed irreducible and NO split_proposal
// is ACCEPTED (no reject reason). Deleting the `if parsedPlan.Irreducible.Declared()
// { return "" }` line makes the same plan reject again.
func TestOverCapSplitRejection_IrreducibleAccepted(t *testing.T) {
	s, _, runRow := newScopePrecheckServer(t, planWarningsCapSpec)
	p := unmarshalPlan(t, overCapIrreducibleBody(t, 3, "the method's receiver base type must live in its own package", "Go receiver rule"))

	if reason := s.overCapSplitRejection(context.Background(), runRow.ID, p); reason != "" {
		t.Errorf("want no reject for an over-cap plan carrying a well-formed irreducible; got %q", reason)
	}
}

// TestOverCapSplitRejection_BlankRationaleStillRejects pins the per-failure-mode
// branch that a whitespace-only rationale is NOT a declaration (#2412): the
// over-cap reject STILL fires. This is the observable proof that Declared()
// trims — a blank rationale is exactly the bare flag the design refuses, so it
// must not widen the reject into an advisory.
func TestOverCapSplitRejection_BlankRationaleStillRejects(t *testing.T) {
	s, _, runRow := newScopePrecheckServer(t, planWarningsCapSpec)
	p := unmarshalPlan(t, overCapIrreducibleBody(t, 3, "   ", ""))

	reason := s.overCapSplitRejection(context.Background(), runRow.ID, p)
	if reason == "" {
		t.Fatal("want the over-cap reject to STILL fire for a whitespace-only irreducible rationale (not a declaration)")
	}
	if !strings.Contains(reason, "declares 3 files") || !strings.Contains(reason, "cap of 2") {
		t.Errorf("reason = %q, want it to name count=3 and cap=2", reason)
	}
}

// TestRunPlanWarnings_IrreducibleDoesNotSuppressOverCapAdvisory is the
// counterfactual vehicle for the advisory ordering/independence in runPlanWarnings
// (#2412): an over-cap plan carrying irreducible emits the count-derived over-cap
// advisory (present AND FIRST) even with irreducible declared, plus the irreducible
// advisory. Deleting the unconditional overCapWarning append turns it red — the
// ordering is the observable proof irreducible never SUPPRESSES the count advisory.
func TestRunPlanWarnings_IrreducibleDoesNotSuppressOverCapAdvisory(t *testing.T) {
	const capLimit = 2
	s, au, runRow := newScopePrecheckServer(t, planWarningsCapSpec)
	body := overCapIrreducibleBody(t, 3, "compile-atomic: receiver base type must live in its own package", "Go receiver rule")

	got := s.runPlanWarnings(context.Background(), runRow.ID, uuid.New(), body)
	if got == nil {
		t.Fatal("want a non-nil result for an over-cap irreducible plan")
	}
	if len(got.Warnings) < 2 {
		t.Fatalf("want at least 2 warnings (count-derived + irreducible), got %v", got.Warnings)
	}
	// The count-derived over-cap advisory must be present AND emitted FIRST.
	if !hasOverCapWarning(got.Warnings[:1], 3, capLimit) {
		t.Errorf("count-derived over-cap advisory must be first; warnings = %v", got.Warnings)
	}
	// The irreducible advisory must also be present, surfacing the rationale as a
	// challengeable claim and stating the declaration does not make it landable.
	foundIrr := false
	for _, w := range got.Warnings {
		if strings.Contains(w, "IRREDUCIBLE") && strings.Contains(w, "does NOT make the change landable") {
			foundIrr = true
		}
	}
	if !foundIrr {
		t.Errorf("want an irreducible advisory naming the non-landability; warnings = %v", got.Warnings)
	}
	// The recorded entry mirrors the returned warnings.
	entries := planWarningsEntries(t, au)
	if len(entries) != 1 {
		t.Fatalf("plan_warnings entries = %d, want 1", len(entries))
	}
}

// TestRunPlanWarnings_IrreducibleUnderCapIsNoOp pins the per-failure-mode branch
// that an UNDER-cap plan carrying irreducible produces NO advisory and no
// behaviour change (#2412) — the #2055 under-cap-unaffected guarantee. The
// irreducible field is never read for an under-cap plan (the count decides
// over/under first).
func TestRunPlanWarnings_IrreducibleUnderCapIsNoOp(t *testing.T) {
	s, au, runRow := newScopePrecheckServer(t, planWarningsCapSpec)
	body := overCapIrreducibleBody(t, 1, "compile-atomic", "Go receiver rule") // 1 file, cap 2 → under cap

	got := s.runPlanWarnings(context.Background(), runRow.ID, uuid.New(), body)
	if got != nil {
		t.Fatalf("want nil result for an under-cap irreducible plan; got %+v", got)
	}
	if len(planWarningsEntries(t, au)) != 0 {
		t.Error("want no plan_warnings entry for an under-cap irreducible plan")
	}
}

// splitProposalMapWithPhaseFiles builds a two-phase split_proposal whose first
// phase declares phase0Files files and second declares phase1Files files.
func splitProposalMapWithPhaseFiles(phase0Files, phase1Files int) map[string]any {
	mkFiles := func(prefix string, n int) []any {
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{"path": fmt.Sprintf("%s/f%d.go", prefix, i), "operation": "modify"})
		}
		return out
	}
	return map[string]any{
		"rationale": "split expand->contract",
		"phases": []any{
			map[string]any{"title": "Expand", "scope": map[string]any{"files": mkFiles("backend/internal/exp", phase0Files)}},
			map[string]any{"title": "Contract", "depends_on": []any{0}, "scope": map[string]any{"files": mkFiles("backend/internal/con", phase1Files)}},
		},
	}
}

// TestRunPlanWarnings_PhaseCapAdvisoryPerViolatingPhase pins the phaseCapWarnings
// leg (#2412): a split_proposal with two over-cap phases emits ONE advisory per
// violating phase naming the phase index, title, declared count, and cap. The
// top-level scope is kept UNDER cap so the count-derived over-cap advisory does
// not fire — isolating the phase-cap advisory.
func TestRunPlanWarnings_PhaseCapAdvisoryPerViolatingPhase(t *testing.T) {
	const capLimit = 2
	s, _, runRow := newScopePrecheckServer(t, planWarningsCapSpec)
	m := planfixture.Valid(func(p map[string]any) {
		p["scope"] = map[string]any{"files": []any{
			map[string]any{"path": "backend/internal/top/only.go", "operation": "modify"},
		}}
	})
	// Both phases over cap (3 files each > cap 2).
	m["split_proposal"] = splitProposalMapWithPhaseFiles(3, 3)
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := plan.Validate(body); err != nil {
		t.Fatalf("fixture does not validate: %v", err)
	}

	got := s.runPlanWarnings(context.Background(), runRow.ID, uuid.New(), body)
	if got == nil {
		t.Fatal("want a non-nil result with phase-cap advisories")
	}
	// No count-derived over-cap advisory (top-level scope is 1 file, under cap 2).
	if hasOverCapWarning(got.Warnings, 1, capLimit) {
		t.Errorf("count-derived over-cap advisory should not fire for an under-cap top-level scope; warnings = %v", got.Warnings)
	}
	phaseAdvisories := 0
	for _, w := range got.Warnings {
		if strings.Contains(w, "split_proposal phase") && strings.Contains(w, "cap of 2") {
			phaseAdvisories++
		}
	}
	if phaseAdvisories != 2 {
		t.Errorf("want one phase-cap advisory per over-cap phase (2), got %d; warnings = %v", phaseAdvisories, got.Warnings)
	}
}

// TestRunPlanWarnings_PhaseCap_UnresolvedCapNoAdvisory pins the phaseCapWarnings
// fail-open: with no implement stage (unresolved cap) a split_proposal with an
// over-cap phase emits NO phase-cap advisory.
func TestRunPlanWarnings_PhaseCap_UnresolvedCapNoAdvisory(t *testing.T) {
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
	s, _, runRow := newScopePrecheckServer(t, specNoImplement)
	m := planfixture.Valid()
	m["split_proposal"] = splitProposalMapWithPhaseFiles(3, 3)
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := s.runPlanWarnings(context.Background(), runRow.ID, uuid.New(), body)
	if got != nil {
		for _, w := range got.Warnings {
			if strings.Contains(w, "split_proposal phase") {
				t.Errorf("want no phase-cap advisory when the cap is unresolved; got %q", w)
			}
		}
	}
}
