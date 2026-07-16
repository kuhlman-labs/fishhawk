package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/google/uuid"
)

// maxFixupPasses mirrors backend server.defaultMaxFixupPasses
// (backend/internal/server/fixup.go), which is unexported. KEEP IN SYNC:
// it is the bound the backend enforces on implement-review fix-up passes,
// and the MCP layer reconstructs RemainingFixupBudget from it. If the
// backend bound changes and this constant drifts, RemainingFixupBudget
// goes silently wrong — the mcp integration test guards this by asserting
// the hint suppresses after exactly one stage_fixup_triggered entry.
const maxFixupPasses = 1

// fixupCeiling mirrors backend server.defaultFixupCeiling
// (backend/internal/server/fixup.go), which is unexported. KEEP IN SYNC:
// it is the absolute hard cap on total fix-up passes per implement stage
// (normal budget + any operator-forced override, #860), and the hint uses
// it to decide whether an override pass is still available. If the backend
// ceiling changes and this constant drifts, OverrideAvailable goes silently
// wrong — the mcp integration test guards this by asserting
// fixup_ceiling_reached fires at exactly this many total passes end-to-end.
const fixupCeiling = 3

// categoryStageFixupTriggered mirrors backend server.CategoryStageFixupTriggered
// (backend/internal/server/fixup.go). KEEP IN SYNC: it is the audit-log
// category the backend writes one entry per fix-up pass under, and the MCP
// layer counts those entries to derive the remaining fix-up budget.
const categoryStageFixupTriggered = "stage_fixup_triggered"

// categoryPlanRevised mirrors backend server.CategoryPlanRevised
// (backend/internal/server/revise.go). KEEP IN SYNC: it is the durable
// audit record of a fishhawk_revise_plan re-open — the plan-stage analog of
// stage_fixup_triggered. The MCP review-status layer floors the plan stage's
// terminal-verdict reads to the latest such entry so a stale pre-revision
// plan-review verdict no longer reads as the current round's terminal state
// (#1201).
const categoryPlanRevised = "plan_revised"

// categoryFixupNoChanges mirrors the backend report category written by
// server.succeedFixupNoChangesStage (backend/internal/server/pullrequest.go)
// when a fix-up re-dispatch produces no commit. KEEP IN SYNC: it is the
// durable refund signal server.countFixupNoChangeRefunds counts (#967), and
// the MCP layer counts the same entries so RemainingFixupBudget mirrors the
// backend's widened MaxPasses (defaultMaxFixupPasses + refunds). If the
// backend category drifts and this constant does not, a refunded no-op pass
// surfaces remaining_budget=0 here while the backend would admit a normal
// pass — the surface-vs-backend disagreement #1150 fixes.
const categoryFixupNoChanges = "fixup_no_changes"

// categoryDispatchReaperFailed mirrors backend server.CategoryDispatchReaperFailed
// (backend/internal/server/reap_failure.go). KEEP IN SYNC: it is the audit
// category the spawn-phase reaper writes when a fix-up re-dispatch dies on
// infrastructure BEFORE the agent runs (the #1747 path). When such an entry
// carries failure_category "C" inside a fix-up trigger window, the backend
// refunds that pass against the NORMAL budget (server.countFixupInfraRefunds,
// #1957), and the MCP layer mirrors that count so RemainingFixupBudget agrees
// with the backend's admit decision. If the backend category drifts and this
// constant does not, an infra-refunded pass surfaces remaining_budget=0 here
// while the backend would admit a normal pass — the #968 hint-vs-backend
// disagreement class.
const categoryDispatchReaperFailed = "dispatch_reaper_failed"

