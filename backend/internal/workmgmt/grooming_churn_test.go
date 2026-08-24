package workmgmt

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// --- fixtures ---------------------------------------------------------------
//
// Every baseline in this file is seeded BY CONSTRUCTION from literal values (or
// from NewGroomingBaseline over a literal prior report), never by calling
// FilterGroomingChurn in a test's own setup. A counterfactual RED must land on
// the behavioral assertion, not on fixture setup that happened to route through
// the control being deleted.

func gcItem(n string) plan.ItemRef {
	return plan.ItemRef{Type: "github_issue", ID: "kuhlman-labs/fishhawk#" + n, URL: "https://example.test/" + n}
}

func gcOrdering(n string, rank int, score float64, rubric ...string) plan.OrderingEntry {
	cites := make([]plan.RubricCitation, 0, len(rubric))
	for _, r := range rubric {
		cites = append(cites, plan.RubricCitation{RubricID: r, Quote: "q-" + r})
	}
	ref := gcItem(n)
	return plan.OrderingEntry{
		ID:              plan.GroomingEntryID(plan.GroomingClassOrdering, "", ref),
		ItemRef:         ref,
		Rank:            rank,
		Score:           score,
		RubricCitations: cites,
		Rationale:       "prose that the fingerprint must ignore",
	}
}

// gcHygiene builds a hygiene defect whose fingerprint basis is the STRUCTURED
// fix (#2847), not the `suggested_fix` prose. The prose is a fixed string so a
// test that changes only `fix` changes only the structural basis, and the
// prose-reword test can change only the prose.
func gcHygiene(n, defect, fix string) plan.HygieneDefect {
	ref := gcItem(n)
	d := plan.HygieneDefect{
		ID:           plan.GroomingEntryID(plan.GroomingClassHygiene, defect, ref),
		ItemRef:      ref,
		Defect:       defect,
		Detail:       "prose detail",
		SuggestedFix: "prose suggestion",
	}
	if fix != "" {
		d.Fix = &plan.HygieneFix{FieldValue: fix}
	}
	return d
}

func gcDuplicate(a, b, confidence string) plan.DuplicateCandidate {
	ra, rb := gcItem(a), gcItem(b)
	return plan.DuplicateCandidate{
		ID:         plan.GroomingEntryID(plan.GroomingClassDuplicate, "", ra, rb),
		Pair:       []plan.ItemRef{ra, rb},
		Basis:      "prose basis",
		Confidence: confidence,
	}
}

func gcDependency(from, to, kind string) plan.DependencyEdge {
	rf, rt := gcItem(from), gcItem(to)
	return plan.DependencyEdge{
		ID:    plan.GroomingEntryID(plan.GroomingClassDependency, "", rf, rt),
		From:  rf,
		To:    rt,
		Basis: "prose basis",
		Kind:  kind,
	}
}

func gcVisionDrift(n, basis, charterRef string) plan.VisionDriftFlag {
	ref := gcItem(n)
	return plan.VisionDriftFlag{
		ID:           plan.GroomingEntryID(plan.GroomingClassVisionDrift, charterRef, ref),
		ItemRef:      ref,
		Basis:        basis,
		CharterRefID: charterRef,
		Detail:       "prose detail",
	}
}

func gcDecomposition(n string, children int) plan.DecompositionSuggestion {
	ref := gcItem(n)
	kids := make([]plan.DecompositionChild, 0, children)
	for i := 0; i < children; i++ {
		kids = append(kids, plan.DecompositionChild{Title: "child " + string(rune('A'+i)), ScopeHint: "hint"})
	}
	return plan.DecompositionSuggestion{
		ID:               plan.GroomingEntryID(plan.GroomingClassDecomposition, "", ref),
		ItemRef:          ref,
		Rationale:        "prose rationale",
		ProposedChildren: kids,
	}
}

// gcReport assembles a report from the given entries, leaving every unnamed
// class empty.
func gcReport(charterHash string, mut func(*plan.GroomingReport)) *plan.GroomingReport {
	gr := &plan.GroomingReport{
		Kind:          plan.KindGroomingReport,
		ReportVersion: plan.GroomingReportVersion,
		Summary:       "fixture",
	}
	if charterHash != "" {
		gr.CharterRef = &plan.GroomingCharterRef{Path: ".fishhawk/charter.md", ContentHash: charterHash}
	}
	if mut != nil {
		mut(gr)
	}
	return gr
}

// gcBaseline builds a baseline literally: id -> (class, disposition, basis,
// rank, score).
func gcBaseline(charterHash string, entries map[string]GroomingBaselineEntry) GroomingBaseline {
	if entries == nil {
		entries = map[string]GroomingBaselineEntry{}
	}
	return GroomingBaseline{CharterHash: charterHash, Entries: entries}
}

func gcDefaultThresholds() GroomingThresholds {
	return Default().ResolveGroomingThresholds()
}

func proposedIDs(res GroomingChurnResult) []string { return res.Summary.ProposedIDs }

func suppressionReason(t *testing.T, res GroomingChurnResult, id string) string {
	t.Helper()
	for _, s := range res.Suppressed {
		if s.EntryID == id {
			return s.Reason
		}
	}
	return ""
}

