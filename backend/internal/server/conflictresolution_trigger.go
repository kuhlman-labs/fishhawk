package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// conflictResolutionMaxPasses is the ceiling on conflict-resolution passes a
// single implement stage may consume (E64.62 / #3202). ONE. A conflict
// resolution is a bounded, operator-authorized assist, never an auto-loop: a
// pass that refuses spends the budget, and the next
// fishhawk_rebase_run_branch invocation takes the fail-closed 422
// rebase_conflict arm naming the spent budget and the resolve-push-vouch
// route.
//
// It is DELIBERATELY a separate constant from defaultMaxFixupPasses /
// defaultFixupCeiling and is counted against a separate audit category, so a
// conflict-resolution pass can never consume — or be consumed by — the fix-up
// budget. That separation is what makes "consumes no fix-up budget"
// STRUCTURALLY true rather than merely asserted: nothing on this path reads or
// writes CategoryStageFixupTriggered.
const conflictResolutionMaxPasses = 1

// errConflictResolutionBudgetSpent is the trigger's fail-closed refusal when
// the stage has already consumed its single conflict-resolution pass. The
// rebase handler maps it to the narrowed 422 rebase_conflict — the SAME error
// code the pre-#3202 unconditional refusal used, now reachable only once the
// budget is spent.
var errConflictResolutionBudgetSpent = errors.New("conflict-resolution budget spent")

// conflictResolutionTriggerAudit is the stage_conflict_resolution_triggered
// audit payload. It embeds conflictResolutionTrigger — the four fields the
// prompt server serves onto the wire — and adds the recovery anchors the
// failure arm reads back (the implement stage's pre-pass state and the
// re-parked review stage), mirroring the stage_fixup_triggered payload shape
// maybeRecoverFixupFailure consumes.
//
// The embedded struct carries no json tag, so its fields promote INLINE and
// resolveConflictResolutionTrigger's decode of the same bytes is unaffected by
// the recovery keys added here.
type conflictResolutionTriggerAudit struct {
	conflictResolutionTrigger
	RunID   string `json:"run_id"`
	StageID string `json:"stage_id"`
	// PriorState is the implement stage's state before the re-open, so a
	// failed pass restores exactly what it displaced instead of guessing.
	PriorState string `json:"prior_state"`
	// ReparkedReviewStageID is the review stage the re-open parked back to
	// pending, restored alongside the implement stage on failure.
	ReparkedReviewStageID string `json:"reparked_review_stage_id,omitempty"`
	// Reason is the operator's rebase-branch note, carried for the record.
	Reason string `json:"reason,omitempty"`
}

// countConflictResolutionPasses returns how many
// stage_conflict_resolution_triggered entries the stage has already recorded —
// the durable pass counter the ceiling-1 budget is enforced against. There is
// no dedicated column, exactly as for the fix-up counter, so the bound holds
// across restarts.
//
// It reads ONLY CategoryStageConflictResolutionTriggered. The fix-up counter is
// never consulted here and never written by this path.
func (s *Server) countConflictResolutionPasses(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageConflictResolutionTriggered)
	if err != nil {
		return 0, fmt.Errorf("list %s audit entries: %w", CategoryStageConflictResolutionTriggered, err)
	}
	n := 0
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			n++
		}
	}
	return n, nil
}

// newestConflictResolutionFailureReason returns the NAMED refusal reason the
// newest stage_conflict_resolution_failed entry for the stage recorded, and
// whether any such entry exists.
//
// It is what lets the budget-spent 422 name WHICH rule the prior pass broke
// (the runner's confinement-gate reason) rather than only that a pass ran.
// Best-effort: an unconfigured repo, a read error or a malformed payload
// degrade to ("", false) and the refusal simply omits the reason.
func (s *Server) newestConflictResolutionFailureReason(ctx context.Context, runID, stageID uuid.UUID) (string, bool) {
	if s.cfg.AuditRepo == nil {
		return "", false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageConflictResolutionFailed)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"conflict resolution: list stage_conflict_resolution_failed audit failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return "", false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			SourceFailureReason string `json:"source_failure_reason"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		return payload.SourceFailureReason, true
	}
	return "", false
}

// conflictResolutionTriggerParams carries the resolved inputs for
// triggerConflictResolutionPass: the implement stage to re-open and the merge
// the runner must perform locally.
type conflictResolutionTriggerParams struct {
	StageID         uuid.UUID
	Branch          string
	BaseRef         string
	ExpectedHeadSHA string
	Reason          string
}

