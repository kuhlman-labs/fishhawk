package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// guardCountingRepo wraps orchestratorRepo to (a) count ListRuns /
// ListStagesForRun so the inert fast-path branches can pin "no read happened",
// (b) inject a ListRuns error so the sibling-walk fail-closed branch is
// reachable (orchestratorRepo has no error hook of its own), and (c) fire a
// mutation hook INSIDE the guard's check-then-act window (#2586).
type guardCountingRepo struct {
	*orchestratorRepo
	listRunsCalls   int
	listStagesCalls int
	listRunsErr     error

	// onListRuns fires AFTER the underlying ListRuns returns and BEFORE the
	// guard consumes the snapshot — exactly the interval named by #2586 (the
	// guard's sibling snapshot is not atomic with the caller's stage CAS). It
	// makes the window's interleavings deterministic: a test mutates sibling
	// state here with no sleeps, no goroutines, and no timing dependence.
	// The seam is a TEST-ONLY wrapper around a call the guard already makes;
	// production code is untouched, which is why the pgtest-backed cross-layer
	// test is kept as the no-hook corroboration.
	onListRuns func()
}

func (r *guardCountingRepo) ListRuns(ctx context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.listRunsCalls++
	if r.listRunsErr != nil {
		return nil, r.listRunsErr
	}
	rows, err := r.orchestratorRepo.ListRuns(ctx, f)
	// SNAPSHOT FIDELITY (load-bearing for the window tests): orchestratorRepo
	// returns pointers INTO its own store, but postgresRepo.ListRuns
	// materializes fresh rows — a true point-in-time value snapshot. Copying
	// here is what makes the fake model the real window: without it, a mutation
	// fired by onListRuns would be visible to the guard through the shared
	// pointer, i.e. the exact OPPOSITE of the production behaviour under test.
	out := make([]*run.Run, len(rows))
	for i, row := range rows {
		cp := *row
		out[i] = &cp
	}
	if r.onListRuns != nil {
		r.onListRuns()
	}
	return out, err
}

func (r *guardCountingRepo) ListStagesForRun(ctx context.Context, id uuid.UUID) ([]*run.Stage, error) {
	r.listStagesCalls++
	return r.orchestratorRepo.ListStagesForRun(ctx, id)
}

// newGuardServer wires a server with the counting run repo + a fake artifact
// repo so loadApprovedPlanForRun can resolve a seeded parent plan.
func newGuardServer(t *testing.T) (*Server, *guardCountingRepo, *fakeArtifactRepo) {
	t.Helper()
	rr := &guardCountingRepo{orchestratorRepo: newOrchestratorRepo()}
	art := newFakeArtifactRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: art})
	return s, rr, art
}

// seedParentDecompPlan seeds a parent run with a plan stage carrying a
// standard_v1 plan whose decomposition sub_plans have the given depends_on
// edges (one entry per sub_plan). Returns the parent run id.
func seedParentDecompPlan(t *testing.T, rr *guardCountingRepo, art *fakeArtifactRepo, subDeps [][]int) uuid.UUID {
	t.Helper()
	parent := rr.seedRun()
	planStage := rr.seedStage(parent.ID, 0, run.StageStateSucceeded) // Type=plan
	subs := make([]plan.SubPlanSummary, len(subDeps))
	for i, deps := range subDeps {
		subs[i] = plan.SubPlanSummary{Title: fmt.Sprintf("slice %d", i), ScopeHint: "x", DependsOn: deps}
	}
	p := &plan.Plan{
		PlanVersion:   "standard_v1",
		Summary:       "s",
		Verification:  plan.Verification{TestStrategy: "t", RollbackPlan: "r"},
		Decomposition: &plan.Decomposition{Rationale: "split", SubPlans: subs},
	}
	seedPlanArtifactContent(t, art, planStage.ID, p)
	return parent.ID
}

