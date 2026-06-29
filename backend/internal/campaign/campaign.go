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
const (
	ItemStatePending   ItemState = "pending"
	ItemStateBlocked   ItemState = "blocked"
	ItemStateRunning   ItemState = "running"
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

// Campaign is the persisted record of an epic-driven multi-issue campaign.
type Campaign struct {
	ID        uuid.UUID
	Repo      string
	EpicRef   string // e.g. "issue:1439" — the epic the campaign decomposes
	State     State
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Item is one issue within a campaign.
type Item struct {
	ID         uuid.UUID
	CampaignID uuid.UUID
	IssueRef   string   // e.g. "issue:1441"
	DependsOn  []string // sibling issue refs this item waits on (the campaign DAG edges)
	// RunID is the run linkage: the nullable FK to the runs row executing
	// this item (campaign_items.run_id, ON DELETE SET NULL). Nil until a
	// run is assigned; nulled (not deleted) if that run is later removed,
	// preserving campaign history.
	RunID     *uuid.UUID
	State     ItemState
	CreatedAt time.Time
	UpdatedAt time.Time
}
