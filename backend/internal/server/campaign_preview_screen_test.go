package server

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// --- path_overlap evaluator units (#5221) ---

// TestPreviewOverlapPathToken pins the prose filter, one row per token class.
// Deleting or loosening any arm of previewOverlapPathToken flips at least one
// row: the extension allowlist (dotted identifiers, versions), the directory
// slash minimum (one-slash prose, two-segment directories), the slash-only
// ambiguous extensions (s.cfg, process.env), and the import-host exclusion.
func TestPreviewOverlapPathToken(t *testing.T) {
	cases := []struct {
		tok  string
		want bool
	}{
		// Slash-words: one slash, no extension.
		{"and/or", false},
		{"title/body", false},
		{"read/write", false},
		{"owner/repo", false},
		// Dotted identifiers: the "extension" is not allowlisted.
		{"campaign.NextEligible", false},
		{"workmgmt.EpicChild.NotRunnable", false},
		// Version and abbreviation tokens.
		{"v1.2", false},
		{"0.5", false},
		{"v1.2.3", false},
		{"e.g.", false},
		{"i.e", false},
		// Ambiguous short extensions without a '/' (D3).
		{"s.cfg", false},
		{"process.env", false},
		{"app.conf", false},
		{"x.h", false},
		// Go import paths with a host-like first segment (D3).
		{"github.com/kuhlman-labs/fishhawk", false},
		{"github.com/kuhlman-labs/fishhawk/backend/internal/x.go", false},
		// Globs, absolute and parent-escaping paths.
		{".github/workflows/*", false},
		{"/tmp/x.json", false},
		{"../x.go", false},
		{".", false},
		// Two-segment directory: a documented false negative.
		{"docs/adr", false},
		// Paths.
		{"backend/internal/x/y.go", true},
		{"docs/guides/x.md", true},
		{".gitlab-ci.yml", true},
		{".github/workflows/ci.yml", true},
		{"client.go", true},
		{"README.MD", true},
		{"backend/internal/campaign", true},
		{"backend/internal/campaign/", true},
		{"./backend/internal/x/y.go", true},
		// Ambiguous extensions WITH a '/' are paths.
		{"config/app.conf", true},
		{"backend/go.mod", true},
		{"include/x.h", true},
	}
	for _, tc := range cases {
		if got := previewOverlapPathToken(tc.tok); got != tc.want {
			t.Errorf("previewOverlapPathToken(%q) = %v, want %v", tc.tok, got, tc.want)
		}
	}
}

func overlapChild(n int, title, body string) workmgmt.EpicChild {
	return workmgmt.EpicChild{Number: n, Title: title, Body: body}
}

func TestEvaluatePreviewPathOverlap_UnorderedPairReported(t *testing.T) {
	got := evaluatePreviewPathOverlap([]workmgmt.EpicChild{
		overlapChild(101, "a", "fix backend/internal/x/y.go:12 first"),
		overlapChild(102, "b", "touch backend/internal/x/y.go too"),
	}, nil)
	want := &campaignPreviewPathOverlap{
		Advisory: true,
		Pairs: []campaignPreviewPathOverlapPair{{
			Items:  []string{"issue:101", "issue:102"},
			Paths:  []string{"backend/internal/x/y.go"},
			Class:  previewOverlapClassCode,
			Remedy: previewOverlapRemedyCode,
		}},
		TotalPairs: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlap = %+v, want %+v", got, want)
	}
}

// Every pair in this fixture is ordered (102 -> 101, 103 -> 102, so 103 -> 101
// transitively), so with the ordering skip removed all three pairs appear.
func TestEvaluatePreviewPathOverlap_OrderedPairOmitted(t *testing.T) {
	body := "edit backend/internal/x/y.go"
	got := evaluatePreviewPathOverlap([]workmgmt.EpicChild{
		overlapChild(101, "a", body),
		overlapChild(102, "b", body),
		overlapChild(103, "c", body),
	}, []workmgmt.DependsEdge{{From: 102, To: 101}, {From: 103, To: 102}})
	if got != nil {
		t.Fatalf("ordered pairs reported: %+v", got)
	}
}

