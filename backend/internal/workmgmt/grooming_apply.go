package workmgmt

// This file carries the grooming APPLY layer (E54.5 / #2237): the pure
// derivation from an approved plan.GroomingReport into a typed, entry-id-joined
// mutation list, and the continue-and-report executor that dispatches each
// mutation through the GroomingMutator capability and audits every one.
//
// TWO LAYERS, deliberately split. deriveGroomingMutations is PURE — no I/O, no
// provider, no clock — so the report -> mutation mapping is unit-testable on
// its own and a mapping regression fails on an assertion rather than on a
// forge fixture. EVERY side effect goes through the capability interface, so
// the containment rules below are enforced in ONE place instead of once per
// provider.
//
// It is named ApplyGrooming / GroomingApplyRequest rather than Apply /
// ApplyRequest because apply.go already owns the pure FILING Apply +
// FilingRequest; the two are different operations and must not share a name.
//
// CONTAINMENT is the point. Seven rules stand between an agent's proposal and
// a tracker write, and each is pinned by a named test that goes RED when the
// rule is deleted:
//
//	1. JOIN            a decision naming an entry that is not in the report
//	                   REFUSES the whole apply (*GroomingJoinError), before any
//	                   dispatch. AC1.
//	2. NOT APPROVED    a rejected/amended/undecided entry is recorded skipped
//	                   and never dispatched. AC2.
//	3. REPORT MODE     a report-mode class SURFACES its proposal and acts on
//	                   nothing — checked BEFORE any gate-approval check, so
//	                   report mode beats an explicit gate approval (#2237
//	                   approval condition I1). It never parks: ApplyGrooming
//	                   has no awaiting-decision return path at all.
//	4. DESTRUCTIVE     a close/icebox dispatches only under mode auto + an
//	                   approved entry, or an explicit per-entry gate approval.
//	                   AC5.
//	5. IDEMPOTENCE     every observable candidate is diffed against current
//	                   state read through the WorkItemReader capability BEFORE
//	                   dispatch, so re-applying an applied report dispatches
//	                   nothing. AC7.
//	6. MANUAL PLACEMENT a board move whose card sits outside the expected
//	                   source set is left alone — the same never-fight-the-human
//	                   courtesy Transitioner honors, and icebox is routed
//	                   through it (approval condition I2). AC6.
//	7. AUDIT           every candidate — applied, failed AND skipped — produces
//	                   an audit record, and a sink error surfaces AFTER the loop
//	                   so nothing is silently unaudited. AC3.
//
// NOTHING IN PRODUCTION CALLS THIS YET. #2237 reserves the apply seam; no HTTP
// route, MCP tool, CLI verb, env var, config field or migration calls
// ApplyGrooming.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// Audit categories written by the apply layer (AC3). They are declared here —
// in non-test Go source, with "Category" in the constant name — because
// audit/categories_completeness_test.go AST-collects exactly that shape and
// fails the build when an emitted category is absent from
// audit.KnownCategories.
const (
	// GroomingMutationAppliedCategory is written once per SETTLED candidate —
	// applied, failed AND skipped alike. The name reflects the family, not the
	// outcome; the outcome is a payload field, so one category filter returns
	// the whole apply.
	GroomingMutationAppliedCategory = "grooming_mutation_applied"
	// GroomingApplyCompletedCategory is written once per apply, carrying the
	// applied/failed/skipped counts and entry ids.
	GroomingApplyCompletedCategory = "grooming_apply_completed"
)

// GroomingVerdict is the operator's per-entry decision.
type GroomingVerdict string

// The three verdicts. Only GroomingApproved authorizes a dispatch; amended is
// conservatively treated as NOT applied-as-proposed, which is the safe
// direction (AC2 requires only that an amended entry is not applied as
// proposed).
const (
	GroomingApproved GroomingVerdict = "approved"
	GroomingRejected GroomingVerdict = "rejected"
	GroomingAmended  GroomingVerdict = "amended"
)

// GroomingDecision is one operator decision on one report entry, keyed by the
// entry's own derived id (plan.GroomingEntryID semantics — the id is derived
// from the report, never minted per run).
//
// CloseTarget names WHICH item of a duplicate pair is the one to close. A
// duplicate candidate is an UNORDERED relation — the report states that two
// items overlap, with no direction — so the item to close is NOT derivable
// from the report, and inventing one would risk closing the wrong issue. The
// operator supplies it here as the provider-native item id (matching one of
// the pair's item_ref ids); an approved duplicate with no CloseTarget is
// recorded skipped rather than guessed. Ignored for every other class.
type GroomingDecision struct {
	EntryID     string          `json:"entry_id"`
	Verdict     GroomingVerdict `json:"verdict"`
	CloseTarget string          `json:"close_target,omitempty"`
}

