package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// categoryPlanGeneratedSurfaceRetry is the audit-log category for the chained
// entry tryGeneratedSurfaceRetry writes when it REFUSES a plan that scopes a
// canonical source without its generated derivative(s) (#3437). Like
// plan_scope_retry, the entry is BOTH the budget counter (consumeRetryBudget
// counts them atomically, #2518) and the feedback source:
// loadGeneratedSurfaceRestoration reads the newest entry's findings back into
// the re-dispatched plan prompt's binding restoration section, and
// required_scope_files enumerates the missing derivatives the corrected plan
// must carry. The payload-key contract (findings[].{trigger_path,missing_tests,
// generator,sub_plan_title}, required_scope_files) is exercised end-to-end by
// the cross-boundary seam test.
const categoryPlanGeneratedSurfaceRetry = "plan_generated_surface_retry"

// maxPlanGeneratedSurfaceRetries bounds the in-run generated-surface refusal
// budget (#3437), mirroring maxPlanScopeRetries. On the first miss the plan
// stage is REFUSED — re-opened and re-dispatched once with the missing
// derivatives fed back — and a second miss exhausts the budget, degrading to
// EXACTLY the prior advisory behaviour (the plan proceeds to review carrying
// the plan_test_sweep evidence). A refusal must never become a new way to lose
// a plan, so exhaustion parks rather than failing the run terminally. The
// budget is tracked by counting plan_generated_surface_retry audit entries.
const maxPlanGeneratedSurfaceRetries = 1

// tryGeneratedSurfaceRetry REFUSES a plan that scopes a canonical source
// without every generated derivative the generated_surface rule table
// associates with it, attempting a bounded in-run re-dispatch of the plan stage
// (#3437). It mirrors tryScopeRetry leg for leg and shares its consume-first,
// audit-first ordering. It returns true when the refusal took effect (the
// caller suppresses stage advancement, so the plan never reaches
// awaiting_approval and spends ZERO reviewer passes) and false when the caller
// should fall through UNCHANGED to today's advisory review path carrying the
// plan_test_sweep evidence.
//
// The refusal is DETERMINISTIC for the unexempted, budget-available case
// (findings are non-empty by the caller's guard, Orchestrator+AuditRepo wired,
// budget not yet spent). Three admitted-with-finding shapes are the criterion's
// stated exceptions, each emitting a NAMED log line so an admission is
// diagnosable: an exempted finding (subtracted upstream in
// evaluateGeneratedSurfaceGate, so this function is never reached with only
// exempted findings), an exhausted budget, and a nil dependency
// (Orchestrator/AuditRepo). See backend/internal/server/README.md.
//
// Preconditions (any false → return false → fall through):
//   - Orchestrator and AuditRepo are wired (needed to re-dispatch and to
//     record/count the budget).
//   - consumeRetryBudget GRANTS the refusal. It declines — and this call
//     returns false — on exhaustion OR any failure (an unreadable/unwritable
//     budget is an unbounded one, so it fails CLOSED rather than granting a
//     refusal on every corrective ship).
//
// The per-step return contract mirrors tryScopeRetry's, each leg leaving a
// DIFFERENT observable state:
//   - consumeRetryBudget decline → false. Caller's fall-through parks the stage
//     at awaiting_approval with the advisory evidence.
//   - FailStage failure → false. The entry is committed and the budget consumed,
//     but the stage never left its state, so the fall-through still parks it.
//   - RetryStage failure → false. The entry is committed AND the stage is left
//     at failed (transient category A, operator-recoverable via retry_stage).
//   - Advance failure → LOG AND RETURN TRUE. The stage is already re-opened to
//     pending, so returning false would fall through to normal advancement
//     holding a pending stage — the stranded state. The refusal stands; only
//     the auto-dispatch is missing, which on the local runner is the normal
//     case anyway (the operator re-drives with a fresh fishhawk_run_stage).
func (s *Server) tryGeneratedSurfaceRetry(r *http.Request, runID, stageID uuid.UUID, findings []TestSweepFinding) bool {
	if s.cfg.Orchestrator == nil || s.cfg.AuditRepo == nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo,
			"plan upload: generated-surface refusal declined (orchestrator/audit repo not wired); admitting plan to review",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()))
		return false
	}

	// The budget entry is BOTH the counter and the feedback source
	// (loadGeneratedSurfaceRestoration reads findings / required_scope_files
	// back). required_scope_files is the sorted union of every missing
	// derivative — the exact enumerated set the corrected plan must carry.
	stamp := func(attempt int) (json.RawMessage, error) {
		type findingPayload struct {
			TriggerPath  string   `json:"trigger_path"`
			MissingTests []string `json:"missing_tests"`
			Generator    string   `json:"generator"`
			SubPlanTitle string   `json:"sub_plan_title,omitempty"`
		}
		fs := make([]findingPayload, 0, len(findings))
		requiredSet := map[string]bool{}
		for _, f := range findings {
			fs = append(fs, findingPayload{
				TriggerPath:  f.TriggerPath,
				MissingTests: f.MissingTests,
				Generator:    f.Generator,
				SubPlanTitle: f.SubPlanTitle,
			})
			for _, mp := range f.MissingTests {
				requiredSet[mp] = true
			}
		}
		required := make([]string, 0, len(requiredSet))
		for mp := range requiredSet {
			required = append(required, mp)
		}
		sort.Strings(required)
		return json.Marshal(map[string]any{
			"run_id":               runID.String(),
			"stage_id":             stageID.String(),
			"attempt":              attempt,
			"findings":             fs,
			"required_scope_files": required,
		})
	}
	granted, attempt := s.consumeRetryBudget(r.Context(), runID, stageID, categoryPlanGeneratedSurfaceRetry, maxPlanGeneratedSurfaceRetries, stamp)
	if !granted {
		// Budget exhausted, or the check-and-consume failed. Fall through to
		// today's advisory review path (the plan carries the plan_test_sweep
		// evidence). A refusal must never become a new way to lose a plan.
		// consumeRetryBudget already emitted its own named log line.
		return false
	}

	// Re-open: running/dispatched → failed (transient A) → pending. The reason
	// names each canonical source, its missing derivative(s) and the generator,
	// CAPPED at maxSchemaValidationErrorBytes exactly as the sibling scope-retry
	// path caps its reason — a stage failure reason is surfaced back to agents
	// and operators, so it must never outgrow the prompt-injection cap.
	reasonParts := make([]string, 0, len(findings))
	for _, f := range findings {
		label := f.TriggerPath
		if f.SubPlanTitle != "" {
			label = "sub-plan \"" + f.SubPlanTitle + "\" " + f.TriggerPath
		}
		reasonParts = append(reasonParts, fmt.Sprintf(
			"canonical source %s scoped without its generated derivative(s) %s (regenerate with `%s`)",
			label, strings.Join(f.MissingTests, ", "), f.Generator))
	}
	reason := "plan_generated_surface_retry: " + strings.Join(reasonParts, "; ")
	if len(reason) > maxSchemaValidationErrorBytes {
		reason = reason[:maxSchemaValidationErrorBytes] + "...[truncated]"
	}
	if _, ferr := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureA, reason); ferr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"plan upload: transition to failed-A for generated-surface retry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", ferr.Error()))
		return false
	}
	if _, rerr := s.cfg.RunRepo.RetryStage(r.Context(), stageID, run.StageStatePending); rerr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"plan upload: re-open stage to pending for generated-surface retry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", rerr.Error()))
		return false
	}

	// Drive the orchestrator to re-dispatch the now-pending plan stage.
	// Best-effort: a failure here logs but still returns TRUE — the stage is
	// re-opened and the entry recorded, so the refusal stands. Returning false
	// would fall through to normal advancement holding a pending stage.
	if _, aerr := s.cfg.Orchestrator.Advance(r.Context(), runID); aerr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"plan upload: orchestrator advance after generated-surface retry re-open failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()))
	}

	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo,
		"plan upload: refused generated-surface scope gap, scheduled in-run generated-surface retry",
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stageID.String()),
		slog.Int("attempt", attempt),
		slog.Int("findings", len(findings)))
	return true
}

