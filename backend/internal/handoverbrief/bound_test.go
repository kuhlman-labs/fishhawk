package handoverbrief

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
)

// synthetic builds a brief with n elements in every collection, hashed the way
// Compose hashes it.
func synthetic(n int) Brief {
	at := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	items := func(base int64) []digest.Item {
		out := []digest.Item{}
		for i := 0; i < n; i++ {
			out = append(out, digest.Item{Category: "merge_verdict_recorded", SourceSequence: base + int64(i),
				SourceEntryHash: strings.Repeat("a", 64), RunID: uuid.New(), At: at, Headline: "merge verdict: merged"})
		}
		return out
	}
	var fl []InFlightItem
	for i := 0; i < n; i++ {
		fl = append(fl, InFlightItem{Kind: "campaign", ID: uuid.New(), State: "running", Ref: "issue:1", CreatedAt: at})
	}
	var ws []DelegationEntry
	for i := 0; i < n; i++ {
		ws = append(ws, DelegationEntry{WorkflowID: "wf", Autonomy: "high", ContentHash: strings.Repeat("b", 64), Confirmation: ConfirmationUnavailable})
	}
	var gaps []digest.Gap
	for i := 0; i < n; i++ {
		gaps = append(gaps, digest.Gap{Kind: digest.GapUnindexed, Sequence: 500 + int64(i), Category: "concern_waived"})
	}
	b := Brief{
		Repo: testRepo, Window: Window{FromSequence: 1, ToSequence: 900, ChainHead: 900, Basis: BasisFirstCaptain},
		Sections: []Section{
			{Kind: SectionWhatChanged, Parts: []Part{{Kind: PartMerges, Items: items(10), Complete: true}}},
			{Kind: SectionInFlight, Parts: []Part{{Kind: PartCampaigns, InFlight: fl, Complete: true}}},
			{Kind: SectionDelegationInForce, Parts: []Part{{Kind: PartWorkflows, Workflows: ws, Complete: true}}},
		},
		Absent: DeclaredAbsences(), Gaps: gaps,
		Degradations: []Degradation{{Kind: DegradationDelegationUnavailable, Detail: "fixed"}},
	}
	b.BriefHash = Hash(b)
	return b
}