func resurfaceRecord(t *testing.T, res GroomingChurnResult, id string) (GroomingResurface, bool) {
	t.Helper()
	for _, r := range res.Resurfaced {
		if r.EntryID == id {
			return r, true
		}
	}
	return GroomingResurface{}, false
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// --- CROSS-BOUNDARY / done-means test ---------------------------------------

// TestShippedDefaultThresholdsDriveTheGuard is the DONE-MEANS test for the
// default VALUES. The scope spans JSON Schema -> shipped YAML -> Go config
// parse -> guard logic, and per-layer units would pass while the seam broke:
// the defaults are a config value, not something compilation enforces, so a
// no-op or comment-only touch of the YAML that satisfies the scope-completeness
// presence gate fails HERE.
func TestShippedDefaultThresholdsDriveTheGuard(t *testing.T) {
	th := gcDefaultThresholds()
	if th.MinRankMovement != 2 {
		t.Errorf("shipped min_rank_movement = %d, want 2", th.MinRankMovement)
	}
	if th.MinScoreDelta != 0.05 {
		t.Errorf("shipped min_score_delta = %v, want 0.05", th.MinScoreDelta)
	}
	for _, d := range DefaultSignificantHygieneDefects() {
		if !th.IsSignificantHygieneDefect(d) {
			t.Errorf("shipped default: %q should be significant (absent set = all six)", d)
		}
	}
	if len(th.Clamped) != 0 {
		t.Errorf("shipped default clamped %v, want none", th.Clamped)
	}

	// End to end with those resolved thresholds: a 1-position / 0.02 movement
	// is suppressed and a 3-position movement is proposed.
	quiet := gcOrdering("1", 2, 0.72, "R1")
	loud := gcOrdering("2", 5, 0.80, "R2")
	report := gcReport("h1", func(gr *plan.GroomingReport) {
		gr.Ordering = []plan.OrderingEntry{quiet, loud}
	})
	base := gcBaseline("h1", map[string]GroomingBaselineEntry{
		quiet.ID: {Class: plan.GroomingClassOrdering, Disposition: GroomingDispositionApplied,
			BasisHash: groomingOrderingBasis(quiet), Rank: 1, Score: 0.70},
		loud.ID: {Class: plan.GroomingClassOrdering, Disposition: GroomingDispositionApplied,
			BasisHash: groomingOrderingBasis(loud), Rank: 2, Score: 0.80},
	})
	res := FilterGroomingChurn(report, base, th)
	if containsID(proposedIDs(res), quiet.ID) {
		t.Errorf("1-position / 0.02 movement was proposed under the shipped defaults; proposed=%v", proposedIDs(res))
	}
	if !containsID(proposedIDs(res), loud.ID) {
		t.Errorf("3-position movement was NOT proposed under the shipped defaults; proposed=%v", proposedIDs(res))
	}
}

// TestShippedDefaultMatchesCodeDefaults pins the shipped YAML against the
// exported constants so the two cannot drift.
func TestShippedDefaultMatchesCodeDefaults(t *testing.T) {
	conv := Default()
	if conv.Grooming == nil || conv.Grooming.Thresholds == nil {
		t.Fatal("shipped default declares no grooming.thresholds block")
	}
	spec := conv.Grooming.Thresholds
	if spec.MinRankMovement == nil || *spec.MinRankMovement != DefaultGroomingMinRankMovement {
		t.Errorf("shipped min_rank_movement = %v, want DefaultGroomingMinRankMovement (%d)",
			spec.MinRankMovement, DefaultGroomingMinRankMovement)
	}
	if spec.MinScoreDelta == nil || *spec.MinScoreDelta != DefaultGroomingMinScoreDelta {
		t.Errorf("shipped min_score_delta = %v, want DefaultGroomingMinScoreDelta (%v)",
			spec.MinScoreDelta, DefaultGroomingMinScoreDelta)
	}
	if spec.SignificantHygieneDefects != nil {
		t.Errorf("shipped default declares significant_hygiene_defects = %v; it must stay OMITTED so all six remain significant",
			spec.SignificantHygieneDefects)
	}
}

// --- ACCEPTANCE-CRITERION TESTS ---------------------------------------------

// TestUnchangedBacklogProposesNothing is AC1: a report whose every entry
// matches the baseline by id and basis yields NoChangesProposed and six empty
// proposal slices.
func TestUnchangedBacklogProposesNothing(t *testing.T) {
	ord := gcOrdering("1", 1, 0.90, "R1")
	hyg := gcHygiene("2", "missing_estimate", "3")
	dup := gcDuplicate("3", "4", "high")
	dep := gcDependency("5", "6", "depends_on")
	vd := gcVisionDrift("7", "non_goal", "V2")
	dec := gcDecomposition("8", 3)
	report := gcReport("charter-a", func(gr *plan.GroomingReport) {
		gr.Ordering = []plan.OrderingEntry{ord}
		gr.HygieneDefects = []plan.HygieneDefect{hyg}
		gr.Duplicates = []plan.DuplicateCandidate{dup}
		gr.DependencyEdges = []plan.DependencyEdge{dep}
		gr.VisionDrift = []plan.VisionDriftFlag{vd}
		gr.DecompositionSuggestions = []plan.DecompositionSuggestion{dec}
	})
	base := gcBaseline("charter-a", map[string]GroomingBaselineEntry{
		ord.ID: {Class: plan.GroomingClassOrdering, Disposition: GroomingDispositionApplied,
			BasisHash: groomingOrderingBasis(ord), Rank: 1, Score: 0.90},
		hyg.ID: {Class: plan.GroomingClassHygiene, Disposition: GroomingDispositionApplied,
			BasisHash: groomingHygieneBasis(hyg)},
		dup.ID: {Class: plan.GroomingClassDuplicate, Disposition: GroomingDispositionApplied,
			BasisHash: groomingDuplicateBasis(dup)},
		dep.ID: {Class: plan.GroomingClassDependency, Disposition: GroomingDispositionApplied,
			BasisHash: groomingDependencyBasis(dep)},
		vd.ID: {Class: plan.GroomingClassVisionDrift, Disposition: GroomingDispositionApplied,
			BasisHash: groomingVisionDriftBasis(vd)},
		dec.ID: {Class: plan.GroomingClassDecomposition, Disposition: GroomingDispositionApplied,
			BasisHash: groomingDecompositionBasis(dec)},
	})

	res := FilterGroomingChurn(report, base, gcDefaultThresholds())
	if !res.NoChangesProposed {
		t.Errorf("NoChangesProposed = false, want true; proposed=%v", proposedIDs(res))
	}
	if !res.Proposals.IsEmpty() {
		t.Errorf("proposal set not empty: %+v", res.Proposals)
	}
	if !res.Summary.NoChangesProposed {
		t.Error("summary.no_changes_proposed = false, want true")
	}
	if res.Summary.Suppressed != 6 {
		t.Errorf("suppressed = %d, want 6 (every difference COMPUTED and reported, never discarded)", res.Summary.Suppressed)
	}
	// The empty set marshals as six ARRAYS, never nulls.
	b, err := json.Marshal(res.Proposals)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "null") {
		t.Errorf("empty proposal set marshalled with a null: %s", b)
	}
}