// seedParentPlanNoDecomp seeds a parent whose plan carries NO decomposition.
func seedParentPlanNoDecomp(t *testing.T, rr *guardCountingRepo, art *fakeArtifactRepo) uuid.UUID {
	t.Helper()
	parent := rr.seedRun()
	planStage := rr.seedStage(parent.ID, 0, run.StageStateSucceeded)
	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "s",
		Verification: plan.Verification{TestStrategy: "t", RollbackPlan: "r"},
	}
	seedPlanArtifactContent(t, art, planStage.ID, p)
	return parent.ID
}

func seedPlanArtifactContent(t *testing.T, art *fakeArtifactRepo, planStageID uuid.UUID, p *plan.Plan) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: b,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
}

// seedGuardChild seeds a decomposed child of parentID at the given slice index
// and run state, returning it for mutation.
func seedGuardChild(rr *guardCountingRepo, parentID uuid.UUID, sliceIdx int, state run.State) *run.Run {
	c := rr.seedRun()
	pid := parentID
	idx := sliceIdx
	c.DecomposedFrom = &pid
	c.SliceIndex = &idx
	c.State = state
	return c
}

// --- ADMIT branches: data ABSENT is not a violation ---

// (1) run not decomposed: guard returns nil AND performs no plan / sibling read
// (the inert fast path — pinned via the call counts, not just the verdict).
func TestGuard_NotDecomposed_AdmitsNoReads(t *testing.T) {
	s, rr, _ := newGuardServer(t)
	child := rr.seedRun() // DecomposedFrom nil

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil || depErr != nil {
		t.Fatalf("guard = (%v, %v), want (nil, nil)", depErr, err)
	}
	if rr.listStagesCalls != 0 || rr.listRunsCalls != 0 {
		t.Errorf("reads happened on the inert path: listStages=%d listRuns=%d, want 0/0",
			rr.listStagesCalls, rr.listRunsCalls)
	}
}

// (2) DecomposedFrom set but SliceIndex nil: still admit, still no plan read.
func TestGuard_NilSliceIndex_AdmitsNoReads(t *testing.T) {
	s, rr, _ := newGuardServer(t)
	parent := rr.seedRun()
	child := rr.seedRun()
	child.DecomposedFrom = &parent.ID // SliceIndex left nil

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil || depErr != nil {
		t.Fatalf("guard = (%v, %v), want (nil, nil)", depErr, err)
	}
	if rr.listStagesCalls != 0 {
		t.Errorf("plan read happened for a nil-SliceIndex child: listStages=%d, want 0", rr.listStagesCalls)
	}
}

// (3) parent plan resolves nil (no plan stage / artifact): admit.
func TestGuard_ParentPlanNil_Admits(t *testing.T) {
	s, rr, _ := newGuardServer(t)
	parent := rr.seedRun() // no plan stage seeded → loadApprovedPlanForRun returns nil
	child := seedGuardChild(rr, parent.ID, 1, run.StatePending)

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil || depErr != nil {
		t.Fatalf("guard = (%v, %v), want (nil, nil)", depErr, err)
	}
}

// (4) plan carries no Decomposition: admit.
func TestGuard_PlanNoDecomposition_Admits(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentPlanNoDecomp(t, rr, art)
	child := seedGuardChild(rr, parentID, 0, run.StatePending)

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil || depErr != nil {
		t.Fatalf("guard = (%v, %v), want (nil, nil)", depErr, err)
	}
}

// (5) *SliceIndex out of range for sub_plans: admit (defensive degrade).
func TestGuard_SliceIndexOutOfRange_Admits(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	child := seedGuardChild(rr, parentID, 5, run.StatePending) // only slices 0,1 exist

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil || depErr != nil {
		t.Fatalf("guard = (%v, %v), want (nil, nil)", depErr, err)
	}
}

// (6) sub_plan declares an empty depends_on: admit.
func TestGuard_EmptyDependsOn_Admits(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	child := seedGuardChild(rr, parentID, 0, run.StatePending) // slice 0 has no deps

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil || depErr != nil {
		t.Fatalf("guard = (%v, %v), want (nil, nil)", depErr, err)
	}
}

