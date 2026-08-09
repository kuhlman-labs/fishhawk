package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// --- the cost gate: every branch ---

func TestShouldFetchChildrenStatus(t *testing.T) {
	parentRef := uuid.NewString()
	cases := []struct {
		name   string
		run    *Run
		stages []Stage
		recent []AuditEntry
		want   bool
	}{
		{"nil run", nil, nil, nil, false},
		{
			"child run (has parent_run_id) never fetches",
			&Run{ID: uuid.NewString(), ParentRunID: &parentRef},
			[]Stage{{Type: "implement", State: "awaiting_children"}},
			nil, false,
		},
		{
			"awaiting_children implement stage fires the gate",
			&Run{ID: uuid.NewString()},
			[]Stage{{Type: "implement", State: "awaiting_children"}},
			nil, true,
		},
		{
			"plan_decomposed marker in recent fires the gate",
			&Run{ID: uuid.NewString()},
			[]Stage{{Type: "implement", State: "succeeded"}},
			[]AuditEntry{{Category: "plan_decomposed"}}, true,
		},
		{
			"slices_integrated marker fires the gate",
			&Run{ID: uuid.NewString()},
			[]Stage{{Type: "implement", State: "succeeded"}},
			[]AuditEntry{{Category: "slices_integrated"}}, true,
		},
		{
			"slice_integration_conflict marker fires the gate",
			&Run{ID: uuid.NewString()},
			[]Stage{{Type: "implement", State: "succeeded"}},
			[]AuditEntry{{Category: "slice_integration_conflict"}}, true,
		},
		{
			"ordinary run: no awaiting_children, no marker",
			&Run{ID: uuid.NewString()},
			[]Stage{{Type: "implement", State: "running"}},
			[]AuditEntry{{Category: "plan_generated"}}, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldFetchChildrenStatus(c.run, c.stages, c.recent); got != c.want {
				t.Errorf("shouldFetchChildrenStatus = %v, want %v", got, c.want)
			}
		})
	}
}

// TestChildrenStatusFor_PlanDecomposedDecodeError asserts the one non-best-effort
// path: a corrupt plan_decomposed payload surfaces as an error from
// childrenStatusFor (which getRunStatus swallows into a nil block — see
// TestGetRunStatus_ChildrenStatus_DecodeError_StillSnapshots).
func TestChildrenStatusFor_PlanDecomposedDecodeError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	parent := uuid.New()
	// A plan_decomposed entry with a nil payload → LatestPlanDecomposed errors.
	fb.mu.Lock()
	fb.perRunAuditByRun[parent] = []AuditEntry{{
		ID: uuid.NewString(), Sequence: 1, RunID: parent.String(), Category: "plan_decomposed",
	}}
	fb.mu.Unlock()

	if _, err := r.childrenStatusFor(context.Background(), parent, nil); err == nil {
		t.Fatal("expected an error for a corrupt plan_decomposed payload")
	}
}

// --- pure classifier: one behavioral assertion per phase ---

func TestClassifyIntegrationPhase(t *testing.T) {
	succeeded := []ChildStatus{{State: "succeeded"}, {State: "succeeded"}}
	inFlight := []ChildStatus{{State: "succeeded"}, {State: "running"}}

	// integratedSeq / conflictSeq are the highest audit Sequence of each fan-in
	// kind, or -1 when absent. The both-present cases assert the ORDERING
	// semantics (the relative sequences decide), not mere presence.
	const absent = int64(-1)
	cases := []struct {
		name          string
		children      []ChildStatus
		integratedSeq int64
		conflictSeq   int64
		want          string
	}{
		{"a child still in flight, no fan-in", inFlight, absent, absent, integrationPhaseRunningChildren},
		{"all succeeded, no fan-in audit yet", succeeded, absent, absent, integrationPhaseReadyToIntegrate},
		{"slices_integrated present", succeeded, 10, absent, integrationPhaseIntegrated},
		{"slice_integration_conflict present", succeeded, absent, 10, integrationPhaseConflict},
		{"conflict superseded by a later clean integration", succeeded, 11, 7, integrationPhaseIntegrated},
		{"older integration masked by a NEWER conflict stays conflict", succeeded, 7, 11, integrationPhaseConflict},
		{"no children at all classifies running_children", nil, absent, absent, integrationPhaseRunningChildren},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyIntegrationPhase(c.children, c.integratedSeq, c.conflictSeq); got != c.want {
				t.Errorf("classifyIntegrationPhase(%+v, integratedSeq=%d, conflictSeq=%d) = %q, want %q",
					c.children, c.integratedSeq, c.conflictSeq, got, c.want)
			}
		})
	}
}

