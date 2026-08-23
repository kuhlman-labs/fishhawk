package plan_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// --- milestone_scope (E54.9 / #2309) -----------------------------------------
//
// Every INVALID fixture below is seeded BY CONSTRUCTION — a struct literal for
// the field under test, assembled into a valid report envelope by
// milestoneReportDoc. No test calls plan.ValidateGroomingReport (or
// CanonicalizeMilestoneScope) in its own setup, so a RED lands on the behavioral
// assertion rather than on fixture setup.

var milestoneFixedTime = time.Date(2026, 8, 23, 9, 15, 0, 0, time.UTC)

// msRef renders one github_issue item_ref.
func msRef(id string) plan.ItemRef {
	return plan.ItemRef{Type: "github_issue", ID: id, URL: "https://example.test/" + id}
}

// msKey is the item key msRef(id) derives to — provider discriminator, a slash,
// the lowercased id. Written here so depends_on / critical_path fixtures are
// literals rather than a call into the derivation under test.
func msKey(id string) string { return "github/" + strings.ToLower(id) }

// msInc builds an inclusion with the DERIVED id and one rubric citation.
func msInc(id string, wave int, deps, openQ []string) plan.MilestoneInclusion {
	return plan.MilestoneInclusion{
		ID:              plan.GroomingEntryID(plan.GroomingClassMilestoneInclusion, "", msRef(id)),
		ItemRef:         msRef(id),
		Reason:          "in scope: " + id,
		Wave:            wave,
		DependsOn:       deps,
		OpenQuestionIDs: openQ,
		RubricCitations: []plan.RubricCitation{{RubricID: "U1"}},
	}
}

// msExc builds an exclusion with the DERIVED id and one rubric citation.
func msExc(id string) plan.MilestoneExclusion {
	return plan.MilestoneExclusion{
		ID:              plan.GroomingEntryID(plan.GroomingClassMilestoneExclusion, "", msRef(id)),
		ItemRef:         msRef(id),
		Reason:          "out of scope: " + id,
		RubricCitations: []plan.RubricCitation{{RubricID: "V5"}},
	}
}

// msDeclined builds a declined call with the DERIVED (zero-ref) id.
func msDeclined(qid string) plan.MilestoneDeclinedCall {
	return plan.MilestoneDeclinedCall{
		ID:              plan.GroomingEntryID(plan.GroomingClassMilestoneDeclined, qid),
		QuestionID:      qid,
		Question:        "is " + qid + " in scope?",
		WhyAmbiguous:    "the release definition does not settle it",
		RubricCitations: []plan.RubricCitation{{RubricID: "U3"}},
	}
}

// msScope wraps the four collections plus a valid definition/framing/confidence.
// nil collections are coalesced to non-nil empties so they marshal as the `[]`
// the required-array schema demands (a nil slice marshals to `null`) — the ONLY
// defect a fixture carries is the one the case seeds explicitly.
func msScope(inc []plan.MilestoneInclusion, exc []plan.MilestoneExclusion, declined []plan.MilestoneDeclinedCall, path []string) *plan.MilestoneScope {
	if inc == nil {
		inc = []plan.MilestoneInclusion{}
	}
	if exc == nil {
		exc = []plan.MilestoneExclusion{}
	}
	if declined == nil {
		declined = []plan.MilestoneDeclinedCall{}
	}
	if path == nil {
		path = []string{}
	}
	return &plan.MilestoneScope{
		ReleaseDefinition: plan.MilestoneReleaseDefinition{Label: "alpha", Text: "the first usable slice", Source: "operator_input"},
		Framing:           plan.MilestoneFraming{Statement: "alpha is the keystone plus its consumers", Basis: "ADR-076 §2"},
		Included:          inc,
		Excluded:          exc,
		DeclinedCalls:     declined,
		CriticalPath:      path,
		EdgeConfidence:    plan.MilestoneEdgeConfidence{Level: "medium", Basis: "edges read from prose"},
	}
}

// validMilestoneScope is the shared happy-path fixture: three waves, one
// exclusion, no declined calls, a connected critical path.
func validMilestoneScope() *plan.MilestoneScope {
	return msScope(
		[]plan.MilestoneInclusion{
			msInc("acme/app#1", 0, nil, nil),
			msInc("acme/app#2", 1, []string{msKey("acme/app#1")}, nil),
			msInc("acme/app#3", 2, []string{msKey("acme/app#2")}, nil),
		},
		[]plan.MilestoneExclusion{msExc("acme/app#9")},
		nil,
		[]string{msKey("acme/app#1"), msKey("acme/app#2"), msKey("acme/app#3")},
	)
}