// (7) every dependency sibling is state succeeded: admit.
func TestGuard_AllDependenciesSucceeded_Admits(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	seedGuardChild(rr, parentID, 0, run.StateSucceeded)
	child := seedGuardChild(rr, parentID, 1, run.StateRunning)

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil || depErr != nil {
		t.Fatalf("guard = (%v, %v), want (nil, nil)", depErr, err)
	}
}

// --- REFUSE branches: a positively-unmet dependency ---

// (8) a dependency sibling in state pending → refusal naming the blocker.
func TestGuard_DependencyPending_Refuses(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	sib := seedGuardChild(rr, parentID, 0, run.StatePending)
	child := seedGuardChild(rr, parentID, 1, run.StatePending)

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depErr == nil {
		t.Fatal("guard admitted a child whose dependency is pending, want refusal")
	}
	if depErr.sliceIndex != 1 || depErr.blockingSliceIndex != 0 {
		t.Errorf("coords = slice %d blocked_by %d, want 1/0", depErr.sliceIndex, depErr.blockingSliceIndex)
	}
	if depErr.blockingRunID != sib.ID.String() || depErr.blockingState != "pending" {
		t.Errorf("blocker = (%s, %s), want (%s, pending)", depErr.blockingRunID, depErr.blockingState, sib.ID)
	}
}

// (9) running / failed / cancelled dependency states each refuse — "not
// succeeded" must not be read as "not terminal".
func TestGuard_DependencyNotSucceededStates_Refuse(t *testing.T) {
	for _, st := range []run.State{run.StateRunning, run.StateFailed, run.StateCancelled} {
		t.Run(string(st), func(t *testing.T) {
			s, rr, art := newGuardServer(t)
			parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
			sib := seedGuardChild(rr, parentID, 0, st)
			child := seedGuardChild(rr, parentID, 1, run.StatePending)

			depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if depErr == nil {
				t.Fatalf("guard admitted a child whose dependency is %s, want refusal", st)
			}
			if depErr.blockingRunID != sib.ID.String() || depErr.blockingState != string(st) {
				t.Errorf("blocker = (%s, %s), want (%s, %s)", depErr.blockingRunID, depErr.blockingState, sib.ID, st)
			}
		})
	}
}

// (10) a dependency slice with NO minted sibling → refusal with blocking_state
// "not_minted" and an empty blocking_run_id. The step-5 filter is pinned here:
// an unrelated run carrying a matching slice_index but a DIFFERENT
// DecomposedFrom must NOT count as the sibling.
func TestGuard_DependencyNotMinted_Refuses(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	child := seedGuardChild(rr, parentID, 1, run.StatePending) // slice 0 never minted

	// A decoy: a succeeded run at slice 0 but under a DIFFERENT parent. If the
	// DecomposedFrom filter were dropped, this would satisfy the dependency and
	// the guard would wrongly admit.
	otherParent := uuid.New()
	seedGuardChild(rr, otherParent, 0, run.StateSucceeded)

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depErr == nil {
		t.Fatal("guard admitted a child whose dependency slice was never minted, want refusal")
	}
	if depErr.blockingSliceIndex != 0 || depErr.blockingRunID != "" || depErr.blockingState != "not_minted" {
		t.Errorf("blocker = (slice %d, run %q, state %q), want (0, \"\", not_minted)",
			depErr.blockingSliceIndex, depErr.blockingRunID, depErr.blockingState)
	}
}

// (11) two unmet dependencies → the LOWEST dependency slice index is named
// (deterministic blocker selection, independent of map iteration order).
func TestGuard_MultipleUnmet_NamesLowest(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, nil, {1, 0}})
	seedGuardChild(rr, parentID, 0, run.StatePending)
	seedGuardChild(rr, parentID, 1, run.StatePending)
	child := seedGuardChild(rr, parentID, 2, run.StatePending)

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depErr == nil {
		t.Fatal("guard admitted a child with two unmet dependencies, want refusal")
	}
	if depErr.blockingSliceIndex != 0 {
		t.Errorf("blocking_slice_index = %d, want 0 (lowest, deterministic)", depErr.blockingSliceIndex)
	}
}

