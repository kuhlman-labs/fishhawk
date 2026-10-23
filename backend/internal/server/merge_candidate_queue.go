package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pglock"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-092 (E83.85 / #4200): the merge-candidate held-pass SERIALIZER. With
// Config.MergeQueue wired, a base_advance or conflict_resolution
// merge-candidate verify pass is no longer started the moment its PR's head
// moves. It is ENQUEUED on its (repository, base ref) and the base is PUMPED:
// at most ONE pass is live per base, every other one is held FIFO and consumes
// no runner slot.
//
//   - startMergeCandidateVerifyPass routes a serialized cause through
//     serializeMergeCandidateVerifyPass (enqueue → pump → answer from the
//     run's entry). A fan_in pass is NOT serialized: it gates the consolidated
//     review that precedes approval, so it is not an approval-time pass.
//   - pumpMergeCandidateQueue admits the base's next held entry when nothing is
//     live, re-reads its PR, auto-advances a behind branch through
//     advanceRunBranch under the system actor, re-anchors, and starts the pass
//     for the resulting head (admitMergeCandidateEntry) — unless, in ENFORCE
//     transfer mode, the auto-advance transferred the prior head's passed
//     verdict (E87.2 / #4293), in which case the entry goes straight to phase
//     passed with no pass. Each admitted entry
//     runs detached from its caller's cancellation under its own
//     mergeCandidateQueueHandOffBudget (#4303); AdmitNext stays on the caller
//     ctx, so a cancelled pump claims no new entry.
//   - mergeCandidateQueueOnVerdict (from recordMergeCandidateVerified): a red
//     pass EJECTS and hands the line on; passed holds it until merge (D4)
//     or a re-open releases it (#4352);
//     not_executed holds it in phase unverified.
//   - mergeCandidateQueueOnPRResolved (from resolveReviewStageOnMerge): merge
//     settles the live entry merged (a held one is dropped
//     pr_merged_externally), close-without-merge drops pr_closed, then pumps.
//   - releaseMergeCandidateQueueForRun / mergeCandidateQueueForRun are the
//     cancel-hook and read-surface seams.
//   - releaseMergeCandidateQueueOnReopen (from fixupStageAs, the
//     conflict-resolution arm and the branch reset, #4352) drops a re-opened
//     run's entry so it stops holding the line; the run re-enters at the TAIL
//     on its next serialized pass (ADR-093 Q2).
//   - ADR-093 slice 2 (E87.5 / #4296): every admission-owned write carries
//     the entry's admission epoch (Unadmit, SetPhase, ReanchorAt, SettleAt
//     are compare-and-swaps), so a decision made against an earlier admission
//     changes nothing. The pump's live-entry branch runs MERGE-ON-PASS
//     (mergeQueueMergeOnPass, merge_queue_merge.go) for a live, passed,
//     intent-bearing entry and applies its outcome; it continues only after
//     a settle, a live admission, a bounded merge-on-pass readvance, or an
//     ADR-112 R3 yield (#4632: a non-approved fold waits at the head for the
//     approval window, then yields; an admission veto yields at once).
//
// Every queue move writes one INTERNAL audit row, best-effort and after the
// store's own transaction committed. The store is the source of truth; a lost
// audit row never unwinds a queue move.

// Queue audit categories (registered in audit/categories.go). INTERNAL: not
// issue-comment surfaces.
const (
	CategoryMergeCandidateQueueHeld       = "merge_candidate_queue_held"
	CategoryMergeCandidateQueueAdmitted   = "merge_candidate_queue_admitted"
	CategoryMergeCandidateQueueReanchored = "merge_candidate_queue_reanchored"
	CategoryMergeCandidateQueueSettled    = "merge_candidate_queue_settled"
)

// mergeCandidateStateHeld is the start answer for a pass HELD behind its
// base's live pass: no trigger, no re-open, no slot.
const mergeCandidateStateHeld = "held"

// mergeCandidateStateNotQueued is the start answer for a serialized pass whose
// run is NOT merge-queue eligible (ADR-093 slice 1, E87.4 / #4295): no entry,
// no trigger, no re-open, no slot. The run takes the line through
// fishhawk_merge_run once it is eligible.
const mergeCandidateStateNotQueued = "not_queued"

// mergeCandidateNotQueuedNote is the not_queued answer's operator note.
const mergeCandidateNotQueuedNote = "the run is not merge-queue eligible (ADR-093 D1), so no merge-candidate pass was queued or started. Clear the named reasons (approve the pull request, settle the reviews, acceptance and checks, disposition blocking concerns), then invoke fishhawk_merge_run, which enqueues the run once it is eligible."

// mergeCandidateQueueReasonNotMergeEligible drops a HELD entry whose run is
// determined ineligible at admission (ADR-093 slice 1); it re-enters at the
// TAIL on its next fishhawk_merge_run (ADR-093 Q2).
const mergeCandidateQueueReasonNotMergeEligible = "not_merge_eligible"

// Queue eject/drop reasons (mergequeue README § "Reasons"; base_changed is the
// store's own).
const (
	mergeCandidateQueueReasonRunTerminal        = "run_terminal"
	mergeCandidateQueueReasonRunCancelled       = "run_cancelled"
	mergeCandidateQueueReasonPRMergedExternally = "pr_merged_externally"
	mergeCandidateQueueReasonPRClosed           = "pr_closed"
	mergeCandidateQueueReasonVerifyFailed       = "verify_failed"
	mergeCandidateQueueReasonAdvanceConflict    = "advance_conflict"
	mergeCandidateQueueReasonPassNotStartable   = "pass_not_startable"
	// Re-open releases (#4352): the run's head is about to move (agent work)
	// or already moved (a reset), so its entry leaves the line.
	mergeCandidateQueueReasonReopenedForFixup              = "reopened_for_fixup"
	mergeCandidateQueueReasonReopenedForConflictResolution = "reopened_for_conflict_resolution"
	mergeCandidateQueueReasonHeadReset                     = "head_reset"
)

// mergeCandidateQueueMaxAdmissions bounds one pump's admission loop. Each
// iteration either stops the pump or settles one entry terminally, so the
// bound only matters for a pathological queue; the next pump continues.
const mergeCandidateQueueMaxAdmissions = 64

