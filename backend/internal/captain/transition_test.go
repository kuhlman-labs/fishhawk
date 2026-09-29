package captain

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func held(subject string) State {
	return State{Current: &Record{Subject: subject, Basis: BasisAssigned}}
}

func withOffer(captain, successor string) State {
	st := held(captain)
	st.PendingOffer = &HandoverOffer{Successor: successor, OfferedBy: captain, EntryHash: "h-offer", Sequence: 7}
	return st
}

func params(actor string) Params {
	return Params{Repo: testRepo, Actor: actor}
}

type verb func(State, Params) (Event, error)

// assertRefused pins a refusal on error IDENTITY and on the zero Event, so a
// refusal can never also emit an event.
func assertRefused(t *testing.T, ev Event, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if !reflect.DeepEqual(ev, Event{}) {
		t.Errorf("refusal returned a non-zero event %+v", ev)
	}
}

func payloadMap(t *testing.T, ev Event) map[string]any {
	t.Helper()
	b, err := ev.Payload()
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// admittingFixtures is, per verb, a state + params on which the verb SUCCEEDS
// for a human actor — so the agent/delegated refusal is the only thing in the
// path when the flags are set.
func admittingFixtures() map[string]struct {
	fn verb
	st State
	p  Params
} {
	claim := params("github:bob")
	claim.Predicate = PredicateTrivial
	offer := params("github:alice")
	offer.Successor = "github:carol"
	return map[string]struct {
		fn verb
		st State
		p  Params
	}{
		"offer":      {Offer, held("github:alice"), offer},
		"withdraw":   {Withdraw, withOffer("github:alice", "github:carol"), params("github:alice")},
		"accept":     {Accept, withOffer("github:alice", "github:carol"), params("github:carol")},
		"relinquish": {Relinquish, held("github:alice"), params("github:alice")},
		"claim":      {Claim, State{}, claim},
	}
}

// TestTransition_AgentRefusedOnEveryVerb pins the ONE agent/delegated guard
// (guardActor) on all five verbs, for both flags, on fixtures where the verb
// otherwise succeeds (asserted first, so the fixture isolates the guard).
func TestTransition_AgentRefusedOnEveryVerb(t *testing.T) {
	for name, fx := range admittingFixtures() {
		t.Run(name, func(t *testing.T) {
			if _, err := fx.fn(fx.st, fx.p); err != nil {
				t.Fatalf("fixture does not admit a human actor: %v", err)
			}
			agent := fx.p
			agent.ActorIsAgent = true
			ev, err := fx.fn(fx.st, agent)
			assertRefused(t, ev, err, ErrAgentIdentity)

			delegated := fx.p
			delegated.ActorIsDelegated = true
			ev, err = fx.fn(fx.st, delegated)
			assertRefused(t, ev, err, ErrAgentIdentity)
		})
	}
}

func TestTransition_ActorRequiredOnEveryVerb(t *testing.T) {
	for name, fx := range admittingFixtures() {
		t.Run(name, func(t *testing.T) {
			p := fx.p
			p.Actor = ""
			ev, err := fx.fn(fx.st, p)
			assertRefused(t, ev, err, ErrActorRequired)
		})
	}
}

// C2: only the sitting captain may offer.
func TestTransition_OfferByNonCaptainRefused(t *testing.T) {
	p := params("github:bob")
	p.Successor = "github:carol"
	ev, err := Offer(held("github:alice"), p)
	assertRefused(t, ev, err, ErrNotCaptain)
}

func TestTransition_OfferWhileVacantRefused(t *testing.T) {
	p := params("github:alice")
	p.Successor = "github:carol"
	ev, err := Offer(State{}, p)
	assertRefused(t, ev, err, ErrNoCaptain)
}

func TestTransition_OfferWithoutSuccessorRefused(t *testing.T) {
	ev, err := Offer(held("github:alice"), params("github:alice"))
	assertRefused(t, ev, err, ErrSuccessorRequired)
}

func TestTransition_OfferToSelfRefused(t *testing.T) {
	p := params("github:alice")
	p.Successor = "github:alice"
	ev, err := Offer(held("github:alice"), p)
	assertRefused(t, ev, err, ErrSelfHandover)
}

// C6 (approval condition 1): the ONE withdraw-authorization guard. The offer
// exists and the actor is a human, so ErrAgentIdentity and ErrNoOffer are
// inert — ErrNotOfferer is the only thing in the path.
func TestTransition_WithdrawByNonOffererRefused(t *testing.T) {
	ev, err := Withdraw(withOffer("github:alice", "github:carol"), params("github:bob"))
	assertRefused(t, ev, err, ErrNotOfferer)
}

func TestTransition_WithdrawWithoutOfferRefused(t *testing.T) {
	ev, err := Withdraw(held("github:alice"), params("github:alice"))
	assertRefused(t, ev, err, ErrNoOffer)
}

// C3: accept requires a pending offer.
func TestTransition_AcceptWithoutOfferRefused(t *testing.T) {
	ev, err := Accept(held("github:alice"), params("github:bob"))
	assertRefused(t, ev, err, ErrNoOffer)
}

// C5: accept only by the NAMED successor. bob is human, the offer exists and
// the seat is held, so ErrAgentIdentity / ErrNoOffer / ErrCaptainExists are
// all inert.
func TestTransition_AcceptBySomeoneOtherThanSuccessorRefused(t *testing.T) {
	ev, err := Accept(withOffer("github:alice", "github:carol"), params("github:bob"))
	assertRefused(t, ev, err, ErrOfferSuccessorMismatch)
}

func TestTransition_RelinquishWhileVacantRefused(t *testing.T) {
	ev, err := Relinquish(State{}, params("github:alice"))
	assertRefused(t, ev, err, ErrNoCaptain)
}

// C7 (pure vehicle): relinquish only by the current captain.
func TestTransition_RelinquishByNonCaptainRefused(t *testing.T) {
	ev, err := Relinquish(held("github:alice"), params("github:bob"))
	assertRefused(t, ev, err, ErrNotCaptain)
}

// C4: claim is refused while a captain sits — with a SATISFIED non-trivial
// predicate, so this branch is the only thing refusing.
func TestTransition_ClaimWhileCaptainExistsRefused(t *testing.T) {
	p := params("github:bob")
	p.Predicate = PredicateNonTrivialSatisfied
	ev, err := Claim(held("github:alice"), p)
	assertRefused(t, ev, err, ErrCaptainExists)
}

// One test per PredicateOutcome value.

func TestTransition_ClaimNonTrivialSatisfiedIsVerified(t *testing.T) {
	p := params("github:bob")
	p.Predicate = PredicateNonTrivialSatisfied
	p.PredicateBasis = "min_permission:admin"
	ev, err := Claim(State{}, p)
	if err != nil {
		t.Fatal(err)
	}
	m := payloadMap(t, ev)
	if m["claim_verified"] != true || m["identity_verified"] != true || m["predicate_basis"] != "min_permission:admin" {
		t.Errorf("payload = %v, want claim_verified true, identity_verified true, basis min_permission:admin", m)
	}
}

// C11 (pure vehicle): a trivial predicate records claim_verified FALSE.
func TestTransition_ClaimTrivialIsUnverified(t *testing.T) {
	p := params("github:bob")
	p.Predicate = PredicateTrivial
	p.PredicateBasis = "trivial:any-non-agent-token-holder"
	ev, err := Claim(State{}, p)
	if err != nil {
		t.Fatal(err)
	}
	m := payloadMap(t, ev)
	v, ok := m["claim_verified"]
	if !ok || v != false {
		t.Errorf("claim_verified = %v (present=%v), want false", v, ok)
	}
	if m["identity_verified"] != true {
		t.Errorf("identity_verified = %v, want true for github:bob — the two flags are separate", m["identity_verified"])
	}
}

// C8 (pure vehicle).
func TestTransition_ClaimNonTrivialRejectedRefused(t *testing.T) {
	p := params("github:bob")
	p.Predicate = PredicateNonTrivialRejected
	ev, err := Claim(State{}, p)
	assertRefused(t, ev, err, ErrPredicateRejected)
}

// C9 (pure vehicle): an undeterminable predicate fails CLOSED with its own
// named sentinel.
func TestTransition_ClaimUndeterminableRefused(t *testing.T) {
	p := params("github:bob")
	p.Predicate = PredicateUndeterminable
	ev, err := Claim(State{}, p)
	assertRefused(t, ev, err, ErrPredicateUndeterminable)
}

func TestTransition_ClaimInvalidPredicateOutcomeRefused(t *testing.T) {
	for _, o := range []PredicateOutcome{0, 99} {
		p := params("github:bob")
		p.Predicate = o
		ev, err := Claim(State{}, p)
		assertRefused(t, ev, err, ErrPredicateOutcomeInvalid)
	}
}

func TestTransition_OfferEvent(t *testing.T) {
	p := params("github:alice")
	p.Successor = "carol@local-mcp"
	ev, err := Offer(held("github:alice"), p)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != CategoryHandoverOffered {
		t.Errorf("kind = %s", ev.Kind)
	}
	m := payloadMap(t, ev)
	if m["repo"] != testRepo || m["subject"] != "github:alice" || m["successor"] != "carol@local-mcp" ||
		m["identity_verified"] != true || m["successor_identity_verified"] != false {
		t.Errorf("offer payload = %v", m)
	}
}

func TestTransition_WithdrawEventNamesTheOffer(t *testing.T) {
	ev, err := Withdraw(withOffer("github:alice", "github:carol"), params("github:alice"))
	if err != nil {
		t.Fatal(err)
	}
	m := payloadMap(t, ev)
	if ev.Kind != CategoryHandoverWithdrawn || m["offer_entry_hash"] != "h-offer" || m["successor"] != "github:carol" {
		t.Errorf("withdraw %s payload = %v", ev.Kind, m)
	}
}

func TestTransition_AcceptEventRecordsPreviousCaptain(t *testing.T) {
	ev, err := Accept(withOffer("github:alice", "github:carol"), params("github:carol"))
	if err != nil {
		t.Fatal(err)
	}
	m := payloadMap(t, ev)
	if ev.Kind != CategoryAssigned || m["subject"] != "github:carol" || m["previous_captain"] != "github:alice" || m["offer_entry_hash"] != "h-offer" {
		t.Errorf("accept %s payload = %v", ev.Kind, m)
	}
	if _, ok := m["claim_verified"]; ok {
		t.Errorf("an assignment must carry no claim_verified: %v", m)
	}
}

func TestTransition_RelinquishEvent(t *testing.T) {
	ev, err := Relinquish(held("github:alice"), params("github:alice"))
	if err != nil {
		t.Fatal(err)
	}
	m := payloadMap(t, ev)
	if ev.Kind != CategoryRelinquished || m["subject"] != "github:alice" {
		t.Errorf("relinquish %s payload = %v", ev.Kind, m)
	}
}

// Constraint 4: a claim after a relinquish records previous_captain from
// State.LastCaptain, and every claim records page_pending.
func TestTransition_ClaimAfterRelinquishRecordsPreviousCaptain(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice"})
	c.add(CategoryRelinquished, map[string]any{"subject": "github:alice"})
	p := params("github:bob")
	p.Predicate = PredicateTrivial
	ev, err := Claim(Derive(testRepo, c.entries), p)
	if err != nil {
		t.Fatal(err)
	}
	m := payloadMap(t, ev)
	if m["previous_captain"] != "github:alice" || m["page_pending"] != true {
		t.Errorf("claim payload = %v, want previous_captain github:alice and page_pending true", m)
	}
}

func TestTransition_FirstClaimHasNoPreviousCaptain(t *testing.T) {
	p := params("github:bob")
	p.Predicate = PredicateTrivial
	ev, err := Claim(State{}, p)
	if err != nil {
		t.Fatal(err)
	}
	m := payloadMap(t, ev)
	if _, ok := m["previous_captain"]; ok {
		t.Errorf("first-ever claim carries previous_captain: %v", m)
	}
	if m["page_pending"] != true {
		t.Errorf("page_pending = %v, want true", m["page_pending"])
	}
}