// triggerConflictResolutionPass re-opens the implement stage for ONE
// operator-authorized conflict-resolution pass (E64.62 / #3202) and returns
// the re-opened stage.
//
// It reuses run.FixupStage's state machine rather than duplicating it, which
// is safe precisely because PriorPassCount / MaxPasses / HardCeiling are
// CALLER-supplied: the counter handed in is the conflict-resolution counter
// (countConflictResolutionPasses), so the fix-up budget is neither read nor
// written. The audit entry this writes carries the DIFFERENT category, so the
// fix-up counter cannot see it either.
//
// The budget decision is taken HERE, before any stage is touched:
// PriorPassCount >= conflictResolutionMaxPasses returns
// errConflictResolutionBudgetSpent and NOTHING is written. run.FixupStage's own
// ceiling is set to the same value as a second line of defence, but this check
// is the one the rebase handler's 422 arm is bound to.
//
// Every post-transition step is best-effort in the same posture as
// fixupStageAs: the audit entry is the durable record of the authorization, so
// an orchestrator or notify failure logs rather than unwinding it.
func (s *Server) triggerConflictResolutionPass(ctx context.Context, id Identity, p conflictResolutionTriggerParams) (*run.FixupDecision, error) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil, errors.New("conflict resolution requires run + audit repositories")
	}
	stage, err := s.cfg.RunRepo.GetStage(ctx, p.StageID)
	if err != nil {
		return nil, fmt.Errorf("get implement stage: %w", err)
	}

	prior, err := s.countConflictResolutionPasses(ctx, stage.RunID, p.StageID)
	if err != nil {
		return nil, err
	}
	// THE CEILING-1 BUDGET CHECK. Fail-closed and taken before any write.
	if prior >= conflictResolutionMaxPasses {
		return nil, errConflictResolutionBudgetSpent
	}

	dec, err := run.FixupStage(ctx, s.cfg.RunRepo, p.StageID, run.FixupOptions{
		PriorPassCount: prior,
		MaxPasses:      conflictResolutionMaxPasses,
		HardCeiling:    conflictResolutionMaxPasses,
	})
	if err != nil {
		return nil, err
	}

	s.writeConflictResolutionTriggeredAudit(ctx, id, dec, p, prior+1)

	if dec.Stage.State == run.StageStatePending && s.cfg.Orchestrator != nil {
		if _, aerr := s.cfg.Orchestrator.Advance(ctx, dec.Stage.RunID); aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
				"conflict resolution: orchestrator advance failed",
				slog.String("run_id", dec.Stage.RunID.String()),
				slog.String("stage_id", dec.Stage.ID.String()),
				slog.String("error", aerr.Error()))
		}
		if updated, gerr := s.cfg.RunRepo.GetStage(ctx, dec.Stage.ID); gerr == nil {
			dec.Stage = updated
		}
	}

	s.notifyStatusUpdate(ctx, dec.Stage.RunID, "stage_conflict_resolution_triggered")
	return dec, nil
}

// writeConflictResolutionTriggeredAudit appends the
// stage_conflict_resolution_triggered entry: the merge the runner must perform
// (branch, base ref, expected head), the pass ordinal, and the recovery anchors
// the failure arm reads back. Operator actor — the pass exists only because an
// operator invoked fishhawk_rebase_run_branch.
//
// Best-effort like every other trigger audit: the transition already committed,
// so an append failure WARNs rather than unwinding it.
func (s *Server) writeConflictResolutionTriggeredAudit(ctx context.Context, id Identity,
	dec *run.FixupDecision, p conflictResolutionTriggerParams, pass int) {
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser

	entry := conflictResolutionTriggerAudit{
		conflictResolutionTrigger: conflictResolutionTrigger{
			Branch:          p.Branch,
			BaseRef:         p.BaseRef,
			ExpectedHeadSHA: p.ExpectedHeadSHA,
			Pass:            pass,
		},
		RunID:      dec.Stage.RunID.String(),
		StageID:    dec.Stage.ID.String(),
		PriorState: string(dec.PriorState),
		Reason:     p.Reason,
	}
	if dec.ReparkedReview != nil {
		entry.ReparkedReviewStageID = dec.ReparkedReview.ID.String()
	}
	payload, _ := json.Marshal(entry)

	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        dec.Stage.RunID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryStageConflictResolutionTriggered,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"conflict resolution: append stage_conflict_resolution_triggered audit entry failed",
			slog.String("run_id", dec.Stage.RunID.String()),
			slog.String("stage_id", dec.Stage.ID.String()),
			slog.String("error", err.Error()))
	}
}

