package workmgmt

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func groomingRef(n int) plan.ItemRef {
	return plan.ItemRef{
		Type: "github_issue",
		ID:   fmt.Sprintf("kuhlman-labs/fishhawk#%d", n),
		URL:  fmt.Sprintf("https://github.com/kuhlman-labs/fishhawk/issues/%d", n),
	}
}

func groomingTarget() Target {
	return Target{Repo: Repo{Owner: "kuhlman-labs", Name: "fishhawk"}}
}

func groomingStates() map[string]string {
	return map[string]string{
		CanonicalStateBacklog:    "Backlog",
		CanonicalStateUpNext:     "Up Next",
		CanonicalStateInProgress: "In Progress",
		CanonicalStateDone:       "Done",
	}
}

// hygieneEntry builds a hygiene defect carrying `fix` in the STRUCTURED member
// this defect's mutation kind reads (#2847). An empty `fix` leaves Fix NIL —
// the absent-proposal case.
//
// The `suggested_fix` PROSE is deliberately fixed and lexically DISJOINT from
// every structured value these tests use, so a dispatched-value assertion can
// never accidentally pass on prose the code was not supposed to read.
func hygieneEntry(n int, defect, fix string) plan.HygieneDefect {
	r := groomingRef(n)
	d := plan.HygieneDefect{
		ID:           plan.GroomingEntryID(plan.GroomingClassHygiene, defect, r),
		ItemRef:      r,
		Defect:       defect,
		Detail:       "detail",
		SuggestedFix: "prose only a human reads; never dispatched",
	}
	if fix != "" {
		d.Fix = structuredFixFor(defect, fix)
	}
	return d
}

// structuredFixFor routes one value into the fix member the defect's mutation
// kind reads. A defect with no mechanical mutation (absent_done_means) still
// gets a FieldValue so the fixture is non-nil; its unmappable-defect skip is
// decided before any fix is read.
func structuredFixFor(defect, value string) *plan.HygieneFix {
	switch defect {
	case "missing_label_namespace":
		return &plan.HygieneFix{Labels: []string{value}}
	case "unlinked_parent_epic", "missing_parent_epic_link":
		return &plan.HygieneFix{ParentEpic: value}
	case "unboarded":
		return &plan.HygieneFix{BoardState: value}
	default:
		return &plan.HygieneFix{FieldValue: value}
	}
}

func orderingEntry(n, rank int) plan.OrderingEntry {
	r := groomingRef(n)
	return plan.OrderingEntry{
		ID:              plan.GroomingEntryID(plan.GroomingClassOrdering, "", r),
		ItemRef:         r,
		Rank:            rank,
		Score:           1,
		RubricCitations: []plan.RubricCitation{{RubricID: "U1"}},
	}
}

func duplicateEntry(a, b int) plan.DuplicateCandidate {
	ra, rb := groomingRef(a), groomingRef(b)
	return plan.DuplicateCandidate{
		ID:         plan.GroomingEntryID(plan.GroomingClassDuplicate, "", ra, rb),
		Pair:       []plan.ItemRef{ra, rb},
		Basis:      "same defect",
		Confidence: "high",
	}
}

func dependencyEntry(from, to int) plan.DependencyEdge {
	rf, rt := groomingRef(from), groomingRef(to)
	return plan.DependencyEdge{
		ID:    plan.GroomingEntryID(plan.GroomingClassDependency, "", rf, rt),
		From:  rf,
		To:    rt,
		Basis: "shared seam",
		Kind:  "depends_on",
	}
}

func decompositionEntry(n int) plan.DecompositionSuggestion {
	r := groomingRef(n)
	return plan.DecompositionSuggestion{
		ID:        plan.GroomingEntryID(plan.GroomingClassDecomposition, "", r),
		ItemRef:   r,
		Rationale: "too large",
		ProposedChildren: []plan.DecompositionChild{
			{Title: "a", ScopeHint: "x"}, {Title: "b", ScopeHint: "y"},
		},
	}
}

func visionDriftEntry(n int) plan.VisionDriftFlag {
	r := groomingRef(n)
	return plan.VisionDriftFlag{
		ID:           plan.GroomingEntryID(plan.GroomingClassVisionDrift, "V3", r),
		ItemRef:      r,
		Basis:        "non-goal",
		CharterRefID: "V3",
		Detail:       "advances a charter non-goal",
	}
}

// approveAll builds an approved decision for every entry id in the report, so
// a test that is not ABOUT the approval gate does not have to restate it.
func approveAll(report *plan.GroomingReport) []GroomingDecision {
	var out []GroomingDecision
	for id := range groomingReportEntryIDs(report) {
		out = append(out, GroomingDecision{EntryID: id, Verdict: GroomingApproved})
	}
	return out
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeGroomingMutator records EVERY dispatch. Tests assert on this call log
// rather than on a returned error, because a containment check that fired and
// a mutation that silently did nothing return byte-identical envelopes — the
// committed state (what the provider was asked to do) is the discriminating
// observation.
type fakeGroomingMutator struct {
	calls  []GroomingMutationRequest
	errOn  func(GroomingMutationRequest) error
	result *GroomingMutationResult
}

func (f *fakeGroomingMutator) ApplyGroomingMutation(_ context.Context, req GroomingMutationRequest) (*GroomingMutationResult, error) {
	f.calls = append(f.calls, req)
	if f.errOn != nil {
		if err := f.errOn(req); err != nil {
			return nil, err
		}
	}
	if f.result != nil {
		return f.result, nil
	}
	return &GroomingMutationResult{Applied: true, ProviderResponse: "applied " + string(req.Kind)}, nil
}

// destructiveCalls is the CLOSE-call log: the dispatches that actually removed
// something from the backlog.
func (f *fakeGroomingMutator) destructiveCalls() []GroomingMutationRequest {
	var out []GroomingMutationRequest
	for _, c := range f.calls {
		if c.Kind.Destructive() {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeGroomingMutator) kinds() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, string(c.Kind))
	}
	return out
}

// fakeGroomingReader serves canned records by ref, or a canned error.
type fakeGroomingReader struct {
	items map[string]*WorkItemRecord
	err   error
	reads []ReadWorkItemRequest
}

func (f *fakeGroomingReader) ReadWorkItem(_ context.Context, req ReadWorkItemRequest) (*WorkItemRecord, error) {
	f.reads = append(f.reads, req)
	if f.err != nil {
		return nil, f.err
	}
	if it, ok := f.items[req.Ref]; ok {
		cp := *it
		cp.Labels = append([]string(nil), it.Labels...)
		return &cp, nil
	}
	return &WorkItemRecord{State: "open"}, nil
}

func (f *fakeGroomingReader) ListWorkItems(context.Context, ListWorkItemsRequest) (*WorkItemPage, error) {
	return nil, errors.New("not used by the apply layer")
}

// fakeGroomingSink records every audit call and can fail on a chosen entry.
type fakeGroomingSink struct {
	records    []GroomingMutationRecord
	summaries  []GroomingApplySummary
	failEntry  string
	summaryErr error
}

func (f *fakeGroomingSink) RecordGroomingMutation(_ context.Context, rec GroomingMutationRecord) error {
	f.records = append(f.records, rec)
	if f.failEntry != "" && rec.EntryID == f.failEntry {
		return errors.New("sink down")
	}
	return nil
}

func (f *fakeGroomingSink) RecordGroomingApplyCompleted(_ context.Context, sum GroomingApplySummary) error {
	f.summaries = append(f.summaries, sum)
	return f.summaryErr
}

// statefulGroomingForge is BOTH reader and mutator over one mutable item set:
// a mutation it applies is visible to the next read. It is what makes the
// re-apply idempotence test a real round trip rather than a canned reply.
//
// EVERY write below must be one the REAL provider performs. That is not a
// stylistic constraint: a fake that persists something production does not
// manufactures the evidence that a defect is absent, which is exactly how the
// epic-link idempotence gap survived the first pass (#2237 review) — the fake
// appended a `Parent epic:` body marker the provider never wrote, so the
// idempotence test passed on behaviour that existed only in the test. The
// marker write is now real (github/grooming.go::groomingLinkEpic), and the
// provider-side round trip is pinned independently by
// github/grooming_test.go::TestApplyGroomingMutation_EpicLinkReapplyIsANoOp,
// so this fake mirrors a shipped write rather than inventing one. Adding a
// case here without a corresponding provider write reopens that hole.
type statefulGroomingForge struct {
	items map[string]*WorkItemRecord
	calls []GroomingMutationRequest
}

func (f *statefulGroomingForge) ReadWorkItem(_ context.Context, req ReadWorkItemRequest) (*WorkItemRecord, error) {
	it, ok := f.items[req.Ref]
	if !ok {
		return nil, fmt.Errorf("no item %s", req.Ref)
	}
	cp := *it
	cp.Labels = append([]string(nil), it.Labels...)
	return &cp, nil
}

func (f *statefulGroomingForge) ListWorkItems(context.Context, ListWorkItemsRequest) (*WorkItemPage, error) {
	return nil, errors.New("not used by the apply layer")
}

func (f *statefulGroomingForge) ApplyGroomingMutation(_ context.Context, req GroomingMutationRequest) (*GroomingMutationResult, error) {
	f.calls = append(f.calls, req)
	it, ok := f.items[req.ItemRef]
	if !ok {
		return nil, fmt.Errorf("no item %s", req.ItemRef)
	}
	switch req.Kind {
	case GroomingKindLabelSet:
		it.Labels = append(it.Labels, req.After.List...)
	case GroomingKindEpicLink:
		// IDEMPOTENCE IS THE STRUCTURAL PARENT (#2952), not the marker. The real
		// provider skips when the sub-issue edge already records the proposed
		// parent; the fake mirrors that by keying on ParentRef, which its own
		// AddSubIssue-equivalent write below sets. A body-marked-but-unlinked
		// item (empty ParentRef with a marker present) is precisely the case that
		// must NOT skip — the marker is a rendering, not the relationship.
		want := "#" + strings.TrimPrefix(strings.TrimSpace(req.After.Scalar), "#")
		if it.ParentRef == want {
			return &GroomingMutationResult{Skipped: true, SkipReason: "parent epic already linked"}, nil
		}
		// The write persists the STRUCTURAL edge (what AddSubIssue does) and
		// stamps the marker only when absent (branch 4/5 of groomingLinkEpic).
		it.ParentRef = want
		if !bodyCarriesMarker(it.Body, "Parent epic:") {
			it.Body += "\nParent epic: " + want
		}
	case GroomingKindDependsOnAdd:
		// ADDITIVE, mirroring the provider's appendDependsOnRef (#2860): a
		// second edge out of the same item MERGES into the existing marker
		// line. A ref already recorded there is the genuine idempotent no-op
		// and reports a provider SKIP. The fake reproduces the SHAPE of the
		// production behaviour deliberately as an independent implementation,
		// so the core's own membership diff is not tested against itself.
		ref := "#" + strings.TrimPrefix(strings.TrimSpace(req.After.Scalar), "#")
		if line, ok := markerLine(it.Body, "Depends on:"); ok {
			for _, tok := range strings.Split(line, ",") {
				if "#"+strings.TrimPrefix(strings.TrimSpace(tok), "#") == ref {
					return &GroomingMutationResult{Skipped: true, SkipReason: "depends_on ref already present"}, nil
				}
			}
			it.Body = strings.Replace(it.Body, "Depends on: "+line, "Depends on: "+line+", "+ref, 1)
			break
		}
		it.Body += "\nDepends on: " + ref
	case GroomingKindBoardPlace, GroomingKindIcebox:
		it.OnBoard = true
		it.BoardColumn = req.After.Scalar
	case GroomingKindCloseDuplicate, GroomingKindCloseNotPlanned:
		it.State = "closed"
	}
	return &GroomingMutationResult{Applied: true, ProviderResponse: "ok"}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// bodyCarriesMarker mirrors the PROVIDERS' `(?im)^<marker>` regexes
// (parentEpicMarkerRE / dependsOnMarkerRE) — case-insensitive, line-anchored —
// deliberately as an INDEPENDENT implementation rather than by calling the
// helper under test, so the fake's "already present" arm is not a tautology
// over groomingBodyMarkerValues.
func bodyCarriesMarker(body, marker string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// markerLine returns the FIRST marker line's value, case-insensitively — the
// fake's independent counterpart to the provider's dependsOnMarkerRE capture,
// so the fake's additive merge is not written in terms of the helper under
// test.
func markerLine(body, marker string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= len(marker) && strings.EqualFold(trimmed[:len(marker)], marker) {
			return strings.TrimSpace(trimmed[len(marker):]), true
		}
	}
	return "", false
}

// countMarkerLines counts the body lines opening with marker, case-insensitively.
func countMarkerLines(body, marker string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), strings.ToLower(marker)) {
			n++
		}
	}
	return n
}

func recordFor(t *testing.T, res *GroomingApplyResult, entryID string) GroomingMutationRecord {
	t.Helper()
	for _, bucket := range [][]GroomingMutationRecord{res.Applied, res.Failed, res.Skipped, res.Refused} {
		for _, r := range bucket {
			if r.EntryID == entryID {
				return r
			}
		}
	}
	t.Fatalf("no record for entry %q in %+v", entryID, res)
	return GroomingMutationRecord{}
}

// ---------------------------------------------------------------------------
// Derivation
// ---------------------------------------------------------------------------

// TestDeriveGroomingMutations_MapsEveryEntryClass is the DERIVATION done-means:
// it asserts the SHIPPED mapping for each of the six report entry classes,
// including that a vision-drift flag derives NO mutation and that an
// unrecognized hygiene defect yields unmappable_defect rather than a guessed
// kind. A comment-only touch that left the mapping unwired fails here, where a
// scope-presence gate would pass.
func TestDeriveGroomingMutations_MapsEveryEntryClass(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{
			hygieneEntry(1, "missing_label_namespace", "area:api"),
			hygieneEntry(2, "unlinked_parent_epic", "#1437"),
			hygieneEntry(3, "missing_parent_epic_link", "#1437"),
			hygieneEntry(4, "unboarded", "Backlog"),
			hygieneEntry(5, "missing_estimate", "3"),
			hygieneEntry(6, "absent_done_means", "write one"),
			{ID: "hygiene:github/kuhlman-labs%2ffishhawk%237:invented", ItemRef: groomingRef(7), Defect: "invented", Detail: "d"},
			hygieneEntry(8, "missing_label_namespace", ""),
		},
		DependencyEdges:          []plan.DependencyEdge{dependencyEntry(10, 11)},
		Ordering:                 []plan.OrderingEntry{orderingEntry(12, 1)},
		Duplicates:               []plan.DuplicateCandidate{duplicateEntry(13, 14)},
		DecompositionSuggestions: []plan.DecompositionSuggestion{decompositionEntry(15)},
		VisionDrift:              []plan.VisionDriftFlag{visionDriftEntry(16)},
	}

	got := map[string]groomingCandidate{}
	for _, c := range deriveGroomingMutations(report) {
		got[c.entryID] = c
	}
	if len(got) != 13 {
		t.Fatalf("derived %d candidates, want one per report entry (13)", len(got))
	}

	type want struct {
		kind       GroomingMutationKind
		skipReason string
		after      string
		class      string
	}
	table := map[string]want{
		report.HygieneDefects[0].ID: {GroomingKindLabelSet, "", "", "hygiene"},
		report.HygieneDefects[1].ID: {GroomingKindEpicLink, "", "#1437", "hygiene"},
		report.HygieneDefects[2].ID: {GroomingKindEpicLink, "", "#1437", "hygiene"},
		// The board state is derived in the CANONICAL vocabulary; the provider
		// option ("Backlog") is resolved later, at request-resolution time.
		report.HygieneDefects[3].ID:           {GroomingKindBoardPlace, "", "backlog", "hygiene"},
		report.HygieneDefects[4].ID:           {GroomingKindFieldSet, "", "3", "hygiene"},
		report.HygieneDefects[5].ID:           {"", GroomingSkipUnmappableDefect, "", "hygiene"},
		report.HygieneDefects[6].ID:           {"", GroomingSkipUnmappableDefect, "", "hygiene"},
		report.HygieneDefects[7].ID:           {GroomingKindLabelSet, GroomingSkipNoStructuredFix, "", "hygiene"},
		report.DependencyEdges[0].ID:          {GroomingKindDependsOnAdd, "", "", "hygiene"},
		report.Ordering[0].ID:                 {GroomingKindRankSet, "", "1", "ordering"},
		report.Duplicates[0].ID:               {GroomingKindCloseDuplicate, "", "closed", "dedup"},
		report.DecompositionSuggestions[0].ID: {GroomingKindIcebox, "", "", "scoping"},
		report.VisionDrift[0].ID:              {"", GroomingSkipFindingOnly, "", "scoping"},
	}
	for id, w := range table {
		c, ok := got[id]
		if !ok {
			t.Errorf("no candidate derived for %q", id)
			continue
		}
		if c.kind != w.kind {
			t.Errorf("%s: kind = %q, want %q", id, c.kind, w.kind)
		}
		if c.skipReason != w.skipReason {
			t.Errorf("%s: skipReason = %q, want %q", id, c.skipReason, w.skipReason)
		}
		if c.after.Scalar != w.after {
			t.Errorf("%s: after.Scalar = %q, want %q", id, c.after.Scalar, w.after)
		}
		if c.class != w.class {
			t.Errorf("%s: action class = %q, want %q", id, c.class, w.class)
		}
	}
	// The label mutation carries its value as a LIST, not a scalar.
	if lc := got[report.HygieneDefects[0].ID]; len(lc.after.List) != 1 || lc.after.List[0] != "area:api" {
		t.Errorf("label candidate after = %+v, want the suggested label as a one-element list", lc.after)
	}
	// The icebox candidate is routed through the placement guard with the
	// unstarted source set (approval condition I2).
	if ic := got[report.DecompositionSuggestions[0].ID]; len(ic.expectedFrom) != 2 {
		t.Errorf("icebox candidate expectedFrom = %v, want the unstarted source set", ic.expectedFrom)
	}
	// No v0 report entry derives a close-as-not-planned or a priority field
	// write: both are vocabulary members with no derivation source, and
	// inventing one from a report that carries no such signal is exactly the
	// guess this layer refuses.
	for id, c := range got {
		if c.kind == GroomingKindCloseNotPlanned || c.kind == GroomingKindPrioritySet {
			t.Errorf("%s derived %q, but no v0 report entry class carries that signal", id, c.kind)
		}
	}
}

