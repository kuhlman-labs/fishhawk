package campaign

// expedite.go holds the pure core of the campaign HOTFIX verb's persistence
// (E83.100 / #4234): an EXPEDITED item (one urgent issue added to a running
// campaign at the head of its queue, run under its recorded workflow) and the
// optional campaign DISPATCH HOLD it may place. Everything here is
// side-effect-free so it is unit-testable without a Repository; the Postgres
// adapter re-runs ValidateExpedite (through ValidateAppend) under the campaign
// row lock, and the test fake enforces the same rule.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ExpediteKindHotfix is the only expedite kind: an item added by the campaign
// hotfix verb. The set is mirrored by the migration-0106 CHECK.
const ExpediteKindHotfix = "hotfix"

// MaxDispatchHold caps a dispatch hold's window: expires_at may be at most
// this far after held_at. A hold the hotfix never releases lapses at the cap.
const MaxDispatchHold = 2 * time.Hour

// The release reasons a campaign_hold_released row carries.
const (
	// HoldReleasedHotfixTerminal: the hotfix item, or the run linked to it,
	// reached a terminal state.
	HoldReleasedHotfixTerminal = "hotfix_terminal"
	// HoldReleasedTimeout: the hold reached its expires_at.
	HoldReleasedTimeout = "timeout"
)

// Sentinel errors of the expedite and dispatch-hold writes.
var (
	// ErrExpediteInvalid: an expedited append or a hold release is malformed —
	// not exactly one new item, an addition alongside it, an unknown kind, an
	// empty workflow, a hold window that is empty or exceeds MaxDispatchHold, or
	// an unknown release reason. Nothing is written.
	ErrExpediteInvalid = errors.New("campaign: invalid expedite")
	// ErrDispatchHoldActive: the campaign already holds a dispatch hold, so an
	// expedited append asking for another is refused and writes nothing (one
	// hold per campaign; the hold row's primary key is the campaign id).
	ErrDispatchHoldActive = errors.New("campaign: a dispatch hold is already active")
	// ErrHoldReleaseUnrecorded: the release's Record callback failed (or none
	// was supplied), so the release rolled back and the hold is still active.
	ErrHoldReleaseUnrecorded = errors.New("campaign: dispatch hold release could not be recorded")
)

// ExpediteParams marks an AppendCampaignItems call as an expedited (hotfix)
// append. The single new item heads the queue, a campaign_item_expedites row
// records Kind / Workflow / Reason, and a non-nil Hold places a dispatch hold
// in the same transaction.
type ExpediteParams struct {
	// Kind is the expedite kind; ExpediteKindHotfix is the only one.
	Kind string
	// Workflow is the workflow the item is run under. Required.
	Workflow string
	// Reason is the operator's free-text justification. It is untrusted-origin
	// text, persisted and rendered only as data. Its rules (a byte cap, required
	// for a non-default workflow) are the verb's, not this package's.
	Reason string
	// Hold, when non-nil, places a dispatch hold naming the new item.
	Hold *DispatchHoldParams
}

// DispatchHoldParams is the window of a dispatch hold. Both stamps come from
// the caller's clock (fishhawkd's s.nowFunc), never Postgres now(), so the
// later release comparison stays in one clock domain (AGENTS.md trap #3048).
type DispatchHoldParams struct {
	HeldAt    time.Time
	ExpiresAt time.Time
}

// ReleaseDispatchHoldParams is the input to Repository.ReleaseDispatchHold.
type ReleaseDispatchHoldParams struct {
	CampaignID uuid.UUID
	// HotfixItemID names the hold being released. A hold naming a different
	// item (a later hold placed after this one was released) is not touched.
	HotfixItemID uuid.UUID
	// Reason is HoldReleasedHotfixTerminal or HoldReleasedTimeout.
	Reason string
	// Record is REQUIRED. It runs inside the transaction after the delete and
	// before commit; an error rolls the delete back and the release returns it
	// wrapped in ErrHoldReleaseUnrecorded. A nil Record is refused the same way,
	// so no release ever commits without its audit row.
	Record func(ctx context.Context, hold DispatchHold) error
}

