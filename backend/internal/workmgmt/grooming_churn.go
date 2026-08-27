package workmgmt

// This file carries the grooming CHURN GUARD (E54.8 / #2240): the pure,
// deterministic run-over-run filter that turns a schema-valid
// plan.GroomingReport into the PROPOSAL SET an operator actually sees.
//
// WHY IT EXISTS. A scoring agent re-run over a large backlog perturbs the
// ranking every time — item 14 and item 15 swap, a score moves by 0.02 — and
// none of it merits an operator's attention. Left unguarded, every run
// produces a long report of trivia and the operator learns to approve without
// reading. ADR-065 names that as its own sharpest adoption risk, because at
// that point the grooming gate is decorative and every other control in E54
// rests on it.
//
// THE THREE-STATE BASELINE (#2240 Notes). Diffing naively against the last
// PROPOSED report is the convergence trap: a rejected proposal is re-derived
// and re-proposed every run, and the operator re-rejects it forever — which
// trains dismissal harder than plain churn does. So the baseline distinguishes
// three states, exactly as the exhaustive-review pattern dedupes against
// everything SEEN rather than everything ACCEPTED:
//
//	applied        — proposed, decided, and the mutation LANDED
//	rejected       — proposed and the operator said no (or amended it)
//	never_proposed — ABSENCE from the baseline map; the fail-safe direction
//
// Absence is the fail-safe because this guard SUPPRESSES: every ambiguity
// resolves toward proposing. An entry whose apply FAILED is therefore absent,
// not applied, so a mutation that did not land resurfaces next run.
//
// PROSE IS EXCLUDED FROM EVERY FINGERPRINT, and that is the load-bearing
// design assumption. The agent regenerates rationale/basis/detail text on
// every run, so a prose-sensitive fingerprint would report "materially
// changed" for an entry over an unchanged backlog and defeat the guard
// outright. See groomingEntryBasis for the per-class field table and the
// residuals that choice accepts.

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// GroomingChurnFilteredCategory is the audit category written once per guard
// pass, carrying the counts, the suppressed/resurfaced entry ids, the
// charter-changed flag and the no-changes-proposed flag. ONE category, so a
// single filter returns the whole pass — the same family-not-outcome naming
// grooming_mutation_applied uses.
//
// Declared here — in non-test Go source, with "Category" in the constant name
// — because audit/categories_completeness_test.go AST-collects exactly that
// shape and fails the build when an emitted category is absent from
// audit.KnownCategories.
const GroomingChurnFilteredCategory = "grooming_churn_filtered"

// GroomingDisposition is what the LAST decided-and-applied run did with an
// entry. Only the two RECORDED states are named: the third, never_proposed, is
// ABSENCE from GroomingBaseline.Entries and deliberately has no constant — a
// named zero value would invite a caller to store it, and a stored
// "never proposed" is indistinguishable from a lost record.
type GroomingDisposition string

const (
	// GroomingDispositionApplied marks an entry whose mutation LANDED.
	GroomingDispositionApplied GroomingDisposition = "applied"
	// GroomingDispositionRejected marks an entry the operator refused. An
	// AMENDED verdict maps here too: amended is conservatively not
	// applied-as-proposed, the same call grooming_apply.go makes.
	GroomingDispositionRejected GroomingDisposition = "rejected"
)

// Suppression reasons. Every withheld entry names exactly one, so an audit
// reader can tell a significance-bar refusal from an already-decided one.
const (
	// GroomingSuppressBelowHygieneSignificance marks a hygiene defect whose
	// class is outside the operator's declared significance set.
	GroomingSuppressBelowHygieneSignificance = "below_hygiene_significance"
	// GroomingSuppressBelowOrderingThreshold marks an ordering entry whose
	// rank AND score movement both stayed under their bars.
	GroomingSuppressBelowOrderingThreshold = "below_ordering_threshold"
	// GroomingSuppressAlreadyApplied marks an unchanged entry whose mutation
	// already landed.
	GroomingSuppressAlreadyApplied = "already_applied"
	// GroomingSuppressPreviouslyRejected marks an unchanged entry the operator
	// already refused — AC5's convergence property.
	GroomingSuppressPreviouslyRejected = "previously_rejected"
)

