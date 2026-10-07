package orchestrator

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Post-deploy hold (E35.1 / #1598, ADR-053). A stage sequenced after a deploy
// stage advances only once every earlier deploy SUCCEEDED. Without the hold the
// Advance stage walk records the in-flight deploy as `gated` but keeps walking
// and dispatches the first PENDING stage behind it.
//
// Counterfactual (C4): replacing DeployAheadNotSucceeded's body with
// `return nil` dispatches the acceptance stage in every in-flight row below
// (state dispatched, one acceptance_dispatched entry, one workflow_dispatch),
// and parks the second deploy at awaiting_deploy_approval in the multi-deploy
// row. The fixture's `next` is an acceptance stage, so the deploy
// pre-execution park cannot mask the deletion, and the orchestrator carries no
// Artifacts, so the acceptance short-circuit cannot either.

// deploySeed is the deploy stage row shape: deploy stages delegate, but
// the orchestrator keys only on Type, so the executor fields are inert here.
func deploySeed(state run.StageState) stageSeed {
	return stageSeed{Type: run.StageTypeDeploy, ExecutorKind: run.ExecutorAgent, ExecutorRef: "github_actions", State: state}
}

func acceptanceSeed(state run.StageState) stageSeed {
	return stageSeed{Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: state}
}

func countAcceptanceDispatched(ra *recordingAudit) int {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	n := 0
	for _, p := range ra.appended {
		if p.Category == "acceptance_dispatched" {
			n++
		}
	}
	return n
}

func TestAdvance_DeployHold_InFlightDeploy_HoldsAcceptancePending(t *testing.T) {
	for _, deployState := range []run.StageState{
		run.StageStateAwaitingDeployApproval,
		run.StageStateDispatched,
		run.StageStateRunning,
		run.StageStateAwaitingDeployment,
	} {
		t.Run(string(deployState), func(t *testing.T) {
			rs := newStubRuns()
			gh := &stubGitHub{}
			ra := &recordingAudit{}
			o := &Orchestrator{Runs: rs, GitHub: gh, Audit: ra}
			r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
				deploySeed(deployState),
				acceptanceSeed(run.StageStatePending),
			})

			out, err := o.Advance(context.Background(), r.ID)
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if out != OutcomeNoOp {
				t.Errorf("Outcome = %q, want noop (acceptance held behind the %s deploy)", out, deployState)
			}
			if stages[1].State != run.StageStatePending {
				t.Errorf("acceptance stage state = %q, want pending (held behind the %s deploy)", stages[1].State, deployState)
			}
			if stages[0].State != deployState {
				t.Errorf("deploy stage state = %q, want %q (untouched)", stages[0].State, deployState)
			}
			gh.mu.Lock()
			calls := len(gh.calls)
			gh.mu.Unlock()
			if calls != 0 {
				t.Errorf("workflow_dispatch calls = %d, want 0", calls)
			}
			if n := countAcceptanceDispatched(ra); n != 0 {
				t.Errorf("acceptance_dispatched entries = %d, want 0", n)
			}
			if r.State != run.StateRunning {
				t.Errorf("run state = %q, want running (held, not completed)", r.State)
			}
		})
	}
}

func TestAdvance_DeployHold_DeploySucceeded_DispatchesAcceptance(t *testing.T) {
	rs := newStubRuns()
	gh := &stubGitHub{}
	ra := &recordingAudit{}
	o := &Orchestrator{Runs: rs, GitHub: gh, Audit: ra}
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		deploySeed(run.StageStateSucceeded),
		acceptanceSeed(run.StageStatePending),
	})

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeDispatched {
		t.Errorf("Outcome = %q, want dispatched", out)
	}
	if stages[1].State != run.StageStateDispatched {
		t.Errorf("acceptance stage state = %q, want dispatched once the deploy succeeded", stages[1].State)
	}
	if n := countAcceptanceDispatched(ra); n != 1 {
		t.Errorf("acceptance_dispatched entries = %d, want 1", n)
	}
}

func TestAdvance_DeployHold_DeployFailed_FailsRunWithoutDispatch(t *testing.T) {
	rs := newStubRuns()
	gh := &stubGitHub{}
	ra := &recordingAudit{}
	o := &Orchestrator{Runs: rs, GitHub: gh, Audit: ra}
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		deploySeed(run.StageStateFailed),
		acceptanceSeed(run.StageStatePending),
	})

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeRunCompleted {
		t.Errorf("Outcome = %q, want run_completed", out)
	}
	if r.State != run.StateFailed {
		t.Errorf("run state = %q, want failed (a failed deploy fails the run)", r.State)
	}
	if stages[1].State == run.StageStateDispatched || stages[1].State == run.StageStateAwaitingHostDispatch {
		t.Errorf("acceptance stage state = %q, must never dispatch behind a failed deploy", stages[1].State)
	}
	if n := countAcceptanceDispatched(ra); n != 0 {
		t.Errorf("acceptance_dispatched entries = %d, want 0", n)
	}
}

