package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// categoryPlanWarnings is the audit-log category for the entry
// runPlanWarnings writes when plan.Warnings() fires at least one advisory
// for an uploaded plan (#1684). Unlike plan_scope_precheck/
// plan_surface_sweep/plan_test_sweep, this entry is written ONLY when
// Warnings() returns a non-empty slice — a warning-free plan gets no
// entry, keeping TestShipPlan's happy-path audit-count assertion green
// (binding condition 3).
const categoryPlanWarnings = "plan_warnings"

// PlanWarningsPayload is the audit-payload shape for a plan_warnings entry
// (#1684). Warnings mirrors plan.Warnings()'s return value verbatim.
type PlanWarningsPayload struct {
	Warnings []string `json:"warnings"`
}

// runPlanWarnings evaluates an uploaded plan with plan.Warnings() —
// notably the all-empty-depends_on decomposition advisory (#1679/#1680),
// plus the pre-existing sub-plan runtime-sum and expensive-gate-vs-budget
// advisories — and, when it returns at least one warning, records an
// advisory plan_warnings audit entry (#1684). This gives plan.Warnings()
// its first production caller, closing the gap where the decomposition
// safety net computed a result nobody ever read.
//
// Advisory-only and fail-open: it guards only on AuditRepo for the
// plan.Warnings() advisories (which depend only on the parsed plan itself),
// and a plan.Parse error or an audit-append failure WARN-logs and continues
// rather than blocking or unwinding the upload. It never transitions or fails
// the plan stage.
//
// SERVER-AUTHORITATIVE over-cap advisory (#2053): it ALSO appends a
// deterministic over-cap advisory when the resolved implement-stage
// max_files_changed cap is > 0 and len(scope.files) > cap. This computation is
// the SINGLE SOURCE OF TRUTH for the over-cap condition — it reads ONLY the
// scanned file count and the resolved cap and MUST NOT read parsedPlan.OverCap
// (the planner's self-declaration hint). It is the guarantee that no monolithic
// over-cap plan silently passes the plan gate; downstream E50 slices key on it.
// Resolving the cap adds a RunRepo dependency the plan.Warnings() advisories do
// not need; that leg fails open independently (nil RunRepo, GetRun error, no
// spec/implement stage, or cap unresolvable → over-cap advisory skipped, plan
// settle never blocked), so the plan.Warnings() advisories still fire even when
// the cap cannot be resolved.
//
// Returns the computed payload so a future caller can thread it into the
// plan-review prompt's gate-evidence section (not wired in this slice); nil when
// neither plan.Warnings() nor the over-cap check found anything to report, or on
// any fail-open path.
func (s *Server) runPlanWarnings(ctx context.Context, runID, stageID uuid.UUID, planBody []byte) *PlanWarningsPayload {
	if s.cfg.AuditRepo == nil {
		return nil
	}

	// Validation already passed in handleShipPlan; decode with json.Unmarshal
	// (schema-only bytes, NOT plan.Parse) so the advisories stay independent of
	// the semanticCheck over_cap ⇒ split_proposal coupling (#2055): an over-cap
	// over_cap:true plan that omits split_proposal would fail plan.Parse, which
	// would SUPPRESS the count-derived over-cap advisory for exactly the flag
	// state #2053 requires it to fire in — reversing #2053's flag-independence
	// guarantee. Decoding here never runs semanticCheck; the authoritative gate
	// (handleShipPlan overCapSplitRejection) owns the reject. A json.Unmarshal
	// failure on schema-valid bytes is an internal inconsistency — log and skip
	// rather than block (fail-open, unchanged).
	var parsedPlan plan.Plan
	if err := json.Unmarshal(planBody, &parsedPlan); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan warnings: decode plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	warnings := plan.Warnings(&parsedPlan)

	// SERVER-AUTHORITATIVE over-cap advisory (#2053): count-derived, computed
	// independently of the planner's over_cap self-declaration. Appended before
	// the emptiness check so an otherwise warning-free over-cap plan still
	// records a plan_warnings entry that surfaces through fishhawk_get_plan.
	if w := s.overCapWarning(ctx, runID, &parsedPlan); w != "" {
		warnings = append(warnings, w)
	}

	if len(warnings) == 0 {
		return nil
	}

	result := &PlanWarningsPayload{Warnings: warnings}
	payload, _ := json.Marshal(result)
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPlanWarnings,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan warnings: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()),
		)
	}
	return result
}

