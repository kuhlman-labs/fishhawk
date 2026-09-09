package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryStageConflictResolutionTriggered is the durable marker the rebase
// verb's 202 arm writes when it authorizes a bounded conflict-resolution pass
// (E64.62 / #3202). It is the pass's OWN budget counter: the fix-up counter
// never reads or writes it, so a conflict-resolution pass can never spend a
// fix-up slot and a fix-up can never spend the pass's ceiling-of-one.
const CategoryStageConflictResolutionTriggered = "stage_conflict_resolution_triggered"

// CategoryStageConflictResolutionFailed is written when the runner reports a
// REFUSED pass. It carries the runner's named refusal reason and CONSUMES the
// trigger it follows: a trigger at or older than the newest failure for the
// same stage is spent and is never served again. Without that consumption an
// ordinary later fix-up on the same stage would be served conflict_resolution
// =true with stale anchors and hijacked into a pass against a repository that
// is not mid-merge.
const CategoryStageConflictResolutionFailed = "stage_conflict_resolution_failed"

// conflictResolutionTrigger is the payload of a
// stage_conflict_resolution_triggered entry: everything the runner needs to
// perform the pass, plus the pre-pass gate state the failure recovery restores.
//
// Branch/BaseRef/ExpectedHeadSHA are served to the runner on the prompt
// response (the conflict_resolution_* wire fields). PriorState and
// ReparkedReviewStageID mirror the stage_fixup_triggered payload's restore
// anchors byte-for-byte, so maybeRecoverConflictResolutionFailure can reuse
// run.RestoreFixupStage rather than re-deriving the transition.
type conflictResolutionTrigger struct {
	Branch                string `json:"branch"`
	BaseRef               string `json:"base_ref"`
	ExpectedHeadSHA       string `json:"expected_head_sha"`
	Pass                  int    `json:"pass"`
	PriorState            string `json:"prior_state,omitempty"`
	ReparkedReviewStageID string `json:"reparked_review_stage_id,omitempty"`
}

// populated reports whether the trigger carries every anchor the runner needs.
// A HALF-populated trigger is treated as NO pass rather than served: a runner
// handed an empty base ref would merge nothing and refuse naming the wrong
// cause, burning the ceiling-of-one budget on a serve bug.
func (t *conflictResolutionTrigger) populated() bool {
	return t != nil && t.Branch != "" && t.BaseRef != "" && t.ExpectedHeadSHA != ""
}

// newestStageEntry returns the most-recent entry of the given category bound to
// stageID, or nil. Entries are append-ordered, so the last match wins — the
// same keep-the-last scan maybeRecoverFixupFailure uses. The bool reports
// whether the READ succeeded: a listing error must never be read as "no entry",
// because both callers treat absence as a licence to proceed.
func (s *Server) newestStageEntry(ctx context.Context, runID, stageID uuid.UUID, category string) (*audit.Entry, bool) {
	if s.cfg.AuditRepo == nil {
		return nil, false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return nil, false
	}
	var newest *audit.Entry
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			newest = e
		}
	}
	return newest, true
}

// resolveConflictResolutionTrigger returns the LIVE conflict-resolution trigger
// for a stage, or nil when there is none to serve.
//
// The comparison against the newest same-stage
// stage_conflict_resolution_failed entry is the load-bearing half (#3202). A
// trigger whose Sequence is at or BELOW that failure's has already been spent
// by the pass that failed, so it is CONSUMED and must not be served again —
// otherwise the next ordinary fix-up dispatch on this stage is handed
// conflict_resolution=true with anchors pointing at a merge that was aborted
// and a HEAD that has since moved.
//
// Fails closed on every uncertainty: an unreadable audit chain, a
// undecodable payload, and a half-populated trigger all return nil (no pass),
// because serving a pass the backend cannot fully justify is strictly worse
// than not serving one — the operator's next rebase invocation simply takes
// the fail-closed 422 it takes today.
func (s *Server) resolveConflictResolutionTrigger(ctx context.Context, runID, stageID uuid.UUID) *conflictResolutionTrigger {
	trigger, ok := s.newestStageEntry(ctx, runID, stageID, CategoryStageConflictResolutionTriggered)
	if !ok || trigger == nil {
		return nil
	}
	failure, ok := s.newestStageEntry(ctx, runID, stageID, CategoryStageConflictResolutionFailed)
	if !ok {
		return nil
	}
	if failure != nil && failure.Sequence >= trigger.Sequence {
		// The trigger was consumed by the pass that failed.
		return nil
	}

	var payload conflictResolutionTrigger
	if err := json.Unmarshal(trigger.Payload, &payload); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"conflict resolution: malformed stage_conflict_resolution_triggered payload — serving no pass",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	if !payload.populated() {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"conflict resolution: half-populated trigger payload — serving no pass",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()))
		return nil
	}
	return &payload
}