// mergeQueueMaxReadvancesPerPump bounds the merge-on-pass readvances ONE pump
// performs (Unadmit then re-admit onto a moved base or head). A yield also
// returns the entry to held but is NOT counted: AdmitNext skips a yielded
// row, so it cannot come back in the same pump. A base that moves on every
// pass leaves the entry held at its place for the next pump past this bound.
const mergeQueueMaxReadvancesPerPump = 3

// admitOutcome is what one admission left the entry as; the pump decides
// from it whether to continue (ADR-093 slice 2, rejection item 5).
type admitOutcome int

const (
	// admitSettled: the admission settled the entry (ejected/dropped).
	admitSettled admitOutcome = iota
	// admitLive: the entry stays live (verifying or passed).
	admitLive
	// admitReturnedToHeld: a transient failure Unadmitted it (or tried to).
	admitReturnedToHeld
	// admitStale: an admission-epoch CAS conflict (another admission owns
	// the entry now) or a failed queue write: nothing more is decided here.
	admitStale
	// admitYielded: an intent-bearing entry whose ONLY ineligibility is the
	// R2 forge veto yielded at its admission epoch (ADR-112 R3 row 2): it is
	// held and marked, AdmitNext skips it, and the pump continues.
	admitYielded
)

// Merge-on-pass verified sources (merge_queue_verified.source).
const (
	mergeQueueVerifiedSourcePass          = "pass"
	mergeQueueVerifiedSourceTransfer      = "transfer"
	mergeQueueVerifiedSourceAlreadyPassed = "already_passed"
	mergeQueueVerifiedSourceNotRequired   = "not_required"
)

// mergeQueueDispatchLedger adapts mergequeue.Store's D1 dispatch ledger to
// merge-on-pass's mergeDispatchLedger seam: ErrAlreadyDispatched is
// claimed=false with a nil error (the forge is never called).
type mergeQueueDispatchLedger struct{ q mergequeue.Store }

func (l mergeQueueDispatchLedger) ClaimMergeDispatch(ctx context.Context, runID uuid.UUID, path, headSHA string) (mergequeue.DispatchClaim, bool, error) {
	c, err := l.q.ClaimMergeDispatch(ctx, runID, path, headSHA)
	if errors.Is(err, mergequeue.ErrAlreadyDispatched) {
		return c, false, nil
	}
	return c, err == nil, err
}

func (l mergeQueueDispatchLedger) MarkMergeDispatchFailed(ctx context.Context, runID uuid.UUID, cause string) error {
	return l.q.MarkMergeDispatchFailed(ctx, runID, cause)
}

func (l mergeQueueDispatchLedger) ReleaseMergeDispatch(ctx context.Context, claim mergequeue.DispatchClaim) error {
	return l.q.ReleaseMergeDispatch(ctx, claim)
}

func (l mergeQueueDispatchLedger) MergeDispatch(ctx context.Context, runID uuid.UUID) (mergequeue.DispatchClaim, bool, error) {
	return l.q.MergeDispatch(ctx, runID)
}

// mergeDispatchLedger is the wired D1 ledger, nil when the queue is unwired
// (merge-on-pass then holds: no merge without a claim).
func (s *Server) mergeDispatchLedger() mergeDispatchLedger {
	if s.cfg.MergeQueue == nil {
		return nil
	}
	return mergeQueueDispatchLedger{q: s.cfg.MergeQueue}
}

// mergeQueueAnchorFor is the eligibility intent anchor of an entry: its
// recorded intent sequence and base when it carries merge intent, else the
// zero anchor (no D6 approval transfer).
func mergeQueueAnchorFor(e mergequeue.Entry) mergeQueueIntentAnchor {
	if e.MergeIntentAt == nil {
		return mergeQueueIntentAnchor{}
	}
	return mergeQueueIntentAnchor{Sequence: e.MergeIntentSequence, BaseRef: e.BaseRef}
}

// mergeCandidateQueueSerialized reports whether a cause goes through the
// queue. ADR-092 D1 defines eligibility as "PR at review approval and needs a
// pass": base_advance and conflict_resolution. fan_in precedes approval.
func mergeCandidateQueueSerialized(cause string) bool {
	return cause == mergeCandidateCauseBaseAdvance || cause == mergeCandidateCauseConflictResolution
}

// mergeCandidateAdmitResult is what the pump did for one admitted run: the
// pass start answer, or a refusal (the entry was settled, or returned to
// held), and the admission outcome the pump continues on.
type mergeCandidateAdmitResult struct {
	start   *mergeCandidateVerifyStart
	refusal *mergeCandidateVerifyRefusal
	outcome admitOutcome
}