// seedSlicesIntegrated / seedSliceConflict build the fan-in audit entries the
// resolver scans in its recent-audit window. They return the entry rather than
// seeding the backend because childrenStatusFor takes recentAudit directly.
func slicesIntegratedEntry(parent uuid.UUID, seq int64, consolidatedBranch string, childIDs []string) AuditEntry {
	return AuditEntry{
		ID:       uuid.NewString(),
		Sequence: seq,
		RunID:    parent.String(),
		Category: "slices_integrated",
		Payload: map[string]any{
			"child_run_ids":       childIDs,
			"consolidated_branch": consolidatedBranch,
			"slice_count":         len(childIDs),
		},
	}
}

func sliceConflictEntry(parent uuid.UUID, seq int64, conflictingChild string, sliceIndex int) AuditEntry {
	return AuditEntry{
		ID:       uuid.NewString(),
		Sequence: seq,
		RunID:    parent.String(),
		Category: "slice_integration_conflict",
		Payload: map[string]any{
			"parent_stage_id":          uuid.NewString(),
			"conflicting_slice_index":  sliceIndex,
			"conflicting_child_run_id": conflictingChild,
		},
	}
}

// --- resolver: discovery -> per-child GetRun -> audit -> classification ---

func TestChildrenStatusFor_RunningChildren(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedChildRun(fb, c1, "running")
	childIDs := []string{c0.String(), c1.String()}
	seedPlanDecomposed(fb, parent, childIDs, 2)

	cs, err := r.childrenStatusFor(context.Background(), parent, nil)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs == nil {
		t.Fatal("expected a ChildrenStatus, got nil")
	}
	if cs.IntegrationPhase != integrationPhaseRunningChildren {
		t.Errorf("phase = %q, want running_children", cs.IntegrationPhase)
	}
	if cs.Total != 2 || cs.Succeeded != 1 || cs.Running != 1 {
		t.Errorf("counts: total=%d succeeded=%d running=%d, want 2/1/1", cs.Total, cs.Succeeded, cs.Running)
	}
	// SliceIndex maps to position in child_run_ids (the run_children ordering).
	if cs.Children[0].RunID != c0.String() || cs.Children[0].SliceIndex != 0 {
		t.Errorf("child[0] = %+v, want run_id=%s slice_index=0", cs.Children[0], c0)
	}
	if cs.Children[1].RunID != c1.String() || cs.Children[1].SliceIndex != 1 {
		t.Errorf("child[1] = %+v, want run_id=%s slice_index=1", cs.Children[1], c1)
	}
}

func TestChildrenStatusFor_ReadyToIntegrate(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedChildRun(fb, c1, "succeeded")
	seedPlanDecomposed(fb, parent, []string{c0.String(), c1.String()}, 2)

	// No fan-in audit yet: every child succeeded but integration hasn't run.
	cs, err := r.childrenStatusFor(context.Background(), parent, nil)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs.IntegrationPhase != integrationPhaseReadyToIntegrate {
		t.Errorf("phase = %q, want ready_to_integrate", cs.IntegrationPhase)
	}
	if cs.Succeeded != 2 {
		t.Errorf("succeeded = %d, want 2", cs.Succeeded)
	}
}

