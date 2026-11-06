package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
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

// --- #4798 ------------------------------------------------------------------

func TestSliceOwnershipRefusalMessage_PreApprovalPhases(t *testing.T) {
	c := &sliceOwnershipCheck{SliceIndex: 2, AncestorSlices: []int{0}, Checked: true,
		Conflicts: []sliceOwnershipOverlap{{Path: "docs/b.md", OwningSlice: 1, OwningTitle: "docs slice",
			OwningPath: "docs/b.md", OwningChildRunID: "child-1", OverlapKind: "identical"}}}
	for _, tc := range []struct {
		phase string
		wants []string
	}{
		{sliceOwnershipPhaseResume, []string{"child run child-1", "still failed and nothing was re-opened", "corrected decomposition"}},
		{sliceOwnershipPhaseAmend, []string{"child run child-1", "build_required_path", "decide fail", "depends_on edge"}},
	} {
		msg := sliceOwnershipRefusalMessage(c, tc.phase)
		for _, want := range tc.wants {
			if !strings.Contains(msg, want) {
				t.Errorf("%s message missing %q:\n%s", tc.phase, want, msg)
			}
		}
	}
	// The request and decide bodies do not change: the new phases only add text
	// on their own arms.
	if req := sliceOwnershipRefusalMessage(c, "request"); !strings.Contains(req, "Remedy: "+sliceOwnershipRemedy) {
		t.Errorf("request message lost its remedy:\n%s", req)
	}
	if d := sliceOwnershipRefusalDetails(c); d["remedy"] != sliceOwnershipRemedy {
		t.Errorf("details remedy = %v, want the request/decide remedy", d["remedy"])
	}
	if _, has := sliceOwnershipRefusalDetails(c)["retryable"]; has {
		t.Error("request and decide details must not gain retryable")
	}
}

// TestSliceOwnershipCheck_WireShape pins that the unexported own-slice field never
// reaches a payload: exactly the six exported keys, byte-identical to a check with
// no own-slice overlap.
func TestSliceOwnershipCheck_WireShape(t *testing.T) {
	subs := ownershipSlices(ownSpec{files: []string{"pkg/a.go"}}, ownSpec{files: []string{"pkg/b.go"}})
	withOwn := classifySliceOwnership(subs, 0, []string{"pkg/a.go"})
	if len(withOwn.ownSlice) != 1 {
		t.Fatalf("fixture must record an own-slice overlap: %+v", withOwn)
	}
	withoutOwn := classifySliceOwnership(subs, 0, []string{"pkg/none.go"})
	rawWith, err := json.Marshal(withOwn)
	if err != nil {
		t.Fatal(err)
	}
	rawWithout, err := json.Marshal(withoutOwn)
	if err != nil {
		t.Fatal(err)
	}
	if string(rawWith) != string(rawWithout) {
		t.Errorf("own-slice overlap changed the wire shape:\n with: %s\n  w/o: %s", rawWith, rawWithout)
	}
	const wantExact = `{"slice_index":0,"ancestor_slices":[],"conflicts":[],"ancestor_owned":[],"checked":true}`
	if string(rawWith) != wantExact {
		t.Errorf("wire bytes = %s, want %s", rawWith, wantExact)
	}

	errored := &sliceOwnershipCheck{Error: "x", ownSlice: withOwn.ownSlice}
	raw, err := json.Marshal(errored)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"ancestor_owned", "ancestor_slices", "checked", "conflicts", "error", "slice_index"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("serialized keys = %v, want exactly %v", keys, want)
	}
}

func TestAttributedOwners_AscendingOwnersPerSlice(t *testing.T) {
	subs := ownershipSlices(
		ownSpec{files: []string{"own/", "shared/file.go"}, deps: []int{3, 1}},
		ownSpec{files: []string{"shared/", "shared/file.go"}},
		ownSpec{files: []string{"shared/other.go"}},
		ownSpec{files: []string{"shared/file.go"}},
	)
	c := classifySliceOwnership(subs, 0, []string{"shared/file.go", "own/x.go", "docs/nobody.md"})
	indices := func(os []run.BuildRequiredOwner) []int {
		out := []int{}
		for _, o := range os {
			if o.SliceIndex != nil {
				out = append(out, *o.SliceIndex)
			}
		}
		return out
	}
	if got, ok := c.attributedOwners("shared/file.go"); !ok || !reflect.DeepEqual(indices(got), []int{0, 1, 3}) {
		t.Errorf("shared/file.go owners = %v (%v), want own slice then ascending ancestors, one per slice: [0 1 3]", indices(got), ok)
	}
	if got, ok := c.attributedOwners("own/x.go"); !ok || !reflect.DeepEqual(indices(got), []int{0}) || got[0].Path != "own/x.go" || got[0].SliceTitle != "slice A" {
		t.Errorf("own/x.go owners = %+v (%v), want the own slice", got, ok)
	}
	if got, ok := c.attributedOwners("docs/nobody.md"); ok || len(got) != 0 {
		t.Errorf("an undeclared path has no owners: %+v (%v)", got, ok)
	}
	var nilCheck *sliceOwnershipCheck
	if got, ok := nilCheck.attributedOwners("x"); ok || got != nil {
		t.Errorf("nil receiver = %+v (%v), want none", got, ok)
	}
}