// serializeMergeCandidateVerifyPass is startMergeCandidateVerifyPass for a
// serialized cause with the queue wired: enqueue (held, or re-anchored in
// place), pump the base, then answer from the run's entry. Held → state held
// with its position (nothing started). Live and admitted by THIS pump → the
// pump's start answer. Live already (the run holds the line, e.g. its
// operator rebased it again) → the pass is started for the new head directly,
// which is where a pending pass's trigger is REPLACED (D2).
func (s *Server) serializeMergeCandidateVerifyPass(ctx context.Context, runID uuid.UUID,
	p mergeCandidateVerifyParams, actor mergeCandidateActor) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the run could not be read (" + err.Error() + ")"}
	}
	// D4: nothing required means nothing queued.
	if command, _, _ := s.resolveVerifyConfig(ctx, runRow, run.StageTypeImplement); command == "" {
		return &mergeCandidateVerifyStart{State: mergeCandidateStateNotRequired}, nil
	}

	// ADR-093 slice 1 (E87.4 / #4295): eligibility decides QUEUE ADMISSION. A
	// run with no active entry takes the line only when it is merge-queue
	// eligible (admission variant); otherwise nothing is enqueued or started.
	// An existing active entry keeps today's re-anchor semantics.
	active, err := s.mergeCandidateQueueForRun(ctx, runID)
	if err != nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the merge-candidate queue could not be read (" + err.Error() +
			"), so no pass was queued or started; re-invoke fishhawk_rebase_run_branch to retry"}
	}
	if active == nil {
		if elig := s.mergeQueueAdmissionEligibility(ctx, runRow, p.HeadSHA); !elig.Eligible {
			return &mergeCandidateVerifyStart{State: mergeCandidateStateNotQueued}, nil
		}
	}

	res, err := s.cfg.MergeQueue.Enqueue(ctx, mergequeue.EnqueueRequest{
		RunID: runID, BaseRef: p.BaseRef, HeadSHA: p.HeadSHA,
	})
	if err != nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the merge-candidate queue could not be written (" + err.Error() +
			"), so no pass was queued or started; re-invoke fishhawk_rebase_run_branch to retry"}
	}
	if res.Dropped != nil {
		s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueSettled, *res.Dropped, nil)
	}
	switch res.Outcome {
	case mergequeue.EnqueueInserted, mergequeue.EnqueueBaseChanged:
		s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueHeld, res.Entry, nil)
	case mergequeue.EnqueueReanchored:
		if res.AnchorChanged {
			s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueReanchored, res.Entry, nil)
		}
	}
	if res.Dropped != nil {
		// The dropped entry's base may have just lost its live entry.
		s.pumpMergeCandidateQueue(ctx, res.Dropped.Base())
	}

	pumped := s.pumpMergeCandidateQueue(ctx, res.Entry.Base())
	if r, ok := pumped[runID]; ok {
		return r.start, r.refusal
	}
	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the merge-candidate queue entry could not be read back (" + err.Error() +
			"); re-invoke fishhawk_rebase_run_branch to retry"}
	}
	if !found {
		return nil, &mergeCandidateVerifyRefusal{Reason: "the merge-candidate queue entry left the queue before its pass could start; read fishhawk_get_run_status for the reason"}
	}
	if st.Entry.State == mergequeue.StateHeld {
		return &mergeCandidateVerifyStart{State: mergeCandidateStateHeld, Position: st.Position, LiveRunID: st.LiveRunID}, nil
	}

	// The run already holds the line: start (or replace) its pass for this
	// head and record the phase the answer implies.
	start, refusal := s.beginMergeCandidateVerifyPass(ctx, runID, p, actor)
	if refusal != nil {
		return nil, refusal
	}
	if s.applyMergeCandidateStartToEntry(ctx, st.Entry, start.State, mergeQueueVerifiedSourceAlreadyPassed) == admitLive &&
		st.Entry.MergeIntentAt != nil &&
		(start.State == mergeCandidateStatePassed || start.State == mergeCandidateStateNotRequired) {
		// Merge-on-pass runs from the pump's live-entry branch: pump the
		// base DETACHED so this answer never waits on a forge merge.
		s.pumpMergeCandidateQueueDetached(pglock.Detach(ctx), st.Entry.Base())
	}
	return start, nil
}

// applyMergeCandidateStartToEntry records what a pass start for a LIVE entry
// means for the line, with the admission-epoch CAS: in_flight → verifying;
// passed / not_required → passed (holds the line until merge-on-pass, a
// merge or a re-open release, D4 / #4352), writing merge_queue_verified for
// an intent-bearing entry (source passedSource, or not_required); failed →
// ejected verify_failed and the base is pumped. It reports the admission
// outcome: admitSettled, admitLive, or admitStale on a CAS conflict.
func (s *Server) applyMergeCandidateStartToEntry(ctx context.Context, e mergequeue.Entry, state, passedSource string) admitOutcome {
	switch state {
	case mergeCandidateStateInFlight:
		return s.setMergeCandidateQueuePhaseAt(ctx, e, mergequeue.PhaseVerifying)
	case mergeCandidateStatePassed, mergeCandidateStateNotRequired:
		out := s.setMergeCandidateQueuePhaseAt(ctx, e, mergequeue.PhasePassed)
		if out == admitLive {
			source := passedSource
			if state == mergeCandidateStateNotRequired {
				source = mergeQueueVerifiedSourceNotRequired
			}
			s.writeMergeQueueVerified(ctx, e, e.AnchoredHeadSHA, source)
		}
		return out
	case mergeCandidateStateFailed:
		return s.settleMergeCandidateQueueEntryAt(ctx, e, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
	}
	return admitLive
}

// pumpMergeCandidateQueue drives one base: while nothing is live it admits the
// oldest held entry and runs its admission; an admission that settles the
// entry (ejected/dropped) admits the next. A live entry whose run is terminal
// is dropped run_terminal. It returns what happened to each run it admitted.
// Concurrent pumps are safe: AdmitNext is atomic under the per-base advisory
// lock, and the admission work is owned by the pump that admitted the entry.
//
// ctx gates AdmitNext and the live-entry check: a cancelled ctx claims no new
// entry. Once AdmitNext has claimed one, its admission (the admitted row and
// admitMergeCandidateEntry) runs on mergeCandidateAdmissionContext, detached
// from ctx's cancellation and bounded by a FRESH
// mergeCandidateQueueHandOffBudget (#4303). A shutdown signal or a departed
// webhook sender therefore cannot cut the re-anchor, the pass start or the
// recovery Unadmit and strand the entry live in phase admitted until
// mergequeue.AdmissionStaleAfter.
func (s *Server) pumpMergeCandidateQueue(ctx context.Context, base mergequeue.Base) map[uuid.UUID]mergeCandidateAdmitResult {
	out := map[uuid.UUID]mergeCandidateAdmitResult{}
	if s.cfg.MergeQueue == nil {
		return out
	}
	readvances := 0
	for i := 0; i < mergeCandidateQueueMaxAdmissions; i++ {
		adm, err := s.cfg.MergeQueue.AdmitNext(ctx, base)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge-candidate queue: admit failed; the base is left as it is until the next pump",
				slog.String("repo", base.Repo), slog.String("base_ref", base.BaseRef),
				slog.String("error", err.Error()))
			return out
		}
		if adm.Entry == nil {
			return out
		}
		if !adm.Admitted {
			// Validate the live entry holding the line.
			if s.mergeCandidateQueueRunTerminal(ctx, adm.Entry.RunID) {
				s.settleMergeCandidateQueueEntry(ctx, *adm.Entry, mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal)
				continue
			}
			e := *adm.Entry
			if e.State != mergequeue.StateLive || e.Phase != mergequeue.PhasePassed || e.MergeIntentAt == nil {
				return out
			}
			// ADR-093 D3: the live head of line passed and carries the
			// operator's merge intent, so the queue merges it.
			mctx, cancel := mergeCandidateAdmissionContext(pglock.Detach(ctx))
			o := s.mergeQueueMergeOnPass(mctx, mergeOnPassRequest{
				Entry: e, IntentSequence: e.MergeIntentSequence, Ledger: s.mergeDispatchLedger(),
			})
			cont := s.applyMergeOnPassOutcome(mctx, e, o, &readvances)
			cancel()
			if !cont {
				return out
			}
			continue
		}
		actx, cancel := mergeCandidateAdmissionContext(ctx)
		s.writeMergeCandidateQueueAudit(actx, CategoryMergeCandidateQueueAdmitted, *adm.Entry,
			map[string]any{"readmitted": adm.Readmitted})
		res := s.admitMergeCandidateEntry(actx, *adm.Entry)
		cancel()
		out[adm.Entry.RunID] = res
		stop := res.outcome != admitSettled && res.outcome != admitLive && res.outcome != admitYielded // ADR-093 slice 2 / ADR-112 R3: a settle, a live admission or a yield continues the pump
		if stop {
			return out
		}
	}
	return out
}

