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

// TestFilterToSubset_IncludedDependsOnExcluded_LandsInDroppedEdges is the
// re-classification branch: an included item whose depends_on targets an
// excluded item becomes a dropped edge, so Assemble fails it closed as a
// dangling dependency — the same guarantee a cross-epic dangling edge gives.
func TestFilterToSubset_IncludedDependsOnExcluded_LandsInDroppedEdges(t *testing.T) {
	// Include 101 (depends on 100) but exclude 100.
	res, err := campaign.FilterToSubset(fullDAG(), []string{"issue:101"})
	if err != nil {
		t.Fatalf("FilterToSubset: %v", err)
	}
	if len(res.DroppedEdges) != 1 || res.DroppedEdges[0] != (workmgmt.DependsEdge{From: 101, To: 100}) {
		t.Fatalf("DroppedEdges = %+v, want [{101 100}]", res.DroppedEdges)
	}
	if _, err := campaign.Assemble("issue:99", res); !errors.Is(err, campaign.ErrDanglingDependency) {
		t.Fatalf("Assemble(dropped edge) err = %v, want ErrDanglingDependency", err)
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
