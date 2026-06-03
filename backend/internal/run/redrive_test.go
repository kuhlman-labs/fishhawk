package run_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// redriveRepo is a small in-memory run.Repository that, unlike
// memRepo, stores both runs and stages so RedriveChild's run-level
// validation + reopen can be exercised end to end. It embeds BaseFake
// so only the methods RedriveChild touches need real behaviour.
type redriveRepo struct {
	run.BaseFake
	run    *run.Run
	stages []*run.Stage
}

func (r *redriveRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if r.run == nil || r.run.ID != id {
		return nil, run.ErrNotFound
	}
	cp := *r.run
	return &cp, nil
}

func (r *redriveRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	out := make([]*run.Stage, 0, len(r.stages))
	for _, s := range r.stages {
		if s.RunID == runID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (r *redriveRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	for _, s := range r.stages {
		if s.ID == id {
			cp := *s
			return &cp, nil
		}
	}
	return nil, run.ErrNotFound
}

func (r *redriveRepo) RetryStage(_ context.Context, id uuid.UUID, to run.StageState) (*run.Stage, error) {
	for _, s := range r.stages {
		if s.ID != id {
			continue
		}
		if !run.ValidStageRetryTransition(s.State, to) {
			return nil, run.InvalidTransitionError{Kind: "stage", From: string(s.State), To: string(to)}
		}
		s.State = to
		s.FailureCategory = nil
		s.FailureReason = nil
		s.EndedAt = nil
		cp := *s
		return &cp, nil
	}
	return nil, run.ErrNotFound
}

func (r *redriveRepo) RetryRun(_ context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	if r.run == nil || r.run.ID != id {
		return nil, run.ErrNotFound
	}
	if !run.ValidRunRetryTransition(r.run.State, to) {
		return nil, run.InvalidTransitionError{Kind: "run", From: string(r.run.State), To: string(to)}
	}
	r.run.State = to
	cp := *r.run
	return &cp, nil
}

// failedChild builds a failed decomposition child run with a single
// failed implement stage in the given category.
func failedChild(cat run.FailureCategory, reason string) *redriveRepo {
	parent := uuid.New()
	runID := uuid.New()
	now := time.Now().UTC()
	catCopy := cat
	reasonCopy := reason
	return &redriveRepo{
		run: &run.Run{
			ID:             runID,
			Repo:           "kuhlman-labs/fishhawk",
			WorkflowID:     "feature_change",
			State:          run.StateFailed,
			DecomposedFrom: &parent,
		},
		stages: []*run.Stage{{
			ID:              uuid.New(),
			RunID:           runID,
			Type:            run.StageTypeImplement,
			State:           run.StageStateFailed,
			FailureCategory: &catCopy,
			FailureReason:   &reasonCopy,
			EndedAt:         &now,
		}},
	}
}

func TestRedriveChild_HappyPath(t *testing.T) {
	repo := failedChild(run.FailureC, "runner OOM")

	dec, err := run.RedriveChild(context.Background(), repo, repo.run.ID)
	if err != nil {
		t.Fatalf("RedriveChild: %v", err)
	}
	if dec.PriorCategory != run.FailureC {
		t.Errorf("PriorCategory = %q, want C", dec.PriorCategory)
	}
	if dec.PriorReason != "runner OOM" {
		t.Errorf("PriorReason = %q", dec.PriorReason)
	}
	if dec.Stage.State != run.StageStatePending {
		t.Errorf("implement stage = %q, want pending", dec.Stage.State)
	}
	if dec.Stage.FailureCategory != nil || dec.Stage.FailureReason != nil {
		t.Errorf("stage still carries failure metadata: %+v", dec.Stage)
	}
	if dec.Run.State != run.StateRunning {
		t.Errorf("run = %q, want running (un-terminal so Advance can re-dispatch)", dec.Run.State)
	}
}

func TestRedriveChild_RejectsNonDecomposedRun(t *testing.T) {
	repo := failedChild(run.FailureC, "runner OOM")
	repo.run.DecomposedFrom = nil // not a child

	_, err := run.RedriveChild(context.Background(), repo, repo.run.ID)
	if !errors.Is(err, run.ErrRedriveNotApplicable) {
		t.Fatalf("err = %v, want ErrRedriveNotApplicable", err)
	}
}

func TestRedriveChild_RejectsNonFailedRun(t *testing.T) {
	repo := failedChild(run.FailureC, "runner OOM")
	repo.run.State = run.StateRunning // already non-terminal

	_, err := run.RedriveChild(context.Background(), repo, repo.run.ID)
	if !errors.Is(err, run.ErrRedriveNotApplicable) {
		t.Fatalf("err = %v, want ErrRedriveNotApplicable", err)
	}
}

func TestRedriveChild_RejectsNoFailedImplementStage(t *testing.T) {
	repo := failedChild(run.FailureC, "runner OOM")
	repo.stages[0].State = run.StageStateSucceeded // no failed implement

	_, err := run.RedriveChild(context.Background(), repo, repo.run.ID)
	if !errors.Is(err, run.ErrRedriveNotApplicable) {
		t.Fatalf("err = %v, want ErrRedriveNotApplicable", err)
	}
}

func TestRedriveChild_NotFound(t *testing.T) {
	repo := failedChild(run.FailureC, "runner OOM")

	_, err := run.RedriveChild(context.Background(), repo, uuid.New())
	if !errors.Is(err, run.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
