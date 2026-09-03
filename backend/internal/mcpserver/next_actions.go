package mcpserver

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/failuresig"
)

// next_actions (#1024) generalizes the review_action_hint pattern
// (#777/#860/#964) across the whole run lifecycle: for every non-terminal
// run state, fishhawk_get_run_status and fishhawk_run_stage surface at
// least one LEGAL next action as a structured entry — the tool to call
// (with key params), its precondition, what it consumes, and a one-line
// reason. The classifier is a pure function over data the tools already
// fetch (run row, stage rows, review statuses, the computed
// review_action_hint, and the drive read view) — no backend endpoint or
// schema is involved. DISPLAY-ONLY, never gates a run: like the
// periodic-budget block and the hint it generalizes, it is advisory.
//
// Structural invariant (the "done means" condition): nextActionsFor NEVER
// returns an empty actions list for a non-terminal run. Any state the
// table does not match falls through to the labeled "unclassified"
// fallback (re-poll + file a product issue naming the state), and a final
// guard enforces the invariant even if a future arm regresses — the
// invariant is structural, not fixture-dependent.

// Consumes values for SuggestedAction: what taking the action spends.
const (
	consumesNone         = "none"
	consumesFixupBudget  = "fixup_budget"
	consumesRetryBudget  = "retry_budget"
	consumesApprovalSlot = "approval_slot"
	consumesNewRun       = "new_run"
)

// flakeTraceEvents are the known infra-flake trace-event names a
// category-A failure detail may cite (best-effort string inspection —
// no backend contract). When one appears in the implement stage's
// failure reason, the retry_stage action's reason names it so the
// operator knows a retry is the cheapest next step.
//
// SOURCED FROM the failure-signature registry (#1703), not declared here: the
// registry is the ONE place the backend module declares these literals, so the
// signature hint and the surrounding action can never disagree about what a
// failure is. A local re-declaration would drift silently — the hint stops
// matching while the action still fires, which reads as "no signature
// matched". TestFailureSignatureAnchorsMatchNextActionsPhrases pins it.
var flakeTraceEvents = []string{failuresig.AnchorVerifyInfraFlake}

// SuggestedAction is one legal next move for the run's current state.
// Action is a tool name (fishhawk_resume_run, fishhawk_merge_run) or a
// named ritual step (approve_pr, post_merge) when the move happens outside
// the MCP surface. The merge itself is the fishhawk_merge_run tool (E48.7 /
// #1954), which replaced the bare merge_pr + post_merge ritual steps; product
// filing is likewise the fishhawk_report_product_issue tool (E32.11 / #1737),
// which replaced the bare file_product_issue ritual step.
type SuggestedAction struct {
	Action       string            `json:"action" jsonschema:"the tool to call (e.g. fishhawk_resume_run, fishhawk_merge_run, fishhawk_fixup_stage, fishhawk_report_product_issue) or a named ritual step outside the MCP surface (approve_pr, post_merge, merge_and_file_follow_up)"`
	Params       map[string]string `json:"params,omitempty" jsonschema:"key parameters for the action (run_id, stage_id, parent_run_id, the concern_ids source, ...); values naming a field path (e.g. run.concerns.items[].id) tell you where to read the real value"`
	Precondition string            `json:"precondition" jsonschema:"one-line statement of when this action is legal"`
	Consumes     string            `json:"consumes" jsonschema:"what taking the action spends: one of none, fixup_budget, retry_budget, approval_slot, new_run"`
	Reason       string            `json:"reason" jsonschema:"one-line why-this-now"`
}

// NextActions is the classified run lifecycle state plus its legal next
// moves. Actions is nil ONLY on a terminal SUCCESS-shaped state (the block
// still names the state); every non-terminal state carries at least one
// action. A terminal FAILED/CANCELLED run is no longer actionless: it carries
// the operator-gated fishhawk_report_product_issue filing suggestion (E32.11 /
// #1737).
type NextActions struct {
	State   string            `json:"state" jsonschema:"the classified run lifecycle state, e.g. plan_gate_parked, plan_awaiting_input, implement_failed_category_b, succeeded_pr_open, terminal states by run state name, or unclassified when no table arm matched"`
	Actions []SuggestedAction `json:"actions,omitempty" jsonschema:"the legal next moves, ordered (first is the suggested default). Nil only on a terminal SUCCESS-shaped run state; every non-terminal state carries at least one entry, and a terminal failed/cancelled run carries the operator-gated fishhawk_report_product_issue filing suggestion. Display-only — never gates the run"`
	// Signature is the failure-signature registry's match on the run's failed
	// stage (#1703): what the failure MEANS plus the recommended recovery
	// sequence. ADDITIVE and DISPLAY-ONLY — it never gates a run and never
	// alters Actions. Absent when no stage failed or when no catalog entry
	// matched (the fail-open contract), in which case every other field is
	// byte-identical to what it would be without the registry.
	Signature *failuresig.Hint `json:"signature,omitempty" jsonschema:"a display-only recovery hint for the run's failed stage: the matched failure-signature id, what the failure means, and the recommended recovery steps. Advisory — it never gates the run and never changes the actions list. Omitted when nothing matched. Catalog: docs/architecture/failure-signatures.md"`
}

// nextActionsFor classifies the run's lifecycle state and returns the
// legal next actions (#1024). Pure function over already-fetched data;
// every input except run and stages may be nil. For drive-enabled runs
// with a distilled NextAction, that action is folded in FIRST so drive
// and next_actions never point different ways.
func nextActionsFor(run *Run, stages []Stage, planReviewStatus, implementReviewStatus *ReviewStatus, hint *ReviewActionHint, drive *DriveStatus, mergeObserved, acceptanceSkippedOutOfScope, acceptanceArbitrated bool, acceptanceVerdict, acceptanceTriageDisposition string, release releaseSignals) *NextActions {
	if run == nil {
		return nil
	}
	// ci_failed (#1045) is decided ahead of the general table: it is a
	// drive-derived presentation status (a red required check on the open
	// PR), not a stage state the table arms key on. Open concerns route to
	// fix-up; zero open concerns is structurally unroutable and names the
	// operator commit+vouch remediation arm (#1044).
	if drive != nil && drive.DerivedStatus == "ci_failed" {
		na := ciFailedNextActions(run, stages, hint)
		if drive.NextAction != nil {
			na.Actions = append([]SuggestedAction{driveAction(run, drive.NextAction)}, na.Actions...)
		}
		foldLiveValidationAdvisory(run, na)
		foldProductIssueSuggestion(run, stages, na)
		foldWorkingDirParams(run, na)
		foldFailureSignature(run, stages, na)
		return na
	}

	na := classifyNextActions(run, stages, planReviewStatus, implementReviewStatus, hint, mergeObserved, acceptanceSkippedOutOfScope, acceptanceArbitrated, acceptanceVerdict, acceptanceTriageDisposition, release)

	if drive != nil && drive.NextAction != nil {
		na.Actions = append([]SuggestedAction{driveAction(run, drive.NextAction)}, na.Actions...)
	}

	// Final structural guard: a non-terminal run must never read an empty
	// actions list, even if a future table arm regresses to one. Run this
	// BEFORE the live-validation fold so the guard measures the classifier's own
	// actions — an advisory note must never mask an otherwise-empty arm that owes
	// the operator the unclassified fallback.
	if !runStateIsTerminal(run.State) && len(na.Actions) == 0 {
		fallback := unclassifiedNextActions(run, stages)
		fallback.State = na.State
		na = fallback
	}
	foldLiveValidationAdvisory(run, na)
	foldProductIssueSuggestion(run, stages, na)
	foldWorkingDirParams(run, na)
	foldFailureSignature(run, stages, na)
	return na
}

// failureSignatureFor adapts the run's failed stage into failuresig.Evidence
// and returns the registry's match, or nil (#1703).
//
// The diagnosis stage is the FIRST stage in state "failed" — a run's stages
// are ordered by sequence, so the earliest failure is the one that explains
// the run. Nil-safe throughout: a nil run, an empty stage slice, or a run with
// no failed stage all return nil, which is also the fail-open answer.
func failureSignatureFor(run *Run, stages []Stage) *failuresig.Hint {
	if run == nil {
		return nil
	}
	failed := firstFailedStage(stages)
	if failed == nil {
		return nil
	}
	ev := failuresig.Evidence{
		StageType:    failed.Type,
		StageState:   failed.State,
		RetryAttempt: run.RetryAttempt,
		RunnerKind:   run.RunnerKind,
		// The orchestrator's minted-child shape: a parent_run_id plus an
		// implement stage but NO plan or review stage of its own — the same
		// in-band signal the category-B decomposition-child arm reads.
		IsDecompositionChild: run.ParentRunID != nil && stageByType(stages, "plan") == nil && stageByType(stages, "review") == nil,
	}
	if failed.FailureCategory != nil {
		ev.FailureCategory = *failed.FailureCategory
	}
	if failed.FailureReason != nil {
		ev.FailureReason = *failed.FailureReason
	}
	// ProgressReported distinguishes "the heartbeat reported zero activity"
	// from "no heartbeat arrived": an absent Progress block leaves the counters
	// at zero, which must never be read as observed inactivity.
	if p := failed.Progress; p != nil {
		ev.ProgressReported = true
		ev.TurnsThisAttempt = p.TurnsThisAttempt
		ev.TokensThisAttempt = p.TokensThisAttempt
	}
	return failuresig.Match(ev)
}

// --- product-directed discovery filing (E32.11 / #1737) --------------------

// productIssueFilingPrecondition is the ONE precondition every emitted
// fishhawk_report_product_issue suggestion carries. Filing stays
// OPERATOR-GATED: nothing in the classifier files anything, the suggestion is
// display-only, and the wording is deliberately conditional so the surface
// never reads as a default recommendation on any failure (a category-B failure
// in particular is more often an agent scope error than product friction).
const productIssueFilingPrecondition = "OPERATOR JUDGEMENT, never automatic: file only when this failure looks like Fishhawk product friction — the loop, the runner, or the tooling behaving wrongly — rather than a defect in the task itself (a category-B scope error is usually the latter, not product friction)"

// productIssueAction returns the pre-populated filing suggestion (#1737).
//
// runID is the run whose product-facts bundle the report carries: a real run id
// when the classifier can resolve one, or a field-path pointer (the
// run.concerns.items[].id idiom already used for concern_ids) when it cannot —
// fishhawk_report_product_issue REQUIRES a run, so an arm with no resolvable id
// either points at where to read it or emits nothing at all.
//
// Params carry only parameters the real tool accepts (ReportProductIssueInput:
// run_id / kind / description / include_free_text) — inventing a param would
// hand the agent an argument the tool would refuse.
//
// why is the one-line evidence anchor: what failed, and in which shape.
func productIssueAction(runID, why string) SuggestedAction {
	return SuggestedAction{
		Action:       "fishhawk_report_product_issue",
		Params:       map[string]string{"run_id": runID, "kind": "bug"},
		Precondition: productIssueFilingPrecondition,
		Consumes:     consumesNone,
		Reason:       why + " — fishhawk_report_product_issue auto-collects this run's redacted product-facts bundle (stage states, failure surface, failure detail class, audit sequence range), so no evidence has to be hand-assembled; it is deduped on that bundle's fingerprint, so a recurrence lands as an occurrence comment rather than a duplicate issue",
	}
}

// productIssueFilingStates is the CLOSED set of classified next-actions states
// on which the filing suggestion is offered (#1737): the category-B/C/D
// implement-failure arms, the terminal failed/cancelled arm that today carries
// zero actions, and the two drive-derived ci_failed arms.
//
// Deliberately EXCLUDED: every healthy state (the anti-noise contract —
// TestNextActions_NoFilingSuggestionOnHealthyRun), and
// implement_failed_category_a, where the failure is an agent/harness fault that
// routes to retry and whose recovery the failure-signature registry (#1703)
// already names — a filing nudge there would be pure noise.
var productIssueFilingStates = map[string]struct{}{
	"implement_failed_category_b":                     {},
	"implement_failed_category_b_decomposition_child": {},
	"slices_integration_conflict":                     {},
	"implement_failed":                                {},
	"ci_failed_routable":                              {},
	"ci_failed_unroutable":                            {},
	"failed":                                          {},
	"cancelled":                                       {},
}

// productIssueFilingState reports whether the classified state is a
// failure shape that warrants offering the filing suggestion. THIS IS THE
// CONTROL that keeps the suggestion off healthy runs: delete it (append
// unconditionally) and TestNextActions_NoFilingSuggestionOnHealthyRun goes red.
func productIssueFilingState(state string) bool {
	_, ok := productIssueFilingStates[state]
	return ok
}

// firstFailedStage returns the FIRST stage in state "failed" (stages are
// ordered by sequence, so the earliest failure is the one that explains the
// run), or nil when none failed.
func firstFailedStage(stages []Stage) *Stage {
	for i := range stages {
		if stages[i].State == "failed" {
			return &stages[i]
		}
	}
	return nil
}

// productIssueFilingWhy renders the evidence anchor for the filing suggestion:
// the failing stage type and its failure category when a failed stage is in
// hand, else the classified state alone (a cancelled run has no failed stage).
func productIssueFilingWhy(stages []Stage, state string) string {
	failed := firstFailedStage(stages)
	if failed == nil {
		return fmt.Sprintf("the run is %s (%s)", state, state)
	}
	// Named failureCat, NOT "category": backend/internal/audit's completeness
	// scanner treats any category-named identifier bound to a lowercase string
	// literal as an AUDIT-category emission and demands a registry entry. This
	// is a stage FAILURE category (A/B/C/D, or unclassified when unset).
	failureCat := "unclassified"
	if failed.FailureCategory != nil && *failed.FailureCategory != "" {
		failureCat = *failed.FailureCategory
	}
	return fmt.Sprintf("the %s stage failed category %s and the run classified %s", failed.Type, failureCat, state)
}

// foldProductIssueSuggestion appends the operator-gated filing suggestion as
// the LAST action when the classified state is one of the failure shapes above
// (#1737). Appended last, after the drive fold, the structural guard and the
// live-validation advisory, so the RECOVERY move always leads and filing is
// never presented as the default next step. na is mutated in place; a nil na
// (only the nil-run early return upstream) is a no-op.
func foldProductIssueSuggestion(run *Run, stages []Stage, na *NextActions) {
	if na == nil || !productIssueFilingState(na.State) {
		return
	}
	na.Actions = append(na.Actions, productIssueAction(run.ID, productIssueFilingWhy(stages, na.State)))
}

// foldFailureSignature attaches the failure-signature hint to na (#1703).
//
// It ONLY sets na.Signature. It never appends to, reorders, or rewrites
// na.Actions — which is what makes both "an unmatched failure behaves exactly
// as today" and "a matched failure keeps today's actions" trivially true and
// directly testable. The block is an advisory surface; a hint that changed the
// operator's legal next moves would be a bug.
//
// Folded AFTER the structural empty-actions guard so that guard keeps measuring
// the classifier's OWN actions, exactly as the live-validation fold requires.
// na is mutated in place; a nil na (only the nil-run early return upstream) is
// a no-op.
func foldFailureSignature(run *Run, stages []Stage, na *NextActions) {
	if na == nil {
		return
	}
	na.Signature = failureSignatureFor(run, stages)
}

