package precedent

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
)

const (
	testRepo  = "acme/widgets"
	testClass = decisionindex.ClassPlanApproval
)

func row(seq int64, mutate func(*decisionindex.Row)) decisionindex.Row {
	r := decisionindex.Row{
		SourceSequence:  seq,
		SourceEntryHash: "hash",
		RunID:           uuid.New(),
		Repo:            testRepo,
		WorkflowID:      "feature_change",
		DoctrineVersion: "sha-1",
		DecisionClass:   testClass,
		StageKind:       "plan",
		Outcome:         "approve",
		TouchedPaths:    []string{},
		EscalationKeys:  []string{},
		DecidedAt:       time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		ReasonSequence:  seq,
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func baseContext() Context {
	return Context{Repo: testRepo, DecisionClass: testClass, StageKind: "plan"}
}

// TestRank_DeterministicUnderInputReordering pins the TOTAL ordering.
//
// MECHANISM: the two rows score EQUALLY (neither matches any ranking signal),
// and differ only in decided_at and source_sequence. sort.SliceStable preserves
// the incoming order for equal elements, so with only `score DESC` in the
// comparator the reversed arm would invert the output — the test is RED.
// COUNTERFACTUAL: delete the decided_at / source_sequence tie-breaks.
func TestRank_DeterministicUnderInputReordering(t *testing.T) {
	older := row(10, func(r *decisionindex.Row) {
		r.DecidedAt = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	})
	newer := row(20, func(r *decisionindex.Row) {
		r.DecidedAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	})

	forward, _ := Rank(baseContext(), []decisionindex.Row{older, newer}, 0)
	reversed, _ := Rank(baseContext(), []decisionindex.Row{newer, older}, 0)

	a, err := json.Marshal(forward)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("ranking depends on input order:\n forward = %s\nreversed = %s", a, b)
	}
	if len(forward) != 2 || forward[0].SourceSequence != 20 {
		t.Fatalf("want the NEWER decision first, got %+v", forward)
	}
}

// TestRank_PathPrefixJaccard pins the path component, including that a SIBLING
// file in the same directory scores lower than an exact hit but above zero.
// COUNTERFACTUAL: delete the touched-paths term from the weighted sum.
func TestRank_PathPrefixJaccard(t *testing.T) {
	c := baseContext()
	c.TouchedPaths = []string{"backend/internal/server/foo.go"}

	exact := row(1, func(r *decisionindex.Row) {
		r.TouchedPaths = []string{"backend/internal/server/foo.go"}
	})
	sibling := row(2, func(r *decisionindex.Row) {
		r.TouchedPaths = []string{"backend/internal/server/bar.go"}
	})
	elsewhere := row(3, func(r *decisionindex.Row) {
		r.TouchedPaths = []string{"cli/internal/spec/spec.go"}
	})

	items, _ := Rank(c, []decisionindex.Row{elsewhere, sibling, exact}, 0)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	bySeq := map[int64]Item{}
	for _, it := range items {
		bySeq[it.SourceSequence] = it
	}
	if got := bySeq[1].Score.TouchedPaths; got != WeightTouchedPaths {
		t.Errorf("exact-hit path component = %v, want the full weight %v", got, WeightTouchedPaths)
	}
	sib := bySeq[2].Score.TouchedPaths
	if !(sib > 0 && sib < WeightTouchedPaths) {
		t.Errorf("sibling path component = %v, want 0 < c < %v", sib, WeightTouchedPaths)
	}
	if got := bySeq[3].Score.TouchedPaths; got != 0 {
		t.Errorf("unrelated path component = %v, want 0", got)
	}
	// The matched keys must name the shared PREFIXES, not the sibling file.
	want := []string{"backend", "backend/internal", "backend/internal/server"}
	if got := bySeq[2].MatchedKeys.TouchedPathPrefixes; len(got) != len(want) {
		t.Fatalf("sibling matched prefixes = %v, want %v", got, want)
	}
	if items[0].SourceSequence != 1 {
		t.Errorf("highest-scoring item = %d, want the exact path hit (1)", items[0].SourceSequence)
	}
}

// TestRank_ConcernCategoryComponent: one signal differing, its own component.
// COUNTERFACTUAL: delete the concern-category term.
func TestRank_ConcernCategoryComponent(t *testing.T) {
	c := baseContext()
	c.ConcernCategory = decisionindex.CorrectnessConcernCategory

	hit := row(1, func(r *decisionindex.Row) {
		r.ConcernCategory = decisionindex.CorrectnessConcernCategory
	})
	miss := row(2, func(r *decisionindex.Row) {
		r.ConcernCategory = decisionindex.TestingConcernCategory
	})
	items, _ := Rank(c, []decisionindex.Row{hit, miss}, 0)
	if items[0].Score.ConcernCategory != WeightConcernCategory {
		t.Errorf("category component = %v, want %v", items[0].Score.ConcernCategory, WeightConcernCategory)
	}
	if items[0].MatchedKeys.ConcernCategory != decisionindex.CorrectnessConcernCategory {
		t.Errorf("matched category = %q, want correctness", items[0].MatchedKeys.ConcernCategory)
	}
	if items[1].Score.ConcernCategory != 0 || items[1].MatchedKeys.ConcernCategory != "" {
		t.Errorf("non-matching row reported a category match: %+v", items[1])
	}
}

// TestRank_SeverityComponent. COUNTERFACTUAL: delete the severity term.
func TestRank_SeverityComponent(t *testing.T) {
	c := baseContext()
	c.Severity = "high"
	hit := row(1, func(r *decisionindex.Row) { r.Severity = "high" })
	miss := row(2, func(r *decisionindex.Row) { r.Severity = "low" })
	items, _ := Rank(c, []decisionindex.Row{miss, hit}, 0)
	if items[0].SourceSequence != 1 || items[0].Score.Severity != WeightSeverity {
		t.Errorf("severity match not ranked/weighted: %+v", items[0])
	}
	if items[1].Score.Severity != 0 {
		t.Errorf("severity miss scored %v, want 0", items[1].Score.Severity)
	}
}

// TestRank_EscalationKeyComponent. COUNTERFACTUAL: delete the escalation term.
func TestRank_EscalationKeyComponent(t *testing.T) {
	c := baseContext()
	c.EscalationKeys = []string{"scope_cap_exceeded", "autonomy_low"}

	both := row(1, func(r *decisionindex.Row) {
		r.EscalationKeys = []string{"scope_cap_exceeded", "autonomy_low"}
	})
	one := row(2, func(r *decisionindex.Row) {
		r.EscalationKeys = []string{"scope_cap_exceeded"}
	})
	none := row(3, func(r *decisionindex.Row) {
		r.EscalationKeys = []string{"unrelated_rule"}
	})
	items, _ := Rank(c, []decisionindex.Row{none, one, both}, 0)
	bySeq := map[int64]Item{}
	for _, it := range items {
		bySeq[it.SourceSequence] = it
	}
	if bySeq[1].Score.EscalationKeys != WeightEscalationKeys {
		t.Errorf("full overlap = %v, want %v", bySeq[1].Score.EscalationKeys, WeightEscalationKeys)
	}
	if got := bySeq[2].Score.EscalationKeys; got != 0.5*WeightEscalationKeys {
		t.Errorf("half overlap = %v, want %v", got, 0.5*WeightEscalationKeys)
	}
	if bySeq[3].Score.EscalationKeys != 0 {
		t.Errorf("no overlap = %v, want 0", bySeq[3].Score.EscalationKeys)
	}
	if got := bySeq[2].MatchedKeys.EscalationKeys; len(got) != 1 || got[0] != "scope_cap_exceeded" {
		t.Errorf("matched escalation keys = %v, want [scope_cap_exceeded]", got)
	}
}

// TestRank_EmptyUnionIsZeroNotNaN: context and row BOTH tokenize to nothing.
//
// A bare len(inter)/len(union) division yields NaN here, and EVERY comparison
// against NaN is false — so a NaN score passes no threshold and fails no
// assertion phrased as an inequality. The assertions are therefore exact
// equality to 0 PLUS the explicit math.IsNaN check.
// COUNTERFACTUAL: replace jaccard's union-zero guard with a bare division.
func TestRank_EmptyUnionIsZeroNotNaN(t *testing.T) {
	c := baseContext()
	c.TouchedPaths = []string{"", "   ", "///"}
	r := row(1, func(rr *decisionindex.Row) { rr.TouchedPaths = []string{""} })

	items, summary := Rank(c, []decisionindex.Row{r}, 0)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	got := items[0].Score
	if math.IsNaN(got.TouchedPaths) || math.IsNaN(got.EscalationKeys) || math.IsNaN(got.Total) {
		t.Fatalf("score carries NaN: %+v", got)
	}
	if got.TouchedPaths != 0 {
		t.Errorf("touched_paths component = %v, want exactly 0", got.TouchedPaths)
	}
	if got.Total != 0 {
		t.Errorf("total = %v, want exactly 0", got.Total)
	}
	if got.Total > 0 {
		t.Errorf("total > 0 compared true for %v", got.Total)
	}
	if math.IsNaN(summary.AgreementRatio) {
		t.Errorf("agreement ratio is NaN")
	}
}

// TestRank_HardFilterOnlyItemReportsItself: a row matching repo+class+stage kind
// and NOTHING else says so, with zeroed components and an empty matched set.
// COUNTERFACTUAL: delete the hard_filter_only assignment.
func TestRank_HardFilterOnlyItemReportsItself(t *testing.T) {
	c := baseContext()
	c.TouchedPaths = []string{"backend/x.go"}
	c.ConcernCategory = decisionindex.TestingConcernCategory
	c.Severity = "high"
	c.EscalationKeys = []string{"k1"}

	r := row(1, func(rr *decisionindex.Row) {
		rr.TouchedPaths = []string{"frontend/y.ts"}
		rr.ConcernCategory = decisionindex.ScopeConcernCategory
		rr.Severity = "low"
		rr.EscalationKeys = []string{"k2"}
	})
	items, summary := Rank(c, []decisionindex.Row{r}, 0)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if !it.HardFilterOnly {
		t.Errorf("hard_filter_only = false, want true for a row matching only the hard filter")
	}
	if it.Score != (ScoreComponents{}) {
		t.Errorf("components = %+v, want all zero", it.Score)
	}
	if len(it.MatchedKeys.TouchedPathPrefixes) != 0 || len(it.MatchedKeys.EscalationKeys) != 0 ||
		it.MatchedKeys.ConcernCategory != "" || it.MatchedKeys.Severity != "" {
		t.Errorf("matched keys = %+v, want empty", it.MatchedKeys)
	}
	if summary.HardFilterOnly != 1 {
		t.Errorf("summary.hard_filter_only = %d, want 1", summary.HardFilterOnly)
	}
}

// TestRank_DefensiveHardFilterDropsForeignRow hands Rank the state a MIS-BUILT
// ListFilter would produce — a row from another repository, and one of another
// class and stage kind — and requires each to be ABSENT.
// COUNTERFACTUAL: delete hardFilterAdmits' checks; the foreign rows come back.
func TestRank_DefensiveHardFilterDropsForeignRow(t *testing.T) {
	c := baseContext()
	rows := []decisionindex.Row{
		row(1, nil),
		row(2, func(r *decisionindex.Row) { r.Repo = "other/repo" }),
		row(3, func(r *decisionindex.Row) { r.DecisionClass = decisionindex.ClassMergeVerdict }),
		row(4, func(r *decisionindex.Row) { r.StageKind = "implement" }),
	}
	items, summary := Rank(c, rows, 0)
	if len(items) != 1 || items[0].SourceSequence != 1 {
		t.Fatalf("items = %+v, want only the in-repo, in-class, in-kind row (1)", items)
	}
	if summary.Count != 1 {
		t.Errorf("summary.count = %d, want 1", summary.Count)
	}
}

// TestRank_LimitKeepsTheStrongestPrefix: the limit cuts the WEAK tail.
func TestRank_LimitKeepsTheStrongestPrefix(t *testing.T) {
	c := baseContext()
	c.TouchedPaths = []string{"a/b.go"}
	strong := row(1, func(r *decisionindex.Row) { r.TouchedPaths = []string{"a/b.go"} })
	weak := row(2, nil)
	items, summary := Rank(c, []decisionindex.Row{weak, strong}, 1)
	if len(items) != 1 || items[0].SourceSequence != 1 {
		t.Fatalf("items = %+v, want only the strongest", items)
	}
	if summary.Count != 1 {
		t.Errorf("summary describes %d items, want the RETURNED 1", summary.Count)
	}
}

// TestSummary_AgreementRatio covers the unanimous, split and empty sets plus the
// human/delegated split and the doctrine-version set.
func TestSummary_AgreementRatio(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		s := Summarize(nil)
		if s.Count != 0 || s.AgreementRatio != 0 || s.ModalOutcome != "" {
			t.Errorf("empty summary = %+v, want zeroed", s)
		}
		if s.DoctrineVersions == nil {
			t.Error("doctrine_versions is nil; want a non-nil empty slice")
		}
	})
	t.Run("unanimous", func(t *testing.T) {
		s := Summarize([]Item{
			{Outcome: "approve", DoctrineVersion: "sha-1"},
			{Outcome: "approve", DoctrineVersion: "sha-1", Delegated: true},
		})
		if s.AgreementRatio != 1 || s.ModalOutcome != "approve" {
			t.Errorf("summary = %+v, want ratio 1 on approve", s)
		}
		if s.Human != 1 || s.Delegated != 1 {
			t.Errorf("human/delegated = %d/%d, want 1/1", s.Human, s.Delegated)
		}
		if len(s.DoctrineVersions) != 1 {
			t.Errorf("doctrine versions = %v, want one", s.DoctrineVersions)
		}
	})
	t.Run("split", func(t *testing.T) {
		s := Summarize([]Item{
			{Outcome: "approve", DoctrineVersion: "sha-1"},
			{Outcome: "approve", DoctrineVersion: "sha-2"},
			{Outcome: "reject", DoctrineVersion: "sha-2"},
		})
		if s.ModalOutcome != "approve" {
			t.Errorf("modal outcome = %q, want approve", s.ModalOutcome)
		}
		if math.Abs(s.AgreementRatio-2.0/3.0) > 1e-12 {
			t.Errorf("agreement ratio = %v, want 2/3", s.AgreementRatio)
		}
		if len(s.DoctrineVersions) != 2 || s.DoctrineVersions[0] != "sha-1" {
			t.Errorf("doctrine versions = %v, want sorted [sha-1 sha-2]", s.DoctrineVersions)
		}
	})
	t.Run("modal tie is deterministic", func(t *testing.T) {
		first := Summarize([]Item{{Outcome: "reject"}, {Outcome: "approve"}})
		second := Summarize([]Item{{Outcome: "approve"}, {Outcome: "reject"}})
		if first.ModalOutcome != second.ModalOutcome {
			t.Errorf("modal outcome depends on order: %q vs %q", first.ModalOutcome, second.ModalOutcome)
		}
		if first.ModalOutcome != "approve" {
			t.Errorf("tie broken to %q, want the lexicographically smallest (approve)", first.ModalOutcome)
		}
	})
}

