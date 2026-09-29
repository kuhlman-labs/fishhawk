package digest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
)

// TestBuild_SectionsCiteChainEntries: a merge verdict and a waiver, both
// indexed, surface in their sections citing the source entry's sequence AND
// entry hash; the waiver carries its reason POINTER, and every item's
// source_sequence resolves to a real audit_entries row.
func TestBuild_SectionsCiteChainEntries(t *testing.T) {
	f := newFixture(t)
	stage := f.seedStage(t, f.run, 1, "implement", "succeeded")
	waive := f.appendEntry(t, f.run, &stage, "concern_waived", map[string]any{"reason": "r"})
	merge := f.appendEntry(t, f.run, nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	f.index(t, waive, decisionindex.ClassConcernWaive, nil)
	f.index(t, merge, decisionindex.ClassMergeVerdict, func(r *decisionindex.Row) { r.Outcome = "merged" })

	d := f.build(t, Request{})
	if len(d.Sections) != len(ContentSections) {
		t.Fatalf("sections = %d, want %d", len(d.Sections), len(ContentSections))
	}
	m := sectionOf(t, d, SectionMerges)
	if len(m.Items) != 1 || m.Items[0].SourceSequence != merge.Sequence || m.Items[0].SourceEntryHash != merge.EntryHash ||
		m.Items[0].Category != "merge_verdict_recorded" {
		t.Errorf("merges = %+v, want one item citing %d/%s", m.Items, merge.Sequence, merge.EntryHash)
	}
	w := sectionOf(t, d, SectionWaiversAndDeferrals)
	if len(w.Items) != 1 || w.Items[0].SourceSequence != waive.Sequence || w.Items[0].ReasonSequence != waive.Sequence ||
		w.Items[0].ReasonKey != "reason" {
		t.Errorf("waivers = %+v, want one item citing %d with reason pointer (%d, reason)", w.Items, waive.Sequence, waive.Sequence)
	}
	hashes, err := f.st.EntryHashes(context.Background(), []int64{m.Items[0].SourceSequence, w.Items[0].SourceSequence})
	if err != nil || len(hashes) != 2 {
		t.Errorf("cited sequences resolve to %d audit rows (%v), want 2", len(hashes), err)
	}
	if len(d.Gaps) != 0 || d.Truncated {
		t.Errorf("gaps = %+v truncated = %v, want none", d.Gaps, d.Truncated)
	}
}

// TestBuild_UnindexedEntryReportedAsGap (failure mode 6): a concern_waived
// entry appended through the RAW audit repository (no indexing decorator) is
// decision-bearing and unindexed BY CONSTRUCTION; it must appear in Gaps with
// its sequence, never be silently absent.
func TestBuild_UnindexedEntryReportedAsGap(t *testing.T) {
	f := newFixture(t)
	e := f.appendEntry(t, f.run, nil, "concern_waived", map[string]any{})
	d := f.build(t, Request{})
	if len(d.Gaps) != 1 || d.Gaps[0].Kind != GapUnindexed || d.Gaps[0].Sequence != e.Sequence || d.Gaps[0].Category != "concern_waived" {
		t.Errorf("gaps = %+v, want one %s gap at %d", d.Gaps, GapUnindexed, e.Sequence)
	}
}

// TestBuild_MissingSourceEntryReportedAsGap (failure mode 7): an index row
// citing a sequence with no audit_entries row yields a source_entry_missing gap
// AND the item is still emitted, marked SourceMissing.
func TestBuild_MissingSourceEntryReportedAsGap(t *testing.T) {
	f := newFixture(t)
	burned := f.burnSequence(t)
	anchor := f.appendEntry(t, f.run, nil, "run_started", map[string]any{})
	ghost := *anchor
	ghost.Sequence = burned
	f.index(t, &ghost, decisionindex.ClassMergeVerdict, nil)

	d := f.build(t, Request{})
	m := sectionOf(t, d, SectionMerges)
	if len(m.Items) != 1 || m.Items[0].SourceSequence != burned || !m.Items[0].SourceMissing {
		t.Errorf("merges = %+v, want the item at %d emitted with source_missing", m.Items, burned)
	}
	if len(d.Gaps) != 1 || d.Gaps[0].Kind != GapSourceEntryMissing || d.Gaps[0].Sequence != burned {
		t.Errorf("gaps = %+v, want one %s gap at %d", d.Gaps, GapSourceEntryMissing, burned)
	}
}

// TestBuild_SourceHashMismatchReportedAsGap: an index row whose cited hash
// differs from the chain entry's is a gap, never silently trusted.
func TestBuild_SourceHashMismatchReportedAsGap(t *testing.T) {
	f := newFixture(t)
	e := f.mergeEntry(t)
	f.index(t, e, decisionindex.ClassMergeVerdict, func(r *decisionindex.Row) { r.SourceEntryHash = "tampered" })
	d := f.build(t, Request{})
	if len(d.Gaps) != 1 || d.Gaps[0].Kind != GapSourceHashMismatch {
		t.Errorf("gaps = %+v, want one %s gap", d.Gaps, GapSourceHashMismatch)
	}
}

// TestBuild_ReasonMissingDegradation: a waiver row with no reason pointer is
// emitted beside a named reason_missing degradation, not a silently blank
// reason. Both arms (sequence 0, key empty) are exercised.
func TestBuild_ReasonMissingDegradation(t *testing.T) {
	f := newFixture(t)
	a := f.appendEntry(t, f.run, nil, "concern_waived", map[string]any{})
	b := f.appendEntry(t, f.run, nil, "concern_deferred", map[string]any{})
	f.index(t, a, decisionindex.ClassConcernWaive, func(r *decisionindex.Row) { r.ReasonKey = "" })
	f.index(t, b, decisionindex.ClassConcernDefer, func(r *decisionindex.Row) { r.ReasonSequence = 0 })
	d := f.build(t, Request{})
	var got []int64
	for _, dg := range d.Degradations {
		if dg.Kind == DegradationReasonMissing {
			got = append(got, dg.Sequence)
		}
	}
	if len(got) != 2 || got[0] != a.Sequence || got[1] != b.Sequence {
		t.Errorf("reason_missing degradations at %v, want [%d %d]", got, a.Sequence, b.Sequence)
	}
	if w := sectionOf(t, d, SectionWaiversAndDeferrals); len(w.Items) != 2 {
		t.Errorf("waivers = %d items, want 2 (still emitted)", len(w.Items))
	}
}

// TestBuild_ScanLimitAppendsDegradation (failure mode 8): more parked stages
// than the scan limit produce a named scan_limit degradation, a truncated
// section and a cursor at the FIRST OMITTED item — not a silently short list.
func TestBuild_ScanLimitAppendsDegradation(t *testing.T) {
	f := newFixture(t)
	var parks []int64
	for i := 0; i < 3; i++ {
		s := f.seedStage(t, f.run, i, "plan", "awaiting_approval")
		parks = append(parks, f.appendEntry(t, f.run, &s, "plan_generated", map[string]any{}).Sequence)
	}
	d := f.build(t, Request{ScanLimit: 2})
	od := sectionOf(t, d, SectionOpenDecisions)
	if len(od.Items) != 2 || !od.Truncated || od.Next == nil || od.Next.FromSequence != parks[2] {
		t.Fatalf("open_decisions = %+v (next %+v), want 2 items truncated with cursor at %d", od.Items, od.Next, parks[2])
	}
	found := false
	for _, dg := range d.Degradations {
		found = found || (dg.Kind == DegradationScanLimit && dg.Section == SectionOpenDecisions)
	}
	if !found {
		t.Errorf("degradations = %+v, want a scan_limit degradation on open_decisions", d.Degradations)
	}
	rest := f.build(t, Request{Section: SectionOpenDecisions, FromSequence: od.Next.FromSequence, ToSequence: od.Next.ToSequence, ScanLimit: 2})
	if got := itemSeqs(sectionOf(t, rest, SectionOpenDecisions).Items); len(got) != 1 || got[0] != parks[2] {
		t.Errorf("following the cursor returned %v, want [%d]", got, parks[2])
	}
}

// TestBuild_OpenDecisionCitesParkingEntry (condition 3): a parked stage cites
// the LATEST entry of its state's parking categories on that stage — not a
// later unrelated entry, not an earlier parking entry — and a parked stage with
// no identifiable parking entry is a parked_without_citation gap, never an item.
func TestBuild_OpenDecisionCitesParkingEntry(t *testing.T) {
	f := newFixture(t)
	plan := f.seedStage(t, f.run, 0, "plan", "awaiting_approval")
	f.appendEntry(t, f.run, &plan, "plan_generated", map[string]any{})
	latest := f.appendEntry(t, f.run, &plan, "plan_generated", map[string]any{})
	f.appendEntry(t, f.run, &plan, "run_started", map[string]any{}) // not a parking category
	input := f.seedStage(t, f.run, 1, "implement", "awaiting_input")
	f.appendEntry(t, f.run, &input, "plan_generated", map[string]any{}) // wrong category for awaiting_input
	deploy := f.seedStage(t, f.run, 2, "deploy", "awaiting_deploy_approval")
	// A parked stage on a TERMINAL run is not an open decision.
	done := f.seedRun(t, testRepo, "succeeded")
	stale := f.seedStage(t, done, 0, "plan", "awaiting_approval")
	f.appendEntry(t, done, &stale, "plan_generated", map[string]any{})

	d := f.build(t, Request{})
	od := sectionOf(t, d, SectionOpenDecisions)
	if len(od.Items) != 1 {
		t.Fatalf("open_decisions = %+v, want exactly the plan stage", od.Items)
	}
	it := od.Items[0]
	if it.SourceSequence != latest.Sequence || it.SourceEntryHash != latest.EntryHash || it.Category != "plan_generated" ||
		*it.StageID != plan || it.StageState != "awaiting_approval" {
		t.Errorf("item = %+v, want the plan stage citing its LATEST plan_generated %d", it, latest.Sequence)
	}
	uncited := map[uuid.UUID]bool{}
	for _, g := range d.Gaps {
		if g.Kind == GapParkedWithoutCitation && g.Sequence == 0 {
			uncited[*g.StageID] = true
		}
	}
	if len(uncited) != 2 || !uncited[input] || !uncited[deploy] {
		t.Errorf("parked_without_citation gaps = %v, want the awaiting_input and awaiting_deploy_approval stages", uncited)
	}
}

// TestBuild_PageAnsweredIsPaired (condition 4): a page is answered ONLY by its
// paired entry. An unrelated later decision on the run (and a same-category
// answer on another stage / another amendment) does NOT answer it.
func TestBuild_PageAnsweredIsPaired(t *testing.T) {
	f := newFixture(t)
	s1 := f.seedStage(t, f.run, 0, "plan", "succeeded")
	s2 := f.seedStage(t, f.run, 1, "implement", "succeeded")
	clar := f.appendEntry(t, f.run, &s1, "clarification_requested", map[string]any{})
	amendA := uuid.NewString()
	amend := f.appendEntry(t, f.run, &s2, "scope_amendment_requested", map[string]any{"amendment_id": amendA})
	esc := f.appendEntry(t, f.run, &s2, "escalation_fired", map[string]any{})
	// Unrelated later decisions: an approval on ANOTHER stage than the
	// clarification, a clarification answer on the WRONG stage, and a decision
	// on a DIFFERENT amendment.
	f.appendEntry(t, f.run, &s2, "clarification_answered", map[string]any{})
	f.appendEntry(t, f.run, &s1, "approval_submitted", map[string]any{})
	f.appendEntry(t, f.run, &s2, "scope_amendment_decided", map[string]any{"amendment_id": uuid.NewString()})

	answered := func(d Digest) map[int64]int64 {
		out := map[int64]int64{}
		for _, it := range sectionOf(t, d, SectionPages).Items {
			if it.Answered == nil {
				t.Fatalf("page item %d has no answered flag", it.SourceSequence)
			}
			if *it.Answered {
				out[it.SourceSequence] = it.AnsweredSequence
			} else {
				out[it.SourceSequence] = 0
			}
		}
		return out
	}
	got := answered(f.build(t, Request{}))
	if len(got) != 3 || got[clar.Sequence] != 0 || got[amend.Sequence] != 0 || got[esc.Sequence] != 0 {
		t.Fatalf("answered = %v, want all three pages unanswered by the unrelated decisions", got)
	}

	ansClar := f.appendEntry(t, f.run, &s1, "clarification_answered", map[string]any{})
	ansAmend := f.appendEntry(t, f.run, &s1, "scope_amendment_decided", map[string]any{"amendment_id": amendA})
	ansEsc := f.appendEntry(t, f.run, &s2, "approval_submitted", map[string]any{})
	got = answered(f.build(t, Request{}))
	if got[clar.Sequence] != ansClar.Sequence || got[amend.Sequence] != ansAmend.Sequence || got[esc.Sequence] != ansEsc.Sequence {
		t.Errorf("answered = %v, want clarification→%d amendment→%d escalation→%d",
			got, ansClar.Sequence, ansAmend.Sequence, ansEsc.Sequence)
	}
	// An answer ABOVE to_sequence does not count: the digest is a snapshot.
	got = answered(f.build(t, Request{ToSequence: ansClar.Sequence - 1}))
	if got[clar.Sequence] != 0 {
		t.Errorf("answered at to_sequence %d = %v, want the clarification unanswered", ansClar.Sequence-1, got)
	}
}

// TestBuild_DefaultWindowStartsAfterWatermark: content before the watermark is
// excluded by default, while a gate parked before it is still an open
// decision.
func TestBuild_DefaultWindowStartsAfterWatermark(t *testing.T) {
	f := newFixture(t)
	plan := f.seedStage(t, f.run, 0, "plan", "awaiting_approval")
	park := f.appendEntry(t, f.run, &plan, "plan_generated", map[string]any{})
	old := f.mergeEntry(t)
	f.index(t, old, decisionindex.ClassMergeVerdict, nil)
	if err := f.st.UpsertWatermark(context.Background(), nil, "captain-a", testRepo, old.Sequence); err != nil {
		t.Fatal(err)
	}
	fresh := f.mergeEntry(t)
	f.index(t, fresh, decisionindex.ClassMergeVerdict, nil)

	d := f.build(t, Request{})
	if d.FromSequence != old.Sequence+1 || d.ToSequence != fresh.Sequence || !d.HasWatermark || d.Watermark != old.Sequence {
		t.Errorf("window = (%d..%d] wm=%d/%v, want from %d to %d", d.FromSequence, d.ToSequence, d.Watermark, d.HasWatermark, old.Sequence+1, fresh.Sequence)
	}
	if got := itemSeqs(sectionOf(t, d, SectionMerges).Items); len(got) != 1 || got[0] != fresh.Sequence {
		t.Errorf("merges = %v, want only [%d]", got, fresh.Sequence)
	}
	if got := itemSeqs(sectionOf(t, d, SectionOpenDecisions).Items); len(got) != 1 || got[0] != park.Sequence {
		t.Errorf("open_decisions = %v, want the pre-watermark park [%d]", got, park.Sequence)
	}
}

// TestBuild_BeyondChainHeadRefused: a to_sequence above the head is refused
// with a typed error naming the head.
func TestBuild_BeyondChainHeadRefused(t *testing.T) {
	f := newFixture(t)
	e := f.appendEntry(t, f.run, nil, "run_started", map[string]any{})
	_, err := Build(context.Background(), f.deps(), Request{Repo: testRepo, CaptainSubject: "c", ToSequence: e.Sequence + 5})
	var beyond *BeyondChainHeadError
	if !errors.As(err, &beyond) || beyond.Head != e.Sequence {
		t.Errorf("err = %v, want *BeyondChainHeadError naming head %d", err, e.Sequence)
	}
}

// TestBuild_RejectsInvalidRequest covers each validation branch.
func TestBuild_RejectsInvalidRequest(t *testing.T) {
	f := newFixture(t)
	for name, req := range map[string]Request{
		"no repo":       {CaptainSubject: "c"},
		"no subject":    {Repo: testRepo},
		"bad section":   {Repo: testRepo, CaptainSubject: "c", Section: "doctrine_changes"},
		"negative from": {Repo: testRepo, CaptainSubject: "c", FromSequence: -1},
	} {
		if _, err := Build(context.Background(), f.deps(), req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// TestBuild_EmptyChainIsEmpty: a repository with no entries yields empty
// sections, not an unbounded read (0 must never reach a query as "no bound").
func TestBuild_EmptyChainIsEmpty(t *testing.T) {
	f := newFixture(t)
	d := f.build(t, Request{Repo: "acme/empty"})
	for _, s := range d.Sections {
		if len(s.Items) != 0 || !s.Complete {
			t.Errorf("section %s = %+v, want empty and complete", s.Kind, s)
		}
	}
}

// TestBuild_DoesNotAdvanceWatermark: three successive builds leave the stored
// watermark unchanged (retrieval never writes).
func TestBuild_DoesNotAdvanceWatermark(t *testing.T) {
	f := newFixture(t)
	first := f.appendEntry(t, f.run, nil, "run_started", map[string]any{})
	if err := f.st.UpsertWatermark(context.Background(), nil, "captain-a", testRepo, first.Sequence); err != nil {
		t.Fatal(err)
	}
	f.appendEntry(t, f.run, nil, "run_started", map[string]any{})
	for i := 0; i < 3; i++ {
		f.build(t, Request{})
	}
	got, _, err := f.st.GetWatermark(context.Background(), nil, "captain-a", testRepo)
	if err != nil || got != first.Sequence {
		t.Errorf("watermark after three builds = %d (%v), want %d", got, err, first.Sequence)
	}
}

// TestWatermark_TwoCaptainsAreIndependent: advancing captain A's watermark
// leaves captain B's digest byte-identical.
func TestWatermark_TwoCaptainsAreIndependent(t *testing.T) {
	f := newFixture(t)
	e := f.mergeEntry(t)
	f.index(t, e, decisionindex.ClassMergeVerdict, nil)
	before, _ := json.Marshal(f.build(t, Request{CaptainSubject: "captain-b"}))
	if _, err := f.st.MarkRead(context.Background(), &recordingAppender{}, MarkReadParams{
		CaptainSubject: "captain-a", Repo: testRepo, ToSequence: e.Sequence,
	}); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(f.build(t, Request{CaptainSubject: "captain-b"}))
	if string(before) != string(after) {
		t.Errorf("captain B's digest changed after captain A marked read:\n before %s\n after  %s", before, after)
	}
	if a := f.build(t, Request{CaptainSubject: "captain-a"}); len(sectionOf(t, a, SectionMerges).Items) != 0 {
		t.Errorf("captain A still sees the merge after marking read")
	}
}

// TestBuild_FollowingCursorTerminates (failure mode 10, against the real
// Build): repeatedly re-Build from the returned cursor under a small budget;
// each FromSequence strictly increases, the union equals the unbounded set,
// and the loop ends within a bounded number of iterations.
func TestBuild_FollowingCursorTerminates(t *testing.T) {
	f := newFixture(t)
	var want []int64
	for i := 0; i < 12; i++ {
		e := f.mergeEntry(t)
		f.index(t, e, decisionindex.ClassMergeVerdict, nil)
		want = append(want, e.Sequence)
	}
	const budget = 3000
	d, err := Bound(f.build(t, Request{}), budget)
	if err != nil {
		t.Fatal(err)
	}
	m := sectionOf(t, d, SectionMerges)
	got := itemSeqs(m.Items)
	next, prev := m.Next, int64(0)
	for i := 0; next != nil; i++ {
		if i > len(want) {
			t.Fatalf("cursor chain did not terminate within %d calls", len(want))
		}
		if next.FromSequence <= prev {
			t.Fatalf("cursor from_sequence %d did not strictly advance past %d", next.FromSequence, prev)
		}
		prev = next.FromSequence
		page, err := Bound(f.build(t, Request{Section: next.Section, FromSequence: next.FromSequence, ToSequence: next.ToSequence}), budget)
		if err != nil {
			t.Fatal(err)
		}
		s := sectionOf(t, page, SectionMerges)
		if len(s.Items) == 0 && s.Next != nil {
			t.Fatalf("a cursor-following call returned no item yet claimed more (condition 1c)")
		}
		got = append(got, itemSeqs(s.Items)...)
		next = s.Next
	}
	if len(got) != len(want) {
		t.Fatalf("retrieved %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("retrieved %v, want %v", got, want)
		}
	}
	if m.Next == nil {
		t.Errorf("budget %d did not truncate the first page; the test exercised no cursor", budget)
	}
}

// TestBuild_FollowingGapsCursorReturnsRest (condition 2): the gaps collection
// is addressable with section=gaps and following its pointer returns the rest.
func TestBuild_FollowingGapsCursorReturnsRest(t *testing.T) {
	f := newFixture(t)
	var want []int64
	for i := 0; i < 10; i++ {
		want = append(want, f.appendEntry(t, f.run, nil, "concern_waived", map[string]any{}).Sequence)
	}
	const budget = 2200
	d, err := Bound(f.build(t, Request{}), budget)
	if err != nil {
		t.Fatal(err)
	}
	if d.GapsNext == nil || !d.GapsTruncated {
		t.Fatalf("budget %d did not truncate gaps (%d gaps); the test exercised no cursor", budget, len(d.Gaps))
	}
	var got []int64
	for _, g := range d.Gaps {
		got = append(got, g.Sequence)
	}
	for next, i := d.GapsNext, 0; next != nil; i++ {
		if i > len(want) {
			t.Fatal("gaps cursor chain did not terminate")
		}
		if next.Section != SectionGaps {
			t.Fatalf("gaps cursor section = %q", next.Section)
		}
		page, err := Bound(f.build(t, Request{Section: SectionGaps, FromSequence: next.FromSequence, ToSequence: next.ToSequence}), budget)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Sections) != 0 {
			t.Fatalf("gaps selector returned content sections %+v", page.Sections)
		}
		if len(page.Gaps) == 0 {
			t.Fatal("a gaps-cursor call returned no gap")
		}
		for _, g := range page.Gaps {
			got = append(got, g.Sequence)
		}
		next = page.GapsNext
	}
	if len(got) != len(want) {
		t.Fatalf("retrieved gaps %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("retrieved gaps %v, want %v", got, want)
		}
	}
}