// workingDirInheritingActions is the closed set of next-action verbs that take
// a working_dir parameter and inherit the run's start_run binding (E66.42 /
// #2482). foldWorkingDirParams stamps the binding onto exactly these.
var workingDirInheritingActions = map[string]struct{}{
	"fishhawk_run_stage":      {},
	"fishhawk_dispatch_stage": {},
	"fishhawk_run_children":   {},
	"fishhawk_drive_run":      {},
}

// foldWorkingDirParams stamps the run's bound working_dir (E66.42 / #2482) onto
// the params of every emitted action whose verb is one of the four
// runner-spawning verbs, so a driving loop propagates the binding without
// re-deriving it. A no-op when the run carries no binding — an unbound run keeps
// today's params rather than advertising an empty-string path the agent would
// pass verbatim (which would then be refused as non-absolute over HTTP). na is
// mutated in place; a nil na (only the nil-run early return upstream) is a
// no-op. One fold at the end of nextActionsFor beats editing ~15 call sites and
// cannot miss a future one.
func foldWorkingDirParams(run *Run, na *NextActions) {
	if na == nil || run.WorkingDir == "" {
		return
	}
	for i := range na.Actions {
		if _, ok := workingDirInheritingActions[na.Actions[i].Action]; !ok {
			continue
		}
		if na.Actions[i].Params == nil {
			na.Actions[i].Params = map[string]string{}
		}
		na.Actions[i].Params["working_dir"] = run.WorkingDir
	}
}

// liveValidationGuidance renders the operator live-validation guidance string
// (#2045, E48.35) from a decoded Run.LiveValidation block, or "" when the run
// carries no pending live-validation walk. Three variants, keyed off the
// backend's filing_failed / filing_incomplete bits (binding condition A(1)):
//
//   - healthy walk (filing_failed=false, a non-empty walk_ref): "walk: #X"
//   - filing failure (filing_failed=true, filing_incomplete=false): the
//     file-manually variant "walk filing failed — file manually"
//   - stranded intent marker (filing_failed=true, filing_incomplete=true — the
//     crash window): "walk filing incomplete — file manually"
//
// The "walk: #X" branch is gated on a NON-EMPTY walk_ref so a malformed
// linked marker (walk_ref empty yet filing_failed=false, which the backend never
// writes) degrades to the file-manually wording rather than rendering a
// nonsensical empty-ref "walk: " string (condition A(1): never a malformed
// empty-ref string, never a healthy "walk: #X" for an un-filed walk).
func liveValidationGuidance(lv *RunLiveValidation) string {
	if lv == nil || lv.PendingCriteriaCount <= 0 {
		return ""
	}
	n := lv.PendingCriteriaCount
	switch {
	case !lv.FilingFailed && lv.WalkRef != "":
		return fmt.Sprintf("%d criteria pending operator live-validation (walk: %s)", n, lv.WalkRef)
	case lv.FilingIncomplete:
		return fmt.Sprintf("%d criteria pending operator live-validation (walk filing incomplete — file manually)", n)
	default:
		return fmt.Sprintf("%d criteria pending operator live-validation (walk filing failed — file manually)", n)
	}
}

// foldLiveValidationAdvisory appends a DISPLAY-ONLY operator-live-validation
// advisory to na when the run carries a pending live-validation walk (#2045).
// It is folded in for EVERY run state — including a merged/terminal run — so an
// un-filed or failed walk is never silently dropped once the PR ships; the walk
// tracks a live check the operator still owes. Appended LAST (after the drive
// fold and the structural guard) so it never reorders the primary next move and
// never suppresses the unclassified fallback. A no-pending-walk run appends
// nothing, leaving every existing surface byte-identical. na is mutated in
// place; a nil na (only ever a nil-run early return upstream) is a no-op.
func foldLiveValidationAdvisory(run *Run, na *NextActions) {
	if na == nil {
		return
	}
	guidance := liveValidationGuidance(run.LiveValidation)
	if guidance == "" {
		return
	}
	params := map[string]string{"run_id": run.ID}
	if ref := run.LiveValidation.WalkRef; ref != "" {
		params["walk_ref"] = ref
	}
	na.Actions = append(na.Actions, SuggestedAction{
		Action:       "operator_live_validation",
		Params:       params,
		Precondition: "the approved plan carries requires_live_validation acceptance criteria — a live forge/deploy/external target the default-deny sandbox cannot reach, so the acceptance stage cannot validate them",
		Consumes:     consumesNone,
		Reason:       guidance + " — perform the live check yourself; when the walk was not durably filed (file manually) open the tracking work item by hand so the pending validation is not shipped silently unvalidated",
	})
}

// classifyNextActions is the state table. Each arm returns a labeled
// state with >= 1 action; only terminal arms return nil actions.
func classifyNextActions(run *Run, stages []Stage, planReviewStatus, implementReviewStatus *ReviewStatus, hint *ReviewActionHint, mergeObserved, acceptanceSkippedOutOfScope, acceptanceArbitrated bool, acceptanceVerdict, acceptanceTriageDisposition string, release releaseSignals) *NextActions {
	plan := stageByType(stages, "plan")
	impl := stageByType(stages, "implement")
	review := stageByType(stages, "review")
	acceptance := stageByType(stages, "acceptance")
	implReviewPending := implementReviewStatus != nil && implementReviewStatus.Status == "pending"

	// Run already succeeded: the wedge arm, then the merge ritual.
	if run.State == "succeeded" {
		if implReviewPending {
			// A succeeded DECOMPOSITION CHILD (#1082): the run reports
			// succeeded while its own implement review is still pending
			// because the PARENT owns review under #1061 — the child pushes
			// to the shared parent branch and never merges or gets reviewed
			// individually. This is NOT the #968 wedge (which is a top-level
			// run that must merge), so the merge_and_file_follow_up framing is
			// wrong here. Detect the orchestrator's minted-child shape — the
			// SAME predicate implementFailedNextActions uses for a category-B
			// child: a parent_run_id plus an implement stage but NO plan or
			// review stage of its own (a CI-retry child carries a review stage
			// and is excluded by the review == nil clause). Point the operator
			// at the parent run instead of a non-existent per-child PR.
			if run.ParentRunID != nil && plan == nil && review == nil {
				return &NextActions{
					State: "awaiting_parent_consolidation",
					Actions: []SuggestedAction{{
						Action:       "fishhawk_get_run_status",
						Params:       map[string]string{"run_id": *run.ParentRunID},
						Precondition: "this is a succeeded decomposition child (it carries a parent_run_id and has only an implement stage — no plan or review of its own) whose own implement review stays pending because the parent gates the consolidated diff (#1061)",
						Consumes:     consumesNone,
						Reason:       "the slice pushed to the shared parent branch and succeeded; the parent run consolidates the fan-out and gates review, so there is no per-child PR to merge and no #968 wedge to recover — poll the parent run for the consolidation state",
					}},
				}
			}
			// #968-class wedge: the run reported succeeded while the
			// implement review gate is still pending (e.g. a forced fix-up
			// pass completed the run early). Documented recovery arm.
			return &NextActions{
				State: "succeeded_review_wedged",
				Actions: []SuggestedAction{{
					Action:       "merge_and_file_follow_up",
					Params:       prParams(run),
					Precondition: "the run is succeeded but the implement review gate is still pending (#968-class wedge) — no further stage execution will resolve it",
					Consumes:     consumesNone,
					Reason:       "documented recovery: review the diff yourself, approve the PR with an operator verdict, merge, and file a follow-up issue for the unreviewed concerns",
				}},
			}
		}
		if run.PullRequestURL != nil && *run.PullRequestURL != "" {
			// Lifecycle owns its post-merge tail (#1370): when a
			// post_merge_observed audit entry is present the backend has
			// observed the PR merge resolve, so the approve_pr/fishhawk_merge_run
			// ritual is already complete. Surface succeeded_merged with only
			// the operator post_merge dev-host step (rebuild/reload stays an
			// operator/deploy concern, ADR-038) — dropping the now-done
			// approve_pr/fishhawk_merge_run steps.
			if mergeObserved {
				return &NextActions{State: "succeeded_merged", Actions: []SuggestedAction{postMergeStep(run)}}
			}
			// E38.3 (#1657): the acceptance stage was auto-terminated because the
			// approved plan declared verification.out_of_scope with no
			// acceptance_criteria — the run stays MERGE-ELIGIBLE (the same
			// merge-ritual actions), only the state label changes so the operator
			// knows why no acceptance verdict was recorded. Graceful degradation:
			// when the skip entry has aged out of the recent-audit window the flag
			// is false and the arm falls back to plain succeeded_pr_open, itself
			// merge-eligible.
			if acceptanceSkippedOutOfScope {
				return &NextActions{State: "succeeded_acceptance_skipped_out_of_scope", Actions: mergeRitualActions(run,
					"the run succeeded with its PR open; the acceptance stage was auto-terminated because the approved plan declared verification.out_of_scope with no acceptance_criteria (E38.3 / #1657) — still merge-eligible")}
			}
			// #2347: the acceptance stage short-circuited to a not_validated
			// verdict — it verified ZERO criteria. Merge-eligible like the arm
			// above (same merge ritual), but the state label and reason say what
			// actually happened so a terminal-run read does not report a pass that
			// never occurred. Same degradation: a verdict aged out of the
			// recent-audit window leaves the flag empty and falls back to plain
			// succeeded_pr_open, itself merge-eligible.
			if acceptanceVerdict == acceptanceVerdictNotValidated {
				return &NextActions{State: "succeeded_acceptance_not_validated", Actions: mergeRitualActions(run,
					"the run succeeded with its PR open; the acceptance stage verified ZERO acceptance criteria (short-circuited with no runner and no preview, #2347) — merge-eligible, but NOT a validated pass: acknowledge in your merge verdict that acceptance validated nothing")}
			}
			// #2512: the terminal-run twin of the acceptance_undecidable arm. The
			// stage RAN and reported at least one criterion it could not DECIDE,
			// with nothing failing. Merge-eligible on the same merge ritual, and
			// the state label + reason are what keep a terminal-run read from
			// reporting a validated pass over an unevaluated criterion. Same
			// degradation: a verdict aged out of the recent-audit window leaves the
			// flag empty and falls back to plain succeeded_pr_open.
			if acceptanceVerdict == acceptanceVerdictUndecidable {
				return &NextActions{State: "succeeded_acceptance_undecidable", Actions: mergeRitualActions(run,
					"the run succeeded with its PR open; the acceptance stage could not DECIDE one or more acceptance criteria (#2512) — no criterion failed, so this is not a triage, and the run is merge-eligible with no arbitration. But it is NOT a validated pass: acknowledge in your merge verdict which criteria went undecided")}
			}
			return &NextActions{State: "succeeded_pr_open", Actions: mergeRitualActions(run, "the run succeeded with its PR open")}
		}
		return &NextActions{State: run.State}
	}

	// Implement-failure recovery arms apply whether the run row is failed
	// (the usual case — a failed stage fails the run) or still running.
	if impl != nil && impl.State == "failed" {
		return implementFailedNextActions(run, plan, stageByType(stages, "review"), impl)
	}

	if runStateIsTerminal(run.State) {
		// failed/cancelled with no recovery arm (e.g. the plan stage
		// failed, or the run was cancelled): nothing legal to do on THIS
		// run — the block still names the state.
		return &NextActions{State: run.State}
	}

	// Plan stage arms.
	if plan != nil && !stageStateIsTerminal(plan.State) {
		return planStageNextActions(run, plan, planReviewStatus)
	}

	// Implement stage arms (the plan gate is behind us, or no plan stage
	// exists — the resume_run recovery-child shape).
	if impl != nil && (plan == nil || plan.State == "succeeded") {
		if a := implementStageNextActions(run, impl, review, acceptance, implementReviewStatus, hint, acceptanceSkippedOutOfScope, acceptanceArbitrated, acceptanceVerdict, acceptanceTriageDisposition); a != nil {
			return a
		}
	}

	// Release-workflow loop arm (E33.5 / #1590, ADR-051). A delegating
	// WorkflowID == "release" run drives the operator through
	// prepare -> preview -> cut -> (human-led tag push) -> publish, tracked via
	// the release_notes-artifact / release_cut / release_published signals plus
	// the deploy stage state (all computed in getRunStatus). Placed BEFORE the
	// generic deploy arm so a release run gets release-verb next-actions (the
	// operator loop) rather than the plain deploy-approval gate; a non-release
	// delegating run (release.IsRelease false) skips this and reaches the deploy
	// arm unchanged. Display-only — like every other arm it never gates the run.
	if release.IsRelease {
		if a := releaseStageNextActions(run, release, stageByType(stages, "deploy")); a != nil {
			return a
		}
	}

	// Deploy stage arms (E23.13 / #1429). A standalone delegating release run
	// has a single deploy stage and no plan/implement of its own, so it falls
	// through every arm above; without this it would read as unclassified.
	// Placed AFTER the implement arm and BEFORE the no-stages / unclassified
	// fallback.
	if deploy := stageByType(stages, "deploy"); deploy != nil && !stageStateIsTerminal(deploy.State) {
		return deployStageNextActions(run, deploy)
	}

	// A run with no stage rows yet (just created; stages not materialized).
	if len(stages) == 0 {
		return &NextActions{
			State: "stages_pending",
			// The bare FLOOR, not the derived cadence (E48.62 / #2489): no
			// stage row exists yet, so there is no started_at to derive
			// elapsed from and nothing has begun consuming the prediction.
			// Polling tightly is correct — the caller is waiting for the
			// stages to materialize, which is a sub-second event.
			Actions: []SuggestedAction{pollAction(run,
				suggestedStageWaitPollIntervalSeconds,
				"the run has no stage rows yet — re-poll until the stages materialize")},
		}
	}

	return unclassifiedNextActions(run, stages)
}

