package server

// ADR-093 slice 3 (D4, E87.6 / #4297): the merge-queue EJECTION record.
//
// Every ejection writes ONE merge_queue_ejected row naming its reason and a
// remedy from a closed next_step vocabulary, written ONLY by the call that
// settled the entry (a redelivered hook finds it already settled and writes
// nothing). The settle helpers (settleMergeCandidateQueueEntryWith /
// settleMergeCandidateQueueEntryAtWith) write it for every ejected settle; a
// caller that knows more passes a mergeQueueEjection (the forge refusal code,
// the D6 route). The verdict hook is the one exception: it defers the row
// (mergeQueueEjection.Deferred) and succeedMergeCandidateVerifiedStage writes
// it AFTER routeMergeCandidateFailure, so the row states what D6 actually did.
//
// The row is best-effort, appended after the store commit like every other
// queue row: a lost row costs that entry its automatic re-entry (fail closed;
// the operator's fishhawk_merge_run still re-enters it at the tail) and
// degrades the read surfaces' next_step to the reason default.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// CategoryMergeQueueEjected is the ONE ejection record per ejected queue
// entry (actor system, INTERNAL).
const CategoryMergeQueueEjected = "merge_queue_ejected"

// The closed eject next_step vocabulary (ADR-093 D4). Read surfaces derive
// next_step through mergeQueueEjectNextStep when the row is unreadable.
const (
	// mergeQueueEjectNextAwaitDelegatedFixup: ADR-090 D6 routed a delegated
	// fix-up; the run re-enters the queue at the tail automatically once that
	// fix-up has pushed and the run is merge-queue eligible again.
	mergeQueueEjectNextAwaitDelegatedFixup = "await_delegated_fixup"
	// mergeQueueEjectNextFixupStage: no fix-up was routed; the operator routes
	// one, then re-invokes fishhawk_merge_run.
	mergeQueueEjectNextFixupStage = "fishhawk_fixup_stage"
	// mergeQueueEjectNextRebaseRunBranch: the auto-advance conflicted; the
	// operator's fishhawk_rebase_run_branch starts the bounded
	// conflict-resolution pass (never auto-started by the pump).
	mergeQueueEjectNextRebaseRunBranch = "fishhawk_rebase_run_branch"
	// mergeQueueEjectNextMergeRun: re-invoke fishhawk_merge_run, which
	// re-enters the run at the tail.
	mergeQueueEjectNextMergeRun = "fishhawk_merge_run"
	// mergeQueueEjectNextGetRunStatus: the pass could not start; read the run
	// status for why.
	mergeQueueEjectNextGetRunStatus = "fishhawk_get_run_status"
)

// The forge's typed merge-refusal codes a forge_merge_refused ejection names.
const (
	mergeQueueForgeReasonNotMergeable     = "not_mergeable"
	mergeQueueForgeReasonUnprocessable    = "unprocessable"
	mergeQueueForgeReasonChecksNotSettled = "checks_not_settled"
)

// mergeQueueChecksNotSettledDetail leads a checks_not_settled refusal's
// forge_detail. The asymmetry is DELIBERATE (ADR-112 R3 row 5, ADR-093 D4):
// pending checks HOLD before dispatch (merge-on-pass's onlyChecksNotGreen
// rule), so slow CI never ejects; only a refusal from the forge's own merge
// PUT ejects, and it is attempted exactly once.
const mergeQueueChecksNotSettledDetail = "the checks were not settled at the merge attempt (the forge refused the pinned merge; pending checks hold before dispatch, only the forge's own refusal ejects): "

// mergeQueueForgeReasonFor maps a TYPED forge merge refusal to its code, or
// "" for any other error.
func mergeQueueForgeReasonFor(err error) string {
	switch {
	case errors.Is(err, forge.ErrPullRequestUnstableStatus):
		return mergeQueueForgeReasonChecksNotSettled
	case errors.Is(err, forge.ErrMergeUnprocessable):
		return mergeQueueForgeReasonUnprocessable
	case errors.Is(err, forge.ErrPullRequestNotMergeable):
		return mergeQueueForgeReasonNotMergeable
	}
	return ""
}

// mergeQueueEjectNextStep is the TOTAL mapping eject reason → next_step.
// fixupRouted matters only for verify_failed; an unknown reason maps to
// fishhawk_merge_run.
func mergeQueueEjectNextStep(reason string, fixupRouted bool) string {
	switch reason {
	case mergeCandidateQueueReasonVerifyFailed:
		if fixupRouted {
			return mergeQueueEjectNextAwaitDelegatedFixup
		}
		return mergeQueueEjectNextFixupStage
	case mergeCandidateQueueReasonAdvanceConflict:
		return mergeQueueEjectNextRebaseRunBranch
	case mergeCandidateQueueReasonForgeMergeRefused:
		return mergeQueueEjectNextMergeRun
	case mergeCandidateQueueReasonPassNotStartable:
		return mergeQueueEjectNextGetRunStatus
	}
	return mergeQueueEjectNextMergeRun
}

