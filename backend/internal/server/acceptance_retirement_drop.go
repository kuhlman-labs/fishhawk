package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// appendRetirementDropOnce is the ONE idempotent append every
// acceptance_scenario_retirement_dropped writer routes through (#3439):
// recordAcceptanceRetirementsDroppedOnCancel (acceptance_retirement_cancel.go,
// the run-cancel sinks), recordAcceptanceScenarioRetirementDropped
// (pullrequest.go, the runner's report) and recordAcceptanceRetirementsUnserved
// (prompt.go, the dispatch prompt path). p is the fully-built ChainAppendParams
// the writer already constructs (category
// CategoryAcceptanceScenarioRetirementDropped, StageID &stageID, actor, a
// payload carrying `reason`); logPrefix names the writer in the fallback WARN.
//
// The idempotency key is (stage_id, reason) — exactly what
// retirementDropAlreadyRecorded matched before this helper existed — and
// deliberately NOT cancel_source: two cancel sinks racing on one run (operator
// cancel + a budget tripwire, or the orchestrator's stage_cancelled
// resolution) are ONE drop and must collapse to one row, while the
// dispatched-race second row with a DIFFERENT reason (#3389's documented
// fail-safe direction) remains legal.
//
// Capability path: when the wired audit repository implements
// audit.DedupedChainAppender (the production Postgres repo — postgres.go's
// compile-time assertion keeps that true), the scan runs INSIDE the append
// transaction under the run-row lock, so the check-then-act window the
// pre-#3439 writers had is closed. A *DedupedDuplicateError is the benign
// already-recorded branch: (false, nil) with a DEBUG line naming the surviving
// sequence. Any other error is (false, err) for the writer's own handling.
//
// Fallback path (a non-capable repo — the in-memory fakes): the prior
// list-then-append leg, byte-preserving its posture — a list error is WARN +
// proceed ("proceeding without idempotency guard"), a duplicate is (false,
// nil), otherwise AppendChained. It is NOT atomic. The residual is stated
// plainly: the atomicity guarantee holds only for the Postgres repository.
//
// Returns (true, nil) when a row landed, so the writer decides whether to
// refresh the status comment; (false, nil) when the row already existed.
func (s *Server) appendRetirementDropOnce(ctx context.Context, runID, stageID uuid.UUID, reason, logPrefix string,
	p audit.ChainAppendParams) (bool, error) {
	if d, ok := s.cfg.AuditRepo.(audit.DedupedChainAppender); ok {
		_, err := d.AppendChainedDeduped(ctx, p, audit.DedupeSpec{
			StageID: &stageID, PayloadKey: "reason", PayloadValue: reason,
		})
		var dup *audit.DedupedDuplicateError
		if errors.As(err, &dup) {
			var existing int64
			if dup.Existing != nil {
				existing = dup.Existing.Sequence
			}
			s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
				logPrefix+": acceptance retirement drop already recorded; duplicate append rejected under the run-row lock",
				slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()),
				slog.String("reason", reason), slog.Int64("existing_sequence", existing))
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}

	s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
		logPrefix+": audit repository does not implement DedupedChainAppender; falling back to the non-atomic list-then-append",
		slog.String("run_id", runID.String()))
	if entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceScenarioRetirementDropped); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			logPrefix+": list audit entries failed; proceeding without idempotency guard",
			slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()), slog.String("error", err.Error()))
	} else if retirementDropAlreadyRecorded(entries, stageID, reason) {
		return false, nil
	}
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, p); err != nil {
		return false, err
	}
	return true, nil
}

// retirementDropAlreadyRecorded reports whether an
// acceptance_scenario_retirement_dropped entry for stageID with the same
// reason is already on the chain — the idempotency key for the drop report.
// It is the fallback leg's scan; the capability path enforces the same key
// inside audit.AppendChainedDedupedTx.
func retirementDropAlreadyRecorded(entries []*audit.Entry, stageID uuid.UUID, reason string) bool {
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.Reason == reason {
			return true
		}
	}
	return false
}