// planStageNextActions covers the plan stage's non-terminal states:
// not started, running, and parked at the approval gate (split on
// whether the plan review has settled).
func planStageNextActions(run *Run, plan *Stage, planReviewStatus *ReviewStatus) *NextActions {
	switch plan.State {
	case "running":
		return &NextActions{
			State: "plan_running",
			Actions: []SuggestedAction{pollAction(run,
				derivedStageWaitPollInterval(run, plan),
				"the plan stage is executing — re-poll until plan_stage_wait_status goes terminal")},
		}
	case "awaiting_input":
		// The planner parked at awaiting_input with a clarification_request
		// (#1080/#1057): the issue was not yet plannable, so the operator
		// must answer the parked questions before planning resumes.
		// fishhawk_answer_clarification (#1088) records the answers as a
		// dedicated clarification_answered audit entry and re-opens the SAME
		// plan stage — no new run, no duplicate reviews (distinct from
		// fishhawk_resume_run, which mints a child run).
		return &NextActions{
			State: "plan_awaiting_input",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_answer_clarification",
				Params:       map[string]string{"run_id": run.ID},
				Precondition: "the plan stage parked at awaiting_input with a clarification_request; read the parked questions first (fishhawk_get_run_status or fishhawk_list_audit on category clarification_requested)",
				Consumes:     consumesNone,
				Reason:       "answer the planner's parked questions; the answers inject into the resumed plan agent's binding conditions and re-open this plan stage in the SAME run — pass one {id, answer} per parked question",
			}},
		}
	case "awaiting_approval":
		if planReviewStatus != nil && planReviewStatus.Status == "pending" {
			return &NextActions{
				State: "plan_review_pending",
				Actions: []SuggestedAction{
					pollAction(run, suggestedReviewPollIntervalSeconds,
						"the plan review was dispatched but no verdict has landed — read it before approving, do NOT approve yet"),
					{
						Action:       "fishhawk_await_review",
						Params:       map[string]string{"run_id": run.ID, "stage": "plan"},
						Precondition: "optional convenience block over the same poll",
						Consumes:     consumesNone,
						Reason:       "blocks until the plan review reaches a terminal status",
					},
				},
			}
		}
		return &NextActions{
			State: "plan_gate_parked",
			Actions: []SuggestedAction{
				{
					Action:       "fishhawk_approve_plan",
					Params:       map[string]string{"run_id": run.ID},
					Precondition: "read fishhawk_get_plan and the reviewer verdicts first; scope amendments in the approval reason must name files as dir/name.ext (or use add_scope_files)",
					Consumes:     consumesApprovalSlot,
					Reason:       "the plan is parked at its approval gate" + reviewVerdictSummary(planReviewStatus),
				},
				{
					Action:       "fishhawk_revise_plan",
					Params:       map[string]string{"run_id": run.ID, "constraint": "<binding design constraint>"},
					Precondition: "the plan's direction is sound but a design constraint must change before it proceeds; cheaper than a reject → fresh-run replan",
					Consumes:     consumesApprovalSlot,
					Reason:       "re-plans IN PLACE with your binding constraint injected and the prior plan as the revision base; re-enters the review → approve gate (bounded, default one pass)",
				},
				{
					Action:       "fishhawk_reject_plan",
					Params:       map[string]string{"run_id": run.ID},
					Precondition: "the plan takes a wrong fork that approval conditions cannot amend",
					Consumes:     consumesApprovalSlot,
					Reason:       "a detailed rejection reason propagates to a NEW run for the same issue as PriorRejectionFeedback",
				},
			},
		}
	case "dispatched":
		// A spawn attempt exists (#1912) — a runner is in flight. Poll rather than
		// offering a dispatch that would double-drive the stage.
		return &NextActions{
			State: "plan_dispatched",
			Actions: []SuggestedAction{pollAction(run,
				derivedStageWaitPollInterval(run, plan),
				"the plan stage is dispatched — a spawn attempt exists (#1912) and a runner is in flight; re-poll until plan_stage_wait_status goes terminal")},
		}
	default: // pending | awaiting_host_dispatch | awaiting_children
		return &NextActions{State: "plan_pending", Actions: dispatchOrPollActions(run, "plan", plan)}
	}
}

// implementStageNextActions covers the implement stage's post-plan
// states. Returns nil when no arm matches (the caller falls through to
// the unclassified fallback). acceptance is the run's acceptance stage
// (nil when the workflow declares none, E31.9); when present it gates
// the merge, so the settled path branches to the acceptance arm before
// the merge ritual. acceptanceVerdict / acceptanceTriageDisposition are
// the signals extracted from the acceptance_outcome_recorded /
// acceptance_triage_decided audit payloads. acceptanceSkippedOutOfScope is the
// recent-audit-window flag threaded down so the acceptance arm can recognize an
// E38.3 / #1877 out-of-scope skip as a merge-eligible disposition.
// review is the run's review stage (nil when the workflow declares none); it is
// read ONLY by the #3116 fix-up-gate check below, which decides whether
// recommending fishhawk_fixup_stage would point at a verb the endpoint refuses.
func implementStageNextActions(run *Run, impl, review, acceptance *Stage, implementReviewStatus *ReviewStatus, hint *ReviewActionHint, acceptanceSkippedOutOfScope, acceptanceArbitrated bool, acceptanceVerdict, acceptanceTriageDisposition string) *NextActions {
	switch impl.State {
	case "pending", "awaiting_host_dispatch":
		// pending and awaiting_host_dispatch (#1912) both await a host spawn — the
		// operator host dispatches. Post-#1912 'dispatched' is a distinct state (a
		// spawn attempt exists), routed to poll-only below.
		return &NextActions{State: "implement_pending", Actions: dispatchOrPollActions(run, "implement", impl)}
	case "dispatched":
		// A spawn attempt exists (#1912) — a runner is in flight. Poll rather than
		// offering a dispatch that would double-drive the stage.
		return &NextActions{
			State: "implement_dispatched",
			Actions: []SuggestedAction{pollAction(run,
				derivedStageWaitPollInterval(run, impl),
				"the implement stage is dispatched — a spawn attempt exists (#1912) and a runner is in flight; re-poll until implement_stage_wait_status goes terminal (fishhawk_drive_run auto-re-dispatches a probed-dead runner; a live-but-unregistered process stops for inspection with NO re-dispatch, and only an unprobeable/UNKNOWN result — pgrep unavailable — hands back a manual re-dispatch)")},
		}
	case "awaiting_children":
		// A DECOMPOSED PARENT parked at awaiting_children (#1147): the legal
		// next move is to fan out the still-pending children, and the
		// children_status block on this same snapshot carries each child's
		// live state + the fan-in/integration phase. Dedicated arm so the
		// operator is pointed at fishhawk_run_children + children_status
		// instead of the generic dispatch-or-poll for a single stage.
		return &NextActions{State: "implement_awaiting_children", Actions: awaitingChildrenActions(run)}
	case "running":
		return &NextActions{
			State: "implement_running",
			Actions: []SuggestedAction{pollAction(run,
				derivedStageWaitPollInterval(run, impl),
				"the implement stage is executing — re-poll until implement_stage_wait_status goes terminal")},
		}
	case "awaiting_scope_decision":
		// #1231, generalized by #2501: the implement stage's ONLY committed-tree
		// gate failure was a shortfall of EITHER class — the #1151
		// missing-declared-scope-file check or the #1171 binding-assertion
		// check — and verify otherwise passed, so the runner pushed the
		// gate-verified commit to the run branch (no PR) and PARKED here instead
		// of failing category-B. The legal next move is the in-band operator
		// decision: exempt (return the stage to dispatch so the held commit's PR
		// opens with NO agent re-run) or fail (fall through to category-B). The
		// shortfall (missing_paths / unsatisfied_assertions) + held SHA are on
		// the scope_completeness_parked audit entry.
		return &NextActions{
			State: "implement_awaiting_scope_decision",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_decide_scope_completeness",
				Params:       map[string]string{"run_id": run.ID, "decision": "exempt|amend|fail"},
				Precondition: "the implement stage parked at awaiting_scope_decision (its SOLE committed-tree gate shortfall was the #1151 missing-declared-scope-file check, an #1171 unsatisfied binding assertion, OR a #2548 build-required scope drift on a decomposition child; the gate-verified commit is already on the run branch). Read the shortfall (missing_paths / unsatisfied_assertions / build_required_paths) + held SHA from the scope_completeness_parked audit entry first",
				Consumes:     consumesNone,
				Reason:       "decide in-band: exempt accepts the already-committed tree and returns the stage to dispatch, so the held commit's PR opens with NO agent re-run (on a local run, dispatch the stage to spawn the runner); amend widens this stage's scope with the paths you name and resumes it so the agent re-runs — the ONLY non-fail resolution for a build_required_paths park, whose exempt is refused because its held tree is red by construction; fail falls through to today's category-B fail-and-restore",
			}},
		}
	case "awaiting_approval", "succeeded":
		if implementReviewStatus != nil && implementReviewStatus.Status == "pending" {
			return &NextActions{
				State: "implement_review_pending",
				Actions: []SuggestedAction{
					pollAction(run, suggestedReviewPollIntervalSeconds,
						implementReviewMergeHint(implementReviewStatus)),
					{
						Action:       "fishhawk_await_review",
						Params:       map[string]string{"run_id": run.ID, "stage": "implement"},
						Precondition: "optional convenience block over the same poll",
						Consumes:     consumesNone,
						Reason:       "blocks until the implement review reaches a terminal status",
					},
				},
			}
		}
		if hint != nil {
			// #3116: recommending fishhawk_fixup_stage is only legal while the
			// fix-up gate is actually open. In a workflow that orders acceptance
			// BEFORE review (feature_change), a succeeded implement stage sits with
			// its review stage still `pending`, and run.findOpenReviewStage refuses
			// the fix-up with 422 fixup_not_applicable. The classifier used to
			// recommend it anyway — a surface naming a verb the endpoint refuses.
			// Both arms keep fishhawk_defer_concern (legal now, spends no fix-up
			// budget) and the hint keeps reporting remaining_fixup_budget, so the
			// operator sees the route-back survives the wait.
			if !fixupGateOpen(impl, review) {
				if acceptance != nil && !stageStateIsTerminal(acceptance.State) {
					return &NextActions{State: "implement_concerns_open_acceptance_pending", Actions: hint.gateClosedActions(run, acceptance)}
				}
				return &NextActions{State: "implement_concerns_open_gate_closed", Actions: hint.gateClosedActions(run, nil)}
			}
			// Open concerns: embed the hint's options as actions. The
			// entries derive FROM the computed ReviewActionHint value
			// (review_action_hint.go), so the two surfaces agree by
			// construction.
			return &NextActions{State: "implement_concerns_open", Actions: hint.suggestedActions(run, impl.ID)}
		}
		// Review settled with nothing to route back. When the workflow
		// declares an acceptance stage (E31.9 / ADR-049), it gates the merge
		// — branch to the acceptance arm BEFORE the merge ritual.
		if acceptance != nil {
			return acceptanceStageNextActions(run, acceptance, acceptanceSkippedOutOfScope, acceptanceArbitrated, acceptanceVerdict, acceptanceTriageDisposition)
		}
		// No acceptance stage: the PR is the next surface — approve and merge.
		return &NextActions{State: "implement_gate_settled", Actions: mergeRitualActions(run, "the implement review is settled with no open concerns")}
	default:
		return nil
	}
}

// deployStageNextActions covers a delegating deploy stage's non-terminal
// states (E23.13 / #1429 / ADR-038). A deploy stage's gate is PRE-execution
// (its effect IS the side effect), so the operator judgment point is the
// approval at awaiting_deploy_approval; once approved, the backend triggers the
// external pipeline and the deployreconciler polls it to terminal
// (awaiting_deployment). The defensive pending/dispatched/running arms cover
// the brief windows the backend itself parks/advances the stage through — there
// is nothing for the operator to do but re-poll.
func deployStageNextActions(run *Run, deploy *Stage) *NextActions {
	switch deploy.State {
	case "awaiting_deploy_approval":
		// The pre-execution approval gate (the operator judgment point). The
		// deploy gate is approved via fishhawk_approve_deploy (E23.15 / #1432),
		// which resolves the type=deploy stage and composes the required
		// --environment=<env> (and optional --override-freeze) into the
		// approval comment the backend deploy pre-flight parses. The older
		// fishhawk_approve_plan hint failed here: it resolves a type=plan stage
		// first and errors on a plan-less release run before reaching the
		// approval endpoint. fishhawk_reject_deploy is the reject counterpart.
		return &NextActions{
			State: "deploy_gate_parked",
			Actions: []SuggestedAction{
				{
					Action:       "fishhawk_approve_deploy",
					Params:       map[string]string{"run_id": run.ID, "environment": "<one of the deploy stage's allowed_environments>"},
					Precondition: "the deploy stage is parked at its pre-execution approval gate (awaiting_deploy_approval); requires an operator token with write:deploy (ADR-038/#1390) and a required environment that is one of the deploy stage's allowed_environments (composed into the approval comment as --environment=<env>); pass override_freeze=true when the stage declares change_freeze. Confirm the corresponding change merged and the pre-flight deploy constraints (allowed_environments, change_freeze, required_upstream) hold before approving",
					Consumes:     consumesApprovalSlot,
					Reason:       "approve the deploy INTENT (ADR-038: a deploy stage's effect is the side effect, so the gate is pre-execution) — approval triggers the external pipeline; a production deploy pages the human regardless of runner kind",
				},
				{
					Action:       "fishhawk_reject_deploy",
					Params:       map[string]string{"run_id": run.ID},
					Precondition: "the deploy stage is parked at its pre-execution approval gate (awaiting_deploy_approval) and the deploy should NOT proceed; reject routes through advanceStage so it needs neither write:deploy nor an environment",
					Consumes:     consumesApprovalSlot,
					Reason:       "reject the deploy INTENT, failing the deploy gate without triggering the external pipeline",
				},
			},
		}
	case "awaiting_deployment":
		// Approved and triggered: the backend deployreconciler is polling the
		// external pipeline to terminal. Nothing for the operator to do but
		// re-poll.
		return &NextActions{
			State: "deploy_in_flight",
			Actions: []SuggestedAction{pollAction(run,
				derivedStageWaitPollInterval(run, deploy),
				"the deploy intent was approved and the external pipeline is running — the backend deployreconciler is polling it to terminal; re-poll until the deploy stage settles")},
		}
	case "dispatched", "running":
		// Defensive: brief windows the backend advances the stage through after
		// approval, before the reconciler picks it up. Poll.
		return &NextActions{
			State: "deploy_in_flight",
			Actions: []SuggestedAction{pollAction(run,
				derivedStageWaitPollInterval(run, deploy),
				"the deploy stage is advancing through the backend toward the external pipeline — re-poll until it settles")},
		}
	default: // pending
		// Defensive: a deploy-first run is parked at the gate at creation
		// (#1429), so a pending deploy stage is a transient pre-park window
		// (or a creation-time Advance that has not landed). Poll — the backend
		// parks it at the gate.
		return &NextActions{
			State: "deploy_initializing",
			Actions: []SuggestedAction{pollAction(run,
				derivedStageWaitPollInterval(run, deploy),
				"the deploy stage has not yet parked at its pre-execution approval gate — re-poll until it reaches awaiting_deploy_approval (the backend parks it at creation)")},
		}
	}
}

// releaseSignals bundles the release-workflow loop signals (E33.5 / #1590,
// ADR-051) the next_actions classifier reads for a WorkflowID == "release" run.
// getRunStatus derives them from data it already fetches — the run's
// WorkflowID (IsRelease), the deploy stage state, the release_cut /
// release_published audit entries in the recent-audit slice, and a
// release_notes-artifact presence probe — and threads them in as an additive
// value param. The zero value (IsRelease false) leaves every non-release run
// byte-identical, so the arm is inert for the whole existing surface. Like the
// rest of next_actions it is DISPLAY-ONLY: it never gates the release.
type releaseSignals struct {
	// IsRelease gates the release arm — true only for the delegating "release"
	// workflow.
	IsRelease bool
	// NotesPrepared is true once a release_notes artifact has been persisted
	// (the prepare verb ran). The persist endpoint creates an ARTIFACT and
	// emits no audit entry, so this is probed from the run's stage artifacts,
	// not the audit trail.
	NotesPrepared bool
	// Cut is true once a release_cut audit entry exists — the operator's
	// version decision is recorded (POST /v0/releases/cut). Records the
	// decision ONLY; it pushes no git tag.
	Cut bool
	// Published is true once a release_published audit entry exists — the notes
	// are live on the GitHub Release (POST /v0/releases/publish).
	Published bool
	// DeployState is the release run's deploy stage state ("" when absent). It
	// distinguishes pipeline_running (the pushed-tag pipeline is in flight)
	// from awaiting_publish (the pipeline has settled).
	DeployState string
}