// TestSubThresholdMovementNotProposed is AC2 and counterfactual vehicle C1: a
// sub-threshold movement is COMPUTED (present in Suppressed) but not proposed,
// while each dimension crossing on its own surfaces the entry — pinning the OR
// combination in both directions.
func TestSubThresholdMovementNotProposed(t *testing.T) {
	th := gcDefaultThresholds()
	cases := []struct {
		name              string
		priorRank         int
		priorScore, score float64
		rank              int
		wantProposed      bool
	}{
		{"one position and 0.02 — both under the bar", 14, 0.70, 0.72, 15, false},
		{"three positions — rank crosses", 2, 0.70, 0.70, 5, true},
		{"0.2 score move — score crosses, rank still", 3, 0.50, 0.70, 3, true},
		{"exactly at the rank bar", 3, 0.70, 0.70, 5, true},
		{"exactly at the score bar", 3, 0.70, 0.75, 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := gcOrdering("1", tc.rank, tc.score, "R1")
			report := gcReport("h", func(gr *plan.GroomingReport) { gr.Ordering = []plan.OrderingEntry{e} })
			base := gcBaseline("h", map[string]GroomingBaselineEntry{
				e.ID: {Class: plan.GroomingClassOrdering, Disposition: GroomingDispositionApplied,
					BasisHash: groomingOrderingBasis(e), Rank: tc.priorRank, Score: tc.priorScore},
			})
			res := FilterGroomingChurn(report, base, th)
			got := containsID(proposedIDs(res), e.ID)
			if got != tc.wantProposed {
				t.Fatalf("proposed = %v, want %v (rank %d->%d, score %v->%v)",
					got, tc.wantProposed, tc.priorRank, tc.rank, tc.priorScore, tc.score)
			}
			if !tc.wantProposed {
				if r := suppressionReason(t, res, e.ID); r != GroomingSuppressBelowOrderingThreshold {
					t.Errorf("suppression reason = %q, want %q (the difference must be COMPUTED, not discarded)",
						r, GroomingSuppressBelowOrderingThreshold)
				}
			}
		})
	}
}

// TestThresholdIsOperatorDeclarable is AC3: a config declaring
// min_rank_movement=5 suppresses a 3-position move the shipped default
// proposes.
func TestThresholdIsOperatorDeclarable(t *testing.T) {
	five := 5
	conv := Default()
	conv.Grooming = &Grooming{Thresholds: &GroomingThresholdSpec{MinRankMovement: &five}}
	strict := conv.ResolveGroomingThresholds()
	if strict.MinRankMovement != 5 {
		t.Fatalf("declared min_rank_movement did not resolve: %d", strict.MinRankMovement)
	}

	e := gcOrdering("1", 5, 0.70, "R1")
	report := gcReport("h", func(gr *plan.GroomingReport) { gr.Ordering = []plan.OrderingEntry{e} })
	base := gcBaseline("h", map[string]GroomingBaselineEntry{
		e.ID: {Class: plan.GroomingClassOrdering, Disposition: GroomingDispositionApplied,
			BasisHash: groomingOrderingBasis(e), Rank: 2, Score: 0.70},
	})
	if containsID(proposedIDs(FilterGroomingChurn(report, base, strict)), e.ID) {
		t.Error("3-position move was proposed under a declared min_rank_movement of 5")
	}
	if !containsID(proposedIDs(FilterGroomingChurn(report, base, gcDefaultThresholds())), e.ID) {
		t.Error("control: the same move must be proposed under the shipped default of 2")
	}
}

// TestDiffIsAgainstAppliedNotProposedState is AC4 and counterfactual vehicle
// C3. The baselines here are built by NewGroomingBaseline over a literal prior
// report plus literal apply records, so the applied/failed distinction is
// exercised at its real construction site rather than hand-stubbed.
func TestDiffIsAgainstAppliedNotProposedState(t *testing.T) {
	landed := gcHygiene("1", "missing_estimate", "3")
	failedApply := gcHygiene("2", "missing_estimate", "5")
	idempotent := gcHygiene("3", "missing_estimate", "8")
	undecided := gcHygiene("4", "missing_estimate", "13")
	prior := gcReport("h", func(gr *plan.GroomingReport) {
		gr.HygieneDefects = []plan.HygieneDefect{landed, failedApply, idempotent, undecided}
	})
	applied := &GroomingApplyResult{
		Applied: []GroomingMutationRecord{{EntryID: landed.ID, Outcome: GroomingOutcomeApplied}},
		Failed:  []GroomingMutationRecord{{EntryID: failedApply.ID, Outcome: GroomingOutcomeFailed, Error: "provider 500"}},
		Skipped: []GroomingMutationRecord{{EntryID: idempotent.ID, Outcome: GroomingOutcomeSkipped, SkipReason: GroomingSkipAlreadyApplied}},
	}
	base := NewGroomingBaseline(prior, nil, applied)

	// The report re-proposes all four, byte-unchanged.
	report := gcReport("h", func(gr *plan.GroomingReport) {
		gr.HygieneDefects = []plan.HygieneDefect{landed, failedApply, idempotent, undecided}
	})
	res := FilterGroomingChurn(report, base, gcDefaultThresholds())

	if containsID(proposedIDs(res), landed.ID) {
		t.Error("an APPLIED entry was proposed again")
	}
	if r := suppressionReason(t, res, landed.ID); r != GroomingSuppressAlreadyApplied {
		t.Errorf("applied suppression reason = %q, want %q", r, GroomingSuppressAlreadyApplied)
	}
	if containsID(proposedIDs(res), idempotent.ID) {
		t.Error("a skipped-already_applied entry was proposed again")
	}
	if !containsID(proposedIDs(res), failedApply.ID) {
		t.Error("an entry whose apply FAILED was NOT re-proposed; a mutation that did not land must resurface (AC4)")
	}
	if !containsID(proposedIDs(res), undecided.ID) {
		t.Error("an entry that was PROPOSED but never decided was not re-proposed; the diff must be against APPLIED state, not proposed state")
	}
}