// TestDeriveGroomingMutations_IsDeterministic pins the fixed dispatch order:
// class order, then entry id. A map-iteration-ordered apply would produce a
// different audit trail on every run.
func TestDeriveGroomingMutations_IsDeterministic(t *testing.T) {
	report := &plan.GroomingReport{
		VisionDrift:              []plan.VisionDriftFlag{visionDriftEntry(9)},
		DecompositionSuggestions: []plan.DecompositionSuggestion{decompositionEntry(8)},
		Ordering:                 []plan.OrderingEntry{orderingEntry(7, 1), orderingEntry(6, 2)},
		HygieneDefects:           []plan.HygieneDefect{hygieneEntry(5, "unboarded", "Backlog")},
	}
	var first []string
	for _, c := range deriveGroomingMutations(report) {
		first = append(first, c.entryID)
	}
	for i := 0; i < 5; i++ {
		var again []string
		for _, c := range deriveGroomingMutations(report) {
			again = append(again, c.entryID)
		}
		if strings.Join(again, ",") != strings.Join(first, ",") {
			t.Fatalf("order drifted: %v then %v", first, again)
		}
	}
	if first[0] != report.HygieneDefects[0].ID {
		t.Errorf("first candidate = %q, want the hygiene entry (class order)", first[0])
	}
	if first[len(first)-1] != report.VisionDrift[0].ID {
		t.Errorf("last candidate = %q, want the vision-drift entry (class order)", first[len(first)-1])
	}
}

// ---------------------------------------------------------------------------
// AC1 — the join control
// ---------------------------------------------------------------------------

// TestApplyGrooming_DecisionForUnknownEntryIsRefused is AC1: a decision naming
// an entry that is not in the report REFUSES the whole apply. It asserts BOTH
// the typed error naming the entry id AND that the fake mutator recorded ZERO
// dispatches — the committed-state read, since a refusal that happened and a
// no-op that happened return the same nil-result envelope.
func TestApplyGrooming_DecisionForUnknownEntryIsRefused(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	mut := &fakeGroomingMutator{}
	sink := &fakeGroomingSink{}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, sink, GroomingApplyRequest{
		Target: groomingTarget(),
		Report: report,
		Decisions: []GroomingDecision{
			{EntryID: report.HygieneDefects[0].ID, Verdict: GroomingApproved},
			{EntryID: "hygiene:github/somewhere%23999:unboarded", Verdict: GroomingApproved},
		},
		Modes:  map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States: groomingStates(),
	})
	var je *GroomingJoinError
	if !errors.As(err, &je) {
		t.Fatalf("err = %v (%T), want *GroomingJoinError", err, err)
	}
	if je.EntryID != "hygiene:github/somewhere%23999:unboarded" {
		t.Errorf("join error names %q, want the unjoined entry id", je.EntryID)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil: an unjoined apply runs nothing", res)
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator recorded %d dispatches, want 0 — the join refusal must precede every dispatch: %v", len(mut.calls), mut.kinds())
	}
	if len(sink.records) != 0 {
		t.Errorf("sink recorded %d rows, want 0", len(sink.records))
	}
}

// TestApplyGrooming_ContradictoryDuplicateDecisionsAreRefused is the
// duplicate-decision fail-closed guard (#2237 review).
//
// Decisions are indexed by entry id and a map insert is last-write-wins, so
// WITHOUT the guard a set carrying both {id, approved} and {id, rejected}
// authorizes or refuses purely by array order. The table sends BOTH orders for
// the same entry and requires the SAME answer from each: a *GroomingJoinError
// naming the entry and ZERO dispatches.
//
// The committed-state read is the mutator call log, not the error: an
// approved-last set that dispatched and a refusal that did not both return
// through the same envelope shape, and only the call log tells them apart.
// Order-independence is the discriminating property — an implementation with
// the guard removed passes the rejected-last rows (nothing dispatches because
// the surviving verdict is a rejection) and fails the approved-last one.
func TestApplyGrooming_ContradictoryDuplicateDecisionsAreRefused(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	id := report.HygieneDefects[0].ID

	cases := []struct {
		name      string
		decisions []GroomingDecision
	}{
		{
			name: "approved then rejected",
			decisions: []GroomingDecision{
				{EntryID: id, Verdict: GroomingApproved},
				{EntryID: id, Verdict: GroomingRejected},
			},
		},
		{
			name: "rejected then approved",
			decisions: []GroomingDecision{
				{EntryID: id, Verdict: GroomingRejected},
				{EntryID: id, Verdict: GroomingApproved},
			},
		},
		{
			// Not contradictory, still ambiguous: two rows where the caller
			// meant one. Refused rather than resolved, so the boundary has no
			// "harmless duplicate" arm an ambiguous set could slip through.
			name: "identical repeats",
			decisions: []GroomingDecision{
				{EntryID: id, Verdict: GroomingApproved},
				{EntryID: id, Verdict: GroomingApproved},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mut := &fakeGroomingMutator{}
			sink := &fakeGroomingSink{}
			res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, sink, GroomingApplyRequest{
				Target:    groomingTarget(),
				Report:    report,
				Decisions: tc.decisions,
				Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
				States:    groomingStates(),
			})
			// COMMITTED STATE FIRST, and never behind a t.Fatalf on the
			// error: with the guard deleted the "rejected then approved" row
			// returns a nil error AND dispatches, so the call log is the
			// assertion that has to be reached to see it.
			if len(mut.calls) != 0 {
				t.Errorf("mutator recorded %d dispatch(es) %v, want 0 — a repeated decision id must never authorize a mutation by array order",
					len(mut.calls), mut.kinds())
			}
			if len(sink.records) != 0 {
				t.Errorf("sink recorded %d rows, want 0 — nothing settled", len(sink.records))
			}
			if res != nil {
				t.Errorf("result = %+v, want nil: an ambiguous decision set runs nothing", res)
			}
			var je *GroomingJoinError
			if !errors.As(err, &je) {
				t.Fatalf("err = %v (%T), want *GroomingJoinError for a repeated decision id", err, err)
			}
			if je.EntryID != id {
				t.Errorf("join error names %q, want the repeated entry id %q", je.EntryID, id)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC2 — rejected / amended / undecided
// ---------------------------------------------------------------------------

// TestApplyGrooming_RejectedAndAmendedEntriesAreNotApplied is AC2. It asserts
// on the fake mutator's CALL LOG, not on a returned error: a control that
// fired and was then bypassed would return a byte-identical nil error, so only
// the committed state discriminates.
func TestApplyGrooming_RejectedAndAmendedEntriesAreNotApplied(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{
			hygieneEntry(1, "missing_label_namespace", "area:api"),   // approved
			hygieneEntry(2, "missing_label_namespace", "area:cli"),   // rejected
			hygieneEntry(3, "missing_label_namespace", "area:agent"), // amended
			hygieneEntry(4, "missing_label_namespace", "area:docs"),  // no decision
		},
	}
	mut := &fakeGroomingMutator{}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, &fakeGroomingSink{}, GroomingApplyRequest{
		Target: groomingTarget(),
		Report: report,
		Decisions: []GroomingDecision{
			{EntryID: report.HygieneDefects[0].ID, Verdict: GroomingApproved},
			{EntryID: report.HygieneDefects[1].ID, Verdict: GroomingRejected},
			{EntryID: report.HygieneDefects[2].ID, Verdict: GroomingAmended},
		},
		Modes:  map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States: groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("mutator recorded %d dispatches, want exactly the approved entry's: %+v", len(mut.calls), mut.calls)
	}
	if mut.calls[0].EntryID != report.HygieneDefects[0].ID {
		t.Errorf("dispatched entry = %q, want the approved one", mut.calls[0].EntryID)
	}
	for i, want := range []string{GroomingSkipNotApproved, GroomingSkipAmended, GroomingSkipNoDecision} {
		rec := recordFor(t, res, report.HygieneDefects[i+1].ID)
		if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != want {
			t.Errorf("entry %d: outcome=%q reason=%q, want skipped/%s", i+1, rec.Outcome, rec.SkipReason, want)
		}
	}
}

// TestApplyGrooming_UnrecognizedVerdictIsNotAnApproval pins the fail-closed
// arm of the verdict switch: a verdict outside the closed set is NOT treated
// as an approval.
func TestApplyGrooming_UnrecognizedVerdictIsNotAnApproval(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	mut := &fakeGroomingMutator{}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: []GroomingDecision{{EntryID: report.HygieneDefects[0].ID, Verdict: "looks-fine"}},
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator recorded %d dispatches, want 0 for an unrecognized verdict", len(mut.calls))
	}
	if rec := recordFor(t, res, report.HygieneDefects[0].ID); rec.SkipReason != GroomingSkipNotApproved {
		t.Errorf("skip reason = %q, want %q", rec.SkipReason, GroomingSkipNotApproved)
	}
}

// ---------------------------------------------------------------------------
// AC3 — audit
// ---------------------------------------------------------------------------

// TestApplyGrooming_EveryMutationIsAudited is AC3: one audit record per
// candidate — applied, failed AND skipped alike — each carrying the join key,
// before/after and the provider response, plus the once-per-apply summary.
func TestApplyGrooming_EveryMutationIsAudited(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{
			hygieneEntry(1, "missing_label_namespace", "area:api"), // applied
			hygieneEntry(2, "missing_label_namespace", "area:cli"), // failed
			hygieneEntry(3, "missing_label_namespace", "area:doc"), // skipped (rejected)
		},
		VisionDrift:              []plan.VisionDriftFlag{visionDriftEntry(4)},           // skipped (finding)
		DecompositionSuggestions: []plan.DecompositionSuggestion{decompositionEntry(5)}, // skipped (report mode)
	}
	mut := &fakeGroomingMutator{errOn: func(req GroomingMutationRequest) error {
		if req.EntryID == report.HygieneDefects[1].ID {
			return errors.New("forge said no")
		}
		return nil
	}}
	sink := &fakeGroomingSink{}
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		"#5": {Number: 5, State: "open", OnBoard: true, BoardColumn: "Backlog"},
	}}
	res, err := ApplyGrooming(context.Background(), mut, reader, sink, GroomingApplyRequest{
		Target: groomingTarget(),
		Report: report,
		Decisions: []GroomingDecision{
			{EntryID: report.HygieneDefects[0].ID, Verdict: GroomingApproved},
			{EntryID: report.HygieneDefects[1].ID, Verdict: GroomingApproved},
			{EntryID: report.HygieneDefects[2].ID, Verdict: GroomingRejected},
			{EntryID: report.VisionDrift[0].ID, Verdict: GroomingApproved},
			{EntryID: report.DecompositionSuggestions[0].ID, Verdict: GroomingApproved},
		},
		Modes:        map[string]GroomingMode{"hygiene": GroomingModeAuto, "scoping": GroomingModeReport},
		GateApproved: map[string]bool{report.DecompositionSuggestions[0].ID: true},
		States:       groomingStates(),
		IceboxColumn: "Icebox",
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(sink.records) != 5 {
		t.Fatalf("sink got %d records, want one per candidate (5): %+v", len(sink.records), sink.records)
	}
	byID := map[string]GroomingMutationRecord{}
	for _, r := range sink.records {
		byID[r.EntryID] = r
	}
	applied := byID[report.HygieneDefects[0].ID]
	if applied.Outcome != GroomingOutcomeApplied || applied.ProviderResponse == "" {
		t.Errorf("applied record = %+v, want outcome applied with a provider response", applied)
	}
	if len(applied.After.List) != 1 || applied.After.List[0] != "area:api" {
		t.Errorf("applied record After = %+v, want the proposed label", applied.After)
	}
	if failed := byID[report.HygieneDefects[1].ID]; failed.Outcome != GroomingOutcomeFailed || !strings.Contains(failed.Error, "forge said no") {
		t.Errorf("failed record = %+v, want outcome failed carrying the provider error", failed)
	}
	if skipped := byID[report.HygieneDefects[2].ID]; skipped.Outcome != GroomingOutcomeSkipped {
		t.Errorf("rejected record = %+v, want outcome skipped", skipped)
	}
	if finding := byID[report.VisionDrift[0].ID]; finding.SkipReason != GroomingSkipFindingOnly {
		t.Errorf("vision-drift record = %+v, want finding_only", finding)
	}
	// The AUDITED report-mode row carries BOTH sides. AC3 says every candidate
	// produces a record; a record that surfaces a proposal with no current
	// value is not an account of what would change (#2237 review). The card
	// sits at Backlog and the proposal is Icebox, so the two are
	// distinguishable — and the entry is ALSO gate-approved, so this row is
	// simultaneously the I1 report-mode-beats-gate-approval case.
	surfaced := byID[report.DecompositionSuggestions[0].ID]
	if surfaced.SkipReason != GroomingSkipReportMode {
		t.Errorf("report-mode record = %+v, want skip reason %q", surfaced, GroomingSkipReportMode)
	}
	if surfaced.Before.Scalar != "Backlog" {
		t.Errorf("report-mode record Before = %+v, want the card's CURRENT column Backlog", surfaced.Before)
	}
	if surfaced.After.Scalar != "Icebox" {
		t.Errorf("report-mode record After = %+v, want the proposed column Icebox", surfaced.After)
	}
	if surfaced.ItemRef != "#5" {
		t.Errorf("report-mode record ItemRef = %q, want #5", surfaced.ItemRef)
	}
	if len(sink.summaries) != 1 {
		t.Fatalf("sink got %d summaries, want exactly 1", len(sink.summaries))
	}
	sum := sink.summaries[0]
	if sum.Applied != 1 || sum.Failed != 1 || sum.Skipped != 3 {
		t.Errorf("summary = %+v, want 1 applied / 1 failed / 3 skipped", sum)
	}
	if res.Summary.Applied != 1 || len(res.Applied) != 1 || len(res.Failed) != 1 || len(res.Skipped) != 3 {
		t.Errorf("result buckets = %+v, want them to mirror the summary", res.Summary)
	}
}

// TestApplyGrooming_AuditSinkErrorSurfacesAfterLoop pins the sink-error
// branch: the failure surfaces as a typed *GroomingAuditError AFTER the loop,
// and the remaining candidates STILL dispatched — continue-and-report is
// preserved and nothing is silently unaudited.
func TestApplyGrooming_AuditSinkErrorSurfacesAfterLoop(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{
			hygieneEntry(1, "missing_label_namespace", "area:api"),
			hygieneEntry(2, "missing_label_namespace", "area:cli"),
		},
	}
	mut := &fakeGroomingMutator{}
	sink := &fakeGroomingSink{failEntry: report.HygieneDefects[0].ID}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, sink, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	var ae *GroomingAuditError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want *GroomingAuditError", err, err)
	}
	if len(ae.Errors) != 1 || !strings.Contains(ae.Errors[0], report.HygieneDefects[0].ID) {
		t.Errorf("audit errors = %v, want one naming the failing entry", ae.Errors)
	}
	if res == nil {
		t.Fatal("result = nil, want the completed apply alongside the audit error")
	}
	if len(mut.calls) != 2 {
		t.Errorf("mutator recorded %d dispatches, want 2 — an audit failure must not abort the loop", len(mut.calls))
	}
	if len(res.Summary.AuditErrors) != 1 {
		t.Errorf("summary AuditErrors = %v, want the collected failure", res.Summary.AuditErrors)
	}
}

// TestApplyGrooming_SummarySinkErrorSurfaces pins the SECOND sink call's error
// branch, which the per-record test does not reach.
func TestApplyGrooming_SummarySinkErrorSurfaces(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	sink := &fakeGroomingSink{summaryErr: errors.New("summary sink down")}
	res, err := ApplyGrooming(context.Background(), &fakeGroomingMutator{}, &fakeGroomingReader{}, sink, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	var ae *GroomingAuditError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want *GroomingAuditError", err, err)
	}
	if len(res.Applied) != 1 {
		t.Errorf("applied = %d, want the mutation to still have landed", len(res.Applied))
	}
	if len(res.Summary.AuditErrors) != 1 || !strings.Contains(res.Summary.AuditErrors[0], "summary sink down") {
		t.Errorf("summary AuditErrors = %v, want the summary sink failure", res.Summary.AuditErrors)
	}
}

// ---------------------------------------------------------------------------
// AC4 — continue and report
// ---------------------------------------------------------------------------

// TestApplyGrooming_ContinuesPastAMidRunProviderFailure is AC4: a provider
// error on the third of five candidates settles all five, and every one is
// audited.
func TestApplyGrooming_ContinuesPastAMidRunProviderFailure(t *testing.T) {
	report := &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{
		hygieneEntry(1, "missing_label_namespace", "area:api"),
		hygieneEntry(2, "missing_label_namespace", "area:cli"),
		hygieneEntry(3, "missing_label_namespace", "area:doc"),
		hygieneEntry(4, "missing_label_namespace", "area:web"),
		hygieneEntry(5, "missing_label_namespace", "area:ops"),
	}}
	failing := report.HygieneDefects[2].ID
	mut := &fakeGroomingMutator{errOn: func(req GroomingMutationRequest) error {
		if req.EntryID == failing {
			return errors.New("rate limited")
		}
		return nil
	}}
	sink := &fakeGroomingSink{}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, sink, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 5 {
		t.Errorf("mutator saw %d dispatches, want all 5 — the loop must not abort", len(mut.calls))
	}
	if len(res.Applied) != 4 || len(res.Failed) != 1 {
		t.Fatalf("buckets = %d applied / %d failed, want 4/1", len(res.Applied), len(res.Failed))
	}
	if res.Failed[0].EntryID != failing || !strings.Contains(res.Failed[0].Error, "rate limited") {
		t.Errorf("failed record = %+v, want the third candidate carrying its error text", res.Failed[0])
	}
	if len(sink.records) != 5 {
		t.Errorf("sink got %d records, want one per candidate", len(sink.records))
	}
}