// applyMergeOnPassOutcome applies one merge-on-pass decision for the live
// entry e and reports whether the pump continues:
//   - merged: the decision settled it inside the lock (D3); otherwise the
//     merge is a fact and it is settled merged here. Continue.
//   - settle: SettleAt with the epoch the decision read. Continue on a
//     settle; a CAS conflict (another admission owns it) stops.
//   - readvance: Unadmit at the epoch read, returning it to held at its
//     place so its next admission re-advances it. Continue, bounded by
//     mergeQueueMaxReadvancesPerPump; a CAS conflict stops.
//   - await_approval (ADR-112 R3 row 3): start or continue the persisted
//     approval wait (Store.ApprovalWait, expiry judged on the database
//     clock). Before expiry the entry holds the line and the pump stops (it
//     admits nothing else; the next pump re-decides); at expiry it yields as
//     below. A CAS conflict or a failed write stops.
//   - yield (ADR-112 R3 rows 2 and 4, or an expired wait): Store.Yield at the
//     epoch read (held, marked), merge_queue_yielded, then CONTINUE to the
//     next held entry. A yield does not count against the readvance bound:
//     AdmitNext skips a yielded row, so it cannot return in this pump. A CAS
//     conflict stops.
//   - held: stop; the next pump re-decides.
//
// It runs OUTSIDE the merge-intent lock (the pump's detached context): the
// wait is a held outcome repeated across pumps, never held inside the lock.
func (s *Server) applyMergeOnPassOutcome(ctx context.Context, e mergequeue.Entry, o mergeOnPassOutcome, readvances *int) bool {
	log := func(msg string, attrs ...slog.Attr) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, msg, append([]slog.Attr{
			slog.String("run_id", e.RunID.String()), slog.String("outcome", string(o.Kind)),
			slog.String("reason", o.Reason), slog.String("detail", o.Detail)}, attrs...)...)
	}
	switch o.Kind {
	case mergeOnPassMerged:
		if !o.Settled {
			res, err := s.cfg.MergeQueue.Settle(ctx, e.ID, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
			if err != nil {
				log("merge queue: the merged entry could not be settled; the next pump re-reads the merged PR",
					slog.String("error", err.Error()))
				return false
			}
			if res.Settled {
				s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueSettled, res.Entry,
					map[string]any{"from_state": string(e.State)})
			}
		}
		return true
	case mergeOnPassSettle:
		return s.settleMergeCandidateQueueEntryAt(ctx, e, o.State, o.Reason) == admitSettled
	case mergeOnPassReadvance:
		if *readvances >= mergeQueueMaxReadvancesPerPump {
			log("merge queue: merge-on-pass readvance bound reached; the next pump re-decides")
			return false
		}
		if _, err := s.cfg.MergeQueue.Unadmit(ctx, e.ID, e.AdmissionEpoch); err != nil {
			log("merge queue: merge-on-pass readvance did not return the entry to held; another admission owns it",
				slog.String("error", err.Error()))
			return false
		}
		*readvances++
		log("merge queue: merge-on-pass readvances the entry at its place")
		return true
	case mergeOnPassAwaitApproval:
		res, err := s.cfg.MergeQueue.ApprovalWait(ctx, e.ID, e.AdmissionEpoch, s.mergeQueueApprovalWait())
		if err != nil {
			log("merge queue: the approval wait could not be recorded; the next pump re-decides",
				slog.String("error", err.Error()))
			return false
		}
		if !res.Expired {
			log("merge queue: the head of line waits for a qualifying forge approval",
				slog.String("next_step", o.Yield.NextStep), slog.Duration("remaining", res.Remaining))
			return false
		}
		return s.yieldMergeQueueEntry(ctx, res.Entry, o.Yield, mergeQueueYieldSiteMergeOnPass)
	case mergeOnPassYield:
		return s.yieldMergeQueueEntry(ctx, e, o.Yield, mergeQueueYieldSiteMergeOnPass)
	}
	log("merge queue: merge-on-pass holds the entry")
	return false
}

// pumpMergeCandidateQueueDetached pumps base on a detached goroutine bounded
// by mergeCandidateAdmissionContext and tracked by bgMergeQueuePump (so
// Server.Shutdown drains it): the merge_run enqueue arm answers without
// waiting on an admission's auto-advance or pass start (approval condition
// C6).
func (s *Server) pumpMergeCandidateQueueDetached(ctx context.Context, base mergequeue.Base) {
	if s.cfg.MergeQueue == nil {
		return
	}
	pctx, cancel := mergeCandidateAdmissionContext(ctx)
	s.bgMergeQueuePump.Add(1)
	go func() {
		defer s.bgMergeQueuePump.Done()
		defer cancel()
		s.pumpMergeCandidateQueue(pctx, base)
	}()
}

// onlyForgeApprovalVetoed reports a result whose every reason is the R2
// forge veto (ADR-112 R3 row 2 alone: no row-1 reason beside it).
func onlyForgeApprovalVetoed(r mergeQueueEligibilityResult) bool {
	if len(r.Reasons) == 0 {
		return false
	}
	for _, x := range r.Reasons {
		if x.Code != mergeQueueReasonForgeApprovalVetoed {
			return false
		}
	}
	return true
}

// mergeQueueReasonsText renders eligibility reasons as "code: detail; …".
func mergeQueueReasonsText(reasons []mergeQueueIneligibility) string {
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, r.Code+": "+r.Detail)
	}
	return strings.Join(parts, "; ")
}

// mergeCandidateAdmissionContext is the context ONE admission runs on:
// detached from ctx's cancellation (WithoutCancel keeps its values and drops
// its deadline) and bounded by a fresh mergeCandidateQueueHandOffBudget. An
// admission nested inside another (a settle's pump) gets its own budget rather
// than inheriting what is left of its parent's.
func mergeCandidateAdmissionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), mergeCandidateQueueHandOffBudget)
}

