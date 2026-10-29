package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// ownershipSlices builds sub_plans from (files, depends_on) pairs; a nil files
// entry leaves Scope nil (an undeclared slice).
func ownershipSlices(specs ...struct {
	files []string
	deps  []int
}) []plan.SubPlanSummary {
	out := make([]plan.SubPlanSummary, len(specs))
	for i, sp := range specs {
		out[i] = plan.SubPlanSummary{Title: "slice " + string(rune('A'+i)), ScopeHint: "x", DependsOn: sp.deps}
		if sp.files != nil {
			sf := make([]plan.ScopeFile, 0, len(sp.files))
			for _, f := range sp.files {
				sf = append(sf, plan.ScopeFile{Path: f, Operation: plan.FileOpModify})
			}
			out[i].Scope = &plan.Scope{Files: sf}
		}
	}
	return out
}

type ownSpec = struct {
	files []string
	deps  []int
}

func TestSliceAncestors(t *testing.T) {
	for _, tc := range []struct {
		name string
		subs []plan.SubPlanSummary
		idx  int
		want []int
	}{
		{"no deps", ownershipSlices(ownSpec{deps: nil}, ownSpec{deps: nil}), 1, []int{}},
		{"direct", ownershipSlices(ownSpec{}, ownSpec{deps: []int{0}}), 1, []int{0}},
		{"transitive", ownershipSlices(ownSpec{}, ownSpec{deps: []int{0}}, ownSpec{deps: []int{1}}), 2, []int{0, 1}},
		{"diamond", ownershipSlices(ownSpec{}, ownSpec{deps: []int{0}}, ownSpec{deps: []int{0}}, ownSpec{deps: []int{1, 2}}), 3, []int{0, 1, 2}},
		{"defensive cycle", ownershipSlices(ownSpec{deps: []int{1}}, ownSpec{deps: []int{0}}), 0, []int{1}},
		{"self-cycle through a dependency", ownershipSlices(ownSpec{deps: []int{1}}, ownSpec{deps: []int{0}}, ownSpec{deps: []int{0}}), 2, []int{0, 1}},
		{"out-of-range edge ignored", ownershipSlices(ownSpec{}, ownSpec{deps: []int{0, 7, -1}}), 1, []int{0}},
		{"out-of-range idx", ownershipSlices(ownSpec{}), 5, []int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sortedSliceSet(sliceAncestors(tc.subs, tc.idx)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("sliceAncestors(%d) = %v, want %v", tc.idx, got, tc.want)
			}
		})
	}
}