// TestApplyGrooming_ProviderResultBranches pins the two remaining provider
// outcomes: a provider-side SKIP is recorded as a skip carrying the provider's
// own reason, and a provider that returns neither result nor error is recorded
// FAILED rather than fabricated as applied.
func TestApplyGrooming_ProviderResultBranches(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	req := GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	}

	skipMut := &fakeGroomingMutator{result: &GroomingMutationResult{Skipped: true, SkipReason: "provider_saw_it_already", ProviderResponse: "no-op"}}
	res, err := ApplyGrooming(context.Background(), skipMut, &fakeGroomingReader{}, &fakeGroomingSink{}, req)
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if rec := recordFor(t, res, report.HygieneDefects[0].ID); rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != "provider_saw_it_already" {
		t.Errorf("record = %+v, want the provider's skip reason preserved", rec)
	}

	nilMut := &nilResultMutator{}
	res, err = ApplyGrooming(context.Background(), nilMut, &fakeGroomingReader{}, &fakeGroomingSink{}, req)
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	rec := recordFor(t, res, report.HygieneDefects[0].ID)
	if rec.Outcome != GroomingOutcomeFailed || !strings.Contains(rec.Error, "no result") {
		t.Errorf("record = %+v, want failed on a result-less provider reply", rec)
	}
}

// TestApplyGrooming_MalformedProviderResultIsFailedNotFabricated pins the
// result-state VALIDATION (#2237 review). GroomingMutationResult's contract is
// that exactly one of Applied/Skipped is true; the two violations were each
// silently absorbed into a load-bearing audit outcome:
//
//   - BOTH FALSE — a zero-value result — fell through to the switch's default
//     arm and was recorded APPLIED, fabricating a tracker write that provably
//     did not happen. This is the row a provider returning `&Result{}` on an
//     unhandled path produces.
//   - BOTH TRUE was recorded SKIPPED, hiding a provider that believes it
//     wrote.
//
// Both must now be FAILED. The assertions read the recorded OUTCOME (and the
// applied/skipped buckets), not an error identity: the call returns nil either
// way, so only the settled record discriminates.
func TestApplyGrooming_MalformedProviderResultIsFailedNotFabricated(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	id := report.HygieneDefects[0].ID

	cases := []struct {
		name   string
		result *GroomingMutationResult
	}{
		{name: "zero value: neither applied nor skipped", result: &GroomingMutationResult{}},
		{
			name:   "contradictory: applied AND skipped",
			result: &GroomingMutationResult{Applied: true, Skipped: true, SkipReason: "both", ProviderResponse: "confused"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mut := &fakeGroomingMutator{result: tc.result}
			sink := &fakeGroomingSink{}
			res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, sink, GroomingApplyRequest{
				Target:    groomingTarget(),
				Report:    report,
				Decisions: approveAll(report),
				Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
				States:    groomingStates(),
			})
			if err != nil {
				t.Fatalf("ApplyGrooming: %v", err)
			}
			rec := recordFor(t, res, id)
			if rec.Outcome != GroomingOutcomeFailed {
				t.Errorf("outcome = %q, want %q — a malformed result must not be classified as a write or a deliberate no-op: %+v",
					rec.Outcome, GroomingOutcomeFailed, rec)
			}
			if !strings.Contains(rec.Error, "malformed result") {
				t.Errorf("error = %q, want it to name the malformed result state", rec.Error)
			}
			if len(res.Applied) != 0 {
				t.Errorf("applied bucket = %+v, want empty: nothing was written", res.Applied)
			}
			if res.Summary.Failed != 1 || res.Summary.Applied != 0 || res.Summary.Skipped != 0 {
				t.Errorf("summary = %+v, want failed=1 applied=0 skipped=0", res.Summary)
			}
			// The audit row the operator reads must agree with the verdict.
			if len(sink.records) != 1 || sink.records[0].Outcome != GroomingOutcomeFailed {
				t.Errorf("audited records = %+v, want one failed row", sink.records)
			}
		})
	}
}

type nilResultMutator struct{}

func (nilResultMutator) ApplyGroomingMutation(context.Context, GroomingMutationRequest) (*GroomingMutationResult, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// AC5 + approval condition I1 — destructive authorization vs report mode
// ---------------------------------------------------------------------------

// TestApplyGrooming_DestructiveKindRequiresAutoOrGateApproval is AC5 plus
// approval condition I1. It asserts on the fake mutator's CLOSE-CALL LOG (the
// committed-state read: a refusal that is later rolled back returns the same
// nil error) for both destructive kinds — close_duplicate AND icebox.
//
// The mode:report + GateApproved=true rows are I1's teeth: report mode
// prohibits EVERY mutation, and it is checked BEFORE the gate-approval arm, so
// a gate approval cannot pull a report-mode class into acting. Those are the
// rows an implementation that checked gate approval first would fail.
func TestApplyGrooming_DestructiveKindRequiresAutoOrGateApproval(t *testing.T) {
	cases := []struct {
		name         string
		mode         GroomingMode
		modeAbsent   bool
		gateApproved bool
		wantDispatch bool
		wantSkip     string
	}{
		{name: "auto+approved", mode: GroomingModeAuto, wantDispatch: true},
		{name: "gated+approved", mode: GroomingModeGated, wantSkip: GroomingSkipDestructiveNotAuthorized},
		{name: "gated+gate-approved", mode: GroomingModeGated, gateApproved: true, wantDispatch: true},
		{name: "report+approved", mode: GroomingModeReport, wantSkip: GroomingSkipReportMode},
		{name: "report+gate-approved", mode: GroomingModeReport, gateApproved: true, wantSkip: GroomingSkipReportMode},
		{name: "absent-class+approved", modeAbsent: true, wantSkip: GroomingSkipDestructiveNotAuthorized},
		{name: "absent-class+gate-approved", modeAbsent: true, gateApproved: true, wantDispatch: true},
		{name: "unknown-mode+approved", mode: "delegated", wantSkip: GroomingSkipDestructiveNotAuthorized},
	}

	for _, kind := range []GroomingMutationKind{GroomingKindCloseDuplicate, GroomingKindIcebox} {
		for _, tc := range cases {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				var report *plan.GroomingReport
				var entryID, class string
				decisions := []GroomingDecision{}
				if kind == GroomingKindCloseDuplicate {
					dup := duplicateEntry(1, 2)
					report = &plan.GroomingReport{Duplicates: []plan.DuplicateCandidate{dup}}
					entryID, class = dup.ID, "dedup"
					decisions = append(decisions, GroomingDecision{
						EntryID: entryID, Verdict: GroomingApproved, CloseTarget: dup.Pair[1].ID,
					})
				} else {
					dec := decompositionEntry(1)
					report = &plan.GroomingReport{DecompositionSuggestions: []plan.DecompositionSuggestion{dec}}
					entryID, class = dec.ID, "scoping"
					decisions = append(decisions, GroomingDecision{EntryID: entryID, Verdict: GroomingApproved})
				}

				modes := map[string]GroomingMode{}
				if !tc.modeAbsent {
					modes[class] = tc.mode
				}
				gate := map[string]bool{}
				if tc.gateApproved {
					gate[entryID] = true
				}
				mut := &fakeGroomingMutator{}
				reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
					"#1": {Number: 1, State: "open", OnBoard: true, BoardColumn: "Backlog"},
					"#2": {Number: 2, State: "open", OnBoard: true, BoardColumn: "Backlog"},
				}}
				res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
					Target:       groomingTarget(),
					Report:       report,
					Decisions:    decisions,
					Modes:        modes,
					GateApproved: gate,
					States:       groomingStates(),
					IceboxColumn: "Icebox",
				})
				if err != nil {
					t.Fatalf("ApplyGrooming: %v", err)
				}
				got := mut.destructiveCalls()
				if tc.wantDispatch {
					if len(got) != 1 {
						t.Fatalf("destructive call log = %+v, want exactly one %s dispatch", got, kind)
					}
					if rec := recordFor(t, res, entryID); rec.Outcome != GroomingOutcomeApplied {
						t.Errorf("record = %+v, want applied", rec)
					}
					return
				}
				if len(got) != 0 {
					t.Fatalf("destructive call log = %+v, want ZERO %s dispatches", got, kind)
				}
				rec := recordFor(t, res, entryID)
				if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != tc.wantSkip {
					t.Errorf("record outcome=%q reason=%q, want skipped/%s", rec.Outcome, rec.SkipReason, tc.wantSkip)
				}
			})
		}
	}
}

// TestApplyGrooming_ReportModeSurfacesAndDoesNotAct is the inherited #2236 AC7
// requirement asserted against the REAL apply path: a report-mode class
// produces a SKIPPED record whose proposal is still visible (before/after
// populated), the mutator records ZERO dispatches for it, and ApplyGrooming
// RETURNS — proving it did not park awaiting a decision it will never receive.
func TestApplyGrooming_ReportModeSurfacesAndDoesNotAct(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	mut := &fakeGroomingMutator{}
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		"#1": {Number: 1, State: "open", Labels: []string{"type:bug"}},
	}}
	done := make(chan struct{})
	var res *GroomingApplyResult
	var err error
	go func() {
		defer close(done)
		res, err = ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
			Target:    groomingTarget(),
			Report:    report,
			Decisions: approveAll(report),
			Modes:     map[string]GroomingMode{"hygiene": GroomingModeReport},
			States:    groomingStates(),
		})
	}()
	<-done // ApplyGrooming has no awaiting-decision return path; it must settle.
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator recorded %d dispatches, want 0 under report mode: %v", len(mut.calls), mut.kinds())
	}
	rec := recordFor(t, res, report.HygieneDefects[0].ID)
	if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != GroomingSkipReportMode {
		t.Fatalf("record = %+v, want skipped/%s", rec, GroomingSkipReportMode)
	}
	if len(rec.After.List) != 1 || rec.After.List[0] != "area:api" {
		t.Errorf("record After = %+v, want the proposal surfaced", rec.After)
	}
	if rec.ItemRef != "#1" {
		t.Errorf("record ItemRef = %q, want the resolved item so the proposal is actionable", rec.ItemRef)
	}
	// SURFACING IS BOTH SIDES. Before must carry the CURRENT value, read
	// through the reader on the real apply path — a proposal an operator
	// cannot diff is not a usable proposal (#2237 review; the requirement
	// this slice inherited from #2236's AC7). The reader is seeded with
	// type:bug and the proposal is area:api, so the two are distinguishable:
	// an implementation that returned before the read leaves Before EMPTY and
	// fails here.
	if len(rec.Before.List) != 1 || rec.Before.List[0] != "type:bug" {
		t.Errorf("record Before = %+v, want the CURRENT label set {type:bug} read on the report-mode path", rec.Before)
	}
	// Committed state, not just the record: the read happened and the write
	// did not.
	if len(reader.reads) != 1 || reader.reads[0].Ref != "#1" {
		t.Errorf("reader reads = %+v, want exactly one read of #1 to populate Before", reader.reads)
	}
	// The read is NOT an idempotence diff — the mode settled this candidate,
	// so the audit row must not claim a diff decided it.
	if rec.IdempotenceChecked {
		t.Error("IdempotenceChecked = true on a report-mode record; the MODE settled it, not a diff")
	}
}

// TestApplyGrooming_ReportModeSurfacesBeforeDespiteAReadFailure pins the
// degradation direction of that read: report mode dispatches nothing whatever
// the reader says, so a reader failure must NOT fail the candidate — it leaves
// Before empty and the proposal is still surfaced. Failing closed here would
// convert a harmless read outage into a lost proposal.
func TestApplyGrooming_ReportModeSurfacesBeforeDespiteAReadFailure(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	mut := &fakeGroomingMutator{}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{err: errors.New("reader down")},
		&fakeGroomingSink{}, GroomingApplyRequest{
			Target:    groomingTarget(),
			Report:    report,
			Decisions: approveAll(report),
			Modes:     map[string]GroomingMode{"hygiene": GroomingModeReport},
			States:    groomingStates(),
		})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	rec := recordFor(t, res, report.HygieneDefects[0].ID)
	if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != GroomingSkipReportMode {
		t.Fatalf("record = %+v, want skipped/%s — a read failure must not fail a report-mode candidate", rec, GroomingSkipReportMode)
	}
	if len(rec.Before.List) != 0 || rec.Before.Scalar != "" {
		t.Errorf("record Before = %+v, want empty when the read failed", rec.Before)
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator dispatched %d, want 0", len(mut.calls))
	}
}

// TestApplyGrooming_SurfacedRecordNamesItsSubjectWhenResolutionSkips is the
// surfaced-but-unresolvable combination (#2237 review): a report-mode
// candidate whose request resolution ALSO ends in a skip. The mode still
// prevents dispatch, but the surfaced row must carry whatever resolution did
// settle rather than dropping it — a proposal with no subject is not
// actionable.
//
// The case is an ICEBOX candidate under report mode with NO icebox column
// configured: the item ref resolves, the target column does not.
func TestApplyGrooming_SurfacedRecordNamesItsSubjectWhenResolutionSkips(t *testing.T) {
	ice := decompositionEntry(8)
	report := &plan.GroomingReport{DecompositionSuggestions: []plan.DecompositionSuggestion{ice}}
	mut := &fakeGroomingMutator{}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, &fakeGroomingSink{},
		GroomingApplyRequest{
			Target:    groomingTarget(),
			Report:    report,
			Decisions: approveAll(report),
			Modes:     map[string]GroomingMode{"scoping": GroomingModeReport},
			States:    groomingStates(),
			// IceboxColumn deliberately absent: resolution skips.
		})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	rec := recordFor(t, res, ice.ID)
	if rec.SkipReason != GroomingSkipReportMode {
		t.Fatalf("skip reason = %q, want %q — report mode outranks the resolution skip", rec.SkipReason, GroomingSkipReportMode)
	}
	if rec.ItemRef != "#8" {
		t.Errorf("record ItemRef = %q, want the resolved subject #8 even though resolution ended in a skip", rec.ItemRef)
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator dispatched %d, want 0", len(mut.calls))
	}
}

// TestApplyGrooming_RefusedRecordNamesItsSubjectToo is the same population
// rule on the NON-report path: an icebox candidate refused for a missing
// column is audited with its subject, so the operator reading the audit row
// knows which item was not parked.
func TestApplyGrooming_RefusedRecordNamesItsSubjectToo(t *testing.T) {
	ice := decompositionEntry(8)
	report := &plan.GroomingReport{DecompositionSuggestions: []plan.DecompositionSuggestion{ice}}
	mut := &fakeGroomingMutator{}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, &fakeGroomingSink{},
		GroomingApplyRequest{
			Target:       groomingTarget(),
			Report:       report,
			Decisions:    approveAll(report),
			Modes:        map[string]GroomingMode{"scoping": GroomingModeAuto},
			States:       groomingStates(),
			GateApproved: map[string]bool{ice.ID: true},
		})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	rec := recordFor(t, res, ice.ID)
	if rec.SkipReason != GroomingSkipIceboxColumnUnavailable {
		t.Fatalf("skip reason = %q, want %q", rec.SkipReason, GroomingSkipIceboxColumnUnavailable)
	}
	if rec.ItemRef != "#8" {
		t.Errorf("record ItemRef = %q, want the resolved subject #8 on a refused candidate", rec.ItemRef)
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator dispatched %d, want 0", len(mut.calls))
	}
}

// ---------------------------------------------------------------------------
// AC6 — never override a human's placement
// ---------------------------------------------------------------------------