// The lower-numbered item depends on the higher one, so only the a->b reach
// (from 101 to 102) orders the pair.
func TestEvaluatePreviewPathOverlap_ReverseOrderedPairOmitted(t *testing.T) {
	body := "edit backend/internal/x/y.go"
	got := evaluatePreviewPathOverlap([]workmgmt.EpicChild{
		overlapChild(101, "a", body),
		overlapChild(102, "b", body),
	}, []workmgmt.DependsEdge{{From: 101, To: 102}})
	if got != nil {
		t.Fatalf("reverse-ordered pair reported: %+v", got)
	}
}

// An out-of-set edge endpoint never orders a pair.
func TestEvaluatePreviewPathOverlap_OutOfSetEdgeDoesNotOrder(t *testing.T) {
	body := "edit backend/internal/x/y.go"
	got := evaluatePreviewPathOverlap([]workmgmt.EpicChild{
		overlapChild(101, "a", body),
		overlapChild(102, "b", body),
	}, []workmgmt.DependsEdge{{From: 101, To: 999}, {From: 999, To: 102}})
	if got == nil || len(got.Pairs) != 1 {
		t.Fatalf("overlap = %+v, want the one unordered pair", got)
	}
}

func TestEvaluatePreviewPathOverlap_DocsOnlyClass(t *testing.T) {
	got := evaluatePreviewPathOverlap([]workmgmt.EpicChild{
		overlapChild(101, "a", "docs/guides/x.md"),
		overlapChild(102, "b", "docs/guides/x.md"),
		overlapChild(103, "c", "docs/guides/y.md and docs/api/v0.openapi.yaml"),
		overlapChild(104, "d", "docs/guides/y.md and docs/api/v0.openapi.yaml"),
		overlapChild(105, "e", "docs/adr/x.md"),
		overlapChild(106, "f", "docs/adr/x.md"),
	}, nil)
	if got == nil || len(got.Pairs) != 3 {
		t.Fatalf("overlap = %+v, want three pairs", got)
	}
	byItems := map[string]campaignPreviewPathOverlapPair{}
	for _, p := range got.Pairs {
		byItems[p.Items[0]+","+p.Items[1]] = p
	}
	if p := byItems["issue:101,issue:102"]; p.Class != previewOverlapClassDocsOnly || p.Remedy != previewOverlapRemedyDocsOnly {
		t.Errorf("101/102 = %+v, want docs_only with the docs remedy", p)
	}
	if p := byItems["issue:103,issue:104"]; p.Class != previewOverlapClassCode || p.Remedy != previewOverlapRemedyCode ||
		!reflect.DeepEqual(p.Paths, []string{"docs/api/v0.openapi.yaml", "docs/guides/y.md"}) {
		t.Errorf("103/104 = %+v, want code (openapi is code)", p)
	}
	if p := byItems["issue:105,issue:106"]; p.Class != previewOverlapClassCode {
		t.Errorf("105/106 = %+v, want code (docs/adr is review-governing)", p)
	}
}

// Both items share ONLY prose tokens that extractCriterionPathTokens still
// yields (bare slash-words and backticked tokens), so the filter is the only
// thing keeping a pair out. No edges exist, so nothing else can mask it.
func TestEvaluatePreviewPathOverlap_IgnoresNonPathTokens(t *testing.T) {
	body := "and/or owner/repo title/body read/write `campaign.NextEligible` `v1.2` `0.5` `e.g.` " +
		"`pause_item` `.github/workflows/*` `s.cfg` `process.env` github.com/kuhlman-labs/fishhawk"
	got := evaluatePreviewPathOverlap([]workmgmt.EpicChild{
		overlapChild(101, "a", body),
		overlapChild(102, "b", body),
	}, nil)
	if got != nil {
		t.Fatalf("prose tokens reported as overlap: %+v", got)
	}
}