// mergeQueueEjectRemedy renders the remedy prose from the closed vocabulary
// ONLY (never forge text): next_step selects the sentence, and a
// checks_not_settled forge refusal states the hold/eject asymmetry.
func mergeQueueEjectRemedy(nextStep, forgeReason string) string {
	switch nextStep {
	case mergeQueueEjectNextAwaitDelegatedFixup:
		return "A delegated fix-up was routed (ADR-090 D6); the run re-enters the merge queue at the tail once that fix-up has pushed and the run is merge-queue eligible again. fishhawk_merge_run re-enters it sooner."
	case mergeQueueEjectNextFixupStage:
		return "No fix-up was routed: route one with fishhawk_fixup_stage on the implement stage, then re-invoke fishhawk_merge_run, which re-enters the run at the tail."
	case mergeQueueEjectNextRebaseRunBranch:
		return "Advancing the branch onto its base conflicted: fishhawk_rebase_run_branch starts the bounded conflict-resolution pass; then re-invoke fishhawk_merge_run, which re-enters the run at the tail."
	case mergeQueueEjectNextGetRunStatus:
		return "The merge-candidate verify pass could not start: read fishhawk_get_run_status for why, resolve it, then re-invoke fishhawk_merge_run, which re-enters the run at the tail."
	}
	if forgeReason == mergeQueueForgeReasonChecksNotSettled {
		return "The forge refused the pinned merge because the checks were not settled at the merge attempt; it is attempted exactly once (pending checks hold before dispatch, only the forge's own refusal ejects). Once the checks settle, re-invoke fishhawk_merge_run, which re-enters the run at the tail."
	}
	if forgeReason != "" {
		return "The forge refused the pinned merge; it is attempted exactly once. Resolve the forge's reason, then re-invoke fishhawk_merge_run, which re-enters the run at the tail."
	}
	return "Re-invoke fishhawk_merge_run, which re-enters the run at the tail."
}

// mergeQueueEjection is what a settling caller knows about an ejection beyond
// its reason. A nil *mergeQueueEjection derives the default
// (defaultMergeQueueEjection).
type mergeQueueEjection struct {
	// FixupRouted is set for verify_failed only: whether ADR-090 D6 routed a
	// delegated fix-up for the failed verdict.
	FixupRouted *bool
	// Refusal is D6's refusal naming fishhawk_fixup_stage when no fix-up was
	// routed.
	Refusal string
	// ForgeReason is the typed forge refusal code (forge_merge_refused).
	ForgeReason string
	// ForgeDetail is the forge's (untrusted) refusal text; the writer
	// sanitises it.
	ForgeDetail string
	// Deferred: the settle writes NO row; the caller writes it later (the
	// verdict hook, so the row carries what D6 did).
	Deferred bool
}

// settleMergeCandidateQueueEntryWith is settleMergeCandidateQueueEntry with
// the ejection detail: when THIS call settled the entry ejected it records
// the ejection (unless ej.Deferred) BEFORE pumping. It returns the settled
// entry and whether this call settled it.
func (s *Server) settleMergeCandidateQueueEntryWith(ctx context.Context, e mergequeue.Entry, state mergequeue.State, reason string,
	ej *mergeQueueEjection) (mergequeue.Entry, bool) {
	res, err := s.cfg.MergeQueue.Settle(ctx, e.ID, state, reason)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: settle failed",
			slog.String("run_id", e.RunID.String()), slog.String("state", string(state)),
			slog.String("reason", reason), slog.String("error", err.Error()))
		return mergequeue.Entry{}, false
	}
	if !res.Settled {
		return mergequeue.Entry{}, false
	}
	s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueSettled, res.Entry,
		map[string]any{"from_state": string(e.State)})
	s.recordMergeQueueEjection(ctx, res.Entry, ej)
	if e.State == mergequeue.StateLive {
		s.pumpMergeCandidateQueue(ctx, e.Base())
	}
	return res.Entry, true
}

// settleMergeCandidateQueueEntryAtWith is settleMergeCandidateQueueEntryAt
// with the ejection detail (see settleMergeCandidateQueueEntryWith).
func (s *Server) settleMergeCandidateQueueEntryAtWith(ctx context.Context, e mergequeue.Entry, state mergequeue.State, reason string,
	ej *mergeQueueEjection) admitOutcome {
	res, err := s.cfg.MergeQueue.SettleAt(ctx, e.ID, e.AdmissionEpoch, state, reason)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: settle failed",
			slog.String("run_id", e.RunID.String()), slog.String("state", string(state)),
			slog.String("reason", reason), slog.Int64("admission_epoch", e.AdmissionEpoch),
			slog.String("error", err.Error()))
		return admitStale
	}
	if !res.Settled {
		return admitSettled
	}
	s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueSettled, res.Entry,
		map[string]any{"from_state": string(e.State)})
	s.recordMergeQueueEjection(ctx, res.Entry, ej)
	if e.State == mergequeue.StateLive {
		s.pumpMergeCandidateQueue(ctx, e.Base())
	}
	return admitSettled
}

