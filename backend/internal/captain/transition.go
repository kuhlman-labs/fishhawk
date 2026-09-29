package captain

import (
	"errors"
	"fmt"
)

// One typed sentinel per refusal mode. Each transition returns exactly one of
// these (errors.Is-comparable) and the zero Event, so a refusal can never also
// emit an event, and no refusal mode collapses into another at the surface.
var (
	// ErrActorRequired — the verb carried no acting subject.
	ErrActorRequired = errors.New("captain: an acting subject is required")
	// ErrAgentIdentity — an agent or delegated identity attempted a verb.
	// Agents are structurally excluded from the captain record.
	ErrAgentIdentity = errors.New("captain: agent and delegated identities cannot hold, hand over or claim the captain seat")
	// ErrNotCaptain — offer or relinquish by someone other than the sitting captain.
	ErrNotCaptain = errors.New("captain: only the sitting captain may do this")
	// ErrNoCaptain — offer or relinquish while the seat is vacant.
	ErrNoCaptain = errors.New("captain: the seat is vacant")
	// ErrNoOffer — accept or withdraw with no pending handover offer.
	ErrNoOffer = errors.New("captain: no handover offer is pending")
	// ErrOfferSuccessorMismatch — accept by someone other than the named successor.
	ErrOfferSuccessorMismatch = errors.New("captain: the pending offer names a different successor")
	// ErrNotOfferer — withdraw by someone other than the captain who made the offer.
	ErrNotOfferer = errors.New("captain: only the captain who made the offer may withdraw it")
	// ErrCaptainExists — claim while a captain sits.
	ErrCaptainExists = errors.New("captain: the seat is held; a claim is only possible at a vacant seat")
	// ErrSelfHandover — offer naming the offering captain as successor.
	ErrSelfHandover = errors.New("captain: cannot offer the seat to yourself")
	// ErrSuccessorRequired — offer naming no successor.
	ErrSuccessorRequired = errors.New("captain: an offer must name a successor")
	// ErrPredicateRejected — the claimant fails the repository's non-trivial
	// approval predicate.
	ErrPredicateRejected = errors.New("captain: the claimant does not satisfy the repository's approval predicate")
	// ErrPredicateUndeterminable — the repository's predicate could not be
	// resolved (no run, no cached spec, unparseable spec, identity provider
	// unavailable). Fails CLOSED: an unreadable predicate is never trivial.
	ErrPredicateUndeterminable = errors.New("captain: the repository's approval predicate could not be determined")
	// ErrPredicateOutcomeInvalid — the caller passed a PredicateOutcome that is
	// none of the four defined values — a programming error, refused.
	ErrPredicateOutcomeInvalid = errors.New("captain: unrecognized predicate outcome")
)

// PredicateOutcome is the claim's predicate-resolution trichotomy (with the
// non-trivial arm split by result). The zero value is deliberately not a
// valid outcome.
type PredicateOutcome int

const (
	// PredicateNonTrivialSatisfied — a declared predicate was checked and the
	// claimant satisfies it — the claim is admitted with claim_verified true.
	PredicateNonTrivialSatisfied PredicateOutcome = iota + 1
	// PredicateNonTrivialRejected — a declared predicate was checked and the
	// claimant fails it — refused ErrPredicateRejected.
	PredicateNonTrivialRejected
	// PredicateTrivial — the spec parsed and POSITIVELY declares no
	// min_permission and no member_of — admitted with claim_verified false.
	PredicateTrivial
	// PredicateUndeterminable — the predicate could not be resolved — refused
	// ErrPredicateUndeterminable.
	PredicateUndeterminable
)

// Params is the input every transition takes. ActorIsAgent and
// ActorIsDelegated are computed by the caller (the server derives them from
// the token subject and the request identity) but REFUSED only here, in
// guardActor — the single authoritative agent/delegated check.
type Params struct {
	Repo             string
	Actor            string
	ActorIsAgent     bool
	ActorIsDelegated bool
	// Successor is the offer's named successor (Offer only).
	Successor string
	// Brief is the handover brief the offer records (Offer only); it is
	// copied onto the event verbatim — composing it is the caller's job,
	// because composition does I/O and a transition is pure.
	Brief OfferBrief
	// Predicate and PredicateBasis are the claim's resolved predicate (Claim
	// only). PredicateBasis names WHY, e.g. "min_permission:admin",
	// "trivial:any-non-agent-token-holder", "undeterminable:spec_unparseable".
	Predicate      PredicateOutcome
	PredicateBasis string
}

// guardActor is the ONE agent/delegated refusal for all five verbs.
func guardActor(p Params) error {
	if p.Actor == "" {
		return ErrActorRequired
	}
	if p.ActorIsAgent || p.ActorIsDelegated {
		return ErrAgentIdentity
	}
	return nil
}