func size(t *testing.T, b Brief) int {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

func TestBound_FitsReturnsWhole(t *testing.T) {
	b := synthetic(3)
	out, err := Bound(b, DefaultByteBudget)
	if err != nil {
		t.Fatal(err)
	}
	if out.Truncated || out.BriefHash != b.BriefHash || len(out.Sections[0].Parts[0].Items) != 3 {
		t.Errorf("bound of a fitting brief changed it: truncated=%v hash=%s", out.Truncated, out.BriefHash)
	}
}

// TestBound_FloorTooSmallIsRefused: a floor over budget is ErrBudgetTooSmall
// and NO value is returned — never an over-budget or empty non-advancing one.
func TestBound_FloorTooSmallIsRefused(t *testing.T) {
	out, err := Bound(synthetic(2), 64)
	if !errors.Is(err, ErrBudgetTooSmall) {
		t.Fatalf("err = %v, want ErrBudgetTooSmall", err)
	}
	if out.Repo != "" || out.BriefHash != "" {
		t.Errorf("refusal returned a value: %+v", out)
	}
}

// TestBound_DescendingBudgetsAlwaysProgress: at every budget the result fits,
// keeps the canonical hash, and for a selected section either returns at
// least one element or reports the collection complete; each cut collection's
// cursor names the underlying query at the first omitted element.
func TestBound_DescendingBudgetsAlwaysProgress(t *testing.T) {
	full := synthetic(20)
	sel, err := Select(full, SectionWhatChanged)
	if err != nil {
		t.Fatal(err)
	}
	fullSize := size(t, sel)
	sawCut := false
	for budget := fullSize; budget > 0; budget -= 97 {
		out, err := Bound(sel, budget)
		if errors.Is(err, ErrBudgetTooSmall) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := size(t, out); got > budget {
			t.Fatalf("budget %d: result is %d bytes", budget, got)
		}
		if out.BriefHash != full.BriefHash {
			t.Fatalf("budget %d: hash re-computed", budget)
		}
		p := out.Sections[0].Parts[0]
		if len(p.Items) == 0 && !p.Complete {
			t.Fatalf("budget %d: non-advancing page (no items, incomplete)", budget)
		}
		if p.Truncated {
			sawCut = true
			first := sel.Sections[0].Parts[0].Items[len(p.Items)].SourceSequence
			want := digest.NewCursor(testRepo, digest.SectionMerges, first, 900).Call
			if p.Next == nil || p.Next.Call != want || p.OmittedCount != 20-len(p.Items) || !out.Truncated {
				t.Fatalf("budget %d: part %+v, want cursor %q", budget, p, want)
			}
		}
	}
	if !sawCut {
		t.Fatal("no budget cut the section; the progress assertion is vacuous")
	}
}

// TestBound_CursorsNameUnderlyingQueries: campaigns, workflows and the gaps
// stream each get a cursor naming its own underlying read.
func TestBound_CursorsNameUnderlyingQueries(t *testing.T) {
	b := synthetic(30)
	// Empty the merges so the budget cuts the later collections.
	b.Sections[0].Parts[0].Items = []digest.Item{}
	floor, err := Bound(b, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Bound(b, size(t, floor)/3)
	if err != nil {
		t.Fatal(err)
	}
	camp, wf := out.Sections[1].Parts[0], out.Sections[2].Parts[0]
	if !camp.Truncated || camp.Next == nil || !strings.HasPrefix(camp.Next.Call, "GET /v0/campaigns?") || camp.Next.Offset != len(camp.InFlight) {
		t.Errorf("campaigns cursor = %+v (kept %d)", camp.Next, len(camp.InFlight))
	}
	if !wf.Truncated || wf.Next == nil || wf.Next.Call != "GET /v0/repos/"+testRepo+"/delegation" {
		t.Errorf("workflows cursor = %+v", wf.Next)
	}
	if !out.GapsTruncated || out.GapsNext == nil || !strings.HasPrefix(out.GapsNext.Call, "GET /v0/digest?") ||
		!strings.Contains(out.GapsNext.Call, "section=gaps") {
		t.Errorf("gaps cursor = %+v", out.GapsNext)
	}
	if out.Next == nil || !out.Truncated {
		t.Error("top-level truncation markers not set")
	}
	if len(out.Absent) != 3 || len(out.Degradations) == 0 {
		t.Error("floor dropped the declared absences or fixed degradations")
	}
}

func TestBound_CapsFieldsAndDoesNotMutateInput(t *testing.T) {
	b := synthetic(1)
	long := strings.Repeat("x", 4*MaxFieldBytes)
	b.Sections[0].Parts[0].Items[0].Headline = long
	b.Sections[1].Parts[0].InFlight[0].Ref = long
	b.Sections[2].Parts[0].Workflows[0].WorkflowID = long
	out, err := Bound(b, DefaultByteBudget)
	if err != nil {
		t.Fatal(err)
	}
	it := out.Sections[0].Parts[0].Items[0]
	if !it.FieldsTruncated || jsonLen(it.Headline) > MaxFieldBytes || !strings.HasSuffix(it.Headline, "…") {
		t.Errorf("headline not capped: %d bytes", jsonLen(it.Headline))
	}
	if fl := out.Sections[1].Parts[0].InFlight[0]; !fl.FieldsTruncated || jsonLen(fl.Ref) > MaxFieldBytes {
		t.Error("in-flight ref not capped")
	}
	if w := out.Sections[2].Parts[0].Workflows[0]; !w.FieldsTruncated || jsonLen(w.WorkflowID) > MaxFieldBytes {
		t.Error("workflow id not capped")
	}
	if b.Sections[0].Parts[0].Items[0].Headline != long {
		t.Error("Bound mutated the caller's brief")
	}
	if out.BriefHash != b.BriefHash {
		t.Error("capping re-hashed the brief")
	}
}
