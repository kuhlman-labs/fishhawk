// Package campaign owns the campaign / campaign-item state machine and its
// persistence interface — the durable campaign object (Track B keystone of
// ADR-047 / #1437, E25.2). It mirrors backend/internal/run: the state
// machine is governed by transition tables defined in transition.go;
// concrete persistence lives in postgres.go but is consumed via the
// Repository interface so unit tests can substitute fakes.
//
// A campaign is the parent record for an epic-driven multi-issue run; it
// owns an ordered set of campaign items, one per issue under the epic. The
// run ↔ campaign cross-boundary link lives on the item's RunID (a nullable
// FK to runs) so a campaign's issue-runs are discoverable via the item rows
// without touching the hot runs table.
//
// State enums are unexported strings rather than ints so audit log entries
// and JSON payloads carry human-readable values forever — the same posture
// as run.State.
package campaign

import (
	"time"

	"github.com/google/uuid"
)

// State is the lifecycle state of a campaign.
//
// A campaign is the parent record; campaign items are children. The
// campaign state is a reduction of its items: it becomes Running when its
// first item dispatches, Succeeded when every item succeeds, Failed on a
// terminal item failure, and Cancelled when manually halted.
type State string

// Campaign states. Terminal states (Succeeded, Failed, Cancelled) admit no
// further transitions; see transition.go for the table.
//
// `paused` is the safety-boundary overlay (Track C / E25.7): the backend
// auto-driver pauses a campaign when a run gate refuses a hand-off a human
// must own (reviewer_reject / requirement_arbitration). It is NON-terminal —
// a human/operator-agent resumes it (paused → running) once the gate is
// handled — and is never derived from item states (DeriveState never emits
// it); only the driver/operator set it.
const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StatePaused    State = "paused"
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

// ItemState is the lifecycle state of a single campaign item.
//
// `blocked` means the item's depends_on edges are not yet satisfied — it is
// waiting on a sibling item. `pending` is the initial admitted state; an
// item with no unsatisfied dependencies advances pending → running
// directly, while one with open dependencies parks pending → blocked until
// they clear (blocked → pending or blocked → running).
type ItemState string

// Campaign item states. Terminal states (Succeeded, Failed, Cancelled)
// admit no further transitions; see transition.go for the table.
//
// `paused` mirrors the campaign-level overlay: an item whose gate the
// auto-driver handed off to a human is paused (running → paused) carrying a
// PauseReason. It is NON-terminal — resuming flips it back to running — and
// is never derived; only the driver/operator set it.
const (
	ItemStatePending   ItemState = "pending"
	ItemStateBlocked   ItemState = "blocked"
	ItemStateRunning   ItemState = "running"
	ItemStatePaused    ItemState = "paused"
	ItemStateSucceeded ItemState = "succeeded"
	ItemStateFailed    ItemState = "failed"
	ItemStateCancelled ItemState = "cancelled"
)

// IsTerminal reports whether the state admits no further transitions.
func (s ItemState) IsTerminal() bool {
	switch s {
	case ItemStateSucceeded, ItemStateFailed, ItemStateCancelled:
		return true
	default:
		return false
	}
}

// PausePolicy governs what the auto-driver pauses when a run gate is handed
// off to a human (Track C / E25.7). It is an operator-configurable choice set
// at campaign creation; the zero value normalizes to PausePolicyPauseCampaign
// (the conservative block-the-campaign safety default for backend auto-drive)
// — see normalizePausePolicy and campaign.Persist.
type PausePolicy string

// Pause policies.
const (
	// PausePolicyPauseCampaign blocks the whole campaign on a page: the
	// affected item AND the campaign pause until a human resumes. The default.
	PausePolicyPauseCampaign PausePolicy = "pause_campaign"
	// PausePolicyPauseItem pauses only the affected item; sibling items keep
	// running (continue-others).
	PausePolicyPauseItem PausePolicy = "pause_item"
)

