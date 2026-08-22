package workmgmt

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mutatorCapableProvider is a Provider that ALSO implements GroomingMutator —
// the shape the GitHub Projects provider has.
type mutatorCapableProvider struct {
	name string
	got  GroomingMutationRequest
}

func (p *mutatorCapableProvider) Name() string { return p.name }
func (p *mutatorCapableProvider) File(context.Context, ProviderRequest) (*CreatedItem, error) {
	return &CreatedItem{Provider: p.name}, nil
}
func (p *mutatorCapableProvider) ApplyGroomingMutation(_ context.Context, req GroomingMutationRequest) (*GroomingMutationResult, error) {
	p.got = req
	return &GroomingMutationResult{Applied: true, ProviderResponse: "applied"}, nil
}

// mutatorFileOnlyProvider is the v0 gitlab / jira shape: File and nothing else.
type mutatorFileOnlyProvider struct{ name string }

func (p *mutatorFileOnlyProvider) Name() string { return p.name }
func (p *mutatorFileOnlyProvider) File(context.Context, ProviderRequest) (*CreatedItem, error) {
	return &CreatedItem{Provider: p.name}, nil
}

// TestMutatorFor_ResolvesImplementingProvider pins the happy path of the
// resolution chokepoint: a provider implementing the capability is returned
// through it and receives the caller's request verbatim.
func TestMutatorFor_ResolvesImplementingProvider(t *testing.T) {
	p := &mutatorCapableProvider{name: "grooming-capable"}
	Register(p)
	m, err := MutatorFor("grooming-capable")
	if err != nil {
		t.Fatalf("MutatorFor: %v", err)
	}
	res, err := m.ApplyGroomingMutation(context.Background(), GroomingMutationRequest{
		EntryID: "hygiene:github/o~r%231:unboarded", Kind: GroomingKindBoardPlace, ItemRef: "#1",
	})
	if err != nil {
		t.Fatalf("ApplyGroomingMutation: %v", err)
	}
	if !res.Applied {
		t.Errorf("result = %+v, want Applied", res)
	}
	if p.got.Kind != GroomingKindBoardPlace || p.got.ItemRef != "#1" {
		t.Errorf("provider got %+v, want the caller's kind and item ref forwarded", p.got)
	}
}

// TestMutatorFor_FileOnlyProviderYieldsTypedUnavailable is the capability
// fail-closed: a File-only provider resolves to a TYPED
// *UnavailableError{Capability: GroomingCapability, Reason: ReasonNotImplemented}
// with a NIL mutator — never a nil interface a caller could dispatch against.
func TestMutatorFor_FileOnlyProviderYieldsTypedUnavailable(t *testing.T) {
	Register(&mutatorFileOnlyProvider{name: "grooming-file-only"})
	m, err := MutatorFor("grooming-file-only")
	if m != nil {
		t.Errorf("mutator = %v, want nil alongside the unavailable error", m)
	}
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *UnavailableError", err, err)
	}
	if ue.Reason != ReasonNotImplemented {
		t.Errorf("Reason = %q, want %q", ue.Reason, ReasonNotImplemented)
	}
	if ue.Provider != "grooming-file-only" || ue.Capability != GroomingCapability {
		t.Errorf("error names provider %q capability %q, want the resolved provider and the grooming capability", ue.Provider, ue.Capability)
	}
	if !strings.Contains(ue.Error(), string(ReasonNotImplemented)) {
		t.Errorf("message %q should name the reason", ue.Error())
	}
}

// TestMutatorFor_UnknownProvider proves MutatorFor does not swallow the
// registry's own fail-closed error: an unregistered id still yields
// *UnknownProviderError, distinct from capability-unavailable.
func TestMutatorFor_UnknownProvider(t *testing.T) {
	m, err := MutatorFor("grooming-never-registered")
	if m != nil {
		t.Errorf("mutator = %v, want nil", m)
	}
	var upe *UnknownProviderError
	if !errors.As(err, &upe) {
		t.Fatalf("err = %v (%T), want *UnknownProviderError", err, err)
	}
	var ue *UnavailableError
	if errors.As(err, &ue) {
		t.Errorf("an unregistered id must not masquerade as capability-unavailable: %v", err)
	}
}