// --- FAIL-CLOSED branches: a required read ERRORED (retryable) ---

// (12) the parent-plan load errors → guard returns an error (never a silent
// admit).
func TestGuard_PlanLoadError_FailsClosed(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	child := seedGuardChild(rr, parentID, 1, run.StatePending)
	art.listErr = errors.New("artifact store down")

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err == nil {
		t.Fatalf("guard = (%v, nil), want a non-nil error (fail closed on plan-load error)", depErr)
	}
	if depErr != nil {
		t.Errorf("depErr = %+v, want nil when the read errored", depErr)
	}
}

// (13) the sibling ListRuns errors → guard returns an error, not a refusal and
// not an admit.
func TestGuard_SiblingListError_FailsClosed(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	child := seedGuardChild(rr, parentID, 1, run.StatePending)
	rr.listRunsErr = errors.New("list runs down")

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err == nil {
		t.Fatalf("guard = (%v, nil), want a non-nil error (fail closed on sibling-list error)", depErr)
	}
	if depErr != nil {
		t.Errorf("depErr = %+v, want nil when the read errored", depErr)
	}
}

// --- WINDOW DIRECTIONS: the check-then-act interval (#2586) ---
//
// guardDecompositionWaveOrder's sibling snapshot is NOT atomic with the
// caller's stage CAS, and no lock spans the two rows. The three tests below
// enumerate every direction sibling state can drift inside that interval,
// using the deterministic onListRuns hook (no sleeps, no goroutines). Two are
// reachable in production and fail closed; the third is the one that would be
// dangerous, and is unreachable because run state `succeeded` is absorbing
// (pinned by run.TestRunSucceededIsAbsorbing and
// run.TestPostgres_SucceededRunNeverLeavesSucceeded).

// (18) WINDOW DIRECTION 1 — a dependency becomes SATISFIED inside the window.
// The guard decides on its (now stale) snapshot and still refuses. This is the
// fail-CLOSED direction: the cost is a spurious 409 the operator clears by
// re-dispatching, never an out-of-order spawn.
func TestGuard_Window_DependencyReachesSucceeded_StillRefuses(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	sib := seedGuardChild(rr, parentID, 0, run.StatePending)
	child := seedGuardChild(rr, parentID, 1, run.StatePending)

	// Fire the sibling's pending → succeeded transition INSIDE the window:
	// after ListRuns returned, before the guard consumes the snapshot.
	rr.onListRuns = func() { sib.State = run.StateSucceeded }

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depErr == nil {
		t.Fatal("guard admitted on a mid-window satisfaction; want a refusal decided on the snapshot (fail closed)")
	}
	if depErr.blockingSliceIndex != 0 || depErr.blockingState != string(run.StatePending) {
		t.Errorf("blocker = (slice %d, state %q), want (0, pending) — the snapshot's view, not the post-window one",
			depErr.blockingSliceIndex, depErr.blockingState)
	}
	// The store really did move on: the refusal is stale, not a mis-seeded fixture.
	if sib.State != run.StateSucceeded {
		t.Fatalf("test precondition: sibling state = %q, want the hook to have advanced it to succeeded", sib.State)
	}
}

// (19) WINDOW DIRECTION 2 — a dependency slice with no minted sibling gains one
// inside the window. Still refuses: a freshly minted run starts `pending`, so
// even a snapshot taken AFTER the mint would refuse. Fail closed again.
func TestGuard_Window_DependencySiblingMintedLate_StillRefuses(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	child := seedGuardChild(rr, parentID, 1, run.StatePending) // slice 0 not minted yet

	var late *run.Run
	rr.onListRuns = func() {
		if late == nil {
			late = seedGuardChild(rr, parentID, 0, run.StatePending)
		}
	}

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depErr == nil {
		t.Fatal("guard admitted on a mid-window mint; want a refusal decided on the snapshot")
	}
	if depErr.blockingSliceIndex != 0 || depErr.blockingState != notMintedState || depErr.blockingRunID != "" {
		t.Errorf("blocker = (slice %d, run %q, state %q), want (0, \"\", not_minted) — the snapshot's view",
			depErr.blockingSliceIndex, depErr.blockingRunID, depErr.blockingState)
	}
	if late == nil {
		t.Fatal("test precondition: the hook never minted the late sibling")
	}
}

