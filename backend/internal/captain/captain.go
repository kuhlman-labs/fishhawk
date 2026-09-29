// Package captain holds the captain record for a repository (E76.2 / #3765,
// implementing ADR-083 #3751 rules 2-5): the five global-chain event kinds a
// handover or fallback claim writes, the PURE fold (Derive) that reconstructs
// the current captain, any pending offer and the last captain before a vacancy
// from those entries, the pure transition rules for the five verbs (see
// transition.go), and the Store that serializes read -> derive -> validate ->
// append inside one advisory-locked transaction (see store.go).
//
// The chain is the sole authority: there is no derived table and no
// migration. Nothing here is read by approval quorum or eligibility.
package captain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The five captain-record audit categories. Each is a run-less (global-chain)
// entry keyed by payload.repo and registered in audit.KnownCategories.
const (
	CategoryHandoverOffered   = "captain_handover_offered"
	CategoryHandoverWithdrawn = "captain_handover_withdrawn"
	CategoryAssigned          = "captain_assigned"
	CategoryRelinquished      = "captain_relinquished"
	CategoryClaimed           = "captain_claimed"
)

// Categories returns the five captain categories in a stable order — the set
// the Store's read filters audit_entries on.
func Categories() []string {
	return []string{
		CategoryAssigned,
		CategoryClaimed,
		CategoryHandoverOffered,
		CategoryHandoverWithdrawn,
		CategoryRelinquished,
	}
}

func isCategory(kind string) bool {
	for _, c := range Categories() {
		if c == kind {
			return true
		}
	}
	return false
}

// Basis names how the current captain came to sit.
type Basis string

const (
	// BasisAssigned is a seat taken by accepting a handover offer.
	BasisAssigned Basis = "assigned"
	// BasisClaimed is a seat taken by a fallback claim at a vacant seat.
	BasisClaimed Basis = "claimed"
)

// Event is one captain-record chain entry before it is appended. Kind is its
// audit category. Subject is the acting identity (the offering captain, the
// withdrawing captain, the accepting successor, the relinquishing captain, or
// the claimant) and IdentityVerified is Subject's provider qualification.
// ClaimVerified is set only on a claim: true when a non-trivial repo predicate
// was checked and satisfied, false when the predicate was positively trivial —
// it is deliberately SEPARATE from IdentityVerified.
type Event struct {
	Kind                      string
	Repo                      string
	Subject                   string
	IdentityVerified          bool
	Successor                 string
	SuccessorIdentityVerified bool
	OfferEntryHash            string
	PreviousCaptain           string
	ClaimVerified             *bool
	PredicateBasis            string
	PagePending               bool
}

// payload is the snake_case wire shape of an Event, and the decode target
// Derive reads entries back through.
type payload struct {
	Repo                      string `json:"repo"`
	Subject                   string `json:"subject"`
	IdentityVerified          bool   `json:"identity_verified"`
	Successor                 string `json:"successor,omitempty"`
	SuccessorIdentityVerified *bool  `json:"successor_identity_verified,omitempty"`
	OfferEntryHash            string `json:"offer_entry_hash,omitempty"`
	PreviousCaptain           string `json:"previous_captain,omitempty"`
	ClaimVerified             *bool  `json:"claim_verified,omitempty"`
	PredicateBasis            string `json:"predicate_basis,omitempty"`
	PagePending               bool   `json:"page_pending,omitempty"`
}

// Payload renders the event as the JSON stored on its chain entry. It refuses
// an event whose Kind is not a captain category or that names no repo or
// subject, so a malformed event can never be appended.
func (e Event) Payload() (json.RawMessage, error) {
	if !isCategory(e.Kind) {
		return nil, fmt.Errorf("captain: event kind %q is not a captain category", e.Kind)
	}
	if e.Repo == "" || e.Subject == "" {
		return nil, fmt.Errorf("captain: %s event needs repo and subject (repo=%q subject=%q)", e.Kind, e.Repo, e.Subject)
	}
	p := payload{
		Repo:             e.Repo,
		Subject:          e.Subject,
		IdentityVerified: e.IdentityVerified,
		Successor:        e.Successor,
		OfferEntryHash:   e.OfferEntryHash,
		PreviousCaptain:  e.PreviousCaptain,
		ClaimVerified:    e.ClaimVerified,
		PredicateBasis:   e.PredicateBasis,
		PagePending:      e.PagePending,
	}
	if e.Kind == CategoryHandoverOffered {
		v := e.SuccessorIdentityVerified
		p.SuccessorIdentityVerified = &v
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("captain: marshal %s payload: %w", e.Kind, err)
	}
	return b, nil
}

