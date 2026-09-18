package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// acceptance_retirement_drop_pg_test.go drives the cancel-sink retirement-drop
// writer against a REAL Postgres (#3439), mirroring
// acceptance_arbitration_pg_test.go's fixture style. A fake audit repository
// structurally cannot exercise what this issue is about — the run-row lock and
// the in-transaction (stage_id, reason) scan live in the store — so the whole
// seam is real: pgtest Postgres, the production run + chained audit
// repositories, and a Server built over them with no issue notifier.

// retirementDropPGFixture is the pg-backed seam.
type retirementDropPGFixture struct {
	s     *Server
	audit audit.Repository
	runID uuid.UUID
	accID uuid.UUID
}

// newRetirementDropPGFixture seeds: a running run, a plan stage driven to
// succeeded, an acceptance stage left PENDING (never spawned, so the cancel
// writer records), and an approval_submitted row on the plan stage carrying
// ONE retire_scenario entry so approvedScenarioRetirements returns it.
func newRetirementDropPGFixture(t *testing.T) *retirementDropPGFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo})

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(autoDriveAcceptanceSpecYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, r.ID, run.StateRunning); err != nil {
		t.Fatalf("run -> running: %v", err)
	}
	plan, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create plan stage: %v", err)
	}
	plan = driveStageTo(t, runRepo, plan, run.StageStateSucceeded)
	acc, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: run.StageTypeAcceptance,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create acceptance stage: %v", err)
	}

	planID := plan.ID
	payload, _ := json.Marshal(map[string]any{
		"stage_id": planID.String(), "decision": "approve",
		"retired_scenarios": cancelDropRetired,
	})
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: r.ID, StageID: &planID, Timestamp: time.Now().UTC(),
		Category: "approval_submitted", Payload: payload,
	}); err != nil {
		t.Fatalf("seed approval_submitted: %v", err)
	}
	return &retirementDropPGFixture{s: s, audit: auditRepo, runID: r.ID, accID: acc.ID}
}

// dropRows reads the committed acceptance_scenario_retirement_dropped rows.
func (f *retirementDropPGFixture) dropRows(t *testing.T) []*audit.Entry {
	t.Helper()
	rows, err := f.audit.ListForRunByCategory(context.Background(), f.runID, CategoryAcceptanceScenarioRetirementDropped)
	if err != nil {
		t.Fatalf("list drop rows: %v", err)
	}
	return rows
}

// TestRecordRetirementsDroppedOnCancel_PG_CapabilityWired guards the SEAM: the
// production audit repository must type-assert to audit.DedupedChainAppender,
// so the concurrent test below cannot silently run appendRetirementDropOnce's
// non-atomic fallback leg and pass by luck (operator condition 2).
func TestRecordRetirementsDroppedOnCancel_PG_CapabilityWired(t *testing.T) {
	f := newRetirementDropPGFixture(t)
	if _, ok := f.s.cfg.AuditRepo.(audit.DedupedChainAppender); !ok {
		t.Fatalf("audit repository %T does not implement audit.DedupedChainAppender — the concurrent test would exercise the fallback", f.s.cfg.AuditRepo)
	}
}

// TestRecordRetirementsDroppedOnCancel_PG_ConcurrentSinksExactlyOne is the
// issue's proof: 8 cancel sinks racing on ONE run with mixed cancel_source
// values commit EXACTLY ONE acceptance_scenario_retirement_dropped row, stamped
// on the acceptance stage with reason run_cancelled_before_acceptance. Before
// #3439 each sink listed the category OUTSIDE the append transaction, so two
// could each pass the (stage_id, reason) scan and each append. Counterfactual:
// force appendRetirementDropOnce onto its fallback leg (skip the type-assert)
// → more than one row (an 8-way race may not always interleave; the plan
// requires reporting the empirical result at n=16 too before concluding).
func TestRecordRetirementsDroppedOnCancel_PG_ConcurrentSinksExactlyOne(t *testing.T) {
	f := newRetirementDropPGFixture(t)
	sources := []string{cancelSourceOperator, cancelSourceRunBudget, cancelSourceStageBudget, cancelSourceStageCancelled}
	const n = 8

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			// The writer is best-effort and t-free by construction: it logs
			// and returns, never panics or fails the test from a goroutine.
			f.s.recordAcceptanceRetirementsDroppedOnCancel(context.Background(), f.runID, src)
		}(sources[i%len(sources)])
	}
	wg.Wait()

	rows := f.dropRows(t)
	if len(rows) != 1 {
		t.Fatalf("committed %s rows = %d, want EXACTLY 1 across %d racing cancel sinks", CategoryAcceptanceScenarioRetirementDropped, len(rows), n)
	}
	if rows[0].StageID == nil || *rows[0].StageID != f.accID {
		t.Errorf("row stage = %v, want the acceptance stage %s", rows[0].StageID, f.accID)
	}
	var payload struct {
		Reason      string   `json:"reason"`
		ScenarioIDs []string `json:"scenario_ids"`
	}
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v\n%s", err, rows[0].Payload)
	}
	if payload.Reason != acceptanceRetirementDropReasonRunCancelled {
		t.Errorf("reason = %q, want %q", payload.Reason, acceptanceRetirementDropReasonRunCancelled)
	}
	if len(payload.ScenarioIDs) != 1 || payload.ScenarioIDs[0] != cancelDropRetired[0].ID {
		t.Errorf("scenario_ids = %v, want [%s]", payload.ScenarioIDs, cancelDropRetired[0].ID)
	}
}