// categoryStageFixupRecovered mirrors backend server.CategoryStageFixupRecovered
// (backend/internal/server/fixup.go). KEEP IN SYNC: it is the audit category
// written when a FAILED fix-up re-dispatch is recovered back to the review gate
// (#788). When such an entry carries source_failure_category "C" inside a fix-up
// trigger window, the pass burned agent work but landed NOTHING on the PR branch,
// so the backend refunds it against the NORMAL budget under the delivered-nothing
// invariant (server.countFixupInfraRefunds, #1957); the MCP layer mirrors that
// count. Drift here silently reintroduces the #968 hint-vs-backend disagreement.
const categoryStageFixupRecovered = "stage_fixup_recovered"

// failureCategoryC mirrors backend run.FailureC ("C", the infrastructure
// failure category — backend/internal/run/run.go). KEEP IN SYNC: only a
// category-C death that delivered nothing to the PR branch is refunded against
// the normal budget (#1957). Category A (agent) and B (policy) failures still
// consume budget, so the MCP mirror keeps only the "C" signals.
const failureCategoryC = "C"

// ReviewActionHint is a DISPLAY-ONLY next-action pointer surfaced on
// fishhawk_get_run_status and fishhawk_run_stage when an implement review
// has landed with unresolved approve_with_concerns concerns and the bounded
// fix-up budget is not yet spent (#777). It NEVER gates a run — like the
// periodic-budget block (#693/#759), it is advisory and computed entirely
// from audit data the MCP layer already reads. It is suppressed once the
// concerns are resolved (a fresh review with no concerns) or the fix-up
// budget is exhausted.
//
// It is NOT surfaced on fishhawk_start_run: no implement review exists at
// run start, so the field would always be empty there.
//
// Direction D (#860): when the bounded budget is SPENT but the latest
// review round still carries concerns, the hint is NO LONGER suppressed —
// it surfaces the exhaustion plus the remaining options (an operator
// override pass while below the hard ceiling, or merge-with-follow-up /
// a fresh run at the ceiling). OverrideAvailable reports whether
// force_additional_pass can still grant one more pass. The concern count is
// scoped to the LATEST review round (concerns landing after the most-recent
// stage_fixup_triggered entry) so it is not inflated across rounds. The
// concerns==0 early return still suppresses the genuinely-resolved case (a
// fresh review with no concerns).
type ReviewActionHint struct {
	Concerns             int    `json:"concerns" jsonschema:"number of unresolved approve_with_concerns concerns from the LATEST implement-review round (summed across reviewers; scoped to concerns that landed after the most-recent fix-up so the count is not inflated across rounds)"`
	RemainingFixupBudget int    `json:"remaining_fixup_budget" jsonschema:"remaining NORMAL fix-up passes for the implement stage (max_passes minus prior stage_fixup_triggered entries that were NOT refunded); 0 once the budget is spent, restored when a prior pass produced no changes (#967) OR died category-C without delivering anything to the PR branch (#1957)"`
	OverrideAvailable    bool   `json:"override_available" jsonschema:"true when the NORMAL budget is spent but an operator override pass (fishhawk_fixup_stage with force_additional_pass=true) can still be granted below the hard ceiling of 3 total passes; false below budget (no override needed) and at/above the ceiling (no override left)"`
	Message              string `json:"message" jsonschema:"one-line advisory pointer at the next action: route concerns back with fishhawk_fixup_stage vs approving to merge (below budget), the operator override vs merge-with-follow-up (budget spent, below ceiling), or merge-with-follow-up vs a fresh run (at the ceiling); display-only, never gates the run"`
}

// implementReviewMergeHint returns a display-only merge-readiness warning for
// the local loop when the implement-stage agent review has been dispatched but
// has not yet landed (#947 local-loop parity). It mirrors the backend's
// review-pending presence gate (auditcomplete rule 6): while the implement
// ReviewStatus is "pending", the required fishhawk_audit_complete check is held
// pending on the SAME condition, so the PR is not safe to merge/resolve yet —
// the check flips green automatically once the verdict lands. Returns "" (no
// hint) for any non-pending status.
//
// Display-only, NEVER gates: there is no MCP merge tool; the operator merges on
// GitHub where branch protection enforces the held check. The wording is kept
// consistent with the backend presence gate.
func implementReviewMergeHint(implementStatus *ReviewStatus) string {
	if implementStatus == nil || implementStatus.Status != "pending" {
		return ""
	}
	return "the implement-stage agent review has not landed yet — the PR is NOT safe to merge or resolve. The required fishhawk_audit_complete check is held pending on this review and flips green automatically once the verdict lands. Re-poll fishhawk_get_run_status on the advertised poll_interval_seconds until implement_review_status is terminal."
}

