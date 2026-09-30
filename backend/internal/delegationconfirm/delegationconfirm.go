// Package delegationconfirm holds delegation confirmation on handover
// (E76.5 / #3768, ADR-083 #3751 rule 7): the two global-chain event kinds an
// incoming captain writes when it confirms or proposes to lower one
// workflow's delegation, the PURE fold (Derive) that reconstructs each
// workflow's confirmation state from those entries AND the captain record,
// the pure transitions for the two write verbs, the lower-validation rule
// that makes a raise inexpressible, and the Store that serializes the
// captain check with the append (see store.go).
//
// The chain is the sole authority: there is no derived table and no
// migration. Three rules the fold enforces:
//
//   - Confirmation state RESETS at every seat change (captain_assigned or
//     captain_claimed): a confirmation written before the latest seat change
//     is never carried across it.
//   - An entry counts only if its actor is the captain IN FORCE at that point
//     in chain order. An outgoing captain's confirmation that lands after a
//     newer captain_assigned is IGNORED (and counted), never folded.
//   - The workflow INVENTORY is not the chain's: callers pass the workflow
//     ids of the delegation view (backend/internal/delegationview) at the
//     relevant ref, so a never-confirmed workflow is reported unconfirmed
//     rather than silently absent.
//
// Long-form contract: backend/internal/delegationconfirm/README.md.
package delegationconfirm

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// The two delegation-confirmation audit categories. Each is a run-less
// (global-chain) entry keyed by payload.repo + payload.workflow and
// registered in audit.KnownCategories.
const (
	CategoryDelegationConfirmed     = "delegation_confirmed"
	CategoryDelegationLowerProposed = "delegation_lower_proposed"
)

// Categories returns the two categories in a stable order.
func Categories() []string {
	return []string{CategoryDelegationConfirmed, CategoryDelegationLowerProposed}
}

func isCategory(kind string) bool {
	return kind == CategoryDelegationConfirmed || kind == CategoryDelegationLowerProposed
}

// The CLOSED action set. There is no raise action and none can be added
// without editing this list, which TestActions_ClosedSet pins.
const (
	ActionRead    = "read"
	ActionConfirm = "confirm"
	ActionLower   = "lower"
)

// Actions returns the closed action set {read, confirm, lower}.
func Actions() []string { return []string{ActionRead, ActionConfirm, ActionLower} }

// One typed sentinel per refusal mode.
var (
	// ErrActorRequired — the verb carried no acting subject.
	ErrActorRequired = errors.New("delegationconfirm: an acting subject is required")
	// ErrAgentIdentity — an agent, run-bound or delegated identity attempted
	// a write verb. Confirmation is a human captain's act.
	ErrAgentIdentity = errors.New("delegationconfirm: agent, run-bound and delegated identities cannot confirm or lower delegation")
	// ErrNoCaptain — the seat is vacant; nobody can confirm.
	ErrNoCaptain = errors.New("delegationconfirm: the captain seat is vacant")
	// ErrNotCaptain — the actor is not the sitting captain.
	ErrNotCaptain = errors.New("delegationconfirm: only the sitting captain may confirm or lower delegation")
	// ErrWorkflowRequired — no workflow named.
	ErrWorkflowRequired = errors.New("delegationconfirm: a workflow is required")
	// ErrContentHashRequired — a confirm naming no content hash.
	ErrContentHashRequired = errors.New("delegationconfirm: a confirm must name the content_hash it confirms")
	// ErrFiledRefRequired — a lower entry naming no filed work item.
	ErrFiledRefRequired = errors.New("delegationconfirm: a lower proposal must name the filed work item")
	// ErrRaiseRefused — a lower proposal whose tier or escalation ceiling is
	// not below the workflow's current tier. There is no raise path.
	ErrRaiseRefused = errors.New("delegationconfirm: a proposal may only lower delegation; nothing may be raised")
	// ErrNothingProposed — a lower proposal that lowers nothing.
	ErrNothingProposed = errors.New("delegationconfirm: the proposal lowers nothing; name proposed_tier or proposed_escalation")
	// ErrReasonRequired — a lower proposal with no reason.
	ErrReasonRequired = errors.New("delegationconfirm: a lower proposal needs a reason")
	// ErrEscalationPathsRequired — a proposed escalation matching no paths.
	ErrEscalationPathsRequired = errors.New("delegationconfirm: a proposed escalation must match at least one path")
)

