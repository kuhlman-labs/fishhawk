// Package upkeep holds the pure, forge-free primitives of the upkeep scan
// (E79 / #3726): the hidden finding marker an upkeep filing embeds, and the
// open-issue dedupe that decides which proposed findings already have an
// issue.
//
// The package is PURE on purpose, in the same posture as intakegroom: it
// imports no forge client, no workmgmt, no server package and — deliberately —
// not backend/internal/plan's report types. It declares its own neutral input
// (Proposal) and the caller adapts a validated upkeep report into it, so the
// dedupe rules are unit-testable against literals and this package stands
// alone ahead of the report contract that feeds it.
//
// Nothing here closes, comments on, relabels or files anything. A Duplicate is
// a MARK the apply step (#3924) reads to SKIP a filing; the long-form contract,
// including the residuals, is in README.md beside this file.
package upkeep

import (
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
)

// markerPrefix and markerSuffix bracket the finding id in the hidden marker.
// The suffix is part of the match: a finding id carries no whitespace and no
// angle bracket (the upkeep_report_v1 id pattern), so requiring the closing
// ` -->` makes a marker match EXACT — flake:TestA can never match the marker
// of flake:TestAB.
const (
	markerPrefix = "<!-- fishhawk-upkeep:v1 finding_id="
	markerSuffix = " -->"
)

// Basis names why a proposal was marked a duplicate.
type Basis string

// The two dedupe bases.
const (
	// BasisMarker: an open issue's body carries this finding's exact hidden
	// marker — the issue an earlier upkeep apply filed for the same finding.
	BasisMarker Basis = "marker"
	// BasisSimilarity: an open issue's title is lexically similar to the
	// proposed title at intakegroom's medium band or higher.
	BasisSimilarity Basis = "similarity"
)

// Proposal is one finding's proposed issue, reduced to what dedupe reads. It
// deliberately carries NO body: the similarity half hands intakegroom a filing
// with an empty body, which keeps intakegroom's same-title-and-body self-match
// guard from hiding an identical open issue (see MarkDuplicates).
type Proposal struct {
	// FindingID is the report's stable `<source>:<subject>` finding id.
	FindingID string
	// Title is the proposed issue title.
	Title string
	// Labels are the proposed label names.
	Labels []string
	// Type is the proposed work-item type (bug, chore, ...).
	Type string
}

// Duplicate marks one proposal as already covered by an open issue.
type Duplicate struct {
	// FindingID is the marked proposal's finding id.
	FindingID string `json:"finding_id"`
	// IssueNumber is the open issue the proposal duplicates.
	IssueNumber int `json:"issue_number"`
	// IssueURL is that issue's URL, when the reader supplied one.
	IssueURL string `json:"issue_url,omitempty"`
	// Basis is why the proposal was marked.
	Basis Basis `json:"basis"`
	// Score is intakegroom's similarity score. Present only for
	// BasisSimilarity; a marker match is exact, not scored. A similarity mark
	// is always at or above intakegroom.ThresholdMedium, so omitempty never
	// hides a real score.
	Score float64 `json:"score,omitempty"`
	// Confidence is the band Score falls in. Present only for BasisSimilarity.
	Confidence intakegroom.Confidence `json:"confidence,omitempty"`
}

// FindingMarker returns the exact hidden marker an upkeep filing for findingID
// embeds in its issue body (#3924 writes it; MarkDuplicates reads it).
func FindingMarker(findingID string) string {
	return markerPrefix + findingID + markerSuffix
}

// MarkDuplicates returns, in proposal order, one Duplicate for each proposal an
// OPEN candidate already covers, and nothing for the rest. It never returns
// nil: an empty result is a non-nil empty slice, so a caller serializing it
// emits [] rather than null.
//
// Rules, in order:
//
//  1. CLOSED CANDIDATES NEVER MARK, by either basis. They are dropped before
//     anything else. A closed issue for a finding the scan still sees means
//     the fix did not hold — that is a reason to file again, not to skip.
//  2. MARKER: the lowest-numbered open candidate whose body contains
//     FindingMarker(id) marks the proposal with BasisMarker.
//  3. SIMILARITY: otherwise, intakegroom.Duplicates scores the proposal's
//     title against the open set and the best result at medium confidence or
//     higher marks it with BasisSimilarity. The filing handed to intakegroom
//     carries an EMPTY body on purpose: intakegroom skips a candidate whose
//     title and body both equal the filing's (its reader-echo guard), and an
//     identical open issue is exactly the duplicate this must catch.
//
// At most one Duplicate is produced per proposal.
func MarkDuplicates(proposals []Proposal, candidates []intakegroom.Candidate) []Duplicate {
	open := make([]intakegroom.Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Closed {
			continue
		}
		open = append(open, c)
	}
	// The marker scan takes the lowest issue number, independent of the
	// reader's (newest-first) order.
	byNumber := make([]intakegroom.Candidate, len(open))
	copy(byNumber, open)
	sort.SliceStable(byNumber, func(i, j int) bool { return byNumber[i].Number < byNumber[j].Number })

	out := make([]Duplicate, 0, len(proposals))
	for _, p := range proposals {
		if d, ok := markerDuplicate(p, byNumber); ok {
			out = append(out, d)
			continue
		}
		if d, ok := similarityDuplicate(p, open); ok {
			out = append(out, d)
		}
	}
	return out
}

// markerDuplicate returns the lowest-numbered candidate carrying p's exact
// marker. candidates must already be open-only and sorted by number.
func markerDuplicate(p Proposal, candidates []intakegroom.Candidate) (Duplicate, bool) {
	marker := FindingMarker(p.FindingID)
	for _, c := range candidates {
		if strings.Contains(c.Body, marker) {
			return Duplicate{
				FindingID:   p.FindingID,
				IssueNumber: c.Number,
				IssueURL:    c.URL,
				Basis:       BasisMarker,
			}, true
		}
	}
	return Duplicate{}, false
}

// similarityDuplicate returns the best medium-or-higher similarity match for
// p among the open candidates.
//
// "Best" rides on intakegroom.Duplicates' ORDERING CONTRACT: it returns its
// results sorted by score descending, then issue number ascending, so the
// first medium-or-higher result is the highest-scoring one (ties to the
// lowest number). TestMarkDuplicates_BestSimilarityWins pins that dependency
// and reddens if the ordering ever changes.
func similarityDuplicate(p Proposal, candidates []intakegroom.Candidate) (Duplicate, bool) {
	// Body stays empty: see MarkDuplicates rule 3.
	filing := intakegroom.Filing{Title: p.Title, Labels: p.Labels, Type: p.Type}
	for _, dc := range intakegroom.Duplicates(filing, candidates) {
		if !dc.Confidence.AtLeastMedium() {
			continue
		}
		return Duplicate{
			FindingID:   p.FindingID,
			IssueNumber: dc.Number,
			IssueURL:    dc.URL,
			Basis:       BasisSimilarity,
			Score:       dc.Score,
			Confidence:  dc.Confidence,
		}, true
	}
	return Duplicate{}, false
}
