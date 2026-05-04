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
	Repo           string
	WorkflowID     string
	WorkflowSHA    string
	TriggerSource  TriggerSource
	TriggerRef     *string
	InstallationID *int64
	// IdempotencyKey, when non-nil, makes the create idempotent
	// against (Repo, *IdempotencyKey): a duplicate insert
	// returns the existing row instead of failing on the unique
	// constraint. Webhook-driven creates leave this nil; the
	// receiver dedups via X-GitHub-Delivery upstream.
	IdempotencyKey *string
}

// CreateStageParams are the inputs needed to insert a new stage.
type CreateStageParams struct {
	RunID        uuid.UUID
	Sequence     int
	Type         StageType
	ExecutorKind ExecutorKind
	ExecutorRef  string
	// GateSLA is the gate's `sla` string from the workflow spec at
	// dispatch time, e.g. "4_business_hours". Nil when the stage's
	// gate has no SLA. The SLA ticker reads it back to detect
	// awaiting_approval timeouts.
	GateSLA *string
}

// StageCompletion captures the optional metadata that accompanies a
// terminal-state stage transition. FailureCategory and FailureReason
// MUST be set when transitioning to StageStateFailed; both MUST be
// nil otherwise.
type StageCompletion struct {
	FailureCategory *FailureCategory
	FailureReason   *string
}

// ListRunsFilter scopes a ListRuns query. Empty strings mean "no
// constraint" — same convention as the underlying SQL. Limit must
// be > 0; Offset must be >= 0.
type ListRunsFilter struct {
	Repo       string
	WorkflowID string
	State      string
	Limit      int
	Offset     int
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

	// GetRunByIdempotencyKey returns the existing run for
	// (repo, key) if one exists. Used by POST /v0/runs to
	// resolve an Idempotency-Key header to an already-created
	// run. Returns ErrNotFound when no row matches.
	GetRunByIdempotencyKey(ctx context.Context, repo, key string) (*Run, error)

	// ListRuns returns runs matching filter, ordered created_at
	// DESC with an id tiebreak. Caller is responsible for the
	// pagination math; this method just hands back the page.
	ListRuns(ctx context.Context, f ListRunsFilter) ([]*Run, error)

	// TransitionRun moves a run to the target state. Returns
	// InvalidTransitionError if the run is in a state from which
	// the target is not reachable. Same-state (idempotent) calls
	// return the unchanged run.
	TransitionRun(ctx context.Context, id uuid.UUID, to State) (*Run, error)

	CreateStage(ctx context.Context, p CreateStageParams) (*Stage, error)
	GetStage(ctx context.Context, id uuid.UUID) (*Stage, error)
	ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*Stage, error)

	// ListStagesAwaitingApproval returns every stage currently in
	// awaiting_approval with a non-null GateSLA. The SLA ticker
	// scans this to find timeout candidates without re-parsing the
	// workflow spec. Order is undefined except that it is stable
	// (Postgres adapter orders by updated_at ASC for early-exit
	// efficiency).
	ListStagesAwaitingApproval(ctx context.Context) ([]*Stage, error)

	// TransitionStage moves a stage to the target state. completion
	// must be non-nil and populated when transitioning to
	// StageStateFailed; nil otherwise.
	TransitionStage(ctx context.Context, id uuid.UUID, to StageState, completion *StageCompletion) (*Stage, error)
}
