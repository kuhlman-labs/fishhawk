package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// TestGroomStartGuard_PG_ParkedGroomBehindSixtyTerminalRunsIsRefused is the
// CROSS-BOUNDARY proof for the store-level non-terminal scan (#5225, M1): the
// real POST /v0/runs handler → checkNoParkedGroom → the real Postgres ListRuns
// SQL. One OLDEST groom with its plan stage at awaiting_approval sits behind 60
// NEWER cancelled grooms of the same repo and workflow; the guard must still
// find it, because the terminal filter runs in SQL before the 50-row LIMIT.
// State is seeded BY CONSTRUCTION through runRepo and direct pool UPDATEs.
//
// Counterfactual: drop the non_terminal arm from db/queries.sql.go's listRuns
// constant and the 50-row window holds only cancelled runs, so the POST is
// admitted 201 instead of refused 409.
func TestGroomStartGuard_PG_ParkedGroomBehindSixtyTerminalRunsIsRefused(t *testing.T) {
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	s := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     runRepo,
		AuditRepo:   audit.NewPostgresRepository(pool),
		ConcernRepo: newFakeConcernRepo(),
	})
	stubConventions(t, conventionsWithCharter(t), nil)
	ctx := context.Background()

	create := func() *run.Run {
		t.Helper()
		r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
			Repo: groomGuardRepo, WorkflowID: groomGuardWorkflow, WorkflowSHA: "abc",
			TriggerSource: run.TriggerCLI,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		return r
	}
	pin := func(r *run.Run, at time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE runs SET created_at=$1 WHERE id=$2`, at, r.ID); err != nil {
			t.Fatalf("pin created_at: %v", err)
		}
	}
	base := time.Now().UTC().Add(-2 * time.Hour)

	parked := create()
	pin(parked, base)
	stage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: parked.ID, Sequence: 1, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runs SET state='running' WHERE id=$1`, parked.ID); err != nil {
		t.Fatalf("set parked run running: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE stages SET state='awaiting_approval' WHERE id=$1`, stage.ID); err != nil {
		t.Fatalf("park stage: %v", err)
	}
	for i := 1; i <= 60; i++ {
		r := create()
		pin(r, base.Add(time.Duration(i)*time.Second))
		if _, err := pool.Exec(ctx, `UPDATE runs SET state='cancelled' WHERE id=$1`, r.ID); err != nil {
			t.Fatalf("cancel run: %v", err)
		}
	}

	w := createRunViaHandler(t, s, groomBody(groomingSpecYAML))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 grooming_run_parked (the parked groom behind 60 terminal runs must be found):\n%s", w.Code, w.Body.String())
	}
	assertParkedRefusal(t, w, parked.ID, run.StageTypePlan)

	all, err := runRepo.ListRuns(ctx, run.ListRunsFilter{Repo: groomGuardRepo, Limit: 1000})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(all) != 61 {
		t.Errorf("run rows after the refusal = %d, want 61 (a refusal inserts nothing)", len(all))
	}
}