// GroomingApplyRequest is the resolved input to ApplyGrooming.
//
// Report is the schema-validated grooming report whose entries are the ONLY
// source of mutations. Decisions are the operator's per-entry verdicts. Modes
// is the resolved action-class matrix (the spec.ResolvedMatrix projection,
// mapped to GroomingMode by the caller); an ABSENT class resolves to gated,
// never auto. GateApproved carries an explicit per-entry gate approval, which
// authorizes a destructive kind under a gated class — but NEVER under report
// mode. States is the Conventions.States canonical -> provider-option map.
//
// IceboxColumn is the provider board option an icebox move targets. It is
// carried explicitly because work-management-v0's `states` map declares no
// icebox canonical state, so there is nothing to resolve it from; an icebox
// candidate with no column configured is recorded SKIPPED with
// GroomingSkipIceboxColumnUnavailable — an explicit, audited refusal, never a
// silent no-op and never a misroute to some other column (approval condition
// I5).
type GroomingApplyRequest struct {
	Target       Target
	Report       *plan.GroomingReport
	Decisions    []GroomingDecision
	Modes        map[string]GroomingMode
	GateApproved map[string]bool
	States       map[string]string
	IceboxColumn string
}

// GroomingOutcome is what happened to one candidate.
type GroomingOutcome string

// The three outcomes. Every settled candidate carries exactly one.
const (
	GroomingOutcomeApplied GroomingOutcome = "applied"
	GroomingOutcomeFailed  GroomingOutcome = "failed"
	GroomingOutcomeSkipped GroomingOutcome = "skipped"
)

// The closed set of skip reasons. Every deliberate no-op names one, so an
// audit reader can tell a containment refusal from a provider failure.
const (
	// GroomingSkipFindingOnly marks a vision-drift flag: a FINDING, which
	// derives no mutation at all.
	GroomingSkipFindingOnly = "finding_only"
	// GroomingSkipUnmappableDefect marks a hygiene defect with no mechanical
	// mutation — an unknown defect value, or a known one whose fix is authored
	// prose (absent_done_means). Recorded rather than guessed.
	GroomingSkipUnmappableDefect = "unmappable_defect"
	// GroomingSkipNoSuggestedFix marks a hygiene defect whose suggested_fix is
	// absent, so the value to write is unknown. Never guessed.
	GroomingSkipNoSuggestedFix = "no_suggested_fix"
	// GroomingSkipNotApproved marks an entry the operator REJECTED.
	GroomingSkipNotApproved = "not_approved"
	// GroomingSkipAmended marks an entry the operator AMENDED — not applied as
	// proposed.
	GroomingSkipAmended = "amended"
	// GroomingSkipNoDecision marks an entry with no operator decision at all.
	GroomingSkipNoDecision = "no_decision"
	// GroomingSkipReportMode marks a report-mode class: the proposal is
	// surfaced (before/after are populated on the record) and nothing acts.
	GroomingSkipReportMode = "mode_report_surface_only"
	// GroomingSkipDestructiveNotAuthorized marks a destructive kind that
	// reached neither mode auto + approval nor an explicit gate approval.
	GroomingSkipDestructiveNotAuthorized = "destructive_not_authorized"
	// GroomingSkipAlreadyApplied marks a candidate whose proposed state is
	// already the observed state.
	GroomingSkipAlreadyApplied = "already_applied"
	// GroomingSkipManualPlacementPreserved marks a board move whose card sits
	// outside the expected source set — a human put it there.
	GroomingSkipManualPlacementPreserved = "manual_placement_preserved"
	// GroomingSkipIceboxColumnUnavailable marks an icebox candidate with no
	// icebox column configured (approval condition I5).
	GroomingSkipIceboxColumnUnavailable = "icebox_column_unavailable"
	// GroomingSkipDuplicateTargetUnspecified marks an approved duplicate whose
	// decision named no CloseTarget — the pair is unordered, so which item to
	// close is not derivable.
	GroomingSkipDuplicateTargetUnspecified = "duplicate_target_unspecified"
	// GroomingSkipDuplicateTargetNotInPair marks a CloseTarget that is not one
	// of the pair's two items.
	GroomingSkipDuplicateTargetNotInPair = "duplicate_target_not_in_pair"
	// GroomingSkipItemOutsideTarget marks an item_ref whose repository
	// coordinates are not the apply's Target repo.
	GroomingSkipItemOutsideTarget = "item_outside_target_repo"
	// GroomingSkipItemRefUnresolvable marks an item_ref this layer cannot
	// resolve to a provider-native issue reference.
	GroomingSkipItemRefUnresolvable = "item_ref_unresolvable"
)

