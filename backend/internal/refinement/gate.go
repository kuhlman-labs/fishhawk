package refinement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// The approval gate is the E34.2 seam that makes approval load-bearing
// (ADR-052 option A). It is pure and repository-agnostic: it derives a
// session's approval state from the session's ordered draft revisions and its
// append-only decision rows. Nothing here writes — the caller (the HTTP
// handler / the E34.3 filing executor) supplies the rows and acts on the
// derived state.
//
// The load-bearing property is that session state is DERIVED, never stored: a
// decision counts only when it targets the session's LATEST revision AND its
// pinned content hash still matches the recomputed hash. So an edit after
// approval (which lands a NEW revision) structurally invalidates the approval —
// the decision now targets a superseded revision — and a stored-hash drift
// fails closed. There is no mutable approval flag to fall out of sync.

// Gate sentinel errors. ApprovedDraft returns these so the E34.3 filing
// executor can distinguish "no usable approval" from "approval exists but the
// approved draft drifted" — both refuse filing, but the drift case is a
// tamper/corruption signal, not merely an ungated session.
var (
	// ErrNotApproved means the session's latest revision carries no approving
	// decision (never decided, rejected, or the approval targets a superseded
	// revision). A draft cannot be filed in any state except approved.
	ErrNotApproved = errors.New("refinement: latest revision is not approved")
	// ErrDraftDrifted means the latest revision's approving decision pins a
	// content hash that no longer matches the recomputed hash — the filing
	// executor refuses a drifted draft rather than filing something other than
	// what was approved.
	ErrDraftDrifted = errors.New("refinement: approved draft content has drifted from the decision")
)

// SessionState is the derived approval state of a refinement session.
type SessionState string

const (
	// StateAwaitingApproval is the default: the latest revision has no counting
	// decision (undecided, or a decision on a superseded revision, or a
	// hash-drift that fails closed).
	StateAwaitingApproval SessionState = "awaiting_approval"
	// StateApproved is set when the latest revision carries a matching approving
	// decision.
	StateApproved SessionState = "approved"
	// StateRejected is set when the latest revision carries a matching rejecting
	// decision.
	StateRejected SessionState = "rejected"
)

// Resolution is the derived approval verdict for a session, returned by
// ResolveState. State is the presentation state; Drifted flags that a decision
// on the latest revision pinned a hash that no longer matches (fail-closed to
// awaiting_approval); LatestDraftID is the session's newest revision; Decision
// is the decision targeting the latest revision, if any (matched or drifted) —
// its presence is what the handler's 409 decision-already-recorded guard reads.
type Resolution struct {
	State         SessionState
	Drifted       bool
	LatestDraftID uuid.UUID
	Decision      *Decision
}

// ContentHash returns the sha256 hex over json.Marshal of the DECODED
// EpicDraft. Hashing the struct — not the stored JSONB bytes — sidesteps
// Postgres's jsonb key-order/whitespace non-preservation (the trap audit's
// canonicalizeJSON exists for): encoding/json marshals struct fields in
// declaration order, deterministically, so a persist→read-back→hash round-trip
// yields the same digest the decision pinned.
func ContentHash(draft EpicDraft) (string, error) {
	raw, err := json.Marshal(draft)
	if err != nil {
		return "", fmt.Errorf("refinement: hash draft: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// latestDecisionFor returns the last decision (in append order) targeting
// draftID, or nil when none does. Append order means a later decision on the
// same revision wins, though the handler's 409 guard prevents a second
// in-band decision on one revision.
func latestDecisionFor(draftID uuid.UUID, decisions []*Decision) *Decision {
	var found *Decision
	for _, d := range decisions {
		if d.DraftID == draftID {
			found = d
		}
	}
	return found
}

// ResolveState derives a session's approval Resolution from its ordered draft
// revisions (created_at ASC — so the last element is the latest revision) and
// its append-only decisions. Only a decision targeting the latest revision
// whose pinned hash equals the recomputed hash counts; a decision on a
// superseded revision leaves the latest revision undecided (awaiting_approval);
// a hash mismatch on the latest revision's decision resolves to
// awaiting_approval with Drifted set (fail closed). An empty drafts slice is a
// caller error (a session always has at least its initial revision).
func ResolveState(drafts []*StoredDraft, decisions []*Decision) (Resolution, error) {
	if len(drafts) == 0 {
		return Resolution{}, errors.New("refinement: session has no draft revisions")
	}
	latest := drafts[len(drafts)-1]
	res := Resolution{State: StateAwaitingApproval, LatestDraftID: latest.ID}

	dec := latestDecisionFor(latest.ID, decisions)
	if dec == nil {
		return res, nil
	}
	res.Decision = dec

	recomputed, err := ContentHash(latest.Draft)
	if err != nil {
		return Resolution{}, err
	}
	if dec.DraftContentHash != recomputed {
		// The decision targets the latest revision but its pinned hash no
		// longer matches — fail closed to awaiting_approval, flagged drifted.
		res.Drifted = true
		return res, nil
	}
	switch dec.Decision {
	case DecisionApproved:
		res.State = StateApproved
	case DecisionRejected:
		res.State = StateRejected
	}
	return res, nil
}

// ApprovedDraft resolves the pinned approved revision the E34.3 filing executor
// consumes, or a typed sentinel. It is the ONE place "a draft cannot be filed
// in any state except approved" and "the filing executor refuses a drifted
// draft" are enforced: it returns the latest revision only when that revision
// carries a matching approving decision, ErrNotApproved when it does not, and
// ErrDraftDrifted when the approving decision's pinned hash no longer matches
// the recomputed hash.
func ApprovedDraft(drafts []*StoredDraft, decisions []*Decision) (*StoredDraft, error) {
	if len(drafts) == 0 {
		return nil, ErrNotApproved
	}
	latest := drafts[len(drafts)-1]
	dec := latestDecisionFor(latest.ID, decisions)
	if dec == nil || dec.Decision != DecisionApproved {
		return nil, ErrNotApproved
	}
	recomputed, err := ContentHash(latest.Draft)
	if err != nil {
		return nil, err
	}
	if dec.DraftContentHash != recomputed {
		return nil, ErrDraftDrifted
	}
	return latest, nil
}