// releaseStageNextActions is the release-workflow loop arm (E33.5 / #1590,
// ADR-051), modeled on deployStageNextActions. It maps the five release-loop
// states to the correct operator verb:
//
//	notes_ready       -> prepare the notes  (fishhawk_release_notes, mode=prepare)
//	awaiting_cut      -> preview then cut    (fishhawk_release_notes preview; `fishhawk release cut`)
//	pipeline_running  -> poll                (the human pushed the tag; the pipeline runs)
//	awaiting_publish  -> publish             (`fishhawk release publish`)
//	published         -> poll                (the loop is complete)
//
// The version cut and the publish are CLI verbs (sibling slice) over the
// /v0/releases/cut and /v0/releases/publish endpoints, so they surface as
// named ritual steps (like approve_pr/merge_pr) rather than MCP tools — the
// only new MCP tool is fishhawk_release_notes (the prepare/preview pair). The
// tag push between cut and the pipeline stays a HUMAN git action per the
// delegating posture, called out in the awaiting_cut reason. States are checked
// most-advanced-first so the freshest signal wins. Every arm carries >= 1
// action, so the arm never returns an empty list.
func releaseStageNextActions(run *Run, sig releaseSignals, deploy *Stage) *NextActions {
	switch {
	case sig.Published:
		// The notes are live on the GitHub Release (a release_published audit
		// entry exists). The release loop is complete; the run winds down.
		return &NextActions{
			State: "published",
			Actions: []SuggestedAction{pollAction(run,
				derivedStageWaitPollInterval(run, deploy),
				"the release notes are published to the GitHub Release (a release_published audit entry exists) — the release loop is complete; re-poll until the run resolves")},
		}
	case sig.Cut && deployInFlight(sig.DeployState):
		// The version is cut and the human pushed the release tag; the external
		// release pipeline (the deploy stage) is in flight. Nothing for the
		// operator to do but re-poll until it settles.
		return &NextActions{
			State: "pipeline_running",
			Actions: []SuggestedAction{pollAction(run,
				derivedStageWaitPollInterval(run, deploy),
				"the version is cut and the release tag was pushed (a human git action); the external release pipeline is running (deploy stage in flight) — re-poll until it settles, then publish the notes")},
		}
	case sig.Cut:
		// Cut and the pipeline (if any) has settled — publish the prepared notes
		// to the GitHub Release.
		return &NextActions{
			State: "awaiting_publish",
			Actions: []SuggestedAction{{
				Action:       "release_publish",
				Params:       map[string]string{"run_id": run.ID},
				Precondition: "the version is cut (a release_cut audit entry exists) and — when a release pipeline gates it — the pipeline has settled; the GitHub Release for the pushed tag exists",
				Consumes:     consumesNone,
				Reason:       "publish the prepared notes with `fishhawk release publish` (POST /v0/releases/publish): it sets the GitHub Release body to the rendered markdown and records a release_published audit entry",
			}},
		}
	case sig.NotesPrepared:
		// Notes are persisted but no cut is recorded — preview them (and the
		// advisory semver bump hint) then record the version decision.
		return &NextActions{
			State: "awaiting_cut",
			Actions: []SuggestedAction{
				{
					Action:       "fishhawk_release_notes",
					Params:       map[string]string{"mode": "preview", "repo": run.Repo},
					Precondition: "a release_notes artifact is prepared but no release_cut decision is recorded; preview the rendered notes and the advisory semver bump hint before cutting",
					Consumes:     consumesNone,
					Reason:       "preview the prepared notes (read-only) to review the changelog and the suggested semver bump before recording the version decision",
				},
				{
					Action:       "release_cut",
					Params:       map[string]string{"run_id": run.ID},
					Precondition: "you have reviewed the previewed notes and chosen the release version",
					Consumes:     consumesNone,
					Reason:       "record the version decision with `fishhawk release cut` (POST /v0/releases/cut) — it writes a release_cut audit entry ONLY and pushes NO git tag. After cut, push the release tag yourself (a human git action) to trigger the external release pipeline",
				},
			},
		}
	default:
		// No notes persisted yet — the release run is ready for the operator to
		// prepare (render + persist) the notes.
		return &NextActions{
			State: "notes_ready",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_release_notes",
				Params:       map[string]string{"mode": "prepare", "repo": run.Repo},
				Precondition: "the release run has no persisted release_notes artifact yet; choose the from/to ref range and the stage_id that keys the artifact",
				Consumes:     consumesNone,
				Reason:       "prepare the release notes: fishhawk_release_notes (mode=prepare) renders the merged-run evidence in the ref range — carrying the advisory semver bump hint — and persists a release_notes artifact the cut and publish verbs consume. Use mode=preview first for a read-only render",
			}},
		}
	}
}

// deployInFlight reports whether a deploy stage state is present and not yet
// terminal — the release arm reads it to split pipeline_running (the pushed-tag
// pipeline is running) from awaiting_publish (no pipeline, or it settled). An
// empty state (no deploy stage) is treated as not-in-flight so a release run
// without a deploy stage falls straight to awaiting_publish after the cut.
func deployInFlight(state string) bool {
	return state != "" && !stageStateIsTerminal(state)
}

// awaitingChildrenActions is the decomposed-parent fan-out arm (#1147): drive
// the still-pending children with fishhawk_run_children, then re-poll — the
// children_status block on the same get_run_status snapshot carries each
// child's live state and the fan-in/integration phase.
func awaitingChildrenActions(run *Run) []SuggestedAction {
	return []SuggestedAction{
		{
			Action:       "fishhawk_run_children",
			Params:       map[string]string{"run_id": run.ID, "workflow": run.WorkflowID},
			Precondition: "the decomposed plan is approved and the parent implement stage is awaiting_children; the children are discovered from the parent's plan_decomposed audit entry",
			Consumes:     consumesNone,
			Reason:       "fan out ALL still-pending decomposed children concurrently (idempotent: in-flight and terminal children are left untouched); a child failure is data, not an error",
		},
		// The bare FLOOR, not the derived cadence (E48.62 / #2489): this arm
		// holds no stage of its own (the parent's implement stage is parked,
		// and the interesting progress is on the CHILD runs, each carrying
		// its own prediction and its own elapsed). Deriving from the parked
		// parent would advertise a cadence for the wrong thing.
		pollAction(run, suggestedStageWaitPollIntervalSeconds,
			"the parent is awaiting_children — re-poll and read the children_status block for each child's live state and the fan-in/integration phase (running_children, ready_to_integrate, integrated, or integration_conflict)"),
	}
}

// reviveRunAction is the fishhawk_revive_run suggestion (#1915) offered on the
// failed-run recovery arms alongside fishhawk_retry_stage. Revive re-parks
// EVERY retryable failed stage WITHOUT dispatching (no orchestrator handoff),
// so it is the safe move when a sibling stage's failure flipped the run
// terminal while a healthy stage's review is still settling — distinct from
// fishhawk_retry_stage, which re-opens ONE stage and auto-dispatches it. Like
// retry, each re-park consumes that stage's per-stage retry budget.
func reviveRunAction(run *Run) SuggestedAction {
	return SuggestedAction{
		Action:       "fishhawk_revive_run",
		Params:       map[string]string{"run_id": run.ID},
		Precondition: "the run flipped terminal-failed and every failed stage is retryable (category A/C, or a retryable D); revive refuses (422 revive_not_applicable) if any failed stage is non-retryable (category-B / D-rejected)",
		Consumes:     consumesRetryBudget,
		Reason:       "re-park ALL retryable failed stages in one operator verb WITHOUT dispatching — the safe batch recovery when a sibling stage's failure flipped the run terminal while a healthy stage's review is still settling. Distinct from fishhawk_retry_stage (which re-opens ONE stage and auto-dispatches): revive never dispatches, so you dispatch each re-parked stage at its proper gate turn via the existing verbs",
	}
}

// implementFailedNextActions branches on the failed implement stage's
// failure category: B routes to the no-replan recovery run (or, for a
// decomposition child, an IN-PLACE re-drive), A to an in-place retry
// (citing a known flake trace event when the failure detail carries one),
// everything else to retry-or-cancel. The category-A and default (retryable)
// arms also offer fishhawk_revive_run (#1915) — the batch no-dispatch re-park.
func implementFailedNextActions(run *Run, plan, review, impl *Stage) *NextActions {
	category := ""
	if impl.FailureCategory != nil {
		category = *impl.FailureCategory
	}
	switch category {
	case "B":
		// Slice integration conflict (ADR-041 / #1142): the PARENT's
		// implement (awaiting_children) stage failed category-B because a
		// slice branch could not merge onto the consolidated branch during
		// fan-in. Recognized by the stable failure-reason PREFIX (human
		// display only); the machine resume target is SOURCED FROM the
		// structured slice_integration_conflict audit payload, NOT parsed
		// from the free-form reason. The conflicting child id is surfaced as
		// a field-path pointer into that payload (the same idiom ci_failed
		// uses for concern_ids) so the consumer reads the real value from
		// the structured entry — `conflicting_child_run_id`. Placed BEFORE
		// the generic category-B parent arms so it wins for the parent run.
		if impl.FailureReason != nil && strings.HasPrefix(*impl.FailureReason, sliceIntegrationConflictReasonPrefix) {
			return &NextActions{
				State: "slices_integration_conflict",
				Actions: []SuggestedAction{{
					Action: "fishhawk_resume_run",
					// resume_run's parent_run_id param holds the conflicting
					// child's OWN id for an in-place decomposition re-drive
					// (#1081). The value is a field-path pointer: read
					// conflicting_child_run_id from the latest
					// slice_integration_conflict audit entry — structured data,
					// never the reason string.
					Params:       map[string]string{"parent_run_id": "recent_audit[category=slice_integration_conflict].payload.conflicting_child_run_id"},
					Precondition: "the parent implement (awaiting_children) stage failed category-B with a slice integration conflict; read the conflicting child id from the latest slice_integration_conflict audit entry's structured payload (conflicting_child_run_id), NOT from the failure reason string",
					Consumes:     consumesNone,
					Reason:       "slice integration conflict during fan-in: the consolidated branch already holds the earlier slices, so re-drive ONLY the conflicting slice child in place (fishhawk_resume_run pointed at conflicting_child_run_id from the slice_integration_conflict audit) to resolve the conflict and resume fan-in — pointing resume at the parent would replan from scratch and discard the succeeded sibling slices",
				}},
			}
		}
		// A failed DECOMPOSITION CHILD recovers IN PLACE (#1081): point
		// fishhawk_resume_run at THIS child's own id and the backend
		// re-drives the SAME run on the shared parent branch — not a new
		// run — so the parked parent fan-out can still consolidate. The
		// MCP Run row does not mirror the backend's decomposed_from field,
		// so the in-band signal is the orchestrator's minted-child shape:
		// a parent_run_id plus an implement stage but NO plan or review
		// stage of its own (each decomposed child carries a single
		// implement stage; the parent owns plan + review). This matches the
		// recover handler's DecomposedFrom gate for every minted child while
		// excluding a CI-retry child, which carries a review stage and is
		// served by the "resume at the parent" arm below.
		if run.ParentRunID != nil && plan == nil && review == nil {
			return &NextActions{
				State: "implement_failed_category_b_decomposition_child",
				Actions: []SuggestedAction{{
					Action:       "fishhawk_resume_run",
					Params:       map[string]string{"parent_run_id": run.ID},
					Precondition: "this run is a failed decomposition child (it carries a parent_run_id and has only an implement stage — no plan or review of its own) whose implement stage failed category-B; point resume at THIS child's own id, NOT the parent",
					Consumes:     consumesNone,
					Reason:       "category-B decomposition-child failure: fishhawk_resume_run pointed at the child re-opens the SAME run in place on the shared parent branch (folding add_scope_files), so the parked parent fan-out can still consolidate — pointing resume at the parent would replan from scratch and discard the succeeded sibling slices",
				}},
			}
		}
		if plan != nil && plan.State == "succeeded" {
			return &NextActions{
				State: "implement_failed_category_b",
				Actions: []SuggestedAction{{
					Action:       "fishhawk_resume_run",
					Params:       map[string]string{"parent_run_id": run.ID},
					Precondition: "the plan stage succeeded and the implement stage failed category-B; clean the working tree (git status) before dispatching the recovery run's implement stage",
					Consumes:     consumesNewRun,
					Reason:       "category-B (scope/constraint) failure: mint a recovery run re-executing the approved plan without replanning; name missing paths via add_scope_files",
				}},
			}
		}
		return &NextActions{
			State: "implement_failed_category_b",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_start_run",
				Params:       map[string]string{"repo": run.Repo, "workflow_id": run.WorkflowID},
				Precondition: "this run has no succeeded plan stage of its own, so fishhawk_resume_run is not eligible against it (point resume at the original parent instead when one exists)",
				Consumes:     consumesNewRun,
				Reason:       "category-B failure without a resumable plan on this run — replan from scratch",
			}},
		}
	case "A":
		reason := "category-A (agent) failure — fishhawk_retry_stage retries it in place; read the trace first for transient harness errors"
		// External-API incident is checked FIRST: a terminal 5xx (e.g. 529
		// overloaded) is an upstream platform incident, not a task failure,
		// so the operator should back off and check status.claude.com rather
		// than immediately burning a retry slot re-hitting the same incident.
		if status, ok := citedExternalAPIStatus(impl); ok {
			reason = fmt.Sprintf("category-A failure from a terminal external API error %d (e.g. 529 overloaded) — likely an upstream incident, not a task failure; back off before fishhawk_retry_stage and check status.claude.com", status)
		} else if citedQuotaUnavailable(impl) {
			// Model-quota exhaustion (a usage / rate cap, #2085): a retry
			// against an unreset cap fails identically, so tell the operator to
			// wait for the cap to reset rather than burn retry budget. Checked
			// after the external-API 5xx arm and before the flake arm.
			reason = "category-A failure: the agent could not obtain model quota (likely a usage/rate cap) — this is not a transient crash and will fail identically until the limit resets; wait for the cap to reset before fishhawk_retry_stage rather than burning retry budget against the wall"
		} else if flake := citedFlakeEvent(impl); flake != "" {
			reason = fmt.Sprintf("category-A failure whose detail cites %s — an absorbed infra flake recurred; a retry is the cheapest next step", flake)
		}
		return &NextActions{
			State: "implement_failed_category_a",
			Actions: []SuggestedAction{
				{
					Action:       "fishhawk_retry_stage",
					Params:       map[string]string{"stage_id": impl.ID},
					Precondition: "the implement stage failed category-A",
					Consumes:     consumesRetryBudget,
					Reason:       reason,
				},
				reviveRunAction(run),
			},
		}
	default:
		return &NextActions{
			State: "implement_failed",
			Actions: []SuggestedAction{
				{
					Action:       "fishhawk_retry_stage",
					Params:       map[string]string{"stage_id": impl.ID},
					Precondition: fmt.Sprintf("the implement stage failed category %q (retry serves categories A/C/D; B routes to fishhawk_resume_run)", category),
					Consumes:     consumesRetryBudget,
					Reason:       "retry the failed stage in place after reading the failure reason",
				},
				reviveRunAction(run),
				{
					Action:       "fishhawk_cancel_run",
					Params:       map[string]string{"run_id": run.ID},
					Precondition: "the failure is not retryable",
					Consumes:     consumesNone,
					Reason:       "cancel this run, then start a fresh one with fishhawk_start_run (consumes a new run)",
				},
			},
		}
	}
}