// Escalation is a proposed escalation: a path match plus an autonomy ceiling.
// It deliberately carries no approvals field, so nothing about it can be read
// as a raise.
type Escalation struct {
	Paths       []string `json:"paths"`
	MaxAutonomy string   `json:"max_autonomy"`
}

// Event is one delegation-confirmation chain entry before it is appended.
type Event struct {
	Kind             string
	Repo             string
	Workflow         string
	Subject          string
	IdentityVerified bool
	// Confirm only.
	ContentHash string
	WorkflowSHA string
	// Lower only.
	ProposedTier       string
	ProposedEscalation *Escalation
	Reason             string
	FiledRef           string
}

// payload is the snake_case wire shape of an Event and the decode target the
// fold reads entries back through.
type payload struct {
	Repo               string      `json:"repo"`
	Workflow           string      `json:"workflow"`
	Subject            string      `json:"subject"`
	IdentityVerified   bool        `json:"identity_verified"`
	ContentHash        string      `json:"content_hash,omitempty"`
	WorkflowSHA        string      `json:"workflow_sha,omitempty"`
	ProposedTier       string      `json:"proposed_tier,omitempty"`
	ProposedEscalation *Escalation `json:"proposed_escalation,omitempty"`
	Reason             string      `json:"reason,omitempty"`
	FiledRef           string      `json:"filed_ref,omitempty"`
}

// Payload renders the event as the JSON stored on its chain entry. A
// malformed event (wrong kind, no repo/workflow/subject) is refused so it can
// never be appended.
func (e Event) Payload() (json.RawMessage, error) {
	if !isCategory(e.Kind) {
		return nil, fmt.Errorf("delegationconfirm: event kind %q is not a delegation-confirmation category", e.Kind)
	}
	if e.Repo == "" || e.Workflow == "" || e.Subject == "" {
		return nil, fmt.Errorf("delegationconfirm: %s event needs repo, workflow and subject (repo=%q workflow=%q subject=%q)",
			e.Kind, e.Repo, e.Workflow, e.Subject)
	}
	p := payload{Repo: e.Repo, Workflow: e.Workflow, Subject: e.Subject, IdentityVerified: e.IdentityVerified}
	if e.Kind == CategoryDelegationConfirmed {
		p.ContentHash, p.WorkflowSHA = e.ContentHash, e.WorkflowSHA
	} else {
		p.ProposedTier, p.ProposedEscalation, p.Reason, p.FiledRef = e.ProposedTier, e.ProposedEscalation, e.Reason, e.FiledRef
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("delegationconfirm: marshal %s payload: %w", e.Kind, err)
	}
	return b, nil
}

// ChainEntry is the slice of an audit entry the fold reads — the captain
// package's type, so one read can serve both folds.
type ChainEntry = captain.ChainEntry

// Confirmation is one counted delegation_confirmed entry.
type Confirmation struct {
	Subject     string    `json:"subject"`
	ContentHash string    `json:"content_hash"`
	WorkflowSHA string    `json:"workflow_sha,omitempty"`
	Sequence    int64     `json:"sequence"`
	EntryHash   string    `json:"entry_hash"`
	At          time.Time `json:"at"`
}