// TestApplyGrooming_ManualBoardPlacementIsPreserved is AC6, over BOTH
// board-placement kinds — board_place AND icebox (approval condition I2).
//
// Each kind is asserted in a PAIR: an out-of-set board state yields a
// manual_placement_preserved SKIP with zero dispatches (tried and was
// REFUSED — the record names the refusal, which a bare "the card did not
// move" assertion could not distinguish from never having tried), and an
// in-set state DOES dispatch (so the guard is not over-broad).
func TestApplyGrooming_ManualBoardPlacementIsPreserved(t *testing.T) {
	cases := []struct {
		name         string
		icebox       bool
		item         *WorkItemRecord
		wantDispatch bool
	}{
		{
			name: "board_place/human already boarded it",
			item: &WorkItemRecord{Number: 1, State: "open", OnBoard: true, BoardColumn: "In Progress"},
		},
		{
			name:         "board_place/genuinely off board",
			item:         &WorkItemRecord{Number: 1, State: "open", OnBoard: false},
			wantDispatch: true,
		},
		{
			name:   "icebox/human moved it to In Progress",
			icebox: true,
			item:   &WorkItemRecord{Number: 1, State: "open", OnBoard: true, BoardColumn: "In Progress"},
		},
		{
			name:         "icebox/still in Backlog",
			icebox:       true,
			item:         &WorkItemRecord{Number: 1, State: "open", OnBoard: true, BoardColumn: "Backlog"},
			wantDispatch: true,
		},
		{
			name:   "icebox/off board entirely",
			icebox: true,
			item:   &WorkItemRecord{Number: 1, State: "open", OnBoard: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var report *plan.GroomingReport
			var entryID, class string
			gate := map[string]bool{}
			if tc.icebox {
				dec := decompositionEntry(1)
				report = &plan.GroomingReport{DecompositionSuggestions: []plan.DecompositionSuggestion{dec}}
				entryID, class = dec.ID, "scoping"
				gate[entryID] = true // destructive: authorized, so AC6 is what decides
			} else {
				h := hygieneEntry(1, "unboarded", "Backlog")
				report = &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{h}}
				entryID, class = h.ID, "hygiene"
			}
			mut := &fakeGroomingMutator{}
			reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{"#1": tc.item}}
			res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
				Target:       groomingTarget(),
				Report:       report,
				Decisions:    []GroomingDecision{{EntryID: entryID, Verdict: GroomingApproved}},
				Modes:        map[string]GroomingMode{class: GroomingModeGated},
				GateApproved: gate,
				States:       groomingStates(),
				IceboxColumn: "Icebox",
			})
			if err != nil {
				t.Fatalf("ApplyGrooming: %v", err)
			}
			rec := recordFor(t, res, entryID)
			if tc.wantDispatch {
				if len(mut.calls) != 1 {
					t.Fatalf("mutator call log = %+v, want one dispatch (the guard must not be over-broad)", mut.calls)
				}
				if rec.Outcome != GroomingOutcomeApplied {
					t.Errorf("record = %+v, want applied", rec)
				}
				return
			}
			if len(mut.calls) != 0 {
				t.Fatalf("mutator call log = %+v, want ZERO dispatches", mut.calls)
			}
			// TRIED AND WAS REFUSED, not did-not-try: the record names the
			// guard, and the reader was consulted. The outcome is REFUSED
			// rather than skipped since #2860 — a declined write, not an
			// already-satisfied no-op — and SkipReason must stay EMPTY so the
			// two audit facts cannot be conflated.
			if rec.Outcome != GroomingOutcomeRefused || rec.RefuseReason != GroomingSkipManualPlacementPreserved {
				t.Errorf("record outcome=%q refuse_reason=%q, want refused/%s", rec.Outcome, rec.RefuseReason, GroomingSkipManualPlacementPreserved)
			}
			if rec.SkipReason != "" {
				t.Errorf("record SkipReason = %q, want empty — a refusal is not a skip", rec.SkipReason)
			}
			if len(reader.reads) != 1 || !reader.reads[0].ResolveBoardState {
				t.Errorf("reader saw %+v, want one board-state-resolving read (proof the candidate was evaluated, not dropped earlier)", reader.reads)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC7 — idempotence
// ---------------------------------------------------------------------------

// TestApplyGrooming_ReApplyingAnAppliedReportIsANoOp is AC7: the same report
// applied twice against a STATEFUL fake (whose reader reflects the first run's
// writes) dispatches every observable mutation once and ZERO the second time,
// with every candidate recorded already_applied.
//
// The corpus covers all five observable kinds INCLUDING icebox (approval
// condition I2) and close_duplicate.
func TestApplyGrooming_ReApplyingAnAppliedReportIsANoOp(t *testing.T) {
	label := hygieneEntry(1, "missing_label_namespace", "area:api")
	epic := hygieneEntry(2, "unlinked_parent_epic", "#1437")
	board := hygieneEntry(3, "unboarded", "Backlog")
	dep := dependencyEntry(4, 5)
	dup := duplicateEntry(6, 7)
	ice := decompositionEntry(8)
	report := &plan.GroomingReport{
		HygieneDefects:           []plan.HygieneDefect{label, epic, board},
		DependencyEdges:          []plan.DependencyEdge{dep},
		Duplicates:               []plan.DuplicateCandidate{dup},
		DecompositionSuggestions: []plan.DecompositionSuggestion{ice},
	}
	forge := &statefulGroomingForge{items: map[string]*WorkItemRecord{
		"#1": {Number: 1, State: "open", Labels: []string{"type:bug"}},
		"#2": {Number: 2, State: "open", Body: "Summary"},
		"#3": {Number: 3, State: "open", OnBoard: false},
		"#4": {Number: 4, State: "open", Body: "Summary"},
		"#7": {Number: 7, State: "open"},
		"#8": {Number: 8, State: "open", OnBoard: true, BoardColumn: "Backlog"},
	}}
	req := GroomingApplyRequest{
		Target: groomingTarget(),
		Report: report,
		Decisions: []GroomingDecision{
			{EntryID: label.ID, Verdict: GroomingApproved},
			{EntryID: epic.ID, Verdict: GroomingApproved},
			{EntryID: board.ID, Verdict: GroomingApproved},
			{EntryID: dep.ID, Verdict: GroomingApproved},
			{EntryID: dup.ID, Verdict: GroomingApproved, CloseTarget: dup.Pair[1].ID},
			{EntryID: ice.ID, Verdict: GroomingApproved},
		},
		Modes:        map[string]GroomingMode{"hygiene": GroomingModeAuto, "dedup": GroomingModeGated, "scoping": GroomingModeGated},
		GateApproved: map[string]bool{dup.ID: true, ice.ID: true},
		States:       groomingStates(),
		IceboxColumn: "Icebox",
	}

	first, err := ApplyGrooming(context.Background(), forge, forge, &fakeGroomingSink{}, req)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if len(first.Applied) != 6 {
		t.Fatalf("first apply landed %d mutations, want 6: skipped=%+v failed=%+v", len(first.Applied), first.Skipped, first.Failed)
	}
	if len(forge.calls) != 6 {
		t.Fatalf("first apply dispatched %d, want 6", len(forge.calls))
	}

	forge.calls = nil
	second, err := ApplyGrooming(context.Background(), forge, forge, &fakeGroomingSink{}, req)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(forge.calls) != 0 {
		t.Fatalf("re-apply dispatched %d mutations, want ZERO: %+v", len(forge.calls), forge.calls)
	}
	if len(second.Skipped) != 6 {
		t.Fatalf("re-apply skipped %d, want all 6: %+v", len(second.Skipped), second)
	}
	for _, rec := range second.Skipped {
		if rec.SkipReason != GroomingSkipAlreadyApplied {
			t.Errorf("%s: skip reason = %q, want %q", rec.EntryID, rec.SkipReason, GroomingSkipAlreadyApplied)
		}
		if !rec.IdempotenceChecked {
			t.Errorf("%s: IdempotenceChecked = false, want the diff to have settled it", rec.EntryID)
		}
	}
}

// TestApplyGrooming_EpicLinkStructuralParentDecidesIdempotence is the #2952
// core done-means: epic_link idempotence is decided by the STRUCTURAL parent,
// not the body marker. A body-marked-but-structurally-unlinked item (#819/#821/
// #930) dispatches; a structurally-linked one is already_applied. The divergent
// `before` — structural parent empty, marker naming the ref — is reported so the
// divergence is visible in the audit rather than collapsed.
func TestApplyGrooming_EpicLinkStructuralParentDecidesIdempotence(t *testing.T) {
	epic := hygieneEntry(1, "unlinked_parent_epic", "#389")
	report := &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{epic}}

	// The #819 shape: the body carries `Parent epic: #389`, but there is NO
	// structural sub-issue edge (ParentRef empty). It must DISPATCH, not skip.
	t.Run("body marker without the structural edge dispatches", func(t *testing.T) {
		mut := &fakeGroomingMutator{}
		reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
			"#1": {Number: 1, State: "open", Body: "Summary\n\nParent epic: #389"},
		}}
		res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
			Target:    groomingTarget(),
			Report:    report,
			Decisions: approveAll(report),
			Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
			States:    groomingStates(),
		})
		if err != nil {
			t.Fatalf("ApplyGrooming: %v", err)
		}
		if len(mut.calls) != 1 {
			t.Fatalf("dispatched %d, want exactly ONE — a body marker without the structural edge is NOT already_applied", len(mut.calls))
		}
		rec := recordFor(t, res, epic.ID)
		if !rec.IdempotenceChecked {
			t.Error("IdempotenceChecked = false, want the structural diff to have run")
		}
		if rec.Before.Scalar != "" {
			t.Errorf("before.scalar = %q, want empty — there is no structural parent", rec.Before.Scalar)
		}
		if len(rec.Before.List) != 1 || rec.Before.List[0] != "#389" {
			t.Errorf("before.list = %v, want [#389] — the body marker, reported SEPARATELY from the structural parent", rec.Before.List)
		}
	})

	// True idempotence: the structural parent IS the proposed one -> zero dispatch.
	t.Run("structural parent already set is already_applied", func(t *testing.T) {
		mut := &fakeGroomingMutator{}
		reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
			"#1": {Number: 1, State: "open", Body: "Summary\n\nParent epic: #389", ParentRef: "#389", ParentResolved: true},
		}}
		res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
			Target:    groomingTarget(),
			Report:    report,
			Decisions: approveAll(report),
			Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
			States:    groomingStates(),
		})
		if err != nil {
			t.Fatalf("ApplyGrooming: %v", err)
		}
		if len(mut.calls) != 0 {
			t.Fatalf("dispatched %d, want ZERO — the structural parent already records the proposal: %+v", len(mut.calls), mut.calls)
		}
		rec := recordFor(t, res, epic.ID)
		if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != GroomingSkipAlreadyApplied {
			t.Errorf("record outcome=%q reason=%q, want skipped/%s", rec.Outcome, rec.SkipReason, GroomingSkipAlreadyApplied)
		}
		if rec.Before.Scalar != "#389" {
			t.Errorf("before.scalar = %q, want #389 — the structural parent is the authority", rec.Before.Scalar)
		}
	})
}

// TestApplyGrooming_EpicLinkReadRequestsTheStructuralParent proves the extra
// round trip is scoped to epic_link ONLY (#2952): the read for an epic_link
// candidate carries ResolveParent, and no other kind's read does.
func TestApplyGrooming_EpicLinkReadRequestsTheStructuralParent(t *testing.T) {
	epic := hygieneEntry(1, "unlinked_parent_epic", "#389")
	label := hygieneEntry(4, "missing_label_namespace", "area:api")
	board := hygieneEntry(5, "unboarded", "Backlog")
	dep := dependencyEntry(2, 3)
	report := &plan.GroomingReport{
		HygieneDefects:  []plan.HygieneDefect{epic, label, board},
		DependencyEdges: []plan.DependencyEdge{dep},
	}
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		"#1": {Number: 1, State: "open", Body: "Summary"},
		"#2": {Number: 2, State: "open", Body: "Summary"},
		"#4": {Number: 4, State: "open"},
		"#5": {Number: 5, State: "open", OnBoard: false},
	}}
	if _, err := ApplyGrooming(context.Background(), &fakeGroomingMutator{}, reader, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	}); err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	sawEpicRead := false
	for _, rd := range reader.reads {
		if rd.Ref == "#1" { // the epic_link candidate
			sawEpicRead = true
			if !rd.ResolveParent {
				t.Errorf("epic_link read %+v did NOT request the structural parent", rd)
			}
			continue
		}
		if rd.ResolveParent {
			t.Errorf("read for %q requested the structural parent; only epic_link should", rd.Ref)
		}
	}
	if !sawEpicRead {
		t.Errorf("the epic_link candidate was never read; reads = %+v", reader.reads)
	}
}

// TestApplyGrooming_DependsOnAddIdempotenceUnchanged is the CONDITION-3
// non-regression pin: splitting the shared epic_link/depends_on_add branch must
// leave depends_on_add's already_applied behaviour exactly as it was — it keys
// on BODY-MARKER membership (GitHub has no native depends_on edge), NOT on the
// structural parent. A body already recording the proposed ref is already_applied
// with ZERO dispatch; a body naming a DIFFERENT ref dispatches.
func TestApplyGrooming_DependsOnAddIdempotenceUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		to           int
		wantDispatch bool
	}{
		{"proposed ref already in the marker is already_applied", "Summary\n\nDepends on: #5", 5, false},
		{"proposed ref absent from the marker dispatches", "Summary\n\nDepends on: #5", 6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep := dependencyEntry(1, tc.to)
			report := &plan.GroomingReport{DependencyEdges: []plan.DependencyEdge{dep}}
			mut := &fakeGroomingMutator{}
			// ParentRef is deliberately UNSET: depends_on_add must not consult it.
			reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
				"#1": {Number: 1, State: "open", Body: tc.body},
			}}
			res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
				Target:    groomingTarget(),
				Report:    report,
				Decisions: approveAll(report),
				Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
				States:    groomingStates(),
			})
			if err != nil {
				t.Fatalf("ApplyGrooming: %v", err)
			}
			rec := recordFor(t, res, dep.ID)
			if tc.wantDispatch {
				if len(mut.calls) != 1 || rec.Outcome != GroomingOutcomeApplied {
					t.Fatalf("dispatched %d outcome=%q, want one dispatch / applied", len(mut.calls), rec.Outcome)
				}
				return
			}
			if len(mut.calls) != 0 {
				t.Fatalf("dispatched %d, want ZERO — the marker already records the ref", len(mut.calls))
			}
			if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != GroomingSkipAlreadyApplied {
				t.Errorf("outcome=%q reason=%q, want skipped/%s", rec.Outcome, rec.SkipReason, GroomingSkipAlreadyApplied)
			}
		})
	}
}

// TestApplyGrooming_MarkerRefShapeDoesNotDefeatIdempotence pins the other half
// of the epic-link idempotence fix (#2237 review): the write and the read must
// agree not only on WHERE the relationship is persisted but on its SHAPE.
//
// The provider normalizes the marker it stamps to `#N`, so an agent that
// suggested the parent as a bare `1437` produces an observed `#1437` against a
// proposed `1437`. A raw string compare treats those as different and
// re-dispatches a link that already exists — the same AC7 break, one layer up.
// The corpus pairs an already-marked body with a bare-ref proposal for BOTH
// body-marker kinds.
func TestApplyGrooming_MarkerRefShapeDoesNotDefeatIdempotence(t *testing.T) {
	epic := hygieneEntry(1, "unlinked_parent_epic", "1437") // bare, no '#'
	dep := dependencyEntry(2, 3)
	report := &plan.GroomingReport{
		HygieneDefects:  []plan.HygieneDefect{epic},
		DependencyEdges: []plan.DependencyEdge{dep},
	}
	mut := &fakeGroomingMutator{}
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		// epic_link now settles on the STRUCTURAL parent (#2952), so the
		// already-applied epic row carries ParentRef, not merely the marker. The
		// bare-ref proposal ("1437") must still match the normalized "#1437".
		"#1": {Number: 1, State: "open", Body: "Summary\n\nParent epic: #1437", ParentRef: "#1437", ParentResolved: true},
		// depends_on keeps its body-marker membership semantics UNCHANGED.
		"#2": {Number: 2, State: "open", Body: "Summary\n\nDepends on: #3"},
	}}
	res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Fatalf("dispatched %d mutations, want ZERO — both relationships are already recorded: %v", len(mut.calls), mut.kinds())
	}
	for _, id := range []string{epic.ID, dep.ID} {
		if rec := recordFor(t, res, id); rec.SkipReason != GroomingSkipAlreadyApplied {
			t.Errorf("%s: skip reason = %q, want %q", id, rec.SkipReason, GroomingSkipAlreadyApplied)
		}
	}
}

// TestApplyGrooming_MarkerObservationMatchesTheProviderParse closes the other
// half of the marker idempotence contract (#2237 fix-up): the read side must
// observe a marker the PROVIDER would consider present, not a narrower subset
// of one.
//
// The provider's regexes are `(?im)` (case-insensitive, line-anchored) and
// `renderDependsOnMarker` emits a COMMA-SEPARATED ref list, so an item filed
// with two dependencies carries `Depends on: #5, #6` on one line and a body
// may legitimately carry a lower-case or a second marker line. A read that
// looked only at the FIRST line, exact-case, comparing the WHOLE captured
// value, observes a non-match for every one of those and re-dispatches a
// relationship that is already recorded — the dispatch is then absorbed by the
// provider's own skip, so AC7's zero-dispatch re-apply guarantee quietly
// narrowed to single-ref, canonical-case bodies.
//
// The last two rows are the over-broadness control: a ref the marker does NOT
// name still dispatches, so membership did not become "any marker satisfies
// any proposal".
func TestApplyGrooming_MarkerObservationMatchesTheProviderParse(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		epicFix      string // non-empty selects the epic_link entry
		parentRef    string // structural parent seeded on #1 for the epic rows (#2952)
		depTo        int    // otherwise the depends_on edge target
		wantDispatch bool
	}{
		{
			name:  "depends_on/first ref of a comma-separated marker",
			body:  "Summary\n\nDepends on: #5, #6",
			depTo: 5,
		},
		{
			name:  "depends_on/second ref of a comma-separated marker",
			body:  "Summary\n\nDepends on: #5, #6",
			depTo: 6,
		},
		{
			name:  "depends_on/lower-case marker",
			body:  "Summary\n\ndepends on: #5",
			depTo: 5,
		},
		{
			name:  "depends_on/ref named by a SECOND marker line",
			body:  "Summary\n\nDepends on: #9\n\nDepends on: #5",
			depTo: 5,
		},
		{
			// epic_link now settles on the STRUCTURAL parent (#2952): a bare
			// proposed ref must match the normalized `#N` structural parent.
			name:      "epic_link/structural parent, bare proposed ref",
			body:      "Summary\n\nPARENT EPIC: #1437",
			epicFix:   "1437",
			parentRef: "#1437",
		},
		{
			name:         "depends_on/ref absent from the marker still dispatches",
			body:         "Summary\n\nDepends on: #5, #6",
			depTo:        7,
			wantDispatch: true,
		},
		{
			// The structural parent is a DIFFERENT epic than the proposal, so the
			// candidate dispatches even though the body marker names #1437.
			name:         "epic_link/different structural parent still dispatches",
			body:         "Summary\n\nParent epic: #1437",
			epicFix:      "1438",
			parentRef:    "#1437",
			wantDispatch: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var report *plan.GroomingReport
			var entryID string
			if tc.epicFix != "" {
				h := hygieneEntry(1, "unlinked_parent_epic", tc.epicFix)
				report = &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{h}}
				entryID = h.ID
			} else {
				d := dependencyEntry(1, tc.depTo)
				report = &plan.GroomingReport{DependencyEdges: []plan.DependencyEdge{d}}
				entryID = d.ID
			}
			mut := &fakeGroomingMutator{}
			reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
				"#1": {Number: 1, State: "open", Body: tc.body, ParentRef: tc.parentRef, ParentResolved: tc.parentRef != ""},
			}}
			res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
				Target:    groomingTarget(),
				Report:    report,
				Decisions: approveAll(report),
				Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
				States:    groomingStates(),
			})
			if err != nil {
				t.Fatalf("ApplyGrooming: %v", err)
			}
			rec := recordFor(t, res, entryID)
			if tc.wantDispatch {
				// COMMITTED STATE, not error identity: the discriminating
				// observation is whether the provider was asked to write.
				if len(mut.calls) != 1 {
					t.Fatalf("dispatched %d mutations, want ONE — the marker does not name this ref: %v", len(mut.calls), mut.kinds())
				}
				if rec.Outcome != GroomingOutcomeApplied {
					t.Errorf("record = %+v, want applied", rec)
				}
				return
			}
			if len(mut.calls) != 0 {
				t.Fatalf("dispatched %d mutations, want ZERO — the marker already records this ref: %v", len(mut.calls), mut.kinds())
			}
			// Tried and was refused BY THE DIFF, not did-not-try: the read
			// happened and the idempotence arm settled it.
			if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != GroomingSkipAlreadyApplied {
				t.Errorf("record outcome=%q reason=%q, want skipped/%s", rec.Outcome, rec.SkipReason, GroomingSkipAlreadyApplied)
			}
			if !rec.IdempotenceChecked {
				t.Error("IdempotenceChecked = false, want the diff to have settled the candidate")
			}
			if len(reader.reads) != 1 {
				t.Errorf("reader saw %+v, want one read", reader.reads)
			}
		})
	}
}

