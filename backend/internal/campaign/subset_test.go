package campaign_test

import (
	"errors"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// fullDAG is the fixture the subset tests filter: three children where 101 and
// 102 both depend on 100 (100 is wave 0; 101, 102 are wave 1).
func fullDAG() *workmgmt.EpicChildrenResult {
	return &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{
			{Number: 100, Title: "first"},
			{Number: 101, Title: "second"},
			{Number: 102, Title: "third"},
		},
		Edges: []workmgmt.DependsEdge{
			{From: 101, To: 100},
			{From: 102, To: 100},
		},
	}
}

// TestFilterToSubset_ValidSubset_FiltersChildrenAndKeepsIntraEdges asserts a
// subset that includes both endpoints of an edge keeps that edge and drops the
// excluded child, and that Assemble then builds a DAG over just the subset.
func TestFilterToSubset_ValidSubset_FiltersChildrenAndKeepsIntraEdges(t *testing.T) {
	res, err := campaign.FilterToSubset(fullDAG(), []string{"issue:100", "issue:101"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	if len(res.Children) != 2 || res.Children[0].Number != 100 || res.Children[1].Number != 101 {
		t.Fatalf("Children = %+v, want [100 101] ascending", res.Children)
	}
	if len(res.Edges) != 1 || res.Edges[0] != (workmgmt.DependsEdge{From: 101, To: 100}) {
		t.Fatalf("Edges = %+v, want [{101 100}]", res.Edges)
	}
	if len(res.DroppedEdges) != 0 {
		t.Fatalf("DroppedEdges = %+v, want none (both endpoints included)", res.DroppedEdges)
	}
	// The filtered result assembles into a two-item DAG.
	a, err := campaign.Assemble("issue:99", res)
	if err != nil {
		t.Fatalf("Assemble(subset): %v", err)
	}
	if len(a.Items) != 2 {
		t.Fatalf("assembled items = %d, want 2", len(a.Items))
	}
}

// TestFilterToSubset_NonChildItem_ReturnsErrItemNotChild is the fail-closed
// branch: a requested ref that is not among the epic's children is rejected.
func TestFilterToSubset_NonChildItem_ReturnsErrItemNotChild(t *testing.T) {
	_, err := campaign.FilterToSubset(fullDAG(), []string{"issue:100", "issue:999"})
	if !errors.Is(err, campaign.ErrItemNotChild) {
		t.Fatalf("err = %v, want ErrItemNotChild", err)
	}
}

// TestFilterToSubset_UnparseableItem_ReturnsErrItemNotChild covers the ref-parse
// failure branch: a ref that is neither a number nor issue:N is an unresolvable
// subset item and maps onto the same fail-closed error.
func TestFilterToSubset_UnparseableItem_ReturnsErrItemNotChild(t *testing.T) {
	_, err := campaign.FilterToSubset(fullDAG(), []string{"not-a-ref"})
	if !errors.Is(err, campaign.ErrItemNotChild) {
		t.Fatalf("err = %v, want ErrItemNotChild", err)
	}
}

// TestFilterToSubset_IncludedDependsOnExcludedIncomplete_LandsInDroppedEdges is
// the re-classification branch: an included item whose depends_on targets an
// excluded item that is NOT complete becomes a dropped edge stamped
// DropExcludedIncomplete, so Assemble fails it closed as a dangling dependency —
// the same guarantee a cross-epic dangling edge gives (#2120).
func TestFilterToSubset_IncludedDependsOnExcludedIncomplete_LandsInDroppedEdges(t *testing.T) {
	// Include 101 (depends on 100) but exclude 100. 100 carries no completion
	// flag (Complete == false), so its dependency is unsatisfied.
	res, err := campaign.FilterToSubset(fullDAG(), []string{"issue:101"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	want := workmgmt.DependsEdge{From: 101, To: 100, Reason: workmgmt.DropExcludedIncomplete}
	if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != want {
		t.Fatalf("DroppedEdges = %+v, want [%+v]", res.DroppedEdges, want)
	}
	if _, err := campaign.Assemble("issue:99", res); !errors.Is(err, campaign.ErrDanglingDependency) {
		t.Fatalf("Assemble(dropped edge) err = %v, want ErrDanglingDependency", err)
	}
}

// TestFilterToSubset_IncludedDependsOnExcludedComplete_DroppedSilently is the
// satisfied-dependency branch (#2120): an included item whose depends_on targets
// an excluded item that IS closed-and-completed is a satisfied dependency, so
// the edge is dropped silently (no DroppedEdges) and Assemble succeeds over the
// included item — the same result the full all-children sweep produces via
// closed-child auto-settle.
func TestFilterToSubset_IncludedDependsOnExcludedComplete_DroppedSilently(t *testing.T) {
	in := fullDAG()
	// Mark the excluded dependency target #100 complete.
	in.Children[0].Complete = true
	res, err := campaign.FilterToSubset(in, []string{"issue:101"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	if len(res.DroppedEdges) != 0 {
		t.Fatalf("DroppedEdges = %+v, want none (excluded #100 is complete → satisfied)", res.DroppedEdges)
	}
	// #2953: the elision is no longer SILENT — it is recorded as a SatisfiedEdge
	// carrying the closed/completed state, so an operator can see it.
	want := workmgmt.SatisfiedEdge{From: 101, To: 100, State: "closed", StateReason: "completed"}
	if len(res.SatisfiedEdges) != 1 || res.SatisfiedEdges[0] != want {
		t.Fatalf("SatisfiedEdges = %+v, want [%+v] (excluded-complete elision recorded)", res.SatisfiedEdges, want)
	}
	a, err := campaign.Assemble("issue:99", res)
	if err != nil {
		t.Fatalf("Assemble(satisfied dep) = %v, want success", err)
	}
	if len(a.Items) != 1 || a.Items[0].IssueRef != "issue:101" {
		t.Fatalf("assembled items = %+v, want [issue:101]", a.Items)
	}
}

// TestFilterToSubset_CarriesInboundSatisfiedEdgesAtMostOnce pins #2953 condition
// 3: an inbound SatisfiedEdge (an out-of-EPIC target the provider already elided)
// is carried through untouched, and FilterToSubset does NOT re-add an edge already
// present in res.SatisfiedEdges — every satisfied edge appears AT MOST ONCE.
func TestFilterToSubset_CarriesInboundSatisfiedEdgesAtMostOnce(t *testing.T) {
	in := fullDAG()
	// #100 is complete: the included #101->#100 excluded edge will be elided.
	in.Children[0].Complete = true
	// Seed an inbound satisfied edge that COLLIDES with the elision the subset
	// filter is about to record, to prove the at-most-once dedup.
	in.SatisfiedEdges = []workmgmt.SatisfiedEdge{
		{From: 101, To: 100, State: "closed", StateReason: "completed"},
		{From: 102, To: 1639, State: "closed", StateReason: "completed"}, // out-of-epic, From excluded
	}
	res, err := campaign.FilterToSubset(in, []string{"issue:101"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	// Count occurrences of each (From,To): none may appear more than once.
	seen := map[[2]int]int{}
	for _, s := range res.SatisfiedEdges {
		seen[[2]int{s.From, s.To}]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("satisfied edge %v appears %d times, want at most once", k, n)
		}
	}
	// The colliding inbound 101->100 must be present exactly once (not doubled by
	// the excluded-complete elision).
	if seen[[2]int{101, 100}] != 1 {
		t.Errorf("101->100 count = %d, want exactly 1 (inbound + elision deduped)", seen[[2]int{101, 100}])
	}
	// The inbound out-of-epic 102->1639 is carried through untouched.
	if seen[[2]int{102, 1639}] != 1 {
		t.Errorf("inbound 102->1639 missing from carried SatisfiedEdges: %+v", res.SatisfiedEdges)
	}
}

// TestFilterToSubset_DedupsInboundDuplicateSatisfiedEdges pins #2953 condition 3
// at the CONSUMER boundary: even if a producer emits the SAME satisfied edge
// twice in res.SatisfiedEdges, FilterToSubset carries it through exactly ONCE.
// The provider now collapses duplicate depends_on tokens at the source, but the
// consumer dedup makes the at-most-once invariant hold for ANY inbound producer.
//
// COUNTERFACTUAL: revert the inbound carry to `append(satisfied, res.SatisfiedEdges...)`
// (no per-(From,To) dedup) → the duplicate survives → this goes RED.
func TestFilterToSubset_DedupsInboundDuplicateSatisfiedEdges(t *testing.T) {
	in := fullDAG()
	// A producer-emitted duplicate of the SAME out-of-epic satisfied edge (From
	// #101 is included in the subset below), plus a distinct one.
	in.SatisfiedEdges = []workmgmt.SatisfiedEdge{
		{From: 101, To: 1639, State: "closed", StateReason: "completed"},
		{From: 101, To: 1639, State: "closed", StateReason: "completed"},
	}
	res, err := campaign.FilterToSubset(in, []string{"issue:101"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	seen := map[[2]int]int{}
	for _, s := range res.SatisfiedEdges {
		seen[[2]int{s.From, s.To}]++
	}
	if seen[[2]int{101, 1639}] != 1 || len(res.SatisfiedEdges) != 1 {
		t.Fatalf("SatisfiedEdges = %+v, want exactly one 101->1639 (inbound duplicate collapsed)", res.SatisfiedEdges)
	}
}

// TestFilterToSubset_ExcludedTargetMissingFromChildren_FailsClosed covers the
// defensive fallback (#2120): if an included item's depends_on edge points at a
// number that is not in childByNumber at all (an unexpected state the provider
// should prevent), the completion lookup misses and the edge fails closed as
// DropExcludedIncomplete rather than panicking or being dropped silently.
func TestFilterToSubset_ExcludedTargetMissingFromChildren_FailsClosed(t *testing.T) {
	in := &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 200, Title: "only child"}},
		// 200 depends on 201, which is NOT in Children at all.
		Edges: []workmgmt.DependsEdge{{From: 200, To: 201}},
	}
	res, err := campaign.FilterToSubset(in, []string{"issue:200"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	want := workmgmt.DependsEdge{From: 200, To: 201, Reason: workmgmt.DropExcludedIncomplete}
	if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != want {
		t.Fatalf("DroppedEdges = %+v, want [%+v] (missing target fails closed)", res.DroppedEdges, want)
	}
	if _, err := campaign.Assemble("issue:99", res); !errors.Is(err, campaign.ErrDanglingDependency) {
		t.Fatalf("Assemble err = %v, want ErrDanglingDependency", err)
	}
}

// TestFilterToSubset_EdgeWhollyExcluded_DroppedSilently asserts an edge whose
// depending item (From) is not in the subset is dropped without becoming a
// dangling dependency — the excluded item is simply not in the campaign.
func TestFilterToSubset_EdgeWhollyExcluded_DroppedSilently(t *testing.T) {
	// Include only 100; both edges (101->100, 102->100) have an excluded From.
	res, err := campaign.FilterToSubset(fullDAG(), []string{"issue:100"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	if len(res.Edges) != 0 {
		t.Fatalf("Edges = %+v, want none", res.Edges)
	}
	if len(res.DroppedEdges) != 0 {
		t.Fatalf("DroppedEdges = %+v, want none (excluded From dropped silently)", res.DroppedEdges)
	}
	a, err := campaign.Assemble("issue:99", res)
	if err != nil {
		t.Fatalf("Assemble(single): %v", err)
	}
	if len(a.Items) != 1 || a.Items[0].IssueRef != "issue:100" {
		t.Fatalf("assembled items = %+v, want [issue:100]", a.Items)
	}
}

// TestFilterToSubset_ExcludedItemCrossEpicEdge_DroppedSilently asserts that a
// provider-surfaced cross-epic DroppedEdges entry whose From is EXCLUDED from
// the subset is dropped silently — so an excluded child's real cross-epic
// depends_on no longer makes the whole epic un-campaignable for a subset that
// leaves that child out (#2087). FilterToSubset returns zero DroppedEdges and
// Assemble succeeds over the included items.
func TestFilterToSubset_ExcludedItemCrossEpicEdge_DroppedSilently(t *testing.T) {
	in := fullDAG()
	// 102 carries a real cross-epic depends_on (e.g. E45.22 -> ADR-057 #1823)
	// the provider already surfaced as a dropped edge.
	in.DroppedEdges = []workmgmt.DependsEdge{{From: 102, To: 1823}}
	// Scope to {100, 101}: 102 (and its cross-epic dropped edge) is excluded.
	res, err := campaign.FilterToSubset(in, []string{"issue:100", "issue:101"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	if len(res.DroppedEdges) != 0 {
		t.Fatalf("DroppedEdges = %+v, want none (excluded item's cross-epic edge dropped silently)", res.DroppedEdges)
	}
	if _, err := campaign.Assemble("issue:99", res); err != nil {
		t.Fatalf("Assemble(subset excluding cross-epic child): %v, want success", err)
	}
}

// TestFilterToSubset_IncludedItemCrossEpicEdge_StillDangling is the fail-closed
// counterpart: the SAME provider cross-epic dropped edge whose From is INCLUDED
// in the subset still carries through, so Assemble fails it closed as a dangling
// dependency — the guarantee is preserved for edges that matter (#2087).
func TestFilterToSubset_IncludedItemCrossEpicEdge_StillDangling(t *testing.T) {
	in := fullDAG()
	in.DroppedEdges = []workmgmt.DependsEdge{{From: 102, To: 1823}}
	// Scope to {100, 102}: 102 (and its cross-epic dropped edge) is included.
	res, err := campaign.FilterToSubset(in, []string{"issue:100", "issue:102"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != (workmgmt.DependsEdge{From: 102, To: 1823}) {
		t.Fatalf("DroppedEdges = %+v, want [{102 1823}] (included item's cross-epic edge carried through)", res.DroppedEdges)
	}
	if _, err := campaign.Assemble("issue:99", res); !errors.Is(err, campaign.ErrDanglingDependency) {
		t.Fatalf("Assemble(included cross-epic edge) err = %v, want ErrDanglingDependency", err)
	}
}

// TestFilterToSubset_EmptyItems_ReturnsUnchanged is the backward-compatible
// no-op: an empty/nil items list returns the exact same result pointer, so the
// all-children sweep is preserved.
func TestFilterToSubset_EmptyItems_ReturnsUnchanged(t *testing.T) {
	in := fullDAG()
	out, err := campaign.FilterToSubset(in, nil)
	if err != nil {
		t.Fatalf("FilterToSubset(nil): %v", err)
	}
	if out != in {
		t.Fatalf("FilterToSubset(nil) returned a different result; want the input unchanged")
	}
	empty, err := campaign.FilterToSubset(in, []string{})
	if err != nil {
		t.Fatalf("FilterToSubset(empty): %v", err)
	}
	if empty != in {
		t.Fatalf("FilterToSubset(empty) returned a different result; want the input unchanged")
	}
}

// TestFilterToSubset_BareAndIssueRefForms_BothResolve proves a subset can name
// items in the bare-number and issue:N forms interchangeably.
func TestFilterToSubset_BareAndIssueRefForms_BothResolve(t *testing.T) {
	res, err := campaign.FilterToSubset(fullDAG(), []string{"100", "issue:101"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	if len(res.Children) != 2 {
		t.Fatalf("Children = %+v, want 2 (both ref forms resolved)", res.Children)
	}
}
