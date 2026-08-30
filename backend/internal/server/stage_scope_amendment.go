package server

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// stageApprovedAmendmentScopePaths returns the paths the operator APPROVED
// mid-stage — via the fishhawk_decide_scope_amendment channel — for THE STAGE
// UNDER REVIEW (#2874), as per-path records carrying the authorizing amendment
// id and the operator's decision reason.
//
// The runner folds an approved mid-stage amendment into the stage's ENFORCED
// scope, so an edit to one of these paths is authorized. The implement-review
// prompt, however, is built from the raw approved plan, so without surfacing
// them the reviewer reads a correctly-amended file as scope drift: on run
// bff9a242 both reviewers independently flagged backend/internal/audit/
// categories.go as drift forty-eight minutes after the operator approved
// amendment 6c8a2006 for exactly that path, costing two waivers and a spurious
// medium. This resolver is the review-side counterpart of the enforced fold.
//
// Contract, branch by branch:
//
//   - nil ScopeAmendmentRepo → nil, no allocation.
//   - ListByRun error → WARN log naming the run, return nil. Best-effort and
//     fail-OPEN, matching approvedAmendmentScopePaths and
//     childApprovedAmendmentScopePaths: a provenance lookup must NEVER block a
//     review. The degraded behaviour is exactly the pre-#2874 behaviour (a
//     correctly-amended path reads as drift), which is a flag-only criterion and
//     never a reject ground.
//   - StageID != stageID → skipped. This is the SAME filter the enforced fold
//     resolveApprovedScopeAmendments applies, so the returned set is provably a
//     SUBSET of what the runner permitted for this stage: it can never assert
//     in-scope a path the runner's scope gate would have rejected. A sibling
//     stage's approved amendment is never surfaced here.
//   - Status != approved → skipped. PENDING and DENIED confer nothing; a
//     request the operator refused must never read as a grant.
//   - Path already in approvedPlan.Scope.Files → skipped. writeApprovedPlan
//     already renders those, so naming one again would only restate existing
//     scope (mirrors amendedScopeFilesForReview).
//   - Duplicate path across two approved amendments → one record, FIRST-WINS,
//     so the oldest authorizing amendment is the one shown.
//
// DEDUPE ACROSS CHANNELS (#2874 approval condition 3): a path already surfaced
// by the approval-time add_scope_files fold (Trigger.AmendedScopeFiles, the
// "Scope amended at approval" section) is deliberately NOT skipped here. The two
// lists have different PROVENANCE — approval-time authorization versus a
// mid-stage request the operator answered — and each section is the authority
// for its own channel; a reader asking "who authorized this path, and when?"
// gets both answers rather than an arbitrary winner. Duplication is harmless to
// a reviewer (both sections say the same thing: in-scope, NOT drift), whereas
// suppressing one would silently hide that the agent also asked mid-stage.
//
// ListByRun's oldest-first ordering is preserved so the rendered prompt is
// stable across repeated builds. Unlike amendedScopeFilesForReview this does NOT
// early-return on an empty approvedPlan.Scope.Files: an operator-approved
// amendment is authorization in its own right regardless of how thin the
// declared scope is. A nil approvedPlan is handled by the caller
// (runImplementReviews returns early), and is tolerated here as "no raw scope to
// exclude against".
func (s *Server) stageApprovedAmendmentScopePaths(ctx context.Context, runID, stageID uuid.UUID, approvedPlan *plan.Plan) []prompt.MidStageAmendedScopePath {
	if s.cfg.ScopeAmendmentRepo == nil {
		return nil
	}
	items, err := s.cfg.ScopeAmendmentRepo.ListByRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"implement review: list scope amendments failed — mid-stage-amendment provenance contributes nothing for this run",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	inRawScope := map[string]struct{}{}
	if approvedPlan != nil {
		for _, f := range approvedPlan.Scope.Files {
			inRawScope[f.Path] = struct{}{}
		}
	}

	var out []prompt.MidStageAmendedScopePath
	seen := make(map[string]struct{})
	for _, a := range items {
		if a.StageID != stageID {
			continue // another stage's amendment was never folded into THIS stage's enforced scope
		}
		if a.Status != scopeamendment.StatusApproved {
			continue // pending / denied confer nothing
		}
		reason := ""
		if a.DecisionReason != nil {
			reason = *a.DecisionReason
		}
		for _, p := range a.Paths {
			if _, ok := inRawScope[p.Path]; ok {
				continue // already rendered by writeApprovedPlan
			}
			if _, ok := seen[p.Path]; ok {
				continue // first-wins dedup
			}
			seen[p.Path] = struct{}{}
			out = append(out, prompt.MidStageAmendedScopePath{
				Path:           p.Path,
				AmendmentID:    a.ID.String(),
				DecisionReason: reason,
			})
		}
	}
	return out
}