// Resurface reasons. A resurfaced entry is PROPOSED and says what changed,
// which is the second half of AC5.
const (
	// GroomingResurfaceBasisChanged marks an already-decided entry whose
	// STRUCTURAL basis moved.
	GroomingResurfaceBasisChanged = "basis_changed"
	// GroomingResurfaceCharterChanged marks a charter-anchored entry whose
	// suppression was computed under a charter revision that has since moved.
	GroomingResurfaceCharterChanged = "charter_changed"
	// GroomingResurfaceUnknownBaselineClass marks a baseline entry carrying a
	// class this build does not recognize. Fail OPEN toward proposing: an
	// unreadable baseline record must never silently suppress.
	GroomingResurfaceUnknownBaselineClass = "unknown_baseline_class"
	// GroomingResurfaceUnknownBaselineDisposition marks a baseline entry
	// carrying neither recorded disposition — a malformed record. Same fail-open
	// direction, named distinctly from the class anomaly so an audit reader can
	// tell an unreadable CLASS from an unreadable DECISION.
	GroomingResurfaceUnknownBaselineDisposition = "unknown_baseline_disposition"
)

// GroomingBaselineEntry is one entry's record in the last decided-and-applied
// state. Rank and Score are carried only for the ordering class; they are the
// NUMERIC comparands the thresholds are measured against, which is why they are
// deliberately absent from the basis fingerprint.
type GroomingBaselineEntry struct {
	Class       string              `json:"class"`
	Disposition GroomingDisposition `json:"disposition"`
	BasisHash   string              `json:"basis_hash"`
	Rank        int                 `json:"rank,omitempty"`
	Score       float64             `json:"score,omitempty"`
}

// GroomingBaseline is the state a new report is diffed against: the entries of
// the last decided-and-applied report keyed by their derived entry id, plus the
// charter revision that report was scored under.
//
// A ZERO GroomingBaseline is the first-ever-run case and is valid input: every
// entry is then absent, so everything the significance bar admits is proposed.
type GroomingBaseline struct {
	CharterHash string                           `json:"charter_hash,omitempty"`
	Entries     map[string]GroomingBaselineEntry `json:"entries,omitempty"`
}

// GroomingProposalSet is the six filtered entry slices — a PROJECTION of a
// report, deliberately NOT a grooming_report_v1 document.
//
// That is not an oversight. grooming-report-v1 requires `ordering` with
// minItems 1, and an EMPTY proposal set is precisely the outcome AC1 demands
// ("a run over an unchanged backlog produces an empty report"), so the empty
// case cannot be expressed as a valid report artifact. If the empty case must
// become a valid report document, that is a grooming-report-v1 change belonging
// to #2235, exactly as #2240's own Notes direct for id-stability gaps.
//
// Every slice is non-nil after a filter pass, so the set marshals to six empty
// ARRAYS rather than nulls — AC6's byte-identity is over that fixed form.
type GroomingProposalSet struct {
	Ordering                 []plan.OrderingEntry           `json:"ordering"`
	Duplicates               []plan.DuplicateCandidate      `json:"duplicates"`
	HygieneDefects           []plan.HygieneDefect           `json:"hygiene_defects"`
	DependencyEdges          []plan.DependencyEdge          `json:"dependency_edges"`
	VisionDrift              []plan.VisionDriftFlag         `json:"vision_drift"`
	DecompositionSuggestions []plan.DecompositionSuggestion `json:"decomposition_suggestions"`
}

// IsEmpty reports whether nothing survived the filter — the "no changes
// proposed" outcome.
func (p GroomingProposalSet) IsEmpty() bool {
	return len(p.Ordering) == 0 && len(p.Duplicates) == 0 &&
		len(p.HygieneDefects) == 0 && len(p.DependencyEdges) == 0 &&
		len(p.VisionDrift) == 0 && len(p.DecompositionSuggestions) == 0
}

// GroomingSuppression records one WITHHELD entry. Suppressed differences are
// computed and reported, never silently discarded (AC2).
type GroomingSuppression struct {
	EntryID string `json:"entry_id"`
	Class   string `json:"class"`
	Reason  string `json:"reason"`
}

// GroomingResurface records one already-decided entry that is proposed again
// because its basis moved, naming WHAT changed (AC5's second half).
type GroomingResurface struct {
	EntryID      string `json:"entry_id"`
	Class        string `json:"class"`
	Reason       string `json:"reason"`
	ChangedField string `json:"changed_field,omitempty"`
}