// suggestedActions translates the computed hint into the next_actions
// concern-arm entries (#1024). Deriving the entries FROM the hint value —
// rather than re-deriving the budget state from audit — is what keeps
// review_action_hint and next_actions agreeing by construction: the
// budget/ceiling branch conditions live once, in reviewActionHintFor, and
// this method only reads the result (RemainingFixupBudget /
// OverrideAvailable carry the branch).
func (h *ReviewActionHint) suggestedActions(run *Run, implementStageID string) []SuggestedAction {
	fixupParams := map[string]string{
		"stage_id":    implementStageID,
		"concern_ids": "run.concerns.items[].id",
	}
	// deferConcern is always legal while a concern is open and consumes NO
	// fix-up budget (#1202): it files a pre-drafted follow-up and resolves
	// the concern. It sits between routing the concern back (fix-up) and
	// accepting it as-is (merge-with-follow-up). parent_epic + n are the
	// non-derivable title coordinates the operator supplies.
	deferConcern := SuggestedAction{
		Action: "fishhawk_defer_concern",
		Params: map[string]string{
			"concern_ids": "run.concerns.items[].id",
			"parent_epic": "<epic the follow-up rolls up to, e.g. #1196>",
			"n":           "<child number for the [E<epic>.<n>] title>",
		},
		Precondition: "the concern is worth a separate change but should not block the merge",
		Consumes:     consumesNone,
		Reason:       fmt.Sprintf("%d open concern(s) — file a pre-drafted follow-up work item and resolve the concern in one call (no fix-up budget spent)", h.Concerns),
	}
	mergeWithFollowUp := SuggestedAction{
		Action:       "merge_and_file_follow_up",
		Params:       prParams(run),
		Precondition: "the remaining concerns are acceptable to land",
		Consumes:     consumesNone,
		Reason:       fmt.Sprintf("%d open concern(s) — approve the PR with an operator verdict, merge, and file a follow-up issue for what was not routed back", h.Concerns),
	}

	// Below the normal budget: route the concerns back, defer into a
	// follow-up, or approve to merge.
	if h.RemainingFixupBudget > 0 {
		return []SuggestedAction{
			{
				Action:       "fishhawk_fixup_stage",
				Params:       fixupParams,
				Precondition: "the implement stage is parked at its review gate (or succeeded with the PR open); stay on a clean default branch — the runner owns the run branch in its lineage worktree",
				Consumes:     consumesFixupBudget,
				Reason:       fmt.Sprintf("%d open concern(s) from the implement review; %d normal fix-up pass(es) remain", h.Concerns, h.RemainingFixupBudget),
			},
			deferConcern,
			mergeWithFollowUp,
		}
	}

	// Budget spent, ceiling open: merge-with-follow-up, defer into a
	// follow-up, or the bounded operator override (#860).
	if h.OverrideAvailable {
		forcedParams := map[string]string{
			"stage_id":              implementStageID,
			"concern_ids":           "run.concerns.items[].id",
			"force_additional_pass": "true",
		}
		return []SuggestedAction{
			mergeWithFollowUp,
			deferConcern,
			{
				Action:       "fishhawk_fixup_stage",
				Params:       forcedParams,
				Precondition: fmt.Sprintf("the normal fix-up budget is spent but the hard ceiling of %d total passes is not reached", fixupCeiling),
				Consumes:     consumesFixupBudget,
				Reason:       fmt.Sprintf("%d open concern(s) remain after the budget is spent; ONE bounded operator override pass is still available", h.Concerns),
			},
		}
	}

	// Hard ceiling reached: merge-with-follow-up, an operator commit-and-vouch
	// for a late CI/SAST finding (#1097), or a fresh run. The commit_and_vouch
	// arm mirrors ciFailedNextActions (next_actions.go): commit the fix on the
	// run branch then fishhawk_vouch_commit so the operator-authored commit
	// clears the run's sole-writer lineage gate (ADR-035) WITHOUT consuming a
	// fix-up pass — the sanctioned in-loop remedy past the ceiling.
	return []SuggestedAction{
		mergeWithFollowUp,
		{
			Action:       "commit_and_vouch",
			Params:       prParams(run),
			Precondition: fmt.Sprintf("the hard fix-up ceiling of %d total passes is reached and a late CI/SAST finding still needs an in-loop fix", fixupCeiling),
			Consumes:     consumesNone,
			Reason:       "commit the fix on the run branch, then fishhawk_vouch_commit (operator/operator-agent token, NOT the run's fhm_ token) so the operator-authored commit clears the run's sole-writer lineage gate without breaking ADR-035 (#1068/#1044)",
		},
		{
			Action:       "fishhawk_start_run",
			Params:       nil,
			Precondition: fmt.Sprintf("the hard fix-up ceiling of %d total passes is reached — no override left", fixupCeiling),
			Consumes:     consumesNewRun,
			Reason:       "address the remaining concerns in a fresh run instead of this one",
		},
	}
}