// ciFailedNextActions covers the drive-derived ci_failed state (#1045):
// a required PR check concluded red on the open PR while the review
// evidence is settled. Routing splits on open-concern presence (the
// same ReviewActionHint signal implementStageNextActions reads, so the
// two surfaces agree by construction):
//
//   - hint != nil (open concerns): ci_failed_routable — route the
//     concerns back with fishhawk_fixup_stage first (a red check is
//     usually the same defect the concerns name), plus a checks re-run
//     for a suspected flake.
//   - hint == nil (no open concerns): ci_failed_unroutable — there is no
//     agent-routable concern, so the fix is the operator's: commit it on
//     the run branch then fishhawk_vouch_commit (#1044), a checks re-run
//     for a flake, or page a human for an unclassifiable failure.
func ciFailedNextActions(run *Run, stages []Stage, hint *ReviewActionHint) *NextActions {
	rerun := SuggestedAction{
		Action:       "rerun_ci_checks",
		Params:       prParams(run),
		Precondition: "the red check is a suspected flake (infra, not a real defect)",
		Consumes:     consumesNone,
		Reason:       "re-run the failed required checks on the PR; a genuine flake goes green on the retry without spending a fix-up pass",
	}
	if hint != nil {
		// Open concerns: the red check is most likely the defect the
		// concerns name — route them back with fishhawk_fixup_stage first,
		// then offer the flake re-run. The merge-with-follow-up ladder that
		// hint.suggestedActions otherwise leads with is deliberately NOT
		// reused here: a red required check is not mergeable.
		fixupParams := map[string]string{"concern_ids": "run.concerns.items[].id"}
		if impl := stageByType(stages, "implement"); impl != nil {
			fixupParams["stage_id"] = impl.ID
		}
		return &NextActions{
			State: "ci_failed_routable",
			Actions: []SuggestedAction{
				{
					Action:       "fishhawk_fixup_stage",
					Params:       fixupParams,
					Precondition: "open implement-review concerns exist and a required PR check is red; stay on a clean default branch — the runner owns the run branch in its lineage worktree",
					Consumes:     consumesFixupBudget,
					Reason:       fmt.Sprintf("%d open concern(s) with a red required check — route them back so the fix-up addresses the defect and re-greens the checks", hint.Concerns),
				},
				rerun,
			},
		}
	}
	return &NextActions{
		State: "ci_failed_unroutable",
		Actions: []SuggestedAction{
			{
				Action:       "commit_and_vouch",
				Params:       prParams(run),
				Precondition: "the review is settled with no open concerns, so there is nothing to route back; the fix is yours to make",
				Consumes:     consumesNone,
				Reason:       "commit the fix on the run branch, then fishhawk_vouch_commit so the operator-authored commit clears the run's sole-writer lineage gate (#1044)",
			},
			rerun,
			{
				Action:       "page_human",
				Params:       map[string]string{"run_id": run.ID},
				Precondition: "the failure is neither a flake nor operator-remediable",
				Consumes:     consumesNone,
				Reason:       "the red required check is unclassifiable or non-remediable here — escalate to a human for a judgment call",
			},
		},
	}
}

// dispatchOrPollActions returns the next move for a stage that exists
// but has not started: on runner_kind local the OPERATOR HOST dispatches
// stages; on github_actions the backend auto-dispatches and the legal
// move is to re-poll. For a local IMPLEMENT stage the DEFAULT is the
// non-blocking fishhawk_dispatch_stage (with fishhawk_run_stage retained
// as an explicit opt-in): the implement stage is the one stage type that
// can file a mid-stage scope amendment (#1189), and a blocking run_stage
// holds the MCP session so the amendment cannot be decided in-band. A
// non-blocking dispatch that never sees an amendment polls to terminal
// identically (ADR-037), so defaulting implement to dispatch has no
// downside (#1247). The plan stage (no amendments) keeps the single
// run_stage action unchanged.
// stage is the pending stage the actions are for; it supplies the started_at
// the poll cadence is derived from (E48.62 / #2489). Pass nil when no stage row
// is in hand — the derivation then yields the floor, which is the honest answer
// for something that has not started.
func dispatchOrPollActions(run *Run, stageType string, stage *Stage) []SuggestedAction {
	if run.RunnerKind == "github_actions" {
		return []SuggestedAction{pollAction(run,
			derivedStageWaitPollInterval(run, stage),
			fmt.Sprintf("runner_kind github_actions auto-dispatches the %s stage — nothing to run from the operator host; re-poll until it starts", stageType))}
	}
	if stageType == "implement" {
		return []SuggestedAction{
			{
				Action:       "fishhawk_dispatch_stage",
				Params:       map[string]string{"run_id": run.ID, "stage": "implement"},
				Precondition: "the plan gate is approved and the working tree on the operator host is clean (git status first); the implement stage can file a mid-stage scope amendment that a blocking fishhawk_run_stage cannot decide in-band (#1189), so dispatch is the default",
				Consumes:     consumesNone,
				Reason:       "dispatch returns the durable (run_id, stage_id) handle immediately so the SINGLE MCP session stays free to fishhawk_decide_scope_amendment between polls; poll fishhawk_get_run_status to terminal (a dispatch that never sees an amendment behaves identically to blocking, ADR-037)",
			},
			{
				Action:       "fishhawk_run_stage",
				Params:       map[string]string{"run_id": run.ID, "stage": "implement"},
				Precondition: "the plan gate is approved and the working tree is clean; explicit opt-in to BLOCK to terminal — the compact one-shot for when a mid-stage amendment is impossible",
				Consumes:     consumesNone,
				Reason:       "blocks to terminal and returns the full events list, diff_summary, and next_actions in one call — choose this only when no in-band amendment decision is needed",
			},
		}
	}
	if stageType == "acceptance" {
		// The acceptance stage (E31.9) also defaults to the non-blocking
		// fishhawk_dispatch_stage: it validates the change against a running
		// preview/target instance and runs long, so the operator wants the
		// session free while it executes. fishhawk_run_stage stays the blocking
		// opt-in. The acceptance stage files no scope amendments, but the
		// long-run + free-session rationale is the same one that makes dispatch
		// the implement default (#1247).
		return []SuggestedAction{
			{
				Action:       "fishhawk_dispatch_stage",
				Params:       map[string]string{"run_id": run.ID, "stage": "acceptance"},
				Precondition: "the implement review is settled and the customer-provisioned preview/target instance the acceptance stage validates against is up; the working tree on the operator host is clean (git status first). Acceptance runs long against the running instance, so dispatch (non-blocking) is the default",
				Consumes:     consumesNone,
				Reason:       "dispatch returns the durable (run_id, stage_id) handle immediately and polls to terminal; the validator drives the preview and ships a verdict — a FAILED verdict leaves the stage succeeded and routes through deterministic server-side triage (ADR-049 decision #2), so read the acceptance_outcome_recorded entry rather than inferring from stage state",
			},
			{
				Action:       "fishhawk_run_stage",
				Params:       map[string]string{"run_id": run.ID, "stage": "acceptance"},
				Precondition: "the preview/target instance is up and the working tree is clean; explicit opt-in to BLOCK the session to terminal",
				Consumes:     consumesNone,
				Reason:       "blocks to terminal and returns the full events list in one call — choose this only when you do not need the session free while acceptance runs",
			},
		}
	}
	reason := fmt.Sprintf("the %s stage is waiting for the operator host to dispatch it (runner_kind local)", stageType)
	precondition := "the run's runner_kind is local and the stage has not started"
	return []SuggestedAction{{
		Action:       "fishhawk_run_stage",
		Params:       map[string]string{"run_id": run.ID, "stage": stageType},
		Precondition: precondition,
		Consumes:     consumesNone,
		Reason:       reason,
	}}
}

// acceptanceStageNextActions is the acceptance-stage arm of the classifier
// (E31.9 / ADR-049). Reached from implementStageNextActions' settled path when
// the workflow declares an acceptance stage — it gates the merge. A failed
// acceptance VERDICT leaves the STAGE 'succeeded' (backend run/acceptance.go),
// so the arm reads the acceptance_outcome_recorded verdict + the
// acceptance_triage_decided disposition (passed as acceptanceVerdict /
// acceptanceTriageDisposition), NEVER the stage state, to decide the move.
//
// skippedOutOfScope is the recent-audit-window flag (E38.3 / #1877): a succeeded
// verdict-less acceptance stage that carries the out-of-scope skip marker is a
// legitimate merge-eligible disposition, so it returns the merge ritual instead
// of the futile retry-reopen read arm. A recorded verdict (passed/failed) always
// wins over the flag; a marker aged out of the window (flag false) degrades to
// the read-first outcome-unknown arm (fail toward read, never toward merge).
//
// The verdict switch has four arms, and the two non-pass, non-fail ones are
// mutually exclusive BY CONSTRUCTION rather than by convention: not_validated
// (#2347) is settled PRE-SPAWN from the plan, so no criteria rows can exist,
// while undecidable (#2512) is derived POST-RUN from rows the agent actually
// reported. Both are merge-eligible and neither is a pass.
//
// arbitrated is the E66.37 / #2474 flag: an operator recorded an
// acceptance_triage_arbitrated discharge BOUND by outcome_sequence to the newest
// recorded verdict, so the server gate reads acceptance_arbitrated and the merge
// verb will succeed. It is checked on the FAILED arm before the paged branch,
// because an arbitrated failure is merge-eligible while an un-arbitrated one is
// still the human-arbitration park.
func acceptanceStageNextActions(run *Run, acceptance *Stage, skippedOutOfScope, arbitrated bool, verdict, disposition string) *NextActions {
	// A non-terminal acceptance stage dispatches (local) or polls
	// (github_actions), mirroring the plan/implement pending arms. A
	// running stage is validating against the preview — poll.
	if !stageStateIsTerminal(acceptance.State) {
		if acceptance.State == "running" {
			return &NextActions{
				State: "acceptance_running",
				Actions: []SuggestedAction{pollAction(run,
					derivedStageWaitPollInterval(run, acceptance),
					"the acceptance stage is validating the change against the running preview — re-poll until acceptance_stage_wait_status goes terminal, then read the acceptance_outcome_recorded verdict")},
			}
		}
		if acceptance.State == "dispatched" {
			// A spawn attempt exists (#1912) — a runner is in flight. Poll rather
			// than offering a dispatch that would double-drive the stage.
			return &NextActions{
				State: "acceptance_dispatched",
				Actions: []SuggestedAction{pollAction(run,
					derivedStageWaitPollInterval(run, acceptance),
					"the acceptance stage is dispatched — a spawn attempt exists (#1912) and a runner is in flight; re-poll until acceptance_stage_wait_status goes terminal")},
			}
		}
		// pending | awaiting_host_dispatch (#1912): the operator host dispatches.
		return &NextActions{State: "acceptance_pending", Actions: dispatchOrPollActions(run, "acceptance", acceptance)}
	}

	// E38.3 / #1877: a succeeded verdict-less acceptance stage that carries the
	// out-of-scope skip marker was auto-terminated because the approved plan
	// declared verification.out_of_scope with zero acceptance_criteria — a
	// legitimate terminal disposition equivalent to a recorded outcome, so the
	// run is MERGE-ELIGIBLE. Return the merge ritual (not the futile
	// retry-reopen arm; the server also 422s a direct retry for the skip). This
	// is checked BEFORE the outcome-unknown defensive arm so the skip disposition
	// routes to merge. Graceful degradation: when the marker has aged out of the
	// recent-audit window the flag is false and the arm falls through to the
	// read-first outcome-unknown arm below (fail toward read, never toward merge).
	if acceptance.State == "succeeded" && verdict == "" && skippedOutOfScope {
		return &NextActions{State: "acceptance_skipped_out_of_scope", Actions: mergeRitualActions(run,
			"the acceptance stage was auto-terminated because the approved plan declared verification.out_of_scope with no acceptance_criteria (E38.3 / #1877) — no verdict was recorded by design, and the run is merge-eligible")}
	}

	// A terminal acceptance stage that never recorded a verdict in the recent
	// window (verdict==""; the default audit_limit is 5, so the entry can age
	// out), or a stage that failed/cancelled its own execution, falls to the
	// defensive read arm — deliberately NEVER the merge ritual (fail toward
	// read, not toward merge).
	if acceptance.State != "succeeded" || verdict == "" {
		return acceptanceOutcomeUnknownActions(run, acceptance)
	}

	switch verdict {
	case acceptanceVerdictPassed:
		// ADR-049 decision #6: the merge is gated on the acceptance_passed
		// evidence condition. The stage passed — the PR is the next surface.
		return &NextActions{State: "acceptance_passed", Actions: mergeRitualActions(run,
			"the acceptance stage passed (ADR-049 decision #6: the merge is gated on the acceptance_passed evidence condition)")}
	case acceptanceVerdictNotValidated:
		// #2347: the pre-spawn short-circuit settled the stage having verified
		// ZERO criteria — every criterion was skip_expected with an
		// expectation_basis, or the plan declared none at all. No runner spawned,
		// no preview came up, nothing was observed. The run is MERGE-ELIGIBLE (a
		// change with no live target must not be stranded), so this returns the
		// merge ritual — but the state string and this reason are what stop the
		// outcome reading as a certification it is not.
		//
		// The acknowledgement ask is DELIBERATELY a prompt, not a gate: enforcing
		// it would mean text-matching operator prose to decide whether a merge may
		// proceed, and stranding a run over verdict wording is a worse failure
		// than the dishonest word this change removes. The reason text below is
		// therefore the only mechanism carrying that ask — next_actions_test.go
		// pins its two load-bearing claims (zero criteria verified; say so in the
		// merge verdict) so a refactor cannot silently drop them.
		return &NextActions{State: "acceptance_not_validated", Actions: mergeRitualActions(run,
			"the acceptance stage verified ZERO acceptance criteria — it was short-circuited with no runner and no preview because every criterion was skip-expected with a basis, or the plan declared none (#2347). The run is merge-eligible, but this is NOT a validated pass: acknowledge in your merge verdict that acceptance validated nothing")}
	case acceptanceVerdictUndecidable:
		// #2512: the acceptance stage RAN — a runner spawned, the preview came up,
		// the agent drove it — and reported per-criterion rows of which at least
		// one it could not DECIDE, with no row failing. The backend's precedence
		// ladder derives this verdict from the rows; a producer cannot ship it.
		//
		// This is the OTHER half of the partition from not_validated above, and
		// the two are mutually exclusive by construction: not_validated is settled
		// PRE-SPAWN from the plan and can carry no criteria rows at all, while
		// undecidable is decided POST-RUN from the agent's own evidence. It is
		// also deliberately not the failed arm: an undecidable row is not a
		// defect, so there is nothing to fix up, retry, or arbitrate — which is
		// exactly the #2474 wedge-surface reduction #2512 delivers. Before it, a
		// validator that could not decide a criterion had to ship `failed`, page a
		// human, and wait for an arbitration.
		//
		// The acknowledgement ask is a PROMPT, not a gate, for the same reason it
		// is on the not_validated arm: text-matching operator prose to decide
		// whether a merge may proceed would strand runs over wording. This reason
		// text is the only mechanism carrying the ask, so next_actions_test.go
		// pins its load-bearing claims.
		return &NextActions{State: "acceptance_undecidable", Actions: mergeRitualActions(run,
			"the acceptance stage RAN but could not DECIDE one or more acceptance criteria (#2512) — read the undecidable_reason on each row in the acceptance artifact. No criterion FAILED, so this is not a triage and there is nothing to arbitrate; the run is merge-eligible. But it is NOT a validated pass: acknowledge in your merge verdict which criteria went undecided and why you are merging anyway")}
	case acceptanceVerdictFailed:
		// E66.37 / #2474: an operator arbitration bound to THIS verdict discharges
		// the paged triage, so the server gate reads acceptance_arbitrated and the
		// merge verb admits the run. Checked BEFORE the paged branch — otherwise a
		// discharged run would keep being offered the arbitration it already has.
		// The correlation rule (payload outcome_sequence EQUALITY against the
		// newest verdict, in acceptanceArbitratedIn) is the SAME rule the server
		// gate applies, so this surface can never offer a merge the server 409s.
		if arbitrated {
			return &NextActions{State: "acceptance_arbitrated", Actions: mergeRitualActions(run,
				"the acceptance verdict FAILED and its triage paged, but an operator recorded an acceptance_triage_arbitrated discharge bound to that verdict (E66.37 / #2474) — the merge gate admits the run. This is NOT a validated pass: say in your merge verdict that you are merging on an arbitrated acceptance failure")}
		}
		if isAcceptancePagedDisposition(disposition) {
			return &NextActions{State: "acceptance_triage_paged", Actions: acceptanceTriagePagedActions(run)}
		}
		if disposition == "" {
			// #2512: a recorded `failed` verdict carrying NO triage disposition.
			// The severity-monotone ladder made this state REACHABLE BY DESIGN: a
			// body the agent shipped as `passed` that carries a `failed` criterion
			// row records `failed` and gates the merge at acceptanceGateTriage, but
			// handleShipAcceptance deliberately keys triage on the agent's OWN
			// failed claim (`acc.Verdict == failed && downgradedVerdict == ""`), so
			// no classifier runs and no acceptance_triage_decided entry is written
			// — there is no failure_mode for the deterministic classifier to read.
			// The server-side comment names operator arbitration as the intended
			// route, so this arm must surface it: routing here to the rerouting
			// POLL below would tell the operator to wait for a re-opened stage that
			// will never appear, leaving the merge gate undischargeable.
			//
			// It also subsumes the pre-existing TRANSIENT reading of an empty
			// disposition (triage decided but its audit entry has not landed in, or
			// has aged out of, the recent window): the arm leads with the
			// acceptance_triage_decided audit read, which answers both cases, and
			// keeps a poll as the last action for the genuinely-transient one.
			return &NextActions{State: "acceptance_triage_no_disposition",
				Actions: acceptanceTriageNoDispositionActions(run, acceptance)}
		}
		// fixup_dispatched / retry_dispatched: the deterministic server-side
		// triage (E31.8) re-opens the implement stage (class 1) or the acceptance
		// stage (class 2), so on the NEXT snapshot the existing implement_pending
		// / acceptance_pending stage-state arms serve the move — nothing to
		// duplicate here. Poll until the re-opened stage surfaces.
		return &NextActions{
			State: "acceptance_triage_rerouting",
			Actions: []SuggestedAction{pollAction(run,
				derivedStageWaitPollInterval(run, acceptance),
				"the acceptance verdict failed and deterministic server-side triage auto-routed it (fixup_dispatched re-opens implement; retry_dispatched re-opens acceptance) — re-poll; the re-opened stage's dispatch arm serves the next move. On the local runner an auto-routed re-open never spawns the runner, so fishhawk_dispatch_stage the re-opened implement (after fixup_dispatched) or acceptance (after retry_dispatched) stage")},
		}
	default:
		return acceptanceOutcomeUnknownActions(run, acceptance)
	}
}

