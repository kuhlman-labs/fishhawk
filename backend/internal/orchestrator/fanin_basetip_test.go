package orchestrator

// Tests for the ADR-096 D4 (#4294) base fold and conflict attribution in
// integrateSlices. "No fold" cases assert on the MergeBranch HEADS (no merge
// whose head is the base sha), not on audit row counts (approval condition C7).

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// comparePair keys a canned ComparePatch answer by (base, head).
type comparePair struct{ base, head string }

// compareStubGitHub embeds *stubGitHub and adds the OPTIONAL ComparePatch
// capability the conflict attribution type-asserts on. An unprogrammed pair
// answers an empty, complete comparison.
type compareStubGitHub struct {
	*stubGitHub
	files  map[comparePair][]string
	trunc  map[comparePair]bool
	errs   map[comparePair]error
	nilRes map[comparePair]bool
	// renames maps a listed path to its rename source for a pair.
	renames map[comparePair]map[string]string
	calls   []comparePair
}

func (g *compareStubGitHub) ComparePatch(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, base, head string) (*forge.ComparePatchResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := comparePair{base, head}
	g.calls = append(g.calls, k)
	if err, ok := g.errs[k]; ok {
		return nil, err
	}
	if g.nilRes[k] {
		return nil, nil
	}
	res := &forge.ComparePatchResult{Truncated: g.trunc[k]}
	for _, p := range g.files[k] {
		f := forge.ComparePatchFile{Path: p, Status: "modified"}
		if prev, ok := g.renames[k][p]; ok {
			f.Status, f.PreviousPath = "renamed", prev
		}
		res.Files = append(res.Files, f)
	}
	return res, nil
}

type foldFixture struct {
	o            *Orchestrator
	rs           *stubRuns
	gh           *compareStubGitHub
	au           *recordingAudit
	parent       *run.Run
	stages       []*run.Stage
	children     []*run.Run
	consolidated string
}

// newFoldFixture seeds a decomposed parent (implement awaiting_children) with
// n succeeded slices, main at "basesha" and the consolidated branch ABSENT.
func newFoldFixture(t *testing.T, n int) *foldFixture {
	t.Helper()
	o, rs, base := newOrchestrator(t)
	gh := &compareStubGitHub{
		stubGitHub: base,
		files:      map[comparePair][]string{},
		trunc:      map[comparePair]bool{},
		errs:       map[comparePair]error{},
		nilRes:     map[comparePair]bool{},
		renames:    map[comparePair]map[string]string{},
	}
	o.GitHub = gh
	o.DefaultRef = "main"
	au := &recordingAudit{}
	o.Audit = au
	gh.branchSHAs = map[string]string{"main": "basesha"}
	parent, stages := seedFanInParent(t, rs, int64Ptr(55))
	children := make([]*run.Run, 0, n)
	for i := 0; i < n; i++ {
		children = append(children, seedSucceededSlice(t, rs, parent.ID, int64Ptr(55), i))
	}
	return &foldFixture{o: o, rs: rs, gh: gh, au: au, parent: parent, stages: stages, children: children, consolidated: consolidatedBranch(parent.ID)}
}

func (f *foldFixture) slice(i int) string { return sliceBranch(f.parent.ID, i) }

// integrateEarlierWave runs a REAL first pass with only the covered slices
// succeeded — creating the consolidated branch at "basesha" and writing the
// real slices_integrated record covering them — then advances main to
// "basesha2", moves the consolidated tip to "consha", makes the covered slices
// answer 204 (already merged) and clears the recorded merge calls. This is the
// earlier-wave shape that leaves the branch on an OLD base.
func (f *foldFixture) integrateEarlierWave(t *testing.T, covered ...int) {
	t.Helper()
	isCovered := map[int]bool{}
	for _, i := range covered {
		isCovered[i] = true
	}
	for i, c := range f.children {
		if !isCovered[i] {
			c.State = run.StateRunning
		}
	}
	if conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID); err != nil || conflict != nil {
		t.Fatalf("earlier-wave IntegrateSlices: conflict=%+v err=%v", conflict, err)
	}
	for _, c := range f.children {
		c.State = run.StateSucceeded
	}
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	f.gh.branchSHAs["main"] = "basesha2"
	f.gh.branchSHAs[f.consolidated] = "consha"
	f.gh.mergeEmptyByHead = map[string]bool{}
	for i := range isCovered {
		f.gh.mergeEmptyByHead[f.slice(i)] = true
	}
	f.gh.mergeCalls = nil
	f.gh.createRefCalls = nil
}