// (20) WINDOW DIRECTION 3 — the DANGEROUS direction: a dependency LEAVES
// `succeeded` inside the window, so the guard admits on a snapshot that is no
// longer true and a slice dispatches out of wave order.
//
// This test asserts the admit, and that is deliberate: it records exactly what
// the guard WOULD do if the invariant were relaxed, rather than claiming an
// atomicity the code does not implement. In production this interleaving is
// UNREACHABLE — run state `succeeded` is absorbing (single UpdateRunState write
// site; both gated callers under SELECT ... FOR UPDATE; ReviveRun refuses a
// non-failed run; no run deletion), pinned by run.TestRunSucceededIsAbsorbing
// and run.TestPostgres_SucceededRunNeverLeavesSucceeded. If EITHER of those goes
// red, this test's admit becomes reachable and the guard needs real
// serialization across the two rows.
//
// The mutation is applied directly to the stored run rather than through
// TransitionRun precisely BECAUSE the repository refuses it — the test has to
// construct a state the production state machine will not produce.
func TestGuard_Window_DependencyLeavesSucceeded_AdmitsOnSnapshot(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	sib := seedGuardChild(rr, parentID, 0, run.StateSucceeded)
	child := seedGuardChild(rr, parentID, 1, run.StatePending)

	rr.onListRuns = func() { sib.State = run.StateFailed }

	depErr, err := s.guardDecompositionWaveOrder(context.Background(), child)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depErr != nil {
		t.Fatalf("guard = %+v; want an ADMIT decided on the snapshot. This test documents the one dangerous "+
			"window direction. It is unreachable in production because `succeeded` is absorbing — see "+
			"run.TestRunSucceededIsAbsorbing and run.TestPostgres_SucceededRunNeverLeavesSucceeded. A change here "+
			"means the guard's window analysis must be re-derived, not that this expectation should be updated",
			depErr)
	}
	if sib.State != run.StateFailed {
		t.Fatalf("test precondition: sibling state = %q, want the hook to have regressed it to failed", sib.State)
	}
}

// TestSliceDependencyError_Message pins the refusal message shapes: the
// minted-blocker form names the run + state, and the not-minted form says so.
func TestSliceDependencyError_Message(t *testing.T) {
	minted := &sliceDependencyError{sliceIndex: 1, blockingSliceIndex: 0, blockingRunID: "abc", blockingState: "pending"}
	if got := minted.message(); !strings.Contains(got, "slice 1") || !strings.Contains(got, "slice 0") ||
		!strings.Contains(got, "abc") || !strings.Contains(got, "pending") {
		t.Errorf("minted message = %q, want it to name slice 1, dependency slice 0, run abc, state pending", got)
	}
	notMinted := &sliceDependencyError{sliceIndex: 2, blockingSliceIndex: 1, blockingRunID: "", blockingState: notMintedState}
	if got := notMinted.message(); !strings.Contains(got, "no sibling run minted") {
		t.Errorf("not-minted message = %q, want it to say no sibling was minted", got)
	}
	d := minted.details()
	if d["slice_index"] != 1 || d["blocking_slice_index"] != 0 || d["blocking_run_id"] != "abc" || d["blocking_state"] != "pending" {
		t.Errorf("details = %+v, want the four structured keys", d)
	}
}

// --- resolveDependentChildBase: the per-wave re-base + integration guard (#2363) ---