// MergeCandidateQueuePumpInterval is the periodic pump's tick: the bound on
// how long a base stays stalled behind a live entry whose run went terminal
// through a path that does not release the queue (a failure, a sweep).
const MergeCandidateQueuePumpInterval = time.Minute

// PumpMergeCandidateQueues pumps every base with a held or live entry once and
// returns how many bases it pumped. Each pump drops a live entry whose run is
// terminal and re-claims a stale admission, so this one sweep is the liveness
// backstop for every hand-off an event hook missed. A list error is logged and
// pumps nothing; the next tick retries.
func (s *Server) PumpMergeCandidateQueues(ctx context.Context) int {
	if s.cfg.MergeQueue == nil {
		return 0
	}
	bases, err := s.cfg.MergeQueue.ActiveBases(ctx)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: listing active bases failed; the next pump tick retries",
			slog.String("error", err.Error()))
		return 0
	}
	for _, b := range bases {
		s.pumpMergeCandidateQueue(ctx, b)
	}
	return len(bases)
}

// RunMergeCandidateQueuePump pumps every active base once at boot and then on
// every interval tick, returning when ctx is cancelled. ctx (fishhawkd's
// signal context) gates only the ticker and NEW admissions: an admission the
// loop already claimed runs detached to completion (pumpMergeCandidateQueue).
// The loop joins bgMergeQueuePump, so Server.Shutdown drains it, bounded by
// the shutdown context (#4303). The Add runs inside the goroutine serve.go
// launches, so a shutdown landing before it is not drained (the boot-window
// residual the other bg groups share); Wait at zero returns at once.
func (s *Server) RunMergeCandidateQueuePump(ctx context.Context, interval time.Duration) {
	s.bgMergeQueuePump.Add(1)
	defer s.bgMergeQueuePump.Done()
	s.PumpMergeCandidateQueues(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.PumpMergeCandidateQueues(ctx)
		}
	}
}

// mergeCandidateQueueRunTerminal reports whether the run is terminal. A read
// error is NOT terminal: an uncertain read never drops an entry.
func (s *Server) mergeCandidateQueueRunTerminal(ctx context.Context, runID uuid.UUID) bool {
	r, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return errors.Is(err, run.ErrNotFound)
	}
	return r.State.IsTerminal()
}

