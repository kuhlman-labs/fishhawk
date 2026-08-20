package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// newHeadroomServer wires a Server with every repo
// effectiveScopeHeadroom reads: run (with cached spec), plan artifact,
// audit (for add_scope_files), and scope amendments. The run carries
// specImplementPathConstraints (implement-stage max_files_changed: 3).
func newHeadroomServer(t *testing.T, workflowSpec []byte, scopeFiles []plan.ScopeFile) (*Server, *orchestratorRepo, *fakeScopeAmendmentRepo, *run.Run, *run.Stage) {
	t.Helper()
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	au := newAuditFake()
	sa := newFakeScopeAmendmentRepo()

	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = workflowSpec
	planStage := rr.seedStage(runRow.ID, 0, run.StageStateAwaitingApproval)
	if scopeFiles != nil {
		seedBudgetPlanArtifact(t, art, planStage.ID, &plan.Plan{
			PlanVersion: "standard_v1",
			Scope:       plan.Scope{Files: scopeFiles},
		})
	}

	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		ArtifactRepo:       art,
		AuditRepo:          au,
		ScopeAmendmentRepo: sa,
	})
	return s, rr, sa, runRow, planStage
}

// TestEffectiveScopeHeadroom_DedupeParityWithFoldScopePaths is the
// dedupe-parity seam test (#983): the count the gate computes must
// equal the file set the prompt builder folds via foldScopePaths for
// identical overlapping inputs — including a trailing-slash directory
// entry. If either side's dedupe semantics change, this fails.
func TestEffectiveScopeHeadroom_DedupeParityWithFoldScopePaths(t *testing.T) {
	scopeFiles := []plan.ScopeFile{
		{Path: "backend/a.go", Operation: plan.FileOpModify},
		{Path: "backend/b.go", Operation: plan.FileOpModify},
	}
	// Overlaps b.go, adds c.go and a trailing-slash directory entry.
	extra := []string{"backend/b.go", "backend/c.go", "docs/newdir/"}

	s, _, _, runRow, _ := newHeadroomServer(t, specImplementPathConstraints, scopeFiles)

	count, maxFiles, ok := s.effectiveScopeHeadroom(context.Background(), runRow.ID, extra)
	if !ok {
		t.Fatal("effectiveScopeHeadroom ok = false, want true")
	}
	if maxFiles != 3 {
		t.Errorf("maxFiles = %d, want 3", maxFiles)
	}

	promptScope := make([]scopeFile, 0, len(scopeFiles))
	for _, f := range scopeFiles {
		promptScope = append(promptScope, scopeFile{Path: f.Path, Operation: string(f.Operation)})
	}
	folded := s.foldScopePaths(context.Background(), promptScope, extra, "test")
	if count != len(folded) {
		t.Errorf("effectiveScopeHeadroom count = %d, foldScopePaths produced %d files — dedupe semantics diverged", count, len(folded))
	}
	if count != 4 {
		t.Errorf("count = %d, want 4 (a, b, c, docs/newdir/)", count)
	}
}