// TestRejectedProposalDoesNotReappear is AC5 — the convergence property — and
// counterfactual vehicle C4 (rejected-state suppression) and C5 (its
// changed-basis row, for the fingerprint comparison).
func TestRejectedProposalDoesNotReappear(t *testing.T) {
	th := gcDefaultThresholds()
	rejected := gcHygiene("1", "missing_estimate", "3")
	prior := gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{rejected} })

	t.Run("unchanged basis stays suppressed", func(t *testing.T) {
		base := NewGroomingBaseline(prior, []GroomingDecision{{EntryID: rejected.ID, Verdict: GroomingRejected}}, nil)
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{rejected} })
		res := FilterGroomingChurn(report, base, th)
		if containsID(proposedIDs(res), rejected.ID) {
			t.Fatal("a rejected proposal with an unchanged basis reappeared")
		}
		if r := suppressionReason(t, res, rejected.ID); r != GroomingSuppressPreviouslyRejected {
			t.Errorf("reason = %q, want %q", r, GroomingSuppressPreviouslyRejected)
		}
	})

	t.Run("re-worded PROSE alone does not resurface", func(t *testing.T) {
		base := NewGroomingBaseline(prior, []GroomingDecision{{EntryID: rejected.ID, Verdict: GroomingRejected}}, nil)
		reworded := rejected
		reworded.Detail = "an entirely different justification, same proposal"
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{reworded} })
		if containsID(proposedIDs(FilterGroomingChurn(report, base, th)), rejected.ID) {
			t.Error("re-worded prose resurfaced a rejected proposal; the fingerprint must exclude prose")
		}
	})

	t.Run("changed STRUCTURAL basis resurfaces and names the field", func(t *testing.T) {
		base := NewGroomingBaseline(prior, []GroomingDecision{{EntryID: rejected.ID, Verdict: GroomingRejected}}, nil)
		changed := gcHygiene("1", "missing_estimate", "8") // fix.field_value 3 -> 8
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{changed} })
		res := FilterGroomingChurn(report, base, th)
		if !containsID(proposedIDs(res), changed.ID) {
			t.Fatal("a changed structural basis did NOT resurface the proposal")
		}
		rec, ok := resurfaceRecord(t, res, changed.ID)
		if !ok {
			t.Fatal("no resurface record; AC5 requires the report to say what changed")
		}
		if rec.Reason != GroomingResurfaceBasisChanged || rec.ChangedField != "fix" {
			t.Errorf("resurface = %+v, want reason %q changed_field %q", rec, GroomingResurfaceBasisChanged, "fix")
		}
	})

	// #2847: the hygiene basis moved off `suggested_fix` onto the structured
	// `fix` — the value the apply path actually dispatches. Both halves are
	// asserted, because either alone is weak: a basis that ignored `fix` would
	// suppress a genuinely different label set, and one that still hashed the
	// prose would resurface an unchanged proposal on a re-worded sentence.
	t.Run("a re-worded suggested_fix alone does not resurface", func(t *testing.T) {
		base := NewGroomingBaseline(prior, []GroomingDecision{{EntryID: rejected.ID, Verdict: GroomingRejected}}, nil)
		reworded := rejected
		reworded.SuggestedFix = "Please set the estimate to three points."
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{reworded} })
		if containsID(proposedIDs(FilterGroomingChurn(report, base, th)), rejected.ID) {
			t.Error("a re-worded suggested_fix resurfaced a rejected proposal; the fingerprint must cover `fix`, not the prose")
		}
	})

	t.Run("a changed LABEL SET resurfaces and names the fix field", func(t *testing.T) {
		before := gcHygiene("1", "missing_label_namespace", "")
		before.Fix = &plan.HygieneFix{Labels: []string{"area:api"}}
		base := NewGroomingBaseline(
			gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{before} }),
			[]GroomingDecision{{EntryID: before.ID, Verdict: GroomingRejected}}, nil)

		after := before
		after.Fix = &plan.HygieneFix{Labels: []string{"area:server-api"}}
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{after} })
		res := FilterGroomingChurn(report, base, th)
		if !containsID(proposedIDs(res), after.ID) {
			t.Fatal("a different proposed LABEL did not resurface; two different label sets must not be one proposal")
		}
		rec, ok := resurfaceRecord(t, res, after.ID)
		if !ok || rec.ChangedField != "fix" {
			t.Errorf("resurface = %+v (ok=%t), want changed_field %q", rec, ok, "fix")
		}
	})

	t.Run("board_state case alone does NOT resurface", func(t *testing.T) {
		// The fingerprint lower-cases the board state, exactly as
		// groomingResolveBoardState lower-cases its lookup (condition C6):
		// `Backlog` and `backlog` are ONE proposal on both sides.
		before := gcHygiene("1", "unboarded", "")
		before.Fix = &plan.HygieneFix{BoardState: "backlog"}
		base := NewGroomingBaseline(
			gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{before} }),
			[]GroomingDecision{{EntryID: before.ID, Verdict: GroomingRejected}}, nil)

		after := before
		after.Fix = &plan.HygieneFix{BoardState: "Backlog"}
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{after} })
		if containsID(proposedIDs(FilterGroomingChurn(report, base, th)), after.ID) {
			t.Error("a board_state case change resurfaced the proposal; the basis and the lookup must agree on case")
		}
	})

	t.Run("an AMENDED verdict suppresses like a rejection", func(t *testing.T) {
		base := NewGroomingBaseline(prior, []GroomingDecision{{EntryID: rejected.ID, Verdict: GroomingAmended}}, nil)
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{rejected} })
		res := FilterGroomingChurn(report, base, th)
		if containsID(proposedIDs(res), rejected.ID) {
			t.Fatal("an amended entry reappeared")
		}
		if r := suppressionReason(t, res, rejected.ID); r != GroomingSuppressPreviouslyRejected {
			t.Errorf("reason = %q, want %q", r, GroomingSuppressPreviouslyRejected)
		}
	})

	// J3: confidence is an agent-regenerated judgment and must NOT be a raw
	// fingerprint input. A rejected duplicate whose confidence drifts one band
	// is the sub-threshold-jitter analogue of the ordering row above.
	t.Run("a rejected duplicate does not resurface on a confidence flip", func(t *testing.T) {
		dup := gcDuplicate("3", "4", "medium")
		priorDup := gcReport("h", func(gr *plan.GroomingReport) { gr.Duplicates = []plan.DuplicateCandidate{dup} })
		base := NewGroomingBaseline(priorDup, []GroomingDecision{{EntryID: dup.ID, Verdict: GroomingRejected}}, nil)
		jittered := gcDuplicate("3", "4", "high")
		jittered.Basis = "re-worded basis prose too"
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.Duplicates = []plan.DuplicateCandidate{jittered} })
		res := FilterGroomingChurn(report, base, th)
		if containsID(proposedIDs(res), dup.ID) {
			t.Error("a confidence flip medium->high resurfaced a rejected duplicate; confidence must not be a fingerprint input")
		}
		if r := suppressionReason(t, res, dup.ID); r != GroomingSuppressPreviouslyRejected {
			t.Errorf("reason = %q, want %q", r, GroomingSuppressPreviouslyRejected)
		}
	})

	// The decomposition trade, pinned in both directions.
	t.Run("decomposition: same child count suppressed, different count proposed", func(t *testing.T) {
		three := gcDecomposition("9", 3)
		priorDec := gcReport("h", func(gr *plan.GroomingReport) { gr.DecompositionSuggestions = []plan.DecompositionSuggestion{three} })
		base := NewGroomingBaseline(priorDec, []GroomingDecision{{EntryID: three.ID, Verdict: GroomingRejected}}, nil)

		reworded := gcDecomposition("9", 3)
		reworded.ProposedChildren[0].Title = "a completely different child title"
		r1 := FilterGroomingChurn(gcReport("h", func(gr *plan.GroomingReport) {
			gr.DecompositionSuggestions = []plan.DecompositionSuggestion{reworded}
		}), base, th)
		if containsID(proposedIDs(r1), three.ID) {
			t.Error("a same-count re-worded split resurfaced; the fingerprint is deliberately the child COUNT")
		}

		four := gcDecomposition("9", 4)
		r2 := FilterGroomingChurn(gcReport("h", func(gr *plan.GroomingReport) {
			gr.DecompositionSuggestions = []plan.DecompositionSuggestion{four}
		}), base, th)
		if !containsID(proposedIDs(r2), three.ID) {
			t.Error("a different-count split did NOT resurface; the fingerprint is not inert")
		}
	})
}