// reviewActionHintFor computes the display-only review-action hint for a run's
// implement stage from audit data (#777, #860). RemainingFixupBudget mirrors the
// backend's widened normal budget: it discounts the SUMMED refunds the backend
// grants — the #967 fixup_no_changes refund AND the #1957 delivered-nothing infra
// refund (a fix-up pass that died category-C without landing anything on the PR
// branch) — under the same defensive clamp, and the hard-ceiling check is hoisted
// ahead of the normal-budget arm to match the backend's error precedence
// (run.ErrFixupCeilingReached before budget exhaustion). It returns nil (no hint)
// when:
//
//   - the run state is terminal (succeeded/failed/cancelled, #968): a
//     terminal run has no actionable fix-up — the server refuses with
//     fixup_not_applicable — so advertising override_available here would
//     make the hint and the server-side applicability predicate disagree;
//   - the implement review is not complete (status != "complete"): no landed
//     verdict to act on yet;
//   - the LATEST review round carries zero approve_with_concerns concerns:
//     nothing to route back (the genuinely-resolved case — including
//     resolved-after-fix-up, since the count is round-scoped).
//
// Unlike before (#860 direction D), a SPENT fix-up budget no longer
// suppresses the hint when concerns remain: it surfaces the exhaustion and
// the remaining options (operator override below the hard ceiling, or
// merge-with-follow-up / a fresh run at it).
//
// implementStatus is the caller's already-computed *ReviewStatus for the
// implement stage so the complete/none gate and the adjacent
// ImplementReviewStatus field derive from one audit read (getRunStatus
// passes the value it already resolved; run_stage queries reviewStatusFor
// itself and passes the result). The concern COUNT, however, is recomputed
// here from a sequence-aware audit read so it can be scoped to the latest
// review round.
func (r *runResolver) reviewActionHintFor(ctx context.Context, runID, implementStageID uuid.UUID, runState string, implementStatus *ReviewStatus) (*ReviewActionHint, error) {
	if runStateIsTerminal(runState) {
		return nil, nil
	}
	if implementStatus == nil || implementStatus.Status != "complete" {
		return nil, nil
	}

	priorPasses, latestFixupSeq, triggerSeqs, err := r.fixupPassesAndLatestSeq(ctx, runID, implementStageID)
	if err != nil {
		return nil, err
	}

	// Scope the concern count to the latest review round: only concerns that
	// landed after the most-recent stage_fixup_triggered entry (with no prior
	// fix-up, latestFixupSeq is 0 and every round counts — a single round).
	concerns, err := r.latestRoundConcerns(ctx, runID, implementStageID, latestFixupSeq)
	if err != nil {
		return nil, err
	}
	if concerns == 0 {
		return nil, nil
	}

	// Refunds against the NORMAL budget: the backend widens MaxPasses by the
	// SUMMED count of two refund signals (handleFixupStage, fixup.go:461), so a
	// refunded pass is admissible WITHOUT force_additional_pass. Mirror both here
	// so the surfaced budget agrees with the backend's admit decision. The refund
	// only affects the NORMAL-budget arm; the hard ceiling keeps counting RAW
	// passes.
	//
	//   - #967/#1150: fixup_no_changes — a fix-up re-dispatch that produced no
	//     commit.
	//   - #1957: the delivered-nothing infra refund — a fix-up pass that died
	//     category-C without landing anything on the PR branch (a
	//     dispatch_reaper_failed with failure_category C, or a
	//     stage_fixup_recovered with source_failure_category C, inside a trigger
	//     window).
	noChangeRefunds, err := r.fixupNoChangeRefunds(ctx, runID, implementStageID)
	if err != nil {
		return nil, err
	}
	infraRefunds, err := r.fixupInfraRefunds(ctx, runID, implementStageID, triggerSeqs)
	if err != nil {
		return nil, err
	}
	refunds := noChangeRefunds + infraRefunds
	// Defensive clamp, mirroring the backend's `if refundedPasses > priorPasses`
	// clamp (fixup.go:462): the SUMMED refund can never exceed the passes
	// actually triggered.
	if refunds > priorPasses {
		refunds = priorPasses
	}
	// effectiveConsumed is the normal-budget consumption the backend actually
	// enforces: it admits when raw priorPasses < (maxFixupPasses + refunds),
	// algebraically priorPasses - refunds < maxFixupPasses.
	effectiveConsumed := priorPasses - refunds

	remaining := maxFixupPasses - effectiveConsumed
	if remaining < 0 {
		remaining = 0
	}

	// Hard ceiling reached: hoisted AHEAD of the normal-budget arm to match the
	// backend's error precedence — run.ErrFixupCeilingReached is checked BEFORE
	// budget exhaustion (fixup.go:555), so a raw priorPasses at the ceiling
	// refuses with fixup_ceiling_reached even when the summed refunds would
	// otherwise leave effectiveConsumed below the normal budget (the raw=3/
	// refunds=3 state the #1957 E2E drives; unreachable before the infra refund
	// because the stage-keyed no-change dedup caps refunds at 1). No override
	// left — merge-with-follow-up, a commit-and-vouch for a late CI/SAST finding
	// (#1097), or a fresh run.
	if priorPasses >= fixupCeiling {
		return &ReviewActionHint{
			Concerns:             concerns,
			RemainingFixupBudget: 0,
			OverrideAvailable:    false,
			Message: fmt.Sprintf(
				"%d concern(s) remain but the hard fix-up ceiling of %d total passes is reached — no override left. Merge now and file a follow-up; for a late CI/SAST finding, commit the fix on the run branch then fishhawk_vouch_commit it (operator/operator-agent token, NOT the run's fhm_ token) so the operator commit clears the run's sole-writer lineage gate (ADR-035, #1068/#1044); or start a fresh run to address them.",
				concerns, fixupCeiling),
		}, nil
	}

	// Below the normal budget (after refunds): the original route-back vs
	// approve pointer. A refunded no-op restores a normal route-back here, so
	// the message explains WHY budget is non-zero after a spent pass.
	if effectiveConsumed < maxFixupPasses {
		var msg string
		if refunds > 0 {
			msg = fmt.Sprintf(
				"%d concern(s) from the implement review — a prior fix-up produced no changes and was refunded against the normal budget, so route-back is available again with fishhawk_fixup_stage(stage_id=%s, concern_ids from run.concerns.items[].id; positional indices are deprecated), or approve to merge. Remaining fix-up budget: %d.",
				concerns, implementStageID, remaining)
		} else {
			msg = fmt.Sprintf(
				"%d concern(s) from the implement review — route them back with fishhawk_fixup_stage(stage_id=%s, concern_ids from run.concerns.items[].id; positional indices are deprecated), or approve to merge. Remaining fix-up budget: %d.",
				concerns, implementStageID, remaining)
		}
		return &ReviewActionHint{
			Concerns:             concerns,
			RemainingFixupBudget: remaining,
			OverrideAvailable:    false,
			Message:              msg,
		}, nil
	}

	// Budget spent but the hard ceiling still has headroom (the ceiling arm above
	// already returned for priorPasses >= fixupCeiling, so this is reached only
	// below the ceiling): surface the exhaustion plus the two options — the
	// bounded operator override or merge-with-follow-up.
	return &ReviewActionHint{
		Concerns:             concerns,
		RemainingFixupBudget: 0,
		OverrideAvailable:    true,
		Message: fmt.Sprintf(
			"%d concern(s) remain after the fix-up budget is spent (%d/%d normal passes used). Either merge now and file a follow-up, or grant ONE bounded override pass with fishhawk_fixup_stage(stage_id=%s, concern_ids from run.concerns.items[].id, force_additional_pass=true) — capped at %d total passes.",
			concerns, priorPasses, maxFixupPasses, implementStageID, fixupCeiling),
	}, nil
}

