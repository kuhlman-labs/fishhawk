package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
)

// bigPrecedentResult builds a fixture result set: n items, each carrying a
// reason excerpt and matched-key lists, so the marshalled response exceeds any
// realistic budget and the ladder is genuinely exercised.
func bigPrecedentResult(n, excerptBytes int) PrecedentResult {
	res := PrecedentResult{
		ResolvedContext: PrecedentResolvedContext{
			Repo:           "acme/widgets",
			DecisionClass:  "plan_approval",
			StageKind:      "implement",
			TouchedPaths:   []string{"backend/internal/server/foo.go"},
			EscalationKeys: []string{"scope_cap_exceeded"},
		},
		Summary: precedent.Summary{Count: n, Human: n, ModalOutcome: "approve",
			AgreementRatio: 1, DoctrineVersions: []string{"sha-1"}},
	}
	res.ResolvedContext.TouchedPathsTotal = len(res.ResolvedContext.TouchedPaths)
	res.ResolvedContext.EscalationKeysTotal = len(res.ResolvedContext.EscalationKeys)
	for i := 0; i < n; i++ {
		res.Results = append(res.Results, precedent.Item{
			SourceSequence:  int64(1000 - i),
			SourceEntryHash: strings.Repeat("a", 64),
			RunID:           uuid.New().String(),
			Repo:            "acme/widgets",
			DecisionClass:   "plan_approval",
			StageKind:       "implement",
			Outcome:         "approve",
			DoctrineVersion: "sha-1",
			DecidedAt:       time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
			ReasonSequence:  int64(1000 - i),
			ReasonKey:       "reason",
			ReasonExcerpt:   strings.Repeat("r", excerptBytes),
			MatchedKeys: precedent.MatchedKeys{
				TouchedPathPrefixes:      []string{"backend", "backend/internal", "backend/internal/server"},
				TouchedPathPrefixesTotal: 3,
				EscalationKeys:           []string{"scope_cap_exceeded"},
				EscalationKeysTotal:      1,
			},
			Score: precedent.ScoreComponents{TouchedPaths: 0.3, EscalationKeys: 0.2,
				ConcernCategory: 0.25, Severity: 0.1, Total: 0.85},
		})
	}
	return res
}

// runPrecedentTool drives the real tool handler against the fixture backend.
func runPrecedentTool(t *testing.T, res PrecedentResult, in PrecedentInput) (PrecedentOutput, *fakeBackend) {
	t.Helper()
	fb, srv := newFakeBackend(t)
	fb.precedentResp = res
	r := newResolver(srv, nil)
	_, out, err := r.precedent(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("precedent tool: %v", err)
	}
	return out, fb
}

func precedentMarshalLen(t *testing.T, out PrecedentOutput) int {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return len(raw)
}

// TestPrecedentTool_InertUnderBudget is the not-a-control control: a small
// response passes through byte-identical with NO elisions block.
func TestPrecedentTool_InertUnderBudget(t *testing.T) {
	res := bigPrecedentResult(2, 40)
	out, _ := runPrecedentTool(t, res, PrecedentInput{
		Repo: "acme/widgets", DecisionClass: "plan_approval"})
	if out.Elisions != nil {
		t.Errorf("elisions block present on an under-budget response: %+v", out.Elisions)
	}
	if len(out.Results) != 2 {
		t.Errorf("results = %d, want the full 2", len(out.Results))
	}
	if out.Results[0].ReasonExcerpt == "" || out.Results[0].Score.TouchedPaths == 0 {
		t.Error("an under-budget response was reduced anyway")
	}
	unbounded := PrecedentOutput{ResolvedContext: res.ResolvedContext, Summary: res.Summary,
		Results: res.Results, Truncated: res.Truncated, Degraded: res.Degraded}
	a, _ := json.Marshal(unbounded)
	b, _ := json.Marshal(out)
	if string(a) != string(b) {
		t.Errorf("bounded render differs from the unbounded one:\n a = %s\n b = %s", a, b)
	}
}