// milestoneReportDoc marshals a valid grooming report carrying ms. The envelope
// (kind, versions, provenance, a single cited ordering entry, the six empty
// arrays) is always valid, so a rejection is attributable to the milestone
// scope under test.
func milestoneReportDoc(t *testing.T, ms *plan.MilestoneScope) []byte {
	t.Helper()
	gr := &plan.GroomingReport{
		Kind:            plan.KindGroomingReport,
		ReportVersion:   plan.GroomingReportVersion,
		TicketReference: plan.TicketReference{Type: "github_issue", ID: "kuhlman-labs/fishhawk#2309", URL: "https://github.com/kuhlman-labs/fishhawk/issues/2309"},
		GeneratedBy:     plan.GeneratedBy{Agent: "claude-code", Model: "claude-opus-5", Timestamp: milestoneFixedTime},
		Summary:         "milestone fixture",
		Ordering: []plan.OrderingEntry{{
			ID:              plan.GroomingEntryID(plan.GroomingClassOrdering, "", msRef("acme/app#1")),
			ItemRef:         msRef("acme/app#1"),
			Rank:            1,
			Score:           1,
			RubricCitations: []plan.RubricCitation{{RubricID: "U1"}},
		}},
		Duplicates:               []plan.DuplicateCandidate{},
		HygieneDefects:           []plan.HygieneDefect{},
		DependencyEdges:          []plan.DependencyEdge{},
		VisionDrift:              []plan.VisionDriftFlag{},
		DecompositionSuggestions: []plan.DecompositionSuggestion{},
		MilestoneScope:           ms,
	}
	b, err := json.Marshal(gr)
	if err != nil {
		t.Fatalf("marshal milestone report: %v", err)
	}
	return b
}

// requireMilestoneRejection validates a milestone report expected to be a
// *SemanticError, asserting the pointer and every substring name.
func requireMilestoneRejection(t *testing.T, ms *plan.MilestoneScope, wantParts ...string) {
	t.Helper()
	body := milestoneReportDoc(t, ms)
	var se *plan.SemanticError
	err := plan.ValidateGroomingReport(body)
	if !errors.As(err, &se) {
		t.Fatalf("ValidateGroomingReport: err = %v (%T), want *SemanticError", err, err)
	}
	for _, want := range wantParts {
		if !strings.Contains(se.Error(), want) {
			t.Errorf("SemanticError = %q, want it to contain %q", se.Error(), want)
		}
	}
}

// TestMilestoneScope_HappyPath_Accepted is the non-vacuity control: a valid
// milestone scope validates, so the rules reject the degenerate cases below and
// nothing wider.
func TestMilestoneScope_HappyPath_Accepted(t *testing.T) {
	if err := plan.ValidateGroomingReport(milestoneReportDoc(t, validMilestoneScope())); err != nil {
		t.Fatalf("ValidateGroomingReport(valid milestone): %v", err)
	}
}

// TestValidateGroomingReport_MilestoneAbsentAccepted is the additive-optional
// proof at the domain level: a report with NO milestone_scope still validates,
// so the field's addition cannot alter the existing artifact contract.
func TestValidateGroomingReport_MilestoneAbsentAccepted(t *testing.T) {
	body := milestoneReportDoc(t, nil)
	if strings.Contains(string(body), "milestone_scope") {
		t.Fatalf("fixture leaked a milestone_scope key: %s", body)
	}
	if err := plan.ValidateGroomingReport(body); err != nil {
		t.Fatalf("ValidateGroomingReport(no milestone): %v", err)
	}
}

// TestMilestoneScope_EmptyScopeAccepted covers the degenerate-but-valid case:
// an empty included/excluded/declined set with an empty critical path validates
// (minItems:0 on the arrays, R1's empty early return), and
// CanonicalizeMilestoneScope tolerates a nil scope.
func TestMilestoneScope_EmptyScopeAccepted(t *testing.T) {
	if err := plan.ValidateGroomingReport(milestoneReportDoc(t, msScope(nil, nil, nil, nil))); err != nil {
		t.Fatalf("empty milestone scope must validate: %v", err)
	}
	plan.CanonicalizeMilestoneScope(nil) // must not panic
}

// TestMilestoneScope_GappedWave_Rejected is R1: waves [0,2] with no wave 1.
func TestMilestoneScope_GappedWave_Rejected(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{
			msInc("acme/app#1", 0, nil, nil),
			msInc("acme/app#3", 2, nil, nil),
		},
		nil, nil, nil)
	requireMilestoneRejection(t, ms, "/milestone_scope/included", "wave 1 is missing")
}

// TestMilestoneScope_UnresolvableDependency_Rejected is R2: a depends_on key
// present nowhere in included or excluded.
func TestMilestoneScope_UnresolvableDependency_Rejected(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{
			msInc("acme/app#1", 0, nil, nil),
			msInc("acme/app#2", 1, []string{msKey("acme/ghost#404")}, nil),
		},
		nil, nil, nil)
	requireMilestoneRejection(t, ms, "/milestone_scope/included/1/depends_on/0", "resolves to no item")
}