// TestEffectiveScopeHeadroom_SubtractsRemovals is the #1726
// cap-overflow-reconciled-at-the-gate done-means: a plan whose scope is one
// file over the max_files_changed cap (4 files, cap 3) drops back under the
// cap once a single path is removed via remove_scope_files, so the count the
// scope-cap gate reads passes without a re-plan.
func TestEffectiveScopeHeadroom_SubtractsRemovals(t *testing.T) {
	scopeFiles := []plan.ScopeFile{
		{Path: "backend/a.go", Operation: plan.FileOpModify},
		{Path: "backend/b.go", Operation: plan.FileOpModify},
		{Path: "backend/c.go", Operation: plan.FileOpModify},
		{Path: "backend/d.go", Operation: plan.FileOpModify},
	}
	s, _, _, runRow, _ := newHeadroomServer(t, specImplementPathConstraints, scopeFiles)

	// Over cap (4 > 3) with no removal.
	count, maxFiles, ok := s.effectiveScopeHeadroom(context.Background(), runRow.ID, nil)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if count != 4 || maxFiles != 3 {
		t.Fatalf("count/maxFiles = %d/%d, want 4/3", count, maxFiles)
	}
	if count <= maxFiles {
		t.Fatalf("expected over-cap without removal: count %d <= max %d", count, maxFiles)
	}

	// Removing one declared path drops the count to the cap. The scope-cap
	// gate reads this through effectiveScopePathSet (the shared helper), so
	// assert against it directly.
	paths, _, ok := s.effectiveScopePathSet(context.Background(), runRow.ID, nil, []string{"backend/d.go"})
	if !ok {
		t.Fatal("ok = false with removal, want true")
	}
	if len(paths) != 3 {
		t.Errorf("count with one removal = %d, want 3 (reconciled to cap)", len(paths))
	}
	if len(paths) > maxFiles {
		t.Errorf("still over cap after removal: count %d > max %d", len(paths), maxFiles)
	}
}

// TestEffectiveScopeHeadroom_FailOpenMissingSpec asserts the
// checkPlanBudget-mirroring fail-open contract: no cached workflow
// spec → ok=false, caller skips the check.
func TestEffectiveScopeHeadroom_FailOpenMissingSpec(t *testing.T) {
	s, _, _, runRow, _ := newHeadroomServer(t, nil, []plan.ScopeFile{
		{Path: "backend/a.go", Operation: plan.FileOpModify},
	})
	if _, _, ok := s.effectiveScopeHeadroom(context.Background(), runRow.ID, nil); ok {
		t.Error("ok = true with no workflow spec, want false (fail-open)")
	}
}

// TestEffectiveScopeHeadroom_FailOpenMissingPlan asserts ok=false when
// the run has no plan artifact.
func TestEffectiveScopeHeadroom_FailOpenMissingPlan(t *testing.T) {
	s, _, _, runRow, _ := newHeadroomServer(t, specImplementPathConstraints, nil)
	if _, _, ok := s.effectiveScopeHeadroom(context.Background(), runRow.ID, nil); ok {
		t.Error("ok = true with no plan artifact, want false (fail-open)")
	}
}

// TestEffectiveScopeHeadroom_FailOpenMissingRun asserts ok=false on a
// run-read failure.
func TestEffectiveScopeHeadroom_FailOpenMissingRun(t *testing.T) {
	s, _, _, _, _ := newHeadroomServer(t, specImplementPathConstraints, nil)
	if _, _, ok := s.effectiveScopeHeadroom(context.Background(), uuid.New(), nil); ok {
		t.Error("ok = true for a nonexistent run, want false (fail-open)")
	}
}

// TestEffectiveScopeHeadroom_AmendmentStatusFiltering asserts only
// APPROVED amendments count toward the effective scope — pending and
// denied paths confer nothing, mirroring the prompt builder's
// resolveApprovedScopeAmendments.
func TestEffectiveScopeHeadroom_AmendmentStatusFiltering(t *testing.T) {
	s, rr, sa, runRow, _ := newHeadroomServer(t, specImplementPathConstraints, []plan.ScopeFile{
		{Path: "backend/a.go", Operation: plan.FileOpModify},
	})
	implStage := rr.seedStage(runRow.ID, 1, run.StageStateRunning)
	implStage.Type = run.StageTypeImplement

	seedAmendment := func(path string, status scopeamendment.Status) {
		t.Helper()
		a, err := sa.Create(context.Background(), scopeamendment.CreateParams{
			RunID:   runRow.ID,
			StageID: implStage.ID,
			Paths:   []scopeamendment.PathEntry{{Path: path, Operation: "modify"}},
			Reason:  "test",
		})
		if err != nil {
			t.Fatalf("create amendment: %v", err)
		}
		if status != scopeamendment.StatusPending {
			if _, err := sa.Decide(context.Background(), scopeamendment.DecideParams{
				ID: a.ID, Status: status, Reason: "test", DecidedBy: "op",
			}); err != nil {
				t.Fatalf("decide amendment: %v", err)
			}
		}
	}
	seedAmendment("backend/approved.go", scopeamendment.StatusApproved)
	seedAmendment("backend/pending.go", scopeamendment.StatusPending)
	seedAmendment("backend/denied.go", scopeamendment.StatusDenied)

	count, _, ok := s.effectiveScopeHeadroom(context.Background(), runRow.ID, nil)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	// a.go (plan) + approved.go; pending/denied excluded.
	if count != 2 {
		t.Errorf("count = %d, want 2 (plan file + approved amendment only)", count)
	}
}