// acceptanceOutcomeUnknownActions is the defensive read arm for a settled
// acceptance stage whose verdict is not visible in the recent-audit window (it
// aged out, or the payload was malformed). It points at the full audit trail
// and, load-bearing, NEVER offers the merge ritual — an unknown acceptance
// outcome must fail toward read, not toward merge (E31.9). It also offers the
// fishhawk_retry_stage recovery verb keyed to the acceptance stage id (#1567):
// when the audit confirms NO acceptance_outcome_recorded entry exists for the
// stage, the reopen lands it in pending so the acceptance_pending arm's
// fishhawk_dispatch_stage serves the actual re-run.
func acceptanceOutcomeUnknownActions(run *Run, acceptance *Stage) *NextActions {
	return &NextActions{
		State: "acceptance_settled_outcome_unknown",
		Actions: []SuggestedAction{
			{
				Action:       "fishhawk_list_audit",
				Params:       map[string]string{"run_id": run.ID, "category": "acceptance_outcome_recorded"},
				Precondition: "the acceptance stage settled but no acceptance_outcome_recorded verdict is visible in the recent-audit window (the default audit_limit is 5 — the entry can age out)",
				Consumes:     consumesNone,
				Reason:       "read the acceptance verdict + triage disposition from the full audit trail before acting — deliberately NOT the merge ritual (fail toward read, not toward merge)",
			},
			{
				Action:       "fishhawk_retry_stage",
				Params:       map[string]string{"stage_id": acceptance.ID},
				Precondition: "ONLY after fishhawk_list_audit confirms NO acceptance_outcome_recorded entry exists for this acceptance stage (the stage settled succeeded but shipped no verdict — the run-f7a4b71b hole). The arm also fires when the verdict merely aged out of the recent-audit window; the server re-checks and 422s retry_not_applicable if a verdict IS recorded",
				Consumes:     consumesNone,
				Reason:       "re-open the settled-outcome-unknown acceptance stage for a re-run (operator token only): the reopen lands the stage in pending, so on the local runner the acceptance_pending arm's fishhawk_dispatch_stage then spawns the actual re-run",
			},
			pollAction(run, derivedStageWaitPollInterval(run, acceptance),
				"re-poll fishhawk_get_run_status with a larger audit_limit to surface the acceptance_outcome_recorded / acceptance_triage_decided entries"),
		},
	}
}

// acceptanceTriagePagedActions is the human-arbitration arm for a failed
// acceptance verdict whose deterministic triage disposition landed on the
// human (paged / rerun_budget_exhausted / *_unavailable_paged / unsettled_paged
// / externally_unvalidatable_paged — ADR-049 decision #2, #1671). It leads with
// reading the evidence, then the operator arbitrates: a manual fix-up pass,
// accept-and-ship, or cancel.
func acceptanceTriagePagedActions(run *Run) []SuggestedAction {
	return []SuggestedAction{
		{
			Action:       "fishhawk_list_audit",
			Params:       map[string]string{"run_id": run.ID, "category": "acceptance_triage_decided"},
			Precondition: "a failed acceptance verdict landed on a paged triage disposition (paged / rerun_budget_exhausted / *_unavailable_paged / unsettled_paged / externally_unvalidatable_paged) — the human arbitrates. Read the acceptance_outcome_recorded criteria results and the acceptance_triage_decided class + reason first",
			Consumes:     consumesNone,
			Reason:       "the deterministic triage classified the failure as page-the-human (class 3/4, an exhausted re-run budget, or an unavailable fix-up/retry route); read the evidence before arbitrating",
		},
		{
			Action:       "fishhawk_fixup_stage",
			Params:       map[string]string{"run_id": run.ID, "concern_ids": "run.concerns.items[].id"},
			Precondition: "you judge the failure is a real, fixable code defect worth a manual fix-up pass (stay on a clean default branch — the runner owns the run branch in its lineage worktree)",
			Consumes:     consumesFixupBudget,
			Reason:       "route the acceptance failure back to the implement agent as a manual fix-up pass — consumes the shared fix-up budget the auto-triage also draws on",
		},
		{
			Action:       "fishhawk_arbitrate_acceptance",
			Params:       map[string]string{"run_id": run.ID, "reason": "<why the change is acceptable despite the failed acceptance verdict>"},
			Precondition: "you judge the change is acceptable despite the failed verdict (a bad/ambiguous criterion, or a criterion no sandbox-reachable target can validate) — the PAGED triage is discharged by this audited operator declaration, not by leaving the loop and hand-merging. A verdict carrying genuinely FAILED criteria additionally needs acknowledge_failed_criteria:true",
			Consumes:     consumesNone,
			Reason:       "record the arbitration that discharges this paged acceptance triage (E66.37 / #2474): it writes an acceptance_triage_arbitrated audit entry bound to this verdict's sequence and flips the merge gate to acceptance_arbitrated, so fishhawk_merge_run then works and your merge verdict stays on the chain. Do this FIRST — the merge verb 409s acceptance_gate_not_passed until it lands",
		},
		{
			Action:       "merge_and_file_follow_up",
			Params:       prParams(run),
			Precondition: "ONLY after fishhawk_arbitrate_acceptance has recorded the discharge for this verdict — until then fishhawk_merge_run refuses with 409 acceptance_gate_not_passed. Use when you judge the failure is a bad/ambiguous acceptance criterion (class 3) or otherwise works-as-planned",
			Consumes:     consumesNone,
			Reason:       "accept the change despite the failed acceptance verdict (e.g. a class-3 bad criterion): approve + merge the PR through the ordinary merge verb and file a follow-up issue for the disputed criterion",
		},
		{
			Action:       "fishhawk_cancel_run",
			Params:       map[string]string{"run_id": run.ID},
			Precondition: "the change should not ship and no fix-up is warranted",
			Consumes:     consumesNone,
			Reason:       "cancel the run — the acceptance failure is neither fixable in-loop nor acceptable",
		},
	}
}

// acceptanceTriageNoDispositionActions is the arm for a recorded `failed`
// acceptance verdict that carries NO triage disposition (#2512). It reuses the
// paged arbitration menu — which is the intended route per handleShipAcceptance
// — with the leading audit-read action's precondition/reason rewritten to
// describe the no-disposition case honestly, and a poll appended for the
// transient reading (a disposition that simply has not landed in, or has aged
// out of, the recent-audit window). The arbitration action is preserved
// verbatim: it is the operator's only way to discharge acceptanceGateTriage for
// this outcome, and dropping it would strand the merge.
func acceptanceTriageNoDispositionActions(run *Run, acceptance *Stage) []SuggestedAction {
	actions := acceptanceTriagePagedActions(run)
	if len(actions) > 0 && actions[0].Action == "fishhawk_list_audit" {
		actions[0].Precondition = "a failed acceptance verdict is recorded but NO acceptance_triage_decided disposition is visible: either the deterministic triage never ran (the agent shipped `passed` and a criterion row carried `failed`, so the severity ladder recorded `failed` with no failure_mode to classify — #2512), or its entry has not landed in / has aged out of the recent-audit window"
		actions[0].Reason = "read the acceptance_outcome_recorded criteria results and confirm whether ANY acceptance_triage_decided entry exists for this outcome before acting — no disposition means no auto-route is coming, and the merge gate stays at acceptance_triage until an operator arbitration discharges it"
	}
	return append(actions, pollAction(run, derivedStageWaitPollInterval(run, acceptance),
		"re-poll with a larger audit_limit in case a triage disposition was decided and merely aged out of the recent-audit window — if none ever appears, arbitrate"))
}

// mergeRitualActions is the ordered operator merge ritual for a run whose
// PR is open and safe to act on (E48.7 / #1954): approve the PR under your
// own GitHub identity (an unchanged gh step), then fishhawk_merge_run — the
// one verb that records the operator merge verdict, queues the squash merge,
// awaits the terminal run state, and surfaces the post-merge dev-host step.
// This replaces the bare merge_pr + post_merge ritual steps; the post-merge
// walk is folded into fishhawk_merge_run's own surfaced next_action.
func mergeRitualActions(run *Run, why string) []SuggestedAction {
	return []SuggestedAction{
		{
			Action:       "approve_pr",
			Params:       prParams(run),
			Precondition: "the implement review is terminal and the diff is reviewed",
			Consumes:     consumesNone,
			Reason:       why + " — record an operator verdict (gh pr review --approve) under your own GitHub identity before merging",
		},
		mergeRunAction(run),
	}
}

// mergeRunAction is the fishhawk_merge_run SuggestedAction (E48.7 / #1954):
// the one verb from verdict to merged+terminal. Reused by mergeRitualActions
// and the drive-folded merge_pr translation (driveAction) so the awaiting-merge
// states name the same verb whether classified or drive-derived. The verdict
// param is a placeholder the operator fills with the merge decision.
func mergeRunAction(run *Run) SuggestedAction {
	return SuggestedAction{
		Action:       "fishhawk_merge_run",
		Params:       map[string]string{"run_id": run.ID, "verdict": "<operator merge verdict>"},
		Precondition: "the PR is approved (gh pr review --approve under your own identity) and the required fishhawk_audit_complete check is green — queueing before approval is also safe (GitHub fires the merge once branch protection is satisfied)",
		Consumes:     consumesNone,
		Reason:       "records your operator merge verdict as a chained audit entry, queues the squash merge through the same seam drive_run's may_merge arm uses, awaits the terminal run state, and surfaces the post-merge dev-host step — one verb replacing the merge_pr + post_merge hand ceremony",
	}
}

// postMergeStep is the single source of truth for the operator post-merge
// dev-host SuggestedAction, reused by mergeRitualActions and the
// succeeded_merged arm (#1370). The rebuild/reload of the dev host stays
// an operator/deploy concern (ADR-038 #925) even once the lifecycle owns
// the merge tail, so this step survives in the succeeded_merged state
// after approve_pr/merge_pr drop away.
func postMergeStep(_ *Run) SuggestedAction {
	return SuggestedAction{
		Action:       "post_merge",
		Params:       nil,
		Precondition: "the PR is merged",
		Consumes:     consumesNone,
		Reason:       "scripts/dev post-merge pulls main, prunes the merged branch, and reloads the stack",
	}
}

// mergeObservedIn reports whether the recent-audit slice carries a
// post_merge_observed entry (#1370) — the backend lifecycle signal that
// the run's PR merge resolved. getRunStatus computes this off the `recent`
// slice it already fetches and threads it into nextActionsFor to gate the
// succeeded_merged state.
func mergeObservedIn(recent []AuditEntry) bool {
	for _, e := range recent {
		if e.Category == "post_merge_observed" {
			return true
		}
	}
	return false
}

// acceptanceSkippedOutOfScopeIn reports whether the recent-audit slice carries
// an acceptance_skipped_out_of_scope entry (E38.3 / #1657) — the orchestrator's
// marker that it auto-terminated a degenerate acceptance stage. getRunStatus
// computes this off the same `recent` slice it already fetches (the
// mergeObservedIn idiom) and threads it into nextActionsFor to relabel the
// succeeded_acceptance_skipped_out_of_scope state; a marker aged out of the
// window degrades to plain succeeded_pr_open (still merge-eligible).
func acceptanceSkippedOutOfScopeIn(recent []AuditEntry) bool {
	for _, e := range recent {
		if e.Category == auditCategoryAcceptanceSkippedOutOfScope {
			return true
		}
	}
	return false
}

