package main

import (
	"context"
	"encoding/json"
	"fmt"

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
	RemainingFixupBudget int    `json:"remaining_fixup_budget" jsonschema:"remaining NORMAL fix-up passes for the implement stage (max_passes minus prior stage_fixup_triggered entries); 0 once the budget is spent"`
	OverrideAvailable    bool   `json:"override_available" jsonschema:"true when the NORMAL budget is spent but an operator override pass (fishhawk_fixup_stage with force_additional_pass=true) can still be granted below the hard ceiling of 3 total passes; false below budget (no override needed) and at/above the ceiling (no override left)"`
	Message              string `json:"message" jsonschema:"one-line advisory pointer at the next action: route concerns back with fishhawk_fixup_stage vs approving to merge (below budget), the operator override vs merge-with-follow-up (budget spent, below ceiling), or merge-with-follow-up vs a fresh run (at the ceiling); display-only, never gates the run"`
}

// reviewActionHintFor computes the display-only review-action hint for a run's
// implement stage from audit data (#777, #860). It returns nil (no hint) when:
//
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
func (r *runResolver) reviewActionHintFor(ctx context.Context, runID, implementStageID uuid.UUID, implementStatus *ReviewStatus) (*ReviewActionHint, error) {
	if implementStatus == nil || implementStatus.Status != "complete" {
		return nil, nil
	}

	priorPasses, latestFixupSeq, err := r.fixupPassesAndLatestSeq(ctx, runID, implementStageID)
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

	remaining := maxFixupPasses - priorPasses
	if remaining < 0 {
		remaining = 0
	}

	// Below the normal budget: the original route-back vs approve pointer.
	if priorPasses < maxFixupPasses {
		return &ReviewActionHint{
			Concerns:             concerns,
			RemainingFixupBudget: remaining,
			OverrideAvailable:    false,
			Message: fmt.Sprintf(
				"%d concern(s) from the implement review — route them back with fishhawk_fixup_stage(stage_id=%s, concern indices), or approve to merge. Remaining fix-up budget: %d.",
				concerns, implementStageID, remaining),
		}, nil
	}

	// Budget spent but the hard ceiling still has headroom: surface the
	// exhaustion plus the two options — the bounded operator override or
	// merge-with-follow-up.
	if priorPasses < fixupCeiling {
		return &ReviewActionHint{
			Concerns:             concerns,
			RemainingFixupBudget: 0,
			OverrideAvailable:    true,
			Message: fmt.Sprintf(
				"%d concern(s) remain after the fix-up budget is spent (%d/%d normal passes used). Either merge now and file a follow-up, or grant ONE bounded override pass with fishhawk_fixup_stage(stage_id=%s, concern indices, force_additional_pass=true) — capped at %d total passes.",
				concerns, priorPasses, maxFixupPasses, implementStageID, fixupCeiling),
		}, nil
	}

	// Hard ceiling reached: no override left — merge-with-follow-up or a
	// fresh run.
	return &ReviewActionHint{
		Concerns:             concerns,
		RemainingFixupBudget: 0,
		OverrideAvailable:    false,
		Message: fmt.Sprintf(
			"%d concern(s) remain but the hard fix-up ceiling of %d total passes is reached — no override left. Merge now and file a follow-up, or start a fresh run to address them.",
			concerns, fixupCeiling),
	}, nil
}

// fixupPassesAndLatestSeq returns both the number of prior
// stage_fixup_triggered audit entries for the implement stage (the durable
// fix-up-pass counter the bound is enforced against) and the audit sequence
// of the most-recent such entry (0 when none exist — the round boundary used
// to scope the latest review round). Mirrors server.countFixupPasses' per-
// entry StageID double-check so a fix-up on a DIFFERENT stage is never counted
// against this one, robust even when an audit backend does not filter by
// stage_id.
func (r *runResolver) fixupPassesAndLatestSeq(ctx context.Context, runID, stageID uuid.UUID) (int, int64, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryStageFixupTriggered,
		StageID:  stageID.String(),
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, 0, err
	}
	n := 0
	var latestSeq int64
	want := stageID.String()
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != want {
			continue
		}
		n++
		if e.Sequence > latestSeq {
			latestSeq = e.Sequence
		}
	}
	return n, latestSeq, nil
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