// Offer decides one verb: the sitting captain offers the seat to a named successor.
func Offer(s State, p Params) (Event, error) {
	if err := guardActor(p); err != nil {
		return Event{}, err
	}
	if s.Current == nil {
		return Event{}, ErrNoCaptain
	}
	if s.Current.Subject != p.Actor {
		return Event{}, ErrNotCaptain
	}
	if p.Successor == "" {
		return Event{}, ErrSuccessorRequired
	}
	if p.Successor == p.Actor {
		return Event{}, ErrSelfHandover
	}
	return Event{
		Kind:                      CategoryHandoverOffered,
		Repo:                      p.Repo,
		Subject:                   p.Actor,
		IdentityVerified:          IdentityVerified(p.Actor),
		Successor:                 p.Successor,
		SuccessorIdentityVerified: IdentityVerified(p.Successor),
		Brief:                     p.Brief,
	}, nil
}

// Withdraw decides one verb: the offering captain withdraws the pending offer. Only the sitting
// captain can offer, so the offerer and the captain coincide; ErrNotOfferer is
// the ONE withdraw-authorization check (approval condition 1).
func Withdraw(s State, p Params) (Event, error) {
	if err := guardActor(p); err != nil {
		return Event{}, err
	}
	if s.PendingOffer == nil {
		return Event{}, ErrNoOffer
	}
	if s.PendingOffer.OfferedBy != p.Actor {
		return Event{}, ErrNotOfferer
	}
	return Event{
		Kind:             CategoryHandoverWithdrawn,
		Repo:             p.Repo,
		Subject:          p.Actor,
		IdentityVerified: IdentityVerified(p.Actor),
		Successor:        s.PendingOffer.Successor,
		OfferEntryHash:   s.PendingOffer.EntryHash,
	}, nil
}

// Accept decides one verb: the named successor accepts the pending offer and becomes captain.
func Accept(s State, p Params) (Event, error) {
	if err := guardActor(p); err != nil {
		return Event{}, err
	}
	if s.PendingOffer == nil {
		return Event{}, ErrNoOffer
	}
	if s.PendingOffer.Successor != p.Actor {
		return Event{}, ErrOfferSuccessorMismatch
	}
	ev := Event{
		Kind:             CategoryAssigned,
		Repo:             p.Repo,
		Subject:          p.Actor,
		IdentityVerified: IdentityVerified(p.Actor),
		OfferEntryHash:   s.PendingOffer.EntryHash,
		PreviousCaptain:  s.PendingOffer.OfferedBy,
	}
	if s.Current != nil {
		ev.PreviousCaptain = s.Current.Subject
	}
	return ev, nil
}

// Relinquish decides one verb: the sitting captain vacates the seat.
func Relinquish(s State, p Params) (Event, error) {
	if err := guardActor(p); err != nil {
		return Event{}, err
	}
	if s.Current == nil {
		return Event{}, ErrNoCaptain
	}
	if s.Current.Subject != p.Actor {
		return Event{}, ErrNotCaptain
	}
	return Event{
		Kind:             CategoryRelinquished,
		Repo:             p.Repo,
		Subject:          p.Actor,
		IdentityVerified: IdentityVerified(p.Actor),
	}, nil
}

// Claim decides one verb: a fallback claim of a VACANT seat, gated on the repository's
// predicate. Every admitted claim records previous_captain (from
// State.LastCaptain) when one exists, and page_pending — the recorded
// obligation to page, whose delivery is E77.6 #3740 / E60.3 #2292.
func Claim(s State, p Params) (Event, error) {
	if err := guardActor(p); err != nil {
		return Event{}, err
	}
	if s.Current != nil {
		return Event{}, ErrCaptainExists
	}
	var verified bool
	switch p.Predicate {
	case PredicateUndeterminable:
		return Event{}, ErrPredicateUndeterminable
	case PredicateNonTrivialRejected:
		return Event{}, ErrPredicateRejected
	case PredicateTrivial:
		verified = false
	case PredicateNonTrivialSatisfied:
		verified = true
	default:
		return Event{}, fmt.Errorf("%w: %d", ErrPredicateOutcomeInvalid, int(p.Predicate))
	}
	ev := Event{
		Kind:             CategoryClaimed,
		Repo:             p.Repo,
		Subject:          p.Actor,
		IdentityVerified: IdentityVerified(p.Actor),
		ClaimVerified:    &verified,
		PredicateBasis:   p.PredicateBasis,
		PagePending:      true,
	}
	if s.LastCaptain != nil {
		ev.PreviousCaptain = *s.LastCaptain
	}
	return ev, nil
}
