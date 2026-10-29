package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// perRunParkAudit is a parkAudit whose ListForRun returns ONLY the entries
// recorded on the requested run (#4426 R7). parkAudit.ListForRun ignores its
// runID, so a parent's plan_decomposed row would otherwise leak into a
// child's park history and make the child read as a decomposed parent.
type perRunParkAudit struct {
	*parkAudit
}

func (p *perRunParkAudit) ListForRun(ctx context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	all, err := p.parkAudit.ListForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	var out []*audit.Entry
	for _, e := range all {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

// rowsOn returns the appended rows of category recorded on runID's chain.
func (p *perRunParkAudit) rowsOn(runID uuid.UUID, category string) []audit.ChainAppendParams {
	var out []audit.ChainAppendParams
	for _, a := range p.rows(category) {
		if a.RunID == runID {
			out = append(out, a)
		}
	}
	return out
}

// retryRuns adds the GetStage + RetryStage pair run.RetryStage needs to the
// shared stubRuns fake, so a test can drive the operator retry of a parked
// stage through the real run.RetryStage decision.
type retryRuns struct {
	*stubRuns
}

func (r *retryRuns) findStage(id uuid.UUID) *run.Stage {
	for _, list := range r.stages {
		for _, st := range list {
			if st.ID == id {
				return st
			}
		}
	}
	return nil
}

func (r *retryRuns) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.findStage(id); st != nil {
		return st, nil
	}
	return nil, run.ErrNotFound
}

func (r *retryRuns) RetryStage(_ context.Context, id uuid.UUID, to run.StageState) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.findStage(id)
	if st == nil {
		return nil, run.ErrNotFound
	}
	if !run.ValidStageRetryTransition(st.State, to) {
		return nil, run.InvalidTransitionError{Kind: "stage", From: string(st.State), To: string(to)}
	}
	st.State = to
	st.EndedAt = nil
	st.SelfRetryCount++
	return st, nil
}

const implInfraReason = "runner_lost"

// seedImplementInfraFailure seeds the feature_change shape with the implement
// stage failed category C on its first execution: plan succeeded, implement
// failed C (EndedAt set), acceptance (agent) pending, review (human) pending.
func seedImplementInfraFailure(t *testing.T, rs *stubRuns) (*run.Run, []*run.Stage) {
	t.Helper()
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateFailed},
		{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, State: run.StageStatePending},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStatePending},
	})
	markInfraFailedC(stages[1], implInfraReason)
	return r, stages
}

func markInfraFailedC(st *run.Stage, reason string) {
	cat := run.FailureC
	ended := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st.State = run.StageStateFailed
	st.FailureCategory = &cat
	st.FailureReason = &reason
	st.EndedAt = &ended
}

func newImplementParkOrchestrator(t *testing.T) (*Orchestrator, *stubRuns, *perRunParkAudit) {
	t.Helper()
	o, rs, au := newParkOrchestrator(t)
	pr := &perRunParkAudit{parkAudit: au}
	o.Audit = pr
	return o, rs, pr
}

// assertRunFailedNoRows pins today's fail-the-run path: run_completed, the
// run failed, and ZERO audit rows appended (no park, no budget row).
func assertRunFailedNoRows(t *testing.T, rs *stubRuns, au *perRunParkAudit, r *run.Run, out Outcome, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeRunCompleted || rs.runs[r.ID].State != run.StateFailed {
		t.Fatalf("Outcome = %q, run state = %q; want run_completed + failed", out, rs.runs[r.ID].State)
	}
	if n := len(au.rows(run.CategoryStageInfraParked)); n != 0 {
		t.Fatalf("stage_infra_parked rows = %d, want 0", n)
	}
	if n := len(au.appended); n != 0 {
		t.Fatalf("audit rows = %d, want 0", n)
	}
}