// TestTwoRunsOverUnchangedBacklogAreByteIdentical is AC6 and counterfactual
// vehicle C7. The shuffled-input row is the vehicle for the deterministic sort:
// agent array-order jitter must not change the serialized bytes.
func TestTwoRunsOverUnchangedBacklogAreByteIdentical(t *testing.T) {
	th := gcDefaultThresholds()
	var ord []plan.OrderingEntry
	var hyg []plan.HygieneDefect
	entries := map[string]GroomingBaselineEntry{}
	for i := 1; i <= 8; i++ {
		e := gcOrdering(string(rune('0'+i)), i, float64(i)/10, "R1")
		ord = append(ord, e)
		entries[e.ID] = GroomingBaselineEntry{Class: plan.GroomingClassOrdering,
			Disposition: GroomingDispositionApplied, BasisHash: groomingOrderingBasis(e), Rank: i + 90, Score: float64(i) / 10}
		hyg = append(hyg, gcHygiene(string(rune('0'+i)), "missing_estimate", "3"))
	}
	report := gcReport("h", func(gr *plan.GroomingReport) {
		gr.Ordering = append([]plan.OrderingEntry(nil), ord...)
		gr.HygieneDefects = append([]plan.HygieneDefect(nil), hyg...)
	})

	base := gcBaseline("h", entries)
	first, err := json.Marshal(FilterGroomingChurn(report, base, th).Proposals)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := json.Marshal(FilterGroomingChurn(report, base, th).Proposals)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("two identical runs differ:\n%s\n%s", first, second)
	}

	// Shuffled input, same content: the property AC6 actually depends on.
	rng := rand.New(rand.NewSource(1))
	shuffled := gcReport("h", func(gr *plan.GroomingReport) {
		gr.Ordering = append([]plan.OrderingEntry(nil), ord...)
		gr.HygieneDefects = append([]plan.HygieneDefect(nil), hyg...)
		rng.Shuffle(len(gr.Ordering), func(i, j int) { gr.Ordering[i], gr.Ordering[j] = gr.Ordering[j], gr.Ordering[i] })
		rng.Shuffle(len(gr.HygieneDefects), func(i, j int) {
			gr.HygieneDefects[i], gr.HygieneDefects[j] = gr.HygieneDefects[j], gr.HygieneDefects[i]
		})
	})
	third, err := json.Marshal(FilterGroomingChurn(shuffled, base, th).Proposals)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(third) != string(first) {
		t.Fatalf("shuffled input changed the serialized bytes:\n%s\n%s", first, third)
	}
}

// --- J2: the hygiene significance bar governs NEW findings too --------------

// TestHygieneOutsideSignificanceSetSuppressed is counterfactual vehicle C2 and
// #2240 approval condition J2's proof: a hygiene defect the operator declared
// insignificant is suppressed on its FIRST appearance — absent from the
// baseline entirely — not proposed once and only then suppressible.
func TestHygieneOutsideSignificanceSetSuppressed(t *testing.T) {
	conv := Default()
	conv.Grooming = &Grooming{Thresholds: &GroomingThresholdSpec{
		SignificantHygieneDefects: []string{"absent_done_means"},
	}}
	th := conv.ResolveGroomingThresholds()

	insignificant := gcHygiene("1", "unboarded", "true")
	significant := gcHygiene("2", "absent_done_means", "")
	report := gcReport("h", func(gr *plan.GroomingReport) {
		gr.HygieneDefects = []plan.HygieneDefect{insignificant, significant}
	})

	t.Run("NEW finding, empty baseline", func(t *testing.T) {
		res := FilterGroomingChurn(report, gcBaseline("", nil), th)
		if containsID(proposedIDs(res), insignificant.ID) {
			t.Error("a sub-significance hygiene defect was proposed on its FIRST appearance; the bar must gate new findings too (J2)")
		}
		if r := suppressionReason(t, res, insignificant.ID); r != GroomingSuppressBelowHygieneSignificance {
			t.Errorf("reason = %q, want %q", r, GroomingSuppressBelowHygieneSignificance)
		}
		if !containsID(proposedIDs(res), significant.ID) {
			t.Error("a NEW in-set hygiene defect was not proposed; the bar must not suppress everything")
		}
	})

	t.Run("baseline-matched finding", func(t *testing.T) {
		base := gcBaseline("h", map[string]GroomingBaselineEntry{
			insignificant.ID: {Class: plan.GroomingClassHygiene, Disposition: GroomingDispositionRejected,
				BasisHash: groomingHygieneBasis(insignificant)},
		})
		res := FilterGroomingChurn(report, base, th)
		if r := suppressionReason(t, res, insignificant.ID); r != GroomingSuppressBelowHygieneSignificance {
			t.Errorf("reason = %q, want %q — the significance bar runs BEFORE the baseline lookup", r, GroomingSuppressBelowHygieneSignificance)
		}
	})

	t.Run("a CHANGED basis does not defeat the bar", func(t *testing.T) {
		base := gcBaseline("h", map[string]GroomingBaselineEntry{
			insignificant.ID: {Class: plan.GroomingClassHygiene, Disposition: GroomingDispositionRejected,
				BasisHash: "a-completely-different-hash"},
		})
		res := FilterGroomingChurn(report, base, th)
		if containsID(proposedIDs(res), insignificant.ID) {
			t.Error("a changed basis routed an insignificant defect around the significance bar")
		}
	})
}

