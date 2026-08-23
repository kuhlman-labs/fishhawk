package campaign_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ghRef builds a github_issue item ref in the normative
// `<owner>/<repo>#<number>` id form.
func ghRef(id string) plan.ItemRef {
	return plan.ItemRef{Type: "github_issue", ID: id, URL: "https://example.test/" + id}
}

func orderingEntry(rank int, ref plan.ItemRef) plan.OrderingEntry {
	return plan.OrderingEntry{
		ID:              plan.GroomingEntryID(plan.GroomingClassOrdering, "", ref),
		ItemRef:         ref,
		Rank:            rank,
		Score:           float64(100 - rank),
		RubricCitations: []plan.RubricCitation{{RubricID: "V1"}},
	}
}

func report(entries ...plan.OrderingEntry) *plan.GroomingReport {
	return &plan.GroomingReport{Ordering: entries}
}

// TestOrderFromReport_RankOrderIsHonored is the DONE-MEANS behavioural
// assertion of the pure layer: the report's ranks are the REVERSE of the
// issues' numeric order, so an implementation that merely collected the issue
// numbers (leaving them ascending) FAILS. Nothing about the type system
// enforces this — it is a value-level property.
func TestOrderFromReport_RankOrderIsHonored(t *testing.T) {
	// Deliberately hand the entries in ascending-number order so the sort, not
	// the input order, is what produces the result.
	got, err := campaign.OrderFromReport(report(
		orderingEntry(3, ghRef("acme/widgets#10")),
		orderingEntry(2, ghRef("acme/widgets#20")),
		orderingEntry(1, ghRef("acme/widgets#30")),
	), "acme", "widgets", 0)
	if err != nil {
		t.Fatalf("OrderFromReport: %v", err)
	}
	want := []int{30, 20, 10}
	if !reflect.DeepEqual(got.Numbers, want) {
		t.Fatalf("Numbers = %v, want %v (rank order, NOT issue-number order)", got.Numbers, want)
	}
	wantRefs := []string{"issue:30", "issue:20", "issue:10"}
	if !reflect.DeepEqual(got.Refs, wantRefs) {
		t.Fatalf("Refs = %v, want %v", got.Refs, wantRefs)
	}
	if len(got.Excluded) != 0 {
		t.Fatalf("Excluded = %v, want none", got.Excluded)
	}
	if got.OmittedByLimit != 0 {
		t.Fatalf("OmittedByLimit = %d, want 0", got.OmittedByLimit)
	}
}

// TestOrderFromReport_ExclusionsAreReportedNotDropped covers each NAMED
// exclusion reason, one case per reason, and asserts the entry is REPORTED —
// a silently-truncated batch is the failure this surface exists to prevent.
func TestOrderFromReport_ExclusionsAreReportedNotDropped(t *testing.T) {
	tests := []struct {
		name       string
		ref        plan.ItemRef
		wantReason string
	}{
		{"other forge", plan.ItemRef{Type: "gitlab_issue", ID: "acme/widgets#11"}, campaign.ExclusionNotGitHubIssue},
		{"other repo", ghRef("acme/gadgets#11"), campaign.ExclusionOtherRepo},
		{"other owner", ghRef("evil/widgets#11"), campaign.ExclusionOtherRepo},
		{"no hash", ghRef("acme/widgets"), campaign.ExclusionUnparseableID},
		{"no slash", ghRef("widgets#11"), campaign.ExclusionUnparseableID},
		{"non-numeric", ghRef("acme/widgets#abc"), campaign.ExclusionUnparseableID},
		{"empty number", ghRef("acme/widgets#"), campaign.ExclusionUnparseableID},
		{"zero number", ghRef("acme/widgets#0"), campaign.ExclusionUnparseableID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Pair the excluded entry with ONE convertible entry so the result is
			// non-empty and the assertion lands on the exclusion, not on the
			// empty-set refusal.
			got, err := campaign.OrderFromReport(report(
				orderingEntry(1, tc.ref),
				orderingEntry(2, ghRef("acme/widgets#99")),
			), "acme", "widgets", 0)
			if err != nil {
				t.Fatalf("OrderFromReport: %v", err)
			}
			if !reflect.DeepEqual(got.Numbers, []int{99}) {
				t.Fatalf("Numbers = %v, want [99] (the excluded ref must not be converted)", got.Numbers)
			}
			if len(got.Excluded) != 1 {
				t.Fatalf("Excluded = %v, want exactly one reported exclusion", got.Excluded)
			}
			if got.Excluded[0].Reason != tc.wantReason {
				t.Fatalf("Excluded[0].Reason = %q, want %q", got.Excluded[0].Reason, tc.wantReason)
			}
			if got.Excluded[0].Ref != tc.ref.ID || got.Excluded[0].Rank != 1 {
				t.Fatalf("Excluded[0] = %+v, want the offending ref+rank named", got.Excluded[0])
			}
		})
	}
}