// Multi-deploy consequence (approval condition 4): a SECOND deploy stage
// behind an in-flight first deploy stays `pending` — the hold runs before the
// deploy pre-execution park, so it is NOT parked at awaiting_deploy_approval
// until the first deploy succeeded.
func TestAdvance_DeployHold_SecondDeployStaysPendingBehindInFlightDeploy(t *testing.T) {
	rs := newStubRuns()
	gh := &stubGitHub{}
	o := &Orchestrator{Runs: rs, GitHub: gh}
	r, stages := rs.seed(t, "x/y", int64Ptr(42), []stageSeed{
		deploySeed(run.StageStateAwaitingDeployment),
		deploySeed(run.StageStatePending),
	})

	out, err := o.Advance(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if out != OutcomeNoOp {
		t.Errorf("Outcome = %q, want noop", out)
	}
	if stages[1].State != run.StageStatePending {
		t.Errorf("second deploy stage state = %q, want pending (not parked at awaiting_deploy_approval)", stages[1].State)
	}

	// Twin: once the first deploy succeeded, the second deploy parks at its
	// own pre-execution gate as before.
	forceStageState(rs, stages[0].ID, run.StageStateSucceeded)
	if _, err := o.Advance(context.Background(), r.ID); err != nil {
		t.Fatalf("Advance after first deploy succeeded: %v", err)
	}
	if stages[1].State != run.StageStateAwaitingDeployApproval {
		t.Errorf("second deploy stage state = %q, want awaiting_deploy_approval once the first deploy succeeded", stages[1].State)
	}
}

func TestDeployAheadNotSucceeded(t *testing.T) {
	stage := func(seq int, typ run.StageType, st run.StageState) *run.Stage {
		return &run.Stage{ID: uuid.New(), Sequence: seq, Type: typ, State: st}
	}
	target := stage(2, run.StageTypeAcceptance, run.StageStatePending)
	selfDeploy := stage(0, run.StageTypeDeploy, run.StageStatePending)

	tests := []struct {
		name   string
		stages []*run.Stage
		target *run.Stage
		want   int // index into stages of the expected hit, -1 for nil
	}{
		{"no deploy stage", []*run.Stage{
			stage(0, run.StageTypePlan, run.StageStateSucceeded),
			stage(1, run.StageTypeImplement, run.StageStateRunning),
			target,
		}, target, -1},
		{"earlier deploy succeeded", []*run.Stage{
			stage(0, run.StageTypeDeploy, run.StageStateSucceeded),
			target,
		}, target, -1},
		{"earlier deploy in flight", []*run.Stage{
			stage(0, run.StageTypeDeploy, run.StageStateAwaitingDeployment),
			target,
		}, target, 0},
		{"earlier deploy failed", []*run.Stage{
			stage(0, run.StageTypeDeploy, run.StageStateFailed),
			target,
		}, target, 0},
		{"earlier deploy cancelled", []*run.Stage{
			stage(0, run.StageTypeDeploy, run.StageStateCancelled),
			target,
		}, target, 0},
		{"earlier deploy pending", []*run.Stage{
			stage(0, run.StageTypeDeploy, run.StageStatePending),
			target,
		}, target, 0},
		{"later deploy ignored", []*run.Stage{
			stage(0, run.StageTypeImplement, run.StageStateSucceeded),
			target,
			stage(3, run.StageTypeDeploy, run.StageStatePending),
		}, target, -1},
		{"first of two unsucceeded deploys returned", []*run.Stage{
			stage(0, run.StageTypeDeploy, run.StageStateSucceeded),
			stage(1, run.StageTypeDeploy, run.StageStateRunning),
			target,
		}, target, 1},
		{"target itself a deploy is not its own blocker", []*run.Stage{
			selfDeploy,
		}, selfDeploy, -1},
		{"nil target", []*run.Stage{
			stage(0, run.StageTypeDeploy, run.StageStateRunning),
		}, nil, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeployAheadNotSucceeded(tt.stages, tt.target)
			switch {
			case tt.want < 0 && got != nil:
				t.Errorf("got %s (%s), want nil", got.ID, got.State)
			case tt.want >= 0 && got != tt.stages[tt.want]:
				t.Errorf("got %v, want stages[%d]", got, tt.want)
			}
		})
	}
}
