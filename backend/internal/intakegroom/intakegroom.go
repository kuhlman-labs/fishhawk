// Package intakegroom derives the advisory intake signals an event-driven
// micro-groom attaches to a freshly filed work item (#2239, E54.7): duplicate
// candidates, a parent-epic suggestion, and a provisional charter-anchored
// score.
//
// The package is PURE and dependency-light on purpose. It imports no forge
// client, no workmgmt, and no server package — it declares its own input
// vocabulary (Filing, Candidate, Charter) and the caller adapts provider
// records into it. That keeps every derivation trivially unit-testable
// against literals and keeps the signal logic out of the transport layers
// that can only be exercised through fakes.
//
// Three things this package deliberately is NOT:
//
//   - It is not a decision. Everything it emits is a CANDIDATE for a human.
//     Nothing here closes, merges, relabels or transitions anything, and the
//     dedup decision stays a workflow action class (charter §5.5).
//   - It is not semantic judgement. Duplicate detection is lexical token
//     overlap, and scoring cites only the rubric lines that are decidable
//     from the filing's own STRUCTURE (S2/S4/U4). Semantic V/R-class
//     judgement is the periodic sweep's job (#2236).
//   - It is not authoritative about the charter. A rule whose rubric id is
//     absent from the repository's declared charter drops its citation, and
//     a filing that fires no surviving rule is reported Unscored with an
//     explicit charter gap — never a fabricated citation (charter §5.1,
//     §6.6).
//
// Evaluate is total: it returns Signals and never an error, because there is
// no decision a caller could make on one. A partial or empty input yields a
// degraded Signals, and the caller files the work item regardless.
package intakegroom

import (
	"strings"
	"time"
)

// Budget constants. They live here, next to the derivations they bound, so
// the hook that spends the budget and the logic that fills it cannot drift.
const (
	// DefaultDeadline bounds the hook's own derived context.
	//
	// HONEST STATEMENT OF THE BOUND: a context deadline does not preempt a
	// callee that never consults the context. This deadline therefore bounds
	// the read for a CANCELLATION-COOPERATIVE reader, which the production
	// path is — githubclient builds every request with
	// http.NewRequestWithContext and Go's HTTP client honours a context
	// deadline, so the real provider read returns at the deadline. A reader
	// that blocks without consulting ctx is NOT bounded by this mechanism,
	// and a test that claims otherwise would be testing a fiction rather
	// than the plumbing.
	//
	// 3s is chosen against the filing path it rides on: a work-item filing
	// is already a multi-round-trip forge write, so a bounded 3s worst case
	// is within the noise of the operation it augments, while being long
	// enough that a healthy single-page enumeration completes inside it.
	DefaultDeadline = 3 * time.Second

	// DefaultMaxScanned bounds how many items the PROVIDER enumerates for
	// the duplicate window. 300 newest-first items is roughly three GraphQL
	// pages — enough that a duplicate filed in the recent past is in the
	// window, small enough that the enumeration fits the deadline above.
	DefaultMaxScanned = 300

	// MaxDuplicates caps how many duplicate candidates are reported. Three
	// is a list a human reads; a longer one is a list a human skips.
	MaxDuplicates = 3
)

// Similarity thresholds for the duplicate confidence bands. They are named
// constants with unit tests precisely because lexical matching's
// false-positive rate is a tuning question, not a correctness one: the bands
// can move without touching any wiring.
const (
	// ThresholdHigh is the score at or above which a duplicate candidate is
	// reported at high confidence.
	ThresholdHigh = 0.60
	// ThresholdMedium is the score at or above which a duplicate candidate
	// is reported at medium confidence.
	ThresholdMedium = 0.45
	// ThresholdLow is the floor: below it, a pair is not a candidate at all.
	ThresholdLow = 0.30
)

// Confidence is the reported band of a similarity score. It exists so a
// reader dismisses a wrong candidate on the band rather than reverse
// engineering a float.
type Confidence string

// The confidence bands.
const (
	// ConfidenceHigh is a score at or above ThresholdHigh.
	ConfidenceHigh Confidence = "high"
	// ConfidenceMedium is a score at or above ThresholdMedium.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceLow is a score at or above ThresholdLow.
	ConfidenceLow Confidence = "low"
)

// AtLeastMedium reports whether c is medium or high — the band at which a
// duplicate candidate is strong enough to fire the S2 scoring rule.
func (c Confidence) AtLeastMedium() bool {
	return c == ConfidenceMedium || c == ConfidenceHigh
}

// DegradeReason is the closed set of reasons intake grooming produced no
// signals. It is a closed set so the surfaces that report it (the 201
// response, the audit summary, the rendered body) can enumerate it, and so a
// degradation is always attributable to a named cause rather than to an
// unexplained empty result.
//
// WHY DEGRADING IS THE RIGHT POSTURE HERE, and why that is not an oversight:
// everywhere else in E54 a missing or unresolvable charter FAILS CLOSED,
// because a grooming run's whole output is charter-anchored ranking and an
// unanchored ranking is worse than none. Intake grooming is the deliberate
// exception. It rides on work-item FILING — a load-bearing write path used
// by operator follow-up filing (#1005), product-issue reporting (#1006),
// deferred review concerns and refinement filing. Making a filing depend on
// grooming health would convert an advisory enhancement into a new failure
// mode on a path whose job is to record work reliably. So every failure here
// is swallowed into one of these reasons, the item is filed anyway, and the
// reason is reported rather than hidden.
type DegradeReason string