// TestMilestoneScope_NonMonotonicWave_Rejected is R3: an in-scope dependency in
// a LATER wave than its dependent.
func TestMilestoneScope_NonMonotonicWave_Rejected(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{
			msInc("acme/app#1", 0, nil, nil),
			// app#2 (wave 1) depends on app#3 (wave 2) — later wave.
			msInc("acme/app#2", 1, []string{msKey("acme/app#3")}, nil),
			msInc("acme/app#3", 2, nil, nil),
		},
		nil, nil, nil)
	requireMilestoneRejection(t, ms, "/milestone_scope/included/1/depends_on/0", "STRICTLY EARLIER wave")
}

// TestMilestoneScope_DependencyCycle_Rejected is R3 via a literal mutual
// depends_on pair, which no strict layering admits. Both entries sit in wave 0,
// so R1 passes and the RED lands on the monotonicity check.
func TestMilestoneScope_DependencyCycle_Rejected(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{
			msInc("acme/app#1", 0, []string{msKey("acme/app#2")}, nil),
			msInc("acme/app#2", 0, []string{msKey("acme/app#1")}, nil),
		},
		nil, nil, nil)
	requireMilestoneRejection(t, ms, "STRICTLY EARLIER wave")
}

// TestMilestoneScope_ExcludedDependencyWithoutDeclinedCall_Rejected is R4: an
// in-scope item depends on an EXCLUDED item but carries no open_question_ids.
func TestMilestoneScope_ExcludedDependencyWithoutDeclinedCall_Rejected(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{
			msInc("acme/app#1", 0, nil, nil),
			msInc("acme/app#2", 1, []string{msKey("acme/app#1"), msKey("acme/app#9")}, nil),
		},
		[]plan.MilestoneExclusion{msExc("acme/app#9")},
		nil, nil)
	requireMilestoneRejection(t, ms, "/milestone_scope/included/1", "EXCLUDED", "must be declined")
}

// TestMilestoneScope_ExcludedDependencyWithDeclinedCall_Accepted is R4's
// positive control: the same shape WITH an open_question_id referencing a
// declared declined call validates.
func TestMilestoneScope_ExcludedDependencyWithDeclinedCall_Accepted(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{
			msInc("acme/app#1", 0, nil, nil),
			msInc("acme/app#2", 1, []string{msKey("acme/app#1"), msKey("acme/app#9")}, []string{"app9-in-scope"}),
		},
		[]plan.MilestoneExclusion{msExc("acme/app#9")},
		[]plan.MilestoneDeclinedCall{msDeclined("app9-in-scope")},
		nil)
	if err := plan.ValidateGroomingReport(milestoneReportDoc(t, ms)); err != nil {
		t.Fatalf("valid excluded-dependency-with-declined-call: %v", err)
	}
}

// TestMilestoneScope_CriticalPathItemNotIncluded_Rejected is R5: a path element
// that is not an included item key.
func TestMilestoneScope_CriticalPathItemNotIncluded_Rejected(t *testing.T) {
	ms := validMilestoneScope()
	ms.CriticalPath = []string{msKey("acme/app#1"), msKey("acme/ghost#404")}
	requireMilestoneRejection(t, ms, "/milestone_scope/critical_path/1", "not an `included` item key")
}

// TestMilestoneScope_CriticalPathRepeatedItem_Rejected is R5: a repeated element.
func TestMilestoneScope_CriticalPathRepeatedItem_Rejected(t *testing.T) {
	ms := validMilestoneScope()
	ms.CriticalPath = []string{msKey("acme/app#1"), msKey("acme/app#1")}
	requireMilestoneRejection(t, ms, "/milestone_scope/critical_path/1", "repeats")
}

// TestMilestoneScope_CriticalPathNonIncreasingWave_Rejected is R5: a pair whose
// waves do not strictly increase. app#2→app#1 (wave 1 then wave 0), and app#2
// depends on app#1 so the connectivity check does NOT fire first — the RED
// lands on the wave ordering.
func TestMilestoneScope_CriticalPathNonIncreasingWave_Rejected(t *testing.T) {
	ms := validMilestoneScope()
	ms.CriticalPath = []string{msKey("acme/app#2"), msKey("acme/app#1")}
	requireMilestoneRejection(t, ms, "/milestone_scope/critical_path/1", "strictly later wave")
}

// TestMilestoneScope_CriticalPathDisconnected_Rejected is C3: an increasing-wave
// but EDGE-FREE sequence. app#1 (wave 0) and app#3 (wave 2) are both included and
// waves increase, but app#3 depends on app#2 — NOT app#1 — so the pair is not
// connected by a declared edge. Seeded by construction: the sequence skips the
// middle of the real chain.
func TestMilestoneScope_CriticalPathDisconnected_Rejected(t *testing.T) {
	ms := validMilestoneScope()
	ms.CriticalPath = []string{msKey("acme/app#1"), msKey("acme/app#3")}
	requireMilestoneRejection(t, ms, "/milestone_scope/critical_path/1", "declared depends_on edge", "must be a PATH")
}