// admitMergeCandidateEntry runs one admission (ADR-092 D2): re-read the PR,
// drop it when the run is terminal or the PR is merged/closed, auto-advance a
// behind branch under the system actor, re-anchor to the resulting head, then
// act on that head's verify state. When the auto-advance PERFORMED a merge,
// the ADR-096 verdict transfer runs first: enforce may transfer the prior
// head's passed verdict (the head then reads passed and no pass starts),
// shadow classifies async after the pass start. It reports whether the entry was settled
// (so the pump admits the next). A TRANSIENT failure (an unreadable run or PR,
// an undeterminable advance, a failed merge) returns the entry to held at its
// original place and stops the pump; a conflict ejects. ctx is the admission
// context pumpMergeCandidateQueue derived (mergeCandidateAdmissionContext):
// already detached from the pump caller's cancellation, so every step here,
// the recovery Unadmit included, runs to completion or to its own budget.
func (s *Server) admitMergeCandidateEntry(ctx context.Context, e mergequeue.Entry) mergeCandidateAdmitResult {
	unadmit := func(why string) mergeCandidateAdmitResult {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: admission failed transiently; entry returned to held at its place",
			slog.String("run_id", e.RunID.String()), slog.String("reason", why))
		outcome := admitReturnedToHeld
		if _, err := s.cfg.MergeQueue.Unadmit(ctx, e.ID, e.AdmissionEpoch); err != nil {
			if errors.Is(err, mergequeue.ErrStateConflict) {
				outcome = admitStale
			}
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge-candidate queue: unadmit failed; the stale-admission bound re-runs it",
				slog.String("run_id", e.RunID.String()), slog.String("error", err.Error()))
		}
		return mergeCandidateAdmitResult{outcome: outcome, refusal: &mergeCandidateVerifyRefusal{
			Reason: "the merge-candidate queue admission could not complete (" + why + "); the pass stays held at its place and is re-admitted on the next queue event"}}
	}
	settle := func(state mergequeue.State, reason, why string) mergeCandidateAdmitResult {
		return mergeCandidateAdmitResult{outcome: s.settleMergeCandidateQueueEntryAt(ctx, e, state, reason), refusal: &mergeCandidateVerifyRefusal{
			Reason: "the merge-candidate queue " + string(state) + " this pass (" + reason + "): " + why}}
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, e.RunID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			return settle(mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal, "the run no longer exists")
		}
		return unadmit("the run could not be read: " + err.Error())
	}
	if runRow.State.IsTerminal() {
		return settle(mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal, "the run is "+string(runRow.State))
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return unadmit("the run has no installation to read its pull request")
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return unadmit("the run repo is unparseable: " + err.Error())
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		return unadmit("the run has no tracked pull request")
	}
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, scope, repo, prNumber)
	if err != nil {
		return unadmit("reading the pull request failed: " + err.Error())
	}
	if pr.Merged {
		return settle(mergequeue.StateDropped, mergeCandidateQueueReasonPRMergedExternally, "the pull request was merged outside the queue")
	}
	if pr.State == "closed" {
		return settle(mergequeue.StateDropped, mergeCandidateQueueReasonPRClosed, "the pull request is closed")
	}
	head, branch := pr.HeadSHA, pr.HeadRef
	if head == "" || branch == "" {
		return unadmit("the pull request returned an empty head sha or branch")
	}

	// ADR-093 slice 1 (E87.4 / #4295): re-evaluate eligibility on the PR's
	// current head BEFORE the behind probe and auto-advance. Determined
	// ineligible → dropped not_merge_eligible (the pump admits the next);
	// undeterminable → unadmit (stays held at its place). ADR-112 R3
	// precedence: a determined non-approval ineligibility (row 1) drops as
	// before even beside a veto; an intent-bearing entry whose ONLY reason is
	// the R2 veto (row 2) YIELDS at its admission epoch, never drops.
	if elig := s.mergeQueueAdmissionEligibilityAt(ctx, runRow, head, mergeQueueAnchorFor(e)); !elig.Eligible {
		if elig.Undeterminable {
			return unadmit("merge-queue eligibility could not be determined: " + mergeQueueReasonsText(elig.Reasons))
		}
		if e.MergeIntentAt != nil && onlyForgeApprovalVetoed(elig) {
			y := newMergeQueueYield(mergeCandidateQueueReasonForgeApprovalVetoed, elig.Reasons[0].Detail, mergeQueueYieldRowVeto)
			if !s.yieldMergeQueueEntry(ctx, e, y, mergeQueueYieldSiteAdmission) {
				return mergeCandidateAdmitResult{outcome: admitStale, refusal: &mergeCandidateVerifyRefusal{
					Reason: "the merge-candidate queue could not yield this entry (another admission owns it, or the write failed)"}}
			}
			return mergeCandidateAdmitResult{outcome: admitYielded, refusal: &mergeCandidateVerifyRefusal{
				Reason: "the merge-candidate queue yielded this entry (" + y.Reason + ", next step " + y.NextStep + "): " + y.Detail}}
		}
		return settle(mergequeue.StateDropped, mergeCandidateQueueReasonNotMergeEligible,
			"the run is no longer merge-queue eligible ("+mergeQueueReasonsText(elig.Reasons)+"); fishhawk_merge_run re-enqueues it at the tail once it is")
	}

	behind, err := s.cfg.GitHub.CompareCommits(ctx, scope, repo, head, e.BaseRef)
	if err != nil {
		return unadmit("the behind-probe failed: " + err.Error())
	}
	// transfer is the ADR-096 clean-advance request, set only when THIS
	// admission performed the auto-advance (E87.2 / #4293).
	var transfer *mergeCandidateTransferRequest
	if len(behind) > 0 {
		adv, ref := s.advanceRunBranch(ctx, runRow, mergeCandidateSystemActor(),
			"merge-candidate queue admission (ADR-092): the branch fell behind "+e.BaseRef+" while held")
		if ref != nil {
			if ref.Kind == rebaseAdvanceRefusalConflict {
				return settle(mergequeue.StateEjected, mergeCandidateQueueReasonAdvanceConflict,
					"advancing the branch onto "+e.BaseRef+" conflicted; resolve it with fishhawk_rebase_run_branch, which re-queues the pass")
			}
			why := ref.Reason
			if ref.Err != nil {
				why = ref.Err.Error()
			}
			return unadmit("the auto-advance was refused (" + ref.Kind + "): " + why)
		}
		if adv.NewHeadSHA == "" {
			return unadmit("the auto-advance merged but its resulting head could not be read back")
		}
		if adv.MergePerformed {
			// Clean only when the new head IS the merge commit AND that merge
			// commit's parents were verified as exactly [prior head, base tip]
			// (ADR-112 R1(a)(ii), adv.CleanVerified): a concurrent push the
			// post-merge re-read observed, or an unverified parent list, is
			// not_clean_advance.
			transfer = &mergeCandidateTransferRequest{Run: runRow, PriorHead: head, NewHead: adv.NewHeadSHA,
				MergePerformed: adv.MergeCommitSHA != "" && adv.NewHeadSHA == adv.MergeCommitSHA && adv.CleanVerified,
				Cause:          mergeCandidateCauseBaseAdvance}
			if e.MergeIntentAt != nil {
				s.writeMergeQueueRow(ctx, CategoryMergeQueueAdvanced, e, map[string]any{
					"prior_head_sha":   head,
					"new_head_sha":     adv.NewHeadSHA,
					"merge_commit_sha": adv.MergeCommitSHA,
					"clean":            transfer.MergePerformed,
					"base_tip_sha":     adv.BaseTipSHA,
					"clean_verified":   adv.CleanVerified,
					"admission_epoch":  e.AdmissionEpoch,
				})
			}
		}
		head = adv.NewHeadSHA
	}
	if transfer != nil {
		// SHADOW classifies ASYNC, after the pass start below, never on this
		// hand-off path (a no-op outside shadow).
		defer s.scheduleMergeCandidateTransferShadow(*transfer)
	}
	if head != e.AnchoredHeadSHA {
		re, err := s.cfg.MergeQueue.ReanchorAt(ctx, e.ID, e.AdmissionEpoch, head, "")
		if errors.Is(err, mergequeue.ErrStateConflict) {
			return mergeCandidateAdmitResult{outcome: admitStale, refusal: &mergeCandidateVerifyRefusal{
				Reason: "another merge-candidate queue admission owns this entry now (" + err.Error() + ")"}}
		}
		if err != nil {
			return unadmit("re-anchoring the entry failed: " + err.Error())
		}
		e = re
		s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueReanchored, e, map[string]any{"at_admission": true})
	}

	if transfer != nil {
		// ENFORCE decides BEFORE the verify-state read: a durable transferred
		// row makes that read answer passed, so the switch below sets phase
		// passed and starts no pass, trigger or slot. The classification gets
		// only a SUB-budget of ctx that leaves mergeCandidateTransferHandOffReserve
		// for the reads, the pass start and Unadmit; a spent sub-budget is
		// undecidable:deadline_exceeded, a refusal, so the pass still starts.
		req := *transfer
		req.Reserve = mergeCandidateTransferHandOffReserve
		s.mergeCandidateTransferEnforce(ctx, req)
	}

	st, err := s.mergeCandidateVerifyState(ctx, runRow, head)
	if err != nil {
		return unadmit("the merge-candidate verify state could not be read: " + err.Error())
	}
	switch st.State {
	case mergeCandidateStateFailed:
		return settle(mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed, "the head already has a failed merge-candidate verdict")
	case mergeCandidateStateInFlight, mergeCandidateStatePassed, mergeCandidateStateNotRequired:
		source := mergeQueueVerifiedSourceAlreadyPassed
		if transfer != nil {
			source = mergeQueueVerifiedSourceTransfer
		}
		return mergeCandidateAdmitResult{outcome: s.applyMergeCandidateStartToEntry(ctx, e, st.State, source),
			start: &mergeCandidateVerifyStart{State: st.State, StageID: st.StageID}}
	}

	start, refusal := s.beginMergeCandidateVerifyPass(ctx, e.RunID, mergeCandidateVerifyParams{
		Branch: branch, BaseRef: e.BaseRef, HeadSHA: head, Cause: st.Cause,
	}, mergeCandidateSystemActor())
	if refusal != nil && refusal.Transient {
		// #4391 (approval condition C1): begin's superseded-fix-up arm could not
		// take the stage's admission fence within its bound; the next pump
		// re-admits the entry.
		return unadmit(refusal.Reason)
	}
	if refusal != nil {
		return settle(mergequeue.StateEjected, mergeCandidateQueueReasonPassNotStartable, refusal.Reason)
	}
	return mergeCandidateAdmitResult{start: start,
		outcome: s.applyMergeCandidateStartToEntry(ctx, e, start.State, mergeQueueVerifiedSourceAlreadyPassed)}
}

