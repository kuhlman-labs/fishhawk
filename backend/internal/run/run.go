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
	State          State
	CreatedAt      time.Time
	UpdatedAt      time.Time
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
	GateSLA   *string
	CreatedAt time.Time
	UpdatedAt time.Time
}