func (f *foldFixture) mergeHeads() []string {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	out := make([]string, 0, len(f.gh.mergeCalls))
	for _, m := range f.gh.mergeCalls {
		out = append(out, m.Head)
	}
	return out
}

func indexOf(heads []string, want string) int {
	for i, h := range heads {
		if h == want {
			return i
		}
	}
	return -1
}

func slicesIntegratedCount(a *recordingAudit) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, p := range a.appended {
		if p.Category == "slices_integrated" {
			n++
		}
	}
	return n
}

func stringsFromAny(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("value %#v is not a JSON array", v)
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// (1) The core mode: a pre-existing consolidated branch on an old base plus a
// pending slice folds the base tip in FIRST.
func TestIntegrateSlices_FoldsBaseTipIntoExistingConsolidated(t *testing.T) {
	f := newFoldFixture(t, 2)
	f.integrateEarlierWave(t, 0)
	rowsBefore := len(integrationCommitRecords(t, f.au))

	if conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID); err != nil || conflict != nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
	}

	f.gh.mu.Lock()
	first := f.gh.mergeCalls[0]
	f.gh.mu.Unlock()
	if first.Base != f.consolidated || first.Head != "basesha2" {
		t.Fatalf("mergeCalls[0] = {%s, %s}, want {%s, basesha2} (the fold must come first)", first.Base, first.Head, f.consolidated)
	}
	if !strings.Contains(first.Msg, "onto main@basesha2 ") || !strings.Contains(first.Msg, "ADR-096 D4") {
		t.Errorf("fold commit message = %q, want it to name main@basesha2 and ADR-096 D4", first.Msg)
	}
	heads := f.mergeHeads()
	if fi, si := indexOf(heads, "basesha2"), indexOf(heads, f.slice(1)); si < 0 || fi > si {
		t.Errorf("merge heads = %v, want the fold strictly before the slice-1 merge", heads)
	}

	rows := integrationCommitRecords(t, f.au)[rowsBefore:]
	if len(rows) != 2 {
		t.Fatalf("new integration_commit_recorded rows = %v, want [fold, slice 1]", rows)
	}
	fold := rows[0]
	if fold["kind"] != "base_fold" || fold["base_sha"] != "basesha2" || fold["merge_sha"] != "mergesha-basesha2" ||
		fold["base_ref"] != "main" || fold["consolidated_branch"] != f.consolidated {
		t.Errorf("fold row = %v, want kind base_fold, base_sha basesha2, merge_sha mergesha-basesha2", fold)
	}
	if _, ok := fold["slice_index"]; ok {
		t.Errorf("fold row carries slice_index: %v", fold)
	}
	if newest := rows[len(rows)-1]; newest["merge_sha"] != "mergesha-"+f.slice(1) {
		t.Errorf("newest integration_commit_recorded = %v, want the slice-1 merge (consolidated-tip invariant)", newest)
	}

	p := auditPayload(t, f.au, "slices_integrated")
	if p["base_sha"] != "basesha2" || p["base_ref"] != "main" || p["base_fold_outcome"] != "merged" {
		t.Errorf("slices_integrated base fields = %v/%v/%v, want basesha2/main/merged", p["base_sha"], p["base_ref"], p["base_fold_outcome"])
	}
	if got, want := integrationSHAsFromPayload(p), []string{"mergesha-basesha2", "mergesha-" + f.slice(1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("integration_commit_shas = %v, want %v (fold sha first)", got, want)
	}
	if _, ok := p["base_fold_conflict"]; ok {
		t.Errorf("base_fold_conflict present on a merged fold: %v", p)
	}
}

