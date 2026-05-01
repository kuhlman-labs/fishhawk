package run

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrNotFound signals a missing run or stage. The Postgres adapter
// translates pgx.ErrNoRows into this; callers can errors.Is against
// it without depending on the database driver.
var ErrNotFound = errors.New("not found")

// CreateRunParams are the inputs needed to insert a new run.
type CreateRunParams struct {
	Repo          string
	WorkflowID    string
	WorkflowSHA   string
	TriggerSource TriggerSource
	TriggerRef    *string
}

// CreateStageParams are the inputs needed to insert a new stage.
type CreateStageParams struct {
	RunID        uuid.UUID
	Sequence     int
	Type         StageType
	ExecutorKind ExecutorKind
	ExecutorRef  string
}

// StageCompletion captures the optional metadata that accompanies a
// terminal-state stage transition. FailureCategory and FailureReason
// MUST be set when transitioning to StageStateFailed; both MUST be
// nil otherwise.
type StageCompletion struct {
	FailureCategory *FailureCategory
	FailureReason   *string
}

// Repository persists runs and stages and applies state-machine
// transitions atomically.
//
// Implementations MUST guarantee that two concurrent transition
// calls observing the same prior state cannot both succeed. The
// Postgres adapter does this with row-level SELECT … FOR UPDATE
// inside a transaction; in-memory test fakes use a mutex.
type Repository interface {
	CreateRun(ctx context.Context, p CreateRunParams) (*Run, error)
	GetRun(ctx context.Context, id uuid.UUID) (*Run, error)

	// TransitionRun moves a run to the target state. Returns
	// InvalidTransitionError if the run is in a state from which
	// the target is not reachable. Same-state (idempotent) calls
	// return the unchanged run.
	TransitionRun(ctx context.Context, id uuid.UUID, to State) (*Run, error)

	CreateStage(ctx context.Context, p CreateStageParams) (*Stage, error)
	GetStage(ctx context.Context, id uuid.UUID) (*Stage, error)
	ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*Stage, error)

	// TransitionStage moves a stage to the target state. completion
	// must be non-nil and populated when transitioning to
	// StageStateFailed; nil otherwise.
	TransitionStage(ctx context.Context, id uuid.UUID, to StageState, completion *StageCompletion) (*Stage, error)
}