// normalizePausePolicy defaults a zero PausePolicy to PausePolicyPauseCampaign
// so an unset policy persists as the conservative block-the-campaign default
// and never as an empty string (which would violate the column CHECK). It is
// the single normalization point shared by campaign.Persist (domain) and the
// Postgres adapter (defensive, for direct repository callers).
func normalizePausePolicy(p PausePolicy) PausePolicy {
	if p == "" {
		return PausePolicyPauseCampaign
	}
	return p
}

// PauseReason records why an item was paused: the page event (the audit
// category that triggered the hand-off, e.g. campaign_gate_paged) plus the
// run/stage and gate the human must act on. Persisted as JSONB on
// campaign_items.pause_reason and surfaced so the operator sees where the
// campaign stalled.
type PauseReason struct {
	// PageEvent is the audit category that triggered the page (the run-chained
	// hand-off marker, e.g. "campaign_gate_paged").
	PageEvent string `json:"page_event,omitempty"`
	// RunID is the run whose gate was handed off, if any.
	RunID *uuid.UUID `json:"run_id,omitempty"`
	// StageID is the gate's stage, if any.
	StageID *uuid.UUID `json:"stage_id,omitempty"`
	// Gate names the gate/decision the human must own (e.g. the gate kind).
	Gate string `json:"gate,omitempty"`
}

// Campaign is the persisted record of an epic-driven multi-issue campaign.
type Campaign struct {
	ID      uuid.UUID
	Repo    string
	EpicRef string // e.g. "issue:1439" — the epic the campaign decomposes
	State   State
	// PausePolicy governs what the auto-driver pauses on a gate hand-off.
	// Always normalized (never empty) on a persisted campaign.
	PausePolicy PausePolicy
	// OperatorAgent is the OPTIONAL campaign-level delegation override
	// (Track E / E25.12): the raw JSONB operator_agent block that, when
	// present, wins WHOLESALE as the outermost rung of the resolution ladder
	// (campaign > gate > workflow) for every issue-run of the campaign. It is
	// stored opaquely as raw bytes here — the campaign package stays spec-free,
	// mirroring how a run carries WorkflowSpec []byte; the server validates it
	// against spec.OperatorAgent at create time and the auto-driver (slice B)
	// parses it at consumption. Nil (NULL column) means no override: each
	// issue-run inherits its workflow's operator_agent contract, byte-identical
	// to pre-E25.12 behavior.
	OperatorAgent []byte
	// IdempotencyKey scopes a create to be safely retriable: a second POST
	// /v0/campaigns with the same (repo, idempotency_key) returns the existing
	// campaign instead of minting a duplicate (E25.13 / #1455). Nil (NULL
	// column) means the campaign was created without a key — the unchanged
	// default — and the partial unique index excludes NULLs so keyless
	// campaigns never collide. Mirrors run.Run.IdempotencyKey.
	IdempotencyKey *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Item is one issue within a campaign.
type Item struct {
	ID         uuid.UUID
	CampaignID uuid.UUID
	IssueRef   string   // e.g. "issue:1441"
	DependsOn  []string // sibling issue refs this item waits on (the campaign DAG edges)
	// Autonomy is the item's autonomy tier (low|medium|high), threaded from the
	// child issue's `autonomy:<tier>` label through EpicChild → AssembledItem →
	// the campaign_items.autonomy column. Empty means unknown/default (the child
	// carried no autonomy label), treated by the engine as non-human-led. A
	// deps-satisfied autonomy:low item is diverted out of the auto-dispatch
	// Eligible slice into HumanLed (#1551 / E32.4).
	Autonomy string
	// RunID is the run linkage: the nullable FK to the runs row executing
	// this item (campaign_items.run_id, ON DELETE SET NULL). Nil until a
	// run is assigned; nulled (not deleted) if that run is later removed,
	// preserving campaign history.
	RunID *uuid.UUID
	State ItemState
	// PauseReason carries why a paused item was handed off to a human. Nil
	// unless the item is (or was) paused. Persisted as JSONB.
	PauseReason *PauseReason
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