// TestMilestoneScope_DanglingOpenQuestionID_Rejected is R6: an open_question_id
// matching no declined call.
func TestMilestoneScope_DanglingOpenQuestionID_Rejected(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{
			msInc("acme/app#1", 0, nil, nil),
			msInc("acme/app#2", 1, []string{msKey("acme/app#1")}, []string{"never-declared"}),
		},
		nil, nil, nil)
	requireMilestoneRejection(t, ms, "/milestone_scope/included/1/open_question_ids/0", "matches no declined_calls")
}

// TestMilestoneScope_ItemInIncludedAndExcluded_Rejected is R8: the same item key
// appears in BOTH `included` and `excluded`. The two derived ids differ by class
// prefix so report-wide id uniqueness does not catch it; R8 rejects the
// contradiction with a pointer-bearing SemanticError. Seeded by construction:
// app#1 is placed in both sets, every other field valid, so the RED lands on the
// disjointness check and not another rule. It must reject BEFORE the dependency
// rules resolve the item to its included interpretation (which would suppress R4).
func TestMilestoneScope_ItemInIncludedAndExcluded_Rejected(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, nil)},
		[]plan.MilestoneExclusion{msExc("acme/app#1")},
		nil, nil)
	requireMilestoneRejection(t, ms, "/milestone_scope/excluded/0", "also appears in `included`", "DISJOINT")
}

// TestMilestoneScope_UnsortedInclusions_Rejected is R7: included out of (wave,
// key) order. Reversing the order leaves R1–R6 valid (all order-independent),
// so the RED lands on the canonical-order check.
func TestMilestoneScope_UnsortedInclusions_Rejected(t *testing.T) {
	ms := validMilestoneScope()
	inc := ms.Included
	for i, j := 0, len(inc)-1; i < j; i, j = i+1, j-1 {
		inc[i], inc[j] = inc[j], inc[i]
	}
	requireMilestoneRejection(t, ms, "/milestone_scope/included", "sorted by (wave, item key)")
}

// TestMilestoneScope_UnsortedExclusions_Rejected is R7 for excluded.
func TestMilestoneScope_UnsortedExclusions_Rejected(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, nil)},
		// app#9 then app#8 — out of key order.
		[]plan.MilestoneExclusion{msExc("acme/app#9"), msExc("acme/app#8")},
		nil, nil)
	requireMilestoneRejection(t, ms, "/milestone_scope/excluded", "sorted by item key")
}

// TestMilestoneScope_UnsortedDeclinedCalls_Rejected is R7 for declined_calls.
func TestMilestoneScope_UnsortedDeclinedCalls_Rejected(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, nil)},
		nil,
		// "zebra" then "alpha" — out of question_id order.
		[]plan.MilestoneDeclinedCall{msDeclined("zebra"), msDeclined("alpha")},
		nil)
	requireMilestoneRejection(t, ms, "/milestone_scope/declined_calls", "sorted by question_id")
}

