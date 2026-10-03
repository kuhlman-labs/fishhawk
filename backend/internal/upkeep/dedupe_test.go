package upkeep_test

import (
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

// TestFindingMarker_Format pins the exact marker bytes. #3924 embeds this
// string in every upkeep filing and MarkDuplicates matches it byte-exact, so a
// change here orphans every issue already filed under the old shape.
func TestFindingMarker_Format(t *testing.T) {
	got := upkeep.FindingMarker("flake:TestWidgetSync")
	want := "<!-- fishhawk-upkeep:v1 finding_id=flake:TestWidgetSync -->"
	if got != want {
		t.Errorf("FindingMarker = %q, want %q", got, want)
	}
}

// TestMarkDuplicates_ExactMarkerOnOpenIssue: an OPEN issue whose body carries
// the finding's marker marks it with basis marker.
//
// Counterfactual: the candidate's title shares NO token with the proposal, so
// the similarity half cannot mark it — making the marker branch never match
// leaves the finding unmarked and this goes RED.
func TestMarkDuplicates_ExactMarkerOnOpenIssue(t *testing.T) {
	proposals := []upkeep.Proposal{{FindingID: "flake:TestA", Title: "Quarantine flaky TestA"}}
	candidates := []intakegroom.Candidate{{
		Number: 40,
		Title:  "Unrelated invoices rollup",
		Body:   "Filed by the upkeep scan.\n\n" + upkeep.FindingMarker("flake:TestA") + "\n",
		URL:    "https://example.test/40",
	}}

	got := upkeep.MarkDuplicates(proposals, candidates)
	want := []upkeep.Duplicate{{
		FindingID:   "flake:TestA",
		IssueNumber: 40,
		IssueURL:    "https://example.test/40",
		Basis:       upkeep.BasisMarker,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MarkDuplicates = %+v, want %+v", got, want)
	}
}

// TestMarkDuplicates_ClosedIssueNeverMarks: closed issues mark nothing, by
// either basis.
//
// Counterfactual: the window holds ONLY closed candidates — one carrying the
// exact marker and one with an identical title — so deleting the Closed
// filter produces a marker mark and this goes RED.
func TestMarkDuplicates_ClosedIssueNeverMarks(t *testing.T) {
	proposals := []upkeep.Proposal{{FindingID: "flake:TestA", Title: "Quarantine flaky TestA"}}
	candidates := []intakegroom.Candidate{
		{Number: 7, Title: "Unrelated invoices rollup", Body: upkeep.FindingMarker("flake:TestA"), Closed: true},
		{Number: 8, Title: "Quarantine flaky TestA", Closed: true},
	}

	got := upkeep.MarkDuplicates(proposals, candidates)
	if got == nil || len(got) != 0 {
		t.Errorf("MarkDuplicates = %#v, want a non-nil empty slice (closed issues never mark)", got)
	}
}

// TestMarkDuplicates_MarkerIsExactNotPrefix: the marker of flake:TestAB does
// not mark flake:TestA.
//
// Counterfactual: the candidate carries the marker for flake:TestAB under a
// title sharing no token with the proposal, so matching the marker WITHOUT its
// closing ` -->` terminator is a prefix match that marks it — RED.
func TestMarkDuplicates_MarkerIsExactNotPrefix(t *testing.T) {
	proposals := []upkeep.Proposal{{FindingID: "flake:TestA", Title: "Quarantine flaky TestA"}}
	candidates := []intakegroom.Candidate{{
		Number: 41,
		Title:  "Unrelated invoices rollup",
		Body:   upkeep.FindingMarker("flake:TestAB"),
	}}

	got := upkeep.MarkDuplicates(proposals, candidates)
	if len(got) != 0 {
		t.Errorf("MarkDuplicates = %+v, want no mark (flake:TestAB's marker is not flake:TestA's)", got)
	}
}

// TestMarkDuplicates_SimilarityMediumOrHigherMarks: an open issue whose title
// restates the proposal marks it with basis similarity at medium or higher.
func TestMarkDuplicates_SimilarityMediumOrHigherMarks(t *testing.T) {
	proposals := []upkeep.Proposal{{
		FindingID: "flake:TestWidgetSync",
		Title:     "Quarantine flaky test TestWidgetSync",
		Labels:    []string{"type:bug"},
		Type:      "bug",
	}}
	candidates := []intakegroom.Candidate{{
		Number: 50,
		Title:  "Flaky test TestWidgetSync: quarantine",
		URL:    "https://example.test/50",
	}}

	got := upkeep.MarkDuplicates(proposals, candidates)
	if len(got) != 1 {
		t.Fatalf("MarkDuplicates = %+v, want exactly one mark", got)
	}
	d := got[0]
	if d.FindingID != "flake:TestWidgetSync" || d.IssueNumber != 50 || d.IssueURL != "https://example.test/50" {
		t.Errorf("mark = %+v, want finding flake:TestWidgetSync on #50", d)
	}
	if d.Basis != upkeep.BasisSimilarity {
		t.Errorf("basis = %q, want %q", d.Basis, upkeep.BasisSimilarity)
	}
	if !d.Confidence.AtLeastMedium() || d.Score < intakegroom.ThresholdMedium {
		t.Errorf("confidence = %q score = %v, want a medium-or-higher band", d.Confidence, d.Score)
	}
}

// TestMarkDuplicates_BestSimilarityWins: when several OPEN candidates clear
// the medium band, the HIGHEST-scoring one marks the proposal.
//
// The fixture proves its own bands (both candidates at or above medium, with
// different scores) and lists the WEAKER candidate first and at the lower
// number, so neither input order nor a lowest-number rule can produce the
// right answer by accident. Counterfactual: making similarityDuplicate keep
// the LAST qualifying result instead of the first marks the weaker #10 — RED.
func TestMarkDuplicates_BestSimilarityWins(t *testing.T) {
	p := upkeep.Proposal{FindingID: "deprecation:ioutil", Title: "Replace deprecated ioutil calls"}
	weaker := intakegroom.Candidate{Number: 10, Title: "Replace deprecated ioutil helpers now", URL: "https://example.test/10"}
	stronger := intakegroom.Candidate{Number: 20, Title: "Replace deprecated ioutil calls", URL: "https://example.test/20"}

	filing := intakegroom.Filing{Title: p.Title}
	ws := intakegroom.Duplicates(filing, []intakegroom.Candidate{weaker})
	ss := intakegroom.Duplicates(filing, []intakegroom.Candidate{stronger})
	if len(ws) != 1 || len(ss) != 1 || !ws[0].Confidence.AtLeastMedium() || !ss[0].Confidence.AtLeastMedium() || ws[0].Score >= ss[0].Score {
		t.Fatalf("fixture is not two medium-or-higher pairs with #20 scoring above #10: weaker = %+v, stronger = %+v", ws, ss)
	}

	got := upkeep.MarkDuplicates([]upkeep.Proposal{p}, []intakegroom.Candidate{weaker, stronger})
	if len(got) != 1 {
		t.Fatalf("MarkDuplicates = %+v, want exactly one mark", got)
	}
	if got[0].IssueNumber != 20 || got[0].Basis != upkeep.BasisSimilarity || got[0].Score != ss[0].Score {
		t.Errorf("mark = %+v, want #20 (score %v) by similarity, not the weaker #10 (score %v)", got[0], ss[0].Score, ws[0].Score)
	}
}

// TestMarkDuplicates_LowSimilarityDoesNotMark: a LOW-band similarity is not a
// duplicate.
//
// The fixture proves its own band: it calls intakegroom.Duplicates on the
// same pair and requires exactly one LOW result, so the test cannot pass on a
// pair that simply never scored. Counterfactual: accepting any non-empty band
// in the similarity half marks this pair — RED.
func TestMarkDuplicates_LowSimilarityDoesNotMark(t *testing.T) {
	p := upkeep.Proposal{FindingID: "flake:TestWidgetSync", Title: "Flaky test TestWidgetSync quarantine"}
	c := intakegroom.Candidate{Number: 60, Title: "Flaky test TestOtherThing"}

	band := intakegroom.Duplicates(intakegroom.Filing{Title: p.Title}, []intakegroom.Candidate{c})
	if len(band) != 1 || band[0].Confidence != intakegroom.ConfidenceLow {
		t.Fatalf("fixture is not a LOW-band pair: intakegroom.Duplicates = %+v", band)
	}

	got := upkeep.MarkDuplicates([]upkeep.Proposal{p}, []intakegroom.Candidate{c})
	if len(got) != 0 {
		t.Errorf("MarkDuplicates = %+v, want no mark for a low-band pair", got)
	}
}

// TestMarkDuplicates_IdenticalOpenIssueStillMarks: an open issue IDENTICAL to
// the proposal is the strongest duplicate there is and must mark.
//
// Proposal carries no body, so the self-match failure is expressed through the
// fields it DOES carry. intakegroom skips a candidate whose title AND body
// both equal the filing's, provided the filing body is non-empty. The fixture
// seeds an open candidate whose title equals the proposal title and whose
// body equals that same title — so any change that derives a non-empty filing
// body from the proposal (the counterfactual: `Body: p.Title` in the
// intakegroom.Filing that similarityDuplicate builds) re-arms the guard, the
// identical issue is skipped, no mark is produced and this goes RED. The body
// carries no marker, so the marker half cannot mask the similarity half.
func TestMarkDuplicates_IdenticalOpenIssueStillMarks(t *testing.T) {
	const title = "Quarantine flaky test TestWidgetSync"
	proposals := []upkeep.Proposal{{FindingID: "flake:TestWidgetSync", Title: title}}
	candidates := []intakegroom.Candidate{{Number: 70, Title: title, Body: title}}

	got := upkeep.MarkDuplicates(proposals, candidates)
	if len(got) != 1 || got[0].IssueNumber != 70 || got[0].Basis != upkeep.BasisSimilarity {
		t.Fatalf("MarkDuplicates = %+v, want #70 marked by similarity (an identical open issue is a duplicate)", got)
	}
	if got[0].Confidence != intakegroom.ConfidenceHigh {
		t.Errorf("confidence = %q, want high for an identical title", got[0].Confidence)
	}
}

// TestMarkDuplicates_MarkerWinsAndOrderIsDeterministic pins the output shape:
// proposal order, one record per marked finding, marker preferred over a
// lower-numbered similarity match, the lowest issue number among several
// marker carriers, and a non-nil empty slice when nothing marks.
func TestMarkDuplicates_MarkerWinsAndOrderIsDeterministic(t *testing.T) {
	proposals := []upkeep.Proposal{
		{FindingID: "deprecation:ioutil", Title: "Replace deprecated ioutil calls"},
		{FindingID: "flake:TestA", Title: "Quarantine flaky TestA"},
		{FindingID: "toolchain_drift:go", Title: "Align the drifting Go toolchain pins"},
	}
	candidates := []intakegroom.Candidate{
		// Newest-first, as the reader returns it.
		{Number: 90, Title: "Unrelated invoices rollup", Body: upkeep.FindingMarker("flake:TestA")},
		{Number: 80, Title: "Unrelated widget sweep", Body: upkeep.FindingMarker("flake:TestA")},
		// A lower-numbered, identical-title candidate for flake:TestA: the
		// marker still wins.
		{Number: 5, Title: "Quarantine flaky TestA"},
		{Number: 12, Title: "Replace deprecated ioutil calls"},
	}

	got := upkeep.MarkDuplicates(proposals, candidates)
	if len(got) != 2 {
		t.Fatalf("MarkDuplicates = %+v, want two marks", got)
	}
	if got[0].FindingID != "deprecation:ioutil" || got[0].IssueNumber != 12 || got[0].Basis != upkeep.BasisSimilarity {
		t.Errorf("first mark = %+v, want deprecation:ioutil on #12 by similarity", got[0])
	}
	if got[1].FindingID != "flake:TestA" || got[1].IssueNumber != 80 || got[1].Basis != upkeep.BasisMarker {
		t.Errorf("second mark = %+v, want flake:TestA on #80 (lowest marker carrier) by marker", got[1])
	}

	for _, tc := range []struct {
		name       string
		proposals  []upkeep.Proposal
		candidates []intakegroom.Candidate
	}{
		{name: "no proposals", candidates: candidates},
		{name: "no candidates", proposals: proposals},
		{name: "nothing matches", proposals: proposals[2:], candidates: candidates},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := upkeep.MarkDuplicates(tc.proposals, tc.candidates)
			if got == nil || len(got) != 0 {
				t.Errorf("MarkDuplicates = %#v, want a non-nil empty slice", got)
			}
		})
	}
}