// loadGeneratedSurfaceRestoration resolves the binding restoration section for a
// plan stage re-dispatched after a generated-surface refusal (#3437). It reads
// the newest plan_generated_surface_retry entry whose StageID matches, decodes
// its findings into prompt.GeneratedSurfaceRefusal rows, and attaches the
// refused plan itself (loadApprovedPlanForRun returns the newest plan artifact
// for the run, which on the corrective re-dispatch IS the refused plan — the
// ship stores the artifact before the gates run) so the planner re-emits it with
// the derivatives added rather than replanning blank-slate.
//
// nil (prompt renders as today) on: nil AuditRepo, an audit read error, an
// undecodable payload, or no matching entry. The refused plan is nil-safe: if
// loadApprovedPlanForRun returns nil the section renders without it and still
// names the missing derivatives.
func (s *Server) loadGeneratedSurfaceRestoration(ctx context.Context, runID, stageID uuid.UUID) *prompt.GeneratedSurfaceRestoration {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, categoryPlanGeneratedSurfaceRetry)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list plan_generated_surface_retry for restoration failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].StageID != nil && *entries[i].StageID != stageID {
			continue
		}
		var payload struct {
			Findings []struct {
				TriggerPath  string   `json:"trigger_path"`
				MissingTests []string `json:"missing_tests"`
				Generator    string   `json:"generator"`
				SubPlanTitle string   `json:"sub_plan_title"`
			} `json:"findings"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: undecodable plan_generated_surface_retry payload",
				slog.String("run_id", runID.String()),
				slog.String("error", err.Error()))
			return nil
		}
		if len(payload.Findings) == 0 {
			continue
		}
		rows := make([]prompt.GeneratedSurfaceRefusal, 0, len(payload.Findings))
		for _, f := range payload.Findings {
			rows = append(rows, prompt.GeneratedSurfaceRefusal{
				TriggerPath:  f.TriggerPath,
				Missing:      f.MissingTests,
				Generator:    f.Generator,
				SubPlanTitle: f.SubPlanTitle,
			})
		}
		restoration := &prompt.GeneratedSurfaceRestoration{Findings: rows}
		// The refused plan is the newest plan artifact on the corrective
		// re-dispatch (stored before the gates run). Nil-safe: a nil/error
		// result leaves RefusedPlan nil and the section still names the
		// derivatives.
		if p, perr := s.loadApprovedPlanForRun(ctx, runID); perr == nil && p != nil {
			if raw, merr := json.MarshalIndent(p, "", "  "); merr == nil {
				str := string(raw)
				restoration.RefusedPlan = &str
			}
		}
		return restoration
	}
	return nil
}
