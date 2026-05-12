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
	// ParentRunID, when non-nil, threads the new run as a follow-
	// up to an existing run (#216). Set by the dispatcher when a
	// fresh trigger lands for a (repo, trigger_ref) tuple that
	// already has a non-terminal run.
	ParentRunID *uuid.UUID
	// RequiredChecksSnapshot is the GitHub branch protection /
	// ruleset snapshot the dispatcher captured at run-create time
	// (#251 / ADR-017). Nil for non-dispatcher creates and for
	// CLI / UI runs in v0 — those paths don't gate on CI.
	RequiredChecksSnapshot *RequiredChecksSnapshot
	// WorkflowSpec is the raw bytes of the workflow file the
	// dispatcher fetched + validated at run-create time (#283).
	// Cached on the run row so the trace handler's policy re-
	// evaluation reads from storage instead of refetching from
	// GitHub. Nil for CLI / UI run-create paths that don't fetch
	// a spec.
	WorkflowSpec []byte
	// RetryAttempt is the new run's position in the auto-retry
	// chain (#279). 0 for original runs; parent.RetryAttempt + 1
	// for CI-failure retries (cap-enforced by the dispatcher).
	RetryAttempt int
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

	// RequiresApproval is true when the workflow-spec stage
	// definition includes any approval-typed gate. Determines the
	// trace-upload-handler's post-upload transition: true → walk
	// to awaiting_approval; false → walk to succeeded directly.
	// Per migration 0013 (#207).
	RequiresApproval bool

	// Gate captures the workflow-spec gate shape so downstream
	// surfaces (the review-stage UI, future check-state ingestion)
	// don't need to re-parse the spec. Per migration 0014 (#213).
	// Nil when the stage has no gate; otherwise carries the *first*
	// gate's type / blocking_checks / approvers (mirrors how
	// GateSLA / RequiresApproval scope to the first approval gate).
	Gate *Gate
}

// StageCompletion captures the optional metadata that accompanies a
// terminal-state stage transition. FailureCategory and FailureReason
// MUST be set when transitioning to StageStateFailed; both MUST be
// nil otherwise.
type StageCompletion struct {
	FailureCategory *FailureCategory
	FailureReason   *string
}

// ListRunsFilter scopes a ListRuns query. Empty strings / nil
// pointers mean "no constraint" — same convention as the
// underlying SQL. Limit must be > 0; Offset must be >= 0.
type ListRunsFilter struct {
	Repo       string
	WorkflowID string
	State      string
	// PullRequestURL filters to runs whose implement-stage
	// pull_request artifact landed at the given URL. Used by the
	// threaded-runs view (#216) to render every run on a PR.
	PullRequestURL *string
	// TriggerRef filters by the parsed trigger source ref (e.g.
	// "issue:42"). Used by the dispatcher to find prior runs on
	// the same issue when threading a new follow-up (#216).
	TriggerRef *string
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

	// SetRunPullRequestURL backfills the implement-stage PR URL
	// onto the run row when the pull_request artifact lands
	// (#216). Idempotent: setting the same URL twice is a no-op.
	// Returns ErrNotFound when the run doesn't exist.
	SetRunPullRequestURL(ctx context.Context, id uuid.UUID, url string) (*Run, error)

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

	// ListStagesDispatched returns every stage currently in
	// 'dispatched' state. The dispatch watchdog (E8.4 #158) scans
	// this to find stages stuck past a configurable timeout — the
	// runner never reported in (action timeout, GitHub-side
	// dispatch failure, network partition). Ordered updated_at ASC.
	ListStagesDispatched(ctx context.Context) ([]*Stage, error)

	// TransitionStage moves a stage to the target state. completion
	// must be non-nil and populated when transitioning to
	// StageStateFailed; nil otherwise.
	TransitionStage(ctx context.Context, id uuid.UUID, to StageState, completion *StageCompletion) (*Stage, error)

	// RetryStage is the explicit override path off a terminal state —
	// today only failed → awaiting_approval per the
	// stageRetryTransitions table in transition.go. Clears the
	// stage's failure_category, failure_reason, and ended_at; the
	// updated_at trigger restarts any timer keyed off it (SLA in
	// particular). The high-level run.RetryStage helper calls this
	// after deciding the retry is applicable; direct callers must
	// already hold that decision.
	RetryStage(ctx context.Context, id uuid.UUID, to StageState) (*Stage, error)
}