// LowerProposal is one counted delegation_lower_proposed entry. A proposal
// does NOT confirm: the delegation in force is unchanged until the filed
// human-authored edit lands.
type LowerProposal struct {
	Subject            string      `json:"subject"`
	ProposedTier       string      `json:"proposed_tier,omitempty"`
	ProposedEscalation *Escalation `json:"proposed_escalation,omitempty"`
	Reason             string      `json:"reason"`
	FiledRef           string      `json:"filed_ref"`
	Sequence           int64       `json:"sequence"`
	EntryHash          string      `json:"entry_hash"`
	At                 time.Time   `json:"at"`
}

// State is the fold of a repository's captain + delegation-confirmation
// entries.
type State struct {
	Repo string
	// Captain is the captain in force after the last entry ("" = vacant).
	Captain string
	// SeatSequence is the sequence of the latest seat change
	// (captain_assigned or captain_claimed); 0 means no captain has ever sat.
	SeatSequence int64
	// Confirmations and Lowers hold, per workflow, the latest COUNTED entry
	// since SeatSequence.
	Confirmations map[string]Confirmation
	Lowers        map[string]LowerProposal
	// SkippedEntries counts confirmation entries that could not be used
	// (undecodable payload, another repo, no workflow or subject, unknown
	// category). IgnoredEntries counts well-formed entries whose actor was
	// not the captain in force when they landed.
	SkippedEntries int
	IgnoredEntries int
}

// Derive folds captainEntries and confirmEntries into State, in ASCENDING
// sequence order regardless of the order given. It is pure.
func Derive(repo string, captainEntries, confirmEntries []ChainEntry) State {
	type tagged struct {
		e         ChainEntry
		isCaptain bool
	}
	all := make([]tagged, 0, len(captainEntries)+len(confirmEntries))
	for _, e := range captainEntries {
		all = append(all, tagged{e, true})
	}
	for _, e := range confirmEntries {
		all = append(all, tagged{e, false})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].e.Sequence < all[j].e.Sequence })

	st := State{Repo: repo, Confirmations: map[string]Confirmation{}, Lowers: map[string]LowerProposal{}}
	var captainSoFar []ChainEntry
	for _, t := range all {
		e := t.e
		if t.isCaptain {
			captainSoFar = append(captainSoFar, e)
			cs := captain.Derive(repo, captainSoFar)
			st.Captain = ""
			if cs.Current != nil {
				st.Captain = cs.Current.Subject
				if cs.Current.AssignedSequence == e.Sequence {
					// A seat change: every earlier confirmation is void.
					st.SeatSequence = e.Sequence
					st.Confirmations = map[string]Confirmation{}
					st.Lowers = map[string]LowerProposal{}
				}
			}
			continue
		}
		var p payload
		if err := json.Unmarshal(e.Payload, &p); err != nil || p.Repo != repo || p.Workflow == "" || p.Subject == "" || !isCategory(e.Category) {
			st.SkippedEntries++
			continue
		}
		// The captain-in-force rule: an entry whose actor is not the captain
		// at this point in chain order never counts.
		if st.Captain == "" || p.Subject != st.Captain {
			st.IgnoredEntries++
			continue
		}
		if e.Category == CategoryDelegationConfirmed {
			st.Confirmations[p.Workflow] = Confirmation{
				Subject: p.Subject, ContentHash: p.ContentHash, WorkflowSHA: p.WorkflowSHA,
				Sequence: e.Sequence, EntryHash: e.EntryHash, At: e.Timestamp,
			}
		} else {
			st.Lowers[p.Workflow] = LowerProposal{
				Subject: p.Subject, ProposedTier: p.ProposedTier, ProposedEscalation: p.ProposedEscalation,
				Reason: p.Reason, FiledRef: p.FiledRef, Sequence: e.Sequence, EntryHash: e.EntryHash, At: e.Timestamp,
			}
		}
	}
	return st
}

