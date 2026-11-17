package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

type reviewInitInterleaveRepo struct {
	run.Repository
	once sync.Once
	park func() error
	err  error
}

func (r *reviewInitInterleaveRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	r.once.Do(func() { r.err = r.park() })
	if r.err != nil {
		return nil, r.err
	}
	return r.Repository.GetRun(ctx, id)
}

// Another publisher parks review after the HTTP replay's eligibility read,
// before the orchestrator re-reads stages. A general Advance would select the
// later pending stage. This deterministic interleaving needs no timing sleeps.
func TestHumanReviewFirst_ConcurrentReplayCannotDispatchLater(t *testing.T) {
	s, rr, rid, sid := newReviewFirstFixture(t, []byte(outsideReviewSpec), "outside_review", true)
	rr.failStage = run.StageStateDispatched
	if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusInternalServerError {
		t.Fatalf("setup=%d: %s", w.Code, w.Body.String())
	}
	rr.failStage = ""
	later, err := rr.CreateStage(context.Background(), run.CreateStageParams{RunID: rid, Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Orchestrator.Runs = &reviewInitInterleaveRepo{Repository: rr.Repository, park: func() error {
		for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateAwaitingApproval} {
			if _, err := rr.Repository.TransitionStage(context.Background(), sid, to, nil); err != nil {
				return err
			}
		}
		return nil
	}}
	if w := reviewFirstUpload(t, s, rid, sid, reviewFirstPRBytes(t)); w.Code != http.StatusOK {
		t.Fatalf("replay=%d: %s", w.Code, w.Body.String())
	}
	got, err := rr.GetStage(context.Background(), later.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != run.StageStatePending {
		t.Fatalf("concurrent replay dispatched later stage: %s", got.State)
	}
	assertReviewFirstState(t, rr, rid, sid, run.StateRunning, run.StageStateAwaitingApproval)
}

func TestHumanReviewFirst_BoundedInitializerDoesNotStartOtherShapes(t *testing.T) {
	specBytes := []byte(strings.Replace(outsideReviewSpec, "human: true", "agent: claude-code", 1))
	_, rr, rid, sid := newReviewFirstFixture(t, specBytes, "outside_review", false)
	orch := &orchestrator.Orchestrator{Runs: rr}
	if _, err := orch.InitializeHumanReviewFirst(context.Background(), rid); err != nil {
		t.Fatal(err)
	}
	assertReviewFirstState(t, rr, rid, sid, run.StatePending, run.StageStatePending)
	empty, err := rr.CreateRun(context.Background(), run.CreateRunParams{Repo: "other/widgets", WorkflowID: "outside_review", WorkflowSHA: "fixture", TriggerSource: run.TriggerCLI, WorkflowSpec: []byte(outsideReviewSpec)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orch.InitializeHumanReviewFirst(context.Background(), empty.ID); err != nil {
		t.Fatal(err)
	}
	got, err := rr.GetRun(context.Background(), empty.ID)
	if err != nil || got.State != run.StatePending {
		t.Fatalf("empty run=%v error=%v", got, err)
	}
}