// fixupPassesAndLatestSeq returns, from ONE stage_fixup_triggered audit read:
// the number of prior fix-up passes for the implement stage (the durable
// pass counter the bound is enforced against), the audit sequence of the
// most-recent such entry (0 when none exist — the round boundary used to scope
// the latest review round), and the ascending-sorted slice of those entries'
// sequences (the trigger-window bounds fixupInfraRefunds pairs the #1957 infra
// refund signals against). Mirrors server.countFixupPasses' per-entry StageID
// double-check so a fix-up on a DIFFERENT stage is never counted against this
// one, robust even when an audit backend does not filter by stage_id. The
// collected sequences are sorted EXPLICITLY rather than assuming the endpoint
// returns them ordered.
func (r *runResolver) fixupPassesAndLatestSeq(ctx context.Context, runID, stageID uuid.UUID) (int, int64, []int64, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryStageFixupTriggered,
		StageID:  stageID.String(),
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, 0, nil, err
	}
	var latestSeq int64
	var triggerSeqs []int64
	want := stageID.String()
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != want {
			continue
		}
		triggerSeqs = append(triggerSeqs, e.Sequence)
		if e.Sequence > latestSeq {
			latestSeq = e.Sequence
		}
	}
	sort.Slice(triggerSeqs, func(i, j int) bool { return triggerSeqs[i] < triggerSeqs[j] })
	return len(triggerSeqs), latestSeq, triggerSeqs, nil
}