// Workflow confirmation statuses and reasons.
const (
	StatusConfirmed   = "confirmed"
	StatusUnconfirmed = "unconfirmed"
	// StatusNoCaptain — no captain has ever sat, so no handover has happened
	// and nothing is awaiting confirmation. Not listed as unconfirmed.
	StatusNoCaptain = "no_captain"

	// ReasonHandover — no counted confirmation since the latest seat change.
	ReasonHandover = "handover"
	// ReasonHashStale — the confirmed content hash is no longer the current
	// one: the delegation changed after it was confirmed.
	ReasonHashStale = "hash_stale"
)

// WorkflowStatus is one workflow's confirmation verdict.
type WorkflowStatus struct {
	Workflow           string         `json:"workflow"`
	Status             string         `json:"status"`
	Reason             string         `json:"reason,omitempty"`
	CurrentContentHash string         `json:"current_content_hash,omitempty"`
	Confirmation       *Confirmation  `json:"confirmation,omitempty"`
	LowerProposal      *LowerProposal `json:"lower_proposal,omitempty"`
}

// Statuses renders the CHAIN-ONLY verdict for each workflow in the given
// inventory, in the order given. No spec is read: a workflow confirmed since
// the latest seat change is confirmed whatever its current hash — pass the
// result through ApplyCurrentHashes to add the staleness half.
func Statuses(st State, workflows []string) []WorkflowStatus {
	out := make([]WorkflowStatus, 0, len(workflows))
	for _, wf := range workflows {
		ws := WorkflowStatus{Workflow: wf}
		if lp, ok := st.Lowers[wf]; ok {
			lp := lp
			ws.LowerProposal = &lp
		}
		switch c, ok := st.Confirmations[wf]; {
		case st.SeatSequence == 0:
			ws.Status = StatusNoCaptain
		case ok:
			c := c
			ws.Status, ws.Confirmation = StatusConfirmed, &c
		default:
			ws.Status, ws.Reason = StatusUnconfirmed, ReasonHandover
		}
		out = append(out, ws)
	}
	return out
}

// ApplyCurrentHashes is the hash-staleness half: a CONFIRMED workflow whose
// recorded content hash differs from its current delegationview hash becomes
// unconfirmed with ReasonHashStale. current maps workflow id -> hash; a
// workflow absent from current is left as it is. It returns a new slice.
func ApplyCurrentHashes(in []WorkflowStatus, current map[string]string) []WorkflowStatus {
	out := make([]WorkflowStatus, len(in))
	copy(out, in)
	for i := range out {
		h, ok := current[out[i].Workflow]
		if !ok {
			continue
		}
		out[i].CurrentContentHash = h
		if out[i].Status == StatusConfirmed && out[i].Confirmation != nil && out[i].Confirmation.ContentHash != h {
			out[i].Status, out[i].Reason = StatusUnconfirmed, ReasonHashStale
		}
	}
	return out
}

// Unconfirmed returns the ids of the unconfirmed workflows, in input order,
// never nil.
func Unconfirmed(in []WorkflowStatus) []string {
	out := []string{}
	for _, ws := range in {
		if ws.Status == StatusUnconfirmed {
			out = append(out, ws.Workflow)
		}
	}
	return out
}

// Params is the input the two write transitions take.
type Params struct {
	Repo     string
	Workflow string
	Actor    string
	// ActorIsAgent / ActorIsDelegated are computed by the caller and refused
	// only by GuardActor.
	ActorIsAgent     bool
	ActorIsDelegated bool
	// Confirm only.
	ContentHash string
	WorkflowSHA string
	// Lower only.
	Lower    LowerRequest
	FiledRef string
}

// GuardActor is the ONE agent/run-bound/delegated refusal for both write
// verbs. The server calls it BEFORE any spec fetch or filing, so a lower by
// an agent files nothing.
func GuardActor(p Params) error {
	if p.Actor == "" {
		return ErrActorRequired
	}
	if p.ActorIsAgent || p.ActorIsDelegated {
		return ErrAgentIdentity
	}
	return nil
}