// TestApplyGrooming_SecondDependencyEdgeIsWrittenAdditively is the #2860 fix
// stated as behaviour, replacing the residual this test used to pin.
//
// BEFORE: `ensureDependsOnMarker` wrote a marker only when the body carried
// NONE, so every SECOND approved edge out of the same item was refused by the
// provider and audited as `depends_on marker already present` — a refusal
// indistinguishable from an idempotent no-op, which is how an 0/8 apply rate
// went unnoticed across three grooming walks.
//
// NOW: the second edge is MERGED into the existing marker line and is APPLIED.
// A re-apply then settles BOTH edges by the CORE's membership diff
// (`already_applied`) with ZERO further dispatch — the AC7 zero-dispatch
// property, reached through membership rather than through a provider refusal.
func TestApplyGrooming_SecondDependencyEdgeIsWrittenAdditively(t *testing.T) {
	first := dependencyEntry(1, 5)
	second := dependencyEntry(1, 6)
	report := &plan.GroomingReport{DependencyEdges: []plan.DependencyEdge{first, second}}
	forge := &statefulGroomingForge{items: map[string]*WorkItemRecord{
		"#1": {Number: 1, State: "open", Body: "Summary"},
	}}
	req := GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	}

	res, err := ApplyGrooming(context.Background(), forge, forge, &fakeGroomingSink{}, req)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	for _, e := range []plan.DependencyEdge{first, second} {
		if rec := recordFor(t, res, e.ID); rec.Outcome != GroomingOutcomeApplied {
			t.Errorf("edge %s outcome=%q reason=%q, want applied — the second edge is no longer refused",
				e.ID, rec.Outcome, rec.SkipReason)
		}
	}
	// ONE marker line carrying BOTH refs: additive, not double-stamped.
	body := forge.items["#1"].Body
	if got := countMarkerLines(body, "Depends on:"); got != 1 {
		t.Errorf("#1 carries %d depends-on marker lines, want exactly 1 — body: %q", got, body)
	}
	if !strings.Contains(body, "Depends on: #5, #6") {
		t.Errorf("second ref was not merged into the marker line — body: %q", body)
	}

	forge.calls = nil
	res, err = ApplyGrooming(context.Background(), forge, forge, &fakeGroomingSink{}, req)
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	for _, e := range []plan.DependencyEdge{first, second} {
		rec := recordFor(t, res, e.ID)
		if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != GroomingSkipAlreadyApplied {
			t.Errorf("edge %s re-apply outcome=%q reason=%q, want skipped/%s",
				e.ID, rec.Outcome, rec.SkipReason, GroomingSkipAlreadyApplied)
		}
	}
	if len(forge.calls) != 0 {
		t.Fatalf("re-apply dispatched %+v, want ZERO — both edges are settled by the core membership diff", forge.calls)
	}
}

// TestGroomingNormalizeRef_LeavesNonRefsAlone is the over-broadness guard on
// that normalization: it must collapse `1437` and `#1437` onto one value and
// NOTHING else. A normalizer that mapped two genuinely different values
// together would suppress a real mutation.
func TestGroomingNormalizeRef_LeavesNonRefsAlone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1437", "#1437"},
		{"#1437", "#1437"},
		{"  #1437  ", "#1437"},
		{"", ""},
		{"#", "#"},
		{"kuhlman-labs/fishhawk#1437", "kuhlman-labs/fishhawk#1437"},
		{"1437a", "1437a"},
		{"#12x", "#12x"},
	}
	for _, tc := range cases {
		if got := NormalizeIssueRef(tc.in); got != tc.want {
			t.Errorf("NormalizeIssueRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// The pair that must NOT collapse, stated as behaviour rather than shape.
	if NormalizeIssueRef("1437") == NormalizeIssueRef("1438") {
		t.Error("normalization collapsed two different issue numbers")
	}
}

// TestApplyGrooming_ValueSetKindsAreNotIdempotenceObservable states the
// RESIDUAL honestly and pins it: WorkItemRecord carries no arbitrary project
// FIELD value, so a rank/priority/field write cannot be diffed and re-applies.
// It is recorded with IdempotenceChecked=false, so the audit row says which
// arm settled the candidate rather than implying a coverage that does not
// exist.
func TestApplyGrooming_ValueSetKindsAreNotIdempotenceObservable(t *testing.T) {
	ord := orderingEntry(1, 3)
	report := &plan.GroomingReport{Ordering: []plan.OrderingEntry{ord}}
	mut := &fakeGroomingMutator{}
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{"#1": {Number: 1, State: "open"}}}
	req := GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: []GroomingDecision{{EntryID: ord.ID, Verdict: GroomingApproved}},
		Modes:     map[string]GroomingMode{"ordering": GroomingModeGated},
		States:    groomingStates(),
	}
	for i := 0; i < 2; i++ {
		res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, req)
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		rec := recordFor(t, res, ord.ID)
		if rec.Outcome != GroomingOutcomeApplied {
			t.Fatalf("apply %d: record = %+v, want applied", i, rec)
		}
		if rec.IdempotenceChecked {
			t.Errorf("apply %d: IdempotenceChecked = true, want false — a field value is not readable", i)
		}
	}
	if len(mut.calls) != 2 {
		t.Errorf("dispatches = %d, want 2 (the documented residual: a value-set write repeats)", len(mut.calls))
	}
	if len(reader.reads) != 0 {
		t.Errorf("reader was consulted %d times for an unobservable kind, want 0", len(reader.reads))
	}
}

// TestApplyGrooming_ReaderUnavailableFailsCandidateClosed pins the paired
// fail-closed branch of the pre-read: a typed reader *UnavailableError records
// the candidate FAILED and dispatches NOTHING. Dispatching blind is exactly
// the duplicate mutation AC7 forbids.
func TestApplyGrooming_ReaderUnavailableFailsCandidateClosed(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	unavailable := &UnavailableError{Provider: "github", Capability: ReaderCapability, Reason: ReasonForbidden}
	mut := &fakeGroomingMutator{}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{err: unavailable}, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator recorded %d dispatches, want 0 — a blind dispatch is what this branch prevents", len(mut.calls))
	}
	rec := recordFor(t, res, report.HygieneDefects[0].ID)
	if rec.Outcome != GroomingOutcomeFailed || !strings.Contains(rec.Error, string(ReasonForbidden)) {
		t.Errorf("record = %+v, want failed carrying the typed reader reason", rec)
	}
}

// TestApplyGrooming_NilReaderFailsObservableCandidatesClosed pins the same
// fail-closed direction for a caller that resolved NO reader at all.
func TestApplyGrooming_NilReaderFailsObservableCandidatesClosed(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	mut := &fakeGroomingMutator{}
	res, err := ApplyGrooming(context.Background(), mut, nil, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator recorded %d dispatches, want 0 with no reader resolved", len(mut.calls))
	}
	rec := recordFor(t, res, report.HygieneDefects[0].ID)
	if rec.Outcome != GroomingOutcomeFailed || !strings.Contains(rec.Error, string(ReasonNotImplemented)) {
		t.Errorf("record = %+v, want failed with the typed capability-unavailable reason", rec)
	}
}

// TestApplyGrooming_ReadReturningNoRecordFailsClosed pins the last pre-read
// branch: a reader that answers with neither record nor error fails the
// candidate rather than dispatching against unknown state.
func TestApplyGrooming_ReadReturningNoRecordFailsClosed(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	mut := &fakeGroomingMutator{}
	res, err := ApplyGrooming(context.Background(), mut, &nilRecordReader{}, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator recorded %d dispatches, want 0", len(mut.calls))
	}
	if rec := recordFor(t, res, report.HygieneDefects[0].ID); rec.Outcome != GroomingOutcomeFailed {
		t.Errorf("record = %+v, want failed", rec)
	}
}

type nilRecordReader struct{}

func (nilRecordReader) ReadWorkItem(context.Context, ReadWorkItemRequest) (*WorkItemRecord, error) {
	return nil, nil
}
func (nilRecordReader) ListWorkItems(context.Context, ListWorkItemsRequest) (*WorkItemPage, error) {
	return nil, errors.New("not used")
}

// ---------------------------------------------------------------------------
// Resolution refusals
// ---------------------------------------------------------------------------

// TestApplyGrooming_IceboxWithoutColumnIsRefused is approval condition I5: a
// conventions set with NO icebox column yields an explicit, audited SKIP —
// never a silent no-op and never a misroute to another column.
func TestApplyGrooming_IceboxWithoutColumnIsRefused(t *testing.T) {
	dec := decompositionEntry(1)
	report := &plan.GroomingReport{DecompositionSuggestions: []plan.DecompositionSuggestion{dec}}
	mut := &fakeGroomingMutator{}
	// The item is on the board in an expected SOURCE state, so the placement
	// guard would let this move through: the ONLY thing standing between an
	// unconfigured icebox column and a misroute to the empty column is the
	// refusal under test.
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		"#1": {Number: 1, State: "open", OnBoard: true, BoardColumn: "Backlog"},
	}}
	res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:       groomingTarget(),
		Report:       report,
		Decisions:    []GroomingDecision{{EntryID: dec.ID, Verdict: GroomingApproved}},
		Modes:        map[string]GroomingMode{"scoping": GroomingModeGated},
		GateApproved: map[string]bool{dec.ID: true},
		States:       groomingStates(),
		// IceboxColumn deliberately absent.
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Fatalf("mutator call log = %+v, want ZERO dispatches with no icebox column configured", mut.calls)
	}
	rec := recordFor(t, res, dec.ID)
	if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != GroomingSkipIceboxColumnUnavailable {
		t.Errorf("record outcome=%q reason=%q, want skipped/%s", rec.Outcome, rec.SkipReason, GroomingSkipIceboxColumnUnavailable)
	}
}

// TestApplyGrooming_DuplicateCloseTargetControls pins both refusals on the
// unordered duplicate pair: which item to close is NOT derivable from the
// report, so an absent choice and a choice outside the pair each skip rather
// than closing an arbitrary member.
func TestApplyGrooming_DuplicateCloseTargetControls(t *testing.T) {
	dup := duplicateEntry(1, 2)
	report := &plan.GroomingReport{Duplicates: []plan.DuplicateCandidate{dup}}
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{name: "unspecified", target: "", want: GroomingSkipDuplicateTargetUnspecified},
		{name: "outside the pair", target: "kuhlman-labs/fishhawk#999", want: GroomingSkipDuplicateTargetNotInPair},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mut := &fakeGroomingMutator{}
			res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, &fakeGroomingSink{}, GroomingApplyRequest{
				Target:       groomingTarget(),
				Report:       report,
				Decisions:    []GroomingDecision{{EntryID: dup.ID, Verdict: GroomingApproved, CloseTarget: tc.target}},
				Modes:        map[string]GroomingMode{"dedup": GroomingModeGated},
				GateApproved: map[string]bool{dup.ID: true},
				States:       groomingStates(),
			})
			if err != nil {
				t.Fatalf("ApplyGrooming: %v", err)
			}
			if len(mut.calls) != 0 {
				t.Fatalf("mutator call log = %+v, want ZERO closes", mut.calls)
			}
			if rec := recordFor(t, res, dup.ID); rec.SkipReason != tc.want {
				t.Errorf("skip reason = %q, want %q", rec.SkipReason, tc.want)
			}
		})
	}
}

// TestApplyGrooming_DuplicateClosesTheOperatorsChosenItem pins the positive
// side: the item the operator named is the one closed, so the control is not
// merely refusing everything.
func TestApplyGrooming_DuplicateClosesTheOperatorsChosenItem(t *testing.T) {
	dup := duplicateEntry(1, 2)
	report := &plan.GroomingReport{Duplicates: []plan.DuplicateCandidate{dup}}
	mut := &fakeGroomingMutator{}
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		"#1": {Number: 1, State: "open"},
		"#2": {Number: 2, State: "open"},
	}}
	_, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:       groomingTarget(),
		Report:       report,
		Decisions:    []GroomingDecision{{EntryID: dup.ID, Verdict: GroomingApproved, CloseTarget: "kuhlman-labs/fishhawk#2"}},
		Modes:        map[string]GroomingMode{"dedup": GroomingModeGated},
		GateApproved: map[string]bool{dup.ID: true},
		States:       groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 1 || mut.calls[0].ItemRef != "#2" {
		t.Fatalf("call log = %+v, want exactly one close of #2 (the operator's choice)", mut.calls)
	}
	// The idempotence read must judge the item that would actually be closed,
	// not the pair's first member.
	if len(reader.reads) != 1 || reader.reads[0].Ref != "#2" {
		t.Errorf("reader saw %+v, want the chosen close target read", reader.reads)
	}
}

// TestApplyGrooming_ItemRefResolutionRefusals pins the two item-ref refusals:
// an item in a DIFFERENT repository is never mutated against this target, and
// an id this layer cannot parse is refused rather than passed through.
func TestApplyGrooming_ItemRefResolutionRefusals(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want string
	}{
		{name: "other repo", id: "someone-else/other#7", want: GroomingSkipItemOutsideTarget},
		{name: "jira-style id", id: "FISH-7", want: GroomingSkipItemRefUnresolvable},
		{name: "no number", id: "kuhlman-labs/fishhawk#", want: GroomingSkipItemRefUnresolvable},
		{name: "non-numeric", id: "kuhlman-labs/fishhawk#abc", want: GroomingSkipItemRefUnresolvable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := plan.ItemRef{Type: "github_issue", ID: tc.id, URL: "https://example.test"}
			h := plan.HygieneDefect{
				ID:      plan.GroomingEntryID(plan.GroomingClassHygiene, "missing_label_namespace", ref),
				ItemRef: ref,
				Defect:  "missing_label_namespace",
				Detail:  "d",
				Fix:     &plan.HygieneFix{Labels: []string{"area:api"}},
			}
			report := &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{h}}
			mut := &fakeGroomingMutator{}
			res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, &fakeGroomingSink{}, GroomingApplyRequest{
				Target:    groomingTarget(),
				Report:    report,
				Decisions: []GroomingDecision{{EntryID: h.ID, Verdict: GroomingApproved}},
				Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
				States:    groomingStates(),
			})
			if err != nil {
				t.Fatalf("ApplyGrooming: %v", err)
			}
			if len(mut.calls) != 0 {
				t.Fatalf("mutator call log = %+v, want ZERO dispatches", mut.calls)
			}
			if rec := recordFor(t, res, h.ID); rec.SkipReason != tc.want {
				t.Errorf("skip reason = %q, want %q", rec.SkipReason, tc.want)
			}
		})
	}
}

// TestApplyGrooming_DependencyTargetOutsideRepoIsRefused pins the SECOND ref
// resolution on a dependency edge — the edge's TO endpoint — which the
// from-endpoint cases above do not reach.
func TestApplyGrooming_DependencyTargetOutsideRepoIsRefused(t *testing.T) {
	from := groomingRef(1)
	to := plan.ItemRef{Type: "github_issue", ID: "someone-else/other#9", URL: "https://example.test"}
	edge := plan.DependencyEdge{
		ID: plan.GroomingEntryID(plan.GroomingClassDependency, "", from, to), From: from, To: to,
		Basis: "b", Kind: "depends_on",
	}
	report := &plan.GroomingReport{DependencyEdges: []plan.DependencyEdge{edge}}
	mut := &fakeGroomingMutator{}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: []GroomingDecision{{EntryID: edge.ID, Verdict: GroomingApproved}},
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Fatalf("mutator call log = %+v, want ZERO dispatches", mut.calls)
	}
	if rec := recordFor(t, res, edge.ID); rec.SkipReason != GroomingSkipItemOutsideTarget {
		t.Errorf("skip reason = %q, want %q", rec.SkipReason, GroomingSkipItemOutsideTarget)
	}
}