// (2) An ABSENT consolidated branch is created at the tip: no fold.
func TestIntegrateSlices_NoFoldWhenConsolidatedCreatedAtTip(t *testing.T) {
	f := newFoldFixture(t, 2)
	if conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID); err != nil || conflict != nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
	}
	if len(f.gh.createRefCalls) != 1 || f.gh.createRefCalls[0].SHA != "basesha" {
		t.Fatalf("CreateRef calls = %+v, want one at basesha", f.gh.createRefCalls)
	}
	if heads := f.mergeHeads(); indexOf(heads, "basesha") >= 0 {
		t.Errorf("merge heads = %v, want no merge of the base sha after CreateRef", heads)
	}
	if p := auditPayload(t, f.au, "slices_integrated"); p["base_fold_outcome"] != "created_at_tip" || p["base_sha"] != "basesha" {
		t.Errorf("slices_integrated base_fold_outcome/base_sha = %v/%v, want created_at_tip/basesha", p["base_fold_outcome"], p["base_sha"])
	}
}

// (3) Re-entrant idempotency: main moved AFTER a record covering every
// succeeded child, so a fold here WOULD move the head; with no pending slice
// it must not.
func TestIntegrateSlices_NoFoldWithoutPendingSlices(t *testing.T) {
	f := newFoldFixture(t, 2)
	f.integrateEarlierWave(t, 0, 1)
	rowsBefore := len(integrationCommitRecords(t, f.au))

	if conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID); err != nil || conflict != nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
	}
	if heads := f.mergeHeads(); indexOf(heads, "basesha2") >= 0 {
		t.Errorf("merge heads = %v, want no fold (no pending slice)", heads)
	}
	if rows := integrationCommitRecords(t, f.au); len(rows) != rowsBefore {
		t.Errorf("integration_commit_recorded rows = %d, want %d (no new row)", len(rows), rowsBefore)
	}
	if p := auditPayload(t, f.au, "slices_integrated"); p["base_fold_outcome"] != "not_attempted" {
		t.Errorf("base_fold_outcome = %v, want not_attempted", p["base_fold_outcome"])
	}
}

// An unreadable slices_integrated history makes every child pending, so a
// pre-existing branch folds (the safe direction: a moved head is unverified).
func TestIntegrateSlices_FoldsWhenIntegratedRecordUnreadable(t *testing.T) {
	f := newFoldFixture(t, 1)
	f.integrateEarlierWave(t, 0)
	f.o.Audit = &erroringCategoryAudit{recordingAudit: f.au}

	if conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID); err != nil || conflict != nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
	}
	if heads := f.mergeHeads(); len(heads) == 0 || heads[0] != "basesha2" {
		t.Errorf("merge heads = %v, want the fold first on an unreadable record", heads)
	}
}

// (4) A 204 fold records nothing; the slices still merge.
func TestIntegrateSlices_FoldAlreadyContained(t *testing.T) {
	f := newFoldFixture(t, 2)
	f.integrateEarlierWave(t, 0)
	f.gh.mergeEmptyByHead["basesha2"] = true
	rowsBefore := len(integrationCommitRecords(t, f.au))

	if conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID); err != nil || conflict != nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
	}
	heads := f.mergeHeads()
	if len(heads) == 0 || heads[0] != "basesha2" {
		t.Fatalf("merge heads = %v, want the fold attempted first", heads)
	}
	if indexOf(heads, f.slice(1)) < 0 {
		t.Errorf("merge heads = %v, want slice 1 still merged", heads)
	}
	rows := integrationCommitRecords(t, f.au)[rowsBefore:]
	if len(rows) != 1 || rows[0]["merge_sha"] != "mergesha-"+f.slice(1) {
		t.Errorf("new integration_commit_recorded rows = %v, want only the slice-1 merge", rows)
	}
	p := auditPayload(t, f.au, "slices_integrated")
	if p["base_fold_outcome"] != "already_contained" {
		t.Errorf("base_fold_outcome = %v, want already_contained", p["base_fold_outcome"])
	}
	if got, want := integrationSHAsFromPayload(p), []string{"mergesha-" + f.slice(1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("integration_commit_shas = %v, want %v", got, want)
	}
}