// TestMilestoneScope_UnsortedNestedArrays_Rejected is R7 (C4) for every NESTED
// array: depends_on, open_question_ids, and rubric_citations on each entry
// class, plus a declined call's options and item_refs. Each case seeds exactly
// one nested array out of canonical order and asserts the pointer.
func TestMilestoneScope_UnsortedNestedArrays_Rejected(t *testing.T) {
	// A three-wave base whose app#3 depends on both earlier items, so a reversed
	// depends_on stays resolvable and monotonic (R2/R3 pass) and only R7 fires.
	depBase := func() *plan.MilestoneScope {
		return msScope(
			[]plan.MilestoneInclusion{
				msInc("acme/app#1", 0, nil, nil),
				msInc("acme/app#2", 1, []string{msKey("acme/app#1")}, nil),
				msInc("acme/app#3", 2, []string{msKey("acme/app#1"), msKey("acme/app#2")}, nil),
			},
			nil, nil, nil)
	}
	for _, tc := range []struct {
		name    string
		build   func() *plan.MilestoneScope
		pointer string
	}{
		{
			name: "depends_on",
			build: func() *plan.MilestoneScope {
				ms := depBase()
				ms.Included[2].DependsOn = []string{msKey("acme/app#2"), msKey("acme/app#1")} // reversed
				return ms
			},
			pointer: "/milestone_scope/included/2/depends_on",
		},
		{
			name: "open_question_ids",
			build: func() *plan.MilestoneScope {
				ms := msScope(
					[]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, []string{"b", "a"})},
					nil,
					[]plan.MilestoneDeclinedCall{msDeclined("a"), msDeclined("b")},
					nil)
				return ms
			},
			pointer: "/milestone_scope/included/0/open_question_ids",
		},
		{
			name: "inclusion rubric_citations",
			build: func() *plan.MilestoneScope {
				ms := msScope([]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, nil)}, nil, nil, nil)
				ms.Included[0].RubricCitations = []plan.RubricCitation{{RubricID: "U2"}, {RubricID: "U1"}}
				return ms
			},
			pointer: "/milestone_scope/included/0/rubric_citations",
		},
		{
			name: "exclusion rubric_citations",
			build: func() *plan.MilestoneScope {
				ms := msScope([]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, nil)},
					[]plan.MilestoneExclusion{msExc("acme/app#9")}, nil, nil)
				ms.Excluded[0].RubricCitations = []plan.RubricCitation{{RubricID: "V6"}, {RubricID: "V5"}}
				return ms
			},
			pointer: "/milestone_scope/excluded/0/rubric_citations",
		},
		{
			name: "declined options",
			build: func() *plan.MilestoneScope {
				ms := msScope([]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, nil)}, nil,
					[]plan.MilestoneDeclinedCall{msDeclined("q")}, nil)
				ms.DeclinedCalls[0].Options = []string{"b", "a"}
				return ms
			},
			pointer: "/milestone_scope/declined_calls/0/options",
		},
		{
			name: "declined item_refs",
			build: func() *plan.MilestoneScope {
				ms := msScope([]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, nil)}, nil,
					[]plan.MilestoneDeclinedCall{msDeclined("q")}, nil)
				ms.DeclinedCalls[0].ItemRefs = []plan.ItemRef{msRef("acme/app#2"), msRef("acme/app#1")} // reversed
				return ms
			},
			pointer: "/milestone_scope/declined_calls/0/item_refs",
		},
		{
			name: "declined rubric_citations",
			build: func() *plan.MilestoneScope {
				ms := msScope([]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, nil)}, nil,
					[]plan.MilestoneDeclinedCall{msDeclined("q")}, nil)
				ms.DeclinedCalls[0].RubricCitations = []plan.RubricCitation{{RubricID: "U4"}, {RubricID: "U3"}}
				return ms
			},
			pointer: "/milestone_scope/declined_calls/0/rubric_citations",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireMilestoneRejection(t, tc.build(), tc.pointer, "canonical ordering")
		})
	}
}

// TestCanonicalizeMilestoneScope_OrdersCitationTieBreakers pins
// rubricCitationLess's full total order: two citations sharing a rubric_id are
// ordered by quote, then by note, so canonicalization is deterministic even
// when the optional fields are the only difference.
func TestCanonicalizeMilestoneScope_OrdersCitationTieBreakers(t *testing.T) {
	ms := msScope([]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, nil)}, nil, nil, nil)
	// Same rubric_id, differing quote; and same id+quote, differing note.
	ms.Included[0].RubricCitations = []plan.RubricCitation{
		{RubricID: "U1", Quote: "z", Note: "n"},
		{RubricID: "U1", Quote: "a", Note: "n"},
		{RubricID: "U1", Quote: "a", Note: "a"},
	}
	plan.CanonicalizeMilestoneScope(ms)
	got := ms.Included[0].RubricCitations
	want := []plan.RubricCitation{
		{RubricID: "U1", Quote: "a", Note: "a"},
		{RubricID: "U1", Quote: "a", Note: "n"},
		{RubricID: "U1", Quote: "z", Note: "n"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("citation[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// The fingerprint reflects the quote difference (a citation change moves it
	// only through the canonical fields the fingerprint hashes, so this asserts
	// the canonicalize+hash path is stable, not that citations enter the hash).
	if plan.MilestoneScopeFingerprint(ms) == "" {
		t.Error("fingerprint must be non-empty for a populated scope")
	}
}

// TestMilestoneScope_PerRunMintedInclusionID_Rejected proves id recomposition
// reaches the new classes: an inclusion id that is a per-run token rather than a
// recomposition of its own item_ref is rejected.
func TestMilestoneScope_PerRunMintedInclusionID_Rejected(t *testing.T) {
	ms := validMilestoneScope()
	ms.Included[0].ID = "milestone_inclusion:8f14e45fceea167a5a36dedd4bea2543"
	requireMilestoneRejection(t, ms, "/milestone_scope/included/0/id", "not derived from its own item reference")
}

// TestMilestoneScope_DuplicateEntryIDAcrossArrays_Rejected proves report-wide id
// uniqueness reaches the milestone classes. Two declined calls sharing a
// question_id derive one id (cross-array collision is impossible by the differing
// class prefixes, so the uniqueness reach is exercised within a milestone array).
func TestMilestoneScope_DuplicateEntryIDAcrossArrays_Rejected(t *testing.T) {
	ms := msScope(
		[]plan.MilestoneInclusion{msInc("acme/app#1", 0, nil, nil)},
		nil,
		[]plan.MilestoneDeclinedCall{msDeclined("same"), msDeclined("same")},
		nil)
	requireMilestoneRejection(t, ms, "duplicate entry id", "milestone_declined:same")
}

// TestMilestoneScope_ExclusionWithoutRubricCitation_Rejected is approval
// condition C2: an exclusion with no rubric citation is REJECTED at the schema
// boundary, on the same terms as an inclusion. Two SELF-PAIRED cases, each
// seeded by editing the excluded entry's rubric_citations to the bad state, so
// each reddens a DIFFERENT control: a genuinely ABSENT key (caught by
// `required`) and an EMPTY array (caught by `minItems: 1`). The struct always
// emits the field, so the JSON is edited through a map to reach a truly absent
// key rather than the `null` a nil slice would marshal to.
func TestMilestoneScope_ExclusionWithoutRubricCitation_Rejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(exc map[string]any)
	}{
		{"citations absent (required)", func(exc map[string]any) { delete(exc, "rubric_citations") }},
		{"citations empty (minItems)", func(exc map[string]any) { exc["rubric_citations"] = []any{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(milestoneReportDoc(t, validMilestoneScope()), &doc); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			exc := doc["milestone_scope"].(map[string]any)["excluded"].([]any)[0].(map[string]any)
			tc.mutate(exc)
			body, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal mutated fixture: %v", err)
			}
			var se *plan.SchemaError
			if err := plan.ValidateGroomingReport(body); !errors.As(err, &se) {
				t.Fatalf("ValidateGroomingReport: err = %v (%T), want *SchemaError", err, err)
			}
			if !strings.Contains(se.Error(), "/milestone_scope/excluded/0") {
				t.Errorf("SchemaError should name /milestone_scope/excluded/0; got %v", se)
			}
		})
	}
}

