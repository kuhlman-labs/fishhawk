package main

import (
	"context"
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
// NOTE: while maxFixupPasses == 1, budget-exhaustion suppression also
// covers the resolved-after-fix-up case (a fresh review only re-runs via a
// fix-up pass, which spends the single budget unit). If maxFixupPasses is
// ever raised > 1, concern-counting must be scoped to the latest review
// round; see #777.
type ReviewActionHint struct {
	Concerns             int    `json:"concerns" jsonschema:"number of unresolved approve_with_concerns concerns from the implement review (summed across reviewers)"`
	RemainingFixupBudget int    `json:"remaining_fixup_budget" jsonschema:"remaining bounded fix-up passes for the implement stage (max_passes minus prior stage_fixup_triggered entries)"`
	Message              string `json:"message" jsonschema:"one-line advisory pointer at fishhawk_fixup_stage (route concerns back to the agent) vs approving to merge; display-only, never gates the run"`
}

// reviewActionHintFor computes the display-only review-action hint for a run's
// implement stage from audit data (#777). It returns nil (no hint) when:
//
//   - the implement review is not complete (status != "complete"): no landed
//     verdict to act on yet;
//   - the completed review carries zero approve_with_concerns concerns:
//     nothing to route back;
//   - the bounded fix-up budget is spent (RemainingFixupBudget <= 0): a
//     fix-up pass already consumed the budget, which (while maxFixupPasses
//     == 1) also covers the resolved-after-fix-up case.
//
// implementStatus is the caller's already-computed *ReviewStatus for the
// implement stage so the hint and the adjacent ImplementReviewStatus field
// derive from one audit read and cannot disagree within a single response
// (getRunStatus passes the value it already resolved; run_stage queries
// reviewStatusFor itself and passes the result).
func (r *runResolver) reviewActionHintFor(ctx context.Context, runID, implementStageID uuid.UUID, implementStatus *ReviewStatus) (*ReviewActionHint, error) {
	if implementStatus == nil || implementStatus.Status != "complete" {
		return nil, nil
	}

	// Sum concerns across every approve_with_concerns verdict, flattened —
	// the same shape server/fixup.go's resolveImplementConcerns addresses.
	concerns := 0
	for _, v := range implementStatus.Reviews {
		if v.Verdict != "approve_with_concerns" {
			continue
		}
		concerns += len(v.Concerns)
	}
	if concerns == 0 {
		return nil, nil
	}

	priorPasses, err := r.countFixupPasses(ctx, runID, implementStageID)
	if err != nil {
		return nil, err
	}
	remaining := maxFixupPasses - priorPasses
	if remaining <= 0 {
		// Budget exhausted — suppress. While maxFixupPasses == 1 this also
		// covers the post-fix-up resolved case.
		return nil, nil
	}

	return &ReviewActionHint{
		Concerns:             concerns,
		RemainingFixupBudget: remaining,
		Message: fmt.Sprintf(
			"%d concern(s) from the implement review — route them back with fishhawk_fixup_stage(stage_id=%s, concern indices), or approve to merge. Remaining fix-up budget: %d.",
			concerns, implementStageID, remaining),
	}, nil
}

// countFixupPasses returns the number of prior stage_fixup_triggered audit
// entries recorded for the implement stage — the durable fix-up-pass counter
// the bound is enforced against. Mirrors server.countFixupPasses: the
// stage_id filter scopes the query, and the per-entry StageID match guards
// against a fix-up on a DIFFERENT stage being counted against this one (the
// same client-side double-check the backend does, robust even when an audit
// backend does not filter by stage_id).
func (r *runResolver) countFixupPasses(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryStageFixupTriggered,
		StageID:  stageID.String(),
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	want := stageID.String()
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == want {
			n++
		}
	}
	return n, nil
}