func TestChildrenStatusFor_Integrated(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedChildRun(fb, c1, "succeeded")
	childIDs := []string{c0.String(), c1.String()}
	seedPlanDecomposed(fb, parent, childIDs, 2)

	// The slices_integrated marker fires the integrated phase even though the
	// parent implement stage is no longer awaiting_children (the gate in
	// getRunStatus also keys on the audit marker for exactly this reason).
	recent := []AuditEntry{slicesIntegratedEntry(parent, 5, "fishhawk/consolidated-x", childIDs)}
	cs, err := r.childrenStatusFor(context.Background(), parent, recent)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs.IntegrationPhase != integrationPhaseIntegrated {
		t.Errorf("phase = %q, want integrated", cs.IntegrationPhase)
	}
	if cs.ConsolidatedBranch != "fishhawk/consolidated-x" {
		t.Errorf("consolidated_branch = %q, want fishhawk/consolidated-x", cs.ConsolidatedBranch)
	}
}

// TestChildrenStatusFor_ConflictNewerThanIntegration is the non-vacuous
// ordering assertion the reviewer asked for: BOTH fan-in kinds are present in
// the recent-audit window, but the slice_integration_conflict has a higher
// Sequence than the slices_integrated, so the latest outcome is a conflict and
// the phase must stay integration_conflict — an older clean integration must
// not mask it. The entries are presented time-descending (item 0 newest) to
// match the real recent_audit ordering.
func TestChildrenStatusFor_ConflictNewerThanIntegration(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedChildRun(fb, c1, "succeeded")
	childIDs := []string{c0.String(), c1.String()}
	seedPlanDecomposed(fb, parent, childIDs, 2)

	// Newer conflict (seq 9) over an older clean integration (seq 4).
	recent := []AuditEntry{
		sliceConflictEntry(parent, 9, c1.String(), 1),
		slicesIntegratedEntry(parent, 4, "fishhawk/consolidated-x", childIDs),
	}
	cs, err := r.childrenStatusFor(context.Background(), parent, recent)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs.IntegrationPhase != integrationPhaseConflict {
		t.Errorf("phase = %q, want integration_conflict (newer conflict masks older integration)", cs.IntegrationPhase)
	}
	if cs.ConflictingChildRunID != c1.String() {
		t.Errorf("conflicting_child_run_id = %q, want %s", cs.ConflictingChildRunID, c1)
	}
}

// TestChildrenStatusFor_IntegrationNewerThanConflict is the mirror case: an
// earlier conflict was superseded by a later clean re-integration (higher
// Sequence), so the phase resolves to integrated.
func TestChildrenStatusFor_IntegrationNewerThanConflict(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedChildRun(fb, c1, "succeeded")
	childIDs := []string{c0.String(), c1.String()}
	seedPlanDecomposed(fb, parent, childIDs, 2)

	recent := []AuditEntry{
		slicesIntegratedEntry(parent, 12, "fishhawk/consolidated-y", childIDs),
		sliceConflictEntry(parent, 6, c1.String(), 1),
	}
	cs, err := r.childrenStatusFor(context.Background(), parent, recent)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs.IntegrationPhase != integrationPhaseIntegrated {
		t.Errorf("phase = %q, want integrated (later clean integration supersedes the conflict)", cs.IntegrationPhase)
	}
	if cs.ConsolidatedBranch != "fishhawk/consolidated-y" {
		t.Errorf("consolidated_branch = %q, want fishhawk/consolidated-y", cs.ConsolidatedBranch)
	}
}

func TestChildrenStatusFor_IntegrationConflict(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedChildRun(fb, c1, "succeeded")
	seedPlanDecomposed(fb, parent, []string{c0.String(), c1.String()}, 2)

	recent := []AuditEntry{sliceConflictEntry(parent, 5, c1.String(), 1)}
	cs, err := r.childrenStatusFor(context.Background(), parent, recent)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs.IntegrationPhase != integrationPhaseConflict {
		t.Errorf("phase = %q, want integration_conflict", cs.IntegrationPhase)
	}
	if cs.ConflictingChildRunID != c1.String() {
		t.Errorf("conflicting_child_run_id = %q, want %s", cs.ConflictingChildRunID, c1)
	}
}