// ValidateExpedite checks p.Expedite in the context of its append; a nil
// p.Expedite is valid (an ordinary append). An expedited append carries
// exactly ONE new item and no depends_on additions — an expedited item
// declares no reverse dependencies — plus a known kind and a non-empty
// workflow. A hold must be non-zero with held_at < expires_at <=
// held_at + MaxDispatchHold. Every failure wraps ErrExpediteInvalid.
func ValidateExpedite(p AppendCampaignItemsParams) error {
	e := p.Expedite
	if e == nil {
		return nil
	}
	if len(p.NewItems) != 1 {
		return fmt.Errorf("%w: an expedited append carries exactly one new item, got %d", ErrExpediteInvalid, len(p.NewItems))
	}
	if len(p.AddDependencies) > 0 {
		return fmt.Errorf("%w: an expedited append carries no depends_on additions", ErrExpediteInvalid)
	}
	if e.Kind != ExpediteKindHotfix {
		return fmt.Errorf("%w: unknown expedite kind %q", ErrExpediteInvalid, e.Kind)
	}
	if strings.TrimSpace(e.Workflow) == "" {
		return fmt.Errorf("%w: an expedited item needs a workflow", ErrExpediteInvalid)
	}
	if h := e.Hold; h != nil {
		if h.HeldAt.IsZero() || !h.ExpiresAt.After(h.HeldAt) {
			return fmt.Errorf("%w: a dispatch hold needs held_at before expires_at", ErrExpediteInvalid)
		}
		if h.ExpiresAt.After(h.HeldAt.Add(MaxDispatchHold)) {
			return fmt.Errorf("%w: a dispatch hold may last at most %s", ErrExpediteInvalid, MaxDispatchHold)
		}
	}
	return nil
}

// HoldReleaseReason decides whether hold h is due for release at now. A
// terminal hotfix (its item, or the run linked to it) wins:
// (HoldReleasedHotfixTerminal, true). Otherwise the hold times out at its
// expires_at — exactly AT expiry is due: (HoldReleasedTimeout, true). Before
// expiry, or for a nil hold, it is not due: ("", false).
func HoldReleaseReason(h *DispatchHold, hotfixTerminal bool, now time.Time) (string, bool) {
	if h == nil {
		return "", false
	}
	if hotfixTerminal {
		return HoldReleasedHotfixTerminal, true
	}
	if !now.Before(h.ExpiresAt) {
		return HoldReleasedTimeout, true
	}
	return "", false
}

// validReleaseReason reports whether reason is one a campaign_hold_released
// row may carry.
func validReleaseReason(reason string) bool {
	return reason == HoldReleasedHotfixTerminal || reason == HoldReleasedTimeout
}

// ValidateReleaseDispatchHold runs the release guards that need no row: a
// known reason and a non-nil Record. It is exported so a test fake enforces
// exactly the production rules.
func ValidateReleaseDispatchHold(p ReleaseDispatchHoldParams) error {
	if !validReleaseReason(p.Reason) {
		return fmt.Errorf("%w: unknown hold release reason %q", ErrExpediteInvalid, p.Reason)
	}
	if p.Record == nil {
		return fmt.Errorf("%w: no recorder supplied", ErrHoldReleaseUnrecorded)
	}
	return nil
}

// ExpeditedHeadPosition is the queue_position an expedited item takes: one
// BELOW every existing item, so it heads the queue and NextEligible lists it
// first with no engine change. It is 0 for an empty roster and may be
// NEGATIVE — the dense 0-based position invariant does not hold once a
// campaign has an expedited item (migration 0074 puts no CHECK on the column).
func ExpeditedHeadPosition(existing []*Item) int {
	if len(existing) == 0 {
		return 0
	}
	minPos := existing[0].Position
	for _, it := range existing[1:] {
		if it.Position < minPos {
			minPos = it.Position
		}
	}
	return minPos - 1
}