func TestClassifySliceOwnership(t *testing.T) {
	// 0: docs/a.md (no deps)          1: docs/b.md (no deps)
	// 2: pkg/c.go, depends_on [0]     3: pkg/d/ dir, depends_on [2] (transitive 0)
	// 4: depends_on [2] (a DESCENDANT of 2) owning pkg/e.go
	// 5: owns docs/a.md's sibling dir docs/shared/ ; 6: depends_on [0] owns docs/shared/x.md
	subs := ownershipSlices(
		ownSpec{files: []string{"docs/a.md", "docs/shared/x.md"}},
		ownSpec{files: []string{"docs/b.md", "docs/shared/x.md", "pkg/foo"}},
		ownSpec{files: []string{"pkg/c.go"}, deps: []int{0}},
		ownSpec{files: []string{"pkg/d/"}, deps: []int{2}},
		ownSpec{files: []string{"pkg/e.go"}, deps: []int{2}},
	)
	type owner struct {
		slice int
		path  string
		kind  string
	}
	for _, tc := range []struct {
		name          string
		idx           int
		paths         []string
		wantConflicts []owner
		wantAncestor  []owner
	}{
		{name: "own-slice path allowed, no record", idx: 2, paths: []string{"pkg/c.go"}},
		{name: "direct-ancestor-owned allowed", idx: 2, paths: []string{"docs/a.md"},
			wantAncestor: []owner{{0, "docs/a.md", "identical"}}},
		{name: "transitive-ancestor-owned allowed", idx: 3, paths: []string{"docs/a.md"},
			wantAncestor: []owner{{0, "docs/a.md", "identical"}}},
		{name: "non-ancestor same-wave owner refused", idx: 0, paths: []string{"docs/b.md"},
			wantConflicts: []owner{{1, "docs/b.md", "identical"}}},
		{name: "non-ancestor earlier-wave non-dependency refused", idx: 2, paths: []string{"docs/b.md"},
			wantConflicts: []owner{{1, "docs/b.md", "identical"}}},
		{name: "descendant-owned refused", idx: 2, paths: []string{"pkg/e.go"},
			wantConflicts: []owner{{4, "pkg/e.go", "identical"}}},
		{name: "directory containment refused with overlap_kind", idx: 0, paths: []string{"pkg/d/x.go"},
			wantConflicts: []owner{{3, "pkg/d/", "descendant_path"}}},
		{name: "requested directory containing an owned file refused", idx: 0, paths: []string{"pkg/"},
			wantConflicts: []owner{{1, "pkg/foo", "ancestor_directory"}, {2, "pkg/c.go", "ancestor_directory"}, {3, "pkg/d/", "ancestor_directory"}, {4, "pkg/e.go", "ancestor_directory"}}},
		{name: "segment-prefix sibling is not an overlap", idx: 0, paths: []string{"pkg/foobar"}},
		{name: "owned by both an ancestor and a non-ancestor names only the non-ancestor as a conflict", idx: 2, paths: []string{"docs/shared/x.md"},
			wantConflicts: []owner{{1, "docs/shared/x.md", "identical"}},
			wantAncestor:  []owner{{0, "docs/shared/x.md", "identical"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := classifySliceOwnership(subs, tc.idx, tc.paths)
			if !c.Checked || c.SliceIndex != tc.idx {
				t.Fatalf("check = %+v, want checked for slice %d", c, tc.idx)
			}
			flatten := func(os []sliceOwnershipOverlap) []owner {
				out := []owner{}
				for _, o := range os {
					out = append(out, owner{o.OwningSlice, o.OwningPath, o.OverlapKind})
				}
				return out
			}
			wantC, wantA := tc.wantConflicts, tc.wantAncestor
			if wantC == nil {
				wantC = []owner{}
			}
			if wantA == nil {
				wantA = []owner{}
			}
			if got := flatten(c.Conflicts); !reflect.DeepEqual(got, wantC) {
				t.Errorf("Conflicts = %v, want %v", got, wantC)
			}
			if got := flatten(c.AncestorOwned); !reflect.DeepEqual(got, wantA) {
				t.Errorf("AncestorOwned = %v, want %v", got, wantA)
			}
			if c.refused() != (len(wantC) > 0) {
				t.Errorf("refused() = %v, want %v", c.refused(), len(wantC) > 0)
			}
		})
	}
	var nilCheck *sliceOwnershipCheck
	if nilCheck.refused() {
		t.Error("a nil check (non-child run) must never refuse")
	}
}

func TestSliceSharedPathOverlaps(t *testing.T) {
	dec := func(subs []plan.SubPlanSummary) *plan.Decomposition {
		return &plan.Decomposition{Rationale: "r", SubPlans: subs}
	}
	t.Run("cross-wave containment fires", func(t *testing.T) {
		got := sliceSharedPathOverlaps(dec(ownershipSlices(
			ownSpec{files: []string{"docs/guide/"}},
			ownSpec{files: []string{"pkg/a.go"}},
			ownSpec{files: []string{"docs/guide/intro.md"}, deps: []int{1}},
		)))
		want := []sliceSharedPathOverlap{{SliceA: 0, SliceB: 2, TitleA: "slice A", TitleB: "slice C",
			PathA: "docs/guide/", PathB: "docs/guide/intro.md", WaveA: 0, WaveB: 1, Related: false}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("overlaps = %+v, want %+v", got, want)
		}
	})
	t.Run("same-wave containment fires; a dependency pair is related", func(t *testing.T) {
		got := sliceSharedPathOverlaps(dec(ownershipSlices(
			ownSpec{files: []string{"docs/guide"}},
			ownSpec{files: []string{"docs/guide/intro.md"}},
		)))
		if len(got) != 1 || got[0].WaveA != 0 || got[0].WaveB != 0 || got[0].Related {
			t.Errorf("same-wave overlaps = %+v, want one unrelated wave-0 pair", got)
		}
		rel := sliceSharedPathOverlaps(dec(ownershipSlices(
			ownSpec{files: []string{"docs/guide/"}},
			ownSpec{files: []string{"docs/guide/intro.md"}, deps: []int{0}},
		)))
		if len(rel) != 1 || !rel[0].Related {
			t.Errorf("dependent overlaps = %+v, want one related pair", rel)
		}
	})
	t.Run("trailing-slash spelling fires", func(t *testing.T) {
		if got := sliceSharedPathOverlaps(dec(ownershipSlices(
			ownSpec{files: []string{"docs/guide"}},
			ownSpec{files: []string{"docs/guide/"}},
		))); len(got) != 1 {
			t.Errorf("trailing-slash overlaps = %+v, want 1", got)
		}
	})
	for _, tc := range []struct {
		name string
		d    *plan.Decomposition
	}{
		{"raw-equal duplicate does not fire (the validator refuses it)", dec(ownershipSlices(
			ownSpec{files: []string{"docs/a.md"}}, ownSpec{files: []string{"docs/a.md"}, deps: []int{0}}))},
		{"disjoint is silent", dec(ownershipSlices(
			ownSpec{files: []string{"docs/a.md"}}, ownSpec{files: []string{"docs/b.md"}}))},
		{"segment-prefix sibling is silent", dec(ownershipSlices(
			ownSpec{files: []string{"pkg/foo"}}, ownSpec{files: []string{"pkg/foobar"}}))},
		{"undeclared scope is silent", dec(ownershipSlices(
			ownSpec{files: []string{"docs/"}}, ownSpec{}))},
		{"non-decomposed is nil", nil},
		{"cyclic depends_on is nil", dec(ownershipSlices(
			ownSpec{files: []string{"docs/"}, deps: []int{1}}, ownSpec{files: []string{"docs/x.md"}, deps: []int{0}}))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sliceSharedPathOverlaps(tc.d); got != nil {
				t.Errorf("overlaps = %+v, want nil", got)
			}
		})
	}
}