// acceptanceArbitratedIn reports whether the recent-audit slice carries an
// acceptance_triage_arbitrated entry that discharges the run's NEWEST acceptance
// verdict (E66.37 / #2474).
//
// BINDING APPROVAL CONDITION 3 — ONE CORRELATION RULE, NOT TWO. This is
// deliberately NOT the ordering idiom latestAcceptanceTriageDisposition uses
// ("an entry at or below the newest verdict belongs to an older attempt"). The
// authoritative server gate (server/acceptance.go acceptanceArbitrationDischarges)
// correlates on payload outcome_sequence EQUALITY against the newest recorded
// outcome's audit sequence, and an ordering rule DISAGREES with it on exactly
// the interleaving the write path permits: an arbitration appended AFTER a newer
// verdict but NAMING an older outcome_sequence is newer by position yet stale by
// binding. Under an ordering rule this surface would offer a merge the server
// then 409s — the self-contradiction #2474 documents as its second-worst
// symptom. So this function applies the SAME equality rule: find the newest
// acceptance_outcome_recorded entry by SEQUENCE, then require an arbitration
// whose payload outcome_sequence EQUALS it.
//
// Returns false when no verdict is present, no arbitration names it, or any
// payload is malformed — the safe direction, since a false negative lands the
// operator in the paged-arbitration arm (which offers the arbitration verb)
// rather than offering a merge the gate refuses.
func acceptanceArbitratedIn(recent []AuditEntry) bool {
	newestOutcome := int64(0)
	haveOutcome := false
	for _, e := range recent {
		if e.Category != auditCategoryAcceptanceOutcomeRecorded {
			continue
		}
		if !haveOutcome || e.Sequence > newestOutcome {
			newestOutcome, haveOutcome = e.Sequence, true
		}
	}
	if !haveOutcome {
		return false
	}
	for _, e := range recent {
		if e.Category != auditCategoryAcceptanceTriageArbitrated {
			continue
		}
		if seq, ok := acceptancePayloadInt64(e.Payload, acceptanceArbitrationOutcomeSequenceField); ok && seq == newestOutcome {
			return true
		}
	}
	return false
}

// acceptancePayloadInt64 reads an integral field from a decoded-JSON audit
// payload. AuditEntry.Payload is `any` (a map[string]any after JSON decode), so
// a JSON number arrives as float64; a json.Number is accepted too in case a
// future decoder is configured for it. Any non-object payload, missing field,
// non-numeric value, or non-integral float yields ok=false — a malformed
// arbitration entry can never discharge a triage.
func acceptancePayloadInt64(payload any, field string) (int64, bool) {
	m, ok := payload.(map[string]any)
	if !ok {
		return 0, false
	}
	switch v := m[field].(type) {
	case float64:
		n := int64(v)
		if float64(n) != v {
			return 0, false
		}
		return n, true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// unclassifiedNextActions is the labeled fallback for any non-terminal
// state the table does not match: re-poll (always legal) plus a pointer
// to file a product issue naming the state so the table gains an arm.
func unclassifiedNextActions(run *Run, stages []Stage) *NextActions {
	shape := make([]string, 0, len(stages))
	for _, s := range stages {
		shape = append(shape, s.Type+"="+s.State)
	}
	desc := fmt.Sprintf("run state %q with stages [%s]", run.State, strings.Join(shape, ", "))
	return &NextActions{
		State: "unclassified",
		Actions: []SuggestedAction{
			// Derived from whichever stage is still live (E48.62 / #2489). The
			// arm has no ONE stage by definition, but an unclassified run is
			// still making progress somewhere, and advertising a flat 30s here
			// while every classified arm widens would be the visible
			// contradiction this parity pass exists to remove. Falls back to
			// the floor when every stage is terminal (firstNonTerminalStage
			// returns nil).
			pollAction(run, derivedStageWaitPollInterval(run, firstNonTerminalStage(stages)),
				desc+" did not match the next-actions state table — re-poll while the run settles"),
			// The ONE filing verb (E32.11 / #1737): the bare
			// file_product_issue ritual step is retired onto the real
			// fishhawk_report_product_issue tool, the same consolidation
			// merge_pr -> fishhawk_merge_run made (E48.7 / #1954).
			productIssueAction(run.ID,
				"the next-actions classifier has no arm for "+desc+", so the table itself is the defect — file a Fishhawk issue naming the state so it gains one"),
		},
	}
}

// driveAction converts the drive read view's distilled next step
// (#1023) into a SuggestedAction so it folds in as the FIRST entry on
// drive-enabled runs — drive and next_actions never point different ways.
//
// The server-stamped merge_pr next_action for the drive-derived
// awaiting_merge state is TRANSLATED at the MCP layer to fishhawk_merge_run
// (E48.7 / #1954) so the awaiting-merge surface names the one merge verb —
// without changing the persisted drive-audit vocabulary (the backend still
// records action:merge_pr; the translation lives only here). The drive
// detail and PR URL carry through so the folded-first entry stays anchored
// on the run's PR.
func driveAction(run *Run, da *RunNextAction) SuggestedAction {
	reason := da.Detail
	if reason == "" {
		reason = "the drive engine's most recent auto-advance distilled this as the operator next step"
	}
	params := map[string]string{"run_id": run.ID}
	if da.PRURL != "" {
		params["pr_url"] = da.PRURL
	}
	if da.Action == driveNextActionMergePR {
		params["verdict"] = "<operator merge verdict>"
		return SuggestedAction{
			Action:       "fishhawk_merge_run",
			Params:       params,
			Precondition: "drive-mode (#1023): the backend parked the run at awaiting_merge; read the accompanying reason/detail for any outstanding advisory rejects or unresolved concerns before merging (#2487)",
			Consumes:     consumesNone,
			Reason:       reason,
		}
	}
	return SuggestedAction{
		Action:       da.Action,
		Params:       params,
		Precondition: "drive-mode (#1023): the backend parked the run on this operator step",
		Consumes:     consumesNone,
		Reason:       reason,
	}
}

// driveNextActionMergePR is the server-stamped drive next_action verb for the
// awaiting_merge state (backend/internal/server/autodrive.go). The MCP layer
// translates it to fishhawk_merge_run in driveAction; the literal is copied
// (not imported) per the thin local-copy rule (import direction is cli →
// backend, not the reverse).
const driveNextActionMergePR = "merge_pr"

// pollAction is the re-poll entry: always legal (read-only), carrying
// the advertised cadence the wait contract suggests for the state.
func pollAction(run *Run, intervalSeconds int, reason string) SuggestedAction {
	return SuggestedAction{
		Action:       "fishhawk_get_run_status",
		Params:       map[string]string{"run_id": run.ID, "poll_interval_seconds": fmt.Sprintf("%d", intervalSeconds)},
		Precondition: "always legal (read-only)",
		Consumes:     consumesNone,
		Reason:       reason,
	}
}

// prParams names the PR an action refers to, when the run carries one.
func prParams(run *Run) map[string]string {
	if run.PullRequestURL == nil || *run.PullRequestURL == "" {
		return nil
	}
	return map[string]string{"pr_url": *run.PullRequestURL}
}

// firstNonTerminalStage returns the first stage that can still make progress,
// or nil when every stage is terminal. Used by the unclassified fallback arm
// (E48.62 / #2489), which has no single stage of its own but should still
// advertise a cadence derived from whatever is actually still running.
func firstNonTerminalStage(stages []Stage) *Stage {
	for i := range stages {
		if !stageStateIsTerminal(stages[i].State) {
			return &stages[i]
		}
	}
	return nil
}

// stageByType returns the first stage of the given type, or nil.
func stageByType(stages []Stage, stageType string) *Stage {
	for i := range stages {
		if stages[i].Type == stageType {
			return &stages[i]
		}
	}
	return nil
}

// sliceIntegrationConflictReasonPrefix is the stable prefix the fan-in
// step stamps on the parent implement stage's failure reason (ADR-041 /
// #1142). The next_actions arm keys on it to recognize the conflict
// state; the machine resume target is sourced from the structured
// slice_integration_conflict audit payload, not parsed from this string.
// MUST match orchestrator.sliceIntegrationConflictReasonPrefix
// (backend/internal/orchestrator/orchestrator.go:1855).
//
// SOURCED FROM the failure-signature registry (#1703) rather than declared
// locally, for the same single-source reason as flakeTraceEvents above.
const sliceIntegrationConflictReasonPrefix = failuresig.AnchorSliceIntegrationConflict

// Acceptance audit categories + verdict/disposition vocabulary (E31.9 /
// ADR-049). These strings are the cross-module seam between the backend, which
// WRITES the acceptance_outcome_recorded / acceptance_triage_decided audit
// payloads, and this classifier, which READS them. fishhawk-mcp deliberately
// does NOT import backend/internal/server (the #875 compile trap; same idiom as
// sliceIntegrationConflictReasonPrefix above), so the literals are copied
// verbatim and pinned by the literal-table test. A backend rename that is not
// mirrored here lands the failure in the labeled defensive
// acceptance_settled_outcome_unknown / acceptance_triage_rerouting arms (safe:
// read, never merge), not a wrong action.
//
// MUST match backend/internal/server/acceptance.go: the CategoryAcceptance*
// consts (lines ~42/46/56) and the acceptanceVerdict* / acceptanceDisposition*
// consts (lines ~77-89).
const (
	auditCategoryAcceptanceOutcomeRecorded = "acceptance_outcome_recorded"
	auditCategoryAcceptanceTriageDecided   = "acceptance_triage_decided"
	// auditCategoryAcceptanceSkippedOutOfScope is the marker the orchestrator
	// emits when it auto-terminates a degenerate acceptance stage (E38.3 /
	// #1657). MUST match backend/internal/server.CategoryAcceptanceSkippedOutOfScope.
	auditCategoryAcceptanceSkippedOutOfScope = "acceptance_skipped_out_of_scope"
	// auditCategoryAcceptanceTriageArbitrated is the operator-only discharge of a
	// PAGED acceptance triage (E66.37 / #2474). MUST match
	// backend/internal/server.CategoryAcceptanceTriageArbitrated.
	auditCategoryAcceptanceTriageArbitrated = "acceptance_triage_arbitrated"

	// acceptanceArbitrationOutcomeSequenceField is the arbitration payload field
	// carrying the acceptance_outcome_recorded sequence the discharge BINDS to.
	// It is the write→read seam between the backend endpoint that writes it
	// (server/acceptance_arbitration.go) and acceptanceArbitratedIn, which keys
	// the whole correlation on it.
	acceptanceArbitrationOutcomeSequenceField = "outcome_sequence"

	acceptanceVerdictPassed = "passed"
	acceptanceVerdictFailed = "failed"
	// acceptanceVerdictNotValidated is the SERVER-INTERNAL third verdict (#2347)
	// the orchestrator's pre-spawn short-circuit records for an acceptance stage
	// that verified ZERO criteria — no runner, no preview, no observation. It
	// never arrives over the wire (the ship endpoint still admits passed/failed
	// only), so this classifier only ever sees it on a short-circuited run. MUST
	// match backend/internal/plan.AcceptanceVerdictNotValidated (mirrored, not
	// imported — the #875 compile trap) and is pinned by the literal-table test.
	acceptanceVerdictNotValidated = "not_validated"
	// acceptanceVerdictUndecidable is the SERVER-DERIVED fourth verdict (#2512):
	// the acceptance stage RAN, drove the preview, and reported per-criterion
	// rows of which at least one could not be DECIDED, with no row failing. Like
	// not_validated it never arrives over the wire — the ship endpoint still
	// admits passed/failed only, and the backend's precedence ladder derives this
	// from the rows — so a producer cannot forge it. MUST match
	// backend/internal/plan.AcceptanceVerdictUndecidable (mirrored, not imported
	// — the #875 compile trap) and is pinned by the literal-table test.
	acceptanceVerdictUndecidable = "undecidable"

	// Auto-routed dispositions (a state transition fired): NOT paged.
	acceptanceDispositionFixupDispatched = "fixup_dispatched"
	acceptanceDispositionRetryDispatched = "retry_dispatched"

	// Paged-family dispositions (no transition — the human arbitrates).
	acceptanceDispositionPaged            = "paged"
	acceptanceDispositionRerunBudget      = "rerun_budget_exhausted"
	acceptanceDispositionFixupUnavailable = "fixup_unavailable_paged"
	acceptanceDispositionRetryUnavailable = "retry_unavailable_paged"
	acceptanceDispositionUnsettled        = "unsettled_paged"
	// acceptanceDispositionUnvalidatable is the class-5 all-skip
	// externally-unvalidatable terminal page (#1671): the acceptance stage stays
	// succeeded (no re-open), so it is a paged-family disposition the human
	// arbitrates via the acceptance_triage_paged arm. MUST match
	// backend/internal/server.acceptanceDispositionUnvalidatable.
	acceptanceDispositionUnvalidatable = "externally_unvalidatable_paged"
)

// isAcceptancePagedDisposition reports whether a triage disposition is a
// page-the-human variant (ADR-049 decision #2). The two auto-routed
// dispositions (fixup_dispatched / retry_dispatched) return false — they fired
// a state transition and the re-opened stage's own arm serves the next move.
// The class-5 externally_unvalidatable_paged disposition (#1671) returns true:
// it is terminal (no re-open), so the human arbitrates via the paged arm.
func isAcceptancePagedDisposition(d string) bool {
	switch d {
	case acceptanceDispositionPaged,
		acceptanceDispositionRerunBudget,
		acceptanceDispositionFixupUnavailable,
		acceptanceDispositionRetryUnavailable,
		acceptanceDispositionUnsettled,
		acceptanceDispositionUnvalidatable:
		return true
	default:
		return false
	}
}

// latestAcceptanceVerdict returns the verdict on the newest
// acceptance_outcome_recorded audit entry in the recent slice (time-descending,
// item 0 newest — the same slice mergeObservedIn scans), or "" when none is
// present or the payload is malformed.
func latestAcceptanceVerdict(recent []AuditEntry) string {
	for _, e := range recent {
		if e.Category == auditCategoryAcceptanceOutcomeRecorded {
			return acceptancePayloadString(e.Payload, "verdict")
		}
	}
	return ""
}

// latestAcceptanceTriageDisposition returns the triage disposition CORRELATED
// with the newest acceptance_outcome_recorded verdict — NOT merely the newest
// acceptance_triage_decided entry. The backend WRITES the triage decision AFTER
// the outcome it triages, so for a given attempt the triage entry sits ABOVE
// (newer than / a lower index than) its verdict in the time-descending recent
// slice. This function therefore finds the newest verdict entry and returns the
// newest triage disposition that is strictly NEWER than it (index <
// verdictIdx); a triage entry at or below the newest verdict belongs to an
// OLDER acceptance attempt and is deliberately ignored.
//
// This correlation is load-bearing: with multiple acceptance attempts in the
// recent window, a fresh failed verdict whose triage decision has not landed
// yet would otherwise inherit the STALE disposition of an earlier failure —
// surfacing acceptance_triage_paged / acceptance_triage_rerouting off the wrong
// attempt. Refusing the stale entry makes acceptanceStageNextActions fall to
// the poll/read arm (empty disposition on a failed verdict → rerouting) until
// the matching triage entry appears. Returns "" when no verdict is present
// (the classifier is in its defensive read arm anyway), no correlated triage
// exists yet, or the payload is malformed.
func latestAcceptanceTriageDisposition(recent []AuditEntry) string {
	verdictIdx := -1
	for i, e := range recent {
		if e.Category == auditCategoryAcceptanceOutcomeRecorded {
			verdictIdx = i
			break
		}
	}
	if verdictIdx < 0 {
		return ""
	}
	for i := 0; i < verdictIdx; i++ {
		if recent[i].Category == auditCategoryAcceptanceTriageDecided {
			return acceptancePayloadString(recent[i].Payload, "disposition")
		}
	}
	return ""
}

// acceptancePayloadString reads a string field from a decoded-JSON audit
// payload. AuditEntry.Payload is `any` (a map[string]any after JSON decode);
// any non-object payload or missing/non-string field yields "" — a malformed
// payload never panics and lands the caller in the defensive arm.
func acceptancePayloadString(payload any, field string) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[field].(string)
	return s
}

// externalAPIReasonPhrase is the stable prefix the runner's claudecode
// adapter embeds in a terminal-external-API-error failure reason
// ("terminal external API error <N> (retries exhausted): …"). next_actions
// parses the integer that follows it to name the status code in the retry
// hint. It is a best-effort string contract — no backend Stage-field
// plumbing — mirroring the citedFlakeEvent discipline (see flakeTraceEvents).
// SOURCED FROM the failure-signature registry (#1703), the single backend-side
// declaration of these literals.
const externalAPIReasonPhrase = failuresig.AnchorExternalAPIError

// citedExternalAPIStatus best-effort parses the status code following the
// stable externalAPIReasonPhrase in the stage's failure reason, returning
// (status, true) when a terminal external-API error is cited and (0, false)
// otherwise. Nil-safe and fail-soft: a nil reason, an absent phrase, or a
// non-integer following token all yield (0, false), so a plain category-A
// failure keeps its generic retry hint.
func citedExternalAPIStatus(s *Stage) (int, bool) {
	if s == nil || s.FailureReason == nil {
		return 0, false
	}
	idx := strings.Index(*s.FailureReason, externalAPIReasonPhrase)
	if idx < 0 {
		return 0, false
	}
	rest := (*s.FailureReason)[idx+len(externalAPIReasonPhrase):]
	// Consume the leading run of digits — the status code — stopping at the
	// first non-digit (a space before "(retries exhausted)").
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	status, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return status, true
}

// quotaUnavailableReasonPhrase is the stable phrase the runner's claudecode
// adapter embeds in a model-quota-exhaustion failure reason ("could not
// obtain model quota (likely a usage/rate cap): …", #2085). next_actions
// substring-matches it to give the operator a quota-aware retry hint (wait
// for the cap to reset rather than burning retry budget). It is a best-effort
// string contract — no backend Stage-field plumbing — mirroring the
// externalAPIReasonPhrase / citedFlakeEvent discipline. The runner and backend
// are separate go.work modules and cannot share the constant, so the runner
// emits this exact prefix and the backend reads it (same #1548 limitation).
// SOURCED FROM the failure-signature registry (#1703), the single backend-side
// declaration of these literals.
const quotaUnavailableReasonPhrase = failuresig.AnchorQuotaUnavailable

// citedQuotaUnavailable reports whether the stage's failure reason cites the
// runner's model-quota-exhaustion phrase. Nil-safe and fail-soft: a nil stage
// or nil reason yields false, so a plain category-A failure keeps its generic
// retry hint.
func citedQuotaUnavailable(s *Stage) bool {
	if s == nil || s.FailureReason == nil {
		return false
	}
	return strings.Contains(*s.FailureReason, quotaUnavailableReasonPhrase)
}

// citedFlakeEvent returns the known flake trace-event name the stage's
// failure reason cites, or "" when none does.
func citedFlakeEvent(s *Stage) string {
	if s.FailureReason == nil {
		return ""
	}
	for _, ev := range flakeTraceEvents {
		if strings.Contains(*s.FailureReason, ev) {
			return ev
		}
	}
	return ""
}

// reviewVerdictSummary renders a short reviewer-verdict suffix for the
// plan-gate approve action, e.g. " — reviews settled: agent approve,
// agent approve_with_concerns(2)". Empty when no verdicts are recorded.
func reviewVerdictSummary(rs *ReviewStatus) string {
	if rs == nil || len(rs.Reviews) == 0 {
		return ""
	}
	parts := make([]string, 0, len(rs.Reviews))
	for _, rev := range rs.Reviews {
		p := rev.ReviewerKind + " " + rev.Verdict
		if n := len(rev.Concerns); n > 0 {
			p += fmt.Sprintf("(%d concern(s))", n)
		}
		parts = append(parts, p)
	}
	return " — reviews settled: " + strings.Join(parts, ", ")
}

// campaignNextActionsFor (E25.8 / #1447) is the campaign arm of the
// next-actions classifier: a pure function mapping a campaign's
// server-computed next_action (computeCampaignNextAction, server/campaigns.go)
// onto a legal MCP operator action. It mirrors EXACTLY the backend's closed
// action set — attention | resume | start_run | attend_human_led | wait |
// complete | closed — so a future backend-added action value lands in the
// labeled campaign_unclassified fallback rather than crashing.
// fishhawk_get_campaign_status embeds the result
// in its output so the operator-agent never reads an unclassified campaign
// state.
//
// Structural invariant (the "never unclassified" done-means): this NEVER
// returns an empty actions list for a non-complete campaign. Only the terminal
// "complete" arm returns nil actions; every other arm — including the
// unknown-action fallback — carries at least one entry, the same structural
// guarantee nextActionsFor upholds for runs.
//
// items is the campaign's item list, used ONLY to resolve the stuck item's run
// id for the operator-gated filing suggestion on the attention/closed arms
// (E32.11 / #1737). Nil/empty is legal and simply yields no filing suggestion.
func campaignNextActionsFor(rollup CampaignRollup, na CampaignNextAction, items []CampaignItem) *NextActions {
	switch na.Action {
	case "attention":
		// A campaign item failed and is GENUINELY STUCK (#1838): its dependencies
		// are unsatisfied or it is human-led, so computeCampaignNextAction left it
		// in the Failed slice rather than diverting it to Restartable (a
		// deps-satisfied, non-human-led failed item surfaces as start_run instead,
		// via fishhawk_start_campaign_item_run). There is no auto-restart forward
		// path here — point at fishhawk_get_run_status on the failed item's run so
		// the operator can resolve it manually. NOTE: a stuck failed item no longer
		// blocks dispatch of still-actionable siblings — start_run/resume outrank
		// this arm, so attention fires only when nothing else is dispatchable.
		attention := &NextActions{
			State: "campaign_attention",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_get_run_status",
				Params:       map[string]string{"issue_ref": na.IssueRef},
				Precondition: "a campaign item failed and cannot be auto-restarted (deps-unsatisfied or human-led — a deps-satisfied failed item is surfaced as start_run instead); resolve the failed run on that item (fishhawk_list_runs / fishhawk_get_run_status) first",
				Consumes:     consumesNone,
				Reason:       "campaign item " + na.IssueRef + " failed and is not auto-restartable — read its run and resolve it manually; still-actionable siblings are surfaced first, so this fires only when no eligible/restartable/paused work remains",
			}},
		}
		foldCampaignProductIssueSuggestion(rollup, na, items, attention)
		return attention
	case "resume":
		// The auto-driver paged a human at a run gate (E25.7) and the campaign
		// (or an item) is paused. Hand it back with fishhawk_resume_campaign
		// once the gate is handled.
		return &NextActions{
			State: "campaign_paused",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_resume_campaign",
				Precondition: "the auto-driver paged a human at a run gate and the campaign (or an item) is paused; handle the gate first",
				Consumes:     consumesNone,
				Reason:       "paused item " + na.IssueRef + " was handed off at a run gate — once you have handled the gate, fishhawk_resume_campaign flips the campaign and every paused item back to running so the driver re-engages",
			}},
		}
	case "start_run":
		// A dispatchable campaign item, surfaced by the server as start_run for
		// BOTH an ELIGIBLE item (deps satisfied, no run yet) and a RESTARTABLE
		// item — a deps-satisfied, non-human-led CANCELLED (#1729) or FAILED
		// (#1838) item the operator can restart. The two need DIFFERENT verbs, so
		// the arm splits on the rollup: restartable items are folded into the wire
		// cancelled slice (toCampaignRollupPayload appends Restartable onto
		// Cancelled), so an item in rollup.Cancelled is the restart path and one in
		// rollup.Eligible is a fresh start.
		//
		// A restartable item MUST use fishhawk_start_campaign_item_run — the ONLY
		// verb that reaches the restart handler (handleStartCampaignItemRun), which
		// resets the item to pending and mints a fresh, re-linked run. The generic
		// fishhawk_start_run never restarts a failed/cancelled item, so the #1838
		// failed-item recovery path depends on the campaign-item verb here. A fresh
		// ELIGIBLE item keeps the established fishhawk_start_run (pinned by
		// campaign_test.go): there is no item to restart, so a plain run on the
		// issue ref advances the campaign.
		if slices.Contains(rollup.Cancelled, na.IssueRef) {
			return &NextActions{
				State: "campaign_start_run",
				Actions: []SuggestedAction{{
					Action:       "fishhawk_start_campaign_item_run",
					Params:       map[string]string{"issue_ref": na.IssueRef},
					Precondition: "this campaign item is a deps-satisfied, non-human-led cancelled/failed item flagged restartable (folded into the wire cancelled slice); pass the campaign_id and workflow_id fishhawk_start_campaign_item_run requires",
					Consumes:     consumesNewRun,
					Reason:       "restart campaign item " + na.IssueRef + " — fishhawk_start_campaign_item_run resets the deps-satisfied cancelled/failed item and mints a fresh, re-linked run through the restart handler (#1729/#1838) so its dependents no longer stay blocked; the generic fishhawk_start_run would neither restart nor link it",
				}},
			}
		}
		return &NextActions{
			State: "campaign_start_run",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_start_run",
				Params:       map[string]string{"trigger_ref": na.IssueRef},
				Precondition: "this campaign item is eligible — its dependencies are all satisfied and it has no run yet (rollup eligible)",
				Consumes:     consumesNewRun,
				Reason:       "dispatch campaign item " + na.IssueRef + " — start a fresh run on its issue ref to advance the campaign",
			}},
		}
	case "attend_human_led":
		// The only deps-satisfied item(s) remaining are autonomy:low (human-led):
		// the methodology reserves this tier for human leadership, so the operator
		// must pick it up by hand — do NOT mint an agent run. This arm fires only
		// when no autonomous item is eligible (start_run wins otherwise), so
		// surfacing it never stalls DAG-independent autonomous work.
		return &NextActions{
			State: "campaign_attend_human_led",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_get_campaign_status",
				Precondition: "the only deps-satisfied campaign item is autonomy:low (human-led); it is in the rollup's human_led slice, not eligible",
				Consumes:     consumesNone,
				Reason:       "campaign item " + na.IssueRef + " is deps-satisfied but autonomy:low — a human must lead it; do NOT start an agent run. The human_led classification is re-read from the issue's autonomy:* label on every fishhawk_get_campaign_status poll (#2355): to drive this item agent-led instead, relabel the issue (e.g. autonomy:low -> autonomy:medium) and re-poll — it moves into eligible on the next poll. Otherwise handle it out-of-band, then re-poll to advance the campaign",
			}},
		}
	case "wait":
		// Items are running or blocked on a dependency; nothing to dispatch.
		// Re-poll until an item becomes eligible, paused, or failed.
		return &NextActions{
			State: "campaign_wait",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_get_campaign_status",
				Precondition: "always legal (read-only)",
				Consumes:     consumesNone,
				Reason:       "items are running or blocked on a dependency; nothing to dispatch yet — re-poll fishhawk_get_campaign_status until an item becomes eligible, pauses, or fails",
			}},
		}
	case "complete":
		// Every item reached a terminal state: the campaign is done. Terminal —
		// no actions (the block still names the state), mirroring a terminal run.
		return &NextActions{State: "campaign_complete"}
	case "closed":
		// The CAMPAIGN itself went terminal (cancelled/succeeded, or failed with
		// no restartable item) while at least one issue is still unfinished
		// (#2681). No campaign verb applies — a cancelled or succeeded campaign is
		// a verdict, and a failed one with nothing restartable has no forward path
		// inside the campaign — so point the operator at driving the stranded
		// issue STANDALONE. Unlike "complete" this arm carries an action: there IS
		// work left, just not campaign-tracked work.
		params := map[string]string{}
		if na.IssueRef != "" {
			params["trigger_ref"] = na.IssueRef
		}
		reason := "the campaign is closed (terminal) and can start no further item runs"
		if na.IssueRef != "" {
			reason = "campaign item " + na.IssueRef + " is unfinished but the campaign is closed (terminal) and can start no further item runs — drive the issue standalone with fishhawk_start_run; the campaign will NOT track its outcome"
		}
		closed := &NextActions{
			State: "campaign_closed",
			Actions: []SuggestedAction{{
				Action:       "fishhawk_start_run",
				Params:       params,
				Precondition: "the campaign is terminal and can start no further item runs, so fishhawk_start_campaign_item_run would be refused campaign_not_startable; a standalone run on the issue ref is the remaining path",
				Consumes:     consumesNewRun,
				Reason:       reason,
			}},
		}
		foldCampaignProductIssueSuggestion(rollup, na, items, closed)
		return closed
	default:
		return campaignUnclassifiedNextActions(na)
	}
}