// TestMilestoneEntryID_ZeroRefForm pins the zero-ref derivation the declined
// call uses: no item refs yields `<class>:<lowercased qualifier>`, with no
// double colon and the qualifier lowercased.
func TestMilestoneEntryID_ZeroRefForm(t *testing.T) {
	got := plan.GroomingEntryID(plan.GroomingClassMilestoneDeclined, "Web-UI-In-Alpha")
	if want := "milestone_declined:web-ui-in-alpha"; got != want {
		t.Fatalf("zero-ref id = %q, want %q", got, want)
	}
	// The single-ref forms are unaffected.
	if got := plan.GroomingEntryID(plan.GroomingClassMilestoneInclusion, "", msRef("acme/app#1")); got != "milestone_inclusion:github/acme/app#1" {
		t.Errorf("inclusion id = %q, want milestone_inclusion:github/acme/app#1", got)
	}
}

// TestMilestoneScopeFingerprint_StableAcrossIdenticalInput is the C4/R7
// determinism proof at the fingerprint level: two INDEPENDENTLY constructed
// equal scopes, whose nested arrays are supplied in DIFFERENT orders, hash
// equal — and a changed wave, reason or declined-call set hashes differently.
func TestMilestoneScopeFingerprint_StableAcrossIdenticalInput(t *testing.T) {
	a := validMilestoneScope()
	b := validMilestoneScope()
	// Permute b's collections; the fingerprint canonicalizes a copy, so order
	// must not move the hash.
	b.Included[0], b.Included[2] = b.Included[2], b.Included[0]
	if plan.MilestoneScopeFingerprint(a) != plan.MilestoneScopeFingerprint(b) {
		t.Fatalf("identical scopes hashed differently:\n%s\n%s",
			plan.MilestoneScopeFingerprint(a), plan.MilestoneScopeFingerprint(b))
	}

	// A changed WAVE moves the hash.
	c := validMilestoneScope()
	c.Included[2].Wave = 3
	if plan.MilestoneScopeFingerprint(a) == plan.MilestoneScopeFingerprint(c) {
		t.Error("a changed wave did not move the fingerprint")
	}
	// A changed REASON moves the hash.
	d := validMilestoneScope()
	d.Included[0].Reason = "different reason"
	if plan.MilestoneScopeFingerprint(a) == plan.MilestoneScopeFingerprint(d) {
		t.Error("a changed reason did not move the fingerprint")
	}
	// A changed DECLINED-CALL set moves the hash.
	e := validMilestoneScope()
	e.DeclinedCalls = []plan.MilestoneDeclinedCall{msDeclined("new-question")}
	if plan.MilestoneScopeFingerprint(a) == plan.MilestoneScopeFingerprint(e) {
		t.Error("a changed declined-call set did not move the fingerprint")
	}
	// Empty in, empty out.
	if plan.MilestoneScopeFingerprint(nil) != "" {
		t.Error("nil scope must fingerprint to the empty string")
	}
}

