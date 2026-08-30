package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// TestHumanReviewGate_BacklogGrooming_EndToEnd_PgBacked is the cross-boundary
// assertion for E54.53 / #3041: the scoped change spans spec parsing, stage-row
// persistence, the HTTP approval handler, the approval/quorum path and the
// orchestrator, and the per-layer unit tests all pass while that seam breaks.
//
// It drives the SHIPPED backlog_grooming declaration — read from
// docs/spec/examples/workflow-v2-backlog-grooming.yaml, so a drift in the
// declaration reddens this test rather than passing against a hand-written
// approximation — through:
//
//	spec.ParseBytes
//	  → webhook.CreateStagesFromSpec  (executor_kind from the PRODUCTION
//	                                   mapExecutor mapping, never hand-set)
//	  → real Orchestrator             (parks the human `confirm` stage at
//	                                   awaiting_approval)
//	  → POST /v0/stages/{id}/approvals (the admission conjunction + the
//	                                   attestation requirement)
//	  → real Orchestrator.completeRun
//
// and asserts BOTH the confirm stage reached succeeded AND the RUN row reached
// terminal `succeeded` — the state run 1499bdb0 cannot reach today.
func TestHumanReviewGate_BacklogGrooming_EndToEnd_PgBacked(t *testing.T) {
	ctx := context.Background()

	specPath := filepath.Join("..", "..", "..", "docs", "spec", "examples", "workflow-v2-backlog-grooming.yaml")
	specBytes, err := os.ReadFile(specPath) //nolint:gosec // fixed in-repo test fixture path
	if err != nil {
		t.Fatalf("read shipped grooming declaration: %v", err)
	}
	parsed, err := spec.ParseBytes(specBytes)
	if err != nil {
		t.Fatalf("parse shipped grooming declaration: %v", err)
	}
	wf, ok := parsed.Workflows["backlog_grooming"]
	if !ok {
		t.Fatalf("shipped declaration has no backlog_grooming workflow")
	}

	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	approvalRepo := approval.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	orch := &orchestrator.Orchestrator{Runs: runRepo}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		ApprovalRepo: approvalRepo,
		AuditRepo:    auditRepo,
		Orchestrator: orch,
	})

	runRow, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "backlog_grooming",
		WorkflowSHA:   "sha",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  specBytes,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// The production mapping writes executor_kind — nothing here hand-sets it,
	// so a mapExecutor that failed to persist `human` would leave the confirm
	// gate refused and the run non-terminal.
	stages, err := webhook.CreateStagesFromSpec(ctx, runRepo, runRow.ID, wf.Stages)
	if err != nil {
		t.Fatalf("create stages from spec: %v", err)
	}
	var confirm *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeReview {
			confirm = st
		}
	}
	if confirm == nil {
		t.Fatalf("shipped declaration produced no review stage row")
	}
	if confirm.ExecutorKind != run.ExecutorHuman {
		t.Fatalf("persisted executor_kind = %s, want human (from webhook.mapExecutor)", confirm.ExecutorKind)
	}

	// Drive the agent stages to succeeded the way the runner would, then let
	// the REAL orchestrator walk the run forward. It is the orchestrator that
	// parks a human-executor stage at awaiting_approval — this test does not
	// place the gate by hand.
	for _, st := range stages {
		if st.Type == run.StageTypeReview {
			continue
		}
		for _, to := range []run.StageState{
			run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded,
		} {
			if _, err := runRepo.TransitionStage(ctx, st.ID, to, nil); err != nil {
				t.Fatalf("transition %s stage to %s: %v", st.Type, to, err)
			}
		}
	}
	if _, err := orch.Advance(ctx, runRow.ID); err != nil {
		t.Fatalf("orchestrator advance to the confirm gate: %v", err)
	}
	confirm, err = runRepo.GetStage(ctx, confirm.ID)
	if err != nil {
		t.Fatalf("reload confirm stage: %v", err)
	}
	if confirm.State != run.StageStateAwaitingApproval {
		t.Fatalf("confirm stage state = %s, want awaiting_approval", confirm.State)
	}

	// This is the call that used to draw an unconditional 409, leaving the run
	// parked in `running` with no approvable surface anywhere.
	w := submitApproval(t, s, confirm.ID,
		`{"decision":"approve","comment":"checked the forge: the tracker carries every applied mutation"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm approval status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	got, err := runRepo.GetStage(ctx, confirm.ID)
	if err != nil {
		t.Fatalf("reload confirm stage after approval: %v", err)
	}
	if got.State != run.StageStateSucceeded {
		t.Fatalf("confirm stage state = %s, want succeeded", got.State)
	}
	finalRun, err := runRepo.GetRun(ctx, runRow.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if finalRun.State != run.StateSucceeded {
		t.Fatalf("run state = %s, want succeeded (the terminal state run 1499bdb0 cannot reach today)", finalRun.State)
	}
}