// GroomingChurnSummary is the audit payload: counts, sorted id lists, and the
// two run-level flags. Anomalies names baseline records this build could not
// interpret; each one FAILED OPEN toward proposing.
type GroomingChurnSummary struct {
	Proposed          int      `json:"proposed"`
	Suppressed        int      `json:"suppressed"`
	Resurfaced        int      `json:"resurfaced"`
	ProposedIDs       []string `json:"proposed_ids,omitempty"`
	SuppressedIDs     []string `json:"suppressed_ids,omitempty"`
	ResurfacedIDs     []string `json:"resurfaced_ids,omitempty"`
	Anomalies         []string `json:"anomalies,omitempty"`
	CharterChanged    bool     `json:"charter_changed"`
	NoChangesProposed bool     `json:"no_changes_proposed"`
	BaselineEntries   int      `json:"baseline_entries"`
}

// GroomingChurnResult is one guard pass.
type GroomingChurnResult struct {
	Proposals         GroomingProposalSet   `json:"proposals"`
	Suppressed        []GroomingSuppression `json:"suppressed,omitempty"`
	Resurfaced        []GroomingResurface   `json:"resurfaced,omitempty"`
	NoChangesProposed bool                  `json:"no_changes_proposed"`
	CharterChanged    bool                  `json:"charter_changed"`
	Summary           GroomingChurnSummary  `json:"summary"`
}

// groomingCharterAnchoredClasses are the classes whose finding is only
// meaningful RELATIVE to the charter: an ordering is a rubric-cited ranking and
// a vision-drift flag is a non-goal/phase-theme judgment, so a suppression
// computed under a charter revision that has since moved was made against a
// rubric that no longer holds.
//
// The OBJECTIVE classes (hygiene, dependency, duplicate, decomposition) are
// deliberately excluded: an applied label fix does not become un-applied
// because the charter was edited. The narrower alternative (record charter
// drift but never lift) would keep suppressing under a rubric that changed,
// which is the unsafe direction for a suppression control; the wider
// alternative (lift everything) is a churn cannon.
var groomingCharterAnchoredClasses = map[string]bool{
	plan.GroomingClassOrdering:    true,
	plan.GroomingClassVisionDrift: true,
}

// groomingKnownClasses is the closed set of report entry classes. A baseline
// record naming anything else is an anomaly, not a suppression.
var groomingKnownClasses = map[string]bool{
	plan.GroomingClassOrdering:      true,
	plan.GroomingClassDuplicate:     true,
	plan.GroomingClassHygiene:       true,
	plan.GroomingClassDependency:    true,
	plan.GroomingClassVisionDrift:   true,
	plan.GroomingClassDecomposition: true,
}

