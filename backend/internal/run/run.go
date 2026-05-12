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
// `dispatched` means the workflow_dispatch event has been sent to
// GitHub but the runner has not yet checked in. `running` means the
// runner has started executing. `awaiting_approval` means a gate is
// blocking on human action.
type StageState string

// Stage states. Terminal states (Succeeded, Failed, Cancelled) admit
// no further transitions; see transition.go for the table.
const (
	StageStatePending          StageState = "pending"
	StageStateDispatched       StageState = "dispatched"
	StageStateRunning          StageState = "running"
	StageStateAwaitingApproval StageState = "awaiting_approval"
	StageStateSucceeded        StageState = "succeeded"
	StageStateFailed           StageState = "failed"
	StageStateCancelled        StageState = "cancelled"
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

// StageType is one of the three stage kinds permitted in v0.
type StageType string

// Stage types. Closed set per MVP_SPEC §4.1; no custom types in v0.
const (
	StageTypePlan      StageType = "plan"
	StageTypeImplement StageType = "implement"
	StageTypeReview    StageType = "review"
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
	// run don't shift the goalposts. Nil for runs created before
	// the snapshot was wired (legacy rows) and for CLI / UI
	// run-create paths that skip protection lookup.
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
	State        State
	CreatedAt    time.Time
	UpdatedAt    time.Time
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
	// snapshot was wired; never nil on a fresh dispatcher run.
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

	CreatedAt time.Time
	UpdatedAt time.Time
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