// overCapWarning computes the SERVER-AUTHORITATIVE, count-derived over-cap
// advisory string for a parsed plan (#2053), or "" when the plan is not over
// cap or the cap cannot be resolved. It is the single source of truth for the
// over-cap condition: the over-cap decision reads ONLY len(scope.files) and the
// resolved implement-stage max_files_changed cap and MUST NOT consult
// parsedPlan.OverCap (the planner's self-declaration hint) — so an over-cap plan
// fires the advisory whether over_cap is omitted, false, or true, and an
// under-cap plan never fires it even when over_cap is true.
//
// Fail-open on every leg (nil RunRepo, GetRun error, no spec/implement stage, or
// an unresolved / zero cap): return "" so the over-cap advisory is skipped and
// the plan settle is never blocked. The cap resolution reuses
// resolveImplementConstraints, the same helper runScopePrecheck uses, so the
// plan-gate advisory and the scope pre-check agree on the resolved cap.
func (s *Server) overCapWarning(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) string {
	count, capLimit, over, ok := s.overCapByCount(ctx, runID, parsedPlan)
	if !ok || !over {
		return ""
	}
	return fmt.Sprintf(
		"plan scope declares %d files, exceeding the implement-stage max_files_changed cap of %d — "+
			"narrow the scope or split the work into a decomposition before approving.",
		count, capLimit,
	)
}

// overCapByCount is the shared #2053 count determination (E50.3 refactor). It
// resolves the implement-stage max_files_changed cap and compares it against
// len(parsedPlan.Scope.Files), NEVER reading parsedPlan.OverCap — the planner's
// self-declaration hint plays no part in whether a plan is over cap. It returns
// count (scanned scope.files), capLimit (the resolved cap), over (count >
// capLimit against a positive cap), and ok (the cap resolved to a positive
// value).
//
// Fail-open on every leg (nil RunRepo, GetRun error, no spec/implement stage,
// or a zero/unresolved cap): ok=false and over=false, so an unresolved cap can
// never be treated as over-cap. Both overCapWarning (advisory) and
// overCapSplitRejection (authoritative reject) route through this one function,
// so the plan-gate advisory and the count-derived reject agree on the resolved
// cap by construction — no duplicated cap logic. The cap resolution reuses
// resolveImplementConstraints, the same helper runScopePrecheck uses.
func (s *Server) overCapByCount(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) (count, capLimit int, over, ok bool) {
	count = len(parsedPlan.Scope.Files)
	if s.cfg.RunRepo == nil {
		return count, 0, false, false
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan warnings: get run for over-cap check failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return count, 0, false, false
	}
	constraints, _, resolved := s.resolveImplementConstraints(ctx, runRow)
	if !resolved || constraints.MaxFilesChanged <= 0 {
		return count, 0, false, false
	}
	return count, constraints.MaxFilesChanged, count > constraints.MaxFilesChanged, true
}

// overCapSplitRejection is the SERVER-AUTHORITATIVE, count-derived over-cap
// REJECT reason (#2055, E50.3) — the E50 keystone. It returns a clear,
// actionable reason when the plan is over cap BY COUNT (len(scope.files) >
// resolved cap) AND carries no split_proposal, and "" otherwise. Like the
// sibling advisory it reads ONLY the file count and the resolved cap via
// overCapByCount and MUST NOT consult parsedPlan.OverCap — so an over-cap-by-
// count monolith lacking a split is rejected whether over_cap is omitted,
// false, or true, and an over-cap plan that carries a split_proposal is
// accepted (structural validity of that split is the plan.Parse semanticCheck's
// job, an additional in-artifact defensive layer).
//
// Fail-open on every leg exactly like overCapWarning: overCapByCount returns
// ok=false (nil RunRepo, GetRun error, no spec/implement stage, unresolved/zero
// cap) → "" so an unresolved cap never spuriously blocks a plan. It reuses
// overCapByCount (and thus resolveImplementConstraints) so the reject and the
// advisory share one cap resolution with no duplicated logic.
func (s *Server) overCapSplitRejection(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) string {
	count, capLimit, over, ok := s.overCapByCount(ctx, runID, parsedPlan)
	if !ok || !over {
		return ""
	}
	if parsedPlan.SplitProposal != nil {
		return ""
	}
	return fmt.Sprintf(
		"plan scope declares %d files, exceeding the implement-stage max_files_changed cap of %d, "+
			"and carries no split_proposal — split the work into an expand->migrate->contract split_proposal "+
			"(each phase at or under the cap) before approving.",
		count, capLimit,
	)
}