// NewGroomingBaseline derives the three-state baseline from the last
// decided-and-applied grooming run: the PRIOR report supplies each entry's
// structural basis (and, for ordering, its rank/score), the operator's
// decisions supply rejections, and the apply result supplies what actually
// LANDED. Pure — no I/O, no clock, no provider.
//
// The join key is the entry id throughout, which is the whole epic's join key
// (#2235 makes it derived-and-recomposable precisely so this diff is mechanical
// rather than aspirational).
//
// The disposition table, stated once:
//
//	applied   ← an apply record with outcome applied, OR outcome skipped with
//	            reason already_applied (the idempotent re-apply: the proposed
//	            state IS the observed state, so the proposal has landed)
//	rejected  ← an operator verdict of rejected or amended
//	ABSENT    ← everything else: no decision at all, an approved entry with no
//	            apply record, or an apply that FAILED or was skipped for any
//	            containment reason
//
// The absence direction is the fail-safe. This guard suppresses, so an entry
// whose mutation did not land must resurface next run rather than be silently
// withheld — which is also exactly what AC4 demands: diff against the last
// APPLIED state, never the last PROPOSED one.
//
// A nil prior report yields the zero baseline (first-ever run).
func NewGroomingBaseline(prior *plan.GroomingReport, decisions []GroomingDecision, applied *GroomingApplyResult) GroomingBaseline {
	out := GroomingBaseline{Entries: map[string]GroomingBaselineEntry{}}
	if prior == nil {
		return out
	}
	if prior.CharterRef != nil {
		out.CharterHash = prior.CharterRef.ContentHash
	}

	// Dispositions first, so the entry walk can look each one up. Applied wins
	// over rejected: a landed mutation is a fact, a verdict is an intent.
	disp := map[string]GroomingDisposition{}
	for _, d := range decisions {
		switch d.Verdict {
		case GroomingRejected, GroomingAmended:
			disp[d.EntryID] = GroomingDispositionRejected
		}
	}
	if applied != nil {
		for _, rec := range applied.Applied {
			if rec.Outcome == GroomingOutcomeApplied {
				disp[rec.EntryID] = GroomingDispositionApplied
			}
		}
		for _, rec := range applied.Skipped {
			if rec.SkipReason == GroomingSkipAlreadyApplied {
				disp[rec.EntryID] = GroomingDispositionApplied
			}
		}
	}

	add := func(id, class, basis string, rank int, score float64) {
		d, ok := disp[id]
		if !ok {
			// never_proposed: absence IS the third state.
			return
		}
		out.Entries[id] = GroomingBaselineEntry{
			Class:       class,
			Disposition: d,
			BasisHash:   basis,
			Rank:        rank,
			Score:       score,
		}
	}
	for _, e := range prior.Ordering {
		add(e.ID, plan.GroomingClassOrdering, groomingOrderingBasis(e), e.Rank, e.Score)
	}
	for _, e := range prior.Duplicates {
		add(e.ID, plan.GroomingClassDuplicate, groomingDuplicateBasis(e), 0, 0)
	}
	for _, e := range prior.HygieneDefects {
		add(e.ID, plan.GroomingClassHygiene, groomingHygieneBasis(e), 0, 0)
	}
	for _, e := range prior.DependencyEdges {
		add(e.ID, plan.GroomingClassDependency, groomingDependencyBasis(e), 0, 0)
	}
	for _, e := range prior.VisionDrift {
		add(e.ID, plan.GroomingClassVisionDrift, groomingVisionDriftBasis(e), 0, 0)
	}
	for _, e := range prior.DecompositionSuggestions {
		add(e.ID, plan.GroomingClassDecomposition, groomingDecompositionBasis(e), 0, 0)
	}
	return out
}

// groomingBasisHash is the canonical, class-tagged fingerprint envelope. The
// class tag prevents a cross-class collision on an identical payload.
func groomingBasisHash(class string, fields ...string) string {
	sum := sha256.Sum256([]byte(class + "\x00" + strings.Join(fields, "\x00")))
	return hex.EncodeToString(sum[:])
}

// The per-class STRUCTURAL basis functions. Every one deliberately excludes
// free prose, and each excludes one further field for a stated reason:
//
//	ordering       sorted, lowercased rubric_id set.
//	               EXCLUDES rank and score — they are compared NUMERICALLY
//	               against the thresholds, not by equality, so hashing them
//	               would make every sub-threshold jitter a "changed basis" and
//	               route around the very bar that exists to absorb it.
//	               EXCLUDES rationale (prose).
//	duplicate      NOTHING. The pair identity is already the entry id, and the
//	               only other fields are `basis` (prose) and `confidence` — an
//	               agent-regenerated judgment. #2240's own churn argument
//	               applies to confidence exactly as it does to score: a
//	               rejected duplicate whose confidence drifts medium→high on a
//	               re-run would resurface every cycle, defeating AC5 for the
//	               class and breaking AC6's byte-identity. Excluded, not
//	               quantized: grooming-report-v1 already types confidence as a
//	               three-value enum (low/medium/high), so it is quantized as
//	               far as it can be and a single-band flip is still pure
//	               jitter. RESIDUAL: a rejected duplicate does not resurface on
//	               a confidence change alone — the pair either overlaps or it
//	               does not, and the operator already ruled on the pair.
//	hygiene        the normalized STRUCTURED fix — the sorted trimmed label
//	               set, the normalized parent-epic ref, the lower-cased board
//	               state and the trimmed field value — so a different proposed
//	               label is a different proposal. EXCLUDES detail AND
//	               suggested_fix, both prose (#2847). It moved off
//	               suggested_fix with the value the apply path reads: with
//	               `fix` present, a re-worded sentence over an unchanged label
//	               set would be a "materially changed" proposal (defeating the
//	               guard), and two different label sets behind one sentence
//	               would be ONE proposal (suppressing a real correction).
//	dependency     kind (the edge type). Direction is already in the id.
//	               EXCLUDES basis (prose).
//	vision_drift   the basis ENUM (non_goal | phase_theme). The charter_ref_id
//	               is already the id qualifier. EXCLUDES detail (prose).
//	decomposition  the proposed-children COUNT. EXCLUDES their titles and
//	               scope hints (prose). This is the COARSEST choice in the
//	               design and a deliberate trade: two proposals to split the
//	               same item into the same number of children are one
//	               proposal, so a re-worded split does not resurface. Switching
//	               to normalized titles is a one-line change if the operator
//	               prefers the opposite trade.
func groomingOrderingBasis(e plan.OrderingEntry) string {
	ids := make([]string, 0, len(e.RubricCitations))
	for _, c := range e.RubricCitations {
		ids = append(ids, strings.ToLower(strings.TrimSpace(c.RubricID)))
	}
	sort.Strings(ids)
	return groomingBasisHash(plan.GroomingClassOrdering, ids...)
}