// CheckCaptain refuses unless actor is the captain in force.
func CheckCaptain(st State, actor string) error {
	if st.Captain == "" {
		return ErrNoCaptain
	}
	if st.Captain != actor {
		return ErrNotCaptain
	}
	return nil
}

// Confirm decides a confirmation. The Store runs it on state read under the
// captain record's lock, so the captain check and the append cannot be split
// by a handover.
func Confirm(st State, p Params) (Event, error) {
	if p.Workflow == "" {
		return Event{}, ErrWorkflowRequired
	}
	if p.ContentHash == "" {
		return Event{}, ErrContentHashRequired
	}
	if err := CheckCaptain(st, p.Actor); err != nil {
		return Event{}, err
	}
	return Event{
		Kind: CategoryDelegationConfirmed, Repo: p.Repo, Workflow: p.Workflow, Subject: p.Actor,
		IdentityVerified: captain.IdentityVerified(p.Actor),
		ContentHash:      p.ContentHash, WorkflowSHA: p.WorkflowSHA,
	}, nil
}

// ProposeLower decides the lower-proposal entry recorded AFTER the work item
// was filed. It re-checks the captain under the lock.
func ProposeLower(st State, p Params) (Event, error) {
	if p.Workflow == "" {
		return Event{}, ErrWorkflowRequired
	}
	if p.FiledRef == "" {
		return Event{}, ErrFiledRefRequired
	}
	if err := CheckCaptain(st, p.Actor); err != nil {
		return Event{}, err
	}
	return Event{
		Kind: CategoryDelegationLowerProposed, Repo: p.Repo, Workflow: p.Workflow, Subject: p.Actor,
		IdentityVerified: captain.IdentityVerified(p.Actor),
		ProposedTier:     p.Lower.ProposedTier, ProposedEscalation: p.Lower.ProposedEscalation,
		Reason: p.Lower.Reason, FiledRef: p.FiledRef,
	}, nil
}

// LowerRequest is a proposed lowering. Every tier-bearing field is validated
// against the workflow's current tier by ValidateLower.
type LowerRequest struct {
	ProposedTier       string
	ProposedEscalation *Escalation
	Reason             string
}

// tierRank ranks a tier; 0 is "not a tier".
func tierRank(t string) int {
	switch spec.AutonomyTier(t) {
	case spec.TierLow:
		return 1
	case spec.TierMedium:
		return 2
	case spec.TierHigh:
		return 3
	}
	return 0
}

// ValidateLower admits a proposal only if it strictly lowers delegation:
//
//   - ProposedTier, when set, must rank STRICTLY below currentTier.
//   - ProposedEscalation, when set, must match at least one path and declare
//     a max_autonomy at or below the effective proposed tier (ProposedTier
//     when set, else currentTier); with no ProposedTier it must be STRICTLY
//     below currentTier, since a ceiling equal to the tier lowers nothing.
//   - An undeclared or unrecognized currentTier admits nothing: a lowering
//     that cannot be proven lower is refused (fail closed).
func ValidateLower(currentTier string, req LowerRequest) error {
	if strings.TrimSpace(req.Reason) == "" {
		return ErrReasonRequired
	}
	if req.ProposedTier == "" && req.ProposedEscalation == nil {
		return ErrNothingProposed
	}
	cur := tierRank(currentTier)
	if cur == 0 {
		return ErrRaiseRefused
	}
	effective := cur
	if req.ProposedTier != "" {
		pr := tierRank(req.ProposedTier)
		if pr == 0 || pr >= cur {
			return ErrRaiseRefused
		}
		effective = pr
	}
	if esc := req.ProposedEscalation; esc != nil {
		if len(esc.Paths) == 0 {
			return ErrEscalationPathsRequired
		}
		ceil := tierRank(esc.MaxAutonomy)
		if ceil == 0 || ceil > effective {
			return ErrRaiseRefused
		}
		if req.ProposedTier == "" && ceil >= cur {
			return ErrNothingProposed
		}
	}
	return nil
}