// (5) A fold conflict degrades: no error, the pending slice still integrates
// on the old base, and the conflict is attributed on slices_integrated.
func TestIntegrateSlices_FoldConflictDegradesWithAttribution(t *testing.T) {
	f := newFoldFixture(t, 2)
	f.integrateEarlierWave(t, 0)
	f.gh.mergeErrByHead = map[string]error{"basesha2": forge.ErrMergeConflict}
	f.gh.files[comparePair{"consha", "basesha2"}] = []string{"b.go", "a.go"}
	f.gh.files[comparePair{"basesha2", f.slice(0)}] = []string{"a.go"}
	rowsBefore := len(integrationCommitRecords(t, f.au))

	conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID)
	if err != nil || conflict != nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v, want nil/nil (degrade)", conflict, err)
	}
	if heads := f.mergeHeads(); indexOf(heads, f.slice(1)) < 0 {
		t.Errorf("merge heads = %v, want slice 1 still merged", heads)
	}
	for _, r := range integrationCommitRecords(t, f.au)[rowsBefore:] {
		if r["kind"] == "base_fold" {
			t.Errorf("a conflicted fold recorded a base_fold row: %v", r)
		}
	}
	p := auditPayload(t, f.au, "slices_integrated")
	if p["base_fold_outcome"] != "conflict" {
		t.Fatalf("base_fold_outcome = %v, want conflict", p["base_fold_outcome"])
	}
	bfc, ok := p["base_fold_conflict"].(map[string]any)
	if !ok {
		t.Fatalf("base_fold_conflict = %#v, want an object", p["base_fold_conflict"])
	}
	if idx, _ := bfc["conflicting_slice_index"].(float64); int(idx) != 0 || bfc["conflicting_child_run_id"] != f.children[0].ID.String() {
		t.Errorf("base_fold_conflict slice = %v/%v, want 0/%s", bfc["conflicting_slice_index"], bfc["conflicting_child_run_id"], f.children[0].ID)
	}
	if got := stringsFromAny(t, bfc["conflicting_base_paths"]); !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Errorf("conflicting_base_paths = %v, want [a.go]", got)
	}
	if bfc["base_attribution"] != "paths_overlap" || bfc["compare_truncated"] != false {
		t.Errorf("base_attribution/compare_truncated = %v/%v, want paths_overlap/false", bfc["base_attribution"], bfc["compare_truncated"])
	}
}

// (6) A non-conflict forge error on the fold parks before any slice merge.
func TestIntegrateSlices_FoldForgeErrorParks(t *testing.T) {
	f := newFoldFixture(t, 2)
	f.integrateEarlierWave(t, 0)
	boom := errors.New("github 500")
	f.gh.mergeErrByHead = map[string]error{"basesha2": boom}
	recordsBefore := slicesIntegratedCount(f.au)

	conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID)
	if !errors.Is(err, boom) || conflict != nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v, want a wrapped %v", conflict, err, boom)
	}
	if heads := f.mergeHeads(); !reflect.DeepEqual(heads, []string{"basesha2"}) {
		t.Errorf("merge heads = %v, want only the fold attempt (zero slice merges)", heads)
	}
	if got := slicesIntegratedCount(f.au); got != recordsBefore {
		t.Errorf("slices_integrated records = %d, want %d (none on a park)", got, recordsBefore)
	}
}

// (7) A pending slice that conflicts with main names the slice and the
// overlapping base paths, through the event-driven settle.
func TestIntegrateSlices_PendingSliceMainConflictNamesSliceAndBasePaths(t *testing.T) {
	f := newFoldFixture(t, 1)
	f.gh.mergeErrByHead = map[string]error{f.slice(0): forge.ErrMergeConflict}
	f.gh.files[comparePair{f.slice(0), "basesha"}] = []string{"y.go", "x.go"}
	f.gh.files[comparePair{"basesha", f.slice(0)}] = []string{"z.go", "y.go"}

	conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID)
	if err != nil || conflict == nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v, want a conflict", conflict, err)
	}
	if conflict.SliceIndex != 0 || conflict.ChildRunID != f.children[0].ID || conflict.BaseSHA != "basesha" ||
		!reflect.DeepEqual(conflict.ConflictingBasePaths, []string{"y.go"}) || conflict.BaseAttribution != "paths_overlap" || conflict.CompareTruncated {
		t.Errorf("conflict = %+v, want slice 0, basesha, [y.go], paths_overlap, not truncated", conflict)
	}
	if !strings.HasSuffix(conflict.Detail, "; conflicting base paths: y.go") {
		t.Errorf("detail = %q, want the base paths appended", conflict.Detail)
	}

	f.o.maybeAdvanceDecomposedParent(context.Background(), f.parent.ID)
	impl := f.stages[0]
	if impl.State != run.StageStateFailed || impl.FailureCategory == nil || *impl.FailureCategory != run.FailureB {
		t.Fatalf("implement stage = %s/%v, want failed/B", impl.State, impl.FailureCategory)
	}
	if impl.FailureReason == nil || !strings.HasPrefix(*impl.FailureReason, sliceIntegrationConflictReasonPrefix+": ") {
		t.Errorf("failure reason = %v, want the stable %q prefix", impl.FailureReason, sliceIntegrationConflictReasonPrefix)
	}
	p := auditPayload(t, f.au, "slice_integration_conflict")
	if idx, _ := p["conflicting_slice_index"].(float64); int(idx) != 0 || p["base_sha"] != "basesha" || p["base_attribution"] != "paths_overlap" || p["compare_truncated"] != false {
		t.Errorf("slice_integration_conflict = %v, want slice 0, basesha, paths_overlap, compare_truncated false", p)
	}
	if got := stringsFromAny(t, p["conflicting_base_paths"]); !reflect.DeepEqual(got, []string{"y.go"}) {
		t.Errorf("conflicting_base_paths = %v, want [y.go]", got)
	}
}

