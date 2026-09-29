package digest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mkItem(seq int64) Item {
	return Item{Category: "merge_verdict_recorded", SourceSequence: seq, SourceEntryHash: strings.Repeat("a", 64),
		RunID: uuid.New(), At: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Outcome: "merged", Headline: "merge verdict: merged"}
}

// fakeSections builds a full digest over the given per-section item
// sequences, emulating Build for a section call: only items >= from.
func fakeDigest(section SectionKind, from int64, items map[SectionKind][]Item, gaps []Gap) Digest {
	d := Digest{Repo: testRepo, CaptainSubject: "cap", FromSequence: 1, ToSequence: 1000, ChainHead: 1000,
		Section: section, Sections: []Section{}, Gaps: []Gap{}, Degradations: []Degradation{}}
	for _, k := range ContentSections {
		if section != "" && section != k {
			continue
		}
		if section == SectionGaps {
			continue
		}
		s := Section{Kind: k, Items: []Item{}, Complete: true}
		for _, it := range items[k] {
			if it.SourceSequence >= from {
				s.Items = append(s.Items, it)
			}
		}
		d.Sections = append(d.Sections, s)
	}
	if section == "" || section == SectionGaps {
		for _, g := range gaps {
			if g.Sequence >= from {
				d.Gaps = append(d.Gaps, g)
			}
		}
	}
	return d
}