// TestPrecedentTool_WithinByteBudget: a LARGE result set is brought under the
// effective budget with the truncation MARKED.
// COUNTERFACTUAL: delete the boundPrecedentOutput call from the handler → the
// over-budget response is returned and this is RED on the measured size.
func TestPrecedentTool_WithinByteBudget(t *testing.T) {
	out, _ := runPrecedentTool(t, bigPrecedentResult(50, 280), PrecedentInput{
		Repo: "acme/widgets", DecisionClass: "plan_approval"})
	budget := mcpResponseByteBudgetDefault
	if n := precedentMarshalLen(t, out); n > budget {
		t.Fatalf("marshalled response = %d bytes, want <= the %d-byte budget", n, budget)
	}
	if out.Elisions == nil {
		t.Fatal("the response was reduced but carries NO elisions block — a silently short ranking")
	}
	if len(out.Results) == 0 {
		t.Error("every item was dropped at a tier above the floor")
	}
}

// TestPrecedentTool_LadderTiersInOrder walks the ladder with a SHRINKING budget
// and asserts each named tier is reached in turn, so B1 and B2 are individually
// exercised rather than only the floor.
func TestPrecedentTool_LadderTiersInOrder(t *testing.T) {
	res := bigPrecedentResult(40, 280)
	unbounded, err := marshalledLen(PrecedentOutput{ResolvedContext: res.ResolvedContext,
		Summary: res.Summary, Results: res.Results})
	if err != nil {
		t.Fatal(err)
	}

	// B1: a budget just under the unbounded size, which dropping the excerpts
	// and the component breakdown alone must satisfy.
	b1, err := boundPrecedentOutput(PrecedentOutput{ResolvedContext: res.ResolvedContext,
		Summary: res.Summary, Results: res.Results}, fixedBudget(unbounded-1))
	if err != nil {
		t.Fatal(err)
	}
	if b1.Elisions == nil || b1.Elisions.Tier != "B1" {
		t.Fatalf("tier = %v, want B1", b1.Elisions)
	}
	if len(b1.Results) != len(res.Results) {
		t.Errorf("B1 dropped %d items; it must drop CONTENT, not items", len(res.Results)-len(b1.Results))
	}
	for _, it := range b1.Results {
		if it.ReasonExcerpt != "" {
			t.Error("B1 kept a reason excerpt")
		}
		if it.Score.TouchedPaths != 0 || it.Score.EscalationKeys != 0 {
			t.Error("B1 kept the per-signal score breakdown")
		}
		if it.Score.Total == 0 {
			t.Error("B1 dropped score.total; only the BREAKDOWN is elided")
		}
		if len(it.MatchedKeys.TouchedPathPrefixes) == 0 {
			t.Error("B1 dropped the matched keys; they are what explains the item")
		}
	}

	// B2: a budget small enough that dropping content is not enough.
	b1Len := precedentMarshalLen(t, b1)
	b2, err := boundPrecedentOutput(PrecedentOutput{ResolvedContext: res.ResolvedContext,
		Summary: res.Summary, Results: res.Results}, fixedBudget(b1Len/2))
	if err != nil {
		t.Fatal(err)
	}
	if b2.Elisions == nil || b2.Elisions.Tier != "B2" {
		t.Fatalf("tier = %v, want B2", b2.Elisions)
	}
	if len(b2.Results) == 0 || len(b2.Results) >= len(res.Results) {
		t.Errorf("B2 kept %d of %d items, want a strict non-empty prefix", len(b2.Results), len(res.Results))
	}
	// The retained prefix is the STRONGEST precedent: the tail is dropped.
	if b2.Results[0].SourceSequence != res.Results[0].SourceSequence {
		t.Errorf("B2 kept the wrong end: first item = %d, want %d",
			b2.Results[0].SourceSequence, res.Results[0].SourceSequence)
	}
	if n := precedentMarshalLen(t, b2); n > b1Len/2 {
		t.Errorf("B2 output = %d bytes, want <= %d", n, b1Len/2)
	}
}

