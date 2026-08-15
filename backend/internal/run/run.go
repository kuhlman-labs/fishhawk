// Package run owns the workflow run / stage state machine and its
// persistence interface. The state machine is governed by transition
// tables defined in transition.go; concrete persistence lives in
// postgres.go but is consumed via the Repository interface so unit
// tests can substitute fakes.
//
// State enums are unexported strings (State / StageState etc.)
// rather than ints so audit log entries and JSON payloads carry
// human-readable values forever.
package run

import (
	"time"

	"github.com/google/uuid"
)

// State is the lifecycle state of a workflow run.
//
// A run is the parent record; stages are children. The run state is
// a reduction of its stages: a run becomes Running when its first
// stage dispatches, Succeeded when its final stage succeeds, Failed
// on any stage failure, and Cancelled when manually halted.
type State string

// Run states. Terminal states (Succeeded, Failed, Cancelled) admit no
// further transitions; see transition.go for the table.
const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// IsTerminal reports whether the state admits no further transitions.
func (s State) IsTerminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

// StageState is the lifecycle state of a single workflow stage.
//
// `awaiting_host_dispatch` and `dispatched` are the two halves of the
// #1912 split of the old conflated local-`dispatched` state.
// `awaiting_host_dispatch` means the backend wants this agent stage
// executed but the runner is host-spawned per ADR-024 and NO spawn
// attempt exists yet — a parked judgment awaiting a host/operator action
// (see the host-dispatch marker endpoint POST
// /v0/runs/{run_id}/stages/{stage_id}/host-dispatch, a sibling slice).
// `dispatched` now unambiguously means a spawn attempt EXISTS — either
// the workflow_dispatch event has been sent to GitHub (the
// github_actions path) or a host spawn was marked (the local path) — but
// the runner has not yet checked in. `running` means the
// runner has started executing. `awaiting_approval` means a gate is
// blocking on human action. `awaiting_input` means the stage parked
// for operator direction — the planner emitted a clarification_request
// (#1057) and the run resumes in place once the answers arrive; it is a
// parked judgment, not a failure. `awaiting_scope_decision` means the
// implement stage parked for an operator exempt-or-fail decision: its
// ONLY committed-tree gate failure was the scope-completeness "missing
// declared scope file(s)" check (#1151), the verified commit is already
// held on the run branch, and the operator decides in-band whether to
// accept the already-committed tree (exempt, zero agent re-run) or fail
// it (category-B) per #1231. Like awaiting_input it is a parked judgment,
// not a failure.
//
// `awaiting_deploy_approval` and `awaiting_deployment` are the deploy
// stage's two non-terminal states (ADR-038 / #1384). They invert the plan/
// review post-hoc-gate model: a deploy stage's effect IS the side effect, so
// its gate is PRE-execution. `awaiting_deploy_approval` is the pre-execution
// gate park — the stage waits for an operator to approve the deploy INTENT
// before anything ships, so (like awaiting_approval) it is a parked judgment
// settled by operator action. `awaiting_deployment` is the in-flight state
// AFTER approval: the executor is polling the external delegating pipeline,
// so (like dispatched/running) it is NOT awaiting operator action and is
// deliberately excluded from IsSettled.
type StageState string

// Stage states. Terminal states (Succeeded, Failed, Cancelled) admit
// no further transitions; see transition.go for the table.
const (
	StageStatePending StageState = "pending"
	// StageStateAwaitingHostDispatch is the parked-for-host-spawn state (#1912):
	// the backend wants this agent stage executed but the runner is host-spawned
	// per ADR-024 and no spawn attempt exists yet. It is written in exactly one
	// place (orchestrator.dispatchStage, a sibling slice) for a runner_kind-
	// locked-local run and cleared by the host-dispatch marker
	// (awaiting_host_dispatch → dispatched). Settled (a parked judgment awaiting
	// operator/host action, mirroring awaiting_approval), never terminal.
	StageStateAwaitingHostDispatch  StageState = "awaiting_host_dispatch"
	StageStateDispatched            StageState = "dispatched"
	StageStateRunning               StageState = "running"
	StageStateAwaitingApproval      StageState = "awaiting_approval"
	StageStateAwaitingChildren      StageState = "awaiting_children"
	StageStateAwaitingInput         StageState = "awaiting_input"
	StageStateAwaitingScopeDecision StageState = "awaiting_scope_decision"
	// StageStateAwaitingDeployApproval is the deploy stage's pre-execution
	// gate park: the stage waits for an operator to approve the deploy
	// INTENT before anything ships (ADR-038 / #1384). Settled (awaiting
	// operator action), never terminal.
	StageStateAwaitingDeployApproval StageState = "awaiting_deploy_approval"
	// StageStateAwaitingDeployment is the deploy stage's in-flight state
	// AFTER approval: the downstream executor is polling the external
	// delegating pipeline (ADR-038 / #1384). Like dispatched/running it is
	// in-flight (NOT awaiting operator action), so it is deliberately
	// EXCLUDED from IsSettled — including it would release the stage
	// terminal-wait long-poll mid-poll (#1384, operator binding condition 2).
	StageStateAwaitingDeployment StageState = "awaiting_deployment"
	StageStateSucceeded          StageState = "succeeded"
	StageStateFailed             StageState = "failed"
	StageStateCancelled          StageState = "cancelled"
)