// ---------------------------------------------------------------------------
// Argument fail-closed
// ---------------------------------------------------------------------------

// TestApplyGrooming_RequiresReportMutatorAndSink pins the three argument
// refusals. The sink case matters most: AC3 requires every mutation be
// audited, so an apply with nowhere to record is refused rather than run
// unaudited.
func TestApplyGrooming_RequiresReportMutatorAndSink(t *testing.T) {
	report := &plan.GroomingReport{
		HygieneDefects: []plan.HygieneDefect{hygieneEntry(1, "missing_label_namespace", "area:api")},
	}
	base := GroomingApplyRequest{Target: groomingTarget(), Report: report, Decisions: approveAll(report)}

	if _, err := ApplyGrooming(context.Background(), &fakeGroomingMutator{}, nil, &fakeGroomingSink{},
		GroomingApplyRequest{Target: groomingTarget()}); err == nil {
		t.Error("a nil report must be refused")
	}
	if _, err := ApplyGrooming(context.Background(), nil, nil, &fakeGroomingSink{}, base); err == nil {
		t.Error("a nil mutator must be refused")
	}
	mut := &fakeGroomingMutator{}
	if _, err := ApplyGrooming(context.Background(), mut, nil, nil, base); err == nil {
		t.Error("a nil audit sink must be refused")
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator recorded %d dispatches, want 0 — an unauditable apply must run nothing", len(mut.calls))
	}
}

// TestApplyGrooming_AuditCategoriesAreStable pins the two category strings the
// audit registry (and every ?category= filter) is keyed on.
func TestApplyGrooming_AuditCategoriesAreStable(t *testing.T) {
	if GroomingMutationAppliedCategory != "grooming_mutation_applied" {
		t.Errorf("per-mutation category = %q", GroomingMutationAppliedCategory)
	}
	if GroomingApplyCompletedCategory != "grooming_apply_completed" {
		t.Errorf("summary category = %q", GroomingApplyCompletedCategory)
	}
}

// TestGroomingActionClassFor pins the EXPORTED report-class -> action-class
// mapping. It is not decoration: the server's groom-gate apply hook (E54.19 /
// #2822) decides "may this entry be auto-applied?" by testing this function's
// answer against spec's `hygiene`, so this table is the contract that keeps a
// remap of `dependency` — or of anything else — out of the hygiene class from
// silently widening what a single gate approval authorizes. A remap made HERE
// fails HERE.
func TestGroomingActionClassFor(t *testing.T) {
	cases := []struct {
		reportClass string
		want        string
	}{
		{plan.GroomingClassHygiene, "hygiene"},
		{plan.GroomingClassDependency, "hygiene"},
		{plan.GroomingClassOrdering, "ordering"},
		{plan.GroomingClassDuplicate, "dedup"},
		{plan.GroomingClassDecomposition, "scoping"},
		{plan.GroomingClassVisionDrift, "scoping"},
		// An unrecognized class returns the EMPTY string, never a guess: an
		// equality test against a named class is then false, so an unknown
		// class can never test as hygiene.
		{"not_a_grooming_class", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := GroomingActionClassFor(tc.reportClass); got != tc.want {
			t.Errorf("GroomingActionClassFor(%q) = %q, want %q", tc.reportClass, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// #2847 — the structured hygiene fix, and the prose that must never reach a
// mutation request.
// ---------------------------------------------------------------------------

// hygieneFixEntry builds a hygiene defect with an EXPLICIT fix, so a test can
// seed a bad value BY CONSTRUCTION (a wrong member, a malformed label) rather
// than by routing it through the helper's defect->member mapping.
func hygieneFixEntry(n int, defect string, fix *plan.HygieneFix) plan.HygieneDefect {
	d := hygieneEntry(n, defect, "")
	d.Fix = fix
	return d
}

// applyOneHygiene drives ONE hygiene entry through the real ApplyGrooming with
// the entry approved and the class on auto, and returns its record together
// with the mutator's full call log. Every #2847 refusal case asserts on the
// call log — the observable behaviour — not on a return code.
func applyOneHygiene(t *testing.T, h plan.HygieneDefect, states map[string]string) (GroomingMutationRecord, []GroomingMutationRequest) {
	t.Helper()
	report := &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{h}}
	mut := &fakeGroomingMutator{}
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		"#1": {Number: 1, State: "open", Body: "Summary"},
		"#2": {Number: 2, State: "open", Body: "Summary"},
		"#3": {Number: 3, State: "open", Body: "Summary"},
	}}
	res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: []GroomingDecision{{EntryID: h.ID, Verdict: GroomingApproved}},
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		// The per-entry gate grant rule 8 (#2855) requires for a
		// delegation-tier proposal. It is granted here so these tests keep
		// exercising the FIX-VALUE contract they are about — including the
		// #2847 incident's literal three-label set, which contains
		// `autonomy:medium` — instead of settling on rule 8's refusal. Rule 8's
		// own behaviour is pinned by the TestGroomingApply_DelegationTier* set,
		// which drives GateApproved explicitly in both directions.
		GateApproved: map[string]bool{h.ID: true},
		States:       states,
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	return recordFor(t, res, h.ID), mut.calls
}

// TestDeriveGroomingMutations_LabelFixNamesEveryLabel is the positive half of
// the #2847 fix: a THREE-label structured fix produces a request naming all
// three names verbatim.
//
// The three-name case is the one the incident could not express: the prose
// `Add area:server-api, autonomy:medium, phase:alpha.` was dispatched as ONE
// label with that literal text. Recovering three names from that sentence is a
// guess; the structured member states them.
func TestDeriveGroomingMutations_LabelFixNamesEveryLabel(t *testing.T) {
	want := []string{"area:server-api", "autonomy:medium", "phase:alpha"}
	h := hygieneFixEntry(1, "missing_label_namespace", &plan.HygieneFix{Labels: want})

	rec, calls := applyOneHygiene(t, h, groomingStates())
	if rec.Outcome != GroomingOutcomeApplied {
		t.Fatalf("outcome = %q (%+v), want applied", rec.Outcome, rec)
	}
	if len(calls) != 1 {
		t.Fatalf("dispatches = %d (%+v), want exactly one", len(calls), calls)
	}
	if got := calls[0].After.List; !equalStrings(got, want) {
		t.Errorf("dispatched labels = %#v, want exactly %#v", got, want)
	}
	if calls[0].After.Scalar != "" {
		t.Errorf("dispatched scalar = %q, want empty: a label set is a LIST", calls[0].After.Scalar)
	}
}

// TestDeriveGroomingMutations_EpicFixAcceptsBothWireForms pins the parent-epic
// validator against BOTH forms the schema admits — a bare `389` and a hashed
// `#389` (approval condition C4) — and against the prose shapes that must not
// dispatch. The validator is explicit rather than delegated to
// NormalizeIssueRef, which returns an unparseable value UNCHANGED and would
// therefore hand `Link as a sub-issue of E22 #389.` straight to the forge.
func TestDeriveGroomingMutations_EpicFixAcceptsBothWireForms(t *testing.T) {
	cases := []struct {
		name       string
		epic       string
		wantAfter  string
		wantSkip   string
		wantCalled bool
	}{
		{name: "bare number", epic: "389", wantAfter: "#389", wantCalled: true},
		{name: "hashed number", epic: "#389", wantAfter: "#389", wantCalled: true},
		{name: "leading zeros normalize", epic: "0389", wantAfter: "#389", wantCalled: true},
		{name: "surrounding space", epic: "  #389  ", wantAfter: "#389", wantCalled: true},
		{name: "prose around the ref", epic: "Link as a sub-issue of E22 #389.", wantSkip: GroomingSkipInvalidFixValue},
		{name: "epic slug", epic: "E22", wantSkip: GroomingSkipInvalidFixValue},
		{name: "zero is not an issue number", epic: "0", wantSkip: GroomingSkipInvalidFixValue},
		{name: "negative", epic: "-5", wantSkip: GroomingSkipInvalidFixValue},
		{name: "two hashes", epic: "##389", wantSkip: GroomingSkipInvalidFixValue},
		{name: "absent", epic: "", wantSkip: GroomingSkipNoStructuredFix},
		{name: "all whitespace", epic: "   ", wantSkip: GroomingSkipNoStructuredFix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hygieneFixEntry(1, "unlinked_parent_epic", &plan.HygieneFix{ParentEpic: tc.epic})
			rec, calls := applyOneHygiene(t, h, groomingStates())
			if !tc.wantCalled {
				if len(calls) != 0 {
					t.Fatalf("dispatches = %+v, want ZERO", calls)
				}
				if rec.SkipReason != tc.wantSkip {
					t.Errorf("skip reason = %q, want %q", rec.SkipReason, tc.wantSkip)
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("dispatches = %d (%+v), want exactly one", len(calls), calls)
			}
			if calls[0].After.Scalar != tc.wantAfter {
				t.Errorf("dispatched parent epic = %q, want %q", calls[0].After.Scalar, tc.wantAfter)
			}
		})
	}
}

// TestGroomingValidLabel_RejectsWhatItPromises reconciles the label rule with
// its stated contract (approval condition C3): "leading or trailing
// punctuation" means ANY punctuation or symbol rune, not the four
// sentence-terminators, so `phase:alpha!` and `phase:alpha?` are refused
// exactly as `Add phase:alpha.` is.
//
// It is a pure-function table; the BEHAVIOURAL half (a rejected label
// dispatches nothing) is TestDeriveGroomingMutations_InvalidLabelIsRefused.
func TestGroomingValidLabel_RejectsWhatItPromises(t *testing.T) {
	cases := []struct {
		name  string
		label string
		want  bool
	}{
		{name: "namespaced label", label: "phase:alpha", want: true},
		{name: "hyphenated value", label: "area:server-api", want: true},
		{name: "milestone", label: "milestone:alpha", want: true},
		{name: "digits", label: "estimate:3", want: true},
		{name: "at the length cap", label: strings.Repeat("a", groomingMaxLabelLen), want: true},

		{name: "the incident's own prose", label: "Add phase:alpha."},
		{name: "the incident's multi-label prose", label: "Add area:server-api, autonomy:medium, phase:alpha."},
		{name: "trailing period", label: "phase:alpha."},
		{name: "trailing exclamation", label: "phase:alpha!"},
		{name: "trailing question mark", label: "phase:alpha?"},
		{name: "trailing semicolon", label: "phase:alpha;"},
		{name: "trailing comma", label: "phase:alpha,"},
		{name: "leading hash", label: "#phase:alpha"},
		{name: "leading colon", label: ":phase"},
		{name: "leading plus symbol", label: "+phase:alpha"},
		{name: "interior space", label: "good first issue"},
		{name: "empty", label: ""},
		// THE RAW BOUNDARY. The caller no longer trims before calling, so a
		// space-padded name is judged as supplied and refused — it is not
		// rewritten into a passing one. Under a caller-side trim these three
		// rows are the same input as the valid `phase:alpha` row above.
		{name: "leading space", label: " phase:alpha"},
		{name: "trailing space", label: "phase:alpha "},
		{name: "leading and trailing space", label: " phase:alpha "},
		{name: "tab", label: "phase:\talpha"},
		{name: "leading tab", label: "\tphase:alpha"},
		{name: "newline", label: "phase:\nalpha"},
		{name: "trailing newline", label: "phase:alpha\n"},
		// A CONTROL rune that unicode.IsSpace does NOT match, and the very
		// separator groomingHygieneBasis joins the label set on.
		{name: "unit separator", label: "phase:alpha\x1fbeta"},
		{name: "NUL", label: "phase:alpha\x00"},
		{name: "one over the length cap", label: strings.Repeat("a", groomingMaxLabelLen+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := groomingValidLabel(tc.label); got != tc.want {
				t.Errorf("groomingValidLabel(%q) = %t, want %t", tc.label, got, tc.want)
			}
		})
	}
}

// TestDeriveGroomingMutations_InvalidLabelIsRefused is the behavioural half of
// the label rule: each rejected shape records `invalid_fix_value` and
// dispatches NOTHING. Bad values are seeded as literals, so the RED under a
// deleted validator lands on the dispatch-count assertion.
//
// The multi-label row is the partial-write rule: ONE invalid name fails the
// WHOLE entry, because a half-applied label set is a fix nobody proposed.
func TestDeriveGroomingMutations_InvalidLabelIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{name: "the incident's prose", labels: []string{"Add phase:alpha."}, want: GroomingSkipInvalidFixValue},
		{name: "interior space", labels: []string{"good first issue"}, want: GroomingSkipInvalidFixValue},
		{name: "trailing period", labels: []string{"phase:alpha."}, want: GroomingSkipInvalidFixValue},
		{name: "trailing exclamation", labels: []string{"phase:alpha!"}, want: GroomingSkipInvalidFixValue},
		{name: "over the length cap", labels: []string{strings.Repeat("a", groomingMaxLabelLen+1)}, want: GroomingSkipInvalidFixValue},
		{name: "one bad name fails the whole set", labels: []string{"phase:alpha", "Add area:api."}, want: GroomingSkipInvalidFixValue},
		{name: "a blank member", labels: []string{"phase:alpha", "   "}, want: GroomingSkipInvalidFixValue},
		// The behavioural half of the raw boundary: a padded label is REFUSED
		// with zero dispatches, not trimmed and written. Under a validate-
		// after-trim ordering each of these dispatches `phase:alpha`.
		{name: "leading space", labels: []string{" phase:alpha"}, want: GroomingSkipInvalidFixValue},
		{name: "trailing space", labels: []string{"phase:alpha "}, want: GroomingSkipInvalidFixValue},
		{name: "trailing newline", labels: []string{"phase:alpha\n"}, want: GroomingSkipInvalidFixValue},
		{name: "one padded name fails the whole set", labels: []string{"phase:alpha", " area:api"}, want: GroomingSkipInvalidFixValue},
		{name: "unit separator control char", labels: []string{"phase:alpha\x1fbeta"}, want: GroomingSkipInvalidFixValue},
		{name: "empty list", labels: []string{}, want: GroomingSkipNoStructuredFix},
		{name: "nil list", labels: nil, want: GroomingSkipNoStructuredFix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hygieneFixEntry(1, "missing_label_namespace", &plan.HygieneFix{Labels: tc.labels})
			rec, calls := applyOneHygiene(t, h, groomingStates())
			if len(calls) != 0 {
				t.Fatalf("dispatches = %+v, want ZERO: a refused label must never reach the provider", calls)
			}
			if rec.SkipReason != tc.want {
				t.Errorf("skip reason = %q, want %q", rec.SkipReason, tc.want)
			}
			if rec.Outcome != GroomingOutcomeSkipped {
				t.Errorf("outcome = %q, want skipped", rec.Outcome)
			}
		})
	}
}

// TestDeriveGroomingMutations_AbsentOrWrongMemberIsNoStructuredFix pins the
// three shapes of "this proposal carries no value for this kind": a nil fix, a
// fix populating only a member this kind does not read, and a blank scalar.
// Each records `no_structured_fix` and dispatches NOTHING — and CRUCIALLY does
// NOT fall back to the `suggested_fix` prose, which every fixture here carries.
func TestDeriveGroomingMutations_AbsentOrWrongMemberIsNoStructuredFix(t *testing.T) {
	cases := []struct {
		name   string
		defect string
		fix    *plan.HygieneFix
	}{
		{name: "label/nil fix", defect: "missing_label_namespace", fix: nil},
		{name: "label/only parent_epic populated", defect: "missing_label_namespace", fix: &plan.HygieneFix{ParentEpic: "#389"}},
		{name: "epic/nil fix", defect: "unlinked_parent_epic", fix: nil},
		{name: "epic/only labels populated", defect: "unlinked_parent_epic", fix: &plan.HygieneFix{Labels: []string{"phase:alpha"}}},
		{name: "epic/all whitespace", defect: "unlinked_parent_epic", fix: &plan.HygieneFix{ParentEpic: "  \t "}},
		{name: "board/nil fix", defect: "unboarded", fix: nil},
		{name: "board/only field_value populated", defect: "unboarded", fix: &plan.HygieneFix{FieldValue: "3"}},
		{name: "board/all whitespace", defect: "unboarded", fix: &plan.HygieneFix{BoardState: "   "}},
		{name: "field/nil fix", defect: "missing_estimate", fix: nil},
		{name: "field/only board_state populated", defect: "missing_estimate", fix: &plan.HygieneFix{BoardState: "backlog"}},
		{name: "field/empty value", defect: "missing_estimate", fix: &plan.HygieneFix{FieldValue: ""}},
		// Spaces and tabs only: a whitespace run CARRYING A NEWLINE is refused
		// as invalid_fix_value by the raw single-line check that now runs
		// ahead of the blank check, not reported as a missing value.
		{name: "field/all whitespace", defect: "missing_estimate", fix: &plan.HygieneFix{FieldValue: " \t "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hygieneFixEntry(1, tc.defect, tc.fix)
			rec, calls := applyOneHygiene(t, h, groomingStates())
			if len(calls) != 0 {
				t.Fatalf("dispatches = %+v, want ZERO: with no structured value there is nothing to write", calls)
			}
			if rec.SkipReason != GroomingSkipNoStructuredFix {
				t.Errorf("skip reason = %q, want %q", rec.SkipReason, GroomingSkipNoStructuredFix)
			}
		})
	}
}