// TestPrecedentTool_SingleOversizedRowFallsToSummaryFloor: ONE item whose
// excerpt and matched-key list ALONE exceed the budget. Without the floor tier
// the ladder cannot converge (B2 cannot drop below one item), so this is RED on
// the measured size.
// COUNTERFACTUAL: delete the precedentFloor call → boundPrecedentOutput returns
// the single over-budget item and the size assertion is RED.
func TestPrecedentTool_SingleOversizedRowFallsToSummaryFloor(t *testing.T) {
	res := bigPrecedentResult(1, 40)
	// Make the single row alone enormous: a long excerpt plus thousands of
	// matched prefixes.
	res.Results[0].ReasonExcerpt = strings.Repeat("R", 6000)
	prefixes := make([]string, 0, 4000)
	for i := 0; i < 4000; i++ {
		prefixes = append(prefixes, "very/long/path/segment/number/"+uuid.New().String())
	}
	res.Results[0].MatchedKeys.TouchedPathPrefixes = prefixes
	res.Results[0].MatchedKeys.TouchedPathPrefixesTotal = len(prefixes)

	out, err := boundPrecedentOutput(PrecedentOutput{ResolvedContext: res.ResolvedContext,
		Summary: res.Summary, Results: res.Results}, fixedBudget(mcpConvergenceFloorBytes))
	if err != nil {
		t.Fatal(err)
	}
	if n := precedentMarshalLen(t, out); n > mcpConvergenceFloorBytes {
		t.Fatalf("floor output = %d bytes, want <= the %d-byte convergence floor", n, mcpConvergenceFloorBytes)
	}
	if out.Elisions == nil || out.Elisions.Tier != floorTierName {
		t.Fatalf("tier = %v, want the floor", out.Elisions)
	}
	if len(out.Results) != 0 {
		t.Errorf("the floor kept %d items, want results entirely elided", len(out.Results))
	}
	if !out.Truncated {
		t.Error("the floor did not mark truncated")
	}
	if out.Summary.Count != res.Summary.Count {
		t.Errorf("summary.count = %d, want the whole-set %d retained", out.Summary.Count, res.Summary.Count)
	}
	var aggregate bool
	for _, f := range out.Elisions.Fields {
		if f.Aggregate {
			aggregate = true
		}
	}
	if !aggregate {
		t.Error("the floor carries no aggregate elision entry")
	}
}

// TestPrecedentTool_FloorIsBoundedByA10000PathResolvedContext is #3731 binding
// condition 2 at the MCP layer: the floor's size must NOT depend on how large
// the echoed resolved context is. The fixture backend returns a resolved context
// with 10,000 touched paths and 10,000 escalation keys — a backend that did not
// cap its echo — and the floor must still fit.
// COUNTERFACTUAL: delete capPrecedentResolvedContext (return rc unchanged) → the
// floor carries all 20,000 strings and the size assertion is RED.
func TestPrecedentTool_FloorIsBoundedByA10000PathResolvedContext(t *testing.T) {
	res := bigPrecedentResult(3, 280)
	paths := make([]string, 0, 10000)
	keys := make([]string, 0, 10000)
	for i := 0; i < 10000; i++ {
		paths = append(paths, "pkg/"+uuid.New().String()+"/file.go")
		keys = append(keys, "rule_"+uuid.New().String())
	}
	res.ResolvedContext.TouchedPaths = paths
	res.ResolvedContext.EscalationKeys = keys

	out, err := boundPrecedentOutput(PrecedentOutput{ResolvedContext: res.ResolvedContext,
		Summary: res.Summary, Results: res.Results}, fixedBudget(mcpConvergenceFloorBytes))
	if err != nil {
		t.Fatal(err)
	}
	n := precedentMarshalLen(t, out)
	if n > mcpConvergenceFloorBytes {
		t.Fatalf("floor with a 10,000-path resolved context = %d bytes, want <= %d", n, mcpConvergenceFloorBytes)
	}
	if out.Elisions == nil || out.Elisions.Tier != floorTierName {
		t.Fatalf("tier = %v, want the floor", out.Elisions)
	}
	rc := out.ResolvedContext
	if len(rc.TouchedPaths) != precedentFloorListCap || len(rc.EscalationKeys) != precedentFloorListCap {
		t.Errorf("floor kept %d paths / %d keys, want %d each",
			len(rc.TouchedPaths), len(rc.EscalationKeys), precedentFloorListCap)
	}
	if !rc.TouchedPathsTruncated || !rc.EscalationKeysTruncated {
		t.Error("the floor capped the lists without marking them truncated")
	}
	if rc.TouchedPathsTotal != 10000 || rc.EscalationKeysTotal != 10000 {
		t.Errorf("floor totals = %d/%d, want the untruncated 10000/10000",
			rc.TouchedPathsTotal, rc.EscalationKeysTotal)
	}
}