// IsTerminal reports whether the state admits no further transitions.
func (s StageState) IsTerminal() bool {
	switch s {
	case StageStateSucceeded, StageStateFailed, StageStateCancelled:
		return true
	default:
		return false
	}
}

// IsSettled reports whether the stage has stopped making forward
// progress on its own — it is either terminal (succeeded, failed,
// cancelled) or parked awaiting an operator action (awaiting_approval,
// awaiting_children, awaiting_input, awaiting_scope_decision,
// awaiting_deploy_approval, awaiting_host_dispatch). The in-flight states
// (pending, dispatched, running, awaiting_deployment) are NOT settled —
// awaiting_deployment is the executor polling the external pipeline, not an
// operator gate (#1384, operator binding condition 2). awaiting_host_dispatch
// IS settled — the stage needs a host/operator spawn action before it can
// proceed, mirroring awaiting_approval (#1912).
//
// This is a strictly wider classifier than IsTerminal, used by the
// stage terminal-wait long-poll (GET /v0/runs/{run_id}/stages/{stage_id}
// ?wait, #1252) to decide when to stop blocking: a detached watcher
// wants to release the moment the stage needs operator attention, not
// only when the run is fully done. IsTerminal is left untouched for its
// narrower callers (transition tables, run-reduction).
func (s StageState) IsSettled() bool {
	switch s {
	case StageStateSucceeded, StageStateFailed, StageStateCancelled,
		StageStateAwaitingApproval, StageStateAwaitingChildren,
		StageStateAwaitingInput, StageStateAwaitingScopeDecision,
		StageStateAwaitingDeployApproval, StageStateAwaitingHostDispatch:
		return true
	default:
		return false
	}
}

// StageType is one of the stage kinds the run state machine recognizes.
type StageType string

// Stage types. plan/implement/review are the v0 closed set per MVP_SPEC
// §4.1; `deploy` is the v1 delegating release stage (ADR-038 / #925, E23.4)
// whose effect is the side effect — its gate is PRE-execution, see
// StageStateAwaitingDeployApproval. `acceptance` is the v1 runner-hosted
// advisory acceptance stage (ADR-049 / #1519, E31.2): unlike deploy it adds
// NO new stage states — it rides the existing agent-stage lifecycle exactly
// like review (deploy's two park states were the exception for its
// delegating pre-execution shape, not the pattern). The gate/orchestration/
// runner semantics that consume the type land in E31.6/E31.7.
const (
	StageTypePlan       StageType = "plan"
	StageTypeImplement  StageType = "implement"
	StageTypeReview     StageType = "review"
	StageTypeDeploy     StageType = "deploy"
	StageTypeAcceptance StageType = "acceptance"
)

// ExecutorKind says who executes the stage.
type ExecutorKind string

// Executor kinds. Agent stages run on the customer's CI under the
// runner action; Human stages block on a person.
const (
	ExecutorAgent ExecutorKind = "agent"
	ExecutorHuman ExecutorKind = "human"
)

// FailureCategory mirrors MVP_SPEC §6: A=agent, B=constraint/policy,
// C=infra, D=approval timeout. Set on a stage that transitions to
// the Failed terminal state; left nil for non-failed terminations.
type FailureCategory string

// Failure categories from MVP_SPEC §6.
const (
	FailureA FailureCategory = "A" // agent failure
	FailureB FailureCategory = "B" // constraint/policy violation
	FailureC FailureCategory = "C" // infrastructure failure
	FailureD FailureCategory = "D" // approval timeout
)

// Valid reports whether c is one of the four canonical categories.
// Empty / unknown values fail this check, which the FailStage
// helper enforces at the call site so a typo can't write a
// non-conforming category to a stage row.
func (c FailureCategory) Valid() bool {
	switch c {
	case FailureA, FailureB, FailureC, FailureD:
		return true
	}
	return false
}

// Description returns a single-line human label for c. Stable
// across calls; the frontend mirrors this map in TypeScript so the
// audit log and the UI agree on the wording. Unknown categories
// surface as the literal value so we don't silently mask bad data.
func (c FailureCategory) Description() string {
	switch c {
	case FailureA:
		return "agent failure"
	case FailureB:
		return "constraint or policy violation"
	case FailureC:
		return "infrastructure failure"
	case FailureD:
		return "approval timeout or rejection"
	}
	return string(c)
}

// DeployOutcome is the terminal disposition of a deploy stage (ADR-038 /
// #1384), modeled alongside FailureCategory rather than overloaded onto it.
//
// The A/B/C/D failure categories all assume re-execution is idempotent — a
// retry re-runs the same work safely. A deploy stage breaks that assumption:
//
//   - `partial` — the deploy neither cleanly succeeded nor is safe to blindly
//     re-run (some targets shipped, some did not). Treating it as a
//     retryable A/C failure would re-ship the already-shipped targets.
//   - `rolled_back` — an explicit operator sub-action reverted the deploy.
//     It is a deliberate terminal disposition, NEVER a blind retry.
//
// `succeeded` / `failed` map to the stage's succeeded / failed terminal
// states; `partial` / `rolled_back` are the disposition a stage can ALSO
// carry on a terminal deploy (see Stage.DeployOutcome). Persisting the
// outcome to a stage column and the producing executor are downstream
// (E23.5/E23.6/E23.10); this slice delivers the representable, validated
// type and its carrier on the in-memory stage.
type DeployOutcome string

