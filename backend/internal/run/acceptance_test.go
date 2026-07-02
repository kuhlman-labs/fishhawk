package run

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// TestStageTypeAcceptance pins the acceptance stage-type wire value
// (ADR-049 / #1519). The constant and migration 0044 must ship together —
// the value here is the exact literal migration 0044 widens
// stages_type_check to admit, cross-checked end-to-end by
// TestPostgres_AcceptanceStage_PersistRoundTrip.
func TestStageTypeAcceptance(t *testing.T) {
	if StageTypeAcceptance != "acceptance" {
		t.Errorf("StageTypeAcceptance = %q, want %q", StageTypeAcceptance, "acceptance")
	}
}

// reopenFake is an in-package run.Repository fake for the ReopenAcceptanceStage
// unit tests (E31.8 / #1536). It embeds BaseFake so only the three methods the
// verb exercises are overridden. TransitionStage validates the fix-up override
// edge exactly as the postgres + memRepo adapters do, so a refused succeeded →
// pending edge would fail loud rather than silently succeed.
type reopenFake struct {
	BaseFake
	stages   map[uuid.UUID]*Stage
	runState State
	transErr map[uuid.UUID]error
}

func newReopenFake(runState State, stages ...*Stage) *reopenFake {
	m := map[uuid.UUID]*Stage{}
	for _, s := range stages {
		m[s.ID] = s
	}
	return &reopenFake{stages: m, runState: runState, transErr: map[uuid.UUID]error{}}
}

func (f *reopenFake) GetStage(_ context.Context, id uuid.UUID) (*Stage, error) {
	s, ok := f.stages[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *reopenFake) GetRun(_ context.Context, id uuid.UUID) (*Run, error) {
	return &Run{ID: id, State: f.runState}, nil
}

func (f *reopenFake) TransitionStage(_ context.Context, id uuid.UUID, to StageState, _ *StageCompletion) (*Stage, error) {
	if err := f.transErr[id]; err != nil {
		return nil, err
	}
	s, ok := f.stages[id]
	if !ok {
		return nil, ErrNotFound
	}
	if s.State == to {
		cp := *s
		return &cp, nil
	}
	if !ValidStageTransition(s.State, to) && !ValidStageFixupTransition(s.State, to) {
		return nil, InvalidTransitionError{Kind: "stage", From: string(s.State), To: string(to)}
	}
	s.State = to
	cp := *s
	return &cp, nil
}

func acceptanceStageFixture(runState State, state StageState) (*reopenFake, *Stage) {
	stage := &Stage{ID: uuid.New(), RunID: uuid.New(), Type: StageTypeAcceptance, State: state}
	return newReopenFake(runState, stage), stage
}

func TestReopenAcceptanceStage_HappyPath(t *testing.T) {
	repo, stage := acceptanceStageFixture(StateRunning, StageStateSucceeded)

	dec, err := ReopenAcceptanceStage(context.Background(), repo, stage.ID)
	if err != nil {
		t.Fatalf("ReopenAcceptanceStage: %v", err)
	}
	if dec.PriorState != StageStateSucceeded {
		t.Errorf("PriorState = %q, want succeeded", dec.PriorState)
	}
	if dec.Stage.State != StageStatePending {
		t.Errorf("post-reopen state = %q, want pending", dec.Stage.State)
	}
}

func TestReopenAcceptanceStage_RefusesWrongType(t *testing.T) {
	stage := &Stage{ID: uuid.New(), RunID: uuid.New(), Type: StageTypeImplement, State: StageStateSucceeded}
	repo := newReopenFake(StateRunning, stage)

	_, err := ReopenAcceptanceStage(context.Background(), repo, stage.ID)
	if !errors.Is(err, ErrAcceptanceReopenNotApplicable) {
		t.Fatalf("err = %v, want ErrAcceptanceReopenNotApplicable", err)
	}
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != StageStateSucceeded {
		t.Errorf("state = %q, want unchanged (succeeded)", cur.State)
	}
}

func TestReopenAcceptanceStage_RefusesNonSucceeded(t *testing.T) {
	// A stage-level FAILURE keeps riding the retry path — a failed acceptance
	// stage is NOT re-openable via this verb.
	repo, stage := acceptanceStageFixture(StateRunning, StageStateFailed)

	_, err := ReopenAcceptanceStage(context.Background(), repo, stage.ID)
	if !errors.Is(err, ErrAcceptanceReopenNotApplicable) {
		t.Fatalf("err = %v, want ErrAcceptanceReopenNotApplicable", err)
	}
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != StageStateFailed {
		t.Errorf("state = %q, want unchanged (failed)", cur.State)
	}
}

func TestReopenAcceptanceStage_RefusesTerminalRun(t *testing.T) {
	repo, stage := acceptanceStageFixture(StateSucceeded, StageStateSucceeded) // terminal run

	_, err := ReopenAcceptanceStage(context.Background(), repo, stage.ID)
	if !errors.Is(err, ErrAcceptanceReopenNotApplicable) {
		t.Fatalf("err = %v, want ErrAcceptanceReopenNotApplicable", err)
	}
	cur, _ := repo.GetStage(context.Background(), stage.ID)
	if cur.State != StageStateSucceeded {
		t.Errorf("state = %q, want unchanged (succeeded)", cur.State)
	}
}

// TestReopenAcceptanceStage_Postgres_HappyPath proves the succeeded → pending
// transition is admitted at the repo layer (the stageFixupTransitions edge is
// keyed by state, not stage type, so it accepts the acceptance-stage row)
// against the real pgtest-backed repository.
func TestReopenAcceptanceStage_Postgres_HappyPath(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()

	r, err := repo.CreateRun(ctx, CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := repo.TransitionRun(ctx, r.ID, StateRunning); err != nil {
		t.Fatalf("transition run → running: %v", err)
	}

	stage, err := repo.CreateStage(ctx, CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         StageTypeAcceptance,
		ExecutorKind: ExecutorAgent,
		ExecutorRef:  "claude-code",
	})
	if err != nil {
		t.Fatalf("create acceptance stage: %v", err)
	}
	for _, to := range []StageState{StageStateDispatched, StageStateRunning, StageStateSucceeded} {
		if _, err := repo.TransitionStage(ctx, stage.ID, to, nil); err != nil {
			t.Fatalf("transition acceptance stage → %s: %v", to, err)
		}
	}

	dec, err := ReopenAcceptanceStage(ctx, repo, stage.ID)
	if err != nil {
		t.Fatalf("ReopenAcceptanceStage: %v", err)
	}
	if dec.Stage.State != StageStatePending {
		t.Errorf("returned state = %q, want pending", dec.Stage.State)
	}
	got, err := repo.GetStage(ctx, stage.ID)
	if err != nil {
		t.Fatalf("get stage: %v", err)
	}
	if got.State != StageStatePending {
		t.Errorf("read-back state = %q, want pending", got.State)
	}
}