// mergeCandidateQueueOnVerdict applies a recorded pass result to the run's
// LIVE entry anchored at that head (D4): failed → ejected verify_failed and
// the base is pumped; passed → phase passed (holds the line until merge, or
// until releaseMergeCandidateQueueOnReopen drops it on a re-open, #4352);
// not_executed → phase unverified (still holds the line; the operator's next
// fishhawk_rebase_run_branch re-triggers it). A verdict for a head other than
// the anchored one, or for a run with no live entry, changes nothing.
func (s *Server) mergeCandidateQueueOnVerdict(ctx context.Context, runID uuid.UUID, headSHA, result string) {
	if s.cfg.MergeQueue == nil {
		return
	}
	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: verdict hook could not read the run's entry",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return
	}
	if !found || st.Entry.State != mergequeue.StateLive || st.Entry.AnchoredHeadSHA != headSHA {
		return
	}
	switch result {
	case mergeCandidateResultFailed:
		s.settleMergeCandidateQueueEntry(ctx, st.Entry, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
	case mergeCandidateResultPassed:
		if s.setMergeCandidateQueuePhaseAt(ctx, st.Entry, mergequeue.PhasePassed) == admitLive {
			if st.Entry.MergeIntentAt != nil {
				s.writeMergeQueueVerified(ctx, st.Entry, headSHA, mergeQueueVerifiedSourcePass)
				// ADR-093 D3: merge-on-pass runs from the pump; pump DETACHED
				// so the verdict upload never waits on a forge merge.
				s.pumpMergeCandidateQueueDetached(pglock.Detach(ctx), st.Entry.Base())
			}
		}
	case mergeCandidateResultNotExecuted:
		s.setMergeCandidateQueuePhase(ctx, st.Entry, mergequeue.PhaseUnverified)
	}
}

// mergeCandidateQueueHandOffBudget bounds ONE admission: every entry
// pumpMergeCandidateQueue admits gets a fresh budget, on every trigger path
// (the periodic pump, the webhook PR-resolution hook, the verdict and cancel
// hooks, the re-open release, a rebase's serialize). Separately it bounds the PR-resolution hook's
// own entry read and settle. An admission runs detached from its caller's
// cancellation because one admission's auto-advance alone can spend the
// bounded post-merge re-read and then its own detached
// rebasePostMergeTailBudget tail, while its caller may be a webhook delivery
// (cancelled when the forge stops listening, ~10s) or fishhawkd's signal
// context (cancelled at shutdown, #4303). On a cancelled context the
// re-anchor, the pass start AND the recovery Unadmit would all fail, leaving
// the entry live in phase admitted until mergequeue.AdmissionStaleAfter
// re-claims it. Twice the tail budget leaves the post-advance steps room after
// a full-length tail.
//
// What the caller ctx still gates is only the TOP-LEVEL AdmitNext. An
// admission that settles its entry (eject/drop) pumps the base again on its
// own detached context, so a nested chain's admissions keep running after a
// shutdown signal until Server.Shutdown's drain closes. A chain (one webhook
// hand-off that ejects entry after entry) is therefore no longer bounded as a
// whole: each admission is, and the chain is bounded by the number of held
// entries (and mergeCandidateQueueMaxAdmissions per pump).
const mergeCandidateQueueHandOffBudget = 2 * rebasePostMergeTailBudget

// mergeCandidateQueueOnPRResolved settles the run's active entry when its PR
// reaches a terminal forge state, then pumps the base (D4): merged → a live
// entry is merged (it held the line to here) and a held one is dropped
// pr_merged_externally; closed without merge → dropped pr_closed. A forge-side
// merge is a fact regardless of review bookkeeping, so the caller runs this
// before any review gate decides. The hook's own entry read and settle run
// detached from ctx's cancellation (its values are kept) and bounded by
// mergeCandidateQueueHandOffBudget, so a webhook sender that disconnects
// cannot stop the hand-off; every admission the settle's pump runs carries its
// own fresh budget (mergeCandidateAdmissionContext), so a long chain is not
// cut by this hook's deadline. Idempotent: a redelivery finds no active entry.
func (s *Server) mergeCandidateQueueOnPRResolved(ctx context.Context, runID uuid.UUID, merged bool) {
	if s.cfg.MergeQueue == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mergeCandidateQueueHandOffBudget)
	defer cancel()
	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: PR-resolution hook could not read the run's entry",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return
	}
	if !found {
		return
	}
	state, reason := mergequeue.StateDropped, mergeCandidateQueueReasonPRClosed
	if merged {
		state, reason = mergequeue.StateMerged, "pr_merged"
		if st.Entry.State == mergequeue.StateHeld {
			state, reason = mergequeue.StateDropped, mergeCandidateQueueReasonPRMergedExternally
		}
	}
	s.settleMergeCandidateQueueEntry(ctx, st.Entry, state, reason)
}

// releaseMergeCandidateQueueForRun drops the run's active entry with reason
// and pumps its base, so a run that ends (cancelled, failed) never holds the
// line. Best-effort; a run with no active entry is a no-op.
func (s *Server) releaseMergeCandidateQueueForRun(ctx context.Context, runID uuid.UUID, reason string) {
	if s.cfg.MergeQueue == nil {
		return
	}
	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: release could not read the run's entry",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return
	}
	if !found {
		return
	}
	s.settleMergeCandidateQueueEntry(ctx, st.Entry, mergequeue.StateDropped, reason)
}

