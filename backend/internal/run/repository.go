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
	// UpstreamRunID, when non-nil, names the upstream feature_change run
	// the deploy stage's required_upstream pre-flight gate evaluates
	// (E23.11 / #1417). NOT parent_run_id (#216): a deploy-gate safety
	// pointer kept off the follow-up/lineage column so the get_plan
	// resolution walk, resume/retry recovery, and decomposition
	// provenance consumers are unaffected. Nil → the gate evaluates the
	// current run (the appended-deploy path).
	UpstreamRunID *uuid.UUID
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
	// MaxRetriesSnapshot is the workflow's
	// on_ci_failure.max_retries cap at run-create time (#280).
	// 0 means "use the migration's default" (1); callers should
	// pass the explicit value from spec parsing — see
	// dispatcher.resolveMaxRetriesForCreate.
	MaxRetriesSnapshot int
	// RunnerKind tags the execution backend that will run this run
	// (ADR-022 / #388). Empty string means "use the migration's
	// default" (RunnerKindGitHubActions); callers stamping a
	// non-GHA backend (Phase C local-runner; future K8s) pass it
	// explicitly. The backend rejects unknown values at the API
	// boundary; only callers known to be valid reach the repo.
	RunnerKind string
	// IssueContext caches the triggering GitHub issue's title,
	// body, url, and number on the run row (#415). Set by the API
	// runs handler when the CLI ships an `issue_context` body
	// alongside `workflow_spec`; the operator's `gh` CLI is the
	// source of truth on the local path. Nil for webhook-dispatched
	// runs and for non-issue triggers — the prompt builder falls
	// back to the existing GitHub fetch path in those cases.
	IssueContext *IssueContext
	// DecomposedFrom, when non-nil, identifies the parent run that
	// minted this child run during orchestrator fanout (#455).
	DecomposedFrom *uuid.UUID
	// Drive opts the run into backend auto-advancement of mechanical
	// transitions (#1023 / #996 theme 1). Resolved by the API handler
	// at run-create time — the workflow spec's `drive` default
	// overridden by the per-run POST /v0/runs field — and snapshotted
	// on the row (migration 0031) so a spec edit mid-run can't change
	// an in-flight run's advancement behavior. False means
	// operator-driven (the default at every layer).
	Drive bool
	// SliceIndex is the decomposed child's 0-based sub_plan position
	// (E24.1 / #1141 / ADR-041). Set by orchestrator fanout for each
	// minted decomposition child; nil for non-decomposed creates. The
	// runner reads it back off the prompt-fetch response to route the
	// child onto its own sole-writer slice branch.
	SliceIndex *int
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
	// RunnerKind filters by execution backend (ADR-022 / #388).
	// Empty = no constraint; equality match against
	// runs.runner_kind otherwise. Compliance consumers use this
	// to filter to `github_actions`-only reports.
	RunnerKind *string
	// DecomposedFrom filters to runs minted as children of the
	// given parent run (#455). Nil = no constraint.
	DecomposedFrom *uuid.UUID
	// AccountID scopes the listing to a tenant workspace account
	// (ADR-057 / E44.5). Empty = no constraint. When set, the query
	// keeps rows whose account_id equals it OR whose account_id is NULL
	// (untenanted rows stay visible) — the account-scoped list contract
	// the /v0/runs handler enforces against the caller's Identity.AccountID.
	AccountID string
	// ParentRunID filters to recovery children minted with this
	// parent_run_id — the resume/retry lineage loadApprovedPlanForRun
	// walks upward (#216). Nil = no constraint. DISTINCT from
	// DecomposedFrom: decomposition children carry decomposed_from,
	// recovery children carry parent_run_id; the two lineages must not
	// be conflated (#1751).
	ParentRunID *uuid.UUID
	Limit       int
	Offset      int
}