func TestSliceSharedPathWarningsAndFindings(t *testing.T) {
	p := &plan.Plan{Decomposition: &plan.Decomposition{Rationale: "r", SubPlans: ownershipSlices(
		ownSpec{files: []string{"docs/guide/"}},
		ownSpec{files: []string{"docs/guide/intro.md"}, deps: []int{0}},
	)}}
	w := sliceSharedPathWarnings(p)
	if len(w) != 1 {
		t.Fatalf("warnings = %v, want 1", w)
	}
	for _, want := range []string{"in different waves", "their edits land in sequence", "single owner", "integration slice"} {
		if !strings.Contains(w[0], want) {
			t.Errorf("related warning missing %q:\n%s", want, w[0])
		}
	}
	f := sliceSharedPathFindings(p)
	if len(f) != 1 || f[0].Pattern != surfacePatternSliceSharedPath || len(f[0].Slices) != 2 ||
		f[0].Slices[0].SliceTitle != "slice A" || f[0].Slices[1].Files[0] != "docs/guide/intro.md" {
		t.Errorf("findings = %+v", f)
	}
	if sliceSharedPathWarnings(nil) != nil || sliceSharedPathFindings(nil) != nil ||
		sliceSharedPathWarnings(&plan.Plan{}) != nil || sliceSharedPathFindings(&plan.Plan{}) != nil {
		t.Error("a nil or non-decomposed plan must yield nil")
	}
}

func TestSliceOwnershipRefusalMessage(t *testing.T) {
	c := &sliceOwnershipCheck{SliceIndex: 2, AncestorSlices: []int{0}, Checked: true,
		Conflicts: []sliceOwnershipOverlap{{Path: "docs/b/x.md", OwningSlice: 1, OwningTitle: "docs slice",
			OwningPath: "docs/b/", OwningChildRunID: "child-1", OverlapKind: "descendant_path"}}}
	req := sliceOwnershipRefusalMessage(c, "request")
	for _, want := range []string{"docs/b/x.md is owned by slice 1 (\"docs slice\", child run child-1) as docs/b/ (descendant_path)",
		"this slice 2 (ancestors: 0)", "Remedy: leave this edit to the owning slice"} {
		if !strings.Contains(req, want) {
			t.Errorf("request message missing %q:\n%s", want, req)
		}
	}
	dec := sliceOwnershipRefusalMessage(&sliceOwnershipCheck{SliceIndex: 0, Conflicts: c.Conflicts[:1]}, "decide")
	for _, want := range []string{"(ancestors: none)", "There is no override: DENY this amendment"} {
		if !strings.Contains(dec, want) {
			t.Errorf("decide message missing %q:\n%s", want, dec)
		}
	}
}