func TestIsAncestorSlice(t *testing.T) {
	c := classifySliceOwnership(ownershipSlices(ownSpec{}, ownSpec{deps: []int{0}}, ownSpec{deps: []int{1}}), 2, nil)
	if !c.isAncestorSlice(0) || !c.isAncestorSlice(1) {
		t.Errorf("direct and transitive ancestors must match: %+v", c.AncestorSlices)
	}
	if c.isAncestorSlice(2) || c.isAncestorSlice(3) {
		t.Error("the own slice and a stranger are not ancestors")
	}
	var nilCheck *sliceOwnershipCheck
	if nilCheck.isAncestorSlice(0) {
		t.Error("a nil check has no ancestors")
	}
}

// TestChildWaveGateApplies mirrors runnerbackend.Resolver's routing, including the
// #1980 case: a child minted un-resolved with the kind copied.
func TestChildWaveGateApplies(t *testing.T) {
	readErr := errors.New("parent read failed")
	for _, tc := range []struct {
		name           string
		kind           string
		locked         bool
		parentKind     string
		parentLocked   bool
		parentReadErr  bool
		want           bool
		wantErr        bool
		wantParentRead bool
	}{
		{name: "locked local", kind: run.RunnerKindLocal, locked: true, want: true},
		{name: "locked github_actions", kind: run.RunnerKindGitHubActions, locked: true},
		{name: "locked gitlab_ci", kind: run.RunnerKindGitLabCI, locked: true},
		{name: "unknown kind", kind: "banana", locked: true},
		{name: "empty kind", kind: "", locked: false},
		{name: "unresolved github_actions never reads the parent", kind: run.RunnerKindGitHubActions, parentReadErr: true},
		{name: "unresolved local under parent locked local", kind: run.RunnerKindLocal, parentKind: run.RunnerKindLocal, parentLocked: true, want: true, wantParentRead: true},
		{name: "unresolved local under parent locked remote", kind: run.RunnerKindLocal, parentKind: run.RunnerKindGitHubActions, parentLocked: true, wantParentRead: true},
		{name: "unresolved local under unresolved parent", kind: run.RunnerKindLocal, want: true, wantParentRead: true},
		{name: "unresolved local with parent read error", kind: run.RunnerKindLocal, parentReadErr: true, wantErr: true, wantParentRead: true},
		{name: "locked local never reads the parent", kind: run.RunnerKindLocal, locked: true, parentReadErr: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := newOrchestratorRepo()
			parent := rr.seedRun()
			parent.RunnerKind = tc.parentKind
			parent.RunnerKindResolved = tc.parentLocked
			parentID := parent.ID
			child := &run.Run{ID: uuid.New(), RunnerKind: tc.kind, RunnerKindResolved: tc.locked, DecomposedFrom: &parentID}
			repo := &countingGetRunRepo{orchestratorRepo: rr}
			if tc.parentReadErr {
				repo.err = readErr
			}
			s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo})
			got, err := s.childWaveGateApplies(context.Background(), child)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("applies = %v, want %v", got, tc.want)
			}
			if (repo.calls > 0) != tc.wantParentRead {
				t.Errorf("parent reads = %d, want read=%v", repo.calls, tc.wantParentRead)
			}
		})
	}
}

// countingGetRunRepo counts GetRun and optionally fails it.
type countingGetRunRepo struct {
	*orchestratorRepo
	calls int
	err   error
}

func (r *countingGetRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.orchestratorRepo.GetRun(ctx, id)
}