// ChainEntry is the slice of an audit entry Derive folds over.
type ChainEntry struct {
	Sequence  int64
	EntryHash string
	Category  string
	Timestamp time.Time
	Payload   json.RawMessage
}

// Record is the derived current captain.
type Record struct {
	Subject          string
	IdentityVerified bool
	// ClaimVerified is nil for an assigned (handed-over) seat, and
	// true/false only for a claimed one.
	ClaimVerified     *bool
	Basis             Basis
	AssignedSequence  int64
	AssignedEntryHash string
	AssignedAt        time.Time
}

// HandoverOffer is the derived pending handover offer. EntryHash is the
// captain_handover_offered entry's entry_hash — the offer's identity, which a
// withdraw or accept names as offer_entry_hash.
type HandoverOffer struct {
	Successor                 string
	SuccessorIdentityVerified bool
	OfferedBy                 string
	EntryHash                 string
	Sequence                  int64
	OfferedAt                 time.Time
}

// State is the fold of a repository's captain chain. LastCaptain is the most
// recent captain to leave the seat (by relinquish or by being handed over
// from) — what a later claim records as previous_captain. SkippedEntries
// counts entries Derive could not use (undecodable payload, another repo, a
// withdraw naming no pending offer); they are skipped, never guessed at.
type State struct {
	Current        *Record
	PendingOffer   *HandoverOffer
	LastCaptain    *string
	SkippedEntries int
}

// Derive folds entries into the repository's captain State, in ASCENDING
// sequence order regardless of the order given. It is pure: no I/O, no clock.
func Derive(repo string, entries []ChainEntry) State {
	sorted := make([]ChainEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Sequence < sorted[j].Sequence })

	var st State
	for _, e := range sorted {
		var p payload
		if err := json.Unmarshal(e.Payload, &p); err != nil || p.Repo != repo || p.Subject == "" {
			st.SkippedEntries++
			continue
		}
		switch e.Category {
		case CategoryHandoverOffered:
			if p.Successor == "" {
				st.SkippedEntries++
				continue
			}
			sv := p.SuccessorIdentityVerified != nil && *p.SuccessorIdentityVerified
			st.PendingOffer = &HandoverOffer{
				Successor:                 p.Successor,
				SuccessorIdentityVerified: sv,
				OfferedBy:                 p.Subject,
				EntryHash:                 e.EntryHash,
				Sequence:                  e.Sequence,
				OfferedAt:                 e.Timestamp,
			}
		case CategoryHandoverWithdrawn:
			if st.PendingOffer == nil || st.PendingOffer.EntryHash != p.OfferEntryHash {
				st.SkippedEntries++
				continue
			}
			st.PendingOffer = nil
		case CategoryAssigned:
			if st.Current != nil {
				last := st.Current.Subject
				st.LastCaptain = &last
			}
			st.Current = &Record{
				Subject:           p.Subject,
				IdentityVerified:  p.IdentityVerified,
				Basis:             BasisAssigned,
				AssignedSequence:  e.Sequence,
				AssignedEntryHash: e.EntryHash,
				AssignedAt:        e.Timestamp,
			}
			st.PendingOffer = nil
		case CategoryRelinquished:
			last := p.Subject
			st.LastCaptain = &last
			st.Current = nil
			st.PendingOffer = nil
		case CategoryClaimed:
			var cv *bool
			if p.ClaimVerified != nil {
				v := *p.ClaimVerified
				cv = &v
			}
			st.Current = &Record{
				Subject:           p.Subject,
				IdentityVerified:  p.IdentityVerified,
				ClaimVerified:     cv,
				Basis:             BasisClaimed,
				AssignedSequence:  e.Sequence,
				AssignedEntryHash: e.EntryHash,
				AssignedAt:        e.Timestamp,
			}
			st.PendingOffer = nil
		default:
			st.SkippedEntries++
		}
	}
	return st
}

// IdentityVerified reports whether subject is provider-qualified — a
// "github:" or "gitlab:" subject resolved through a forge identity. A static
// token subject such as "brett@local-mcp" carries false, and every surface
// that shows the captain renders that flag.
func IdentityVerified(subject string) bool {
	for _, prefix := range []string{"github:", "gitlab:"} {
		if strings.HasPrefix(subject, prefix) && len(subject) > len(prefix) {
			return true
		}
	}
	return false
}
