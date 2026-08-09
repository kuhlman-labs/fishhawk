package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// seedParentDecompPlanWithScopes seeds a parent run whose approved plan's
// decomposition sub_plans declare the given per-slice scope.files — the
// authority attributeBuildRequiredPaths reads to name the OWNING sibling slice.
func seedParentDecompPlanWithScopes(t *testing.T, rr *guardCountingRepo, art *fakeArtifactRepo, sliceFiles [][]string) uuid.UUID {
	t.Helper()
	parent := rr.seedRun()
	planStage := rr.seedStage(parent.ID, 0, run.StageStateSucceeded) // Type=plan
	subs := make([]plan.SubPlanSummary, len(sliceFiles))
	for i, files := range sliceFiles {
		sf := make([]plan.ScopeFile, 0, len(files))
		for _, f := range files {
			sf = append(sf, plan.ScopeFile{Path: f, Operation: "modify"})
		}
		subs[i] = plan.SubPlanSummary{
			Title:     fmt.Sprintf("slice %d", i),
			ScopeHint: "x",
			Scope:     &plan.Scope{Files: sf},
		}
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

// TestAttributeBuildRequiredPaths_NamesOwningSiblingSlice is the #2548 live
// case: slice 0's committed tree needed backend/internal/server/prompt.go,
// which slice 1 DECLARES. The attribution must name slice 1 — that naming is
// the entire operator value of this class, since without it the park says only
// "a file was out of scope" and sends the operator back to reading plans.
func TestAttributeBuildRequiredPaths_NamesOwningSiblingSlice(t *testing.T) {
	s, rr, art := newGuardServer(t)
	parentID := seedParentDecompPlanWithScopes(t, rr, art, [][]string{
		{"backend/internal/server/scope_completeness.go"},
		{"backend/internal/server/prompt.go", "backend/internal/server/reads.go"},
	})
	child := seedGuardChild(rr, parentID, 0, run.StateRunning)

	owners := s.attributeBuildRequiredPaths(context.Background(), child,
		[]string{"backend/internal/server/prompt.go"})
	if len(owners) != 1 {
		t.Fatalf("owners = %+v, want one entry per path", owners)
	}
	if owners[0].Path != "backend/internal/server/prompt.go" {
		t.Errorf("owner path = %q", owners[0].Path)
	}
	if owners[0].SliceIndex == nil || *owners[0].SliceIndex != 1 {
		t.Fatalf("owning slice index = %v, want 1 (the sibling that declares the file)", owners[0].SliceIndex)
	}
	if owners[0].SliceTitle != "slice 1" {
		t.Errorf("owning slice title = %q, want \"slice 1\"", owners[0].SliceTitle)
	}
}

// TestAttributeBuildRequiredPaths_Degrades covers EVERY degrade branch, each
// asserting the SAME contract: no error, one unattributed owner per path, and
// the park is still recordable. Attribution is diagnostic — a failure to
// attribute must never cost the operator the preserved commit.
func TestAttributeBuildRequiredPaths_Degrades(t *testing.T) {
	paths := []string{"backend/internal/server/prompt.go"}

	assertUnattributed := func(t *testing.T, owners []run.BuildRequiredOwner, why string) {
		t.Helper()
		if len(owners) != 1 || owners[0].Path != paths[0] {
			t.Fatalf("%s: owners = %+v, want one entry naming the path", why, owners)
		}
		if owners[0].SliceIndex != nil || owners[0].SliceTitle != "" {
			t.Errorf("%s: want an UNATTRIBUTED owner, got %+v", why, owners[0])
		}
	}

	t.Run("no_parent_plan", func(t *testing.T) {
		// A parent with NO plan stage → loadApprovedPlanForRun returns (nil, nil).
		s, rr, _ := newGuardServer(t)
		parent := rr.seedRun()
		child := seedGuardChild(rr, parent.ID, 0, run.StateRunning)
		assertUnattributed(t, s.attributeBuildRequiredPaths(context.Background(), child, paths), "no parent plan")
	})

	t.Run("no_decomposition", func(t *testing.T) {
		s, rr, art := newGuardServer(t)
		parentID := seedParentPlanNoDecomp(t, rr, art)
		child := seedGuardChild(rr, parentID, 0, run.StateRunning)
		assertUnattributed(t, s.attributeBuildRequiredPaths(context.Background(), child, paths), "plan without decomposition")
	})

	t.Run("path_no_sub_plan_claims", func(t *testing.T) {
		s, rr, art := newGuardServer(t)
		parentID := seedParentDecompPlanWithScopes(t, rr, art, [][]string{
			{"backend/internal/server/scope_completeness.go"},
			{"backend/internal/server/reads.go"},
		})
		child := seedGuardChild(rr, parentID, 0, run.StateRunning)
		assertUnattributed(t, s.attributeBuildRequiredPaths(context.Background(), child, paths), "a path no sub-plan declares")
	})

	t.Run("plan_load_error", func(t *testing.T) {
		// A plan-LOAD error is the one place this helper deliberately DIVERGES
		// from resolveSliceDependencies' fail-closed posture: it degrades rather
		// than failing, because failing here would discard the preserved commit.
		s, rr, art := newGuardServer(t)
		parentID := seedParentDecompPlanWithScopes(t, rr, art, [][]string{{"backend/internal/server/prompt.go"}})
		art.listErr = errors.New("artifact list boom")
		child := seedGuardChild(rr, parentID, 0, run.StateRunning)
		assertUnattributed(t, s.attributeBuildRequiredPaths(context.Background(), child, paths), "a plan-load error")
	})

	t.Run("not_a_fan_out_child", func(t *testing.T) {
		s, rr, _ := newGuardServer(t)
		standalone := rr.seedRun() // DecomposedFrom nil
		assertUnattributed(t, s.attributeBuildRequiredPaths(context.Background(), standalone, paths), "a non-decomposed run")
	})

	t.Run("no_paths_returns_nil", func(t *testing.T) {
		s, rr, _ := newGuardServer(t)
		child := rr.seedRun()
		if got := s.attributeBuildRequiredPaths(context.Background(), child, nil); got != nil {
			t.Errorf("no paths must return nil, got %+v", got)
		}
	})
}

// TestParkScopeCompleteness_EmitsParentSignalWithoutSiblingTransition is p3, the
// LAST-NON-TERMINAL-CHILD case and the reason the emission is DUAL.
// maybeAdvanceDecomposedParent is reached ONLY from completeRun on a child's
// TERMINAL transition; a parked child does not complete, so if it is the last
// non-terminal sibling that hook never fires again. This proves the parent-side
// surfacing does NOT depend on the orchestrator path firing: parking child 0
// alone, with no subsequent child terminal transition, puts the FULL-payload
// signal on the PARENT's chain.
func TestParkScopeCompleteness_EmitsParentSignalWithoutSiblingTransition(t *testing.T) {
	s, rr, art := newGuardServer(t)
	au := &auditCapture{}
	s.cfg.AuditRepo = au

	parentID := seedParentDecompPlanWithScopes(t, rr, art, [][]string{
		{"backend/internal/server/scope_completeness.go"},
		{"backend/internal/server/prompt.go"},
	})
	// The parent is parked in awaiting_children.
	parentStage := rr.seedStage(parentID, 1, run.StageStateAwaitingChildren)
	child := seedGuardChild(rr, parentID, 0, run.StateRunning)
	childStage := rr.seedStage(child.ID, 1, run.StageStateRunning)
	childStage.Type = run.StageTypeImplement

	park := run.ScopeCompletenessPark{
		HeldCommitSHA:      "1111111111111111111111111111111111111111",
		RunBranch:          "fishhawk/run-parent/slice-0",
		VerifiedTreeSHA:    "2222222222222222222222222222222222222222",
		BaseSHA:            "3333333333333333333333333333333333333333",
		BuildRequiredPaths: []string{"backend/internal/server/prompt.go"},
	}
	park.OwningSlices = s.attributeBuildRequiredPaths(context.Background(), child, park.BuildRequiredPaths)

	// NO sibling transition of any kind — the orchestrator hook is never invoked.
	s.emitParentAwaitingChildScopeDecision(context.Background(), child, childStage.ID, park)

	au.mu.Lock()
	defer au.mu.Unlock()
	var entry *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == CategoryParentAwaitingChildScopeDecision {
			entry = &au.appended[i]
		}
	}
	if entry == nil {
		t.Fatalf("the PARENT must carry a %s entry with no sibling transition; got %+v",
			CategoryParentAwaitingChildScopeDecision, au.appended)
	}
	if entry.RunID != parentID {
		t.Errorf("the entry must be written on the PARENT run %s, got %s", parentID, entry.RunID)
	}
	assertParentAwaitingPayload(t, entry.Payload, child.ID, childStage.ID, 0, parentStage.ID)
}

// TestParkScopeCompleteness_NoParentSignalForNonBuildRequiredPark pins the
// no-op guard: the two OLDER shortfall classes (and a non-decomposed run) must
// not emit the parent-side signal at all — this class is decomposed-children-only
// and the older classes have their own (exempt-eligible) resolution path.
func TestParkScopeCompleteness_NoParentSignalForNonBuildRequiredPark(t *testing.T) {
	s, rr, art := newGuardServer(t)
	au := &auditCapture{}
	s.cfg.AuditRepo = au
	parentID := seedParentDecompPlanWithScopes(t, rr, art, [][]string{{"a.go"}, {"b.go"}})
	rr.seedStage(parentID, 1, run.StageStateAwaitingChildren)
	child := seedGuardChild(rr, parentID, 0, run.StateRunning)
	childStage := rr.seedStage(child.ID, 1, run.StageStateRunning)

	// (a) a missing-scope-class park on a decomposed child: no build-required
	// paths → no signal.
	s.emitParentAwaitingChildScopeDecision(context.Background(), child, childStage.ID,
		run.ScopeCompletenessPark{MissingPaths: []string{"a.go"}})
	// (b) a build-required park on a NON-decomposed run → no signal.
	standalone := rr.seedRun()
	s.emitParentAwaitingChildScopeDecision(context.Background(), standalone, childStage.ID,
		run.ScopeCompletenessPark{BuildRequiredPaths: []string{"a.go"}})

	au.mu.Lock()
	defer au.mu.Unlock()
	for _, e := range au.appended {
		if e.Category == CategoryParentAwaitingChildScopeDecision {
			t.Fatalf("no parent signal may be emitted for a non-build-required park or a non-decomposed run; got %+v", au.appended)
		}
	}
}

// assertParentAwaitingPayload pins the FULL parent-signal payload — not merely
// that a child is parked. The attribution (shortfall_class,
// build_required_paths, owning_slices) is the operator value of the entry; an
// emission that dropped it would otherwise pass an existence-only assertion.
func assertParentAwaitingPayload(t *testing.T, raw []byte, childRunID, childStageID uuid.UUID, sliceIdx int, parentStageID uuid.UUID) {
	t.Helper()
	var p struct {
		ParentStageID      string   `json:"parent_stage_id"`
		ChildRunID         string   `json:"child_run_id"`
		ChildStageID       string   `json:"child_stage_id"`
		ChildSliceIndex    *int     `json:"child_slice_index"`
		ShortfallClass     string   `json:"shortfall_class"`
		BuildRequiredPaths []string `json:"build_required_paths"`
		OwningSlices       []struct {
			Path       string `json:"path"`
			SliceIndex *int   `json:"slice_index"`
			SliceTitle string `json:"slice_title"`
		} `json:"owning_slices"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode parent signal payload: %v (%s)", err, raw)
	}
	if p.ChildRunID != childRunID.String() {
		t.Errorf("child_run_id = %q, want %q", p.ChildRunID, childRunID)
	}
	if p.ChildStageID != childStageID.String() {
		t.Errorf("child_stage_id = %q, want %q", p.ChildStageID, childStageID)
	}
	if p.ChildSliceIndex == nil || *p.ChildSliceIndex != sliceIdx {
		t.Errorf("child_slice_index = %v, want %d", p.ChildSliceIndex, sliceIdx)
	}
	if p.ParentStageID != parentStageID.String() {
		t.Errorf("parent_stage_id = %q, want %q", p.ParentStageID, parentStageID)
	}
	// THE ATTRIBUTION — condition 2. An emission that named only the child would
	// pass every assertion above and still send the operator back to reading
	// plans by hand.
	if p.ShortfallClass != ShortfallClassBuildRequired {
		t.Errorf("shortfall_class = %q, want %q", p.ShortfallClass, ShortfallClassBuildRequired)
	}
	if len(p.BuildRequiredPaths) != 1 || p.BuildRequiredPaths[0] != "backend/internal/server/prompt.go" {
		t.Errorf("build_required_paths = %v, want [backend/internal/server/prompt.go]", p.BuildRequiredPaths)
	}
	if len(p.OwningSlices) != 1 {
		t.Fatalf("owning_slices = %+v, want one attributed entry", p.OwningSlices)
	}
	if p.OwningSlices[0].SliceIndex == nil || *p.OwningSlices[0].SliceIndex != 1 || p.OwningSlices[0].SliceTitle != "slice 1" {
		t.Errorf("owning_slices[0] = %+v, want slice_index 1 / title \"slice 1\"", p.OwningSlices[0])
	}
}

// assertBuildRequiredAuditAttribution pins the VALUES of the build-required
// attribution on a scope_completeness_* audit payload — the paths that broke the
// boundary and the sibling slice that owns each. A presence-only check
// ("build_required_paths != nil") stays green when the emitter substitutes a
// wrong non-empty array, and the audit trail is the operator's only record of
// WHICH boundary was cut, so the derived audit copies get the same value-level
// pin the park row and the parent signal already carry.
func assertBuildRequiredAuditAttribution(t *testing.T, raw []byte, wantPath string, wantSliceIdx int, wantTitle string) {
	t.Helper()
	var p struct {
		BuildRequiredPaths []string `json:"build_required_paths"`
		OwningSlices       []struct {
			Path       string `json:"path"`
			SliceIndex *int   `json:"slice_index"`
			SliceTitle string `json:"slice_title"`
		} `json:"owning_slices"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode audit payload: %v (%s)", err, raw)
	}
	if len(p.BuildRequiredPaths) != 1 || p.BuildRequiredPaths[0] != wantPath {
		t.Errorf("build_required_paths = %v, want [%s]", p.BuildRequiredPaths, wantPath)
	}
	if len(p.OwningSlices) != 1 {
		t.Fatalf("owning_slices = %+v, want one attributed entry", p.OwningSlices)
	}
	got := p.OwningSlices[0]
	if got.Path != wantPath || got.SliceIndex == nil || *got.SliceIndex != wantSliceIdx || got.SliceTitle != wantTitle {
		t.Errorf("owning_slices[0] = %+v, want path %q / slice_index %d / title %q",
			got, wantPath, wantSliceIdx, wantTitle)
	}
}

// parkStageListErrRepo makes ListStagesForRun fail so the parent-stage resolution
// degrade branch of emitParentAwaitingChildScopeDecision is reachable.
type parkStageListErrRepo struct {
	*guardCountingRepo
	err error
}

func (r *parkStageListErrRepo) ListStagesForRun(ctx context.Context, id uuid.UUID) ([]*run.Stage, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.guardCountingRepo.ListStagesForRun(ctx, id)
}

// appendErrAudit fails every AppendChained — the best-effort degrade branch.
type appendErrAudit struct{ auditCapture }

func (a *appendErrAudit) AppendChained(context.Context, audit.ChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("append boom")
}

// TestEmitParentAwaitingChildScopeDecision_DegradeBranches covers EVERY
// defensive branch of the park-time emitter. Each asserts the same contract: a
// failure here NEVER unwinds the park, which is the thing preserving the
// slice's ~25-minute pass. The signal is best-effort diagnostics.
func TestEmitParentAwaitingChildScopeDecision_DegradeBranches(t *testing.T) {
	buildPark := func() run.ScopeCompletenessPark {
		return run.ScopeCompletenessPark{
			HeldCommitSHA:      "1111111111111111111111111111111111111111",
			RunBranch:          "fishhawk/run-parent/slice-0",
			BuildRequiredPaths: []string{"backend/internal/server/prompt.go"},
		}
	}

	t.Run("nil_audit_repo_is_a_no_op", func(t *testing.T) {
		s, rr, _ := newGuardServer(t) // New(Config{...}) with no AuditRepo
		parent := rr.seedRun()
		child := seedGuardChild(rr, parent.ID, 0, run.StateRunning)
		// Must not panic.
		s.emitParentAwaitingChildScopeDecision(context.Background(), child, uuid.New(), buildPark())
	})

	t.Run("parent_stage_list_error_skips_emission", func(t *testing.T) {
		rr := &parkStageListErrRepo{guardCountingRepo: &guardCountingRepo{orchestratorRepo: newOrchestratorRepo()}}
		au := &auditCapture{}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		parent := rr.seedRun()
		child := seedGuardChild(rr.guardCountingRepo, parent.ID, 0, run.StateRunning)
		rr.err = errors.New("stage list boom")

		s.emitParentAwaitingChildScopeDecision(context.Background(), child, uuid.New(), buildPark())

		au.mu.Lock()
		defer au.mu.Unlock()
		if len(au.appended) != 0 {
			t.Errorf("an unresolvable parent must skip the emission, got %+v", au.appended)
		}
	})

	t.Run("append_error_does_not_unwind", func(t *testing.T) {
		rr := &guardCountingRepo{orchestratorRepo: newOrchestratorRepo()}
		au := &appendErrAudit{}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		parent := rr.seedRun()
		rr.seedStage(parent.ID, 1, run.StageStateAwaitingChildren)
		child := seedGuardChild(rr, parent.ID, 0, run.StateRunning)
		// Must return cleanly — the park has already landed and must not unwind.
		s.emitParentAwaitingChildScopeDecision(context.Background(), child, uuid.New(), buildPark())
	})

	t.Run("parent_without_awaiting_children_stage_still_emits", func(t *testing.T) {
		// A nil parent stage is NOT an error: the entry is appended run-scoped
		// (StageID nil, no parent_stage_id) rather than dropped.
		rr := &guardCountingRepo{orchestratorRepo: newOrchestratorRepo()}
		au := &auditCapture{}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		parent := rr.seedRun() // no stages
		child := seedGuardChild(rr, parent.ID, 0, run.StateRunning)
		childStageID := uuid.New()

		s.emitParentAwaitingChildScopeDecision(context.Background(), child, childStageID, buildPark())

		au.mu.Lock()
		defer au.mu.Unlock()
		if len(au.appended) != 1 {
			t.Fatalf("want one run-scoped entry, got %+v", au.appended)
		}
		if au.appended[0].StageID != nil {
			t.Errorf("StageID must be nil when the parent has no awaiting_children stage, got %v", au.appended[0].StageID)
		}
		var p map[string]any
		_ = json.Unmarshal(au.appended[0].Payload, &p)
		if _, ok := p["parent_stage_id"]; ok {
			t.Errorf("parent_stage_id must be absent: %v", p)
		}
		if p["child_run_id"] != child.ID.String() {
			t.Errorf("the signal must still name the parked child: %v", p)
		}
	})

	t.Run("child_without_slice_index_omits_it", func(t *testing.T) {
		rr := &guardCountingRepo{orchestratorRepo: newOrchestratorRepo()}
		au := &auditCapture{}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
		parent := rr.seedRun()
		rr.seedStage(parent.ID, 1, run.StageStateAwaitingChildren)
		child := rr.seedRun()
		pid := parent.ID
		child.DecomposedFrom = &pid // SliceIndex deliberately nil

		s.emitParentAwaitingChildScopeDecision(context.Background(), child, uuid.New(), buildPark())

		au.mu.Lock()
		defer au.mu.Unlock()
		if len(au.appended) != 1 {
			t.Fatalf("want one entry, got %+v", au.appended)
		}
		var p map[string]any
		_ = json.Unmarshal(au.appended[0].Payload, &p)
		if _, ok := p["child_slice_index"]; ok {
			t.Errorf("child_slice_index must be omitted when the child carries none: %v", p)
		}
	})
}