// TestAdvance_ImplementInfraFailure_ParksRun (T-ORCH-1, #4426 done-means 1):
// a first-execution category-C implement failure parks the run — Advance is a
// no-op twice, the run stays running, the implement stage stays failed/C,
// exactly one implement-scoped stage_infra_parked row exists, and nothing
// downstream (acceptance) is dispatched.
func TestAdvance_ImplementInfraFailure_ParksRun(t *testing.T) {
	o, rs, au := newImplementParkOrchestrator(t)
	r, stages := seedImplementInfraFailure(t, rs)
	impl, acc := stages[1], stages[2]

	for i := 0; i < 2; i++ {
		out, err := o.Advance(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if out != OutcomeNoOp {
			t.Fatalf("call %d: Outcome = %q, want noop", i, out)
		}
	}
	if got := rs.runs[r.ID].State; got != run.StateRunning {
		t.Fatalf("run state = %q, want running (parked)", got)
	}
	if impl.State != run.StageStateFailed || impl.FailureCategory == nil || *impl.FailureCategory != run.FailureC {
		t.Fatalf("implement = %q / %v, want failed / C", impl.State, impl.FailureCategory)
	}
	if acc.State != run.StageStatePending {
		t.Fatalf("acceptance state = %q, want pending (nothing downstream dispatched)", acc.State)
	}
	parks := au.rows(run.CategoryStageInfraParked)
	if len(parks) != 1 {
		t.Fatalf("stage_infra_parked rows = %d, want 1", len(parks))
	}
	if parks[0].StageID == nil || *parks[0].StageID != impl.ID {
		t.Fatalf("park row not scoped to the implement stage: %+v", parks[0].StageID)
	}
	var p map[string]any
	if err := json.Unmarshal(parks[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["stage_type"] != "implement" || p["park_ordinal"] != float64(1) || p["failure_reason"] != implInfraReason {
		t.Fatalf("park payload = %v", p)
	}
}

// TestAdvance_ImplementInfraFailure_BudgetExhaustedFailsRun (T-ORCH-2): three
// earlier-episode park rows fail the run with one budget-exhausted row. The
// twin adds an acceptance_outcome_recorded row scoped to the ACCEPTANCE stage:
// it never resets the implement stage's lifetime count.
func TestAdvance_ImplementInfraFailure_BudgetExhaustedFailsRun(t *testing.T) {
	for _, withOutcome := range []bool{false, true} {
		t.Run(fmt.Sprintf("acceptance_outcome_on_acceptance=%v", withOutcome), func(t *testing.T) {
			o, rs, au := newImplementParkOrchestrator(t)
			r, stages := seedImplementInfraFailure(t, rs)
			impl := stages[1]
			impl.SelfRetryCount = 3
			for i := 0; i < run.InfraFailureParkRetryBudget; i++ {
				payload, _ := json.Marshal(map[string]any{"failed_at": fmt.Sprintf("2026-10-29T0%d:00:00Z", i), "retry_ordinal": i})
				au.seeded = append(au.seeded, &audit.Entry{Sequence: int64(2*i + 1), RunID: &r.ID, StageID: &impl.ID, Category: run.CategoryStageInfraParked, Payload: payload})
				if withOutcome {
					au.seeded = append(au.seeded, &audit.Entry{Sequence: int64(2*i + 2), RunID: &r.ID, StageID: &stages[2].ID, Category: "acceptance_outcome_recorded"})
				}
			}
			out, err := o.Advance(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if out != OutcomeRunCompleted || rs.runs[r.ID].State != run.StateFailed {
				t.Fatalf("Outcome = %q, run state = %q; want run_completed + failed", out, rs.runs[r.ID].State)
			}
			rows := au.rows(run.CategoryStageInfraParkBudgetExhausted)
			if len(rows) != 1 {
				t.Fatalf("budget-exhausted rows = %d, want 1", len(rows))
			}
			var p map[string]any
			if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p["prior_parks"] != float64(3) || p["stage_type"] != "implement" {
				t.Fatalf("budget payload = %v", p)
			}
			if n := len(au.rows(run.CategoryStageInfraParked)); n != 0 {
				t.Fatalf("stage_infra_parked rows = %d, want 0", n)
			}
		})
	}
}

// TestAdvance_DecomposedParentFanInFailure_StillFailsRun (T-ORCH-3): the ONLY
// difference from T-ORCH-1's fixture is the run's plan_decomposed row — a
// decomposed parent's fan-in failure is not an infrastructure park.
func TestAdvance_DecomposedParentFanInFailure_StillFailsRun(t *testing.T) {
	o, rs, au := newImplementParkOrchestrator(t)
	r, stages := seedImplementInfraFailure(t, rs)
	markInfraFailedC(stages[1], "one or more decomposed child runs failed")
	au.seeded = []*audit.Entry{{Sequence: 1, RunID: &r.ID, StageID: &stages[1].ID, Category: "plan_decomposed"}}

	out, err := o.Advance(context.Background(), r.ID)
	assertRunFailedNoRows(t, rs, au, r, out, err)
}

// TestAdvance_DecomposedChildImplementInfraFailure_ParentWaitsThenResumes
// (T-ORCH-4, #4426 done-means 2): a decomposition child whose implement stage
// fails category C parks; its parent keeps waiting in awaiting_children with
// no re-drive park and no settle; once the child is retried and succeeds, the
// parent's fan-in settles succeeded. The per-run audit view keeps the
// parent's plan_decomposed row out of the child's history (R7).
func TestAdvance_DecomposedChildImplementInfraFailure_ParentWaitsThenResumes(t *testing.T) {
	o, rs, au := newImplementParkOrchestrator(t)
	rr := &retryRuns{stubRuns: rs}
	o.Runs = rr

	parent, parentStages := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
		{Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStatePending},
	})
	au.seeded = []*audit.Entry{{Sequence: 1, RunID: &parent.ID, StageID: &parentStages[1].ID, Category: "plan_decomposed"}}

	c1, _ := rs.seed(t, "x/y", nil, nil)
	c1.DecomposedFrom = &parent.ID
	c1.State = run.StateSucceeded

	c2, c2Stages := rs.seed(t, "x/y", nil, []stageSeed{
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateFailed},
	})
	c2.DecomposedFrom = &parent.ID
	c2Impl := c2Stages[0]
	markInfraFailedC(c2Impl, implInfraReason)

	out, err := o.Advance(context.Background(), c2.ID)
	if err != nil {
		t.Fatalf("Advance(C2): %v", err)
	}
	if out != OutcomeNoOp || rs.runs[c2.ID].State != run.StateRunning {
		t.Fatalf("C2: Outcome = %q, run state = %q; want noop + running (parked)", out, rs.runs[c2.ID].State)
	}
	if n := len(au.rowsOn(c2.ID, run.CategoryStageInfraParked)); n != 1 {
		t.Fatalf("C2 stage_infra_parked rows = %d, want 1", n)
	}
	if parentStages[1].State != run.StageStateAwaitingChildren {
		t.Fatalf("parent implement = %q, want awaiting_children", parentStages[1].State)
	}
	for _, cat := range []string{"parent_awaiting_redrive", "children_settled"} {
		if n := len(au.rowsOn(parent.ID, cat)); n != 0 {
			t.Fatalf("parent %s rows = %d, want 0 while a child is parked", cat, n)
		}
	}

	// Operator retry: the real run.RetryStage decision re-opens the parked
	// stage to pending (C2 is not itself a decomposed parent).
	dec, err := run.RetryStage(context.Background(), rr, c2Impl.ID, run.RetryOptions{})
	if err != nil {
		t.Fatalf("RetryStage(C2 implement): %v", err)
	}
	if dec.Stage.State != run.StageStatePending || rs.runs[c2.ID].State != run.StateRunning {
		t.Fatalf("after retry: stage = %q, run = %q; want pending + running", dec.Stage.State, rs.runs[c2.ID].State)
	}
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded} {
		if _, err := rs.TransitionStage(context.Background(), c2Impl.ID, to, nil); err != nil {
			t.Fatalf("walk C2 implement -> %s: %v", to, err)
		}
	}

	out, err = o.Advance(context.Background(), c2.ID)
	if err != nil {
		t.Fatalf("Advance(C2) after retry: %v", err)
	}
	if out != OutcomeRunCompleted || rs.runs[c2.ID].State != run.StateSucceeded {
		t.Fatalf("C2 after retry: Outcome = %q, run state = %q; want run_completed + succeeded", out, rs.runs[c2.ID].State)
	}
	if parentStages[1].State != run.StageStateSucceeded {
		t.Fatalf("parent implement = %q, want succeeded (fan-in settled)", parentStages[1].State)
	}
	settled := au.rowsOn(parent.ID, "children_settled")
	if len(settled) != 1 {
		t.Fatalf("parent children_settled rows = %d, want 1", len(settled))
	}
	var p map[string]any
	if err := json.Unmarshal(settled[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["outcome"] != "succeeded" {
		t.Fatalf("children_settled payload = %v, want outcome succeeded", p)
	}
}

// TestAdvance_ImplementInfraFailure_PostMergeCompletes (T-ORCH-5): the C2
// post-merge carve-out applies to implement too.
func TestAdvance_ImplementInfraFailure_PostMergeCompletes(t *testing.T) {
	o, rs, au := newImplementParkOrchestrator(t)
	r, _ := seedImplementInfraFailure(t, rs)
	au.seeded = []*audit.Entry{{Sequence: 1, RunID: &r.ID, Category: "pr_merged"}}

	out, err := o.Advance(context.Background(), r.ID)
	assertRunFailedNoRows(t, rs, au, r, out, err)
}

// TestAdvance_ImplementInfraFailure_AuditReadErrorFailsRun (T-ORCH-6 as
// amended by #4426 approval condition C1): an unreadable history on an
// IMPLEMENT failure counts as a re-opened pass, so the run fails as today —
// never a new park — and Advance returns no error. (The acceptance arm keeps
// #4356's error-with-no-transition contract:
// TestAdvance_AcceptanceInfraFailure_AuditFailurePaths.) The nil-Audit row
// takes today's path too.
func TestAdvance_ImplementInfraFailure_AuditReadErrorFailsRun(t *testing.T) {
	t.Run("history read error", func(t *testing.T) {
		o, rs, au := newImplementParkOrchestrator(t)
		au.listErr = errors.New("audit down")
		r, stages := seedImplementInfraFailure(t, rs)
		out, err := o.Advance(context.Background(), r.ID)
		assertRunFailedNoRows(t, rs, au, r, out, err)
		if stages[2].State != run.StageStatePending {
			t.Fatalf("acceptance = %q, want pending", stages[2].State)
		}
	})
	t.Run("nil Audit", func(t *testing.T) {
		o, rs, _ := newOrchestrator(t)
		r, _ := seedImplementInfraFailure(t, rs)
		out, err := o.Advance(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if out != OutcomeRunCompleted || rs.runs[r.ID].State != run.StateFailed {
			t.Fatalf("Outcome = %q, run state = %q; want run_completed + failed", out, rs.runs[r.ID].State)
		}
	})
}

// TestAdvance_ImplementInfraFailure_ReopenedPassStillFailsRun (T-ORCH-7, R4
// and #4426 approval condition C1): a pass-trigger row, or a head-producing
// row (the durable signal that survives a lost trigger row), scoped to the
// failed implement stage keeps today's fail-the-run path. Each twin scopes
// the same row to a DIFFERENT stage, and the run parks.
func TestAdvance_ImplementInfraFailure_ReopenedPassStillFailsRun(t *testing.T) {
	categories := append(run.InfraParkReopenedPassCategories(), "pull_request_opened", "fixup_pushed", "child_pushed")
	for _, cat := range categories {
		t.Run(cat+" scoped to the implement stage", func(t *testing.T) {
			o, rs, au := newImplementParkOrchestrator(t)
			r, stages := seedImplementInfraFailure(t, rs)
			au.seeded = []*audit.Entry{{Sequence: 1, RunID: &r.ID, StageID: &stages[1].ID, Category: cat}}
			out, err := o.Advance(context.Background(), r.ID)
			assertRunFailedNoRows(t, rs, au, r, out, err)
		})
		t.Run(cat+" scoped to another stage parks", func(t *testing.T) {
			o, rs, au := newImplementParkOrchestrator(t)
			r, stages := seedImplementInfraFailure(t, rs)
			au.seeded = []*audit.Entry{{Sequence: 1, RunID: &r.ID, StageID: &stages[0].ID, Category: cat}}
			out, err := o.Advance(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if out != OutcomeNoOp || rs.runs[r.ID].State != run.StateRunning {
				t.Fatalf("Outcome = %q, run state = %q; want noop + running (parked)", out, rs.runs[r.ID].State)
			}
			if n := len(au.rows(run.CategoryStageInfraParked)); n != 1 {
				t.Fatalf("stage_infra_parked rows = %d, want 1", n)
			}
		})
	}
}