// TestEffectiveScopeHeadroom_CountsPriorSliceAdds is the #2515 cap-arithmetic
// pin: a PRIOR approval's per-slice adds count in the effective set, because the
// prompt builder folds each slice's entry into that child's scope — so omitting
// them here would make the number the cap gate reports smaller than the scope
// actually assembled. The control (no per-slice entry seeded) proves the count
// is attributable to the recorded map and not to the plan alone.
func TestEffectiveScopeHeadroom_CountsPriorSliceAdds(t *testing.T) {
	scopeFiles := []plan.ScopeFile{
		{Path: "backend/a.go", Operation: plan.FileOpModify},
		{Path: "backend/b.go", Operation: plan.FileOpModify},
	}

	t.Run("control: no recorded per-slice map", func(t *testing.T) {
		s, _, _, runRow, _ := newHeadroomServer(t, specImplementPathConstraints, scopeFiles)
		count, _, ok := s.effectiveScopeHeadroom(context.Background(), runRow.ID, nil)
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if count != 2 {
			t.Errorf("count = %d, want 2 (plan scope only)", count)
		}
	})

	t.Run("prior per-slice adds are counted", func(t *testing.T) {
		s, _, _, runRow, _ := newHeadroomServer(t, specImplementPathConstraints, scopeFiles)
		au := s.cfg.AuditRepo.(*auditFake)
		rid := runRow.ID
		payload, err := json.Marshal(map[string]any{
			"decision": "approve",
			"add_scope_files_to_slice": map[string][]string{
				"0": {"backend/slice0.go"},
				"1": {"backend/slice1.go"},
			},
		})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		au.seeded = append(au.seeded, &audit.Entry{
			ID:       uuid.New(),
			RunID:    &rid,
			Category: "approval_submitted",
			Payload:  payload,
		})

		paths, _, ok := s.effectiveScopePathSet(context.Background(), runRow.ID, nil, nil)
		if !ok {
			t.Fatal("ok = false, want true")
		}
		got := map[string]bool{}
		for _, p := range paths {
			got[p] = true
		}
		for _, want := range []string{"backend/a.go", "backend/b.go", "backend/slice0.go", "backend/slice1.go"} {
			if !got[want] {
				t.Errorf("effective set missing %q; got %v", want, paths)
			}
		}
		if len(paths) != 4 {
			t.Errorf("count = %d, want 4 (plan scope + both slices' recorded adds)", len(paths))
		}
	})
}