// nthListRunsErrRepo fails the Nth ListRuns call (1-indexed) and delegates the
// rest. resolveDependentChildBase's sibling walk is the SECOND ListRuns on the
// host-dispatch path (guardDecompositionWaveOrder makes the first), so a repo
// that fails every call would redden on the guard's walk instead and never
// reach the branch under test.
type nthListRunsErrRepo struct {
	*guardCountingRepo
	failOnCall int
	calls      int
	err        error
}

func (r *nthListRunsErrRepo) ListRuns(ctx context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.calls++
	if r.calls == r.failOnCall {
		return nil, r.err
	}
	return r.guardCountingRepo.ListRuns(ctx, f)
}

// newBaseServer wires a server with the counting run repo, a fake artifact repo
// AND an audit fake, so resolveDependentChildBase can read the parent's
// slices_integrated history. newGuardServer deliberately wires no AuditRepo
// (the wave-ORDER guard needs none), which is why this has its own harness.
func newBaseServer(t *testing.T) (*Server, *guardCountingRepo, *fakeArtifactRepo, *auditCompleteAuditFake) {
	t.Helper()
	rr := &guardCountingRepo{orchestratorRepo: newOrchestratorRepo()}
	art := newFakeArtifactRepo()
	au := newAuditCompleteAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: art, AuditRepo: au})
	return s, rr, art, au
}

// (1) a run that is not a fan-out child admits with NO base — the caller keeps
// whatever base it had, byte-identical to pre-#2363 behaviour.
func TestResolveDependentChildBase_NotDecomposed_Admits(t *testing.T) {
	s, rr, _, _ := newBaseServer(t)
	child := rr.seedRun()

	base, waveErr, err := s.resolveDependentChildBase(context.Background(), child)
	if err != nil || waveErr != nil || base != "" {
		t.Fatalf("got (%q, %v, %v), want (\"\", nil, nil)", base, waveErr, err)
	}
}

// (2) a wave-0 child (empty depends_on) admits with NO base: there is nothing
// to integrate, so it spawns on the base it already had.
func TestResolveDependentChildBase_EmptyDependsOn_Admits(t *testing.T) {
	s, rr, art, au := newBaseServer(t)
	parent := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	child := seedGuardChild(rr, parent, 0, run.StateRunning)
	seedSlicesIntegrated(t, au, parent, "fishhawk/run-x-consolidated", nil)

	base, waveErr, err := s.resolveDependentChildBase(context.Background(), child)
	if err != nil || waveErr != nil || base != "" {
		t.Fatalf("got (%q, %v, %v), want (\"\", nil, nil) — a wave-0 child gets no derived base", base, waveErr, err)
	}
}

// (3) UNCONFIGURED audit reads as ABSENT, not as a violation: a deployment
// without an audit repository admits with no derived base rather than having
// every dependent dispatch wedged behind a 409 it can never clear. Same
// three-way partition resolveSliceDependencies documents.
func TestResolveDependentChildBase_NilAuditRepo_Admits(t *testing.T) {
	s, rr, art := newGuardServer(t) // no AuditRepo
	parent := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	seedGuardChild(rr, parent, 0, run.StateSucceeded)
	child := seedGuardChild(rr, parent, 1, run.StateRunning)

	base, waveErr, err := s.resolveDependentChildBase(context.Background(), child)
	if err != nil || waveErr != nil || base != "" {
		t.Fatalf("got (%q, %v, %v), want (\"\", nil, nil) — an unconfigured audit repo is ABSENT input", base, waveErr, err)
	}
}

// (4) a plan-LOAD error fails closed (retryable), never a silent admit.
func TestResolveDependentChildBase_PlanLoadError_FailsClosed(t *testing.T) {
	rr := &guardCountingRepo{orchestratorRepo: newOrchestratorRepo()}
	art := newFakeArtifactRepo()
	art.listErr = errors.New("artifact store down")
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: art, AuditRepo: newAuditCompleteAuditFake()})
	parent := rr.seedRun()
	rr.seedStage(parent.ID, 0, run.StageStateSucceeded)
	child := seedGuardChild(rr, parent.ID, 1, run.StateRunning)

	if _, _, err := s.resolveDependentChildBase(context.Background(), child); err == nil {
		t.Fatal("err = nil, want the plan-load error propagated (fail closed)")
	}
}