// TestChildrenStatusFor_CrossLayer_RealHandlerToBlockedChild is the operator's
// binding condition 1: drive the REAL server handler response into the REAL MCP
// client decode and on into childrenStatusFor, asserting a blocked child is
// reported blocked with its blocker named. Unlike the fake-backed unit tests
// above (which decode a hand-authored body this test suite wrote itself), this
// spans the whole path against the shared testcontainers Postgres — the real
// toRunResponse emits slice_depends_on from a real plan artifact, the real MCP
// Run mirror decodes it (byte-matching json tags — the #371 trap this seals),
// and childrenStatusFor computes blocked/blocked_by from it. A json-tag drift on
// either side would decode slice_depends_on to nil and this test would report
// the blocked child as dispatchable.
func TestChildrenStatusFor_CrossLayer_RealHandlerToBlockedChild(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := runpkg.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	srv := server.New(server.Config{RunRepo: runRepo, ArtifactRepo: artRepo, AuditRepo: auditRepo})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	r := newResolver(ts, nil)

	newRun := func(sliceIdx *int, parent *uuid.UUID) *runpkg.Run {
		t.Helper()
		row, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
			TriggerSource: runpkg.TriggerCLI, DecomposedFrom: parent, ParentRunID: parent, SliceIndex: sliceIdx,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		return row
	}

	// Parent + plan stage + a decomposition plan whose slice 1 depends on slice 0.
	parent := newRun(nil, nil)
	planStage, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: parent.ID, Sequence: 0, Type: runpkg.StageTypePlan,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create plan stage: %v", err)
	}
	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "s",
		Verification: plan.Verification{TestStrategy: "t", RollbackPlan: "r"},
		Decomposition: &plan.Decomposition{Rationale: "split", SubPlans: []plan.SubPlanSummary{
			{Title: "A", ScopeHint: "a"},
			{Title: "B", ScopeHint: "b", DependsOn: []int{0}},
		}},
	}
	pb, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: pb,
	}); err != nil {
		t.Fatalf("create plan artifact: %v", err)
	}

	i0, i1 := 0, 1
	child0 := newRun(&i0, &parent.ID) // slice 0, still pending (not succeeded)
	child1 := newRun(&i1, &parent.ID) // slice 1, depends on slice 0

	pdPayload, err := json.Marshal(map[string]any{
		"child_run_ids":          []string{child0.ID.String(), child1.ID.String()},
		"effective_max_parallel": 0,
	})
	if err != nil {
		t.Fatalf("marshal plan_decomposed: %v", err)
	}
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: parent.ID, Timestamp: time.Now().UTC(), Category: "plan_decomposed", Payload: pdPayload,
	}); err != nil {
		t.Fatalf("append plan_decomposed: %v", err)
	}

	cs, err := r.childrenStatusFor(ctx, parent.ID, nil)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs == nil || len(cs.Children) != 2 {
		t.Fatalf("children = %+v, want 2", cs)
	}
	// child1 (slice 1) must be reported blocked, named by child0 (slice 0) —
	// proving slice_depends_on survived the real handler → real decode round trip.
	if !cs.Children[1].Blocked {
		t.Errorf("child[1] Blocked = false, want true (real slice_depends_on decode failed?)")
	}
	if len(cs.Children[1].BlockedBy) != 1 || cs.Children[1].BlockedBy[0] != child0.ID.String() {
		t.Errorf("child[1].BlockedBy = %v, want [%s]", cs.Children[1].BlockedBy, child0.ID)
	}
	if len(cs.Children[1].DependsOn) != 1 || cs.Children[1].DependsOn[0] != 0 {
		t.Errorf("child[1].DependsOn = %v, want [0] (decoded from the real handler's slice_depends_on)", cs.Children[1].DependsOn)
	}
	if cs.Children[0].Blocked {
		t.Errorf("child[0] Blocked = true, want false (wave-0 slice has no dependencies)")
	}
}

// seedChildRunDeps seeds a child run row carrying slice_depends_on (the field
// GET /v0/runs/{id} surfaces on the single-run read, E48.99 / #2546) so the
// blocked/blocked_by computation is exercised.
func seedChildRunDeps(fb *fakeBackend, childID uuid.UUID, state string, deps []int) {
	stageID := uuid.New()
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.getRunByID[childID] = Run{ID: childID.String(), State: state, Repo: "x/y", SliceDependsOn: deps}
	fb.stagesByRun[childID] = []Stage{{ID: stageID.String(), RunID: childID.String(), Type: "implement", State: state}}
}

