package server

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
)

// CategoryStageConflictResolutionTriggered is the audit-log category for the
// entry the rebase-branch handler writes when a CONFLICTING base merge triggers
// an operator-authorized agent pass that resolves the conflict ON the run branch
// (E64.62 / #3202), instead of the outright 422 rebase_conflict that forced the
// operator to resolve-push-vouch onto a branch ADR-035 declares runner-owned.
//
// This entry IS the durable record of the pass budget: the trigger counts prior
// entries of this category for the stage against a ceiling of 1. It is
// DELIBERATELY DISTINCT from CategoryStageFixupTriggered — the fix-up counter is
// never read and never written on this path, which is what makes "a
// conflict-resolution pass consumes no fix-up budget" structurally true rather
// than merely asserted.
const CategoryStageConflictResolutionTriggered = "stage_conflict_resolution_triggered"

// CategoryStageConflictResolutionFailed is the audit-log category written when a
// conflict-resolution pass refused or errored (E64.62 / #3202). The runner
// reports the NAMED refusal reason its confinement gate returned, so the record
// says which rule the agent's resolution broke rather than only that the pass
// failed.
//
// It is also the budget-SPENT marker: the trigger reader treats a trigger whose
// pass failed as CONSUMED, so the next fishhawk_rebase_run_branch invocation
// takes the fail-closed 422 rebase_conflict arm naming the failed pass and
// today's resolve-push-vouch route. A pass is an assist, never an escalation —
// the failure arm restores the pre-pass review gate rather than leaving the run
// terminal-failed.
const CategoryStageConflictResolutionFailed = "stage_conflict_resolution_failed"

// conflictResolutionTrigger is the payload of a
// stage_conflict_resolution_triggered audit entry: everything the prompt server
// needs to instruct the runner, plus the pass ordinal for the record.
//
// It is the SINGLE source of the four conflict-resolution wire fields the prompt
// response serves, so the runner's local merge targets exactly the base the
// trigger authorized — not whatever the run row happens to say when the prompt
// is fetched.
type conflictResolutionTrigger struct {
	// Branch is the run branch the merge lands on. The runner checks it out
	// at ExpectedHeadSHA and refuses if the tip has moved.
	Branch string `json:"branch"`
	// BaseRef is the base BRANCH name (not a remote-qualified ref); the
	// runner qualifies it with its own remote.
	BaseRef string `json:"base_ref"`
	// ExpectedHeadSHA is the run's recorded head at trigger time — the same
	// ADR-035 lineage source FixupExpectedHeadSHA resolves from.
	ExpectedHeadSHA string `json:"expected_head_sha"`
	// Pass is the 1-based pass ordinal. Ceiling 1 today; carried so a future
	// ceiling raise needs no payload change.
	Pass int `json:"pass,omitempty"`
}

// resolveConflictResolutionTrigger returns the NEWEST
// stage_conflict_resolution_triggered payload bound to the given stage, and
// whether one exists.
//
// Newest wins for the same reason the fix-up resolvers scan newest-first: a
// trigger re-opens the stage to pending, so the prompt must reflect the most
// recent authorization. A payload missing the branch or the base ref is treated
// as ABSENT rather than served half-populated — a runner handed an empty base
// ref would merge nothing and the pass would look like a no-conflict abort.
//
// Returns (zero, false) when the AuditRepo is unconfigured, the stage carries no
// trigger (the common, non-conflict case), or on any error — best-effort, the
// same WARN-and-proceed posture as the other prompt resolvers.
func (s *Server) resolveConflictResolutionTrigger(ctx context.Context, runID, stageID uuid.UUID) (conflictResolutionTrigger, bool) {
	var zero conflictResolutionTrigger
	if s.cfg.AuditRepo == nil {
		return zero, false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageConflictResolutionTriggered)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list stage_conflict_resolution_triggered audit failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return zero, false
	}
	// ListForRunByCategory returns entries ordered ASC by ts; scan newest-first.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload conflictResolutionTrigger
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: unmarshal stage_conflict_resolution_triggered payload failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()),
			)
			continue
		}
		if payload.Branch == "" || payload.BaseRef == "" {
			continue
		}
		return payload, true
	}
	return zero, false
}