// The closed DegradeReason set.
const (
	// DegradeReasonReaderUnavailable: the work-item read capability is not
	// implemented or not configured for the target provider.
	DegradeReasonReaderUnavailable DegradeReason = "reader_unavailable"
	// DegradeReasonReaderError: the reader was available and returned an
	// error (forbidden, rate limited, transport failure).
	DegradeReasonReaderError DegradeReason = "reader_error"
	// DegradeReasonCharterUndeclared: the repository's conventions declare
	// no charter, so there is no rubric to cite.
	DegradeReasonCharterUndeclared DegradeReason = "charter_undeclared"
	// DegradeReasonCharterUnresolved: a charter is declared but could not be
	// resolved from the base ref.
	DegradeReasonCharterUnresolved DegradeReason = "charter_unresolved"
	// DegradeReasonCharterRubricUnparsed: the charter resolved but no rubric
	// line ids could be parsed from it.
	DegradeReasonCharterRubricUnparsed DegradeReason = "charter_rubric_unparsed"
	// DegradeReasonBudgetExceeded: the hook's deadline elapsed before the
	// read completed.
	DegradeReasonBudgetExceeded DegradeReason = "budget_exceeded"
	// DegradeReasonHookPanic: a panic inside the hook was recovered.
	DegradeReasonHookPanic DegradeReason = "hook_panic"
	// DegradeReasonSeamUnwired: a seam the hook needs (document resolver,
	// base ref) is not configured on this deployment.
	DegradeReasonSeamUnwired DegradeReason = "seam_unwired"
)

// DegradeReasons returns the closed reason set in a stable order. It is the
// single source the documented surfaces enumerate, so a new reason cannot be
// added without the enumerating tests and docs seeing it.
func DegradeReasons() []DegradeReason {
	return []DegradeReason{
		DegradeReasonReaderUnavailable,
		DegradeReasonReaderError,
		DegradeReasonCharterUndeclared,
		DegradeReasonCharterUnresolved,
		DegradeReasonCharterRubricUnparsed,
		DegradeReasonBudgetExceeded,
		DegradeReasonHookPanic,
		DegradeReasonSeamUnwired,
	}
}

// Candidate is one already-existing work item in the scanned window. It is
// the package's own neutral shape: the caller adapts provider records into
// it, so this package never imports workmgmt or a forge client.
type Candidate struct {
	// Number is the item's tracker number.
	Number int
	// Title is the item's title.
	Title string
	// Body is the item's body. Carried for the self-match guard; the
	// similarity score itself is title-only (see duplicate.go).
	Body string
	// Labels are the item's label names, as declared by the tracker.
	Labels []string
	// URL is the item's canonical URL, rendered into the advisory section.
	URL string
	// Closed reports whether the item is already closed. A closed duplicate
	// is still worth surfacing — it is often the resolution.
	Closed bool
}

// Filing is the item being filed, as Apply just rendered it. It is the
// package's own neutral shape for the same reason Candidate is.
type Filing struct {
	// Title is the rendered title, conventions prefix included.
	Title string
	// Summary is the one-line summary the filer supplied.
	Summary string
	// Body is the rendered body, before any advisory section is appended.
	Body string
	// Labels are the rendered label names.
	Labels []string
	// Type is the work-item type (bug, feature, adr, ...).
	Type string
	// ParentEpicRef is the declared parent epic reference, empty when the
	// filer omitted one. Emptiness is what enables the epic suggestion and
	// what fires the S4 structural rule.
	ParentEpicRef string
	// DependsOn are the declared dependency edges. Emptiness fires U4.
	DependsOn []string
	// MissingLabelNamespaces are the conventions-required label namespaces
	// the filing did not populate. Non-emptiness fires S4.
	MissingLabelNamespaces []string
}

// Charter is the repository's resolved charter, reduced to what scoring
// needs. ContentHash is carried so a reader can tell which charter revision
// a score was anchored to.
type Charter struct {
	// Path is the charter's repo-relative path, as the conventions declare it.
	Path string
	// ContentHash identifies the resolved charter revision.
	ContentHash string
	// RubricIDs is the rubric parsed out of the charter — the ids that may
	// be cited, each with its line text for quoting. An empty rubric is a
	// degradation, never a licence to cite an id that is not there.
	RubricIDs Rubric
}

// Citation is one rubric line a score cites, with the quote a reviewer
// reads and the note saying what in the filing fired it.
type Citation struct {
	// RubricID is the charter rubric line id (V1, R3, S4, ...).
	RubricID string `json:"rubric_id"`
	// Quote is the rubric line's text, as parsed from the charter.
	Quote string `json:"quote"`
	// Note says what about this filing fired the line.
	Note string `json:"note"`
}