// TestMinPhysicalFileCount table-drives the #2415 minimum-physical-file
// estimator on every branch: modify-only (no pairing), equal creates/deletes
// (full rename pairing), unequal creates/deletes (max wins), generated/vendored
// exemption (parity with policy.CountedFileCount), unknown-op paths (an
// add_scope_files/amendment path with no ops entry counts one each), the empty
// set, and the HONEST-LIMIT case where an UNDECLARED create paired with a
// declared delete makes the estimate OVER-count the true physical diff.
func TestMinPhysicalFileCount(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		ops   map[string]plan.FileOperation
		want  int
	}{
		{
			name:  "empty set",
			paths: nil,
			ops:   nil,
			want:  0,
		},
		{
			name:  "all modify, no pairing",
			paths: []string{"a.go", "b.go", "c.go"},
			ops:   map[string]plan.FileOperation{"a.go": plan.FileOpModify, "b.go": plan.FileOpModify, "c.go": plan.FileOpModify},
			want:  3,
		},
		{
			name:  "equal creates and deletes pair to N",
			paths: []string{"new1.go", "new2.go", "old1.go", "old2.go"},
			ops: map[string]plan.FileOperation{
				"new1.go": plan.FileOpCreate, "new2.go": plan.FileOpCreate,
				"old1.go": plan.FileOpDelete, "old2.go": plan.FileOpDelete,
			},
			// others=0, max(2,2)=2 — two delete+create pairs can each collapse
			// into one rename row.
			want: 2,
		},
		{
			name:  "unequal creates and deletes take the max",
			paths: []string{"n1.go", "n2.go", "n3.go", "o1.go"},
			ops: map[string]plan.FileOperation{
				"n1.go": plan.FileOpCreate, "n2.go": plan.FileOpCreate, "n3.go": plan.FileOpCreate,
				"o1.go": plan.FileOpDelete,
			},
			// others=0, max(3,1)=3 — only one pair can collapse; two creates
			// remain unpaired.
			want: 3,
		},
		{
			name:  "modifies plus a balanced rename pair",
			paths: []string{"keep.go", "new.go", "old.go"},
			ops: map[string]plan.FileOperation{
				"keep.go": plan.FileOpModify,
				"new.go":  plan.FileOpCreate,
				"old.go":  plan.FileOpDelete,
			},
			// others=1, max(1,1)=1 → 2.
			want: 2,
		},
		{
			name:  "generated and vendored paths exempt",
			paths: []string{"backend/internal/x/db/queries.sql.go", "vendor/foo/bar.go", "backend/real.go"},
			ops: map[string]plan.FileOperation{
				"backend/internal/x/db/queries.sql.go": plan.FileOpModify,
				"vendor/foo/bar.go":                    plan.FileOpModify,
				"backend/real.go":                      plan.FileOpModify,
			},
			// Only backend/real.go is counted — the db/ and vendor/ paths are
			// exempt exactly as policy.CountedFileCount exempts them.
			want: 1,
		},
		{
			name:  "unknown-op paths count one each",
			paths: []string{"declared.go", "added1.go", "added2.go"},
			ops:   map[string]plan.FileOperation{"declared.go": plan.FileOpModify},
			// added1/added2 have no ops entry (an add_scope_files/amendment
			// path) → treated as non-pairing modifies, one each.
			want: 3,
		},
		{
			name:  "honest limit: undeclared create over-counts a real rename",
			paths: []string{"old.go", "new.go"},
			// Only the delete is declared; the create carries no ops entry, so
			// the estimator cannot pair them. deletes=1, others=1 → 2, while
			// git would emit ONE rename row (physical 1). The estimate is a
			// generous OVER-count here — never an under-count — so the refusal
			// it feeds can only over-refuse, never admit an over-cap landing.
			ops:  map[string]plan.FileOperation{"old.go": plan.FileOpDelete},
			want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := minPhysicalFileCount(tc.paths, tc.ops); got != tc.want {
				t.Errorf("minPhysicalFileCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEffectiveScopePathSetWithOps_ConsistentOpsMap asserts the ops map the
// #2415 helper returns is consistent with its returned path set: every key is a
// surviving path, a removed plan path is absent from BOTH, and each plan-
// declared surviving path carries its declared operation. It also proves an
// add_scope_files path is present in the path set but carries NO ops entry (the
// non-pairing-modify case minPhysicalFileCount reads).
func TestEffectiveScopePathSetWithOps_ConsistentOpsMap(t *testing.T) {
	scopeFiles := []plan.ScopeFile{
		{Path: "backend/keep.go", Operation: plan.FileOpModify},
		{Path: "backend/old.go", Operation: plan.FileOpDelete},
		{Path: "backend/drop.go", Operation: plan.FileOpCreate},
	}
	s, _, _, runRow, _ := newHeadroomServer(t, specImplementPathConstraints, scopeFiles)

	paths, ops, maxFiles, ok := s.effectiveScopePathSetWithOps(
		context.Background(), runRow.ID,
		[]string{"backend/added.go"}, // add
		[]string{"backend/drop.go"},  // remove a declared create
	)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if maxFiles != 3 {
		t.Errorf("maxFiles = %d, want 3", maxFiles)
	}

	pathSet := map[string]bool{}
	for _, p := range paths {
		pathSet[p] = true
	}
	// The removed declared path is gone from the path set AND the ops map.
	if pathSet["backend/drop.go"] {
		t.Errorf("removed path still in path set: %v", paths)
	}
	if _, present := ops["backend/drop.go"]; present {
		t.Errorf("removed path still in ops map: %v", ops)
	}
	// Every ops key is a surviving path.
	for k := range ops {
		if !pathSet[k] {
			t.Errorf("ops key %q is not in the returned path set %v", k, paths)
		}
	}
	// Declared surviving paths carry their declared operation.
	if ops["backend/keep.go"] != plan.FileOpModify {
		t.Errorf("ops[keep] = %q, want modify", ops["backend/keep.go"])
	}
	if ops["backend/old.go"] != plan.FileOpDelete {
		t.Errorf("ops[old] = %q, want delete", ops["backend/old.go"])
	}
	// The add_scope_files path is in the set but carries no ops entry.
	if !pathSet["backend/added.go"] {
		t.Errorf("added path missing from path set: %v", paths)
	}
	if _, present := ops["backend/added.go"]; present {
		t.Errorf("add_scope_files path unexpectedly has an ops entry: %v", ops)
	}
}

// TestEffectiveScopePathSetWithOps_FailOpenLegs asserts the single ok=false
// fail-open contract (#2415): whenever paths do not resolve, ops is nil too —
// there is no distinct "ops unavailable" state, so ok governs the whole
// resolution.
func TestEffectiveScopePathSetWithOps_FailOpenLegs(t *testing.T) {
	t.Run("missing spec", func(t *testing.T) {
		s, _, _, runRow, _ := newHeadroomServer(t, nil, []plan.ScopeFile{{Path: "a.go", Operation: plan.FileOpModify}})
		paths, ops, _, ok := s.effectiveScopePathSetWithOps(context.Background(), runRow.ID, nil, nil)
		if ok || paths != nil || ops != nil {
			t.Errorf("want (nil,nil,_,false) on missing spec; got paths=%v ops=%v ok=%v", paths, ops, ok)
		}
	})
	t.Run("missing plan", func(t *testing.T) {
		s, _, _, runRow, _ := newHeadroomServer(t, specImplementPathConstraints, nil)
		paths, ops, _, ok := s.effectiveScopePathSetWithOps(context.Background(), runRow.ID, nil, nil)
		if ok || paths != nil || ops != nil {
			t.Errorf("want (nil,nil,_,false) on missing plan; got paths=%v ops=%v ok=%v", paths, ops, ok)
		}
	})
	t.Run("missing run", func(t *testing.T) {
		s, _, _, _, _ := newHeadroomServer(t, specImplementPathConstraints, nil)
		paths, ops, _, ok := s.effectiveScopePathSetWithOps(context.Background(), uuid.New(), nil, nil)
		if ok || paths != nil || ops != nil {
			t.Errorf("want (nil,nil,_,false) on missing run; got paths=%v ops=%v ok=%v", paths, ops, ok)
		}
	})
}

// TestEffectiveScopeHeadroom_IgnoresSliceMoves pins the #2596 cap-neutrality
// property at the headroom-computation level: a recorded move_scope_files_to_slice
// entry does NOT change the effective scope count. A move relocates an
// already-declared path between slices — the total file count is unchanged — so
// it must consume zero headroom. The count is asserted byte-identical to the
// no-move control; a regression that threaded the move into the effective set
// (as an add would) turns this red.
func TestEffectiveScopeHeadroom_IgnoresSliceMoves(t *testing.T) {
	scopeFiles := []plan.ScopeFile{
		{Path: "backend/a.go", Operation: plan.FileOpModify},
		{Path: "backend/b.go", Operation: plan.FileOpModify},
	}

	control := func(t *testing.T) int {
		t.Helper()
		s, _, _, runRow, _ := newHeadroomServer(t, specImplementPathConstraints, scopeFiles)
		count, _, ok := s.effectiveScopeHeadroom(context.Background(), runRow.ID, nil)
		if !ok {
			t.Fatal("control ok = false, want true")
		}
		return count
	}
	want := control(t)

	s, _, _, runRow, _ := newHeadroomServer(t, specImplementPathConstraints, scopeFiles)
	au := s.cfg.AuditRepo.(*auditFake)
	rid := runRow.ID
	payload, err := json.Marshal(map[string]any{
		"decision": "approve",
		"move_scope_files_to_slice": map[string][]string{
			"1": {"backend/a.go"},
		},
		"move_scope_files_resolved": []map[string]any{
			{"path": "backend/a.go", "from_slice": 0, "to_slice": 1},
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	au.seeded = append(au.seeded, &audit.Entry{
		ID: uuid.New(), RunID: &rid, Category: "approval_submitted", Payload: payload,
	})

	count, _, ok := s.effectiveScopeHeadroom(context.Background(), runRow.ID, nil)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if count != want {
		t.Errorf("effective scope count = %d, want %d (a recorded move consumes no headroom)", count, want)
	}
}

// TestCheckPlanScopeCap_MoveConsumesNoHeadroom is the gate-level cap-neutrality
// pin (#2596): approving a move on a decomposed plan whose union scope is already
// EXACTLY at max_files_changed returns 200. A move is deliberately NOT threaded
// into unionScopeAdds, so the cap gate sees the unchanged plan-scope count. The
// regression this guards is a future edit that folded the move into the cap
// arithmetic — that would 422 an at-cap move.
func TestCheckPlanScopeCap_MoveConsumesNoHeadroom(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	app := newFakeApprovalRepo()
	au := newApprovalAuditFake()
	o := &orchestrator.Orchestrator{Runs: rr}
	runRow := rr.seedRun()
	runRow.WorkflowID = "feature_change"
	runRow.WorkflowSpec = specImplementPathConstraints // implement max_files_changed: 3
	stage := rr.seedStage(runRow.ID, 0, run.StageStateAwaitingApproval)

	// Decomposed plan: slice 0 owns a.go + a_test.go, slice 1 owns b.go — union of
	// 3, exactly the cap (the _test.go satisfies the spec's required-tests gate).
	// Moving a.go to slice 1 leaves slice 0 with a_test.go and the union count
	// unchanged at 3.
	p := decomposedPlanWithScopedSlices(
		[]string{"slice-a", "slice-b"},
		[]*plan.Scope{sliceScope("backend/a.go", "backend/a_test.go"), sliceScope("backend/b.go")},
	)
	p.Scope = plan.Scope{Files: []plan.ScopeFile{
		{Path: "backend/a.go", Operation: plan.FileOpModify},
		{Path: "backend/a_test.go", Operation: plan.FileOpModify},
		{Path: "backend/b.go", Operation: plan.FileOpModify},
	}}
	seedBudgetPlanArtifact(t, art, stage.ID, p)

	s := New(Config{
		Addr:         "127.0.0.1:0",
		ApprovalRepo: app,
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: o,
		ArtifactRepo: art,
	})

	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","move_scope_files_to_slice":{"slice-b":["backend/a.go"]}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a move on an at-cap plan consumes no headroom):\n%s", w.Code, w.Body.String())
	}
}