// (8) Attribution failure never changes routing: the conflict still names the
// right slice, with empty paths and an unavailable attribution.
func TestIntegrateSlices_ConflictAttributionUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *foldFixture)
		reason string
	}{
		{"no compare capability", func(f *foldFixture) { f.o.GitHub = f.gh.stubGitHub }, "unavailable:compare_unsupported"},
		{"compare error", func(f *foldFixture) {
			f.gh.errs[comparePair{f.slice(1), "basesha"}] = errors.New("compare 502")
		}, "unavailable:compare_failed"},
		{"second compare error", func(f *foldFixture) {
			f.gh.errs[comparePair{"basesha", f.slice(1)}] = errors.New("compare 502")
		}, "unavailable:compare_failed"},
		{"nil compare result", func(f *foldFixture) {
			f.gh.nilRes[comparePair{f.slice(1), "basesha"}] = true
		}, "unavailable:compare_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFoldFixture(t, 2)
			f.gh.mergeErrByHead = map[string]error{f.slice(1): forge.ErrMergeConflict}
			tc.setup(f)
			conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID)
			if err != nil || conflict == nil {
				t.Fatalf("IntegrateSlices: conflict=%+v err=%v, want the slice-1 conflict", conflict, err)
			}
			if conflict.SliceIndex != 1 || conflict.ChildRunID != f.children[1].ID {
				t.Errorf("conflict names slice %d (%s), want slice 1 (%s)", conflict.SliceIndex, conflict.ChildRunID, f.children[1].ID)
			}
			if conflict.BaseAttribution != tc.reason || conflict.ConflictingBasePaths == nil || len(conflict.ConflictingBasePaths) != 0 {
				t.Errorf("attribution = %q paths = %#v, want %q and an empty non-nil slice", conflict.BaseAttribution, conflict.ConflictingBasePaths, tc.reason)
			}
		})
	}
}

// (9) No overlap on complete compares is a slice-vs-slice conflict.
func TestIntegrateSlices_PendingSliceConflictNoBaseOverlap(t *testing.T) {
	f := newFoldFixture(t, 2)
	f.gh.mergeErrByHead = map[string]error{f.slice(1): forge.ErrMergeConflict}
	f.gh.files[comparePair{f.slice(1), "basesha"}] = []string{"a.go"}
	f.gh.files[comparePair{"basesha", f.slice(1)}] = []string{"b.go"}

	conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID)
	if err != nil || conflict == nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
	}
	if conflict.BaseAttribution != "no_base_overlap" || len(conflict.ConflictingBasePaths) != 0 || conflict.CompareTruncated {
		t.Errorf("conflict = %+v, want no_base_overlap, no paths, not truncated", conflict)
	}
	if strings.Contains(conflict.Detail, "conflicting base paths") {
		t.Errorf("detail = %q, want no base paths appended", conflict.Detail)
	}
}