// foldCampaignProductIssueSuggestion appends the operator-gated filing
// suggestion (E32.11 / #1737) as the LAST action on the campaign arms whose
// item is genuinely stuck — attention (a failed/cancelled item with no
// auto-restart path) and closed (the campaign went terminal with the item
// unfinished). The suggestion is pre-populated with the STUCK ITEM'S OWN run
// id, so the bundle the report carries is the wedged run's, not the campaign's.
//
// It emits NOTHING when no item matches na.IssueRef or the matched item carries
// no run id: fishhawk_report_product_issue REQUIRES a run, so an action whose
// run_id param is empty would be unusable — worse than no suggestion at all.
// THAT run-id presence check is the control TestCampaignNextActions_NoFilingWithoutRunID
// guards; delete it and the test goes red on an empty run_id param.
func foldCampaignProductIssueSuggestion(rollup CampaignRollup, na CampaignNextAction, items []CampaignItem, out *NextActions) {
	if out == nil || na.IssueRef == "" {
		return
	}
	var item *CampaignItem
	for i := range items {
		if items[i].IssueRef == na.IssueRef {
			item = &items[i]
			break
		}
	}
	if item == nil || item.RunID == "" {
		return
	}
	out.Actions = append(out.Actions, productIssueAction(item.RunID, fmt.Sprintf(
		"campaign item %s is %s with no forward path inside the campaign, and %d dependent item(s) stay blocked behind it",
		na.IssueRef, item.State, len(rollup.Blocked))))
}

// campaignUnclassifiedNextActions is the labeled fallback for any campaign
// next_action value the arm above does not recognize (a future backend-added
// action): re-poll (always legal) plus a pointer to file a product issue naming
// the action so the classifier gains an arm. It ALWAYS returns a non-empty
// actions list — the campaign analogue of unclassifiedNextActions, upholding
// the "never unclassified" invariant for a non-complete campaign.
func campaignUnclassifiedNextActions(na CampaignNextAction) *NextActions {
	desc := fmt.Sprintf("campaign next_action %q", na.Action)
	return &NextActions{
		State: "campaign_unclassified",
		Actions: []SuggestedAction{
			{
				Action:       "fishhawk_get_campaign_status",
				Precondition: "always legal (read-only)",
				Consumes:     consumesNone,
				Reason:       desc + " did not match the campaign next-actions classifier — re-poll while the campaign settles",
			},
			// Same one-verb consolidation as the run-level fallback
			// (E32.11 / #1737). No run id is resolvable from a bare
			// CampaignNextAction, so run_id carries the FIELD-PATH POINTER
			// telling the caller where to read one — the arm is the
			// classifier's own defect and must keep offering a filing move,
			// so dropping it (the empty-run-id rule the attention/closed arms
			// follow) would regress the surface.
			productIssueAction("campaign.items[].run_id",
				"the campaign classifier has no arm for "+desc+", so the classifier itself is the defect — file a Fishhawk issue naming the action so it gains one"),
		},
	}
}
