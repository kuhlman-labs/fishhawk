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

// CategoryConflictResolutionPushed is the terminal SUCCESS marker the
// pull-request report handler writes when the runner ships a
// conflict_resolution_pushed outcome. It CONSUMES its trigger exactly as the
// failure entry does (#3202 round-3): a pass that SUCCEEDED has spent the
// ceiling-of-one budget just as surely as one that was refused, and leaving the
// trigger live would hand the next ordinary fix-up on the same implement stage
// conflict_resolution=true with anchors that name a merge already committed and
// pushed. Success and failure are therefore both consumption, and the resolver
// takes whichever is newest.
const CategoryConflictResolutionPushed = "conflict_resolution_pushed"

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
// The comparison against the newest same-stage CONSUMPTION entry is the
// load-bearing half (#3202). Consumption is written by BOTH terminal outcomes
// — stage_conflict_resolution_failed for a refused pass and
// conflict_resolution_pushed for a successful one — because either settles the
// pass and spends its ceiling-of-one budget. A trigger whose Sequence is at or
// BELOW the newest of those has already been spent, so it is CONSUMED and must
// not be served again — otherwise the next ordinary fix-up dispatch on this
// stage is handed conflict_resolution=true with anchors pointing either at a
// merge that was aborted and a HEAD that has since moved (the failure case) or
// at a merge already committed and pushed (the success case).
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
	for _, category := range []string{CategoryStageConflictResolutionFailed, CategoryConflictResolutionPushed} {
		settled, ok := s.newestStageEntry(ctx, runID, stageID, category)
		if !ok {
			return nil
		}
		if settled != nil && settled.Sequence >= trigger.Sequence {
			// The trigger was consumed by the pass that settled — refused
			// (stage_conflict_resolution_failed) or succeeded
			// (conflict_resolution_pushed). Either way its budget is spent.
			return nil
		}
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
// The consumption marker is appended BEFORE the restore, and a persistence
// failure REFUSES the recovery (#3202 round-3). Consumption lives only in the
// audit chain, so a restore that lands while the marker append fails would
// acknowledge the recovery — implement back to succeeded, review back to
// awaiting_approval — while leaving the trigger LIVE and therefore dispatchable:
// the operator's next ordinary fix-up on that stage is served
// conflict_resolution=true with anchors naming a merge that was aborted. Marker
// first inverts the exposure: if the append fails nothing is restored, the
// stage stays `failed`, the caller's normal failure path runs, and no ordinary
// fix-up can be dispatched off the stale instruction. The converse residual is
// deliberate and strictly smaller: a marker that lands ahead of a restore that
// turns out not to apply spends a trigger for a pass the runner has already
// reported as failed, which is what the trigger's budget means anyway.
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

	// Write the consumption marker BEFORE the restore: it is the ONLY durable
	// record that this trigger is spent, so a restore we cannot pair with it
	// must not happen at all.
	if err := s.writeConflictResolutionFailedAudit(ctx, runID, stageID, trigger, reason); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"conflict resolution recovery: consumption marker not persisted — refusing the recovery so the stale trigger cannot be dispatched",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return false
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

	s.notifyStatusUpdate(ctx, runID, "stage_conflict_resolution_failed")
	_ = recovery
	return true
}

// writeConflictResolutionFailedAudit appends the stage_conflict_resolution_
// failed entry and RETURNS the append error. It is not best-effort: the entry
// is the trigger's consumption record, and its caller refuses the recovery
// outright when it cannot be persisted.
func (s *Server) writeConflictResolutionFailedAudit(ctx context.Context, runID, stageID uuid.UUID, trigger *conflictResolutionTrigger, reason string) error {
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
		return err
	}
	return nil
}