// Repository persists runs and stages and applies state-machine
// transitions atomically.
//
// Implementations MUST guarantee that two concurrent transition
// calls observing the same prior state cannot both succeed. The
// Postgres adapter does this with row-level SELECT … FOR UPDATE
// inside a transaction; in-memory test fakes use a mutex.
type Repository interface {
	// AccountGetter is a REQUIRED part of the interface (E44.11 / #2074):
	// every Repository implementation MUST be able to resolve a run's
	// owning tenant account, so no wiring can silently skip the lookup.
	AccountGetter

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

	// RetryRun is the explicit run-level reopen override off a
	// terminal state — today only failed → running per the
	// runRetryTransitions table in transition.go (#698). Reuses the
	// plain UpdateRunState query: runs carry no failure metadata to
	// clear (only stages do), so no dedicated clearing query is
	// needed. Returns InvalidTransitionError when the run is not in a
	// state from which the retry target is reachable. The high-level
	// run.RedriveChild helper calls this after validating the run is a
	// failed decomposition child.
	RetryRun(ctx context.Context, id uuid.UUID, to State) (*Run, error)

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

	// ListReviewStagesAwaitingApproval returns every review stage
	// currently in awaiting_approval, REGARDLESS of GateSLA. The merge
	// reconciler scans this to find review gates to resolve from live PR
	// state. Distinct from ListStagesAwaitingApproval — that one filters
	// to a non-null GateSLA for the SLA ticker, which would hide the
	// feature_change review gate (it carries no sla) from the reconciler
	// (#725). Ordered updated_at ASC.
	ListReviewStagesAwaitingApproval(ctx context.Context) ([]*Stage, error)

	// NOTE (#1386 / E23.6): the deploy reconciler's analogous
	// "ListDeployStagesAwaitingDeployment" (deploy stages parked at
	// awaiting_deployment) is deliberately NOT on this broad interface. It is
	// a capability only the deploy executor consumes, so it lives on the
	// concrete *postgresRepo and is reached through deployreconciler's narrow
	// DeployStageSource interface (serve.go type-asserts cfg.RunRepo to it) —
	// keeping it off run.Repository avoids forcing a stub into every
	// run.Repository test fake for a method nothing else calls. Its rollback
	// sibling "ListDeployStagesRollbackPending" (#1398 — deploy stages with a
	// deployment_rollback_initiated audit entry and no deployment_rollback_completed)
	// is off this interface for the same reason: only the deploy reconciler's
	// rollback scan consumes it, so it likewise lives on *postgresRepo and
	// reaches the reconciler through the widened DeployStageSource.

	// ListStagesAwaitingChildren returns every stage currently in
	// awaiting_children state. The child-completion sweeper scans
	// this to find parent stages whose children may have reached
	// terminal states. Ordered updated_at ASC.
	ListStagesAwaitingChildren(ctx context.Context) ([]*Stage, error)

	// ListStagesDispatched returns every stage currently in
	// 'dispatched' state. The dispatch watchdog (E8.4 #158) scans
	// this to find stages stuck past a configurable timeout — the
	// runner never reported in (action timeout, GitHub-side
	// dispatch failure, network partition). Ordered updated_at ASC.
	ListStagesDispatched(ctx context.Context) ([]*Stage, error)

	// TransitionStage moves a stage to the target state. completion
	// must be non-nil and populated when transitioning to
	// StageStateFailed; nil otherwise.
	//
	// Same-state (idempotent) calls return the unchanged stage and a
	// nil error — the row is not mutated and UpdatedAt is not bumped.
	// Racing callers (e.g. maybeAdvanceDecomposedParent from
	// concurrent child completions) rely on this: when the parent
	// stage was already advanced by the first caller, subsequent
	// callers silently no-op rather than returning an error. "No
	// change" is not surfaced as a distinct outcome.
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

// StageCASTransitioner is an OPTIONAL capability on the concrete postgres
// repo — a compare-and-swap sibling of TransitionStage that applies the
// move ONLY when the stage's row-locked current state still equals `from`.
// If another writer flipped the stage between the caller's load and this
// call, it returns StageStateChangedError and mutates nothing; otherwise
// it behaves exactly like TransitionStage (same completion validation,
// started_at/ended_at stamping, override-table union).
//
// It is deliberately kept OFF the Repository interface — mirroring the
// ResumeAwaitingInputAndAppend / ParkScopeCompletenessAndAppend /
// AddRunCost precedent — because adding a method to Repository would break
// every manually-written full-interface test fake (~20 across the
// backend). run.FailStage type-asserts this capability and drives its
// transitions through it when present (production postgresRepo), falling
// back to the plain Repository.TransitionStage path for in-memory fakes
// that do not implement it. That fallback retains the (fake-only) post-load
// race window; production always has the capability.
type StageCASTransitioner interface {
	TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to StageState, completion *StageCompletion) (*Stage, error)
}

// AccountGetter is the cheap tenant-account lookup (ADR-057 / E44.5) that
// returns just a run's account_id ("" for an untenanted NULL row, the account
// UUID string otherwise) without materializing the whole run.
//
// It is REQUIRED, not optional: Repository embeds it (E44.11 / #2074), so
// every Repository implementation must resolve a run's account. The named
// interface survives as a readable name for the capability and as the anchor
// for the `var _ run.AccountGetter = run.Repository(nil)` compile-time
// assertion that a future refactor pulling the method back off Repository must
// break.
//
// The bearer-auth mcp:run path calls it UNCONDITIONALLY — there is no
// type-assertion degrade left, so a wiring gap can no longer produce an
// accountless mcp identity (the #2074 exposure). Returning "" with a nil error
// is the untenanted happy path (empty AccountID, allowed); ANY error fails
// CLOSED with 503. BaseFake provides a stub so a fake embedding it satisfies
// Repository.
type AccountGetter interface {
	GetRunAccountID(ctx context.Context, id uuid.UUID) (string, error)
}