// TestGroomingMutationKind_Predicates pins the three kind predicates as
// TABLES over the whole closed vocabulary, so a kind added later without a
// deliberate classification fails here.
//
// Icebox is asserted destructive AND board-placement (approval condition I2):
// it is the kind whose omission from the placement guard would let a grooming
// apply overwrite a human's manual placement on a destructive move.
func TestGroomingMutationKind_Predicates(t *testing.T) {
	type want struct{ destructive, placement, observable bool }
	table := map[GroomingMutationKind]want{
		GroomingKindLabelSet:        {false, false, true},
		GroomingKindFieldSet:        {false, false, false},
		GroomingKindBoardPlace:      {false, true, true},
		GroomingKindEpicLink:        {false, false, true},
		GroomingKindPrioritySet:     {false, false, false},
		GroomingKindRankSet:         {false, false, false},
		GroomingKindDependsOnAdd:    {false, false, true},
		GroomingKindCloseDuplicate:  {true, false, true},
		GroomingKindCloseNotPlanned: {true, false, true},
		GroomingKindIcebox:          {true, true, true},
	}
	for kind, w := range table {
		if got := kind.Destructive(); got != w.destructive {
			t.Errorf("%s.Destructive() = %v, want %v", kind, got, w.destructive)
		}
		if got := kind.BoardPlacement(); got != w.placement {
			t.Errorf("%s.BoardPlacement() = %v, want %v", kind, got, w.placement)
		}
		if got := kind.IdempotenceObservable(); got != w.observable {
			t.Errorf("%s.IdempotenceObservable() = %v, want %v", kind, got, w.observable)
		}
	}
	if len(table) != len(allGroomingKinds) {
		t.Errorf("predicate table covers %d kinds but the vocabulary has %d — a new kind needs a deliberate classification",
			len(table), len(allGroomingKinds))
	}
}

// allGroomingKinds is the vocabulary the predicate table must cover in full.
var allGroomingKinds = []GroomingMutationKind{
	GroomingKindLabelSet, GroomingKindFieldSet, GroomingKindBoardPlace, GroomingKindEpicLink,
	GroomingKindPrioritySet, GroomingKindRankSet, GroomingKindDependsOnAdd,
	GroomingKindCloseDuplicate, GroomingKindCloseNotPlanned, GroomingKindIcebox,
}

// TestResolveGroomingMode_FailsClosed pins the fail-closed default: an absent
// class (empty string) and an UNRECOGNIZED mode string both resolve to gated,
// never auto. This is the mirror of spec.resolveBlock's always-emit-a-gated-
// default rule, and the reason a fourth spec mode this package does not know
// about under-approximates in the SAFE direction.
func TestResolveGroomingMode_FailsClosed(t *testing.T) {
	cases := map[GroomingMode]GroomingMode{
		GroomingModeAuto:   GroomingModeAuto,
		GroomingModeGated:  GroomingModeGated,
		GroomingModeReport: GroomingModeReport,
		"":                 GroomingModeGated,
		"delegated":        GroomingModeGated,
		"AUTO":             GroomingModeGated,
	}
	for in, want := range cases {
		if got := ResolveGroomingMode(in); got != want {
			t.Errorf("ResolveGroomingMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGroomingValue_EqualAndContains pins the two comparison helpers the
// idempotence diff is built on. Equal is order-INSENSITIVE over List (a label
// set is a set; an order-sensitive compare would re-dispatch an applied label
// mutation on every re-apply), and Contains is the additive-set test.
func TestGroomingValue_EqualAndContains(t *testing.T) {
	a := GroomingValue{List: []string{"area:api", "type:bug"}}
	b := GroomingValue{List: []string{"type:bug", "area:api"}}
	if !a.Equal(b) {
		t.Errorf("%v.Equal(%v) = false, want true (a label set is unordered)", a, b)
	}
	if a.Equal(GroomingValue{List: []string{"area:api"}}) {
		t.Error("a two-element set must not equal a one-element set")
	}
	if !(GroomingValue{Scalar: "Backlog"}).Equal(GroomingValue{Scalar: "Backlog"}) {
		t.Error("equal scalars must compare equal")
	}
	if (GroomingValue{Scalar: "Backlog"}).Equal(GroomingValue{Scalar: "Icebox"}) {
		t.Error("different scalars must not compare equal")
	}
	if !a.Contains([]string{"type:bug"}) {
		t.Error("Contains must be satisfied by a present element")
	}
	if a.Contains([]string{"type:bug", "phase:p1"}) {
		t.Error("Contains must not be satisfied when an element is absent")
	}
}