// Deploy outcomes (ADR-038 / #1384).
const (
	DeployOutcomeSucceeded  DeployOutcome = "succeeded"
	DeployOutcomeFailed     DeployOutcome = "failed"
	DeployOutcomePartial    DeployOutcome = "partial"
	DeployOutcomeRolledBack DeployOutcome = "rolled_back"
)

// Valid reports whether o is one of the four canonical deploy outcomes.
// Empty / unknown values fail this check — the same fail-closed posture
// FailureCategory.Valid enforces so a typo can't write a non-conforming
// outcome to a deploy stage.
func (o DeployOutcome) Valid() bool {
	switch o {
	case DeployOutcomeSucceeded, DeployOutcomeFailed,
		DeployOutcomePartial, DeployOutcomeRolledBack:
		return true
	}
	return false
}

// Description returns a single-line human label for o. Stable across calls;
// mirrors FailureCategory.Description's shape. Unknown outcomes surface as
// the literal value so bad data is not silently masked.
func (o DeployOutcome) Description() string {
	switch o {
	case DeployOutcomeSucceeded:
		return "deploy succeeded"
	case DeployOutcomeFailed:
		return "deploy failed"
	case DeployOutcomePartial:
		return "deploy partially succeeded (not safe to blindly re-run)"
	case DeployOutcomeRolledBack:
		return "deploy rolled back (explicit operator sub-action)"
	}
	return string(o)
}

// TriggerSource identifies where a run originated.
type TriggerSource string

// Trigger sources for v0. Linear and Jira land under v0.x per
// MVP_SPEC §7.1.
const (
	TriggerGitHubIssue TriggerSource = "github_issue"
	TriggerCLI         TriggerSource = "cli"
	TriggerUI          TriggerSource = "ui"
)