// TestChildrenStatusFor_BlockedByUnsucceededDependency is the acceptance
// criterion for the children-view deliverable (#2546, operator condition 2): a
// child whose dependency slice has NOT succeeded is reported Blocked with
// BlockedBy naming the blocking sibling, and a child whose dependencies have all
// succeeded is NOT blocked.
func TestChildrenStatusFor_BlockedByUnsucceededDependency(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "running")               // dependency sibling, not succeeded
	seedChildRunDeps(fb, c1, "pending", []int{0}) // slice 1 depends on slice 0
	seedPlanDecomposed(fb, parent, []string{c0.String(), c1.String()}, 2)

	cs, err := r.childrenStatusFor(context.Background(), parent, nil)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs.Children[0].Blocked {
		t.Errorf("child[0] (the dependency itself) Blocked = true, want false")
	}
	if !cs.Children[1].Blocked {
		t.Errorf("child[1] Blocked = false, want true (its dependency slice 0 is still running)")
	}
	if len(cs.Children[1].BlockedBy) != 1 || cs.Children[1].BlockedBy[0] != c0.String() {
		t.Errorf("child[1].BlockedBy = %v, want [%s]", cs.Children[1].BlockedBy, c0)
	}
}

// TestChildrenStatusFor_AllDependenciesSucceeded_NotBlocked is the other half of
// the acceptance criterion: once every dependency slice has succeeded, the child
// is no longer blocked (dispatchable).
func TestChildrenStatusFor_AllDependenciesSucceeded_NotBlocked(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedChildRunDeps(fb, c1, "pending", []int{0})
	seedPlanDecomposed(fb, parent, []string{c0.String(), c1.String()}, 2)

	cs, err := r.childrenStatusFor(context.Background(), parent, nil)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs.Children[1].Blocked {
		t.Errorf("child[1] Blocked = true, want false (its dependency has succeeded)")
	}
	if len(cs.Children[1].BlockedBy) != 0 {
		t.Errorf("child[1].BlockedBy = %v, want empty", cs.Children[1].BlockedBy)
	}
}

// TestChildrenStatusFor_UnknownDependencyStateBlocks: a dependency whose
// per-child read failed (State "unknown") counts as blocking, never as
// dispatchable — the best-effort posture must not report a false all-clear.
func TestChildrenStatusFor_UnknownDependencyStateBlocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	// c0 is never seeded and its GetRun 404s → State "unknown".
	fb.mu.Lock()
	fb.getStatusByID[c0] = 404
	fb.mu.Unlock()
	seedChildRunDeps(fb, c1, "pending", []int{0})
	seedPlanDecomposed(fb, parent, []string{c0.String(), c1.String()}, 2)

	cs, err := r.childrenStatusFor(context.Background(), parent, nil)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs.Children[0].State != "unknown" {
		t.Fatalf("child[0] state = %q, want unknown (precondition)", cs.Children[0].State)
	}
	if !cs.Children[1].Blocked || len(cs.Children[1].BlockedBy) != 1 || cs.Children[1].BlockedBy[0] != c0.String() {
		t.Errorf("child[1] blocked=%v blockedBy=%v, want blocked by the unknown-state sibling %s",
			cs.Children[1].Blocked, cs.Children[1].BlockedBy, c0)
	}
}