// TestOrderFromReport_OwnerRepoMatchIsCaseInsensitive: GitHub owner/repo names
// are case-insensitive, so a report that spells the repo differently must not
// silently exclude every entry (which would surface as a confusing "names no
// issue" refusal).
func TestOrderFromReport_OwnerRepoMatchIsCaseInsensitive(t *testing.T) {
	got, err := campaign.OrderFromReport(report(
		orderingEntry(1, ghRef("ACME/Widgets#7")),
	), "acme", "widgets", 0)
	if err != nil {
		t.Fatalf("OrderFromReport: %v", err)
	}
	if !reflect.DeepEqual(got.Numbers, []int{7}) {
		t.Fatalf("Numbers = %v, want [7]; excluded = %+v", got.Numbers, got.Excluded)
	}
}

// TestOrderFromReport_LimitTakesTopNAndReportsOmitted pins K4: the
// omitted-by-limit count must be DERIVABLE from the result, and it must be
// distinct from Excluded — a capped entry WAS convertible.
func TestOrderFromReport_LimitTakesTopNAndReportsOmitted(t *testing.T) {
	got, err := campaign.OrderFromReport(report(
		orderingEntry(4, ghRef("acme/widgets#1")),
		orderingEntry(1, ghRef("acme/widgets#2")),
		orderingEntry(3, ghRef("acme/widgets#3")),
		orderingEntry(2, ghRef("acme/widgets#4")),
		// A non-convertible entry ranked INSIDE the cap: it must not consume a
		// slot, and it must be reported as an exclusion rather than counted as
		// omitted-by-limit.
		orderingEntry(5, plan.ItemRef{Type: "jira_issue", ID: "PROJ-1"}),
	), "acme", "widgets", 2)
	if err != nil {
		t.Fatalf("OrderFromReport: %v", err)
	}
	if want := []int{2, 4}; !reflect.DeepEqual(got.Numbers, want) {
		t.Fatalf("Numbers = %v, want %v (top 2 by rank)", got.Numbers, want)
	}
	if got.OmittedByLimit != 2 {
		t.Fatalf("OmittedByLimit = %d, want 2 (issues 3 and 1 were convertible but capped)", got.OmittedByLimit)
	}
	if len(got.Excluded) != 1 || got.Excluded[0].Reason != campaign.ExclusionNotGitHubIssue {
		t.Fatalf("Excluded = %+v, want exactly the jira entry, NOT the capped ones", got.Excluded)
	}
	if got.Limit != 2 {
		t.Fatalf("Limit = %d, want 2", got.Limit)
	}
}

// TestOrderFromReport_EmptyConvertibleSetFailsClosed: an item-less campaign is
// never persisted; the caller maps this to a 422.
func TestOrderFromReport_EmptyConvertibleSetFailsClosed(t *testing.T) {
	_, err := campaign.OrderFromReport(report(
		orderingEntry(1, ghRef("other/repo#1")),
		orderingEntry(2, plan.ItemRef{Type: "gitlab_issue", ID: "acme/widgets#2"}),
	), "acme", "widgets", 0)
	var goe *campaign.GroomingOrderError
	if !errors.As(err, &goe) || goe.Code != campaign.GroomingOrderErrEmpty {
		t.Fatalf("err = %v, want a *GroomingOrderError with code %q", err, campaign.GroomingOrderErrEmpty)
	}
	if !errors.Is(err, campaign.ErrGroomingOrder) {
		t.Fatalf("errors.Is(err, ErrGroomingOrder) = false, want true")
	}
}

// TestOrderFromReport_DuplicateIssueNumberFailsClosed is a COUNTERFACTUAL
// vehicle for the duplicate guard: two ordering entries naming the same issue
// would otherwise seed two campaign items for one issue.
func TestOrderFromReport_DuplicateIssueNumberFailsClosed(t *testing.T) {
	_, err := campaign.OrderFromReport(report(
		orderingEntry(1, ghRef("acme/widgets#5")),
		// Same number, spelled with different case — so the guard must compare
		// the RESOLVED number, not the raw id string.
		orderingEntry(2, ghRef("ACME/WIDGETS#5")),
	), "acme", "widgets", 0)
	var goe *campaign.GroomingOrderError
	if !errors.As(err, &goe) || goe.Code != campaign.GroomingOrderErrDuplicate {
		t.Fatalf("err = %v, want a *GroomingOrderError with code %q", err, campaign.GroomingOrderErrDuplicate)
	}
}

func childrenResult(numbers ...int) *workmgmt.EpicChildrenResult {
	res := &workmgmt.EpicChildrenResult{}
	for _, n := range numbers {
		res.Children = append(res.Children, workmgmt.EpicChild{Number: n, Title: "issue " + issueNum(n)})
	}
	return res
}

