package server

import (
	"context"
	"encoding/json"
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
// Advisory-only and fail-open: it guards only on AuditRepo (unlike the
// sibling gates it needs no RunRepo/workflow spec/GitHub client — Warnings
// depends only on the parsed plan itself), and a plan.Parse error or an
// audit-append failure WARN-logs and continues rather than blocking or
// unwinding the upload. It never transitions or fails the plan stage.
//
// Returns the computed payload so a future caller can thread it into the
// plan-review prompt's gate-evidence section (not wired in this slice);
// nil when Warnings() found nothing to report or on any fail-open path.
func (s *Server) runPlanWarnings(ctx context.Context, runID, stageID uuid.UUID, planBody []byte) *PlanWarningsPayload {
	if s.cfg.AuditRepo == nil {
		return nil
	}

	// Validation already passed in handleShipPlan; a parse failure here is
	// an internal inconsistency — log and skip rather than block.
	parsedPlan, err := plan.Parse(planBody)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan warnings: parse plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	warnings := plan.Warnings(parsedPlan)
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
