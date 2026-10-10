package handoverbrief

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
)

// detID is a deterministic id, so synthetic's canonical bytes (and hash) are a
// function of n alone — TestHash_IgnoresContinuations pins that hash.
func detID(kind string, i int64) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(kind+"/"+strconv.FormatInt(i, 10)))
}

// synthetic builds a brief with n elements in every collection, hashed the way
// Compose hashes it.
func synthetic(n int) Brief {
	at := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	items := func(base int64) []digest.Item {
		out := []digest.Item{}
		for i := 0; i < n; i++ {
			out = append(out, digest.Item{Category: "merge_verdict_recorded", SourceSequence: base + int64(i),
				SourceEntryHash: strings.Repeat("a", 64), RunID: detID("run", base+int64(i)), At: at, Headline: "merge verdict: merged"})
		}
		return out
	}
	var fl []InFlightItem
	for i := 0; i < n; i++ {
		fl = append(fl, InFlightItem{Kind: "campaign", ID: detID("campaign", int64(i)), State: "running", Ref: "issue:1", CreatedAt: at})
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
			{Kind: SectionStandingOrders, Parts: []Part{}, StandingOrders: &StandingOrders{
				Source: "run_cache", WorkflowSHA: strings.Repeat("c", 40), SpecVersion: "2", SchemaMajor: 2,
				DelegationContentHash: strings.Repeat("b", 64)}},
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

// TestBound_DelegationCursorNamesComposedSource: the workflows cursor carries
// the ?source the brief's standing orders recorded, because the delegation
// route DEFAULTS to source=ref — a run_cache brief whose cursor named the bare
// path would point at a different read (and can 502 forge_unavailable where
// the composed read succeeded). No standing orders (delegation unavailable) =
// no source to name, so the bare path stands.
func TestBound_DelegationCursorNamesComposedSource(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"run_cache", "GET /v0/repos/" + testRepo + "/delegation?source=run_cache"},
		{"ref", "GET /v0/repos/" + testRepo + "/delegation?source=ref"},
		{"", "GET /v0/repos/" + testRepo + "/delegation"},
	} {
		t.Run("source="+tc.source, func(t *testing.T) {
			b := synthetic(30)
			b.Sections[0].Parts[0].Items = []digest.Item{}
			for i := range b.Sections {
				if b.Sections[i].Kind != SectionStandingOrders {
					continue
				}
				if tc.source == "" {
					b.Sections[i].StandingOrders, b.Sections[i].Unavailable = nil, true
					continue
				}
				b.Sections[i].StandingOrders.Source = tc.source
			}
			b.BriefHash = Hash(b)
			floor, err := Bound(b, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			out, err := Bound(b, size(t, floor)/3)
			if err != nil {
				t.Fatal(err)
			}
			wf := out.Sections[2].Parts[0]
			if !wf.Truncated {
				t.Fatalf("workflows not cut (kept %d); the cursor assertion is vacuous", len(wf.Workflows))
			}
			if wf.Next == nil || wf.Next.Call != tc.want {
				t.Errorf("workflows cursor = %+v, want call %q", wf.Next, tc.want)
			}
		})
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
	// The cursor must name the query the brief ACTUALLY read: the route
	// defaults to source=ref, so a run_cache composition names ?source=run_cache.
	if !wf.Truncated || wf.Next == nil || wf.Next.Call != "GET /v0/repos/"+testRepo+"/delegation?source=run_cache" {
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

// ---- per-state in_flight continuations (#3862) ----

// longRef pads an in-flight item past a cursor's size, so a render keeping
// more rows is always larger than one keeping fewer (and budgetKeeping's
// search is monotone).
var longRef = strings.Repeat("r", 200)

// backingStore simulates the per-state list queries an in_flight part reads:
// each state's rows in the list routes' order.
type backingStore map[string][]uuid.UUID

// inFlightPart builds an in_flight part exactly as Compose builds it over
// store rows: states in inFlightStates order, at most limit rows per state,
// and one scan-limit continuation per overflowing state (Next the first).
func inFlightPart(kind PartKind, rowsPerState map[string]int, limit int) (Part, backingStore) {
	itemKind := "run"
	if kind == PartCampaigns {
		itemKind = "campaign"
	}
	at := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	store := backingStore{}
	p := Part{Kind: kind, InFlight: []InFlightItem{}, Complete: true}
	for _, st := range inFlightStates(kind) {
		for i := 0; i < rowsPerState[st]; i++ {
			store[st] = append(store[st], detID(string(kind)+"/"+st, int64(i)))
		}
		rows := store[st]
		if len(rows) > limit {
			rows = rows[:limit]
			p.Complete, p.Truncated = false, true
			p.OmittedCount++
			p.Continuations = append(p.Continuations, *listCursor(testRepo, kind, st, limit))
		}
		for _, id := range rows {
			p.InFlight = append(p.InFlight, InFlightItem{Kind: itemKind, ID: id, State: st, Ref: longRef, CreatedAt: at})
		}
	}
	if len(p.Continuations) > 0 {
		next := p.Continuations[0]
		p.Next = &next
	}
	return p, store
}

// follow decodes a continuation's call the way the list routes do (state +
// base64url "offset:N" cursor) and returns the state and the rows it reaches.
func follow(t *testing.T, kind PartKind, store backingStore, cur Cursor) (string, []uuid.UUID) {
	t.Helper()
	method, rest, _ := strings.Cut(cur.Call, " ")
	path, query, _ := strings.Cut(rest, "?")
	wantPath := "/v0/runs"
	if kind == PartCampaigns {
		wantPath = "/v0/campaigns"
	}
	if method != "GET" || path != wantPath || cur.Part != kind {
		t.Fatalf("continuation %+v does not name GET %s", cur, wantPath)
	}
	q, err := url.ParseQuery(query)
	if err != nil || q.Get("repo") != testRepo {
		t.Fatalf("continuation %q: query %v (%v), want repo=%s", cur.Call, q, err, testRepo)
	}
	off := 0
	if c := q.Get("cursor"); c != "" {
		raw, err := base64.URLEncoding.DecodeString(c)
		n, ok := strings.CutPrefix(string(raw), "offset:")
		if err != nil || !ok {
			t.Fatalf("continuation %q: undecodable cursor %q", cur.Call, c)
		}
		if off, err = strconv.Atoi(n); err != nil {
			t.Fatalf("continuation %q: cursor offset %q: %v", cur.Call, n, err)
		}
	}
	if off != cur.Offset {
		t.Fatalf("continuation %q: call offset %d, Offset field %d", cur.Call, off, cur.Offset)
	}
	st := q.Get("state")
	if off > len(store[st]) {
		t.Fatalf("continuation %q: offset %d past the %d %s rows", cur.Call, off, len(store[st]), st)
	}
	return st, store[st][off:]
}

// assertCoverage is the follow-and-cover oracle: for every state, the kept
// items plus the rows its continuation reaches equal the backing rows EXACTLY
// (no gap, no duplicate, order kept); at most one continuation per state, in
// state order; Next equals Continuations[0]; and a part is truncated iff it
// carries continuations.
func assertCoverage(t *testing.T, label string, p Part, store backingStore) {
	t.Helper()
	if p.Truncated != (len(p.Continuations) > 0) {
		t.Fatalf("%s: truncated=%v with %d continuations", label, p.Truncated, len(p.Continuations))
	}
	if len(p.Continuations) == 0 && p.Next != nil {
		t.Fatalf("%s: next %+v without continuations", label, p.Next)
	}
	if len(p.Continuations) > 0 && (p.Next == nil || !reflect.DeepEqual(*p.Next, p.Continuations[0])) {
		t.Fatalf("%s: next %+v, want continuations[0] %+v", label, p.Next, p.Continuations[0])
	}
	got := map[string][]uuid.UUID{}
	for _, it := range p.InFlight {
		got[it.State] = append(got[it.State], it.ID)
	}
	followed, last := map[string]bool{}, -1
	for _, cur := range p.Continuations {
		st, rows := follow(t, p.Kind, store, cur)
		if followed[st] {
			t.Fatalf("%s: two continuations for state %s: %+v", label, st, p.Continuations)
		}
		followed[st] = true
		if i := slices.Index(inFlightStates(p.Kind), st); i >= 0 {
			if i < last {
				t.Fatalf("%s: continuations out of state order: %+v", label, p.Continuations)
			}
			last = i
		}
		got[st] = append(got[st], rows...)
	}
	for st, want := range store {
		if !slices.Equal(got[st], want) {
			t.Fatalf("%s: state %s: kept+followed = %d rows %v, want the %d backing rows %v (continuations %+v)",
				label, st, len(got[st]), got[st], len(want), want, p.Continuations)
		}
	}
	for st := range got {
		if _, ok := store[st]; !ok {
			t.Fatalf("%s: rows for unknown state %s", label, st)
		}
	}
}

// findPart returns b's part of kind (fatal when absent).
func findPart(t *testing.T, b Brief, kind PartKind) Part {
	t.Helper()
	for _, s := range b.Sections {
		for _, p := range s.Parts {
			if p.Kind == kind {
				return p
			}
		}
	}
	t.Fatalf("brief has no %s part", kind)
	return Part{}
}

// inFlightBrief is a brief whose in_flight section holds parts (merges and
// workflows from synthetic(n) around it), hashed as Compose hashes it.
func inFlightBrief(n int, parts ...Part) Brief {
	b := synthetic(n)
	b.Sections[1].Parts = parts
	b.BriefHash = Hash(b)
	return b
}

// fullSize is the size of b's unbounded render (every marker attached).
func fullSize(t *testing.T, b Brief) int {
	t.Helper()
	out, err := Bound(b, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	return size(t, out)
}

// budgetKeeping returns the smallest budget at which Bound keeps exactly k
// elements of b's kind part (fatal when no budget keeps exactly k).
func budgetKeeping(t *testing.T, b Brief, kind PartKind, k int) int {
	t.Helper()
	kept := func(budget int) int {
		out, err := Bound(b, budget)
		if err != nil {
			return -1
		}
		return partLen(findPart(t, out, kind))
	}
	lo, hi := 1, fullSize(t, b)
	for lo < hi {
		mid := (lo + hi) / 2
		if kept(mid) >= k {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	if got := kept(lo); got != k {
		t.Fatalf("no budget keeps exactly %d %s elements (budget %d keeps %d)", k, kind, lo, got)
	}
	return lo
}

// TestBound_InFlightContinuationPerOverflowingState: two states over the scan
// limit in each in_flight part; at EVERY budget from the full size down to the
// floor, every omitted row of every state is reachable through exactly one
// continuation. Each regime (in_flight uncut / cut inside running only / cut
// inside pending with running fully cut) must actually be reached.
func TestBound_InFlightContinuationPerOverflowingState(t *testing.T) {
	camp, campStore := inFlightPart(PartCampaigns, map[string]int{"pending": 5, "running": 5}, 3)
	runs, runStore := inFlightPart(PartRuns, map[string]int{"pending": 4, "running": 6}, 3)
	if len(camp.Continuations) != 2 || len(runs.Continuations) != 2 {
		t.Fatalf("fixture: %d campaign / %d run continuations, want 2 each", len(camp.Continuations), len(runs.Continuations))
	}
	b := inFlightBrief(4, camp, runs)
	regimes := map[string]bool{}
	for budget := fullSize(t, b); budget > 0; budget -= 11 {
		out, err := Bound(b, budget)
		if errors.Is(err, ErrBudgetTooSmall) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := size(t, out); got > budget {
			t.Fatalf("budget %d: result is %d bytes", budget, got)
		}
		label := "budget " + strconv.Itoa(budget)
		c := findPart(t, out, PartCampaigns)
		assertCoverage(t, label+" campaigns", c, campStore)
		assertCoverage(t, label+" runs", findPart(t, out, PartRuns), runStore)
		switch k := len(c.InFlight); {
		case k == len(camp.InFlight):
			regimes["in_flight uncut"] = true
		case k >= 3:
			regimes["cut inside running only"] = true
		case k > 0:
			regimes["cut inside pending, running fully cut"] = true
		}
		for _, s := range out.Sections {
			for _, p := range s.Parts {
				if !isInFlight(p.Kind) && len(p.Continuations) > 0 {
					t.Fatalf("%s: %s part carries continuations %+v", label, p.Kind, p.Continuations)
				}
			}
		}
	}
	for _, r := range []string{"in_flight uncut", "cut inside running only", "cut inside pending, running fully cut"} {
		if !regimes[r] {
			t.Errorf("regime %q never reached; its coverage assertion is vacuous (reached %v)", r, regimes)
		}
	}
}

// TestBound_ReboundKeepsContinuationOnlyStates is the REST -> MCP path: the
// REST bound cuts running ENTIRELY (the body carries no running item, only a
// running@0 continuation), the body crosses the JSON wire, and the MCP re-bound
// cuts further into pending — every row stays reachable against the ORIGINAL
// store, and the running@0 continuation survives.
func TestBound_ReboundKeepsContinuationOnlyStates(t *testing.T) {
	camp, store := inFlightPart(PartCampaigns, map[string]int{"pending": 6, "running": 5}, 4)
	b := inFlightBrief(2, camp)
	b1 := budgetKeeping(t, b, PartCampaigns, 3)
	rest, err := Bound(b, b1)
	if err != nil {
		t.Fatal(err)
	}
	rp := findPart(t, rest, PartCampaigns)
	for _, it := range rp.InFlight {
		if it.State != "pending" {
			t.Fatalf("REST bound kept a %s row; the fixture must cut running entirely", it.State)
		}
	}
	assertCoverage(t, "REST bound", rp, store)
	raw, err := json.Marshal(rest)
	if err != nil {
		t.Fatal(err)
	}
	var wire Brief
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	b2 := budgetKeeping(t, wire, PartCampaigns, 1)
	if b2 >= b1 {
		t.Fatalf("re-bound budget %d is not below the REST budget %d", b2, b1)
	}
	mcp, err := Bound(wire, b2)
	if err != nil {
		t.Fatal(err)
	}
	mp := findPart(t, mcp, PartCampaigns)
	assertCoverage(t, "MCP re-bound", mp, store)
	want := []Cursor{*listCursor(testRepo, PartCampaigns, "pending", 1), *listCursor(testRepo, PartCampaigns, "running", 0)}
	if !reflect.DeepEqual(mp.Continuations, want) {
		t.Errorf("re-bound continuations = %+v, want %+v", mp.Continuations, want)
	}
	if mcp.BriefHash != b.BriefHash {
		t.Error("re-bound re-hashed the brief")
	}
}

// legacyBrief is a single-part brief over p with no other collection, so a
// budget cut lands in p alone.
func legacyBrief(p Part) Brief {
	b := Brief{
		Repo: testRepo, Window: Window{FromSequence: 1, ToSequence: 900, ChainHead: 900, Basis: BasisFirstCaptain},
		Sections: []Section{{Kind: SectionInFlight, Parts: []Part{p}}},
		Absent:   DeclaredAbsences(), Gaps: []digest.Gap{}, Degradations: []Degradation{},
	}
	b.BriefHash = Hash(b)
	return b
}

// TestBound_LegacyNextOnlyInFlightPartKeepsItsState: a REST body from a
// pre-continuations fishhawkd carries only Next (here running@0, its single
// cursor). A cut inside pending must keep that state: [pending@kept, running@0].
func TestBound_LegacyNextOnlyInFlightPartKeepsItsState(t *testing.T) {
	p, store := inFlightPart(PartCampaigns, map[string]int{"pending": 3}, 3)
	store["running"] = []uuid.UUID{detID("legacy/running", 0), detID("legacy/running", 1)}
	p.Truncated, p.Complete, p.OmittedCount = true, false, 2
	p.Next = listCursor(testRepo, PartCampaigns, "running", 0)
	if len(p.Continuations) != 0 {
		t.Fatal("fixture: a legacy part carries no continuations")
	}
	b := legacyBrief(p)
	out, err := Bound(b, budgetKeeping(t, b, PartCampaigns, 1))
	if err != nil {
		t.Fatal(err)
	}
	got := findPart(t, out, PartCampaigns)
	assertCoverage(t, "legacy", got, store)
	want := []Cursor{*listCursor(testRepo, PartCampaigns, "pending", 1), *listCursor(testRepo, PartCampaigns, "running", 0)}
	if !reflect.DeepEqual(got.Continuations, want) {
		t.Errorf("continuations = %+v, want %+v", got.Continuations, want)
	}
}

// TestBound_InFlightContinuationsKeepNonCanonicalStates: nothing is dropped
// for a state outside the canonical list — one seen only in the items, and one
// seen only in an existing continuation, each keep a continuation.
func TestBound_InFlightContinuationsKeepNonCanonicalStates(t *testing.T) {
	at := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	item := func(st string, i int64) InFlightItem {
		return InFlightItem{Kind: "campaign", ID: detID("nc/"+st, i), State: st, Ref: longRef, CreatedAt: at}
	}
	t.Run("item-only state", func(t *testing.T) {
		p := Part{Kind: PartCampaigns, Complete: true, InFlight: []InFlightItem{item("pending", 0), item("pending", 1), item("draining", 0), item("draining", 1)}}
		store := backingStore{"pending": {p.InFlight[0].ID, p.InFlight[1].ID}, "draining": {p.InFlight[2].ID, p.InFlight[3].ID}}
		b := legacyBrief(p)
		out, err := Bound(b, budgetKeeping(t, b, PartCampaigns, 1))
		if err != nil {
			t.Fatal(err)
		}
		got := findPart(t, out, PartCampaigns)
		assertCoverage(t, "item-only state", got, store)
		if len(got.Continuations) != 2 || cursorState(got.Continuations[1]) != "draining" {
			t.Errorf("continuations = %+v, want pending then draining", got.Continuations)
		}
	})
	t.Run("continuation-only state", func(t *testing.T) {
		p := Part{Kind: PartCampaigns, Truncated: true, OmittedCount: 1,
			InFlight:      []InFlightItem{item("pending", 0), item("pending", 1)},
			Continuations: []Cursor{*listCursor(testRepo, PartCampaigns, "draining", 0)}}
		next := p.Continuations[0]
		p.Next = &next
		store := backingStore{"pending": {p.InFlight[0].ID, p.InFlight[1].ID}, "draining": {detID("nc/draining", 9)}}
		b := legacyBrief(p)
		out, err := Bound(b, budgetKeeping(t, b, PartCampaigns, 1))
		if err != nil {
			t.Fatal(err)
		}
		got := findPart(t, out, PartCampaigns)
		assertCoverage(t, "continuation-only state", got, store)
		if len(got.Continuations) != 2 || cursorState(got.Continuations[1]) != "draining" {
			t.Errorf("continuations = %+v, want pending then draining", got.Continuations)
		}
	})
	t.Run("continuations keep state order", func(t *testing.T) {
		// A foreign body naming pending only through a continuation while it
		// holds running rows: the walk is canonical-order first, so pending's
		// continuation still precedes running's.
		p := Part{Kind: PartCampaigns, Truncated: true, OmittedCount: 1,
			InFlight:      []InFlightItem{item("running", 0), item("running", 1)},
			Continuations: []Cursor{*listCursor(testRepo, PartCampaigns, "pending", 0)}}
		next := p.Continuations[0]
		p.Next = &next
		store := backingStore{"pending": {detID("nc/pending", 9)}, "running": {p.InFlight[0].ID, p.InFlight[1].ID}}
		b := legacyBrief(p)
		out, err := Bound(b, budgetKeeping(t, b, PartCampaigns, 1))
		if err != nil {
			t.Fatal(err)
		}
		got := findPart(t, out, PartCampaigns)
		assertCoverage(t, "state order", got, store)
		want := []Cursor{*listCursor(testRepo, PartCampaigns, "pending", 0), *listCursor(testRepo, PartCampaigns, "running", 1)}
		if !reflect.DeepEqual(got.Continuations, want) {
			t.Errorf("continuations = %+v, want %+v", got.Continuations, want)
		}
	})
}

// preContinuationsSyntheticHash is Hash(synthetic(3)) computed on the tree
// BEFORE Part.Continuations existed (#3862, approval condition 3). The field is
// omitempty and canonical() zeroes it, so the canonical bytes — and every
// brief_hash already stamped on a captain_handover_offered entry — must not
// move.
const preContinuationsSyntheticHash = "6a04a9552ac0f97c490e4eb30b6eccabc257399ed377df856cb2e65d64031f44"

// TestHash_IgnoresContinuations: two briefs differing ONLY in an in_flight
// part's Continuations (and Next) hash identically, to the pre-change golden,
// and the canonical bytes carry no continuations key.
func TestHash_IgnoresContinuations(t *testing.T) {
	if got := Hash(synthetic(3)); got != preContinuationsSyntheticHash {
		t.Fatalf("Hash(synthetic(3)) = %s, want the pre-change golden %s: the canonical bytes moved", got, preContinuationsSyntheticHash)
	}
	plain := synthetic(3)
	plain.Sections[1].Parts[0].Truncated = true
	plain.Sections[1].Parts[0].Next = listCursor(testRepo, PartCampaigns, "running", 3)
	withConts := synthetic(3)
	withConts.Sections[1].Parts[0].Truncated = true
	withConts.Sections[1].Parts[0].Continuations = []Cursor{
		*listCursor(testRepo, PartCampaigns, "pending", 3), *listCursor(testRepo, PartCampaigns, "running", 3),
	}
	withConts.Sections[1].Parts[0].Next = &withConts.Sections[1].Parts[0].Continuations[0]
	if Hash(plain) != preContinuationsSyntheticHash || Hash(withConts) != preContinuationsSyntheticHash {
		t.Errorf("hashes %s / %s, want both the pre-change golden %s", Hash(plain), Hash(withConts), preContinuationsSyntheticHash)
	}
	raw, err := json.Marshal(canonical(withConts))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"continuations"`) {
		t.Error("canonical form carries a continuations key")
	}
}