func marshalLen(t *testing.T, d Digest) int {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

// TestBound_MarksTruncationAndFitsBudget (failure mode 9): a digest several
// times over the budget comes back within it, marked truncated, with every
// cut section carrying OmittedCount and a cursor at its FIRST OMITTED item.
func TestBound_MarksTruncationAndFitsBudget(t *testing.T) {
	items := map[SectionKind][]Item{}
	for i := int64(1); i <= 40; i++ {
		items[SectionMerges] = append(items[SectionMerges], mkItem(i))
		items[SectionPages] = append(items[SectionPages], mkItem(100+i))
	}
	d := fakeDigest("", 0, items, nil)
	const budget = 4000
	if n := marshalLen(t, d); n < 3*budget {
		t.Fatalf("fixture is %d bytes, want several times the %d budget", n, budget)
	}
	out, err := Bound(d, budget)
	if err != nil {
		t.Fatal(err)
	}
	if n := marshalLen(t, out); n > budget {
		t.Errorf("bounded size %d > budget %d", n, budget)
	}
	if !out.Truncated || out.Next == nil {
		t.Errorf("truncated=%v next=%v, want marked", out.Truncated, out.Next)
	}
	for _, s := range out.Sections {
		orig := len(items[s.Kind])
		if len(s.Items) == orig {
			if !s.Complete || s.Next != nil {
				t.Errorf("%s untouched but marked %+v", s.Kind, s)
			}
			continue
		}
		if !s.Truncated || s.Complete || s.OmittedCount != orig-len(s.Items) || s.Next == nil {
			t.Fatalf("%s cut to %d/%d but markers %+v", s.Kind, len(s.Items), orig, s)
		}
		if want := items[s.Kind][len(s.Items)].SourceSequence; s.Next.FromSequence != want {
			t.Errorf("%s cursor from %d, want the first omitted item's %d", s.Kind, s.Next.FromSequence, want)
		}
		if !strings.Contains(s.Next.Call, "section="+string(s.Kind)) || !strings.Contains(s.Next.Call, "from_sequence=") {
			t.Errorf("%s cursor call %q does not name the next call", s.Kind, s.Next.Call)
		}
	}
}

// TestBound_UnderBudgetIsUnchanged: a digest that fits is returned whole and
// unmarked; the input value is not mutated by capping.
func TestBound_UnderBudgetIsUnchanged(t *testing.T) {
	d := fakeDigest("", 0, map[SectionKind][]Item{SectionMerges: {mkItem(1)}}, nil)
	out, err := Bound(d, DefaultByteBudget)
	if err != nil {
		t.Fatal(err)
	}
	if out.Truncated || len(sectionOf(t, out, SectionMerges).Items) != 1 {
		t.Errorf("out = %+v, want whole and unmarked", out)
	}
}

// TestBound_EmptyPageStillCarriesCursor (condition 1b): when the full-digest
// budget retains nothing of a section, the section still carries a cursor at
// its first item.
func TestBound_EmptyPageStillCarriesCursor(t *testing.T) {
	items := map[SectionKind][]Item{}
	for i := int64(1); i <= 30; i++ {
		items[SectionMerges] = append(items[SectionMerges], mkItem(i))
		items[SectionOpenDecisions] = append(items[SectionOpenDecisions], mkItem(200+i))
	}
	out, err := Bound(fakeDigest("", 0, items, nil), 3500)
	if err != nil {
		t.Fatal(err)
	}
	od := sectionOf(t, out, SectionOpenDecisions)
	if len(od.Items) != 0 || od.Next == nil || od.Next.FromSequence != 201 || od.OmittedCount != 30 {
		t.Errorf("open_decisions = %d items next %+v omitted %d, want an empty page with a cursor at 201", len(od.Items), od.Next, od.OmittedCount)
	}
}

// TestBound_OversizedFirstItemIsCappedAndReturned (condition 1): a section
// whose FIRST item alone is far over the budget is returned by a section call
// with its variable-length fields capped and marked, and the cursor chain
// terminates.
func TestBound_OversizedFirstItemIsCappedAndReturned(t *testing.T) {
	huge := mkItem(1)
	huge.Headline = strings.Repeat("hé\n", 20000)
	huge.ConcernCategory = strings.Repeat("c", 50000)
	all := []Item{huge, mkItem(2), mkItem(3)}
	const budget = 2500
	from, calls := int64(0), 0
	var got []int64
	for {
		calls++
		if calls > 10 {
			t.Fatal("cursor chain did not terminate")
		}
		out, err := Bound(fakeDigest(SectionMerges, from, map[SectionKind][]Item{SectionMerges: all}, nil), budget)
		if err != nil {
			t.Fatalf("call %d: %v", calls, err)
		}
		if n := marshalLen(t, out); n > budget {
			t.Fatalf("call %d size %d > %d", calls, n, budget)
		}
		s := sectionOf(t, out, SectionMerges)
		if len(s.Items) == 0 {
			t.Fatalf("call %d returned no item (condition 1c)", calls)
		}
		for _, it := range s.Items {
			if it.SourceSequence == 1 {
				if !it.FieldsTruncated || !strings.HasSuffix(it.Headline, "…") || jsonLen(it.Headline) > MaxFieldBytes {
					t.Errorf("oversized item not capped+marked: truncated=%v headline len %d", it.FieldsTruncated, jsonLen(it.Headline))
				}
			}
			got = append(got, it.SourceSequence)
		}
		if s.Next == nil {
			if !s.Complete {
				t.Fatal("no cursor but not complete")
			}
			break
		}
		if s.Next.FromSequence <= from {
			t.Fatalf("cursor %d did not advance past %d", s.Next.FromSequence, from)
		}
		from = s.Next.FromSequence
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("retrieved %v, want [1 2 3]", got)
	}
}

// TestBound_FollowingCursorTerminates (failure mode 10, pure): following the
// section cursor strictly advances, retrieves exactly the unbounded set, and
// ends within a bounded number of calls.
func TestBound_FollowingCursorTerminates(t *testing.T) {
	var all []Item
	for i := int64(1); i <= 50; i++ {
		all = append(all, mkItem(i*3))
	}
	src := map[SectionKind][]Item{SectionPages: all}
	var got []int64
	from, prev := int64(0), int64(-1)
	for calls := 0; ; calls++ {
		if calls > len(all) {
			t.Fatal("cursor chain did not terminate")
		}
		out, err := Bound(fakeDigest(SectionPages, from, src, nil), 1500)
		if err != nil {
			t.Fatal(err)
		}
		s := sectionOf(t, out, SectionPages)
		got = append(got, itemSeqs(s.Items)...)
		if s.Next == nil {
			break
		}
		if s.Next.FromSequence <= prev {
			t.Fatalf("from_sequence %d did not strictly increase past %d", s.Next.FromSequence, prev)
		}
		prev, from = s.Next.FromSequence, s.Next.FromSequence
	}
	if len(got) != len(all) {
		t.Fatalf("retrieved %d items, want %d", len(got), len(all))
	}
	for i := range all {
		if got[i] != all[i].SourceSequence {
			t.Fatalf("retrieved %v", got)
		}
	}
}

// TestBound_GapsAndDegradationsBounded: the gaps stream (sequenced gaps and
// degradations merged by sequence) is capped with its own markers and a gaps
// cursor, while sequence-less entries ride the floor untrimmed.
func TestBound_GapsAndDegradationsBounded(t *testing.T) {
	var gaps []Gap
	for i := int64(1); i <= 40; i++ {
		gaps = append(gaps, Gap{Kind: GapUnindexed, Sequence: i, Category: "concern_waived", Detail: strings.Repeat("d", 100)})
	}
	d := fakeDigest("", 0, nil, gaps)
	d.Gaps = append([]Gap{{Kind: GapParkedWithoutCitation, Detail: "floor"}}, d.Gaps...)
	d.Degradations = []Degradation{{Kind: DegradationScanLimit, Detail: "floor"}, {Kind: DegradationReasonMissing, Sequence: 5}}
	out, err := Bound(d, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if n := marshalLen(t, out); n > 3000 {
		t.Errorf("size %d > 3000", n)
	}
	if !out.GapsTruncated || out.GapsNext == nil || out.GapsNext.Section != SectionGaps || !out.Truncated {
		t.Fatalf("gaps markers = %v %+v", out.GapsTruncated, out.GapsNext)
	}
	if out.Gaps[0].Kind != GapParkedWithoutCitation || out.Degradations[0].Kind != DegradationScanLimit {
		t.Errorf("floor entries dropped: gaps[0]=%+v degradations[0]=%+v", out.Gaps[0], out.Degradations[0])
	}
	last := out.Gaps[len(out.Gaps)-1].Sequence
	if out.GapsNext.FromSequence != last+1 || out.GapsOmittedCount != 40-int(last) {
		t.Errorf("gaps cursor from %d omitted %d, want %d and %d", out.GapsNext.FromSequence, out.GapsOmittedCount, last+1, 40-last)
	}
}

// TestBound_BudgetTooSmall: a budget below the floor is refused, never
// returned over budget.
func TestBound_BudgetTooSmall(t *testing.T) {
	_, err := Bound(fakeDigest("", 0, map[SectionKind][]Item{SectionMerges: {mkItem(1)}}, nil), 50)
	if !errors.Is(err, ErrBudgetTooSmall) {
		t.Errorf("err = %v, want ErrBudgetTooSmall", err)
	}
}

// TestCapString pins the per-field cap: short strings pass untouched; long,
// escape-heavy and invalid-UTF-8 strings are cut to MaxFieldBytes ENCODED
// bytes and marked.
func TestCapString(t *testing.T) {
	if s, cut := capString("short"); s != "short" || cut {
		t.Errorf("short = %q %v", s, cut)
	}
	for name, in := range map[string]string{
		"ascii":   strings.Repeat("x", 1000),
		"escapes": strings.Repeat("\x01\"", 500),
		"runes":   strings.Repeat("é世", 500),
		"invalid": strings.Repeat("\xff", 1000),
	} {
		s, cut := capString(in)
		if !cut || jsonLen(s) > MaxFieldBytes || !strings.HasSuffix(s, "…") {
			t.Errorf("%s: cut=%v encoded len %d, want cut and <= %d", name, cut, jsonLen(s), MaxFieldBytes)
		}
	}
}

// TestParseSection covers the selector set, including gaps and a refused
// not-yet-shipped kind.
func TestParseSection(t *testing.T) {
	for _, ok := range []string{"", "merges", "waivers_and_deferrals", "pages", "open_decisions", "gaps"} {
		if _, err := ParseSection(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"doctrine_changes", "scheduled_runs", "x"} {
		if _, err := ParseSection(bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%q: err = %v, want ErrInvalidRequest", bad, err)
		}
	}
}

// TestBound_SectionCallNeverReturnsAnEmptyPage isolates the one-item floor
// (condition 1c): the budget fits the EMPTY section-call digest but not that
// digest plus its first item. Without the floor, Bound would return an empty
// page whose cursor names the same first item — a call that never progresses.
// With it, Bound refuses loudly instead.
func TestBound_SectionCallNeverReturnsAnEmptyPage(t *testing.T) {
	first := mkItem(7)
	first.Headline = strings.Repeat("h", 5000)
	d := fakeDigest(SectionMerges, 0, map[SectionKind][]Item{SectionMerges: {first}}, nil)
	empty := capDigest(d)
	empty.Sections[0].Items = []Item{}
	empty.Sections[0].Next = NewCursor(testRepo, SectionMerges, 7, 1000)
	empty.Sections[0].Truncated, empty.Sections[0].OmittedCount = true, 1
	empty.Truncated, empty.Next = true, empty.Sections[0].Next
	budget := marshalLen(t, empty) + 10
	if full := marshalLen(t, capDigest(d)); full <= budget {
		t.Fatalf("fixture: floor+item %d fits budget %d; the case isolates nothing", full, budget)
	}
	out, err := Bound(d, budget)
	if !errors.Is(err, ErrBudgetTooSmall) {
		t.Errorf("Bound = %d items (next %+v), err %v; want ErrBudgetTooSmall, never an empty section-call page",
			len(out.Sections[0].Items), out.Sections[0].Next, err)
	}
}