// 101..112 all name a hub path (66 pairs, fanout 12); only 113 and 114 name a
// specific path (fanout 2). Ranking by fanout keeps the specific pair first.
func TestEvaluatePreviewPathOverlap_CapKeepsSpecificPairs(t *testing.T) {
	var children []workmgmt.EpicChild
	for n := 101; n <= 112; n++ {
		children = append(children, overlapChild(n, "hub "+strconv.Itoa(n), "edit backend/internal/hub/hub.go"))
	}
	children = append(children,
		overlapChild(113, "x", "edit backend/internal/x/y.go"),
		overlapChild(114, "y", "edit backend/internal/x/y.go"),
	)
	got := evaluatePreviewPathOverlap(children, nil)
	if got == nil {
		t.Fatal("overlap = nil, want 50 capped pairs")
	}
	if len(got.Pairs) != previewPathOverlapMaxPairs || !got.Truncated || got.TotalPairs != 67 {
		t.Fatalf("pairs=%d truncated=%v total=%d, want 50 true 67", len(got.Pairs), got.Truncated, got.TotalPairs)
	}
	if want := []string{"issue:113", "issue:114"}; !reflect.DeepEqual(got.Pairs[0].Items, want) {
		t.Errorf("pairs[0] = %v, want %v", got.Pairs[0].Items, want)
	}
}

// Within one fanout, code pairs rank before docs_only pairs.
func TestEvaluatePreviewPathOverlap_CodeBeforeDocsOnlyAtEqualFanout(t *testing.T) {
	got := evaluatePreviewPathOverlap([]workmgmt.EpicChild{
		overlapChild(101, "a", "docs/guides/x.md"),
		overlapChild(102, "b", "docs/guides/x.md"),
		overlapChild(103, "c", "backend/internal/x/y.go"),
		overlapChild(104, "d", "backend/internal/x/y.go"),
	}, nil)
	if got == nil || len(got.Pairs) != 2 {
		t.Fatalf("overlap = %+v, want two pairs", got)
	}
	if got.Pairs[0].Class != previewOverlapClassCode || got.Pairs[1].Class != previewOverlapClassDocsOnly {
		t.Errorf("classes = %s, %s, want code then docs_only", got.Pairs[0].Class, got.Pairs[1].Class)
	}
}

func TestEvaluatePreviewPathOverlap_TitleAndBodyUnion(t *testing.T) {
	got := evaluatePreviewPathOverlap([]workmgmt.EpicChild{
		overlapChild(101, "edit backend/internal/x/y.go", "nothing here"),
		overlapChild(102, "plain title", "edit backend/internal/x/y.go"),
	}, nil)
	if got == nil || len(got.Pairs) != 1 || !reflect.DeepEqual(got.Pairs[0].Items, []string{"issue:101", "issue:102"}) {
		t.Fatalf("overlap = %+v, want one title/body pair", got)
	}
}

func TestEvaluatePreviewPathOverlap_NilWhenNoSharedPath(t *testing.T) {
	got := evaluatePreviewPathOverlap([]workmgmt.EpicChild{
		overlapChild(101, "a", "backend/internal/x/a.go"),
		overlapChild(102, "b", "backend/internal/x/b.go"),
	}, nil)
	if got != nil {
		t.Fatalf("overlap = %+v, want nil", got)
	}
	if got := evaluatePreviewPathOverlap(nil, nil); got != nil {
		t.Fatalf("empty input = %+v, want nil", got)
	}
}

// A child repeated in the input never pairs with itself.
func TestEvaluatePreviewPathOverlap_DuplicateChildNoSelfPair(t *testing.T) {
	c := overlapChild(101, "a", "edit backend/internal/x/y.go")
	if got := evaluatePreviewPathOverlap([]workmgmt.EpicChild{c, c}, nil); got != nil {
		t.Fatalf("overlap = %+v, want nil (no self pair)", got)
	}
}

// previewReaches terminates on a cycle and never reports an unreachable node.
func TestPreviewReaches_TerminatesOnCycle(t *testing.T) {
	deps := map[int][]int{1: {2}, 2: {1}}
	if !previewReaches(deps, 1, 2) || !previewReaches(deps, 2, 1) {
		t.Fatal("cycle members do not reach each other")
	}
	if previewReaches(deps, 1, 3) {
		t.Fatal("reached a node outside the cycle")
	}
}