// TestPrecedentTool_QueryEncoding pins the CLIENT half: repeatable paths and
// escalation_keys, and every scalar parameter reaching the wire.
func TestPrecedentTool_QueryEncoding(t *testing.T) {
	_, fb := runPrecedentTool(t, bigPrecedentResult(1, 20), PrecedentInput{
		DecisionClass:   "concern_waive",
		Repo:            "acme/widgets",
		RunID:           "11111111-1111-1111-1111-111111111111",
		StageID:         "22222222-2222-2222-2222-222222222222",
		StageKind:       "implement",
		Paths:           []string{"a.go", "b.go", ""},
		ConcernCategory: "Bug",
		Severity:        "high",
		EscalationKeys:  []string{"k1", "k2"},
		Limit:           7,
	})
	fb.mu.Lock()
	q := fb.lastPrecedentQuery
	fb.mu.Unlock()
	for _, want := range []string{
		"decision_class=concern_waive", "repo=acme%2Fwidgets",
		"run_id=11111111-1111-1111-1111-111111111111",
		"stage_id=22222222-2222-2222-2222-222222222222",
		"stage_kind=implement", "concern_category=Bug", "severity=high", "limit=7",
		"paths=a.go", "paths=b.go", "escalation_keys=k1", "escalation_keys=k2",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q is missing %q", q, want)
		}
	}
	if strings.Count(q, "paths=") != 2 {
		t.Errorf("query %q: paths appears %d times, want 2 (the empty value must be dropped)", q, strings.Count(q, "paths="))
	}
}

// TestPrecedentTool_DecodesResponse pins the client decode: the ranked item's
// explanation fields and the summary survive the round trip.
func TestPrecedentTool_DecodesResponse(t *testing.T) {
	res := bigPrecedentResult(1, 20)
	res.Truncated = true
	res.Degraded = []PrecedentDegraded{{Reason: "window_truncated", Detail: "d"}}
	out, _ := runPrecedentTool(t, res, PrecedentInput{Repo: "acme/widgets", DecisionClass: "plan_approval"})
	if !out.Truncated || len(out.Degraded) != 1 || out.Degraded[0].Reason != "window_truncated" {
		t.Errorf("truncation/degradation lost in decode: %+v", out)
	}
	if out.Summary.ModalOutcome != "approve" || out.Summary.AgreementRatio != 1 {
		t.Errorf("summary lost in decode: %+v", out.Summary)
	}
	it := out.Results[0]
	if it.SourceEntryHash == "" || it.ReasonKey != "reason" || it.Score.Total != 0.85 {
		t.Errorf("item lost in decode: %+v", it)
	}
	if len(it.MatchedKeys.TouchedPathPrefixes) != 3 {
		t.Errorf("matched keys lost in decode: %+v", it.MatchedKeys)
	}
	if out.ResolvedContext.Repo != "acme/widgets" {
		t.Errorf("resolved context lost in decode: %+v", out.ResolvedContext)
	}
}