// TestGroomingStructuredFix_UnreadKindFailsClosed pins the switch's DEFAULT
// arm. It is unreachable from the derivation today — groomingHygieneKinds maps
// exactly the four kinds the switch reads — so it is driven directly, which is
// the only way to prove a kind added to that map without a corresponding arm
// fails CLOSED (a named skip, nothing to write) rather than dispatching an
// empty value.
func TestGroomingStructuredFix_UnreadKindFailsClosed(t *testing.T) {
	for _, kind := range []GroomingMutationKind{GroomingKindRankSet, GroomingKindCloseDuplicate, GroomingKindIcebox, ""} {
		got, reason := groomingStructuredFix(kind, &plan.HygieneFix{
			Labels: []string{"area:api"}, ParentEpic: "#1", BoardState: "backlog", FieldValue: "3",
		})
		if reason != GroomingSkipNoStructuredFix {
			t.Errorf("kind %q: reason = %q, want %q", kind, reason, GroomingSkipNoStructuredFix)
		}
		if got.Scalar != "" || len(got.List) != 0 {
			t.Errorf("kind %q: value = %+v, want the zero value: an unread kind has no member to read", kind, got)
		}
	}
}

// TestDeriveGroomingMutations_MultiLineFieldValueIsRefused pins the remaining
// field_set branch: a value carrying a newline is not a board field value, and
// is refused pre-dispatch rather than written.
//
// THE BOUNDARY ROWS ARE THE POINT. An interior newline is refused whichever
// order the trim and the check run in; a LEADING or TRAILING one is refused
// only because the single-line rule is enforced against the RAW value. With
// the trim first, `3\n` and `\r3` become `3` and are dispatched — the exact
// value the schema promises is refused.
func TestDeriveGroomingMutations_MultiLineFieldValueIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{name: "interior newline", value: "3\nand also 5"},
		{name: "trailing newline", value: "3\n"},
		{name: "leading carriage return", value: "\r3"},
		{name: "trailing carriage return", value: "3\r"},
		{name: "trailing CRLF", value: "3\r\n"},
		{name: "leading newline", value: "\n3"},
		{name: "newline inside surrounding spaces", value: " 3\n "},
		{name: "whitespace run carrying a newline", value: " \n "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hygieneFixEntry(1, "missing_estimate", &plan.HygieneFix{FieldValue: tc.value})
			rec, calls := applyOneHygiene(t, h, groomingStates())
			if len(calls) != 0 {
				t.Fatalf("dispatches = %+v, want ZERO: a value carrying a newline must never reach the provider", calls)
			}
			if rec.SkipReason != GroomingSkipInvalidFixValue {
				t.Errorf("skip reason = %q, want %q", rec.SkipReason, GroomingSkipInvalidFixValue)
			}
			if rec.Outcome != GroomingOutcomeSkipped {
				t.Errorf("outcome = %q, want skipped", rec.Outcome)
			}
		})
	}
}

// TestResolveGroomingRequest_BoardStateResolvesThroughTheStatesMap drives the
// board-state rule at the layer that RESOLVES it (approval condition C5):
// resolveGroomingRequest maps the derivation's CANONICAL state through the
// request's states map to that board's own column option, and refuses a state
// the conventions do not declare.
//
// A derivation-level test cannot observe either half — the derivation has no
// states map — so both the positive resolution and the refusal are asserted
// here and re-asserted end-to-end on the dispatched request below.
func TestResolveGroomingRequest_BoardStateResolvesThroughTheStatesMap(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		states     map[string]string
		wantOption string
		wantSkip   string
	}{
		{name: "canonical backlog", state: "backlog", states: groomingStates(), wantOption: "Backlog"},
		{name: "canonical up_next", state: "up_next", states: groomingStates(), wantOption: "Up Next"},
		{
			// Condition C6: the lookup is case-insensitive on BOTH sides, so
			// it cannot disagree with the churn fingerprint (which lower-cases)
			// about whether `Backlog` and `backlog` are one state.
			name: "mixed case canonical", state: "BackLog", states: groomingStates(), wantOption: "Backlog",
		},
		{
			// The other half of C6's symmetry: a conventions map whose KEY is
			// mixed-case resolves too. Without the key-side normalization this
			// row alone reddens — the input-side one below cannot cover it.
			name: "mixed case conventions KEY", state: "backlog",
			states: map[string]string{"BackLog": "Backlog"}, wantOption: "Backlog",
		},
		{name: "state the conventions do not declare", state: "icebox", states: groomingStates(), wantSkip: GroomingSkipInvalidFixValue},
		{name: "provider option, not a canonical state", state: "In Progress", states: groomingStates(), wantSkip: GroomingSkipInvalidFixValue},
		{name: "prose", state: "Add to Project #7 with Status=Backlog.", states: groomingStates(), wantSkip: GroomingSkipInvalidFixValue},
		{name: "no states configured", state: "backlog", states: nil, wantSkip: GroomingSkipInvalidFixValue},
		{name: "state declared with an empty option", state: "backlog", states: map[string]string{CanonicalStateBacklog: "  "}, wantSkip: GroomingSkipInvalidFixValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := groomingCandidate{
				entryID: "hygiene:x:unboarded",
				class:   "hygiene",
				kind:    GroomingKindBoardPlace,
				ref:     groomingRef(1),
				after:   GroomingValue{Scalar: tc.state},
			}
			out, reason := resolveGroomingRequest(c, GroomingDecision{}, GroomingApplyRequest{
				Target: groomingTarget(),
				States: tc.states,
			})
			if reason != tc.wantSkip {
				t.Fatalf("skip reason = %q, want %q", reason, tc.wantSkip)
			}
			if tc.wantSkip != "" {
				return
			}
			if out.After.Scalar != tc.wantOption {
				t.Errorf("resolved board option = %q, want %q (the PROVIDER's column name, not the canonical state)",
					out.After.Scalar, tc.wantOption)
			}
		})
	}
}

// TestApplyGrooming_UnresolvableBoardStateDispatchesNothing is the behavioural
// half of the resolution refusal: the entry is approved, the class is on auto,
// and the item is genuinely off-board — so with the refusal deleted the
// mutation really does dispatch.
func TestApplyGrooming_UnresolvableBoardStateDispatchesNothing(t *testing.T) {
	h := hygieneFixEntry(1, "unboarded", &plan.HygieneFix{BoardState: "icebox"})
	report := &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{h}}
	mut := &fakeGroomingMutator{}
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		"#1": {Number: 1, State: "open", OnBoard: false},
	}}
	res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: []GroomingDecision{{EntryID: h.ID, Verdict: GroomingApproved}},
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Fatalf("dispatches = %+v, want ZERO for a board state the conventions do not declare", mut.calls)
	}
	if rec := recordFor(t, res, h.ID); rec.SkipReason != GroomingSkipInvalidFixValue {
		t.Errorf("skip reason = %q, want %q", rec.SkipReason, GroomingSkipInvalidFixValue)
	}
}

// TestApplyGrooming_BoardPlaceDispatchesTheProviderOption is the positive
// control for the pair above: a declared canonical state dispatches, carrying
// the BOARD's option string — the same vocabulary groomingObserved projects
// item.BoardColumn onto, so a resolved placement still settles on re-apply.
func TestApplyGrooming_BoardPlaceDispatchesTheProviderOption(t *testing.T) {
	h := hygieneFixEntry(1, "unboarded", &plan.HygieneFix{BoardState: "backlog"})
	report := &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{h}}
	mut := &fakeGroomingMutator{}
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		"#1": {Number: 1, State: "open", OnBoard: false},
	}}
	if _, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: []GroomingDecision{{EntryID: h.ID, Verdict: GroomingApproved}},
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	}); err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("dispatches = %d (%+v), want exactly one", len(mut.calls), mut.calls)
	}
	if got := mut.calls[0].After.Scalar; got != "Backlog" {
		t.Errorf("dispatched board value = %q, want the provider option %q", got, "Backlog")
	}
}

// TestGroomingApply_NeverReadsSuggestedFix is the DEFECT-CLASS PIN the issue's
// AC4 asks for, asserted directly rather than inferred: no `.SuggestedFix`
// selector appears ANYWHERE in grooming_apply.go.
//
// A behavioural test can only prove that the paths it drives do not read the
// prose. This proves the file cannot, so a future "just fall back when `fix` is
// absent" one-liner fails HERE — it is the un-reappearable form of the fix.
func TestGroomingApply_NeverReadsSuggestedFix(t *testing.T) {
	const file = "grooming_apply.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel != nil && sel.Sel.Name == "SuggestedFix" {
			t.Errorf("%s:%d reads .SuggestedFix — the apply path must read the STRUCTURED `fix` and never the prose (#2847)",
				file, fset.Position(sel.Pos()).Line)
		}
		return true
	})
}

// TestGroomingApply_ProseFixIsNeverDispatched is the LIVE-INCIDENT regression
// fixture: the three literal `suggested_fix` sentences the groomer emitted on
// 2026-08-22, with NO structured fix. Every entry must skip and nothing may
// dispatch.
//
// It is deliberately NOT the vehicle for the disjointness assertion (approval
// condition C1): `phase:alpha` is a SUBSTRING of `Add phase:alpha.`, so "no
// request field contains the prose" cannot discriminate on these strings. That
// assertion lives in the test below, on a fixture whose prose and structured
// value share no substring.
func TestGroomingApply_ProseFixIsNeverDispatched(t *testing.T) {
	cases := []struct {
		defect string
		prose  string
	}{
		{defect: "missing_label_namespace", prose: "Add phase:alpha."},
		{defect: "missing_label_namespace", prose: "Add area:server-api, autonomy:medium, phase:alpha."},
		{defect: "unlinked_parent_epic", prose: "Link as a sub-issue of E22 #389."},
		{defect: "unboarded", prose: "Add to Project #7 with Status=Backlog."},
		{defect: "missing_estimate", prose: "Set the estimate to 3 points."},
	}
	report := &plan.GroomingReport{}
	for i, tc := range cases {
		h := hygieneEntry(i+1, tc.defect, "")
		h.SuggestedFix = tc.prose
		h.Fix = nil
		report.HygieneDefects = append(report.HygieneDefects, h)
	}

	mut := &fakeGroomingMutator{}
	reader := &fakeGroomingReader{}
	res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if len(mut.calls) != 0 {
		t.Fatalf("dispatches = %+v, want ZERO: prose is not a mutation payload", mut.calls)
	}
	if len(res.Skipped) != len(cases) {
		t.Fatalf("skipped = %d, want one per entry (%d)", len(res.Skipped), len(cases))
	}
	for _, rec := range res.Skipped {
		if rec.SkipReason != GroomingSkipNoStructuredFix {
			t.Errorf("%s: skip reason = %q, want %q — an absent `fix` must be NAMED, not silently absorbed",
				rec.EntryID, rec.SkipReason, GroomingSkipNoStructuredFix)
		}
	}
}

// TestGroomingApply_DispatchedValueIsStructuredAndNotProse is approval
// condition C1's fixture: prose that is LEXICALLY DISJOINT from the structured
// value, so both halves of the assertion are satisfiable AND discriminating.
//
//   - POSITIVE: the dispatched value EQUALS the structured fix exactly.
//   - NEGATIVE: no field of the request contains any word of the prose.
//
// Equality alone would pass a code path that dispatched the structured value
// while ALSO leaking the prose somewhere; disjointness alone would pass a path
// that dispatched nothing. Together they pin the defect class shut. The
// incident's own strings are kept as the separate regression fixture above,
// where `phase:alpha ⊂ "Add phase:alpha."` makes disjointness unsatisfiable
// for correct behaviour.
func TestGroomingApply_DispatchedValueIsStructuredAndNotProse(t *testing.T) {
	const prose = "Please attach the ownership marking for backend duties"
	const label = "area:server-api"

	// Words of four runes or more; the short connectives ("the", "for") are
	// not discriminating tokens and would only make the assertion brittle.
	var proseWords []string
	for _, w := range strings.Fields(prose) {
		if len(w) >= 4 {
			proseWords = append(proseWords, w)
		}
	}
	if len(proseWords) < 4 {
		t.Fatalf("fixture bug: %d discriminating prose words, want several", len(proseWords))
	}

	h := hygieneFixEntry(1, "missing_label_namespace", &plan.HygieneFix{Labels: []string{label}})
	h.SuggestedFix = prose
	// THE DISJOINTNESS PRECONDITION, checked rather than assumed: if any prose
	// word were a substring of the structured value, the negative assertion
	// below could not pass for CORRECT behaviour — which is exactly the flaw
	// condition C1 identified in the incident's own strings.
	for _, w := range proseWords {
		if strings.Contains(label, w) || strings.Contains(w, label) {
			t.Fatalf("fixture bug: prose word %q overlaps the structured value %q; the fixture must be lexically disjoint", w, label)
		}
	}

	rec, calls := applyOneHygiene(t, h, groomingStates())
	if rec.Outcome != GroomingOutcomeApplied {
		t.Fatalf("outcome = %q (%+v), want applied", rec.Outcome, rec)
	}
	if len(calls) != 1 {
		t.Fatalf("dispatches = %d (%+v), want exactly one", len(calls), calls)
	}
	req := calls[0]

	// POSITIVE half: the request carries the structured value EXACTLY.
	if !equalStrings(req.After.List, []string{label}) {
		t.Errorf("dispatched labels = %#v, want exactly %#v", req.After.List, []string{label})
	}

	// NEGATIVE half: no part of the request came from the prose.
	blob := fmt.Sprintf("%#v", req)
	if strings.Contains(blob, prose) {
		t.Errorf("the dispatched request carries the whole suggested_fix sentence: %s", blob)
	}
	for _, w := range proseWords {
		if strings.Contains(blob, w) {
			t.Errorf("the dispatched request carries the prose word %q — a value derived from suggested_fix reached the provider: %s", w, blob)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Containment rule 8: the delegation tier (E54.34 / #2855)
// ---------------------------------------------------------------------------

// tierHygieneEntry builds a missing_label_namespace defect proposing an
// arbitrary label SET, which hygieneEntry's single-value shape cannot express.
// The prose stays lexically disjoint from every structured value for the same
// reason hygieneEntry's does.
func tierHygieneEntry(n int, labels ...string) plan.HygieneDefect {
	d := hygieneEntry(n, "missing_label_namespace", "placeholder")
	d.Fix = &plan.HygieneFix{Labels: labels}
	return d
}

// applyTierEntry runs ONE hygiene entry through the real apply layer under the
// given mode and gate grant, and returns the settled record alongside the
// mutator's dispatch log. State, not error identity, is the discriminating
// observation: rule 8's whole effect is a write that did not happen.
func applyTierEntry(t *testing.T, e plan.HygieneDefect, mode GroomingMode, gate map[string]bool,
) (GroomingMutationRecord, *fakeGroomingMutator) {
	t.Helper()
	report := &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{e}}
	mut := &fakeGroomingMutator{}
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		"#" + strings.TrimPrefix(e.ItemRef.ID, "kuhlman-labs/fishhawk#"): {State: "open", Labels: []string{"type:bug"}},
	}}
	res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:       groomingTarget(),
		Report:       report,
		Decisions:    approveAll(report),
		Modes:        map[string]GroomingMode{"hygiene": mode},
		GateApproved: gate,
		States:       groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	return recordFor(t, res, e.ID), mut
}

// TestGroomingApply_DelegationTierLabelRefusedUnderModeAuto is rule 8's primary
// failure mode: mode auto + an APPROVED entry + no per-entry grant — the exact
// authority a whole-report gate approval carries in this repository — must NOT
// write a delegation tier.
//
// COUNTERFACTUAL: delete the two-line rule in settleGroomingCandidate and the
// entry dispatches; the zero-dispatch assertion reddens.
func TestGroomingApply_DelegationTierLabelRefusedUnderModeAuto(t *testing.T) {
	e := tierHygieneEntry(1, "area:backend", "autonomy:low", "phase:alpha")
	rec, mut := applyTierEntry(t, e, GroomingModeAuto, nil)

	if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != GroomingSkipDelegationTierNotAuthorized {
		t.Fatalf("record = %+v, want skipped/%s", rec, GroomingSkipDelegationTierNotAuthorized)
	}
	// COMMITTED STATE, not just the reason string: the provider was asked to do
	// nothing at all.
	if len(mut.calls) != 0 {
		t.Errorf("mutator recorded %d dispatches, want 0 — a delegation tier reached the tracker: %+v", len(mut.calls), mut.calls)
	}
	// THE SUGGESTION STAYS VISIBLE (#2855 AC2): the refusal is an audited skip
	// carrying every proposed label, not a filter that hides the defect.
	if !equalStrings(rec.After.List, []string{"area:backend", "autonomy:low", "phase:alpha"}) {
		t.Errorf("record After = %+v, want all three proposed labels surfaced on the refusal", rec.After)
	}
	// THE ONE BEHAVIOURAL COST, pinned rather than left to prose: the refusal
	// fails the WHOLE entry, so the clerical area:/phase: halves do not land
	// either. Same conservative direction as the invalid-label rule.
	if len(mut.calls) != 0 {
		t.Errorf("a mixed entry partially applied; the whole entry must be refused")
	}
}

// TestGroomingApply_DelegationTierLabelAppliedUnderPerEntryGateApproval proves
// this is a ROUTING change, not a blanket ban: an explicit per-entry gate
// approval — the grant #2843's per-entry disposition surface will supply — is a
// live authorization path that needs no further edit here.
//
// COUNTERFACTUAL: deleting the rule reddens this only VACUOUSLY, so the
// discriminating deletion is removing the `!req.GateApproved[c.entryID]`
// conjunct (the rule becomes unconditional and the applied assertion reddens),
// which is what proves the grant is a real path rather than dead code.
func TestGroomingApply_DelegationTierLabelAppliedUnderPerEntryGateApproval(t *testing.T) {
	e := tierHygieneEntry(1, "area:backend", "autonomy:low", "phase:alpha")
	rec, mut := applyTierEntry(t, e, GroomingModeAuto, map[string]bool{e.ID: true})

	if rec.Outcome != GroomingOutcomeApplied {
		t.Fatalf("record = %+v, want applied under an explicit per-entry gate approval", rec)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("mutator recorded %d dispatches, want exactly 1: %+v", len(mut.calls), mut.calls)
	}
	if !equalStrings(mut.calls[0].After.List, []string{"area:backend", "autonomy:low", "phase:alpha"}) {
		t.Errorf("dispatched labels = %+v, want the full proposed set", mut.calls[0].After)
	}
}

// TestGroomingApply_DelegationTierRefusalIsCaseInsensitive: a case-variant tier
// label is still a tier proposal. A case-sensitive check is evadable, and the
// direction an evasion fails in is a tier written with no human in the loop.
//
// COUNTERFACTUAL: delete the rule (or drop the ToLower from
// LabelsSetDelegationTier) and the entry dispatches.
func TestGroomingApply_DelegationTierRefusalIsCaseInsensitive(t *testing.T) {
	e := tierHygieneEntry(1, "Autonomy:Low")
	rec, mut := applyTierEntry(t, e, GroomingModeAuto, nil)

	if rec.Outcome != GroomingOutcomeSkipped || rec.SkipReason != GroomingSkipDelegationTierNotAuthorized {
		t.Fatalf("record = %+v, want skipped/%s for a case-variant tier label", rec, GroomingSkipDelegationTierNotAuthorized)
	}
	if len(mut.calls) != 0 {
		t.Errorf("mutator recorded %d dispatches, want 0 — `Autonomy:Low` evaded the tier rule: %+v", len(mut.calls), mut.calls)
	}
}

// TestGroomingApply_ReportModeBeatsDelegationTierRefusal is a PRECEDENCE claim:
// rule 8 sits AFTER rule 3, so a report-mode class still surfaces as
// mode_report_surface_only rather than reporting the tier reason. That is
// approval condition I1's ordering, undisturbed.
//
// COUNTERFACTUAL: deletion cannot discriminate a precedence claim (delete rule 8
// and report mode still short-circuits correctly, leaving this green). The
// discriminating change is a MOVE: relocate rule 8 ahead of the report-mode
// short-circuit and the UNGRANTED case reddens on the SkipReason.
//
// THE UNGRANTED CASE IS THE ONE THAT DISCRIMINATES, and it is why this test does
// not take the shape the plan sketched (report mode + a gate grant). With the
// gate GRANTED rule 8 never fires at all, so no placement of it is observable and
// the assertion is vacuous in exactly the way binding condition C1 called out on
// the basis test. The granted case is kept as the companion because it pins the
// ORIGINAL approval condition I1 claim — report mode beats an explicit gate
// approval — which is a different claim from rule 8's position.
func TestGroomingApply_ReportModeBeatsDelegationTierRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate map[string]bool
	}{
		// Rule 8 WOULD fire here. Report mode must reach its short-circuit first.
		{"no gate grant (discriminates rule 8's placement)", nil},
		// Rule 8 would not fire; this pins approval condition I1's own claim.
		{"gate granted (approval condition I1)", map[string]bool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tierHygieneEntry(1, "autonomy:low")
			gate := tc.gate
			if gate != nil {
				gate[e.ID] = true
			}
			rec, mut := applyTierEntry(t, e, GroomingModeReport, gate)

			if rec.SkipReason != GroomingSkipReportMode {
				t.Fatalf("SkipReason = %q, want %q — rule 8 must not preempt the report-mode short-circuit",
					rec.SkipReason, GroomingSkipReportMode)
			}
			if len(mut.calls) != 0 {
				t.Errorf("mutator recorded %d dispatches, want 0 under report mode", len(mut.calls))
			}
		})
	}
}