func groomingDuplicateBasis(_ plan.DuplicateCandidate) string {
	return groomingBasisHash(plan.GroomingClassDuplicate)
}

func groomingHygieneBasis(e plan.HygieneDefect) string {
	var labels []string
	var epic, state, value string
	if e.Fix != nil {
		for _, l := range e.Fix.Labels {
			if t := strings.TrimSpace(l); t != "" {
				labels = append(labels, t)
			}
		}
		sort.Strings(labels)
		epic = NormalizeIssueRef(e.Fix.ParentEpic)
		// Lower-cased to agree with groomingResolveBoardState's
		// case-insensitive lookup: fingerprinting `Backlog` and `backlog`
		// identically while resolving them differently is the idempotence
		// asymmetry nobody can reproduce (approval condition C6).
		state = strings.ToLower(strings.TrimSpace(e.Fix.BoardState))
		value = strings.TrimSpace(e.Fix.FieldValue)
	}
	// TWO SEPARATORS, TWO DIFFERENT JOBS. The label set is joined on US
	// (\x1f) so two different SETS do not collapse onto one fingerprint
	// (["ab"] vs ["a","b"]); the MEMBER boundary is the \x00 groomingBasisHash
	// joins FIELDS with, which is what stops a label forging the field value's
	// contribution (["ab"] vs ["a"]+"b"). Both pairs are pinned in
	// TestGroomingBasisExcludesProse — each collides under the corresponding
	// empty join, so neither claim rests on prose.
	//
	// RESIDUAL: a label containing \x1f itself still collides with the
	// equivalent two-label set here. It is bounded rather than fixed, because
	// groomingValidLabel refuses any control rune, so such a label is
	// undispatchable and the collision can only suppress a proposal that could
	// never have been applied.
	return groomingBasisHash(plan.GroomingClassHygiene, strings.Join(labels, "\x1f"), epic, state, value)
}

func groomingDependencyBasis(e plan.DependencyEdge) string {
	return groomingBasisHash(plan.GroomingClassDependency, strings.TrimSpace(e.Kind))
}

func groomingVisionDriftBasis(e plan.VisionDriftFlag) string {
	return groomingBasisHash(plan.GroomingClassVisionDrift, strings.TrimSpace(e.Basis))
}

func groomingDecompositionBasis(e plan.DecompositionSuggestion) string {
	return groomingBasisHash(plan.GroomingClassDecomposition, strconv.Itoa(len(e.ProposedChildren)))
}

// groomingChurnDecision is one entry's verdict inside the filter.
type groomingChurnDecision struct {
	propose      bool
	reason       string
	changedField string
}