// (5) an audit LIST error fails closed — the refusal must never be decided on a
// read that did not happen.
func TestResolveDependentChildBase_AuditListError_FailsClosed(t *testing.T) {
	rr := &guardCountingRepo{orchestratorRepo: newOrchestratorRepo()}
	art := newFakeArtifactRepo()
	au := &slicesIntegratedErrAudit{auditCompleteAuditFake: newAuditCompleteAuditFake(), err: errors.New("audit read boom")}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: art, AuditRepo: au})
	parent := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	seedGuardChild(rr, parent, 0, run.StateSucceeded)
	child := seedGuardChild(rr, parent, 1, run.StateRunning)

	_, waveErr, err := s.resolveDependentChildBase(context.Background(), child)
	if err == nil {
		t.Fatal("err = nil, want the audit-read error propagated (fail closed)")
	}
	if waveErr != nil {
		t.Errorf("waveErr = %+v, want nil — an errored read is retryable, not a refusal", waveErr)
	}
}

// (6) a sibling-LIST error fails closed. The failure is injected on the SECOND
// ListRuns so the wave-order guard's own walk succeeds and execution genuinely
// reaches resolveDependentChildBase's walk.
func TestResolveDependentChildBase_SiblingListError_FailsClosed(t *testing.T) {
	inner := &guardCountingRepo{orchestratorRepo: newOrchestratorRepo()}
	rr := &nthListRunsErrRepo{guardCountingRepo: inner, failOnCall: 1, err: errors.New("list runs boom")}
	art := newFakeArtifactRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: art, AuditRepo: newAuditCompleteAuditFake()})
	parent := seedParentDecompPlan(t, inner, art, [][]int{nil, {0}})
	seedGuardChild(inner, parent, 0, run.StateSucceeded)
	child := seedGuardChild(inner, parent, 1, run.StateRunning)

	if _, _, err := s.resolveDependentChildBase(context.Background(), child); err == nil {
		t.Fatal("err = nil, want the sibling-list error propagated (fail closed)")
	}
}

// (7) an UNDECODABLE newest payload reads as ABSENT and REFUSES — it must not
// admit on a payload the server could not understand.
func TestResolveDependentChildBase_UndecodablePayload_Refuses(t *testing.T) {
	s, rr, art, au := newBaseServer(t)
	parent := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	seedGuardChild(rr, parent, 0, run.StateSucceeded)
	child := seedGuardChild(rr, parent, 1, run.StateRunning)
	au.appendChained(t, parent, nil, "slices_integrated", json.RawMessage(`"not-an-object"`))

	base, waveErr, err := s.resolveDependentChildBase(context.Background(), child)
	if err != nil {
		t.Fatalf("err = %v, want nil (an undecodable payload is ABSENT, not an errored read)", err)
	}
	if waveErr == nil || base != "" {
		t.Fatalf("got (%q, %v), want a refusal with no base", base, waveErr)
	}
	if waveErr.consolidatedBranchPresent {
		t.Error("consolidated_branch_present = true, want false on an undecodable payload")
	}
}

// (8) COVERED but with an EMPTY branch still refuses: the response's base_branch
// is what the child spawns against, so an empty one would silently degrade to
// the caller's default base — the #1302 stale-base class this guard exists for.
func TestResolveDependentChildBase_CoveredWithEmptyBranch_Refuses(t *testing.T) {
	s, rr, art, au := newBaseServer(t)
	parent := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	dep := seedGuardChild(rr, parent, 0, run.StateSucceeded)
	child := seedGuardChild(rr, parent, 1, run.StateRunning)
	seedSlicesIntegrated(t, au, parent, "", []string{dep.ID.String()})

	base, waveErr, err := s.resolveDependentChildBase(context.Background(), child)
	if err != nil || waveErr == nil || base != "" {
		t.Fatalf("got (%q, %v, %v), want a refusal with no base", base, waveErr, err)
	}
	if waveErr.consolidatedBranchPresent {
		t.Error("consolidated_branch_present = true, want false on an empty branch")
	}
	if len(waveErr.missing) != 0 {
		t.Errorf("missing = %v, want empty — coverage held; the branch is what was absent", waveErr.missing)
	}
}