// TestRank_MatchedKeysAreCapped: the EXPLANATION is bounded, and its total is
// reported, so a caller supplying thousands of paths cannot make one item's
// matched-key list unbounded.
func TestRank_MatchedKeysAreCapped(t *testing.T) {
	paths := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		paths = append(paths, "pkg"+string(rune('a'+i%26))+"/f"+string(rune('a'+i%26))+".go")
	}
	c := baseContext()
	c.TouchedPaths = paths
	r := row(1, func(rr *decisionindex.Row) { rr.TouchedPaths = paths })
	items, _ := Rank(c, []decisionindex.Row{r}, 0)
	mk := items[0].MatchedKeys
	if len(mk.TouchedPathPrefixes) != MaxMatchedKeys {
		t.Errorf("kept prefixes = %d, want the cap %d", len(mk.TouchedPathPrefixes), MaxMatchedKeys)
	}
	if !mk.TouchedPathPrefixesTruncated {
		t.Error("truncation not marked")
	}
	if mk.TouchedPathPrefixesTotal <= MaxMatchedKeys {
		t.Errorf("reported total = %d, want > %d", mk.TouchedPathPrefixesTotal, MaxMatchedKeys)
	}
}

// fingerprintFixture ranks three rows and returns the items + summary a gate
// block would be fingerprinted over.
func fingerprintFixture(t *testing.T) ([]Item, Summary) {
	t.Helper()
	rows := []decisionindex.Row{
		row(10, func(r *decisionindex.Row) { r.TouchedPaths = []string{"a/b.go"} }),
		row(11, func(r *decisionindex.Row) { r.Outcome = "reject" }),
		row(12, nil),
	}
	items, s := Rank(Context{Repo: testRepo, DecisionClass: testClass, TouchedPaths: []string{"a/c.go"}}, rows, 0)
	if len(items) != 3 {
		t.Fatalf("fixture ranked %d items, want 3", len(items))
	}
	return items, s
}