// recordMergeQueueEjection writes the merge_queue_ejected row for an entry
// THIS call settled ejected: nothing for another state or a deferred row; the
// default detail when ej is nil.
func (s *Server) recordMergeQueueEjection(ctx context.Context, settled mergequeue.Entry, ej *mergeQueueEjection) {
	if settled.State != mergequeue.StateEjected {
		return
	}
	if ej != nil && ej.Deferred {
		return
	}
	if ej == nil {
		d := s.defaultMergeQueueEjection(ctx, settled)
		ej = &d
	}
	s.writeMergeQueueEjected(ctx, settled, *ej)
}

// defaultMergeQueueEjection is the detail of an ejection whose caller passed
// none. A verify_failed ejection (the failed start, the failed verdict at
// admission, merge-on-pass seeing a failed verdict) derives fixup_routed from
// the run's audit chain (mergeQueueVerifyFailedRouted); every other reason
// carries none.
func (s *Server) defaultMergeQueueEjection(ctx context.Context, settled mergequeue.Entry) mergeQueueEjection {
	if settled.EjectReason != mergeCandidateQueueReasonVerifyFailed {
		return mergeQueueEjection{}
	}
	routed := s.mergeQueueVerifyFailedRouted(ctx, settled.RunID, settled.AnchoredHeadSHA)
	ej := mergeQueueEjection{FixupRouted: &routed}
	if !routed {
		ej.Refusal = "no delegated fix-up is recorded for the failed merge-candidate verdict at this head. " + mergeCandidateFixupRoute
	}
	return ej
}

// mergeQueueVerifyFailedRouted reports whether ADR-090 D6 routed a delegated
// fix-up for the newest FAILED merge-candidate verdict at head, matched by
// CHAIN SEQUENCE (never by timestamps): a stage_fixup_triggered row on the
// verdict's stage, labelled the D6 route (delegated
// merge_candidate_verify_failed), whose sequence follows the verdict's and
// precedes any later verdict on that stage. Undeterminable (a read error, no
// failed verdict at head) is false: fail closed to the operator's merge_run.
func (s *Server) mergeQueueVerifyFailedRouted(ctx context.Context, runID uuid.UUID, head string) bool {
	if s.cfg.AuditRepo == nil || head == "" {
		return false
	}
	verdicts, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeCandidateVerified)
	if err != nil {
		return false
	}
	var failed *audit.Entry
	for _, v := range verdicts {
		var p mergeCandidateVerifiedPayload
		if json.Unmarshal(v.Payload, &p) != nil || p.HeadSHA != head {
			continue
		}
		failed = nil
		if p.Result == mergeCandidateResultFailed && v.StageID != nil {
			failed = v
		}
	}
	if failed == nil {
		return false
	}
	// The window closes at the next verdict on the failed verdict's stage.
	var until int64
	for _, v := range verdicts {
		if v.StageID != nil && *v.StageID == *failed.StageID && v.Sequence > failed.Sequence {
			until = v.Sequence
			break
		}
	}
	fixups, err := s.stageEntriesStrict(ctx, runID, *failed.StageID, CategoryStageFixupTriggered)
	if err != nil {
		return false
	}
	for _, f := range fixups {
		if f.Sequence <= failed.Sequence || (until > 0 && f.Sequence >= until) {
			continue
		}
		var a fixupTriggerAnchors
		if json.Unmarshal(f.Payload, &a) == nil && a.Delegated == mergeCandidateDelegatedRule {
			return true
		}
	}
	return false
}

// writeMergeQueueEjected appends ONE merge_queue_ejected row for the settled
// entry e, actor system, best-effort. forge_detail is untrusted forge text,
// passed through the yield sanitiser (mergeQueueYieldDetail).
func (s *Server) writeMergeQueueEjected(ctx context.Context, e mergequeue.Entry, ej mergeQueueEjection) {
	if s.cfg.AuditRepo == nil {
		return
	}
	reason := e.EjectReason
	routed := ej.FixupRouted != nil && *ej.FixupRouted
	next := mergeQueueEjectNextStep(reason, routed)
	fields := map[string]any{
		"entry_id":          e.ID.String(),
		"repo":              e.Repo,
		"base_ref":          e.BaseRef,
		"reason":            reason,
		"merge_intent":      e.MergeIntentAt != nil,
		"anchored_head_sha": e.AnchoredHeadSHA,
		"next_step":         next,
		"remedy":            mergeQueueEjectRemedy(next, ej.ForgeReason),
	}
	if reason == mergeCandidateQueueReasonVerifyFailed {
		fields["fixup_routed"] = routed
		if !routed && ej.Refusal != "" {
			fields["refusal"] = ej.Refusal
		}
	}
	if ej.ForgeReason != "" {
		fields["forge_reason"] = ej.ForgeReason
	}
	if ej.ForgeDetail != "" {
		fields["forge_detail"] = mergeQueueYieldDetail(ej.ForgeDetail)
	}
	payload, _ := json.Marshal(fields)
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        e.RunID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeQueueEjected,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: append merge_queue_ejected failed; the entry loses its automatic re-entry and fishhawk_merge_run still re-enters it",
			slog.String("run_id", e.RunID.String()), slog.String("reason", reason),
			slog.String("error", err.Error()))
	}
}