func issueNum(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestReorderByPriority_PermutesChildrenAndPreservesEdges asserts the two
// halves of the contract in one place: the Children slice IS permuted, and the
// edge sets are carried through byte-identically (the DAG is invariant).
func TestReorderByPriority_PermutesChildrenAndPreservesEdges(t *testing.T) {
	res := childrenResult(10, 20, 30)
	res.Edges = []workmgmt.DependsEdge{{From: 10, To: 30}}
	res.DroppedEdges = []workmgmt.DependsEdge{{From: 99, To: 1, Reason: workmgmt.DropNotChild}}

	got, err := campaign.ReorderByPriority(res, []int{30, 10, 20})
	if err != nil {
		t.Fatalf("ReorderByPriority: %v", err)
	}
	gotNumbers := []int{}
	for _, c := range got.Children {
		gotNumbers = append(gotNumbers, c.Number)
	}
	if want := []int{30, 10, 20}; !reflect.DeepEqual(gotNumbers, want) {
		t.Fatalf("Children numbers = %v, want %v", gotNumbers, want)
	}
	if !reflect.DeepEqual(got.Edges, res.Edges) {
		t.Fatalf("Edges = %v, want them carried through untouched (%v)", got.Edges, res.Edges)
	}
	if !reflect.DeepEqual(got.DroppedEdges, res.DroppedEdges) {
		t.Fatalf("DroppedEdges = %v, want them carried through untouched", got.DroppedEdges)
	}
	// The input must not be mutated: the caller still holds it.
	inputNumbers := []int{}
	for _, c := range res.Children {
		inputNumbers = append(inputNumbers, c.Number)
	}
	if want := []int{10, 20, 30}; !reflect.DeepEqual(inputNumbers, want) {
		t.Fatalf("input Children mutated to %v, want %v", inputNumbers, want)
	}
}

// TestReorderByPriority_PreservesWaves pins the risk the plan names: permuting
// Children must change ONLY the item creation order, never the wave assignment.
// Assemble is driven twice over the same edge set — once unpermuted, once
// permuted — and each issue's wave must be identical.
func TestReorderByPriority_PreservesWaves(t *testing.T) {
	res := childrenResult(10, 20, 30)
	// 10 depends on 30; 20 depends on 10. Waves: {30}, {10}, {20}.
	res.Edges = []workmgmt.DependsEdge{{From: 10, To: 30}, {From: 20, To: 10}}

	before, err := campaign.Assemble("", res)
	if err != nil {
		t.Fatalf("Assemble(unpermuted): %v", err)
	}
	permuted, err := campaign.ReorderByPriority(res, []int{30, 20, 10})
	if err != nil {
		t.Fatalf("ReorderByPriority: %v", err)
	}
	after, err := campaign.Assemble("", permuted)
	if err != nil {
		t.Fatalf("Assemble(permuted): %v", err)
	}

	waveOf := func(a *campaign.Assembly) map[string]int {
		out := map[string]int{}
		for _, it := range a.Items {
			out[it.IssueRef] = it.Wave
		}
		return out
	}
	if !reflect.DeepEqual(waveOf(before), waveOf(after)) {
		t.Fatalf("wave assignment changed under the permutation: %v -> %v", waveOf(before), waveOf(after))
	}
	// ...while the ORDER (and therefore the persisted queue position) did change.
	var gotRefs []string
	for _, it := range after.Items {
		gotRefs = append(gotRefs, it.IssueRef)
		if it.Position != len(gotRefs)-1 {
			t.Fatalf("Items[%d].Position = %d, want %d (dense 0-based queue position)", len(gotRefs)-1, it.Position, len(gotRefs)-1)
		}
	}
	if want := []string{"issue:30", "issue:20", "issue:10"}; !reflect.DeepEqual(gotRefs, want) {
		t.Fatalf("assembled item order = %v, want %v", gotRefs, want)
	}
}

// TestReorderByPriority_MismatchedSetFailsClosed is the COUNTERFACTUAL vehicle
// for the set-equality check. Both mismatch DIRECTIONS get their own case: a
// dropped item and an invented one fail in different ways and a guard that
// only checked one would pass the other.
func TestReorderByPriority_MismatchedSetFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		children []int
		order    []int
	}{
		{"ordered number the provider did not resolve", []int{10, 20}, []int{10, 20, 30}},
		{"resolved child the order does not name", []int{10, 20, 30}, []int{10, 20}},
		{"order names the same issue twice", []int{10, 20}, []int{10, 10, 20}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := campaign.ReorderByPriority(childrenResult(tc.children...), tc.order)
			var goe *campaign.GroomingOrderError
			if !errors.As(err, &goe) || goe.Code != campaign.GroomingOrderErrSetMismatch {
				t.Fatalf("err = %v, want a *GroomingOrderError with code %q", err, campaign.GroomingOrderErrSetMismatch)
			}
		})
	}
}