// GroomingMutationRecord is the per-candidate audit row. It carries the join
// key (EntryID), what was proposed (Kind/Before/After), and what happened
// (Outcome + SkipReason or Error + ProviderResponse).
//
// IdempotenceChecked reports whether the pre-dispatch diff actually settled
// this candidate's current state. It is false for the three value-set kinds
// whose current value WorkItemRecord cannot expose (see
// GroomingMutationKind.IdempotenceObservable), so the residual is visible in
// the audit row rather than only in a doc comment.
type GroomingMutationRecord struct {
	EntryID            string               `json:"entry_id"`
	Class              string               `json:"class"`
	ReportClass        string               `json:"report_class"`
	Kind               GroomingMutationKind `json:"kind"`
	ItemRef            string               `json:"item_ref,omitempty"`
	Before             GroomingValue        `json:"before,omitempty"`
	After              GroomingValue        `json:"after,omitempty"`
	Outcome            GroomingOutcome      `json:"outcome"`
	SkipReason         string               `json:"skip_reason,omitempty"`
	Error              string               `json:"error,omitempty"`
	ProviderResponse   string               `json:"provider_response,omitempty"`
	IdempotenceChecked bool                 `json:"idempotence_checked"`
}

// GroomingApplySummary is the once-per-apply audit payload: the counts and the
// entry ids per outcome, plus any audit-sink errors collected during the loop.
type GroomingApplySummary struct {
	Applied     int      `json:"applied"`
	Failed      int      `json:"failed"`
	Skipped     int      `json:"skipped"`
	AppliedIDs  []string `json:"applied_ids,omitempty"`
	FailedIDs   []string `json:"failed_ids,omitempty"`
	SkippedIDs  []string `json:"skipped_ids,omitempty"`
	AuditErrors []string `json:"audit_errors,omitempty"`
}

// GroomingApplyResult is what ApplyGrooming returns: every settled candidate,
// bucketed by outcome, plus the summary.
type GroomingApplyResult struct {
	Applied []GroomingMutationRecord `json:"applied,omitempty"`
	Failed  []GroomingMutationRecord `json:"failed,omitempty"`
	Skipped []GroomingMutationRecord `json:"skipped,omitempty"`
	Summary GroomingApplySummary     `json:"summary"`
}

// GroomingAuditSink records the apply's audit trail. Both methods are called
// with a NON-BEST-EFFORT contract: a per-mutation call happens immediately
// after each candidate settles (so a mid-run failure still leaves everything
// before it audited), and an error from either is collected and surfaced as a
// *GroomingAuditError after the loop rather than aborting it.
type GroomingAuditSink interface {
	RecordGroomingMutation(ctx context.Context, rec GroomingMutationRecord) error
	RecordGroomingApplyCompleted(ctx context.Context, sum GroomingApplySummary) error
}

// GroomingJoinError is the fail-CLOSED refusal of an ENTIRE apply (AC1): a
// decision names an entry id that is not in the report, or a derived candidate
// carries an id the report does not contain. AC1 calls an unjoined mutation a
// bug, not a convenience, so nothing is dispatched — this is not a
// per-candidate skip.
type GroomingJoinError struct {
	EntryID string
	Reason  string
}

func (e *GroomingJoinError) Error() string {
	return fmt.Sprintf("workmgmt: grooming apply refused: entry %q %s", e.EntryID, e.Reason)
}

// GroomingAuditError reports that one or more audit writes failed. It is
// returned AFTER the loop completes, so continue-and-report still holds while
// nothing is silently unaudited.
type GroomingAuditError struct {
	Errors []string
}

func (e *GroomingAuditError) Error() string {
	return fmt.Sprintf("workmgmt: grooming apply completed but %d audit write(s) failed: %s",
		len(e.Errors), strings.Join(e.Errors, "; "))
}

// groomingCandidate is one derived mutation proposal before any containment
// rule has run. skipReason non-empty marks a DERIVATION-time skip (a finding,
// an unmappable defect, a missing suggested fix): the candidate is recorded
// and never dispatched.
type groomingCandidate struct {
	entryID      string
	class        string
	reportClass  string
	kind         GroomingMutationKind
	ref          plan.ItemRef
	pair         []plan.ItemRef
	after        GroomingValue
	expectedFrom []string
	skipReason   string
}

// groomingClassOrder fixes the iteration order so an apply's dispatch sequence
// (and therefore its audit trail) is deterministic rather than dependent on
// report field order. Within a class, candidates are ordered by entry id.
var groomingClassOrder = map[string]int{
	plan.GroomingClassHygiene:       0,
	plan.GroomingClassDependency:    1,
	plan.GroomingClassOrdering:      2,
	plan.GroomingClassDuplicate:     3,
	plan.GroomingClassDecomposition: 4,
	plan.GroomingClassVisionDrift:   5,
}

