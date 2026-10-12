package server

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
//     pass EJECTS and hands the line on; passed holds it until merge (D4);
//     not_executed holds it in phase unverified.
//   - mergeCandidateQueueOnPRResolved (from resolveReviewStageOnMerge): merge
//     settles the live entry merged (a held one is dropped
//     pr_merged_externally), close-without-merge drops pr_closed, then pumps.
//   - releaseMergeCandidateQueueForRun / mergeCandidateQueueForRun are the
//     cancel-hook and read-surface seams.
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
)

// mergeCandidateQueueMaxAdmissions bounds one pump's admission loop. Each
// iteration either stops the pump or settles one entry terminally, so the
// bound only matters for a pathological queue; the next pump continues.
const mergeCandidateQueueMaxAdmissions = 64

// mergeCandidateQueueSerialized reports whether a cause goes through the
// queue. ADR-092 D1 defines eligibility as "PR at review approval and needs a
// pass": base_advance and conflict_resolution. fan_in precedes approval.
func mergeCandidateQueueSerialized(cause string) bool {
	return cause == mergeCandidateCauseBaseAdvance || cause == mergeCandidateCauseConflictResolution
}

// mergeCandidateAdmitResult is what the pump did for one admitted run: the
// pass start answer, or a refusal (the entry was settled, or returned to held).
type mergeCandidateAdmitResult struct {
	start   *mergeCandidateVerifyStart
	refusal *mergeCandidateVerifyRefusal
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
	s.applyMergeCandidateStartToEntry(ctx, st.Entry, start.State)
	return start, nil
}

// applyMergeCandidateStartToEntry records what a pass start for a LIVE entry
// means for the line: in_flight → verifying; passed / not_required → passed
// (holds the line until merge, D4); failed → ejected verify_failed and the
// base is pumped. Reports whether the entry was settled.
func (s *Server) applyMergeCandidateStartToEntry(ctx context.Context, e mergequeue.Entry, state string) bool {
	switch state {
	case mergeCandidateStateInFlight:
		s.setMergeCandidateQueuePhase(ctx, e, mergequeue.PhaseVerifying)
	case mergeCandidateStatePassed, mergeCandidateStateNotRequired:
		s.setMergeCandidateQueuePhase(ctx, e, mergequeue.PhasePassed)
	case mergeCandidateStateFailed:
		s.settleMergeCandidateQueueEntry(ctx, e, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
		return true
	}
	return false
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
			return out
		}
		actx, cancel := mergeCandidateAdmissionContext(ctx)
		s.writeMergeCandidateQueueAudit(actx, CategoryMergeCandidateQueueAdmitted, *adm.Entry,
			map[string]any{"readmitted": adm.Readmitted})
		res, settled := s.admitMergeCandidateEntry(actx, *adm.Entry)
		cancel()
		out[adm.Entry.RunID] = res
		if !settled {
			return out
		}
	}
	return out
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
func (s *Server) admitMergeCandidateEntry(ctx context.Context, e mergequeue.Entry) (mergeCandidateAdmitResult, bool) {
	unadmit := func(why string) (mergeCandidateAdmitResult, bool) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: admission failed transiently; entry returned to held at its place",
			slog.String("run_id", e.RunID.String()), slog.String("reason", why))
		if _, err := s.cfg.MergeQueue.Unadmit(ctx, e.ID); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"merge-candidate queue: unadmit failed; the stale-admission bound re-runs it",
				slog.String("run_id", e.RunID.String()), slog.String("error", err.Error()))
		}
		return mergeCandidateAdmitResult{refusal: &mergeCandidateVerifyRefusal{
			Reason: "the merge-candidate queue admission could not complete (" + why + "); the pass stays held at its place and is re-admitted on the next queue event"}}, false
	}
	settle := func(state mergequeue.State, reason, why string) (mergeCandidateAdmitResult, bool) {
		s.settleMergeCandidateQueueEntry(ctx, e, state, reason)
		return mergeCandidateAdmitResult{refusal: &mergeCandidateVerifyRefusal{
			Reason: "the merge-candidate queue " + string(state) + " this pass (" + reason + "): " + why}}, true
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
			transfer = &mergeCandidateTransferRequest{Run: runRow, PriorHead: head, NewHead: adv.NewHeadSHA,
				MergePerformed: true, Cause: mergeCandidateCauseBaseAdvance}
		}
		head = adv.NewHeadSHA
	}
	if transfer != nil {
		// SHADOW classifies ASYNC, after the pass start below, never on this
		// hand-off path (a no-op outside shadow).
		defer s.scheduleMergeCandidateTransferShadow(*transfer)
	}
	if head != e.AnchoredHeadSHA {
		re, err := s.cfg.MergeQueue.Reanchor(ctx, e.ID, head, "")
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
	case mergeCandidateStateInFlight:
		s.setMergeCandidateQueuePhase(ctx, e, mergequeue.PhaseVerifying)
		return mergeCandidateAdmitResult{start: &mergeCandidateVerifyStart{State: mergeCandidateStateInFlight, StageID: st.StageID}}, false
	case mergeCandidateStatePassed, mergeCandidateStateNotRequired:
		s.setMergeCandidateQueuePhase(ctx, e, mergequeue.PhasePassed)
		return mergeCandidateAdmitResult{start: &mergeCandidateVerifyStart{State: st.State}}, false
	}

	start, refusal := s.beginMergeCandidateVerifyPass(ctx, e.RunID, mergeCandidateVerifyParams{
		Branch: branch, BaseRef: e.BaseRef, HeadSHA: head, Cause: st.Cause,
	}, mergeCandidateSystemActor())
	if refusal != nil {
		return settle(mergequeue.StateEjected, mergeCandidateQueueReasonPassNotStartable, refusal.Reason)
	}
	if s.applyMergeCandidateStartToEntry(ctx, e, start.State) {
		return mergeCandidateAdmitResult{start: start}, true
	}
	return mergeCandidateAdmitResult{start: start}, false
}

// mergeCandidateQueueOnVerdict applies a recorded pass result to the run's
// LIVE entry anchored at that head (D4): failed → ejected verify_failed and
// the base is pumped; passed → phase passed (holds the line until merge);
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
		s.setMergeCandidateQueuePhase(ctx, st.Entry, mergequeue.PhasePassed)
	case mergeCandidateResultNotExecuted:
		s.setMergeCandidateQueuePhase(ctx, st.Entry, mergequeue.PhaseUnverified)
	}
}

// mergeCandidateQueueHandOffBudget bounds ONE admission: every entry
// pumpMergeCandidateQueue admits gets a fresh budget, on every trigger path
// (the periodic pump, the webhook PR-resolution hook, the verdict and cancel
// hooks, a rebase's serialize). Separately it bounds the PR-resolution hook's
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

// setMergeCandidateQueuePhase records a live entry's phase, best-effort.
func (s *Server) setMergeCandidateQueuePhase(ctx context.Context, e mergequeue.Entry, phase mergequeue.Phase) {
	if _, err := s.cfg.MergeQueue.SetPhase(ctx, e.ID, phase); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: set phase failed",
			slog.String("run_id", e.RunID.String()), slog.String("phase", string(phase)),
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