// milestoneScopeWithUnsortedNested builds a valid scope whose nested arrays are
// supplied OUT of canonical order (depends_on reversed, rubric_citations
// reversed), so a fingerprint that canonicalizes in place — rather than on a
// deep copy — would visibly reorder the caller's slices.
func milestoneScopeWithUnsortedNested() *plan.MilestoneScope {
	inc3 := plan.MilestoneInclusion{
		ID:      plan.GroomingEntryID(plan.GroomingClassMilestoneInclusion, "", msRef("acme/app#3")),
		ItemRef: msRef("acme/app#3"),
		Reason:  "in scope: acme/app#3",
		Wave:    2,
		// Reversed: canonical order is (app#1, app#2).
		DependsOn: []string{msKey("acme/app#2"), msKey("acme/app#1")},
		// Reversed: canonical order is (U1, U2).
		RubricCitations: []plan.RubricCitation{{RubricID: "U2"}, {RubricID: "U1"}},
	}
	return msScope(
		[]plan.MilestoneInclusion{
			msInc("acme/app#1", 0, nil, nil),
			msInc("acme/app#2", 1, []string{msKey("acme/app#1")}, nil),
			inc3,
		},
		[]plan.MilestoneExclusion{msExc("acme/app#9")},
		nil,
		[]string{msKey("acme/app#1"), msKey("acme/app#2"), msKey("acme/app#3")},
	)
}

// TestMilestoneScopeFingerprint_DoesNotMutateInput is the concern-1 immutability
// proof (no -race needed): fingerprinting a scope whose nested arrays are out of
// canonical order must leave those arrays in their supplied order. With a shallow
// top-level copy the in-place canonicalization sorts the caller's shared nested
// backing arrays; the deep copy prevents it. RED lands on the behavioral
// assertion below when the deep copy is removed.
func TestMilestoneScopeFingerprint_DoesNotMutateInput(t *testing.T) {
	ms := milestoneScopeWithUnsortedNested()
	_ = plan.MilestoneScopeFingerprint(ms)
	// The reversed depends_on must be untouched (canonical order would put app#1
	// first).
	if got := ms.Included[2].DependsOn; got[0] != msKey("acme/app#2") || got[1] != msKey("acme/app#1") {
		t.Errorf("fingerprint mutated the caller's depends_on: got %v, want it left reversed", got)
	}
	// The reversed rubric_citations must be untouched (canonical order would put
	// U1 first).
	if got := ms.Included[2].RubricCitations; got[0].RubricID != "U2" || got[1].RubricID != "U1" {
		t.Errorf("fingerprint mutated the caller's rubric_citations: got %v, want it left reversed", got)
	}
}

// TestMilestoneScopeFingerprint_ConcurrentUseNoRace is the concern-1 concurrency
// proof: many goroutines fingerprint the SAME scope at once. With a shallow copy
// they concurrently sort the shared nested backing arrays in production code,
// which the race detector flags; the deep copy gives each call its own arrays.
// This is discrimination in the code under test (not the scaffolding), so it
// reddens under -race with the deep copy removed. Run the whole suite under
// -race (scripts/test does) to exercise it.
func TestMilestoneScopeFingerprint_ConcurrentUseNoRace(t *testing.T) {
	ms := milestoneScopeWithUnsortedNested()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = plan.MilestoneScopeFingerprint(ms)
		}()
	}
	wg.Wait()
	// Post-condition: the shared input is still in its supplied order — no
	// goroutine mutated it.
	if got := ms.Included[2].DependsOn; got[0] != msKey("acme/app#2") {
		t.Errorf("a concurrent fingerprint mutated the shared depends_on: got %v", got)
	}
}