// groomingActionClass maps a REPORT entry class onto the ACTION class the
// resolved autonomy matrix keys on (spec's hygiene/ordering/dedup/scoping —
// docs/spec/workflow-v2.md § backlog-grooming action classes). Dependency
// edges are hygiene: adding an absent link is the objective, reversible fix
// that class is defined by. Vision drift is scoping for record-keeping only —
// it derives no mutation, so no mode is ever consulted for it.
var groomingActionClass = map[string]string{
	plan.GroomingClassHygiene:       "hygiene",
	plan.GroomingClassDependency:    "hygiene",
	plan.GroomingClassOrdering:      "ordering",
	plan.GroomingClassDuplicate:     "dedup",
	plan.GroomingClassDecomposition: "scoping",
	plan.GroomingClassVisionDrift:   "scoping",
}

// groomingHygieneKinds maps each hygiene defect in grooming-report-v1's closed
// enum onto its mutation kind. A defect ABSENT from this map derives no
// mutation and is recorded GroomingSkipUnmappableDefect — including
// absent_done_means, whose fix is authored prose no mutation kind expresses.
// Guessing a kind for an unrecognized defect is the silent-wrong-answer shape
// this layer exists to prevent.
var groomingHygieneKinds = map[string]GroomingMutationKind{
	"missing_label_namespace":  GroomingKindLabelSet,
	"unlinked_parent_epic":     GroomingKindEpicLink,
	"missing_parent_epic_link": GroomingKindEpicLink,
	"unboarded":                GroomingKindBoardPlace,
	"missing_estimate":         GroomingKindFieldSet,
}

// groomingIceboxExpectedFrom is the never-fight-the-human source set for an
// icebox move: only an item nobody has started may be parked. A card a human
// moved to in_progress / in_review / blocked / done is left alone.
var groomingIceboxExpectedFrom = []string{CanonicalStateBacklog, CanonicalStateUpNext}

