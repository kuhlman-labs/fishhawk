package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// checkBlockingBudget is the HTTP-handler admission gate for blocking
// periodic budgets (#688 / ADR-030), the counterpart to the webhook
// dispatcher's refusedByBlockingBudget. It returns true to ADMIT the
// run and false when the run was refused (an HTTP error response is
// written and the caller must return immediately).
//
// It shares the decision core (webhook.CheckBlockingBudget) with the
// dispatcher seam. Behaviour:
//   - RunRepo/AuditRepo nil, or RunRepo doesn't implement the cost-sum
//     capability → admit (capability-absent skip; mirrors
//     checkBudgetAlerts).
//   - sum error → WARN-log and admit (fail-open).
//   - blocking budget over + override → append a
//     run_admitted_budget_override audit and admit.
//   - blocking budget over + no override → append a run_rejected_budget
//     audit, write 402 budget_exhausted, refuse.
func (s *Server) checkBlockingBudget(w http.ResponseWriter, r *http.Request, repo, workflowID string, budgets []spec.PeriodicBudget, override bool) bool {
	if len(budgets) == 0 {
		return true
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return true
	}
	summer, ok := s.cfg.RunRepo.(webhook.CostSummer)
	if !ok {
		// Capability-absent (e.g. an in-memory test fake that doesn't
		// sum cost): admit, consistent with the warn path.
		return true
	}

	now := time.Now()
	blocked, b, dec, err := webhook.CheckBlockingBudget(r.Context(), summer, repo, workflowID, budgets, now, s.cfg.BudgetLocation)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"blocking budget: sum period spend failed — admitting run (fail-open)",
			slog.String("repo", repo),
			slog.String("workflow_id", workflowID),
			slog.String("error", err.Error()))
		return true
	}
	if !blocked {
		return true
	}

	systemKind := audit.ActorKind("system")
	if override {
		// Operator force-past. Record the override decision in the
		// global chain so the bypass is auditable.
		payload, _ := json.Marshal(map[string]any{
			"workflow_id": workflowID,
			"repo":        repo,
			"period":      b.Period,
			"limit_usd":   b.LimitUSD,
			"spent":       dec.Spent,
		})
		if _, aerr := s.cfg.AuditRepo.AppendGlobalChained(r.Context(), audit.GlobalChainAppendParams{
			Timestamp: now.UTC(),
			Category:  "run_admitted_budget_override",
			ActorKind: &systemKind,
			Payload:   payload,
		}); aerr != nil {
			s.cfg.Logger.Warn("append run_admitted_budget_override audit entry failed",
				"repo", repo, "workflow_id", workflowID, "error", aerr.Error())
		}
		return true
	}

	payload, _ := json.Marshal(map[string]any{
		"reason":      "budget_exhausted",
		"workflow_id": workflowID,
		"repo":        repo,
		"period":      b.Period,
		"limit_usd":   b.LimitUSD,
		"spent":       dec.Spent,
	})
	if _, aerr := s.cfg.AuditRepo.AppendGlobalChained(r.Context(), audit.GlobalChainAppendParams{
		Timestamp: now.UTC(),
		Category:  "run_rejected_budget",
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.Warn("append run_rejected_budget audit entry failed",
			"repo", repo, "workflow_id", workflowID, "error", aerr.Error())
	}

	s.writeError(w, r, http.StatusPaymentRequired, "budget_exhausted",
		fmt.Sprintf("workflow %q has exhausted its %s cost budget for the current period "+
			"(limit $%.2f, spent $%.2f); raise limit_usd, set the budget's enforcement to advisory, "+
			"or pass budget_override to force this run", workflowID, b.Period, b.LimitUSD, dec.Spent),
		map[string]any{"workflow_id": workflowID, "period": b.Period, "limit_usd": b.LimitUSD, "spent": dec.Spent})
	return false
}