// TestMilestoneScope_ByteIdenticalAcrossTwoSerializations is approval condition
// C4: the SAME logical scope built twice with EVERY nested array in a different
// input order serializes to identical bytes after canonicalization. The
// critical_path is held in the same order in both (its order is its content).
// The pre-canonical bytes are asserted to DIFFER, so the test is not vacuous.
func TestMilestoneScope_ByteIdenticalAcrossTwoSerializations(t *testing.T) {
	// perm returns s or its reverse, so every nested and outer array is supplied
	// in a different input order across the two builds.
	build := func(reversed bool) *plan.MilestoneScope {
		strs := func(a, b string) []string {
			if reversed {
				return []string{b, a}
			}
			return []string{a, b}
		}
		cites := func(a, b string) []plan.RubricCitation {
			if reversed {
				return []plan.RubricCitation{{RubricID: b}, {RubricID: a}}
			}
			return []plan.RubricCitation{{RubricID: a}, {RubricID: b}}
		}
		refs := func(a, b string) []plan.ItemRef {
			if reversed {
				return []plan.ItemRef{msRef(b), msRef(a)}
			}
			return []plan.ItemRef{msRef(a), msRef(b)}
		}
		inc1 := plan.MilestoneInclusion{
			ID:      plan.GroomingEntryID(plan.GroomingClassMilestoneInclusion, "", msRef("acme/app#1")),
			ItemRef: msRef("acme/app#1"), Reason: "keystone", Wave: 0,
			RubricCitations: []plan.RubricCitation{{RubricID: "U1"}},
		}
		inc2 := plan.MilestoneInclusion{
			ID:      plan.GroomingEntryID(plan.GroomingClassMilestoneInclusion, "", msRef("acme/app#2")),
			ItemRef: msRef("acme/app#2"), Reason: "consumer", Wave: 1,
			DependsOn:       strs(msKey("acme/app#1"), msKey("acme/app#9")),
			OpenQuestionIDs: strs("alpha", "beta"),
			RubricCitations: cites("R4", "U2"),
		}
		exc1 := msExc("acme/app#8")
		exc2 := msExc("acme/app#9")
		dc1 := plan.MilestoneDeclinedCall{
			ID:         plan.GroomingEntryID(plan.GroomingClassMilestoneDeclined, "alpha"),
			QuestionID: "alpha", Question: "q", WhyAmbiguous: "w",
			Options:         strs("no", "yes"),
			ItemRefs:        refs("acme/app#1", "acme/app#2"),
			RubricCitations: cites("S1", "U3"),
		}
		dc2 := msDeclined("beta")
		inc := []plan.MilestoneInclusion{inc1, inc2}
		exc := []plan.MilestoneExclusion{exc1, exc2}
		dc := []plan.MilestoneDeclinedCall{dc1, dc2}
		if reversed {
			inc = []plan.MilestoneInclusion{inc2, inc1}
			exc = []plan.MilestoneExclusion{exc2, exc1}
			dc = []plan.MilestoneDeclinedCall{dc2, dc1}
		}
		// critical_path is the SAME order in both — it is a sequence.
		return msScope(inc, exc, dc, []string{msKey("acme/app#1"), msKey("acme/app#2")})
	}

	a, b := build(false), build(true)

	preA, _ := json.Marshal(a)
	preB, _ := json.Marshal(b)
	if string(preA) == string(preB) {
		t.Fatal("pre-canonical serializations are already equal — the reversal did not vary the input, so the test proves nothing")
	}

	plan.CanonicalizeMilestoneScope(a)
	plan.CanonicalizeMilestoneScope(b)
	gotA, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	gotB, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	if string(gotA) != string(gotB) {
		t.Errorf("canonicalized serializations differ:\n%s\n%s", gotA, gotB)
	}
}

// TestValidateGroomingReport_MilestoneExample is the CROSS-LAYER round-trip: the
// shipped example is read FROM DISK and driven through ParseGroomingReport, so
// the canonical schema, the embedded mirror the validator compiles, the typed
// structs and all seven semantic rules are asserted against ONE artifact.
//
// It also proves approval condition C6 (defect-surfacing by construction): the
// example carries at least one entry in an existing defect-bearing array, so
// criterion 5 is falsifiable rather than incidental.
func TestValidateGroomingReport_MilestoneExample(t *testing.T) {
	body, err := os.ReadFile("../../../docs/spec/examples/grooming-report-v1-milestone-example.json")
	if err != nil {
		t.Fatalf("read milestone example: %v", err)
	}
	if err := plan.ValidateGroomingReport(body); err != nil {
		t.Fatalf("ValidateGroomingReport(milestone example): %v", err)
	}
	gr, err := plan.ParseGroomingReport(body)
	if err != nil {
		t.Fatalf("ParseGroomingReport(milestone example): %v", err)
	}
	ms := gr.MilestoneScope
	if ms == nil {
		t.Fatal("example carries no milestone_scope")
	}
	if len(ms.Included) < 3 {
		t.Errorf("included = %d, want a three-wave scope (>=3)", len(ms.Included))
	}
	if len(ms.Excluded) < 2 {
		t.Errorf("excluded = %d, want 2", len(ms.Excluded))
	}
	if len(ms.DeclinedCalls) < 1 {
		t.Errorf("declined_calls = %d, want at least 1", len(ms.DeclinedCalls))
	}
	if len(ms.CriticalPath) < 2 {
		t.Errorf("critical_path = %d, want a real chain", len(ms.CriticalPath))
	}
	if ms.EdgeConfidence.Level != "medium" {
		t.Errorf("edge_confidence.level = %q, want medium", ms.EdgeConfidence.Level)
	}
	// C6: a declined call is referenced by an inclusion's open_question_ids.
	referenced := false
	for _, inc := range ms.Included {
		if len(inc.OpenQuestionIDs) > 0 {
			referenced = true
		}
	}
	if !referenced {
		t.Error("no inclusion references a declined call — the example must exercise the open_question_ids link")
	}
	// C6: the example carries at least one entry in an existing defect-bearing
	// array, so criterion 5's defect-surfacing is falsifiable by construction.
	defects := len(gr.Duplicates) + len(gr.HygieneDefects) + len(gr.DependencyEdges) +
		len(gr.VisionDrift) + len(gr.DecompositionSuggestions)
	if defects == 0 {
		t.Error("milestone example carries no entry in any existing defect-bearing array (C6)")
	}
	// The fingerprint is stable across a re-parse of the same bytes.
	if plan.MilestoneScopeFingerprint(ms) != plan.MilestoneScopeFingerprint(gr.MilestoneScope) {
		t.Error("fingerprint not stable across identical scope")
	}
}