// deriveGroomingMutations is the PURE report -> candidate mapping. One
// candidate per report entry, keyed by that entry's OWN id. No I/O, no
// provider, no request-dependent resolution (item refs, icebox columns and
// duplicate close targets are resolved by the executor, so a skip on one of
// those never masks a higher-precedence containment refusal).
func deriveGroomingMutations(report *plan.GroomingReport) []groomingCandidate {
	if report == nil {
		return nil
	}
	var out []groomingCandidate

	for _, e := range report.HygieneDefects {
		c := groomingCandidate{
			entryID:     e.ID,
			reportClass: plan.GroomingClassHygiene,
			class:       groomingActionClass[plan.GroomingClassHygiene],
			ref:         e.ItemRef,
		}
		kind, ok := groomingHygieneKinds[e.Defect]
		switch {
		case !ok:
			c.skipReason = GroomingSkipUnmappableDefect
		case strings.TrimSpace(e.SuggestedFix) == "":
			// The value to write is unknown. A hygiene fix with no proposed
			// value is recorded, never guessed.
			c.kind = kind
			c.skipReason = GroomingSkipNoSuggestedFix
		default:
			c.kind = kind
			fix := strings.TrimSpace(e.SuggestedFix)
			if kind == GroomingKindLabelSet {
				c.after = GroomingValue{List: []string{fix}}
			} else {
				c.after = GroomingValue{Scalar: fix}
			}
		}
		out = append(out, c)
	}

	for _, e := range report.DependencyEdges {
		out = append(out, groomingCandidate{
			entryID:     e.ID,
			reportClass: plan.GroomingClassDependency,
			class:       groomingActionClass[plan.GroomingClassDependency],
			kind:        GroomingKindDependsOnAdd,
			ref:         e.From,
			pair:        []plan.ItemRef{e.From, e.To},
		})
	}

	for _, e := range report.Ordering {
		out = append(out, groomingCandidate{
			entryID:     e.ID,
			reportClass: plan.GroomingClassOrdering,
			class:       groomingActionClass[plan.GroomingClassOrdering],
			kind:        GroomingKindRankSet,
			ref:         e.ItemRef,
			after:       GroomingValue{Scalar: strconv.Itoa(e.Rank)},
		})
	}

	for _, e := range report.Duplicates {
		c := groomingCandidate{
			entryID:     e.ID,
			reportClass: plan.GroomingClassDuplicate,
			class:       groomingActionClass[plan.GroomingClassDuplicate],
			kind:        GroomingKindCloseDuplicate,
			pair:        append([]plan.ItemRef(nil), e.Pair...),
			after:       GroomingValue{Scalar: "closed"},
		}
		if len(e.Pair) > 0 {
			c.ref = e.Pair[0]
		}
		out = append(out, c)
	}

	for _, e := range report.DecompositionSuggestions {
		out = append(out, groomingCandidate{
			entryID:      e.ID,
			reportClass:  plan.GroomingClassDecomposition,
			class:        groomingActionClass[plan.GroomingClassDecomposition],
			kind:         GroomingKindIcebox,
			ref:          e.ItemRef,
			expectedFrom: groomingIceboxExpectedFrom,
		})
	}

	for _, e := range report.VisionDrift {
		out = append(out, groomingCandidate{
			entryID:     e.ID,
			reportClass: plan.GroomingClassVisionDrift,
			class:       groomingActionClass[plan.GroomingClassVisionDrift],
			ref:         e.ItemRef,
			skipReason:  GroomingSkipFindingOnly,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if groomingClassOrder[out[i].reportClass] != groomingClassOrder[out[j].reportClass] {
			return groomingClassOrder[out[i].reportClass] < groomingClassOrder[out[j].reportClass]
		}
		return out[i].entryID < out[j].entryID
	})
	return out
}

// groomingReportEntryIDs collects every entry id the report carries — the
// authority the join check validates decisions against.
func groomingReportEntryIDs(report *plan.GroomingReport) map[string]struct{} {
	ids := map[string]struct{}{}
	if report == nil {
		return ids
	}
	for _, e := range report.Ordering {
		ids[e.ID] = struct{}{}
	}
	for _, e := range report.Duplicates {
		ids[e.ID] = struct{}{}
	}
	for _, e := range report.HygieneDefects {
		ids[e.ID] = struct{}{}
	}
	for _, e := range report.DependencyEdges {
		ids[e.ID] = struct{}{}
	}
	for _, e := range report.VisionDrift {
		ids[e.ID] = struct{}{}
	}
	for _, e := range report.DecompositionSuggestions {
		ids[e.ID] = struct{}{}
	}
	return ids
}

// validateGroomingJoin is containment rule 1 (AC1). It refuses the WHOLE apply
// — before any dispatch — when a decision names an entry id absent from the
// report, or when a derived candidate carries an id the report does not
// contain (a derivation bug). Fail-closed by design: an unjoined mutation is a
// bug, not a convenience, so there is no partial-execution path.
func validateGroomingJoin(report *plan.GroomingReport, decisions []GroomingDecision, candidates []groomingCandidate) error {
	ids := groomingReportEntryIDs(report)
	for _, d := range decisions {
		if _, ok := ids[d.EntryID]; !ok {
			return &GroomingJoinError{EntryID: d.EntryID, Reason: "has a decision but is not in the grooming report"}
		}
	}
	for _, c := range candidates {
		if _, ok := ids[c.entryID]; !ok {
			return &GroomingJoinError{EntryID: c.entryID, Reason: "derived a mutation but is not in the grooming report"}
		}
	}
	return nil
}

// ApplyGrooming executes the approved mutations of one grooming report.
//
// It is CONTINUE-AND-REPORT (AC4, the decision recorded on #2232): a provider
// failure on one candidate is recorded and the loop moves to the next — there
// is no abort path — so a partial apply is fully reported rather than
// half-executed and unexplained. It NEVER parks: there is no awaiting-decision
// return, so a report-mode class surfaces and the call returns.
//
// mutator and sink are required. reader is required whenever any dispatchable
// candidate needs a pre-dispatch read; a nil reader fails those candidates
// CLOSED rather than dispatching blind.
//
// The returned error is a *GroomingJoinError (nothing ran), an argument error
// (nothing ran), or a *GroomingAuditError (everything ran, some audit writes
// failed). The result is non-nil in the audit-error case.
func ApplyGrooming(ctx context.Context, mutator GroomingMutator, reader WorkItemReader, sink GroomingAuditSink, req GroomingApplyRequest) (*GroomingApplyResult, error) {
	if req.Report == nil {
		return nil, errors.New("workmgmt: grooming apply requires a report")
	}
	if mutator == nil {
		return nil, errors.New("workmgmt: grooming apply requires a grooming mutator")
	}
	if sink == nil {
		// AC3 requires every mutation be audited; an apply with nowhere to
		// record is refused rather than run unaudited.
		return nil, errors.New("workmgmt: grooming apply requires an audit sink")
	}

	candidates := deriveGroomingMutations(req.Report)
	if err := validateGroomingJoin(req.Report, req.Decisions, candidates); err != nil {
		return nil, err
	}

	decisions := make(map[string]GroomingDecision, len(req.Decisions))
	for _, d := range req.Decisions {
		decisions[d.EntryID] = d
	}

	result := &GroomingApplyResult{}
	var auditErrs []string

	for _, c := range candidates {
		rec := settleGroomingCandidate(ctx, c, req, decisions, mutator, reader)
		switch rec.Outcome {
		case GroomingOutcomeApplied:
			result.Applied = append(result.Applied, rec)
			result.Summary.AppliedIDs = append(result.Summary.AppliedIDs, rec.EntryID)
		case GroomingOutcomeFailed:
			result.Failed = append(result.Failed, rec)
			result.Summary.FailedIDs = append(result.Summary.FailedIDs, rec.EntryID)
		default:
			result.Skipped = append(result.Skipped, rec)
			result.Summary.SkippedIDs = append(result.Summary.SkippedIDs, rec.EntryID)
		}
		// Audit immediately, so a later failure still leaves everything before
		// it audited. A sink error is collected, never an abort.
		if err := sink.RecordGroomingMutation(ctx, rec); err != nil {
			auditErrs = append(auditErrs, fmt.Sprintf("%s: %v", rec.EntryID, err))
		}
	}

	result.Summary.Applied = len(result.Applied)
	result.Summary.Failed = len(result.Failed)
	result.Summary.Skipped = len(result.Skipped)
	result.Summary.AuditErrors = auditErrs

	if err := sink.RecordGroomingApplyCompleted(ctx, result.Summary); err != nil {
		auditErrs = append(auditErrs, fmt.Sprintf("apply summary: %v", err))
		result.Summary.AuditErrors = auditErrs
	}
	if len(auditErrs) > 0 {
		return result, &GroomingAuditError{Errors: auditErrs}
	}
	return result, nil
}

// settleGroomingCandidate runs the containment ladder for ONE candidate and
// returns its record. The ORDER of the ladder is load-bearing and is the
// subject of approval condition I1: report mode short-circuits BEFORE any
// gate-approval check, so a report-mode class cannot be mutated by an explicit
// gate approval.
func settleGroomingCandidate(ctx context.Context, c groomingCandidate, req GroomingApplyRequest,
	decisions map[string]GroomingDecision, mutator GroomingMutator, reader WorkItemReader) GroomingMutationRecord {
	rec := GroomingMutationRecord{
		EntryID:     c.entryID,
		Class:       c.class,
		ReportClass: c.reportClass,
		Kind:        c.kind,
		After:       c.after,
	}

	// Rule 0: a derivation-time skip (a finding, an unmappable defect, a
	// missing suggested fix) never reaches a decision or a provider.
	if c.skipReason != "" {
		return groomingSkipped(rec, c.skipReason)
	}

	// Rule 2 (AC2): only an approved entry may act. (Rule 1, the join, is not
	// per-candidate — it refused the whole apply before this loop began.)
	d, ok := decisions[c.entryID]
	switch {
	case !ok:
		return groomingSkipped(rec, GroomingSkipNoDecision)
	case d.Verdict == GroomingRejected:
		return groomingSkipped(rec, GroomingSkipNotApproved)
	case d.Verdict == GroomingAmended:
		return groomingSkipped(rec, GroomingSkipAmended)
	case d.Verdict != GroomingApproved:
		// An unrecognized verdict is NOT an approval — fail closed.
		return groomingSkipped(rec, GroomingSkipNotApproved)
	}

	// Request-dependent resolution: the item reference, the duplicate pair's
	// close target, the icebox column. It is COMPUTED here — so a surfaced
	// proposal shows the value that would have been written — but its skip is
	// not RETURNED until after the containment ladder below, so a resolution
	// skip can never mask a higher-precedence refusal.
	mreq, reason := resolveGroomingRequest(c, d, req)
	if reason == "" {
		rec.ItemRef = mreq.ItemRef
		rec.After = mreq.After
	}

	// Rule 3 (approval condition I1): report mode surfaces the proposal and
	// acts on NOTHING. It is checked BEFORE the destructive gate's
	// gate-approval arm, because the two rules disagree exactly there: a
	// gate-approved entry in a report-mode class must NOT mutate. Before/After
	// stay on the record so the proposal is still visible.
	mode := ResolveGroomingMode(req.Modes[c.class])
	if mode == GroomingModeReport {
		return groomingSkipped(rec, GroomingSkipReportMode)
	}

	// Rule 4 (AC5): a destructive kind needs mode auto on an approved entry,
	// or an explicit per-entry gate approval.
	if c.kind.Destructive() && mode != GroomingModeAuto && !req.GateApproved[c.entryID] {
		return groomingSkipped(rec, GroomingSkipDestructiveNotAuthorized)
	}

	if reason != "" {
		return groomingSkipped(rec, reason)
	}

	// Rules 5 + 6 (AC7 + AC6): the pre-dispatch read.
	if c.kind.IdempotenceObservable() || c.kind.BoardPlacement() {
		observed, item, err := readGroomingObserved(ctx, reader, c, mreq.ItemRef, req)
		if err != nil {
			// A reader degradation is NOT swallowed: dispatching blind is
			// exactly the duplicate mutation AC7 forbids.
			rec.Outcome = GroomingOutcomeFailed
			rec.Error = err.Error()
			return rec
		}
		rec.Before = observed
		rec.IdempotenceChecked = true
		mreq.Before = observed
		if groomingSatisfied(c.kind, observed, mreq.After) {
			return groomingSkipped(rec, GroomingSkipAlreadyApplied)
		}
		if c.kind.BoardPlacement() && !groomingPlacementAllowed(c.expectedFrom, item, req.States) {
			return groomingSkipped(rec, GroomingSkipManualPlacementPreserved)
		}
	}

	res, err := mutator.ApplyGroomingMutation(ctx, mreq)
	switch {
	case err != nil:
		rec.Outcome = GroomingOutcomeFailed
		rec.Error = err.Error()
	case res == nil:
		// A provider that returns neither a result nor an error told us
		// nothing; recording it applied would be a fabrication.
		rec.Outcome = GroomingOutcomeFailed
		rec.Error = "provider returned no result and no error"
	case res.Skipped:
		rec.ProviderResponse = res.ProviderResponse
		return groomingSkipped(rec, res.SkipReason)
	default:
		rec.Outcome = GroomingOutcomeApplied
		rec.ProviderResponse = res.ProviderResponse
	}
	return rec
}

// resolve turns a derived candidate into the provider request, doing the
// request-dependent work: resolving the item reference against the target
// repo, picking the duplicate pair's close target from the operator's
// decision, and resolving the icebox column. It returns a skip reason instead
// of a request when any of those cannot be resolved — never a guess.
func resolveGroomingRequest(c groomingCandidate, d GroomingDecision, req GroomingApplyRequest) (GroomingMutationRequest, string) {
	target := c.ref
	if c.kind == GroomingKindCloseDuplicate {
		chosen, reason := groomingCloseTarget(c.pair, d.CloseTarget)
		if reason != "" {
			return GroomingMutationRequest{}, reason
		}
		target = chosen
	}

	ref, reason := groomingResolveItemRef(target, req.Target)
	if reason != "" {
		return GroomingMutationRequest{}, reason
	}

	after := c.after
	switch c.kind {
	case GroomingKindDependsOnAdd:
		if len(c.pair) != 2 {
			return GroomingMutationRequest{}, GroomingSkipItemRefUnresolvable
		}
		toRef, r := groomingResolveItemRef(c.pair[1], req.Target)
		if r != "" {
			return GroomingMutationRequest{}, r
		}
		after = GroomingValue{Scalar: toRef}
	case GroomingKindIcebox:
		col := strings.TrimSpace(req.IceboxColumn)
		if col == "" {
			// Approval condition I5: an explicit, audited refusal — never a
			// silent no-op and never a misroute to another column.
			return GroomingMutationRequest{}, GroomingSkipIceboxColumnUnavailable
		}
		after = GroomingValue{Scalar: col}
	}

	return GroomingMutationRequest{
		Target:       req.Target,
		EntryID:      c.entryID,
		Class:        c.class,
		Kind:         c.kind,
		ItemRef:      ref,
		After:        after,
		ExpectedFrom: c.expectedFrom,
		States:       req.States,
	}, ""
}

// readGroomingObserved performs the pre-dispatch read through the
// WorkItemReader capability and projects the record onto the observable value
// for this kind. It reads the ALREADY-RESOLVED item ref (the duplicate pair's
// chosen close target, not the pair's first member), so the state the
// idempotence diff and the placement guard judge is the state of the item the
// mutation would actually touch. Board state is resolved only for
// board-placement kinds, so a label-only apply is not failed closed on a board
// it never asked about.
func readGroomingObserved(ctx context.Context, reader WorkItemReader, c groomingCandidate, ref string,
	req GroomingApplyRequest) (GroomingValue, *WorkItemRecord, error) {
	if reader == nil {
		// Typed, not a bare error: a caller switches on the reason exactly as
		// it would for a provider that cannot read.
		return GroomingValue{}, nil, &UnavailableError{
			Capability: ReaderCapability,
			Reason:     ReasonNotImplemented,
			Detail:     "grooming apply needs a pre-dispatch read to stay idempotent; no reader capability was resolved",
		}
	}
	item, err := reader.ReadWorkItem(ctx, ReadWorkItemRequest{
		Target:            req.Target,
		Ref:               ref,
		ResolveBoardState: c.kind.BoardPlacement(),
		States:            req.States,
	})
	if err != nil {
		return GroomingValue{}, nil, err
	}
	if item == nil {
		return GroomingValue{}, nil, fmt.Errorf("workmgmt: grooming apply read %s returned no record", ref)
	}
	return groomingObserved(c.kind, item), item, nil
}

// groomingObserved projects a read work item onto the value this kind mutates.
// A kind whose current value the read capability cannot expose (the value-set
// kinds) yields the zero value, which never equals a proposed value, so the
// candidate dispatches — see GroomingMutationKind.IdempotenceObservable.
func groomingObserved(kind GroomingMutationKind, item *WorkItemRecord) GroomingValue {
	switch kind {
	case GroomingKindLabelSet:
		return GroomingValue{List: append([]string(nil), item.Labels...)}
	case GroomingKindBoardPlace, GroomingKindIcebox:
		return GroomingValue{Scalar: item.BoardColumn}
	case GroomingKindEpicLink:
		return GroomingValue{Scalar: groomingBodyMarkerValue(item.Body, "Parent epic:")}
	case GroomingKindDependsOnAdd:
		return GroomingValue{Scalar: groomingBodyMarkerValue(item.Body, "Depends on:")}
	case GroomingKindCloseDuplicate, GroomingKindCloseNotPlanned:
		if strings.EqualFold(item.State, "closed") {
			return GroomingValue{Scalar: "closed"}
		}
		return GroomingValue{Scalar: item.State}
	default:
		return GroomingValue{}
	}
}

// groomingSatisfied reports whether the proposed value is ALREADY the observed
// value — the idempotence diff (AC7). A label mutation is ADDITIVE, so it is
// satisfied when the observed set already carries the proposed labels whatever
// else it carries; every other kind compares values.
func groomingSatisfied(kind GroomingMutationKind, observed, after GroomingValue) bool {
	if kind == GroomingKindLabelSet {
		return len(after.List) > 0 && observed.Contains(after.List)
	}
	if after.Scalar == "" && len(after.List) == 0 {
		return false
	}
	return observed.Equal(after)
}

// groomingBodyMarkerValue returns the value following the FIRST body line
// beginning with marker (e.g. "Parent epic: #1437" -> "#1437"), or "" when no
// such line exists. It is the read-side counterpart of the body-marker
// convention the providers write, so an epic link or depends_on edge that is
// already recorded is observable and never applied twice.
func groomingBodyMarkerValue(body, marker string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, marker) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
		}
	}
	return ""
}