// FilterGroomingChurn is the guard: it turns a schema-valid grooming report
// into the PROPOSAL SET an operator sees, given the last decided-and-applied
// baseline and the operator's resolved significance bar. Pure — no I/O, no
// clock, no provider — so a filtering regression fails on an assertion rather
// than on a forge fixture.
//
// PRECONDITION: report has already passed plan.ParseGroomingReport, whose
// semantic check enforces that entry ids are unique report-wide and recompose
// from the entry's own fields. The guard therefore trusts entry ids as join
// keys and does not re-derive them. Fed an unvalidated report carrying a
// duplicate id it stays deterministic (both entries are evaluated
// independently and the output sort is stable) rather than panicking.
//
// THE DECISION ORDER, per entry:
//
//  1. hygiene whose defect is outside the significance set → SUPPRESS.
//     FIRST, ahead of the baseline lookup, so the bar governs NEW findings as
//     well as baseline matches (#2240 approval condition J2). An operator who
//     declares a defect class insignificant expects not to see it — not to
//     see it once, on its first appearance, and only then never again.
//  2. absent from the baseline → PROPOSE. A finding that was never decided is
//     not churn.
//  3. a baseline record whose class OR disposition this build cannot read →
//     PROPOSE, recording the anomaly. BOTH integrity checks run here, ahead of
//     EVERY suppression path, because the ordering-threshold branch at (5)
//     never consults the disposition: validating it only at (6) let a
//     degraded ordering record be suppressed on a sub-threshold move.
//  4. structural basis differs from the baseline's → PROPOSE, recording a
//     resurface that NAMES what changed (AC5's second half).
//  5. charter-anchored class under a moved charter → PROPOSE, recording a
//     charter_changed resurface.
//  6. ordering whose |rank delta| >= MinRankMovement OR |score delta| >=
//     MinScoreDelta → PROPOSE; otherwise SUPPRESS below_ordering_threshold.
//     The deltas are measured against the last APPLIED rank/score, so a
//     crossing move is a genuinely different proposal even for a
//     previously-rejected entry.
//  7. otherwise SUPPRESS, naming the baseline disposition.
func FilterGroomingChurn(report *plan.GroomingReport, baseline GroomingBaseline, th GroomingThresholds) GroomingChurnResult {
	res := GroomingChurnResult{
		Proposals: GroomingProposalSet{
			Ordering:                 []plan.OrderingEntry{},
			Duplicates:               []plan.DuplicateCandidate{},
			HygieneDefects:           []plan.HygieneDefect{},
			DependencyEdges:          []plan.DependencyEdge{},
			VisionDrift:              []plan.VisionDriftFlag{},
			DecompositionSuggestions: []plan.DecompositionSuggestion{},
		},
	}
	res.Summary.BaselineEntries = len(baseline.Entries)

	if report == nil {
		// A nil report proposes nothing and says so, rather than panicking:
		// "no changes proposed" is the correct reading of "no report".
		res.NoChangesProposed = true
		res.Summary.NoChangesProposed = true
		return res
	}

	res.CharterChanged = groomingCharterMoved(report, baseline)
	res.Summary.CharterChanged = res.CharterChanged

	var anomalies []string
	decide := func(id, class, basis string, rank int, score float64, defect string) bool {
		d := groomingDecide(id, class, basis, rank, score, defect, baseline, th, res.CharterChanged, &anomalies)
		switch {
		case d.propose && d.reason != "":
			res.Resurfaced = append(res.Resurfaced, GroomingResurface{
				EntryID: id, Class: class, Reason: d.reason, ChangedField: d.changedField,
			})
		case !d.propose:
			res.Suppressed = append(res.Suppressed, GroomingSuppression{
				EntryID: id, Class: class, Reason: d.reason,
			})
		}
		return d.propose
	}

	for _, e := range report.Ordering {
		if decide(e.ID, plan.GroomingClassOrdering, groomingOrderingBasis(e), e.Rank, e.Score, "") {
			res.Proposals.Ordering = append(res.Proposals.Ordering, e)
		}
	}
	for _, e := range report.Duplicates {
		if decide(e.ID, plan.GroomingClassDuplicate, groomingDuplicateBasis(e), 0, 0, "") {
			res.Proposals.Duplicates = append(res.Proposals.Duplicates, e)
		}
	}
	for _, e := range report.HygieneDefects {
		if decide(e.ID, plan.GroomingClassHygiene, groomingHygieneBasis(e), 0, 0, e.Defect) {
			res.Proposals.HygieneDefects = append(res.Proposals.HygieneDefects, e)
		}
	}
	for _, e := range report.DependencyEdges {
		if decide(e.ID, plan.GroomingClassDependency, groomingDependencyBasis(e), 0, 0, "") {
			res.Proposals.DependencyEdges = append(res.Proposals.DependencyEdges, e)
		}
	}
	for _, e := range report.VisionDrift {
		if decide(e.ID, plan.GroomingClassVisionDrift, groomingVisionDriftBasis(e), 0, 0, "") {
			res.Proposals.VisionDrift = append(res.Proposals.VisionDrift, e)
		}
	}
	for _, e := range report.DecompositionSuggestions {
		if decide(e.ID, plan.GroomingClassDecomposition, groomingDecompositionBasis(e), 0, 0, "") {
			res.Proposals.DecompositionSuggestions = append(res.Proposals.DecompositionSuggestions, e)
		}
	}

	sortGroomingProposals(&res.Proposals)
	sort.SliceStable(res.Suppressed, func(i, j int) bool { return res.Suppressed[i].EntryID < res.Suppressed[j].EntryID })
	sort.SliceStable(res.Resurfaced, func(i, j int) bool { return res.Resurfaced[i].EntryID < res.Resurfaced[j].EntryID })

	res.NoChangesProposed = res.Proposals.IsEmpty()
	res.Summary = groomingChurnSummary(res, anomalies, len(baseline.Entries))
	return res
}