// TestGroomingApply_DelegationTierRefusalBeatsResolutionSkip is rule 8's OTHER
// placement claim — the half `..._ReportModeBeatsDelegationTierRefusal` does not
// reach. That test pins the rule's position AFTER rule 3; this one pins it
// BEFORE the resolution-skip return, so a containment refusal is never masked by
// an item-ref resolution skip and the audit row classifies the entry by the
// reason a human must act on.
//
// It matters for CLASSIFICATION, not for the write: both orderings dispatch
// nothing. But `item_ref_unresolvable` reads as a malformed proposal an operator
// can ignore, while `delegation_tier_not_authorized` is the tier decision #2843
// must route to a human — so a masked refusal loses the entry in the wrong
// audit bucket.
//
// COUNTERFACTUAL: deleting rule 8 cannot discriminate a precedence claim (the
// entry still skips, on the resolution reason). The discriminating change is a
// MOVE: relocate rule 8 below the `if reason != ""` return in
// settleGroomingCandidate and the tier sub-case reddens on the SkipReason.
//
// THE NON-TIER SUB-CASE IS THE FIXTURE CONTROL, and it is what stops the tier
// assertion being vacuous: it proves this ItemRef genuinely DOES produce a
// resolution skip. Without it, an ItemRef that quietly stayed resolvable would
// leave the tier assertion passing for the wrong reason — there would be no
// resolution skip for rule 8 to beat.
func TestGroomingApply_DelegationTierRefusalBeatsResolutionSkip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		want   string
	}{
		// The claim: the containment refusal wins over the resolution skip.
		{"tier proposal (discriminates rule 8's placement)", []string{"area:backend", "autonomy:low"}, GroomingSkipDelegationTierNotAuthorized},
		// The control: same unresolvable ref, no tier label — the resolution
		// skip is real and IS what surfaces when rule 8 does not fire.
		{"no tier proposal (fixture control)", []string{"area:backend"}, GroomingSkipItemRefUnresolvable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tierHygieneEntry(1, tc.labels...)
			// A ref whose number is not a number: resolveGroomingRequest returns
			// item_ref_unresolvable. The entry id was computed at construction
			// from the ORIGINAL ref, so the decision still joins and the entry
			// reaches the ladder.
			e.ItemRef.ID = "kuhlman-labs/fishhawk#not-a-number"
			rec, mut := applyTierEntry(t, e, GroomingModeAuto, nil)

			if rec.Outcome != GroomingOutcomeSkipped {
				t.Fatalf("record = %+v, want skipped", rec)
			}
			if rec.SkipReason != tc.want {
				t.Fatalf("SkipReason = %q, want %q", rec.SkipReason, tc.want)
			}
			// COMMITTED STATE: neither ordering may dispatch.
			if len(mut.calls) != 0 {
				t.Errorf("mutator recorded %d dispatches, want 0: %+v", len(mut.calls), mut.calls)
			}
		})
	}
}

// TestGroomingApply_NonDelegationLabelsUnaffected is the NARROWNESS control: an
// ordinary clerical label set still applies exactly as before. A future
// over-broad predicate (e.g. one widened to any `:`-bearing label) reddens here
// — the regression direction this change could plausibly take.
func TestGroomingApply_NonDelegationLabelsUnaffected(t *testing.T) {
	e := tierHygieneEntry(1, "area:backend", "phase:alpha")
	rec, mut := applyTierEntry(t, e, GroomingModeAuto, nil)

	if rec.Outcome != GroomingOutcomeApplied {
		t.Fatalf("record = %+v, want applied — rule 8 must not touch a non-tier label set", rec)
	}
	if len(mut.calls) != 1 {
		t.Fatalf("mutator recorded %d dispatches, want exactly 1: %+v", len(mut.calls), mut.calls)
	}
	if !equalStrings(mut.calls[0].After.List, []string{"area:backend", "phase:alpha"}) {
		t.Errorf("dispatched labels = %+v, want the clerical set unchanged", mut.calls[0].After)
	}
}

// TestGroomingApply_DelegationTierRefusalLeavesBaselineUnaffected pins the churn
// half of AC4 at the record level: the refused entry settles as `skipped`, never
// `applied`, so it never enters the churn guard's decided-and-applied baseline
// and correctly RESURFACES on the next grooming run for the human to decide.
func TestGroomingApply_DelegationTierRefusalLeavesBaselineUnaffected(t *testing.T) {
	e := tierHygieneEntry(1, "autonomy:medium")
	rec, _ := applyTierEntry(t, e, GroomingModeAuto, nil)

	if rec.Outcome == GroomingOutcomeApplied {
		t.Fatalf("record outcome = %q; a refused tier proposal recorded APPLIED would enter the churn baseline and be suppressed forever", rec.Outcome)
	}
	if rec.Outcome != GroomingOutcomeSkipped {
		t.Errorf("record outcome = %q, want %q", rec.Outcome, GroomingOutcomeSkipped)
	}
}

// TestLabelsSetDelegationTier tables the exported predicate itself, including
// the two boundaries the prose claims: a MALFORMED tier (`autonomy:critical`,
// which ParseAutonomyLabel normalizes to "" = non-human-led) is still a tier
// PROPOSAL and refused, and a namespace merely CONTAINING the word (`area:autonomy`)
// is not.
func TestLabelsSetDelegationTier(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		want   bool
	}{
		{"recognized tier", []string{"autonomy:low"}, true},
		{"malformed tier is still a tier proposal", []string{"autonomy:critical"}, true},
		{"leading whitespace", []string{" autonomy:low"}, true},
		{"upper case", []string{"AUTONOMY:HIGH"}, true},
		{"mixed set with a tier", []string{"area:backend", "autonomy:low", "phase:alpha"}, true},
		{"namespace containing the word", []string{"area:autonomy"}, false},
		{"no colon", []string{"autonomy"}, false},
		{"ordinary clerical label", []string{"phase:alpha"}, false},
		{"empty label", []string{""}, false},
		{"nil set", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LabelsSetDelegationTier(tc.labels); got != tc.want {
				t.Errorf("LabelsSetDelegationTier(%q) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Refused outcome (#2860)
// ---------------------------------------------------------------------------

// TestApplyGrooming_ProviderRefusalIsNotASkip is the #2860 audit done-means: a
// provider REFUSAL — a requested write that did not happen and left nothing
// correct behind — must be observable as its own outcome, not folded into the
// skipped bucket where it reads as benign idempotence.
//
// It asserts BOTH the count (bucketing happened) and that the entry ID SURVIVES
// into Summary.RefusedIDs (approval condition 2): a count alone does not name
// WHICH edges were refused, and not naming them is precisely why an 0/8 apply
// rate went unnoticed for three grooming walks.
func TestApplyGrooming_ProviderRefusalIsNotASkip(t *testing.T) {
	entry := hygieneEntry(1, "missing_estimate", "5")
	report := &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{entry}}
	mut := &fakeGroomingMutator{result: &GroomingMutationResult{
		Refused: true, RefuseReason: "not on board", ProviderResponse: "#1 carries no card",
	}}
	sink := &fakeGroomingSink{}
	res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, sink, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	if got := len(res.Refused); got != 1 {
		t.Fatalf("refused bucket has %d records, want 1 (result = %+v)", got, res)
	}
	if res.Summary.Refused != 1 || res.Summary.Skipped != 0 || res.Summary.Applied != 0 {
		t.Errorf("summary applied=%d skipped=%d refused=%d, want 0/0/1",
			res.Summary.Applied, res.Summary.Skipped, res.Summary.Refused)
	}
	// Approval condition 2: the ENTRY ID, not just the count.
	if len(res.Summary.RefusedIDs) != 1 || res.Summary.RefusedIDs[0] != entry.ID {
		t.Errorf("Summary.RefusedIDs = %q, want [%q]", res.Summary.RefusedIDs, entry.ID)
	}
	rec := recordFor(t, res, entry.ID)
	if rec.Outcome != GroomingOutcomeRefused {
		t.Errorf("outcome = %q, want %q", rec.Outcome, GroomingOutcomeRefused)
	}
	if rec.RefuseReason != "not on board" {
		t.Errorf("RefuseReason = %q, want %q", rec.RefuseReason, "not on board")
	}
	if rec.SkipReason != "" {
		t.Errorf("SkipReason = %q, want empty — a refusal is not a skip", rec.SkipReason)
	}
	if rec.ProviderResponse != "#1 carries no card" {
		t.Errorf("ProviderResponse = %q", rec.ProviderResponse)
	}
	// The refusal reaches the AUDIT, both per-mutation and in the summary.
	if len(sink.summaries) != 1 || sink.summaries[0].Refused != 1 ||
		len(sink.summaries[0].RefusedIDs) != 1 || sink.summaries[0].RefusedIDs[0] != entry.ID {
		t.Errorf("audited summary = %+v, want refused=1 with the entry id", sink.summaries)
	}
}

// TestApplyGrooming_MalformedResultFailsForEachRejectedCombination names ONE
// case per rejected (Applied, Skipped, Refused) combination — not the happy
// path plus a subset. Each must record FAILED with an error naming ALL THREE
// booleans, because the message is what an operator reads to tell which
// provider misreported.
func TestApplyGrooming_MalformedResultFailsForEachRejectedCombination(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  *GroomingMutationResult
	}{
		{"all false (zero value)", &GroomingMutationResult{}},
		{"applied+skipped", &GroomingMutationResult{Applied: true, Skipped: true}},
		{"applied+refused", &GroomingMutationResult{Applied: true, Refused: true}},
		{"skipped+refused", &GroomingMutationResult{Skipped: true, Refused: true}},
		{"all true", &GroomingMutationResult{Applied: true, Skipped: true, Refused: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := hygieneEntry(1, "missing_estimate", "5")
			report := &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{entry}}
			mut := &fakeGroomingMutator{result: tc.res}
			res, err := ApplyGrooming(context.Background(), mut, &fakeGroomingReader{}, &fakeGroomingSink{},
				GroomingApplyRequest{
					Target:    groomingTarget(),
					Report:    report,
					Decisions: approveAll(report),
					Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
					States:    groomingStates(),
				})
			if err != nil {
				t.Fatalf("ApplyGrooming: %v", err)
			}
			rec := recordFor(t, res, entry.ID)
			if rec.Outcome != GroomingOutcomeFailed {
				t.Fatalf("outcome = %q, want %q (record %+v)", rec.Outcome, GroomingOutcomeFailed, rec)
			}
			for _, want := range []string{
				fmt.Sprintf("applied=%t", tc.res.Applied),
				fmt.Sprintf("skipped=%t", tc.res.Skipped),
				fmt.Sprintf("refused=%t", tc.res.Refused),
				"exactly one must be true",
			} {
				if !strings.Contains(rec.Error, want) {
					t.Errorf("error %q does not name %q", rec.Error, want)
				}
			}
		})
	}
}

// TestApplyGrooming_ManualPlacementGuardIsRefusedNotSkipped pins the CORE's
// pre-dispatch placement guard. It is the SAME refusal the provider makes in
// its own defence-in-depth re-check, decided one layer earlier, and the two
// must agree in the audit — otherwise the outcome would depend on which layer
// happened to notice first.
func TestApplyGrooming_ManualPlacementGuardIsRefusedNotSkipped(t *testing.T) {
	entry := hygieneEntry(1, "unboarded", CanonicalStateBacklog)
	report := &plan.GroomingReport{HygieneDefects: []plan.HygieneDefect{entry}}
	// The card sits at a column OUTSIDE the expected source set: a human put it
	// there, so the move is declined.
	reader := &fakeGroomingReader{items: map[string]*WorkItemRecord{
		"#1": {Number: 1, State: "open", OnBoard: true, BoardColumn: "In Progress"},
	}}
	mut := &fakeGroomingMutator{}
	res, err := ApplyGrooming(context.Background(), mut, reader, &fakeGroomingSink{}, GroomingApplyRequest{
		Target:    groomingTarget(),
		Report:    report,
		Decisions: approveAll(report),
		Modes:     map[string]GroomingMode{"hygiene": GroomingModeAuto},
		States:    groomingStates(),
	})
	if err != nil {
		t.Fatalf("ApplyGrooming: %v", err)
	}
	rec := recordFor(t, res, entry.ID)
	if rec.Outcome != GroomingOutcomeRefused || rec.RefuseReason != GroomingSkipManualPlacementPreserved {
		t.Errorf("outcome=%q refuse_reason=%q skip_reason=%q, want refused/%s",
			rec.Outcome, rec.RefuseReason, rec.SkipReason, GroomingSkipManualPlacementPreserved)
	}
	if res.Summary.Refused != 1 || len(res.Summary.RefusedIDs) != 1 || res.Summary.RefusedIDs[0] != entry.ID {
		t.Errorf("summary refused=%d ids=%q, want 1 and [%q]", res.Summary.Refused, res.Summary.RefusedIDs, entry.ID)
	}
	if len(mut.calls) != 0 {
		t.Errorf("dispatched %v, want ZERO — the guard refuses BEFORE dispatch", mut.kinds())
	}
}

// TestNormalizeIssueRef_IsTheOnlyNormalizer states the SHARED-normalizer
// property #2860 turns on: one exported function decides the emitted ref SHAPE,
// named by both `workmgmt` and `workmgmt/github`. A second copy in either layer
// — even one believed to agree — would rebuild the two-layers-disagree defect
// one level down.
//
// The non-numeric rows are the discriminating ones: a normalizer that
// unconditionally prefixed `#` would map `owner/repo#123` and `#owner/repo#123`
// onto ONE value, so the layer that writes and the layer that reads would
// silently disagree about membership.
func TestNormalizeIssueRef_IsTheOnlyNormalizer(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1437", "#1437"},
		{"#1437", "#1437"},
		{"owner/repo#123", "owner/repo#123"},
		{"#owner/repo#123", "#owner/repo#123"},
		{"", ""},
		{"   ", ""},
		{"#", "#"},
	} {
		if got := NormalizeIssueRef(tc.in); got != tc.want {
			t.Errorf("NormalizeIssueRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if NormalizeIssueRef("1437") == NormalizeIssueRef("1438") {
		t.Error("normalization collapsed two different issue numbers")
	}
	if NormalizeIssueRef("owner/repo#123") == NormalizeIssueRef("#owner/repo#123") {
		t.Error("normalization collapsed two different non-numeric refs — the two-normalizer defect")
	}
}