// groomingPlacementAllowed is the never-fight-the-human courtesy (AC6),
// applied to EVERY board-placement kind including icebox (approval condition
// I2).
//
// With an EMPTY expected-source set the mutation is a placement of an
// OFF-BOARD item: it proceeds only while the item is genuinely off-board, so a
// card a human has already boarded is left where they put it. With a non-empty
// set the item's observed canonical state must be in it.
func groomingPlacementAllowed(expectedFrom []string, item *WorkItemRecord, states map[string]string) bool {
	if item == nil {
		return false
	}
	if len(expectedFrom) == 0 {
		return !item.OnBoard
	}
	if !item.OnBoard {
		return false
	}
	current := CanonicalStateForOption(states, item.BoardColumn)
	for _, s := range expectedFrom {
		if s == current {
			return true
		}
	}
	return false
}

// groomingCloseTarget resolves WHICH member of an unordered duplicate pair the
// operator chose to close. It never guesses: an absent choice and a choice
// outside the pair each yield a skip reason.
func groomingCloseTarget(pair []plan.ItemRef, chosen string) (plan.ItemRef, string) {
	if strings.TrimSpace(chosen) == "" {
		return plan.ItemRef{}, GroomingSkipDuplicateTargetUnspecified
	}
	for _, ref := range pair {
		if strings.EqualFold(strings.TrimSpace(ref.ID), strings.TrimSpace(chosen)) {
			return ref, ""
		}
	}
	return plan.ItemRef{}, GroomingSkipDuplicateTargetNotInPair
}

// groomingResolveItemRef maps a report item_ref onto the provider-native issue
// reference ("#2237") for the apply's target repo. An item belonging to a
// DIFFERENT repository is refused rather than applied against the wrong repo,
// and an id this layer cannot parse is refused rather than passed through.
func groomingResolveItemRef(ref plan.ItemRef, target Target) (string, string) {
	id := strings.TrimSpace(ref.ID)
	idx := strings.LastIndex(id, "#")
	if idx <= 0 || idx == len(id)-1 {
		return "", GroomingSkipItemRefUnresolvable
	}
	repo, number := id[:idx], id[idx+1:]
	if _, err := strconv.Atoi(number); err != nil {
		return "", GroomingSkipItemRefUnresolvable
	}
	want := target.Repo.Owner + "/" + target.Repo.Name
	if target.Repo.Owner == "" || target.Repo.Name == "" || !strings.EqualFold(repo, want) {
		return "", GroomingSkipItemOutsideTarget
	}
	return "#" + number, ""
}

// groomingSkipped stamps a record as a deliberate, audited no-op.
func groomingSkipped(rec GroomingMutationRecord, reason string) GroomingMutationRecord {
	rec.Outcome = GroomingOutcomeSkipped
	rec.SkipReason = reason
	return rec
}