// Score is the provisional, charter-anchored structural score.
//
// Value is ADVISORY ONLY. Charter §5.1 says to cite rather than blend, so
// the citations are what a reviewer reads; the number exists to sort a list,
// not to justify a ranking.
type Score struct {
	// Value is the summed weight of the surviving citations.
	Value float64 `json:"value"`
	// Citations are the rubric lines this filing fires, in rule order.
	Citations []Citation `json:"citations,omitempty"`
	// Unscored reports that no citation survived. It is set rather than a
	// citation being invented — charter §6.6 makes a gap a finding.
	Unscored bool `json:"unscored"`
	// CharterGap explains the Unscored verdict.
	CharterGap string `json:"charter_gap,omitempty"`
}

// DuplicateCandidate is one possible duplicate of the filing.
type DuplicateCandidate struct {
	// Number is the candidate item's tracker number.
	Number int `json:"number"`
	// URL is the candidate item's URL.
	URL string `json:"url,omitempty"`
	// Title is the candidate item's title.
	Title string `json:"title"`
	// Score is the similarity score in [0,1].
	Score float64 `json:"score"`
	// Confidence is the band Score falls in.
	Confidence Confidence `json:"confidence"`
	// Basis is the shared token set, rendered — what makes a wrong candidate
	// cheap to dismiss.
	Basis string `json:"basis"`
	// Closed reports whether the candidate is already closed.
	Closed bool `json:"closed"`
}

// EpicSuggestion is the single best parent-epic candidate for a filing that
// declared none.
type EpicSuggestion struct {
	// Number is the suggested epic's tracker number.
	Number int `json:"number"`
	// URL is the suggested epic's URL.
	URL string `json:"url,omitempty"`
	// Title is the suggested epic's title.
	Title string `json:"title"`
	// Score is the similarity score in [0,1].
	Score float64 `json:"score"`
	// Confidence is the band Score falls in.
	Confidence Confidence `json:"confidence"`
	// Basis is the shared token set, rendered.
	Basis string `json:"basis"`
}

// Signals is everything intake grooming derived for one filing. It is what
// the 201 response carries, what the hidden body marker encodes, and what
// #2236's periodic sweep reads back instead of redoing the analysis.
type Signals struct {
	// Duplicates are the possible duplicates, best first, at most
	// MaxDuplicates.
	Duplicates []DuplicateCandidate `json:"duplicates,omitempty"`
	// EpicSuggestion is the suggested parent epic, nil when the filing
	// declared one or nothing scored above threshold.
	EpicSuggestion *EpicSuggestion `json:"epic_suggestion,omitempty"`
	// Score is the provisional structural score.
	Score Score `json:"score"`
	// Degraded reports that grooming did not complete.
	Degraded bool `json:"degraded"`
	// DegradeReason names why, from the closed set.
	DegradeReason DegradeReason `json:"degrade_reason,omitempty"`
	// ScannedItems is how many existing items were compared against.
	ScannedItems int `json:"scanned_items"`
	// WindowTruncated reports that the scan window was cut at its cap rather
	// than exhausted, so a duplicate outside the window would be missed.
	// Set by the CALLER from the read's page result — Evaluate cannot know
	// whether the slice it was handed is the whole set.
	WindowTruncated bool `json:"window_truncated"`
	// DurationMS is the hook's measured wall clock. Set by the CALLER, so
	// the latency claim is reported rather than assumed.
	DurationMS int64 `json:"duration_ms"`
}

// HasFindings reports whether the signals carry anything a human would read.
//
// Unscored deliberately does NOT count: a degraded run produces no
// duplicates, no suggestion and no citations, and rendering "unscored
// because the reader was unavailable" onto every filed body would be noise
// on a path whose promise is that a degraded hook is invisible. RenderBody
// uses this to keep a degraded filing's body byte-identical to today's.
func (s Signals) HasFindings() bool {
	return len(s.Duplicates) > 0 || s.EpicSuggestion != nil || len(s.Score.Citations) > 0
}

// Degrade returns a Signals carrying only the degradation. It is the one
// constructor the hook's error paths use, so every swallowed failure
// produces the same shape.
func Degrade(reason DegradeReason) Signals {
	return Signals{Degraded: true, DegradeReason: reason}
}

// Evaluate derives every signal for one filing against the scanned window
// and the resolved charter. It is pure and total: it never returns an error,
// because there is no caller decision to make on one.
//
// The caller sets WindowTruncated and DurationMS afterwards, and may
// override DegradeReason with a more specific cause it observed upstream
// (an unresolvable charter is charter_unresolved, not the
// charter_rubric_unparsed an empty Charter would produce here).
func Evaluate(f Filing, candidates []Candidate, c Charter) Signals {
	s := Signals{ScannedItems: len(candidates)}
	s.Duplicates = Duplicates(f, candidates)
	if strings.TrimSpace(f.ParentEpicRef) == "" {
		s.EpicSuggestion = SuggestEpic(f, candidates)
	}
	s.Score = ScoreFiling(f, s.Duplicates, c.RubricIDs)
	if c.RubricIDs.Len() == 0 {
		s.Degraded = true
		s.DegradeReason = DegradeReasonCharterRubricUnparsed
	}
	return s
}