func TestFingerprint_StableAcrossRecomputation(t *testing.T) {
	items1, s1 := fingerprintFixture(t)
	items2, s2 := fingerprintFixture(t)
	a := Fingerprint("plan_approval", "stage-1", items1, s1)
	b := Fingerprint("plan_approval", "stage-1", items2, s2)
	if a != b || a == "" {
		t.Fatalf("fingerprint not stable across recomputation: %q vs %q", a, b)
	}
}

func TestFingerprint_ChangesOnCitedSet(t *testing.T) {
	items, s := fingerprintFixture(t)
	base := Fingerprint("plan_approval", "stage-1", items, s)
	fewer := items[:2]
	if Fingerprint("plan_approval", "stage-1", fewer, Summarize(fewer)) == base {
		t.Error("removing a cited row did not change the fingerprint")
	}
	more := append(append([]Item(nil), items...), Item{SourceSequence: 99, SourceEntryHash: "h99"})
	if Fingerprint("plan_approval", "stage-1", more, Summarize(more)) == base {
		t.Error("adding a cited row did not change the fingerprint")
	}
	// A cited row swapped for a DIFFERENT row with the same outcome leaves the
	// summary identical, so only the per-item citation can move the key.
	swapped := append([]Item(nil), items...)
	swapped[0].SourceSequence, swapped[0].SourceEntryHash = 999, "other-hash"
	if Summarize(swapped).AgreementRatio != s.AgreementRatio || Summarize(swapped).ModalOutcome != s.ModalOutcome {
		t.Fatal("fixture: the swap must leave the summary unchanged")
	}
	if Fingerprint("plan_approval", "stage-1", swapped, s) == base {
		t.Error("swapping one cited row (summary unchanged) did not change the fingerprint")
	}
	if Fingerprint("plan_approval", "stage-2", items, s) == base {
		t.Error("a different stage id did not change the fingerprint")
	}
	if Fingerprint("merge_verdict", "stage-1", items, s) == base {
		t.Error("a different decision class did not change the fingerprint")
	}
}

func TestFingerprint_ChangesOnIndexVersion(t *testing.T) {
	items, s := fingerprintFixture(t)
	if fingerprintAt(IndexVersion, "plan_approval", "stage-1", items, s) != Fingerprint("plan_approval", "stage-1", items, s) {
		t.Fatal("Fingerprint does not key on IndexVersion")
	}
	if fingerprintAt(IndexVersion+"-next", "plan_approval", "stage-1", items, s) == Fingerprint("plan_approval", "stage-1", items, s) {
		t.Error("a different ranking-contract version produced the same fingerprint")
	}
}

func TestFingerprint_IgnoresReasonExcerpt(t *testing.T) {
	items, s := fingerprintFixture(t)
	base := Fingerprint("plan_approval", "stage-1", items, s)
	withProse := append([]Item(nil), items...)
	for i := range withProse {
		withProse[i].ReasonExcerpt = "a reason read from the chain"
	}
	if Fingerprint("plan_approval", "stage-1", withProse, s) != base {
		t.Error("the fingerprint changed on ReasonExcerpt alone — a degraded chain read would re-record")
	}
}