// TestCharterChangeLiftsCharterAnchoredSuppressionOnly is counterfactual
// vehicle C6. Both halves are asserted in ONE call, so deleting the lift OR
// deleting the class restriction reddens it.
func TestCharterChangeLiftsCharterAnchoredSuppressionOnly(t *testing.T) {
	ord := gcOrdering("1", 3, 0.70, "R1")
	vd := gcVisionDrift("2", "non_goal", "V2")
	hyg := gcHygiene("3", "missing_estimate", "3")
	dep := gcDependency("4", "5", "depends_on")
	dup := gcDuplicate("6", "7", "high")
	dec := gcDecomposition("8", 2)

	report := gcReport("charter-NEW", func(gr *plan.GroomingReport) {
		gr.Ordering = []plan.OrderingEntry{ord}
		gr.VisionDrift = []plan.VisionDriftFlag{vd}
		gr.HygieneDefects = []plan.HygieneDefect{hyg}
		gr.DependencyEdges = []plan.DependencyEdge{dep}
		gr.Duplicates = []plan.DuplicateCandidate{dup}
		gr.DecompositionSuggestions = []plan.DecompositionSuggestion{dec}
	})
	base := gcBaseline("charter-OLD", map[string]GroomingBaselineEntry{
		ord.ID: {Class: plan.GroomingClassOrdering, Disposition: GroomingDispositionRejected,
			BasisHash: groomingOrderingBasis(ord), Rank: 3, Score: 0.70},
		vd.ID: {Class: plan.GroomingClassVisionDrift, Disposition: GroomingDispositionRejected,
			BasisHash: groomingVisionDriftBasis(vd)},
		hyg.ID: {Class: plan.GroomingClassHygiene, Disposition: GroomingDispositionApplied,
			BasisHash: groomingHygieneBasis(hyg)},
		dep.ID: {Class: plan.GroomingClassDependency, Disposition: GroomingDispositionApplied,
			BasisHash: groomingDependencyBasis(dep)},
		dup.ID: {Class: plan.GroomingClassDuplicate, Disposition: GroomingDispositionRejected,
			BasisHash: groomingDuplicateBasis(dup)},
		dec.ID: {Class: plan.GroomingClassDecomposition, Disposition: GroomingDispositionRejected,
			BasisHash: groomingDecompositionBasis(dec)},
	})

	res := FilterGroomingChurn(report, base, gcDefaultThresholds())
	if !res.CharterChanged {
		t.Fatal("CharterChanged = false, want true")
	}
	for _, id := range []string{ord.ID, vd.ID} {
		if !containsID(proposedIDs(res), id) {
			t.Errorf("charter-anchored entry %q stayed suppressed under a MOVED charter", id)
		}
		rec, ok := resurfaceRecord(t, res, id)
		if !ok || rec.Reason != GroomingResurfaceCharterChanged {
			t.Errorf("entry %q: resurface = %+v (ok=%v), want reason %q", id, rec, ok, GroomingResurfaceCharterChanged)
		}
	}
	for _, id := range []string{hyg.ID, dep.ID, dup.ID, dec.ID} {
		if containsID(proposedIDs(res), id) {
			t.Errorf("OBJECTIVE entry %q was lifted by a charter change; an applied label fix does not become un-applied because the charter was edited", id)
		}
	}

	t.Run("an UNCHANGED charter lifts nothing", func(t *testing.T) {
		same := gcBaseline("charter-NEW", base.Entries)
		r := FilterGroomingChurn(report, same, gcDefaultThresholds())
		if r.CharterChanged {
			t.Error("CharterChanged = true for an identical hash")
		}
		if !r.NoChangesProposed {
			t.Errorf("an unchanged charter proposed %v", proposedIDs(r))
		}
	})

	t.Run("an ABSENT charter_ref is unknown, not moved", func(t *testing.T) {
		noRef := gcReport("", func(gr *plan.GroomingReport) {
			gr.Ordering = []plan.OrderingEntry{ord}
			gr.VisionDrift = []plan.VisionDriftFlag{vd}
		})
		r := FilterGroomingChurn(noRef, base, gcDefaultThresholds())
		if r.CharterChanged {
			t.Error("an absent charter_ref was read as a MOVED charter; unknown must not lift every suppression")
		}
	})
}

// --- PER-FAILURE-MODE TESTS -------------------------------------------------