// TestPrecedentTool_FloorIsBoundedByAnOversizedSummary is the size evidence for
// the one part the floor RETAINS: the agreement summary. Its counts are
// fixed-width, but ModalOutcome is an outcome value read out of an audit payload
// and DoctrineVersions is a set with one entry per distinct charter revision the
// scored rows span — both index-derived and neither bounded by a fixed
// vocabulary. The floor's size must be a constant even at their maximum, WHILE
// the resolved context is simultaneously oversized, so the two caps are measured
// together rather than one at a time.
//
// COUNTERFACTUAL: replace capPrecedentSummary's body with `return s` → the floor
// carries the 4KB modal outcome and all 500 doctrine versions and the size
// assertion is RED.
func TestPrecedentTool_FloorIsBoundedByAnOversizedSummary(t *testing.T) {
	res := bigPrecedentResult(3, 280)
	res.Summary.ModalOutcome = strings.Repeat("o", 4096)
	versions := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		versions = append(versions, strings.Repeat("v", 200)+uuid.New().String())
	}
	res.Summary.DoctrineVersions = versions
	// Oversized resolved context at the same time: the floor keeps BOTH, so the
	// bound has to hold for their sum.
	paths := make([]string, 0, 10000)
	keys := make([]string, 0, 10000)
	for i := 0; i < 10000; i++ {
		paths = append(paths, "pkg/"+uuid.New().String()+"/file.go")
		keys = append(keys, "rule_"+uuid.New().String())
	}
	res.ResolvedContext.TouchedPaths = paths
	res.ResolvedContext.EscalationKeys = keys

	out, err := boundPrecedentOutput(PrecedentOutput{ResolvedContext: res.ResolvedContext,
		Summary: res.Summary, Results: res.Results}, fixedBudget(mcpConvergenceFloorBytes))
	if err != nil {
		t.Fatal(err)
	}
	if n := precedentMarshalLen(t, out); n > mcpConvergenceFloorBytes {
		t.Fatalf("floor with a maximal summary AND a 10,000-path context = %d bytes, want <= %d", n, mcpConvergenceFloorBytes)
	}
	if out.Elisions == nil || out.Elisions.Tier != floorTierName {
		t.Fatalf("tier = %v, want the floor", out.Elisions)
	}
	if len(out.Summary.ModalOutcome) > floorFieldCap {
		t.Errorf("floor kept a %d-byte modal outcome, want <= %d", len(out.Summary.ModalOutcome), floorFieldCap)
	}
	// AT MOST the cap, not EXACTLY it: this fixture is maximal, so the measured
	// shrink may legitimately have halved past the starting cap. The exact-count
	// assertion belongs to the non-maximal fixture — see
	// TestPrecedentTool_FloorIsBoundedByA10000PathResolvedContext, which asserts
	// EXACTLY precedentFloorListCap and so pins that the cap is applied at all.
	if n := len(out.Summary.DoctrineVersions); n == 0 || n > precedentFloorListCap {
		t.Errorf("floor kept %d doctrine versions, want 1..%d", n, precedentFloorListCap)
	}
	for _, v := range out.Summary.DoctrineVersions {
		if len(v) > floorFieldCap {
			t.Errorf("floor kept a %d-byte doctrine version, want <= %d", len(v), floorFieldCap)
		}
	}
	// The NUMBERS are exact: only the strings are capped, so the summary still
	// describes the whole set.
	if out.Summary.Count != res.Summary.Count || out.Summary.Human != res.Summary.Human ||
		out.Summary.AgreementRatio != res.Summary.AgreementRatio {
		t.Errorf("floor summary numbers = %+v, want the exact %+v", out.Summary, res.Summary)
	}
}

// TestPrecedentTool_FloorFitsWithNoListsAtAll pins the terminal step of the
// floor's measured shrink: at list cap 0 the floor is the scalars plus the
// aggregate elision prose and nothing else, and THAT must fit — it is what makes
// the shrink loop converge rather than spin. Every scalar is oversized here, so
// the case is the worst one reachable at cap 0.
//
// COUNTERFACTUAL: delete capJSONString from capPrecedentResolvedContext's scalar
// caps (assign rc's values straight through) → the seven 4KB scalars are retained
// and the size assertion is RED.
func TestPrecedentTool_FloorFitsWithNoListsAtAll(t *testing.T) {
	res := bigPrecedentResult(2, 280)
	big := strings.Repeat("s", 4096)
	res.ResolvedContext.Repo = big
	res.ResolvedContext.DecisionClass = big
	res.ResolvedContext.StageKind = big
	res.ResolvedContext.ConcernCategory = big
	res.ResolvedContext.Severity = big
	res.ResolvedContext.RunID = big
	res.ResolvedContext.StageID = big
	res.Summary.ModalOutcome = big

	floor, err := buildPrecedentFloor(PrecedentOutput{ResolvedContext: res.ResolvedContext,
		Summary: res.Summary, Results: res.Results}, fixedBudget(mcpConvergenceFloorBytes), 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := precedentMarshalLen(t, floor); n > mcpConvergenceFloorBytes {
		t.Fatalf("floor at list cap 0 = %d bytes, want <= %d — the shrink loop would not converge", n, mcpConvergenceFloorBytes)
	}
	if len(floor.ResolvedContext.TouchedPaths) != 0 || len(floor.ResolvedContext.EscalationKeys) != 0 ||
		len(floor.Summary.DoctrineVersions) != 0 {
		t.Errorf("cap 0 retained list entries: %+v / %v", floor.ResolvedContext, floor.Summary.DoctrineVersions)
	}
	// The totals survive the shrink, so even the emptiest floor reports how much
	// there was.
	if floor.ResolvedContext.TouchedPathsTotal != len(res.ResolvedContext.TouchedPaths) {
		t.Errorf("touched_paths_total = %d, want the untruncated %d",
			floor.ResolvedContext.TouchedPathsTotal, len(res.ResolvedContext.TouchedPaths))
	}
}