// releaseMergeCandidateQueueOnReopen drops the run's active entry with reason
// when the run is re-opened (#4352), pumping the base when the entry was live
// so the next held entry is admitted at once. A re-opened run's entry leaves
// the line because the head will move: a fix-up or conflict-resolution agent
// pushes new work, a reset already rewound it. The pass the entry anchored, or
// its passed verdict, is therefore not the one that merges, and holding the
// line LIVE through a long agent pass head-of-line blocks every ready entry
// behind it. The run re-enters at the TAIL on its next serialized pass (a
// rebase or base advance), as ADR-093 Q2 rules.
//
// heldToo decides a HELD entry. An agent re-open (heldToo true) drops it too:
// admitting it mid-pass would auto-advance the branch under the running agent
// and then eject it pass_not_startable. A head reset (heldToo false) keeps it,
// because admission re-reads the PR and re-anchors to its current head anyway
// (ADR-092 D2). No verdict is touched: a head whose fix-up delivered nothing
// still reads passed. Best-effort: an unreadable entry WARN-logs and changes
// nothing (an uncertain read never drops an entry); no active entry is a no-op.
func (s *Server) releaseMergeCandidateQueueOnReopen(ctx context.Context, runID uuid.UUID, reason string, heldToo bool) {
	if s.cfg.MergeQueue == nil {
		return
	}
	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: re-open release could not read the run's entry; the entry is left as it is",
			slog.String("run_id", runID.String()), slog.String("reason", reason),
			slog.String("error", err.Error()))
		return
	}
	if !found {
		return
	}
	if st.Entry.State != mergequeue.StateLive && !heldToo {
		return
	}
	s.settleMergeCandidateQueueEntry(ctx, st.Entry, mergequeue.StateDropped, reason)
}

// mergeCandidateQueueForRun is the read-surface seam: the run's active entry
// with its held position and the base's live run, or (nil, nil) when the
// queue is unwired or the run has no active entry.
func (s *Server) mergeCandidateQueueForRun(ctx context.Context, runID uuid.UUID) (*mergequeue.Status, error) {
	if s.cfg.MergeQueue == nil {
		return nil, nil
	}
	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil || !found {
		return nil, err
	}
	return &st, nil
}

// settleMergeCandidateQueueEntry settles e terminally, writes the settled row
// when THIS call settled it, and pumps the base when the entry was live (the
// line is free) — a settled HELD entry frees nothing.
func (s *Server) settleMergeCandidateQueueEntry(ctx context.Context, e mergequeue.Entry, state mergequeue.State, reason string) {
	res, err := s.cfg.MergeQueue.Settle(ctx, e.ID, state, reason)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: settle failed",
			slog.String("run_id", e.RunID.String()), slog.String("state", string(state)),
			slog.String("reason", reason), slog.String("error", err.Error()))
		return
	}
	if !res.Settled {
		return
	}
	s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueSettled, res.Entry,
		map[string]any{"from_state": string(e.State)})
	if e.State == mergequeue.StateLive {
		s.pumpMergeCandidateQueue(ctx, e.Base())
	}
}

// setMergeCandidateQueuePhase records a live entry's phase at the admission
// epoch e was read at, best-effort.
func (s *Server) setMergeCandidateQueuePhase(ctx context.Context, e mergequeue.Entry, phase mergequeue.Phase) {
	s.setMergeCandidateQueuePhaseAt(ctx, e, phase)
}

// setMergeCandidateQueuePhaseAt is setMergeCandidateQueuePhase reporting the
// admission outcome: admitLive when the phase is recorded, admitStale when
// the SetPhase CAS refused (the entry is no longer live at e's epoch) or the
// write failed.
func (s *Server) setMergeCandidateQueuePhaseAt(ctx context.Context, e mergequeue.Entry, phase mergequeue.Phase) admitOutcome {
	if _, err := s.cfg.MergeQueue.SetPhase(ctx, e.ID, e.AdmissionEpoch, phase); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: set phase failed",
			slog.String("run_id", e.RunID.String()), slog.String("phase", string(phase)),
			slog.Int64("admission_epoch", e.AdmissionEpoch), slog.String("error", err.Error()))
		return admitStale
	}
	return admitLive
}

// settleMergeCandidateQueueEntryAt is settleMergeCandidateQueueEntry for an
// admission-owned or merge-on-pass decision: SettleAt at e's admission
// epoch. admitSettled when the entry is terminal after the call (this call
// settled it, or it already was); admitStale on a CAS conflict or a failed
// write, with nothing written.
func (s *Server) settleMergeCandidateQueueEntryAt(ctx context.Context, e mergequeue.Entry, state mergequeue.State, reason string) admitOutcome {
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
	if e.State == mergequeue.StateLive {
		s.pumpMergeCandidateQueue(ctx, e.Base())
	}
	return admitSettled
}

// writeMergeQueueVerified appends merge_queue_verified for an intent-bearing
// entry whose head reads passed (source pass, transfer, already_passed or
// not_required); an entry without merge intent writes nothing.
func (s *Server) writeMergeQueueVerified(ctx context.Context, e mergequeue.Entry, head, source string) {
	if e.MergeIntentAt == nil {
		return
	}
	s.writeMergeQueueRow(ctx, CategoryMergeQueueVerified, e, map[string]any{"head_sha": head, "source": source})
}

// writeMergeQueueRow appends one merge-on-pass pump row (merge_queue_advanced
// or merge_queue_verified) under the system actor, best-effort.
func (s *Server) writeMergeQueueRow(ctx context.Context, category string, e mergequeue.Entry, fields map[string]any) {
	if s.cfg.AuditRepo == nil {
		return
	}
	fields["entry_id"] = e.ID.String()
	fields["base_ref"] = e.BaseRef
	payload, _ := json.Marshal(fields)
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        e.RunID,
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: append pump row failed",
			slog.String("run_id", e.RunID.String()), slog.String("category", category),
			slog.String("error", err.Error()))
	}
}

// writeMergeCandidateQueueAudit appends one queue row under the system actor.
// Best-effort: the store already committed the move.
func (s *Server) writeMergeCandidateQueueAudit(ctx context.Context, category string, e mergequeue.Entry, extra map[string]any) {
	if s.cfg.AuditRepo == nil {
		return
	}
	fields := map[string]any{
		"entry_id":          e.ID.String(),
		"repo":              e.Repo,
		"base_ref":          e.BaseRef,
		"state":             string(e.State),
		"anchored_head_sha": e.AnchoredHeadSHA,
	}
	if e.Phase != "" {
		fields["phase"] = string(e.Phase)
	}
	if e.EjectReason != "" {
		fields["reason"] = e.EjectReason
	}
	for k, v := range extra {
		fields[k] = v
	}
	payload, _ := json.Marshal(fields)
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        e.RunID,
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: append audit row failed",
			slog.String("run_id", e.RunID.String()), slog.String("category", category),
			slog.String("error", err.Error()))
	}
}