// TestChildrenStatusFor_NotMintedDependencyBlocks (concern #2546 fix-up): a
// dependency slice with NO minted sibling in child_run_ids (slice index out of
// range) must render the child BLOCKED, not dispatchable — the read-side mirror
// of the host-dispatch guard's not_minted refusal. The pre-fix second pass took
// a `continue` for the out-of-range index and left Blocked=false, advertising a
// dispatch the backend would 409 dependency_not_satisfied. The not-minted slice
// is named by the synthetic "slice N (not_minted)" marker (no run id exists).
func TestChildrenStatusFor_NotMintedDependencyBlocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	// slice 1 depends on slice 2, which is NEVER minted (child_run_ids has only
	// two entries, indices 0 and 1) — the read/write-inconsistency case.
	seedChildRunDeps(fb, c1, "pending", []int{2})
	seedPlanDecomposed(fb, parent, []string{c0.String(), c1.String()}, 2)

	cs, err := r.childrenStatusFor(context.Background(), parent, nil)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if !cs.Children[1].Blocked {
		t.Errorf("child[1] Blocked = false, want true (its dependency slice 2 was never minted)")
	}
	if len(cs.Children[1].BlockedBy) != 1 || cs.Children[1].BlockedBy[0] != "slice 2 (not_minted)" {
		t.Errorf("child[1].BlockedBy = %v, want [\"slice 2 (not_minted)\"]", cs.Children[1].BlockedBy)
	}
	if cs.Children[0].Blocked {
		t.Errorf("child[0] Blocked = true, want false (wave-0 slice has no dependencies)")
	}
}

// TestChildrenStatusFor_LegacyNoDependsOn: a legacy backend that omits
// slice_depends_on decodes to nil DependsOn, so Blocked is false and the block
// renders exactly as it did before this field existed (back-compat).
func TestChildrenStatusFor_LegacyNoDependsOn(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "running")
	seedChildRun(fb, c1, "running") // no SliceDependsOn set → nil
	seedPlanDecomposed(fb, parent, []string{c0.String(), c1.String()}, 2)

	cs, err := r.childrenStatusFor(context.Background(), parent, nil)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	for i := range cs.Children {
		if cs.Children[i].Blocked || len(cs.Children[i].DependsOn) != 0 {
			t.Errorf("child[%d] = %+v, want no DependsOn and Blocked=false (legacy degrade)", i, cs.Children[i])
		}
	}
}

func TestChildrenStatusFor_NotDecomposed_ReturnsNil(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	// No plan_decomposed audit entry on this run → not a decomposed parent.
	cs, err := r.childrenStatusFor(context.Background(), uuid.New(), nil)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs != nil {
		t.Errorf("expected nil ChildrenStatus for a non-decomposed run, got %+v", cs)
	}
}

// TestChildrenStatusFor_BestEffortChildFailure: a per-child GetRun 404 yields
// State="unknown" for that child while the snapshot still returns (no error).
func TestChildrenStatusFor_BestEffortChildFailure(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedChildRun(fb, c1, "running")
	// Fail c1's GetRun specifically.
	fb.mu.Lock()
	fb.getStatusByID[c1] = 404
	fb.mu.Unlock()
	seedPlanDecomposed(fb, parent, []string{c0.String(), c1.String()}, 2)

	cs, err := r.childrenStatusFor(context.Background(), parent, nil)
	if err != nil {
		t.Fatalf("childrenStatusFor must not fail on a per-child GetRun error: %v", err)
	}
	if cs == nil {
		t.Fatal("expected a ChildrenStatus despite the child failure")
	}
	if cs.Children[1].State != "unknown" {
		t.Errorf("failed child state = %q, want unknown", cs.Children[1].State)
	}
	// The unknown child is in flight from the classifier's view → running_children.
	if cs.IntegrationPhase != integrationPhaseRunningChildren {
		t.Errorf("phase = %q, want running_children (unknown child is not succeeded)", cs.IntegrationPhase)
	}
	if cs.Succeeded != 1 {
		t.Errorf("succeeded = %d, want 1 (the unknown child is not counted)", cs.Succeeded)
	}
}

// TestChildrenStatusFor_UnparseableChildID exercises the uuid.Parse guard: a
// non-UUID child id degrades to State="unknown" without failing the snapshot.
func TestChildrenStatusFor_UnparseableChildID(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	parent := uuid.New()
	c0 := uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedPlanDecomposed(fb, parent, []string{c0.String(), "not-a-uuid"}, 2)

	cs, err := r.childrenStatusFor(context.Background(), parent, nil)
	if err != nil {
		t.Fatalf("childrenStatusFor: %v", err)
	}
	if cs.Children[1].State != "unknown" {
		t.Errorf("unparseable child state = %q, want unknown", cs.Children[1].State)
	}
}