// groomingCharterMoved reports whether the report's charter revision differs
// from the one the baseline was decided under. BOTH sides must name a hash: an
// absent charter_ref on either side is "unknown", not "different", because
// charter_ref is x-intended-required rather than required in
// grooming-report-v1 and treating unknown as moved would lift every
// charter-anchored suppression on every report that omits it.
func groomingCharterMoved(report *plan.GroomingReport, baseline GroomingBaseline) bool {
	if report.CharterRef == nil || report.CharterRef.ContentHash == "" || baseline.CharterHash == "" {
		return false
	}
	return report.CharterRef.ContentHash != baseline.CharterHash
}

// groomingDecide is the per-entry decision order documented on
// FilterGroomingChurn. It appends to anomalies rather than returning them so
// the caller's six typed loops stay one line each.
func groomingDecide(
	id, class, basis string, rank int, score float64, defect string,
	baseline GroomingBaseline, th GroomingThresholds, charterChanged bool,
	anomalies *[]string,
) groomingChurnDecision {
	// (1) The hygiene significance bar, ahead of the baseline lookup (J2).
	if class == plan.GroomingClassHygiene && !th.IsSignificantHygieneDefect(defect) {
		return groomingChurnDecision{propose: false, reason: GroomingSuppressBelowHygieneSignificance}
	}

	prev, known := baseline.Entries[id]
	if !known {
		// (2) Never proposed. A new finding is not churn.
		return groomingChurnDecision{propose: true}
	}

	// (3) A baseline record this build cannot interpret must never suppress.
	// Both integrity checks live HERE, ahead of every suppression path, rather
	// than the disposition being validated by the switch at (6): the ordering
	// branch at (5) returns below_ordering_threshold without ever reading the
	// disposition, so a record carrying neither recorded disposition was
	// SUPPRESSED whenever its rank and score deltas were sub-threshold —
	// exactly the fail-toward-proposing direction this guard must never
	// violate.
	if !groomingKnownClasses[prev.Class] {
		*anomalies = append(*anomalies, "unknown_baseline_class:"+id+":"+prev.Class)
		return groomingChurnDecision{propose: true, reason: GroomingResurfaceUnknownBaselineClass, changedField: "class"}
	}
	if prev.Disposition != GroomingDispositionApplied && prev.Disposition != GroomingDispositionRejected {
		*anomalies = append(*anomalies, "unknown_baseline_disposition:"+id+":"+string(prev.Disposition))
		return groomingChurnDecision{propose: true, reason: GroomingResurfaceUnknownBaselineDisposition, changedField: "disposition"}
	}

	// (4) Structural basis moved → propose and say what changed.
	if prev.BasisHash != basis {
		return groomingChurnDecision{
			propose:      true,
			reason:       GroomingResurfaceBasisChanged,
			changedField: groomingBasisFieldName(class),
		}
	}

	// (5) The charter this suppression was computed under has moved.
	if charterChanged && groomingCharterAnchoredClasses[class] {
		return groomingChurnDecision{propose: true, reason: GroomingResurfaceCharterChanged, changedField: "charter_ref.content_hash"}
	}

	// (6) Ordering movement against the last APPLIED position. Reached only
	// with a VALIDATED disposition, per (3).
	if class == plan.GroomingClassOrdering {
		rankDelta := rank - prev.Rank
		if rankDelta < 0 {
			rankDelta = -rankDelta
		}
		scoreDelta := score - prev.Score
		if scoreDelta < 0 {
			scoreDelta = -scoreDelta
		}
		if rankDelta >= th.MinRankMovement || scoreDelta >= th.MinScoreDelta {
			return groomingChurnDecision{propose: true}
		}
		return groomingChurnDecision{propose: false, reason: GroomingSuppressBelowOrderingThreshold}
	}

	// (7) Unchanged and already decided. The disposition is one of exactly two
	// values here — (3) proposed and returned for anything else.
	if prev.Disposition == GroomingDispositionApplied {
		return groomingChurnDecision{propose: false, reason: GroomingSuppressAlreadyApplied}
	}
	return groomingChurnDecision{propose: false, reason: GroomingSuppressPreviouslyRejected}
}