// TestFilterFailureModes covers one behavioral assertion per named defensive
// branch, not a happy path plus a subset.
func TestFilterFailureModes(t *testing.T) {
	th := gcDefaultThresholds()

	t.Run("F1 nil report", func(t *testing.T) {
		res := FilterGroomingChurn(nil, gcBaseline("h", nil), th)
		if !res.NoChangesProposed || !res.Proposals.IsEmpty() {
			t.Errorf("nil report: NoChangesProposed=%v empty=%v, want true/true", res.NoChangesProposed, res.Proposals.IsEmpty())
		}
		if !res.Summary.NoChangesProposed {
			t.Error("nil report: summary.no_changes_proposed = false")
		}
	})

	t.Run("F2 empty baseline proposes everything", func(t *testing.T) {
		ord := gcOrdering("1", 1, 0.9, "R1")
		hyg := gcHygiene("2", "missing_estimate", "3")
		report := gcReport("h", func(gr *plan.GroomingReport) {
			gr.Ordering = []plan.OrderingEntry{ord}
			gr.HygieneDefects = []plan.HygieneDefect{hyg}
		})
		res := FilterGroomingChurn(report, GroomingBaseline{}, th)
		if len(proposedIDs(res)) != 2 {
			t.Errorf("first-ever run proposed %v, want both entries", proposedIDs(res))
		}
		if len(res.Suppressed) != 0 {
			t.Errorf("first-ever run suppressed %+v, want none", res.Suppressed)
		}
	})

	t.Run("F3 no grooming block resolves the package defaults", func(t *testing.T) {
		got := Conventions{}.ResolveGroomingThresholds()
		if got.MinRankMovement != DefaultGroomingMinRankMovement || got.MinScoreDelta != DefaultGroomingMinScoreDelta {
			t.Errorf("resolve with no grooming block = %+v, want the package defaults", got)
		}
		if len(got.SignificantHygieneDefects) != len(DefaultSignificantHygieneDefects()) {
			t.Errorf("significance set = %v, want all six", got.SignificantHygieneDefects)
		}
	})

	t.Run("F4 sub-floor declared thresholds CLAMP to the defaults", func(t *testing.T) {
		zero, neg := 0, -1.0
		conv := Conventions{Grooming: &Grooming{Thresholds: &GroomingThresholdSpec{
			MinRankMovement:           &zero,
			MinScoreDelta:             &neg,
			SignificantHygieneDefects: []string{"   ", ""},
		}}}
		got := conv.ResolveGroomingThresholds()
		if got.MinRankMovement != DefaultGroomingMinRankMovement {
			t.Errorf("min_rank_movement = %d, want the default %d — 0 would disable the guard",
				got.MinRankMovement, DefaultGroomingMinRankMovement)
		}
		if got.MinScoreDelta != DefaultGroomingMinScoreDelta {
			t.Errorf("min_score_delta = %v, want the default %v", got.MinScoreDelta, DefaultGroomingMinScoreDelta)
		}
		if len(got.SignificantHygieneDefects) != len(DefaultSignificantHygieneDefects()) {
			t.Errorf("an all-blank significance set resolved to %v, want the default six", got.SignificantHygieneDefects)
		}
		for _, want := range []string{"min_rank_movement", "min_score_delta", "significant_hygiene_defects"} {
			if !containsID(got.Clamped, want) {
				t.Errorf("clamped = %v, missing %q — a silent substitution hides that the declaration did not take", got.Clamped, want)
			}
		}
	})

	t.Run("F5 unknown baseline class fails OPEN toward proposing", func(t *testing.T) {
		hyg := gcHygiene("1", "missing_estimate", "3")
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{hyg} })
		base := gcBaseline("h", map[string]GroomingBaselineEntry{
			hyg.ID: {Class: "not_a_class", Disposition: GroomingDispositionApplied, BasisHash: groomingHygieneBasis(hyg)},
		})
		res := FilterGroomingChurn(report, base, th)
		if !containsID(proposedIDs(res), hyg.ID) {
			t.Fatal("an unreadable baseline record silently SUPPRESSED its entry")
		}
		if len(res.Summary.Anomalies) == 0 || !strings.Contains(res.Summary.Anomalies[0], "unknown_baseline_class") {
			t.Errorf("anomalies = %v, want the unknown class named", res.Summary.Anomalies)
		}
	})

	t.Run("F6 unrecognized disposition fails OPEN toward proposing", func(t *testing.T) {
		hyg := gcHygiene("1", "missing_estimate", "3")
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.HygieneDefects = []plan.HygieneDefect{hyg} })
		base := gcBaseline("h", map[string]GroomingBaselineEntry{
			hyg.ID: {Class: plan.GroomingClassHygiene, Disposition: GroomingDisposition("pending"),
				BasisHash: groomingHygieneBasis(hyg)},
		})
		res := FilterGroomingChurn(report, base, th)
		if !containsID(proposedIDs(res), hyg.ID) {
			t.Fatal("a baseline record with neither recorded disposition silently SUPPRESSED its entry")
		}
		if len(res.Summary.Anomalies) == 0 || !strings.Contains(res.Summary.Anomalies[0], "unknown_baseline_disposition") {
			t.Errorf("anomalies = %v, want the unknown disposition named", res.Summary.Anomalies)
		}
	})

	// The hygiene row above cannot reach the ordering-threshold branch, and that
	// branch is the one that used to suppress a degraded record: it returned
	// below_ordering_threshold without ever reading the disposition. So the
	// SUB-THRESHOLD move is load-bearing here — a supra-threshold move would be
	// proposed by the threshold branch itself and prove nothing.
	t.Run("F6b unrecognized disposition on a SUB-THRESHOLD ordering entry still proposes", func(t *testing.T) {
		ord := gcOrdering("1", 1, 0.90, "R1")
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.Ordering = []plan.OrderingEntry{ord} })
		base := gcBaseline("h", map[string]GroomingBaselineEntry{
			// Rank and score are IDENTICAL to the report's, so both deltas are 0
			// — under every bar — and the basis hash matches, so the only thing
			// standing between this entry and a silent suppression is the
			// disposition check.
			ord.ID: {Class: plan.GroomingClassOrdering, Disposition: GroomingDisposition(""),
				BasisHash: groomingOrderingBasis(ord), Rank: 1, Score: 0.90},
		})
		res := FilterGroomingChurn(report, base, th)
		if !containsID(proposedIDs(res), ord.ID) {
			t.Fatalf("an ordering baseline entry with NO recorded disposition was suppressed as %q; a suppression control that cannot read its own record must PROPOSE",
				suppressionReason(t, res, ord.ID))
		}
		rec, ok := resurfaceRecord(t, res, ord.ID)
		if !ok || rec.Reason != GroomingResurfaceUnknownBaselineDisposition || rec.ChangedField != "disposition" {
			t.Errorf("resurface record = %+v (found=%v), want reason %q on field disposition",
				rec, ok, GroomingResurfaceUnknownBaselineDisposition)
		}
		if len(res.Summary.Anomalies) == 0 || !strings.Contains(res.Summary.Anomalies[0], "unknown_baseline_disposition") {
			t.Errorf("anomalies = %v, want the unknown disposition named", res.Summary.Anomalies)
		}
	})

	t.Run("F7 duplicate entry id stays deterministic rather than panicking", func(t *testing.T) {
		e := gcOrdering("1", 1, 0.9, "R1")
		report := gcReport("h", func(gr *plan.GroomingReport) { gr.Ordering = []plan.OrderingEntry{e, e} })
		a, err := json.Marshal(FilterGroomingChurn(report, GroomingBaseline{}, th).Proposals)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b, err := json.Marshal(FilterGroomingChurn(report, GroomingBaseline{}, th).Proposals)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(a) != string(b) {
			t.Errorf("an unvalidated duplicate id produced non-deterministic output:\n%s\n%s", a, b)
		}
	})
}

// TestNewGroomingBaselineDispositionTable pins the constructor's disposition
// rules directly, including the ABSENCE cases that are the fail-safe.
func TestNewGroomingBaselineDispositionTable(t *testing.T) {
	if got := NewGroomingBaseline(nil, nil, nil); len(got.Entries) != 0 || got.CharterHash != "" {
		t.Errorf("nil prior report = %+v, want the zero baseline", got)
	}

	approvedNoApply := gcHygiene("1", "missing_estimate", "3")
	rejected := gcHygiene("2", "missing_estimate", "5")
	landed := gcHygiene("3", "missing_estimate", "8")
	prior := gcReport("charter-x", func(gr *plan.GroomingReport) {
		gr.HygieneDefects = []plan.HygieneDefect{approvedNoApply, rejected, landed}
	})
	base := NewGroomingBaseline(prior,
		[]GroomingDecision{
			{EntryID: approvedNoApply.ID, Verdict: GroomingApproved},
			{EntryID: rejected.ID, Verdict: GroomingRejected},
		},
		&GroomingApplyResult{Applied: []GroomingMutationRecord{{EntryID: landed.ID, Outcome: GroomingOutcomeApplied}}})

	if base.CharterHash != "charter-x" {
		t.Errorf("charter hash = %q, want %q", base.CharterHash, "charter-x")
	}
	if _, ok := base.Entries[approvedNoApply.ID]; ok {
		t.Error("an APPROVED entry with no apply record entered the baseline; only a LANDED mutation counts (AC4)")
	}
	if got := base.Entries[rejected.ID].Disposition; got != GroomingDispositionRejected {
		t.Errorf("rejected disposition = %q, want %q", got, GroomingDispositionRejected)
	}
	if got := base.Entries[landed.ID].Disposition; got != GroomingDispositionApplied {
		t.Errorf("applied disposition = %q, want %q", got, GroomingDispositionApplied)
	}

	t.Run("APPLIED wins over a rejection verdict", func(t *testing.T) {
		b := NewGroomingBaseline(prior,
			[]GroomingDecision{{EntryID: landed.ID, Verdict: GroomingRejected}},
			&GroomingApplyResult{Applied: []GroomingMutationRecord{{EntryID: landed.ID, Outcome: GroomingOutcomeApplied}}})
		if got := b.Entries[landed.ID].Disposition; got != GroomingDispositionApplied {
			t.Errorf("disposition = %q, want %q — a landed mutation is a fact, a verdict is an intent", got, GroomingDispositionApplied)
		}
	})

	t.Run("a non-already_applied skip records NO disposition", func(t *testing.T) {
		b := NewGroomingBaseline(prior, nil, &GroomingApplyResult{
			Skipped: []GroomingMutationRecord{{EntryID: landed.ID, Outcome: GroomingOutcomeSkipped,
				SkipReason: GroomingSkipDestructiveNotAuthorized}},
		})
		if _, ok := b.Entries[landed.ID]; ok {
			t.Error("a containment SKIP entered the baseline; only applied/rejected are recorded states")
		}
	})

	t.Run("a prior report with no charter_ref carries no charter hash", func(t *testing.T) {
		b := NewGroomingBaseline(gcReport("", func(gr *plan.GroomingReport) {
			gr.HygieneDefects = []plan.HygieneDefect{landed}
		}), nil, &GroomingApplyResult{Applied: []GroomingMutationRecord{{EntryID: landed.ID, Outcome: GroomingOutcomeApplied}}})
		if b.CharterHash != "" {
			t.Errorf("charter hash = %q, want empty", b.CharterHash)
		}
	})
}