// C5: a truncated compare is never reported as a confirmed absence of overlap,
// and the flag reaches the persisted conflict payload.
func TestIntegrateSlices_TruncatedCompareNeverReportsNoBaseOverlap(t *testing.T) {
	t.Run("no overlap found", func(t *testing.T) {
		f := newFoldFixture(t, 2)
		f.gh.mergeErrByHead = map[string]error{f.slice(1): forge.ErrMergeConflict}
		f.gh.files[comparePair{f.slice(1), "basesha"}] = []string{"a.go"}
		f.gh.trunc[comparePair{f.slice(1), "basesha"}] = true
		f.gh.files[comparePair{"basesha", f.slice(1)}] = []string{"b.go"}

		f.o.maybeAdvanceDecomposedParent(context.Background(), f.parent.ID)
		p := auditPayload(t, f.au, "slice_integration_conflict")
		if p["base_attribution"] != "unavailable:compare_truncated" || p["compare_truncated"] != true {
			t.Errorf("payload = %v, want unavailable:compare_truncated + compare_truncated true", p)
		}
		if got := stringsFromAny(t, p["conflicting_base_paths"]); len(got) != 0 {
			t.Errorf("conflicting_base_paths = %v, want []", got)
		}
	})
	t.Run("overlap found on a truncated compare", func(t *testing.T) {
		f := newFoldFixture(t, 2)
		f.gh.mergeErrByHead = map[string]error{f.slice(1): forge.ErrMergeConflict}
		f.gh.files[comparePair{f.slice(1), "basesha"}] = []string{"a.go"}
		f.gh.files[comparePair{"basesha", f.slice(1)}] = []string{"a.go"}
		f.gh.trunc[comparePair{"basesha", f.slice(1)}] = true

		conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID)
		if err != nil || conflict == nil {
			t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
		}
		if conflict.BaseAttribution != "paths_overlap" || !conflict.CompareTruncated || !reflect.DeepEqual(conflict.ConflictingBasePaths, []string{"a.go"}) {
			t.Errorf("conflict = %+v, want paths_overlap [a.go] with CompareTruncated", conflict)
		}
	})
}

// C6: the slice side is bounded by the sub-plan's declared scope, so a
// later-wave slice is not attributed an earlier slice's files; a sub-plan with
// no declared scope stays unbounded.
func TestIntegrateSlices_ConflictAttributionBoundedBySliceScope(t *testing.T) {
	cases := []struct {
		name      string
		scopes    [][]string
		mutate    func(f *foldFixture)
		wantPaths []string
	}{
		{"declared scope bounds the slice side", [][]string{{"w.go"}, {"y.go"}}, nil, []string{"y.go"}},
		{"no declared scope stays unbounded", [][]string{{"w.go"}, nil}, nil, []string{"w.go", "y.go"}},
		{"unreadable parent stages stay unbounded", [][]string{{"w.go"}, {"y.go"}}, func(f *foldFixture) {
			f.rs.listStagesErrIDs = map[uuid.UUID]error{f.parent.ID: errors.New("db down")}
		}, []string{"w.go", "y.go"}},
		{"plan without a decomposition stays unbounded", [][]string{{"w.go"}, {"y.go"}}, func(f *foldFixture) {
			arts := f.o.Artifacts.(*fakeArtifacts)
			arts.byStage[f.stages[0].ID][0].Content = []byte(`{"summary":"no decomposition"}`)
		}, []string{"w.go", "y.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopedFoldFixture(t, tc.scopes)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			f.gh.mergeErrByHead = map[string]error{f.slice(1): forge.ErrMergeConflict}
			// Main touched w.go and y.go; slice 1's three-dot compare also lists
			// w.go because its branch carries slice 0's commits.
			f.gh.files[comparePair{f.slice(1), "basesha"}] = []string{"w.go", "y.go"}
			f.gh.files[comparePair{"basesha", f.slice(1)}] = []string{"w.go", "y.go"}

			conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID)
			if err != nil || conflict == nil {
				t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
			}
			if !reflect.DeepEqual(conflict.ConflictingBasePaths, tc.wantPaths) {
				t.Errorf("conflicting base paths = %v, want %v", conflict.ConflictingBasePaths, tc.wantPaths)
			}
		})
	}
}