// groomingBasisFieldName names the structural field a class's fingerprint
// covers, so a resurface record says what changed rather than only that
// something did.
func groomingBasisFieldName(class string) string {
	switch class {
	case plan.GroomingClassOrdering:
		return "rubric_citations"
	case plan.GroomingClassHygiene:
		return "fix"
	case plan.GroomingClassDependency:
		return "kind"
	case plan.GroomingClassVisionDrift:
		return "basis"
	case plan.GroomingClassDecomposition:
		return "proposed_children"
	default:
		return "basis"
	}
}

// sortGroomingProposals orders every emitted slice by entry id. This is what
// makes AC6 hold: an agent may emit its arrays in any order run to run, and
// without this the serialized bytes would differ for an identical proposal set.
// Stable sorts, so an unvalidated report carrying a duplicate id still
// serializes deterministically.
func sortGroomingProposals(p *GroomingProposalSet) {
	sort.SliceStable(p.Ordering, func(i, j int) bool { return p.Ordering[i].ID < p.Ordering[j].ID })
	sort.SliceStable(p.Duplicates, func(i, j int) bool { return p.Duplicates[i].ID < p.Duplicates[j].ID })
	sort.SliceStable(p.HygieneDefects, func(i, j int) bool { return p.HygieneDefects[i].ID < p.HygieneDefects[j].ID })
	sort.SliceStable(p.DependencyEdges, func(i, j int) bool { return p.DependencyEdges[i].ID < p.DependencyEdges[j].ID })
	sort.SliceStable(p.VisionDrift, func(i, j int) bool { return p.VisionDrift[i].ID < p.VisionDrift[j].ID })
	sort.SliceStable(p.DecompositionSuggestions, func(i, j int) bool {
		return p.DecompositionSuggestions[i].ID < p.DecompositionSuggestions[j].ID
	})
}

// groomingChurnSummary builds the audit payload. Every id list is sorted, so
// two passes over identical inputs marshal to identical bytes (AC6).
func groomingChurnSummary(res GroomingChurnResult, anomalies []string, baselineEntries int) GroomingChurnSummary {
	sum := GroomingChurnSummary{
		Suppressed:        len(res.Suppressed),
		Resurfaced:        len(res.Resurfaced),
		CharterChanged:    res.CharterChanged,
		NoChangesProposed: res.NoChangesProposed,
		BaselineEntries:   baselineEntries,
	}
	for _, e := range res.Proposals.Ordering {
		sum.ProposedIDs = append(sum.ProposedIDs, e.ID)
	}
	for _, e := range res.Proposals.Duplicates {
		sum.ProposedIDs = append(sum.ProposedIDs, e.ID)
	}
	for _, e := range res.Proposals.HygieneDefects {
		sum.ProposedIDs = append(sum.ProposedIDs, e.ID)
	}
	for _, e := range res.Proposals.DependencyEdges {
		sum.ProposedIDs = append(sum.ProposedIDs, e.ID)
	}
	for _, e := range res.Proposals.VisionDrift {
		sum.ProposedIDs = append(sum.ProposedIDs, e.ID)
	}
	for _, e := range res.Proposals.DecompositionSuggestions {
		sum.ProposedIDs = append(sum.ProposedIDs, e.ID)
	}
	for _, s := range res.Suppressed {
		sum.SuppressedIDs = append(sum.SuppressedIDs, s.EntryID)
	}
	for _, r := range res.Resurfaced {
		sum.ResurfacedIDs = append(sum.ResurfacedIDs, r.EntryID)
	}
	sort.Strings(sum.ProposedIDs)
	sort.Strings(sum.SuppressedIDs)
	sort.Strings(sum.ResurfacedIDs)
	sum.Proposed = len(sum.ProposedIDs)
	if len(anomalies) > 0 {
		sum.Anomalies = append([]string(nil), anomalies...)
		sort.Strings(sum.Anomalies)
	}
	return sum
}