// TestSliceOwnershipForRun_Sentinels pins each sentinel per shape and the
// not-a-child (nil, nil).
func TestSliceOwnershipForRun_Sentinels(t *testing.T) {
	paths := []scopeamendment.PathEntry{{Path: "docs/x.md", Operation: scopeamendment.OperationModify}}
	notChild := func(t *testing.T) (*amendFixture, *run.Run) {
		f := newAmendFixture(t)
		return f, &run.Run{ID: uuid.New()}
	}
	t.Run("not a child", func(t *testing.T) {
		f, r := notChild(t)
		got, err := f.s.sliceOwnershipForRun(context.Background(), r, paths)
		if got != nil || err != nil {
			t.Errorf("= %+v, %v; want nil, nil", got, err)
		}
	})
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *amendFixture)
		want  error
	}{
		{"nil slice index", func(_ *testing.T, f *amendFixture) { f.child.SliceIndex = nil }, errSliceOwnershipPlanAbsent},
		{"no plan artifact", func(_ *testing.T, f *amendFixture) { f.art.all = nil }, errSliceOwnershipPlanAbsent},
		{"no decomposition", func(t *testing.T, f *amendFixture) {
			f.seedParentPlan(t, &plan.Plan{PlanVersion: "standard_v1"})
		}, errSliceOwnershipPlanAbsent},
		{"slice out of range", func(_ *testing.T, f *amendFixture) {
			idx := 9
			f.child.SliceIndex = &idx
		}, errSliceOwnershipPlanAbsent},
		{"negative slice", func(_ *testing.T, f *amendFixture) {
			idx := -1
			f.child.SliceIndex = &idx
		}, errSliceOwnershipPlanAbsent},
		{"nil artifact repo", func(_ *testing.T, f *amendFixture) { f.s.cfg.ArtifactRepo = nil }, errSliceOwnershipPlanUnconfigured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAmendFixture(t)
			tc.setup(t, f)
			_, err := f.s.sliceOwnershipForRun(context.Background(), f.child, paths)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("read error is ordinary", func(t *testing.T) {
		f := newAmendFixture(t)
		f.art.listErr = errors.New("offline")
		_, err := f.s.sliceOwnershipForRun(context.Background(), f.child, paths)
		if err == nil || errors.Is(err, errSliceOwnershipPlanAbsent) || errors.Is(err, errSliceOwnershipPlanUnconfigured) {
			t.Errorf("err = %v, want an ordinary read failure", err)
		}
	})
}

// TestAmendmentSliceOwnership_AbsentPlanPreservedOnRequestAndDecide pins that the
// two new channels' fail-closed partition did not reach the request and decide
// routes: an absent plan still answers (nil, nil) there, and the decide route
// approves through a real handler call.
func TestAmendmentSliceOwnership_AbsentPlanPreservedOnRequestAndDecide(t *testing.T) {
	f := newAmendFixture(t)
	f.art.all = nil
	paths := []scopeamendment.PathEntry{{Path: amendCoupledPath, Operation: scopeamendment.OperationModify}}
	if got, err := f.s.amendmentSliceOwnership(context.Background(), f.child.ID, paths); got != nil || err != nil {
		t.Errorf("amendmentSliceOwnership = %+v, %v; want nil, nil", got, err)
	}
	if _, err := f.s.sliceOwnershipForRun(context.Background(), f.child, paths); !errors.Is(err, errSliceOwnershipPlanAbsent) {
		t.Errorf("sliceOwnershipForRun err = %v, want the absent sentinel", err)
	}
	a, err := f.sa.Create(context.Background(), scopeamendment.CreateParams{
		RunID: f.child.ID, StageID: f.stage.ID, Paths: paths, Reason: "agent asked",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.stage.State = run.StageStateRunning
	w := postDecision(t, f.s, f.child.ID, a.ID, `{"decision":"approve","reason":"ok"}`, operatorWriteStages)
	if w.Code != http.StatusOK {
		t.Fatalf("decide status = %d, want 200 on an absent plan; body = %s", w.Code, w.Body.String())
	}
	got, err := f.sa.ListByRun(context.Background(), f.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != scopeamendment.StatusApproved {
		t.Errorf("rows = %+v, want one approved", got)
	}
}

// TestSliceOwnership_RequestAndDecideWireBytesUnchanged drives the REAL request and
// decide routes on an own-slice path, the one case the new unexported field
// records, and pins the exact slice_ownership bytes each audit payload carries.
func TestSliceOwnership_RequestAndDecideWireBytesUnchanged(t *testing.T) {
	const want = `{"slice_index":2,"ancestor_slices":[0],"conflicts":[],"ancestor_owned":[],"checked":true}`
	rawOwnership := func(t *testing.T, f *sliceOwnershipFixture, category string) string {
		t.Helper()
		f.au.mu.Lock()
		defer f.au.mu.Unlock()
		for _, p := range f.au.appended {
			if p.Category != category {
				continue
			}
			var payload struct {
				SliceOwnership json.RawMessage `json:"slice_ownership"`
			}
			if err := json.Unmarshal(p.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			return string(payload.SliceOwnership)
		}
		t.Fatalf("no %s entry", category)
		return ""
	}
	f := newSliceOwnershipFixture(t)
	w := f.request(t, f.children[2].ID, "pkg/c.go")
	if w.Code != http.StatusCreated {
		t.Fatalf("request status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if got := rawOwnership(t, f, CategoryScopeAmendmentRequested); got != want {
		t.Errorf("requested slice_ownership = %s, want %s", got, want)
	}
	id := f.seedPending(t, "pkg/c.go")
	if w := f.decide(t, f.children[2].ID, id, "approve"); w.Code != http.StatusOK {
		t.Fatalf("decide status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := rawOwnership(t, f, CategoryScopeAmendmentDecided); got != want {
		t.Errorf("decided slice_ownership = %s, want %s", got, want)
	}
}