// (9) the happy path: covered dependencies + a named branch admits WITH that
// branch as the derived base.
func TestResolveDependentChildBase_Covered_ReturnsConsolidatedBranch(t *testing.T) {
	s, rr, art, au := newBaseServer(t)
	parent := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	dep := seedGuardChild(rr, parent, 0, run.StateSucceeded)
	child := seedGuardChild(rr, parent, 1, run.StateRunning)
	seedSlicesIntegrated(t, au, parent, "fishhawk/run-abc-consolidated", []string{dep.ID.String()})

	base, waveErr, err := s.resolveDependentChildBase(context.Background(), child)
	if err != nil || waveErr != nil {
		t.Fatalf("got (%v, %v), want an admit", waveErr, err)
	}
	if base != "fishhawk/run-abc-consolidated" {
		t.Errorf("base = %q, want the consolidated branch from the newest entry", base)
	}
}

// (10) the branch and the coverage set must come from THE SAME entry. An older
// entry naming a branch plus a newer one covering the dependency (but naming no
// branch) must NOT be spliced into an admit — latestSlicesIntegrated reads one
// entry, which is the whole point of the single helper.
func TestResolveDependentChildBase_DoesNotSpliceAcrossEntries(t *testing.T) {
	s, rr, art, au := newBaseServer(t)
	parent := seedParentDecompPlan(t, rr, art, [][]int{nil, {0}})
	dep := seedGuardChild(rr, parent, 0, run.StateSucceeded)
	child := seedGuardChild(rr, parent, 1, run.StateRunning)
	seedSlicesIntegrated(t, au, parent, "fishhawk/run-old-consolidated", nil)
	seedSlicesIntegrated(t, au, parent, "", []string{dep.ID.String()})

	base, waveErr, err := s.resolveDependentChildBase(context.Background(), child)
	if err != nil || waveErr == nil || base != "" {
		t.Fatalf("got (%q, %v, %v), want a refusal — the older entry's branch must not be spliced onto the newer entry's coverage", base, waveErr, err)
	}
}

// TestWaveIntegrationError_MessageAndDetails pins both refusal shapes and the
// structured payload the 409 body carries.
func TestWaveIntegrationError_MessageAndDetails(t *testing.T) {
	none := &waveIntegrationError{sliceIndex: 2, dependsOn: []int{0, 1}, consolidatedBranchPresent: false}
	if got := none.message(); !strings.Contains(got, "no consolidated branch") || !strings.Contains(got, "slice 2") {
		t.Errorf("no-integration message = %q, want it to name slice 2 and the absent consolidated branch", got)
	}
	stale := &waveIntegrationError{sliceIndex: 2, dependsOn: []int{0, 1}, missing: []int{1}, consolidatedBranchPresent: true}
	if got := stale.message(); !strings.Contains(got, "stale consolidated branch") || !strings.Contains(got, "[1]") {
		t.Errorf("stale message = %q, want it to name the stale branch and the uncovered slices", got)
	}
	d := stale.details()
	if d["slice_index"] != 2 || d["consolidated_branch_present"] != true {
		t.Errorf("details = %+v, want slice_index 2 and consolidated_branch_present true", d)
	}
	if got, ok := d["missing_dependency_slices"].([]int); !ok || len(got) != 1 || got[0] != 1 {
		t.Errorf("missing_dependency_slices = %v, want [1]", d["missing_dependency_slices"])
	}
	// Always a non-nil array, so a consumer need not distinguish null from empty.
	if got, ok := none.details()["missing_dependency_slices"].([]int); !ok || got == nil {
		t.Errorf("missing_dependency_slices = %v, want a non-nil empty array", none.details()["missing_dependency_slices"])
	}
}