// maybeRecoverConflictResolutionFailure handles a REPORTED conflict-resolution
// pass failure: it writes the stage_conflict_resolution_failed entry carrying
// the runner's named refusal reason (which CONSUMES the trigger) and restores
// the run to its pre-pass review gate, returning true so the caller SKIPS both
// the ordinary fix-up recovery and the run-failing orchestrator advance.
//
// A pass is an ASSIST, never an escalation: the operator asked whether a
// mechanical resolution was possible, and "no" must leave the run exactly where
// it was — at its review gate with the intact PR un-orphaned — not fail it.
//
// It returns false on every miss (not an implement stage, no live trigger, the
// restore not applicable) so the normal failure path proceeds unchanged. The
// live-trigger check reuses resolveConflictResolutionTrigger, so a stage whose
// trigger was ALREADY consumed by an earlier failure is not recovered twice.
func (s *Server) maybeRecoverConflictResolutionFailure(ctx context.Context, runID, stageID uuid.UUID, reason string) bool {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return false
	}
	stage, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil || stage.Type != run.StageTypeImplement {
		return false
	}
	trigger := s.resolveConflictResolutionTrigger(ctx, runID, stageID)
	if trigger == nil {
		return false
	}

	var reviewStageID *uuid.UUID
	if trigger.ReparkedReviewStageID != "" {
		rid, perr := uuid.Parse(trigger.ReparkedReviewStageID)
		if perr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"conflict resolution recovery: unparseable reparked_review_stage_id — leaving failure path in force",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", perr.Error()))
			return false
		}
		reviewStageID = &rid
	}

	recovery, err := run.RestoreFixupStage(ctx, s.cfg.RunRepo, stageID,
		run.StageState(trigger.PriorState), reviewStageID)
	if err != nil {
		if !errors.Is(err, run.ErrFixupRecoveryNotApplicable) {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"conflict resolution recovery: restore failed — leaving failure path in force",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()))
		}
		return false
	}

	// Write the consumption marker AFTER the restore lands. Ordering is
	// deliberate: the entry is what makes the trigger spent, and a spent
	// trigger with an un-restored stage would leave the run failed with no
	// route back — whereas a restored stage whose marker append fails is
	// re-attemptable (the next report re-enters this function with the trigger
	// still live).
	s.writeConflictResolutionFailedAudit(ctx, runID, stageID, trigger, reason)
	s.notifyStatusUpdate(ctx, runID, "stage_conflict_resolution_failed")
	_ = recovery
	return true
}

// writeConflictResolutionFailedAudit appends the stage_conflict_resolution_
// failed entry. Best-effort: the recovery transition is already committed, so a
// failure here logs but does not unwind.
func (s *Server) writeConflictResolutionFailedAudit(ctx context.Context, runID, stageID uuid.UUID, trigger *conflictResolutionTrigger, reason string) {
	payload, _ := json.Marshal(map[string]any{
		"stage_id":          stageID.String(),
		"branch":            trigger.Branch,
		"base_ref":          trigger.BaseRef,
		"expected_head_sha": trigger.ExpectedHeadSHA,
		"pass":              trigger.Pass,
		"reason":            reason,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryStageConflictResolutionFailed,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"conflict resolution recovery: append stage_conflict_resolution_failed audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
}