// fixupInfraRefunds returns the number of fix-up passes for the stage that died
// category-C (infrastructure) WITHOUT delivering anything to the PR branch, and
// are therefore refunded against the NORMAL budget alongside the #967 no-change
// refund (#1957). Mirrors server.countFixupInfraRefunds BYTE-FOR-BYTE:
//
//   - two signal shapes qualify — a dispatch_reaper_failed entry with
//     failure_category "C" (the #1747 pre-agent spawn-phase reaper death), and a
//     stage_fixup_recovered entry with source_failure_category "C" (the #788
//     post-agent-work recovery that still landed nothing on the PR branch);
//   - only category C refunds — category A (agent) and B (policy) failures still
//     consume budget, matching the delivered-nothing invariant;
//   - an unparseable/missing payload is SKIPPED (never counted);
//   - each trigger window (triggerSeqs[i], triggerSeqs[i+1]) — open-ended for the
//     newest — refunds at most once when at least one signal Sequence falls
//     STRICTLY inside it (sig > lo && sig < hi), so a signal sequenced before the
//     first trigger (an original-dispatch spawn death, not a fix-up) never
//     refunds.
//
// triggerSeqs is the ascending-sorted trigger-window bounds from
// fixupPassesAndLatestSeq (one shared stage_fixup_triggered read). The per-entry
// StageID double-check mirrors the other helpers so a signal on a DIFFERENT stage
// is never counted against this one.
func (r *runResolver) fixupInfraRefunds(ctx context.Context, runID, stageID uuid.UUID, triggerSeqs []int64) (int, error) {
	if len(triggerSeqs) == 0 {
		return 0, nil
	}
	want := stageID.String()

	// Pre-agent spawn-phase reaper deaths (failure_category "C").
	reapers, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryDispatchReaperFailed,
		StageID:  stageID.String(),
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, err
	}
	var signalSeqs []int64
	for _, e := range reapers {
		if e.StageID == nil || *e.StageID != want {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var p struct {
			FailureCategory string `json:"failure_category"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		if p.FailureCategory == failureCategoryC {
			signalSeqs = append(signalSeqs, e.Sequence)
		}
	}

	// Post-agent-work #788 recovery deaths (source_failure_category "C").
	recovered, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryStageFixupRecovered,
		StageID:  stageID.String(),
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, err
	}
	for _, e := range recovered {
		if e.StageID == nil || *e.StageID != want {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var p struct {
			SourceFailureCategory string `json:"source_failure_category"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		if p.SourceFailureCategory == failureCategoryC {
			signalSeqs = append(signalSeqs, e.Sequence)
		}
	}
	if len(signalSeqs) == 0 {
		return 0, nil
	}

	// Per-window pairing: at most one refund per trigger, regardless of how many
	// signals land in its window.
	refunds := 0
	for i, lo := range triggerSeqs {
		hi := int64(math.MaxInt64)
		if i+1 < len(triggerSeqs) {
			hi = triggerSeqs[i+1]
		}
		for _, sig := range signalSeqs {
			if sig > lo && sig < hi {
				refunds++
				break
			}
		}
	}
	return refunds, nil
}

// fixupNoChangeRefunds returns the number of fixup_no_changes audit entries
// recorded for the stage — the fix-up passes that produced no commit and are
// refunded against the NORMAL budget (#967). Mirrors
// server.countFixupNoChangeRefunds, including the per-entry StageID
// double-check (as fixupPassesAndLatestSeq does) so a refund on a DIFFERENT
// stage is never counted against this one. The backend report path's
// stage-keyed idempotency dedup admits at most one such entry per stage, so in
// practice this is 0 or 1.
func (r *runResolver) fixupNoChangeRefunds(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryFixupNoChanges,
		StageID:  stageID.String(),
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	want := stageID.String()
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != want {
			continue
		}
		n++
	}
	return n, nil
}

// latestRoundConcerns sums the approve_with_concerns concerns from
// implement_reviewed audit entries for the stage that landed AFTER afterSeq —
// the latest review round (#860). Scoping by audit sequence avoids summing
// concerns across rounds once a fix-up has run; with afterSeq == 0 (no prior
// fix-up) every entry is in the single round and all concerns count. The
// per-entry StageID double-check mirrors the fix-up counter so a different
// stage's review is never counted here.
func (r *runResolver) latestRoundConcerns(ctx context.Context, runID, stageID uuid.UUID, afterSeq int64) (int, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: "implement_reviewed",
		StageID:  stageID.String(),
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, err
	}
	want := stageID.String()
	total := 0
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != want {
			continue
		}
		if e.Sequence <= afterSeq {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var review PlanReview
		if uerr := json.Unmarshal(raw, &review); uerr != nil {
			continue
		}
		if review.Verdict != "approve_with_concerns" {
			continue
		}
		total += len(review.Concerns)
	}
	return total, nil
}
