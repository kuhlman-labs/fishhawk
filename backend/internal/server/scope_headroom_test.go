package server

import (
	"context"
	"testing"

	"github.com/google/uuid"

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