// newScopedFoldFixture seeds a parent WITH a plan stage whose approved plan
// declares one sub-plan per entry of scopes (nil = no declared scope).
func newScopedFoldFixture(t *testing.T, scopes [][]string) *foldFixture {
	t.Helper()
	f := newFoldFixture(t, 0)
	parent, stages := f.rs.seed(t, "kuhlman-labs/fishhawk", int64Ptr(55), []stageSeed{
		{Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", State: run.StageStateSucceeded},
		{Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateAwaitingChildren},
	})
	subs := make([]map[string]any, 0, len(scopes))
	for i, files := range scopes {
		sp := map[string]any{"title": "slice", "scope_hint": "hint"}
		if files != nil {
			entries := make([]map[string]any, 0, len(files))
			for _, p := range files {
				entries = append(entries, map[string]any{"path": p, "operation": "modify"})
			}
			sp["scope"] = map[string]any{"files": entries}
		}
		subs = append(subs, sp)
		f.children = append(f.children, seedSucceededSlice(t, f.rs, parent.ID, int64Ptr(55), i))
	}
	body, err := json.Marshal(map[string]any{"decomposition": map[string]any{"rationale": "r", "sub_plans": subs}})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	schemaV := "standard_v1"
	f.o.Artifacts = &fakeArtifacts{byStage: map[uuid.UUID][]*artifact.Artifact{
		stages[0].ID: {{ID: uuid.New(), StageID: stages[0].ID, Kind: artifact.KindPlan, SchemaVersion: &schemaV, Content: body, CreatedAt: time.Now().UTC()}},
	}}
	f.parent, f.stages, f.consolidated = parent, stages, consolidatedBranch(parent.ID)
	return f
}

// The fold-conflict attribution modes beyond the attributed case (5).
func TestAttributeFoldConflict_Modes(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(f *foldFixture)
		wantIdx   *int
		wantAttr  string
		wantPaths []string
		wantTrunc bool
	}{
		{"no compare capability", func(f *foldFixture) { f.o.GitHub = f.gh.stubGitHub }, nil, "unavailable:compare_unsupported", []string{}, false},
		{"base delta compare fails", func(f *foldFixture) {
			f.gh.errs[comparePair{"consha", "basesha2"}] = errors.New("502")
		}, nil, "unavailable:compare_failed", []string{}, false},
		{"slice compare fails, no other overlap", func(f *foldFixture) {
			f.gh.files[comparePair{"consha", "basesha2"}] = []string{"a.go"}
			f.gh.errs[comparePair{"basesha2", f.slice(0)}] = errors.New("502")
			f.gh.files[comparePair{"basesha2", f.slice(1)}] = []string{"b.go"}
		}, nil, "unavailable:compare_failed", []string{}, false},
		{"truncated with no overlap", func(f *foldFixture) {
			f.gh.files[comparePair{"consha", "basesha2"}] = []string{"a.go"}
			f.gh.trunc[comparePair{"consha", "basesha2"}] = true
		}, nil, "unavailable:compare_truncated", []string{}, true},
		{"complete with no overlap", func(f *foldFixture) {
			f.gh.files[comparePair{"consha", "basesha2"}] = []string{"a.go"}
			f.gh.files[comparePair{"basesha2", f.slice(0)}] = []string{"b.go"}
		}, nil, "no_base_overlap", []string{}, false},
		{"lowest OVERLAPPING slice is attributed", func(f *foldFixture) {
			f.gh.files[comparePair{"consha", "basesha2"}] = []string{"c.go"}
			f.gh.files[comparePair{"basesha2", f.slice(0)}] = []string{"b.go"}
			f.gh.files[comparePair{"basesha2", f.slice(1)}] = []string{"c.go"}
		}, intPtr(1), "paths_overlap", []string{"c.go"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFoldFixture(t, 3)
			f.integrateEarlierWave(t, 0, 1)
			f.gh.mergeErrByHead = map[string]error{"basesha2": forge.ErrMergeConflict}
			tc.setup(f)
			if conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID); err != nil || conflict != nil {
				t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
			}
			bfc, ok := auditPayload(t, f.au, "slices_integrated")["base_fold_conflict"].(map[string]any)
			if !ok {
				t.Fatal("base_fold_conflict missing")
			}
			idx, hasIdx := bfc["conflicting_slice_index"].(float64)
			_, hasChild := bfc["conflicting_child_run_id"]
			switch {
			case tc.wantIdx == nil && (hasIdx || hasChild):
				t.Errorf("base_fold_conflict = %v, want unattributed (no slice keys)", bfc)
			case tc.wantIdx != nil && (!hasIdx || int(idx) != *tc.wantIdx || bfc["conflicting_child_run_id"] != f.children[*tc.wantIdx].ID.String()):
				t.Errorf("base_fold_conflict = %v, want slice %d", bfc, *tc.wantIdx)
			}
			if bfc["base_attribution"] != tc.wantAttr || bfc["compare_truncated"] != tc.wantTrunc {
				t.Errorf("attribution/truncated = %v/%v, want %s/%v", bfc["base_attribution"], bfc["compare_truncated"], tc.wantAttr, tc.wantTrunc)
			}
			if got := stringsFromAny(t, bfc["conflicting_base_paths"]); !reflect.DeepEqual(got, tc.wantPaths) {
				t.Errorf("conflicting_base_paths = %v, want %v", got, tc.wantPaths)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

// The emitter renders conflicting_base_paths as an array even for a
// SliceConflict built without attribution (a nil slice).
func TestEmitSliceIntegrationConflict_BasePathsAlwaysArray(t *testing.T) {
	f := newFoldFixture(t, 1)
	f.o.emitSliceIntegrationConflict(context.Background(), f.parent.ID, f.stages[0].ID, &SliceConflict{SliceIndex: 0, ChildRunID: f.children[0].ID})
	f.au.mu.Lock()
	raw := string(f.au.appended[len(f.au.appended)-1].Payload)
	f.au.mu.Unlock()
	if !strings.Contains(raw, `"conflicting_base_paths":[]`) {
		t.Errorf("payload = %s, want conflicting_base_paths:[]", raw)
	}
}

// A rename contributes its SOURCE path: main modifying old.go conflicts with a
// slice that renamed old.go to new.go.
func TestIntegrateSlices_ConflictAttributionCountsRenameSource(t *testing.T) {
	f := newFoldFixture(t, 1)
	f.gh.mergeErrByHead = map[string]error{f.slice(0): forge.ErrMergeConflict}
	f.gh.files[comparePair{f.slice(0), "basesha"}] = []string{"old.go"}
	k := comparePair{"basesha", f.slice(0)}
	f.gh.files[k] = []string{"new.go"}
	f.gh.renames[k] = map[string]string{"new.go": "old.go"}

	conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID)
	if err != nil || conflict == nil {
		t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
	}
	if !reflect.DeepEqual(conflict.ConflictingBasePaths, []string{"old.go"}) || conflict.BaseAttribution != "paths_overlap" {
		t.Errorf("conflict = %+v, want [old.go] paths_overlap", conflict)
	}
}

// The fold and its audit rows are best-effort on the audit side: a nil Audit
// (every child then counts as pending) or a failing append still folds and
// integrates.
func TestIntegrateSlices_FoldAuditBestEffort(t *testing.T) {
	t.Run("nil audit", func(t *testing.T) {
		f := newFoldFixture(t, 1)
		f.integrateEarlierWave(t, 0)
		f.o.Audit = nil
		if conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID); err != nil || conflict != nil {
			t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
		}
		if heads := f.mergeHeads(); len(heads) == 0 || heads[0] != "basesha2" {
			t.Errorf("merge heads = %v, want the fold first", heads)
		}
	})
	t.Run("append fails", func(t *testing.T) {
		f := newFoldFixture(t, 2)
		f.integrateEarlierWave(t, 0)
		f.o.Audit = &failingAppendAudit{recordingAudit: f.au}
		if conflict, err := f.o.IntegrateSlices(context.Background(), f.parent.ID); err != nil || conflict != nil {
			t.Fatalf("IntegrateSlices: conflict=%+v err=%v", conflict, err)
		}
		if heads := f.mergeHeads(); len(heads) != 3 || heads[0] != "basesha2" || heads[2] != f.slice(1) {
			t.Errorf("merge heads = %v, want [fold, slice 0, slice 1]", heads)
		}
	})
}

func TestShortSHA(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef01234567"
	if got := shortSHA(full); got != "0123456789ab" {
		t.Errorf("shortSHA(%q) = %q, want 0123456789ab", full, got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA(abc) = %q, want abc", got)
	}
}