// Run is the persisted record of a workflow execution.
type Run struct {
	ID             uuid.UUID
	Repo           string
	WorkflowID     string // e.g. "feature_change", matches a key under workflows in the spec
	WorkflowSHA    string // git SHA of .fishhawk/workflows.yaml at run time
	TriggerSource  TriggerSource
	TriggerRef     *string // e.g. "issue:1247" or nil for ad-hoc runs
	InstallationID *int64  // GitHub App installation that owns the repo; nil for non-GitHub triggers
	// IdempotencyKey scopes a CLI / UI trigger to be safely
	// retriable: a duplicate POST /v0/runs with the same
	// (repo, idempotency_key) returns the existing run instead
	// of creating a fresh one. Nil for webhook-driven runs (the
	// receiver dedups via X-GitHub-Delivery).
	IdempotencyKey *string
	// ParentRunID, when non-nil, identifies the prior run this one
	// follows up on (#216). Set by the dispatcher when it sees a
	// new trigger for a (repo, trigger_ref) that already has a
	// recent run. Lets the SPA render "follow-up to <short-id>"
	// + a thread of related runs without a recursive walk.
	ParentRunID *uuid.UUID
	// UpstreamRunID, when non-nil, names the upstream feature_change run
	// whose ci_green / review_merged the deploy stage's required_upstream
	// pre-flight gate evaluates (E23.11 / #1417). DISTINCT from ParentRunID
	// (#216) by deliberate design: a standalone deploy-only release run has
	// no implement/review stage of its own, so the gate needs a cross-run
	// reference to evaluate — but that reference must NOT carry the
	// follow-up/lineage semantics ParentRunID's get_plan plan-resolution
	// walk, resume/retry recovery, and decomposition provenance consumers
	// key on. Nil → the deploy gate evaluates the CURRENT run (the
	// appended-deploy path), byte-for-byte today's behavior.
	UpstreamRunID *uuid.UUID
	// PullRequestURL is set when the run's implement stage produces
	// a pull_request artifact (#216). Denormalized so "show me
	// every run on this PR" is a single equality query rather than
	// a recursive parent walk. Nil for runs that haven't reached
	// the implement stage yet, and for follow-up runs before their
	// own implement stage lands.
	PullRequestURL *string
	// RequiredChecksSnapshot is the union of required-status-check
	// context names across classic branch protection and any
	// applicable rulesets at run-create time (#251 / ADR-017). The
	// approval flow + the SPA read this to know which contexts must
	// pass before the PR can merge — protection edits during the
	// run don't shift the goalposts (FRESHNESS CONTRACT: resolved
	// ONCE at run-create, never re-resolved for the life of the run;
	// GitHub's own protected-branch enforcement remains the
	// merge-time authority). Captured on the webhook path by the
	// dispatcher and on the local / MCP / CLI / campaign create paths
	// by the server (#2506, from the best-effort App installation),
	// degrading to NIL — never a present-but-empty snapshot — on any
	// lookup failure so the #2497 checks_unresolved arm engages rather
	// than a vacuous green. Nil for legacy rows created before the
	// snapshot was wired and for runs with no attributable installation.
	RequiredChecksSnapshot *RequiredChecksSnapshot
	// WorkflowSpec is the raw bytes of `.fishhawk/workflows.yaml`
	// the dispatcher fetched + validated at run-create time (#283).
	// Cached here so the trace handler's policy re-evaluation can
	// read constraints from storage rather than refetching from
	// GitHub (the refetch path was broken — it passed `workflow_sha`
	// as a contents-API ref, but that's a blob SHA, not a commit
	// ref; GitHub returned 404 and the policy_evaluated audit row
	// was skipped). Nil for legacy rows created before this column
	// existed — the trace handler emits a skip-with-reason in that
	// case rather than re-introducing the broken refetch.
	WorkflowSpec []byte
	// RetryAttempt records this run's position in an auto-retry
	// chain (#279 / E16). 0 = original (canonical first attempt);
	// 1 = first retry; 2 = second retry; capped at the spec's
	// on_ci_failure.max_retries (default 1 per spec.DefaultMaxRetries).
	// The CI-failure dispatcher (handleCIFailureRetry) reads
	// parent.RetryAttempt + 1 when creating a follow-up run and
	// refuses to create a new child when retry_attempt >=
	// max_retries — emitting a `ci_retry_exhausted` audit instead.
	RetryAttempt int
	// MaxRetriesSnapshot is the workflow's
	// on_ci_failure.max_retries cap captured at run-create time
	// (#280 / E16). Snapshotted alongside RequiredChecksSnapshot
	// so a spec edit during a long-running auto-retry chain
	// doesn't shift the goalposts on what the SPA renders or what
	// the dispatcher enforces. Defaults to spec.DefaultMaxRetries
	// (= 1) when the workflow has no `on_ci_failure:` block.
	// Legacy rows (created before migration 0021) carry the
	// migration's column-level default of 1, which matches the
	// no-block case.
	MaxRetriesSnapshot int
	// RunnerKind tags the execution backend that runs this run
	// (ADR-022 / #388 + #404). v0 set: `github_actions` (the
	// canonical published runner action), `local` (operator-on-
	// workstation dev loop, Phase C of E22 / #389). The backend
	// assigns this at run-create time based on the dispatch path;
	// the runner never self-declares (a falsifiable claim from the
	// runner defeats the audit-integrity story).
	//
	// Legacy rows (created before migration 0024) carry the
	// migration's column-level default of `github_actions`.
	//
	// As of #1346 / ADR-045 the create-time value is a HINT, not the
	// authority: the runner self-reports the observed execution channel in
	// its SIGNED trace-bundle manifest, and the trace handler reconciles —
	// the first report LOCKS runner_kind (see RunnerKindResolved). This
	// closes the #1344 local-loop wedge where an omitted runner_kind:local
	// defaulted to github_actions and the drive's plan-approval gate waited
	// on a phantom GitHub-Actions runner.
	RunnerKind string
	// RunnerKindResolved reports whether RunnerKind has been LOCKED by a
	// runner self-report (#1346 / ADR-045). False (the migration 0036
	// default for every legacy row and every freshly-created run) means
	// RunnerKind is still the un-locked creation-time hint; the first signed
	// manifest report flips it true and a later disagreeing report is
	// FLAGGED (runner_kind_mismatch audit) rather than allowed to mutate.
	RunnerKindResolved bool
	// IssueContext caches the triggering GitHub issue's title,
	// body, url, and number at run-create time (#415). Populated
	// by the CLI's operator-side `gh issue view` fetch for runs
	// minted outside the webhook flow — the runs the backend can't
	// fetch the issue for because they carry no installation_id.
	// Webhook-dispatched runs leave this nil and fall through to
	// the existing GitHub fetch path in prompt.fillIssueContext.
	IssueContext *IssueContext
	// DecomposedFrom, when non-nil, identifies the parent run that
	// minted this child run during orchestrator fanout (#455). The
	// child-completion sweeper uses parent_run_id to find children;
	// DecomposedFrom provides the direct ancestry link for the SPA's
	// "decomposed from" breadcrumb.
	DecomposedFrom *uuid.UUID
	// CostUSDTotal is the rolled estimated US-dollar cost of every
	// model invocation in the run (#649). Accumulated control-plane-
	// side by the trace handler from the signed bundle manifest's
	// token counts via the shared `pricing` table — see
	// backend/internal/cost. The figure is an ESTIMATE (point-in-time
	// pricing; unknown models contribute 0); the per-bundle
	// cost_recorded audit entries are the canonical per-invocation
	// ledger. Defaults to 0 for legacy rows (migration 0028).
	CostUSDTotal float64
	// ResolvedModel pins the agent model id the run executed under,
	// read from the trace-bundle manifest (#649 / G6 reproducibility).
	// Last-write-wins across the run's bundles (every stage runs the
	// same agent model in v0). Empty for legacy rows and runs whose
	// runner didn't stamp a model.
	ResolvedModel string
	// Drive opts the run into backend auto-advancement of mechanical
	// transitions (#1023 / #996 theme 1). Resolved at run-create time
	// — the workflow spec's `drive` default overridden by the per-run
	// POST /v0/runs field — and snapshotted here so a spec edit
	// mid-run can't change an in-flight run's advancement behavior.
	// Defaults to false for legacy rows (migration 0031). Persisted
	// but not yet consumed; the drive engine lands in a sibling slice.
	Drive bool
	// SliceIndex is the decomposed child's 0-based sub_plan position
	// (E24.1 / #1141 / ADR-041). Set by orchestrator fanout when this
	// run is minted as a decomposition child; nil for non-decomposed
	// runs (standalone parents and ordinary runs). The runner reads it
	// back off the prompt-fetch response to route the child onto its
	// own sole-writer slice branch fishhawk/run-<parent>/slice-<n>.
	SliceIndex *int
	// AccountID is the tenant workspace account that owns this run
	// (ADR-057 / E44.5, migration 0055's nullable runs.account_id). Empty
	// string when the run is untenanted (NULL account_id — CLI/local runs,
	// and every legacy row until a later child backfills + tightens to NOT
	// NULL). Populated by construction: GetRun / ListRuns SELECT the column
	// and rowToRun maps NULL → "". The account-ownership authorization
	// middleware (internal/server) reads it to bound a run-scoped request to
	// the caller's account; an untenanted run is allowed through (#1830 will
	// close the NULL-allow window once every row is populated).
	AccountID string
	// WorkingDir is the local checkout the run's local (host-spawned)
	// stages execute in, bound ONCE at start_run and inherited by every
	// later runner-spawning verb (E66.42 / #2482). Empty means 'no binding'
	// — a legacy row (migration 0065's '' default), or a run created without
	// one (a github_actions run has no local checkout to bind). The MCP
	// resolveWorkingDirForRun ladder reads it: an omitted parameter inherits
	// this value, and an explicit value conflicting with a non-empty binding
	// is refused (the #1866 contamination shape). The empty state is what the
	// #2479 HTTP refusal still catches.
	WorkingDir string
	// PredictedRuntimeMinutes is the approved plan artifact's
	// predicted_runtime_minutes, stamped onto the run row at plan approval
	// (E48.62 / #2489, migration 0066). Zero means UNSTAMPED: a legacy row,
	// or a run whose plan has not been approved yet — notably while the plan
	// stage itself is running, since no plan exists to predict from.
	//
	// Advisory. It drives the MCP surface's advertised stage-wait poll
	// cadence (a quarter of the REMAINING predicted runtime, clamped to a 30s
	// floor and a 900s ceiling) and NOTHING gates on it: a stale, absent or
	// wildly wrong prediction costs at most a coarser or finer poll interval,
	// never a run's progress.
	PredictedRuntimeMinutes int
	State                   State
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// IssueContext is the cached payload from `gh issue view --json
// title,body,url,number`. Persisted on the run row as JSONB; the
// prompt builder reads it back into a prompt.Trigger.
//
// Fields mirror the GitHub REST API's issue shape; URL is the
// canonical github.com URL rather than the api.github.com URL so
// the agent and any rendered surfaces link directly to what humans
// see.
type IssueContext struct {
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	URL      string         `json:"url"`
	Number   int            `json:"number"`
	Comments []IssueComment `json:"comments,omitempty"`
	// Labels are the triggering issue's label NAMES, captured at
	// run-create from `gh issue view --json labels` (E53.3 / #2226).
	// They are the sole producer for the `applies_to` predicate's
	// `labels` criterion.
	//
	// Persisted inside the existing runs.issue_context JSONB payload —
	// ADDITIVE, no migration: migration 0025 added the column as JSONB
	// and postgres.go marshals/unmarshals the whole IssueContext, so a
	// new field round-trips and pre-#2226 rows unmarshal it to a nil
	// slice. Exactly the shape #618 used for Comments.
	//
	// The snapshot is deliberately IMMUTABLE for the life of the run:
	// both applies_to evaluation points (admission and the plan gate)
	// read this one value rather than re-reading mutable forge state,
	// so a control that fires twice cannot reach two answers about one
	// run. A nil/empty slice is an EMPTY label set, which fails closed
	// against a labels-declaring workflow — never match-all.
	Labels []string `json:"labels,omitempty"`
}

// IssueComment is one issue comment captured alongside the body at
// run-create (#618). Persisted inside the runs.issue_context JSONB
// payload (additive — existing rows lacking the key unmarshal to a
// nil slice). The prompt builder renders these into the plan-stage
// prompt so comment-borne refinements reach the plan agent.
type IssueComment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// RunnerKind enumerates the execution backends Fishhawk supports.
// Closed-set; new kinds extend via a migration that updates the
// CHECK constraint on runs.runner_kind.
//
// `gitlab_ci` (ADR-058 / E45.8, #1861) is the GitLab pipeline dispatch
// backend added as additive, DORMANT surface: the enum member + the
// migration 0054 CHECK widening land so the value is representable and
// persistable, but no gitlab_ci run is ever created in this change (go-live
// enablement is carved to #2043). It parallels `github_actions` as a
// non-host-dispatched backend that triggers a customer-side CI pipeline.
const (
	RunnerKindGitHubActions = "github_actions"
	RunnerKindLocal         = "local"
	RunnerKindGitLabCI      = "gitlab_ci"
)

// ValidRunnerKinds is the closed-set membership check.
var ValidRunnerKinds = map[string]struct{}{
	RunnerKindGitHubActions: {},
	RunnerKindLocal:         {},
	RunnerKindGitLabCI:      {},
}

// RunnerKindResolution is the outcome of reconciling a runner self-report
// against a run's persisted runner_kind (#1346 / ADR-045). Exactly one of
// the three outcomes is meaningful at a time:
//
//   - Changed: the report LOCKED the run (first report on an un-resolved
//     run) AND the locked value differs from the prior hint — emit a
//     runner_kind_resolved audit entry (Prior → Locked). The #1344 fix.
//   - Locked-but-not-Changed: the report locked (or re-affirmed) the run to
//     a value equal to the prior hint — no audit, nothing to surface.
//   - Mismatch: the run was ALREADY locked to a value that disagrees with
//     this report — the row is NOT mutated (warn, never silently flip) and
//     a runner_kind_mismatch audit entry is emitted (Prior=locked,
//     Observed=report). The post-execution guardrail.
//
// A no-op (empty/invalid observed value, or a run that doesn't exist) is the
// zero value: Locked=="" and all bools false.
type RunnerKindResolution struct {
	// Locked is the value runner_kind holds after reconciliation. Empty for
	// a no-op (unrecognized report).
	Locked string
	// Changed is true only when this report locked the run AND moved
	// runner_kind off its prior value (the create-time hint was wrong).
	Changed bool
	// Mismatch is true when the run was already locked to a DIFFERENT value;
	// the row was left unchanged.
	Mismatch bool
	// Observed is the runner-reported value that drove this reconciliation.
	Observed string
	// Prior is the runner_kind value before reconciliation (the value that
	// remains on a Mismatch).
	Prior string
}

// RequiredChecksSnapshot captures the required-status-checks list
// derived from the GitHub branch protection + rulesets APIs at
// run-create time. Persisted to runs.required_checks_snapshot per
// migration 0017 (#251). The shape is intentionally narrow — v0
// only reads `required_status_checks.contexts` and the surfaces
// that contributed; future fields land alongside without a schema
// migration.
type RequiredChecksSnapshot struct {
	// Contexts is the deduped union of context names across each
	// surface in Sources. Order is the order discovered (classic
	// protection first, then rulesets in the order GitHub returned
	// them) — stable so audit-log diffs are meaningful.
	Contexts []string `json:"contexts"`
	// Sources records which surfaces contributed contexts to the
	// union. Each entry is one of `branch_protection` or
	// `ruleset:<id>`. Empty when the run was created before the
	// snapshot was wired, and legitimately empty on an
	// AUTHORITATIVELY-empty snapshot — a repo that genuinely requires
	// nothing yields a non-nil snapshot with zero Contexts and zero
	// Sources (#2506). The dispatcher never persists such a snapshot
	// (it refuses an empty result), but the server capture does: nil
	// means "greenness unknown", present-but-empty means "nothing
	// required".
	Sources []string `json:"sources"`
}

// Stage is one ordered unit of work within a run.
type Stage struct {
	ID              uuid.UUID
	RunID           uuid.UUID
	Sequence        int
	Type            StageType
	ExecutorKind    ExecutorKind
	ExecutorRef     string // e.g. "claude-code" for agent executors
	State           StageState
	StartedAt       *time.Time
	EndedAt         *time.Time
	FailureCategory *FailureCategory
	FailureReason   *string
	// GateSLA is the gate's SLA string from the workflow spec at
	// stage-create time, e.g. "4_business_hours" or "24_hours".
	// Nil when the stage's gate has no SLA, when the stage isn't
	// gated, or for rows that predate the column. Parsed by the
	// SLA ticker into a wall-clock duration via internal/sla.Parse.
	GateSLA *string

	// RequiresApproval captures whether the workflow-spec stage
	// definition included an approval-typed gate. The trace upload
	// handler reads this to pick the right post-upload state:
	// gated stages (plan, review per the v0 default workflow)
	// transition to awaiting_approval; gateless stages (implement)
	// transition straight to succeeded so the orchestrator can
	// dispatch the next stage immediately. Persisted at
	// stage-create time so the handler doesn't need to re-parse the
	// workflow spec on every upload. Per migration 0013 (#207).
	RequiresApproval bool

	// Gate is the workflow-spec gate shape captured at stage-create
	// time. Nil when the stage has no gate; otherwise carries the
	// gate's type, blocking_checks, and approvers so downstream
	// surfaces (the review-stage detail UI, future check-state
	// ingestion) don't need to re-parse the spec at request time.
	// v0 stages typically carry one gate; if multiple are
	// configured the dispatcher persists the *primary* one (first
	// approval gate, else first check gate). Per migration 0014
	// (#213).
	Gate *Gate

	// SelfRetryCount tracks how many times this stage has been
	// retried by an agent via POST /v0/stages/{id}/retry with the
	// write:retries scope. Incremented atomically by RetryStageState
	// on each retry; 0 means the stage has not been agent-retried.
	// Used as retry_ordinal in the stage_retried audit receipt.
	SelfRetryCount int

	// ScopeCompletenessPark carries the held-commit coordinates when an
	// implement stage parks in awaiting_scope_decision (#1231): the
	// missing-declared-scope-file-only gate outcome pushed its verified
	// commit to the run branch (no PR) and is waiting for an operator
	// exempt-or-fail decision. Nil for every stage not currently parked
	// for a scope-completeness decision (the column is NULL). Persisted
	// to stages.scope_completeness_park JSONB per migration 0035.
	ScopeCompletenessPark *ScopeCompletenessPark

	// DeployOutcome carries the terminal disposition of a deploy stage
	// (ADR-038 / #1384). Nil on every non-deploy stage and on a deploy
	// stage that has not reached a terminal disposition. It is what makes
	// `partial` / `rolled_back` representable terminal states in THIS slice
	// (#1384, operator binding condition 3): a deploy stage's succeeded /
	// failed terminal STATE plus this field together describe the full
	// disposition (e.g. State=failed + DeployOutcome=partial, or
	// State=failed + DeployOutcome=rolled_back). The DB column + migration
	// and the producing executor are downstream (E23.5/E23.6); for now the
	// carrier is in-memory only — persistence does not round-trip it yet.
	DeployOutcome *DeployOutcome

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ScopeCompletenessPark is the durable payload an implement stage carries
// while parked in awaiting_scope_decision (#1231, generalized by #2501 and
// #2548). It pins exactly what the runner held when the SOLE committed-tree
// gate shortfall — of ONE of THREE classes — held up an otherwise-green
// implement pass: the verified commit already pushed to the run branch, the
// tree it verified, and the shortfall itself. Exactly one of MissingPaths /
// UnsatisfiedAssertions / BuildRequiredPaths is populated; a compound failure
// never parks (it keeps today's category-B abort). The three classes are:
//
//   - MissingPaths — the "missing declared scope file(s)" check (#1151).
//     Standalone open-PR pushes only. Exempt-eligible.
//   - UnsatisfiedAssertions — the unsatisfied-binding-assertion check (#1171,
//     #2501). Standalone open-PR pushes only. Exempt-eligible.
//   - BuildRequiredPaths — build-required scope drift (#2548), DECOMPOSED
//     CHILDREN ONLY. Exempt-INELIGIBLE: the held commit's committed tree is RED
//     by construction (that is what the park records), so resuming it would
//     open a PR from a red tree. The server refuses `exempt` for this class
//     (server/scope_completeness.go) and the prompt emission gate withholds the
//     held-commit fields independently (server/prompt.go). The only admissible
//     decision is `fail`; recovery is to correct the decomposition and re-run,
//     with the slice's work preserved on its sole-writer slice branch.
//
// For the two exempt-eligible classes the operator's exempt decision opens the
// PR from HeldCommitSHA with no agent re-run; the fail decision drops to
// today's category-B.
//
// ROLLBACK PRECONDITION (#2548): reverting the change that introduced
// BuildRequiredPaths drops the key on decode AND removes both safeguards, so an
// already-parked build-required row would look like an ordinary park and become
// EXEMPTIBLE. Resolve every outstanding build-required park with `fail` BEFORE
// such a revert lands — see backend/internal/server/README.md.
//
// The JSON tags are the byte-identical cross-module wire contract with
// the runner's park-report upload struct (runner/internal/upload —
// sibling slice), following the established ScopeExemption duplication
// pattern rather than a shared module. Keep the two in lockstep.
type ScopeCompletenessPark struct {
	// HeldCommitSHA is the gate-verified commit the runner already pushed
	// to RunBranch (no PR). The exempt resolution opens the PR from this
	// exact SHA — byte-identical tree, zero agent re-run.
	HeldCommitSHA string `json:"held_commit_sha"`
	// RunBranch is the run's own sole-writer branch the held commit was
	// pushed to (ADR-035): the resolution opens the PR from this branch.
	RunBranch string `json:"run_branch"`
	// VerifiedTreeSHA is the tree object the committed-tree verify gate
	// passed on. Recorded so the resolution can assert the opened PR head
	// reflects the identical tree.
	VerifiedTreeSHA string `json:"verified_tree_sha"`
	// BaseSHA is the base commit the held commit was built on — the merge
	// target the exempt resolution ships as the pull-request artifact's
	// base_sha (#2563). Recorded on the park so the zero-re-run exempt resume
	// has the value the backend's success-arm validate() requires; a pre-#2563
	// park row decodes with an empty BaseSHA (the column is JSONB — no
	// migration) and the exempt resolution falls back to the newest
	// scope_completeness_parked audit entry's base_sha.
	BaseSHA string `json:"base_sha,omitempty"`
	// MissingPaths are the declared scope.files the agent never touched —
	// the #1151 gate shortfall class. Repo-relative. Empty on an
	// assertion-class park.
	MissingPaths []string `json:"missing_paths"`
	// UnsatisfiedAssertions are the operator-declared binding assertions the
	// committed tree did not satisfy — the #1171 gate shortfall class (#2501).
	// Empty on a missing-scope-class park, so a pre-#2501 park row round-trips
	// through this struct unchanged (the column is JSONB — no migration).
	UnsatisfiedAssertions []UnsatisfiedAssertion `json:"unsatisfied_assertions,omitempty"`
	// BuildRequiredPaths are the scope-EXCLUDED paths a DECOMPOSED CHILD's
	// committed tree needed to build — the #2548 shortfall class. Repo-relative.
	// Empty on the two older classes, so a pre-#2548 park row round-trips
	// through this struct unchanged (the column is JSONB — no migration).
	// Non-empty makes the park exempt-INELIGIBLE (see the type doc).
	BuildRequiredPaths []string `json:"build_required_paths,omitempty"`
	// OwningSlices attributes each build-required path to the sibling sub-plan
	// that DECLARES it in the parent's approved decomposition — the whole
	// operator value of this class, since it names which slice boundary was cut
	// rather than only that a file was out of scope. Resolved at park time from
	// decomposition.sub_plans[i].scope.files. Persisted (rather than recomputed)
	// so the orchestrator's parent-side signal carries byte-identical
	// attribution without re-reading the parent plan. Empty when attribution
	// degraded — attribution is diagnostic and must never cost the operator the
	// preserved commit.
	OwningSlices []BuildRequiredOwner `json:"owning_slices,omitempty"`
	// PRTitle and PRBody are the AGENT-AUTHORED pull-request text the parking
	// pass resolved (#2570), carried so an EXEMPT resume opens a real pull
	// request instead of the `chore: fishhawk implement stage <id>` placeholder
	// — which carries no summary, no test plan, and no `Closes #N`, so the
	// trigger issue never auto-closes on merge.
	//
	// They are persisted here rather than re-read at resume time because the
	// runner's /tmp PR-description handoff is already GONE: loadAgentAuthoredPR
	// deletes it on every read path, the parking pass consumed it before the park
	// existed, and on an ephemeral runner /tmp does not survive the stage at all.
	// This JSONB column is the only storage both a same-host and a fresh-host
	// resume can reach.
	//
	// PRBody EXCLUDES the Fishhawk attribution footer: the runner that OPENS the
	// pull request appends it exactly once at open time, so a persisted footer
	// would either double up or carry the wrong branch/audit URL.
	//
	// Both omitempty and additive — the column is JSONB, so no migration — and a
	// pre-#2570 park row decodes them EMPTY, where heldCommitPRTitleBody
	// (backend/internal/server/heldcommitpr.go) synthesizes the documented
	// issue-context fallback instead.
	PRTitle string `json:"pr_title,omitempty"`
	PRBody  string `json:"pr_body,omitempty"`
}

// BuildRequiredOwner attributes one build-required path to the decomposition
// sub-plan that owns it (#2548). SliceIndex is nil and SliceTitle empty when no
// sub-plan claims the path (or the parent plan/decomposition was unresolvable):
// an unattributed entry is a legitimate degrade, not an error.
type BuildRequiredOwner struct {
	// Path is the repo-relative build-required path.
	Path string `json:"path"`
	// SliceIndex is the owning sub-plan's index in decomposition.sub_plans,
	// nil when unattributed.
	SliceIndex *int `json:"slice_index,omitempty"`
	// SliceTitle is the owning sub-plan's title, empty when unattributed.
	SliceTitle string `json:"slice_title,omitempty"`
}

// UnsatisfiedAssertion is one operator-declared binding assertion the held
// commit did not satisfy (#2501). The JSON tags (type/path/literal) are the
// byte-identical cross-module wire contract with the runner's
// upload.BindingAssertionReport, following the same ScopeExemption duplication
// pattern as ScopeCompletenessPark itself. Keep the two in lockstep.
type UnsatisfiedAssertion struct {
	// Type is the declared assertion type (v0: file_contains / test_asserts).
	Type string `json:"type"`
	// Path is the repo-relative file the assertion was evaluated against.
	Path string `json:"path"`
	// Literal is the substring the committed content of Path did not contain.
	Literal string `json:"literal"`
}

// GateKind names the two flavors of gate the workflow spec admits:
// approval (humans must act) and check (named status checks must
// pass). Mirrors spec.GateType but lives in the run package so the
// stages row's persisted shape doesn't depend on the spec parser.
type GateKind string

// Gate kinds per the workflow spec.
const (
	GateKindApproval GateKind = "approval"
	GateKindCheck    GateKind = "check"
)

// GateApprovers names the roles whose members can satisfy an approval
// gate. Exactly one of AnyOf or AllOf is set when populated; both nil
// means the gate has no approvers (either it's a check gate or the
// stage isn't gated). Mirrors spec.Approvers shape on the wire.
type GateApprovers struct {
	AnyOf []string `json:"any_of,omitempty"`
	AllOf []string `json:"all_of,omitempty"`
}

// Gate is the persisted shape of a stage's workflow-spec gate. The
// review-stage UI reads it to decide whether to surface the
// approval panel. Persisted to stages.gate_type / .gate_approvers
// per migration 0014; the gate_blocking_checks column was dropped
// in migration 0018 (#254 / ADR-017) along with the spec field —
// required CI checks now live in branch protection (#251).
type Gate struct {
	Kind      GateKind
	Approvers *GateApprovers
}