// TestGroomingBasisExcludesProse pins the load-bearing exclusion in both
// directions per class: re-worded prose leaves the fingerprint identical, and a
// changed STRUCTURAL field changes it.
func TestGroomingBasisExcludesProse(t *testing.T) {
	ord := gcOrdering("1", 1, 0.9, "R1", "V2")
	ordProse := ord
	ordProse.Rationale = "different prose"
	ordProse.RubricCitations = []plan.RubricCitation{{RubricID: "v2", Quote: "different quote"}, {RubricID: "R1", Note: "n"}}
	if groomingOrderingBasis(ord) != groomingOrderingBasis(ordProse) {
		t.Error("ordering: prose/citation-order/case changed the fingerprint; rank+score+prose must be excluded and rubric ids normalized")
	}
	ordStruct := gcOrdering("1", 1, 0.9, "R1", "S3")
	if groomingOrderingBasis(ord) == groomingOrderingBasis(ordStruct) {
		t.Error("ordering: a changed rubric-citation SET did not change the fingerprint")
	}

	// Rank and score are excluded so the numeric thresholds — not equality —
	// decide ordering movement.
	ordMoved := gcOrdering("1", 9, 0.1, "R1", "V2")
	if groomingOrderingBasis(ord) != groomingOrderingBasis(ordMoved) {
		t.Error("ordering: rank/score are in the fingerprint, which routes every jitter around the threshold bar")
	}

	dep := gcDependency("1", "2", "depends_on")
	depProse := dep
	depProse.Basis = "different"
	if groomingDependencyBasis(dep) != groomingDependencyBasis(depProse) {
		t.Error("dependency: prose basis changed the fingerprint")
	}

	vd := gcVisionDrift("1", "non_goal", "V2")
	vdProse := vd
	vdProse.Detail = "different"
	if groomingVisionDriftBasis(vd) != groomingVisionDriftBasis(vdProse) {
		t.Error("vision_drift: prose detail changed the fingerprint")
	}
	if groomingVisionDriftBasis(vd) == groomingVisionDriftBasis(gcVisionDrift("1", "phase_theme", "V2")) {
		t.Error("vision_drift: the basis ENUM must change the fingerprint")
	}

	hyg := gcHygiene("1", "missing_estimate", "3")
	hygProse := hyg
	hygProse.Detail = "different"
	if groomingHygieneBasis(hyg) != groomingHygieneBasis(hygProse) {
		t.Error("hygiene: prose detail changed the fingerprint")
	}

	// THE TWO SEPARATOR BOUNDARIES, pinned rather than asserted in prose. Each
	// pair is chosen so it COLLIDES under the corresponding naive
	// concatenation, which is what makes it a counterfactual vehicle: a
	// one-element label set joins to itself whatever the separator, so a pair
	// built on one is separator-blind and pins nothing.
	//
	// (1) The label SET boundary: joining on US (\x1f) keeps ["ab"] and
	// ["a", "b"] apart, where an empty join would fingerprint them
	// identically and merge two different proposals into one.
	withHygieneFix := func(f *plan.HygieneFix) plan.HygieneDefect {
		d := gcHygiene("1", "missing_label_namespace", "")
		d.Fix = f
		return d
	}
	oneLabel := withHygieneFix(&plan.HygieneFix{Labels: []string{"ab"}})
	twoLabels := withHygieneFix(&plan.HygieneFix{Labels: []string{"a", "b"}})
	if groomingHygieneBasis(oneLabel) == groomingHygieneBasis(twoLabels) {
		t.Error("hygiene: two DIFFERENT label sets share a fingerprint; the label-set join separator is not separating")
	}

	// (2) The MEMBER boundary: fields are joined on NUL by groomingBasisHash,
	// so a label cannot forge the field_value's contribution. Without that
	// separator both of these hash "ab".
	labelOnly := withHygieneFix(&plan.HygieneFix{Labels: []string{"ab"}})
	labelPlusValue := withHygieneFix(&plan.HygieneFix{Labels: []string{"a"}, FieldValue: "b"})
	if groomingHygieneBasis(labelOnly) == groomingHygieneBasis(labelPlusValue) {
		t.Error("hygiene: a label forged the field_value's contribution to the fingerprint; the member separator is not separating")
	}

	// RESIDUAL, stated honestly: a label containing \x1f ITSELF would still
	// collide with the equivalent two-label set in this fingerprint. That
	// label is refused pre-dispatch by groomingValidLabel's control-rune rule
	// (unicode.IsSpace does not match \x1f, unicode.IsControl does), so it can
	// never be applied — the collision is confined to churn suppression of a
	// proposal that is undispatchable anyway.
	if groomingValidLabel("a\x1fb") {
		t.Error("a label carrying the label-set join separator must be undispatchable, which is what bounds the residual collision above")
	}

	// Cross-class collision guard: identical payloads under different classes
	// must not share a fingerprint.
	if groomingDuplicateBasis(gcDuplicate("1", "2", "high")) == groomingBasisHash(plan.GroomingClassHygiene) {
		t.Error("class tag missing: two classes collided on an identical payload")
	}
}