// maybeRecoverConflictResolutionFailure detects a failed conflict-resolution
// re-dispatch and, when it is one, restores the run to its pre-pass review gate
// instead of letting the run fail (E64.62 / #3202). It returns true ONLY when
// it recovered the run — the caller then SKIPS the run-failing orchestrator
// advance.
//
// A conflict-resolution pass is an ASSIST, never an escalation: the run was
// parked at a healthy review gate with an open, mergeable PR when the operator
// invoked fishhawk_rebase_run_branch, so a pass whose confinement gate REFUSED
// must leave the run exactly where it was. Landing the implement stage terminal
// `failed` would destroy intact work over a merge the operator can still
// perform by hand.
//
// ORDERING IS LOAD-BEARING at both call sites: this runs BEFORE
// maybeRecoverFixupFailure. A stage that consumed a fix-up pass earlier and a
// conflict-resolution pass later carries BOTH trigger categories, and the
// fix-up recovery keys on its own newest stage_fixup_triggered entry — which is
// now STALE — so it would restore from the wrong anchor. This function matches
// only on a stage_conflict_resolution_triggered entry and returns false
// otherwise, so a stage with no conflict-resolution pass falls straight through
// to the unchanged fix-up path.
//
// Best-effort and self-contained, mirroring maybeRecoverFixupFailure: it owns
// the recovery transition, the stage_conflict_resolution_failed audit emission
// and the status-comment refresh; a failure in any step returns false so the
// caller's normal Advance-to-failed path runs.
func (s *Server) maybeRecoverConflictResolutionFailure(ctx context.Context, runID, stageID uuid.UUID) bool {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return false
	}

	stage, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil || stage.Type != run.StageTypeImplement {
		return false
	}

	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageConflictResolutionTriggered)
	if err != nil {
		return false
	}
	var triggered *audit.Entry
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			triggered = e // entries are append-ordered; keep the last
		}
	}
	if triggered == nil {
		return false
	}

	var payload struct {
		PriorState            string `json:"prior_state"`
		ReparkedReviewStageID string `json:"reparked_review_stage_id"`
	}
	if err := json.Unmarshal(triggered.Payload, &payload); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"conflict resolution recovery: malformed stage_conflict_resolution_triggered payload — leaving failure path in force",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return false
	}

	var reviewStageID *uuid.UUID
	if payload.ReparkedReviewStageID != "" {
		rid, perr := uuid.Parse(payload.ReparkedReviewStageID)
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
		run.StageState(payload.PriorState), reviewStageID)
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

	s.writeConflictResolutionFailedAudit(ctx, runID, recovery)
	s.notifyStatusUpdate(ctx, runID, "stage_conflict_resolution_failed")
	return true
}

// writeConflictResolutionFailedAudit appends the
// stage_conflict_resolution_failed entry: the restored implement state, the
// restored review stage, and the source failure category/reason the pass died
// with. That reason is the runner's NAMED confinement-gate refusal, so the
// record says WHICH rule the agent's resolution broke — and the budget-spent
// 422 reads it back to name the failed pass.
//
// Best-effort: the recovery transition is already committed, so an append
// failure logs but does not unwind.
func (s *Server) writeConflictResolutionFailedAudit(ctx context.Context, runID uuid.UUID, rec *run.FixupRecovery) {
	fields := map[string]any{
		"stage_id":       rec.Stage.ID.String(),
		"restored_state": string(rec.Stage.State),
	}
	if rec.RestoredReview != nil {
		fields["restored_review_stage_id"] = rec.RestoredReview.ID.String()
		fields["restored_review_state"] = string(rec.RestoredReview.State)
	}
	if rec.PriorFailureCategory != nil {
		fields["source_failure_category"] = string(*rec.PriorFailureCategory)
	}
	if rec.PriorFailureReason != nil {
		fields["source_failure_reason"] = *rec.PriorFailureReason
	}
	payload, _ := json.Marshal(fields)

	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &rec.Stage.ID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryStageConflictResolutionFailed,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"conflict resolution recovery: append stage_conflict_resolution_failed audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", rec.Stage.ID.String()),
			slog.String("error", err.Error()))
	}
}
