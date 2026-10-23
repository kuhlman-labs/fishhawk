package server

// ADR-093 D8 (E87.7 / #4298), slice 2: the restart resume's MERGING and
// SETTLING steps, the boot pass, and the per-Server in-flight registry.
//
// A live head-of-line entry records its LAST RECORDED step in its phase.
// merge-on-pass stamps merging in-lock BEFORE its D1 dispatch claim and
// settling after the forge accepts (merge_queue_merge.go). After a restart
// the pump routes a merging/settling entry here (mergeQueueResumeMerge),
// unless THIS process has its merge in flight (mergeInFlightRegistry).
//
// ADR-109 governs the ledger: the resume adds NO ledger transition and
// dispatches NO merge (rulings R1/R2). It makes ONE forge re-read under the
// run's merge-intent lock and reconciles:
//
//   - merged at a QUEUE claim's head → the normal merged tail (Resume=true:
//     merge_queue_merged written only when no row names the entry);
//   - merged with no claim, or another path's claim at the merged head →
//     settled merged pr_merged, the claim untouched;
//   - merged at a head the claim does not name → hold;
//   - not merged (or unreadable) with a crash-left QUEUE claim still
//     dispatched → the EXISTING MarkMergeDispatchFailed(orphaned_by_restart),
//     then the entry HOLDS naming the operator re-arm (closed: dropped);
//   - an inline/delegated claim, or a dispatch_failed one → hold, no ledger
//     write (approval condition C1);
//   - no claim → redecide: back to passed, and ordinary merge-on-pass decides
//     from scratch (no dispatch was ever handed to the forge, so not a retry).
//
// Every merge_queue_resumed row is appended BEFORE its action and fails
// closed (held resume_unrecorded, nothing changed). Settles other than the
// merged tail are returned as outcomes the pump applies after the lock.
// Contract: backend/internal/server/README.md § "Merge-queue durability
// (ADR-093 D8)".

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// mergeQueueCrashLeftClaimMargin is the slack added to the crash-left window.
const mergeQueueCrashLeftClaimMargin = 30 * time.Second

// mergeQueueCrashLeftClaimAge is the age past which a still-dispatched QUEUE
// claim observed under the run's merge-intent lock is crash-left (approval
// condition C2). Derivation: a live merge-on-pass holds the merge-intent lock
// for at most mergequeue.IntentFnTimeout (the ADR-108 lock fn deadline, 90s),
// and its pinned merge call is bounded by mergeOnPassMergeCallTimeout (30s,
// inside that deadline); plus mergeQueueCrashLeftClaimMargin (30s) = 150s. A
// claim younger than that may belong to a live process's dispatch: it holds
// with nothing written (merge_dispatch_in_progress). The age is judged on the
// DATABASE clock (Resumer.DatabaseNow against the claim's claimed_at), never
// Go time.Now(); there is no boot-time cutoff.
const mergeQueueCrashLeftClaimAge = mergequeue.IntentFnTimeout + mergeOnPassMergeCallTimeout + mergeQueueCrashLeftClaimMargin

// The merging/settling resume's reasons (merge_queue_resumed.reason, and the
// held outcome's reason).
const (
	// mergeQueueResumeReasonUnrecorded: a merge_queue_resumed (or
	// merge_dispatch_settled / merge_queue_merged) append failed, so nothing
	// was changed (fail closed, ruling R8d).
	mergeQueueResumeReasonUnrecorded = "resume_unrecorded"
	// mergeQueueResumeReasonInProgress: a still-dispatched queue claim is
	// younger than mergeQueueCrashLeftClaimAge (C2).
	mergeQueueResumeReasonInProgress = "merge_dispatch_in_progress"
	// mergeQueueResumeReasonOrphanedByRestart is the MarkMergeDispatchFailed
	// cause and the merge_dispatch_settled reason for a crash-left queue
	// claim (C3).
	mergeQueueResumeReasonOrphanedByRestart = "orphaned_by_restart"
	// mergeQueueResumeReasonDispatchUncertain: the crash-left claim is now
	// dispatch_failed and the entry holds for the operator re-arm.
	mergeQueueResumeReasonDispatchUncertain = "merge_dispatch_uncertain"
	// mergeQueueResumeReasonSettlingUnconfirmed: the entry recorded settling
	// (the forge accepted) but the PR still reads open.
	mergeQueueResumeReasonSettlingUnconfirmed = "settling_unconfirmed"
	// mergeQueueResumeReasonMergedAtClaim: the PR merged at the queue
	// claim's head.
	mergeQueueResumeReasonMergedAtClaim = "merged_at_claim_head"
	// mergeQueueResumeReasonNoClaim: a merging entry with no dispatch claim.
	mergeQueueResumeReasonNoClaim = "no_dispatch_claim"
	// mergeQueueResumeReasonBootInstant: the boot pump re-claimed a dead admission (the boot instant, or AdmissionStaleAfter).
	mergeQueueResumeReasonBootInstant = "admission_reclaimed"
)

// mergeQueueResumeNextOperatorRearm is the crash-left hold's next_step.
const mergeQueueResumeNextOperatorRearm = "operator_rearm"

// mergeQueueResumeRearmRemedy names the crash-left hold's operator path
// (approval condition C8a).
const mergeQueueResumeRearmRemedy = "the queue claim was orphaned by a restart and is now dispatch_failed (orphaned_by_restart). " +
	"Confirm on the forge that the pull request did not merge, then apply the operator re-arm in backend/internal/mergequeue/README.md " +
	"§ \"Merge-dispatch ledger\" (DELETE the run's dispatch_failed claim). The queue-path re-arm needs NO fishhawk_merge_run " +
	"re-invocation: the queue's next pump re-decides the merge from scratch (the re-invoke sentence there applies to the inline path)."

// mergeQueueResumeSettlingRemedy names the settling_unconfirmed hold's
// operator path (approval condition C8b).
const mergeQueueResumeSettlingRemedy = "the forge accepted the queue's merge but the pull request still reads open; this depends on the forge converging. " +
	"Once the forge shows the pull request merged, fishhawk_reconcile_merge (or fishhawk_record_merge_observation) settles it, " +
	"or the next pump does; otherwise the forge must converge."

// mergeQueueMergeActorVerifiedKey is the settle_merged row's key recording
// whether the forge's merge actor or merge commit was checked (C6b).
const mergeQueueMergeActorVerifiedKey = "merge_actor_verified"

// mergeInFlightRegistry is the per-Server, process-local set of runs whose
// head-of-line queue merge THIS process has in flight: marked by
// mergeOnPassLocked after the merging stamp, cleared by the pump AFTER the
// outcome is applied (ruling R8c). A fresh Server starts empty, so a merging
// entry a PRIOR process left is not in it.
type mergeInFlightRegistry struct {
	mu   sync.Mutex
	runs map[uuid.UUID]int
}

func newMergeInFlightRegistry() *mergeInFlightRegistry {
	return &mergeInFlightRegistry{runs: map[uuid.UUID]int{}}
}

func (r *mergeInFlightRegistry) mark(runID uuid.UUID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[runID] = 1
}

func (r *mergeInFlightRegistry) clear(runID uuid.UUID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runs, runID)
}

func (r *mergeInFlightRegistry) has(runID uuid.UUID) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.runs[runID]
	return ok
}

// mergeQueueGateHoldRegistry records the re-claimed admissions the R7
// stage-live gate holds (entry id → admission epoch), process-local: the
// periodic pump re-checks the gate for exactly these entries (approval
// condition C5) rather than waiting out AdmissionStaleAfter. An entry
// admitted/advancing for any other reason is never re-run from here.
type mergeQueueGateHoldRegistry struct {
	mu      sync.Mutex
	entries map[uuid.UUID]int64
}

func newMergeQueueGateHoldRegistry() *mergeQueueGateHoldRegistry {
	return &mergeQueueGateHoldRegistry{entries: map[uuid.UUID]int64{}}
}

func (r *mergeQueueGateHoldRegistry) hold(e mergequeue.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[e.ID] = e.AdmissionEpoch
}

// take reports whether e (at its epoch) is gate-held and removes the mark.
func (r *mergeQueueGateHoldRegistry) take(e mergequeue.Entry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	epoch, ok := r.entries[e.ID]
	if !ok {
		return false
	}
	delete(r.entries, e.ID)
	return epoch == e.AdmissionEpoch
}

// mergeQueueMergedRowNames reports whether a merge_queue_merged row of e's
// run names e (payload entry_id): the resume's tail dedup (ruling R4).
func (s *Server) mergeQueueMergedRowNames(ctx context.Context, e mergequeue.Entry) (bool, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, CategoryMergeQueueMerged)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		var p struct {
			EntryID string `json:"entry_id"`
		}
		if row != nil && json.Unmarshal(row.Payload, &p) == nil && p.EntryID == e.ID.String() {
			return true, nil
		}
	}
	return false, nil
}

// mergeQueueResumeMerge resumes a LIVE entry whose last recorded step is
// merging or settling (see the file comment). It returns an outcome the pump
// applies after the lock; only the merged tail settles in-lock.
func (s *Server) mergeQueueResumeMerge(ctx context.Context, e mergequeue.Entry) mergeOnPassOutcome {
	held := func(reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassHeld, Reason: reason, Detail: detail, HeadSHA: e.AnchoredHeadSHA}
	}
	if s.cfg.MergeQueue == nil || s.cfg.GitHub == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return held(mergeOnPassReasonTransient, "the queue, GitHub client, run or audit repository is not wired")
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, e.RunID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			return mergeOnPassOutcome{Kind: mergeOnPassSettle, State: mergequeue.StateDropped,
				Reason: mergeCandidateQueueReasonRunTerminal, Detail: "the run no longer exists", HeadSHA: e.AnchoredHeadSHA}
		}
		return held(mergeOnPassReasonTransient, "the run could not be read: "+err.Error())
	}
	target, err := mergeQueueResumeForgeTarget(runRow)
	if err != nil {
		return held(mergeOnPassReasonTransient, err.Error())
	}
	var out mergeOnPassOutcome
	ran := false
	lockErr := s.withRunMergeLock(ctx, e.RunID, func(lctx context.Context) error {
		ran = true
		out = s.mergeQueueResumeMergeLocked(lctx, e, target)
		return nil
	})
	if !ran {
		reason := "the run's merge-intent lock could not be taken"
		if lockErr != nil {
			reason += ": " + lockErr.Error()
		}
		return held(mergeOnPassReasonLockUnavailable, reason)
	}
	return out
}

// mergeQueueResumeMergeLocked is the locked half of mergeQueueResumeMerge.
func (s *Server) mergeQueueResumeMergeLocked(ctx context.Context, e mergequeue.Entry, target mergeOnPassTarget) mergeOnPassOutcome {
	held := func(reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassHeld, Reason: reason, Detail: detail, HeadSHA: e.AnchoredHeadSHA}
	}
	settle := func(state mergequeue.State, reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassSettle, State: state, Reason: reason, Detail: detail, HeadSHA: e.AnchoredHeadSHA}
	}
	cur, ok := s.mergeQueueResumeLiveEntry(ctx, e, mergequeue.PhaseMerging, mergequeue.PhaseSettling)
	if !ok {
		return held(mergeOnPassReasonEntryChanged, "the queue entry changed before the resume could decide")
	}
	step := string(cur.Phase)
	// record appends the row BEFORE its action; false = hold, nothing changed.
	record := func(r mergeQueueResumed) bool {
		r.Step = step
		if _, err := s.appendMergeQueueResumed(ctx, cur, r); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "merge queue resume: merge_queue_resumed could not be appended; nothing is changed",
				slog.String("run_id", cur.RunID.String()), slog.String("action", r.Action), slog.String("error", err.Error()))
			return false
		}
		return true
	}
	unrecorded := func(what string) mergeOnPassOutcome {
		return held(mergeQueueResumeReasonUnrecorded, what+" could not be recorded; nothing was changed")
	}
	holdRow := func(reason, detail string, r mergeQueueResumed) mergeOnPassOutcome {
		r.Action, r.Reason = mergeQueueResumeActionHold, reason
		if !record(r) {
			return unrecorded("merge_queue_resumed")
		}
		return held(reason, detail)
	}
	claimFields := func(c mergequeue.DispatchClaim) mergeQueueResumed {
		return mergeQueueResumed{ClaimState: string(c.State), ClaimPath: c.Path, ClaimHeadSHA: c.HeadSHA}
	}

	ledger := s.mergeDispatchLedger()
	claim, hasClaim, err := ledger.MergeDispatch(ctx, cur.RunID)
	if err != nil {
		return held(mergeOnPassReasonTransient, "the run's dispatch claim could not be read: "+err.Error())
	}
	queueDispatched := hasClaim && claim.Path == mergeDispatchPathQueue && claim.State == mergequeue.DispatchDispatched

	pr, perr := s.mergeOnPassReadPR(ctx, target)
	if perr != nil {
		// An indeterminate read (C3): only a crash-left queue claim moves.
		if queueDispatched {
			return s.mergeQueueResumeOrphan(ctx, cur, claim, false, record, unrecorded, held)
		}
		return held(mergeOnPassReasonTransient, "reading the pull request failed: "+perr.Error())
	}
	merged := pr.Merged // ADR-093 D8: the resume re-read

	if merged {
		switch {
		case hasClaim && claim.HeadSHA != pr.HeadSHA:
			r := claimFields(claim)
			return holdRow(mergeOnPassReasonMergedClaimMismatch,
				"the pull request merged at "+pr.HeadSHA+" but the run's dispatch claim names "+claim.HeadSHA, r)
		case hasClaim && claim.Path == mergeDispatchPathQueue:
			return s.mergeQueueResumeMergedTail(ctx, cur, claim, pr, record, unrecorded, held)
		}
		// No claim, or another path's claim at the merged head: the merge is
		// a fact; the claim is untouched (C1).
		r := mergeQueueResumed{Action: mergeQueueResumeActionSettlePRMerged, Reason: mergeCandidateQueueReasonPRMerged}
		if hasClaim {
			r.ClaimState, r.ClaimPath, r.ClaimHeadSHA = string(claim.State), claim.Path, claim.HeadSHA
		}
		if !record(r) {
			return unrecorded("merge_queue_resumed")
		}
		return settle(mergequeue.StateMerged, mergeCandidateQueueReasonPRMerged, "the pull request is already merged")
	}

	closed := pr.State == "closed"
	if queueDispatched {
		if cur.Phase == mergequeue.PhaseSettling && !closed {
			r := claimFields(claim)
			r.NextStep, r.Remedy = "reconcile_merge", mergeQueueResumeSettlingRemedy
			return holdRow(mergeQueueResumeReasonSettlingUnconfirmed, mergeQueueResumeSettlingRemedy, r)
		}
		return s.mergeQueueResumeOrphan(ctx, cur, claim, closed, record, unrecorded, held)
	}
	if closed {
		if !record(mergeQueueResumed{Action: mergeQueueResumeActionDropPRClosed, Reason: mergeCandidateQueueReasonPRClosed}) {
			return unrecorded("merge_queue_resumed")
		}
		return settle(mergequeue.StateDropped, mergeCandidateQueueReasonPRClosed, "the pull request is closed")
	}
	if hasClaim && claim.Path == mergeDispatchPathQueue && claim.State == mergequeue.DispatchFailed &&
		claim.FailureCause == mergeQueueResumeReasonOrphanedByRestart {
		// A claim an earlier resume orphaned: the same hold (deduped), still
		// naming the operator re-arm.
		r := claimFields(claim)
		r.NextStep, r.Remedy = mergeQueueResumeNextOperatorRearm, mergeQueueResumeRearmRemedy
		return holdRow(mergeQueueResumeReasonDispatchUncertain, mergeQueueResumeRearmRemedy, r)
	}
	if hasClaim {
		// dispatch_failed, or another path's claim (C1): no ledger write.
		return holdRow(mergeOnPassReasonAlreadyDispatched,
			"a merge dispatch is already claimed for this run (state "+string(claim.State)+", path "+claim.Path+
				"); the resume makes no ledger write", claimFields(claim))
	}
	// No claim: no dispatch was ever handed to the forge. Redecide.
	if !record(mergeQueueResumed{Action: mergeQueueResumeActionRedecide, Reason: mergeQueueResumeReasonNoClaim, NextStep: string(mergequeue.PhasePassed)}) {
		return unrecorded("merge_queue_resumed")
	}
	if _, err := s.cfg.MergeQueue.SetPhase(ctx, cur.ID, cur.AdmissionEpoch, mergequeue.PhasePassed); err != nil {
		return held(mergeOnPassReasonEntryChanged, "the entry could not be returned to passed: "+err.Error())
	}
	return mergeOnPassOutcome{Kind: mergeOnPassRedecide, Reason: mergeQueueResumeReasonNoClaim,
		Detail: "no dispatch claim exists; ordinary merge-on-pass decides from scratch", HeadSHA: cur.AnchoredHeadSHA}
}

// mergeQueueResumeOrphan is the crash-left arm for a still-dispatched QUEUE
// claim whose PR is not merged (or could not be read): when the claim is
// older than mergeQueueCrashLeftClaimAge on the database clock, a strict
// merge_dispatch_settled {dispatch_failed, orphaned_by_restart} row, then
// the EXISTING MarkMergeDispatchFailed(orphaned_by_restart), then the hold
// row naming the operator re-arm (or, PR closed, the drop row and a
// pr_closed drop). A younger claim, or an unreadable database clock, holds
// with nothing written.
func (s *Server) mergeQueueResumeOrphan(ctx context.Context, cur mergequeue.Entry, claim mergequeue.DispatchClaim, closed bool,
	record func(mergeQueueResumed) bool, unrecorded func(string) mergeOnPassOutcome,
	held func(string, string) mergeOnPassOutcome) mergeOnPassOutcome {
	aged, why := s.mergeQueueClaimCrashLeft(ctx, claim)
	if !aged {
		return held(mergeQueueResumeReasonInProgress, why)
	}
	if err := s.appendMergeDispatchSettledStrict(ctx, claim, string(mergeDispatchUncertain), mergeQueueResumeReasonOrphanedByRestart); err != nil {
		return unrecorded("merge_dispatch_settled (" + err.Error() + ")")
	}
	if err := s.mergeDispatchLedger().MarkMergeDispatchFailed(ctx, claim.RunID, mergeQueueResumeReasonOrphanedByRestart); err != nil {
		return held(mergeOnPassReasonTransient, "the crash-left claim could not be marked dispatch_failed: "+err.Error())
	}
	r := mergeQueueResumed{ClaimState: string(mergequeue.DispatchFailed), ClaimPath: claim.Path, ClaimHeadSHA: claim.HeadSHA}
	if closed {
		r.Action, r.Reason = mergeQueueResumeActionDropPRClosed, mergeCandidateQueueReasonPRClosed
		if !record(r) {
			return unrecorded("merge_queue_resumed")
		}
		return mergeOnPassOutcome{Kind: mergeOnPassSettle, State: mergequeue.StateDropped, Reason: mergeCandidateQueueReasonPRClosed,
			Detail: "the pull request is closed; the crash-left claim is dispatch_failed", HeadSHA: cur.AnchoredHeadSHA}
	}
	r.Action, r.Reason = mergeQueueResumeActionHold, mergeQueueResumeReasonDispatchUncertain
	r.NextStep, r.Remedy = mergeQueueResumeNextOperatorRearm, mergeQueueResumeRearmRemedy
	if !record(r) {
		return unrecorded("merge_queue_resumed")
	}
	return held(mergeQueueResumeReasonDispatchUncertain, mergeQueueResumeRearmRemedy)
}

// mergeQueueClaimCrashLeft judges C2's age predicate on the DATABASE clock:
// the claim is crash-left when claimed_at < db_now - mergeQueueCrashLeftClaimAge.
// A store with no Resumer, or an unreadable clock, is not crash-left (fail
// closed: hold, nothing written).
func (s *Server) mergeQueueClaimCrashLeft(ctx context.Context, claim mergequeue.DispatchClaim) (bool, string) {
	rs, ok := s.cfg.MergeQueue.(mergequeue.Resumer)
	if !ok {
		return false, "the queue store cannot read the database clock, so the claim's age is undecidable"
	}
	now, err := rs.DatabaseNow(ctx)
	if err != nil {
		return false, "the database clock could not be read: " + err.Error()
	}
	if !claim.ClaimedAt.Before(now.Add(-mergeQueueCrashLeftClaimAge)) {
		return false, "the queue dispatch claim is younger than " + mergeQueueCrashLeftClaimAge.String() + "; a live dispatch may own it"
	}
	return true, ""
}

// mergeQueueResumeMergedTail settles a PR merged at its QUEUE claim's head:
// the tail references (ruling R4) are read back; a DECLARED approval gate
// with no matching merge_queue_authorized row holds (C6a); then the
// settle_merged row (merge_actor_verified, C6b), the strict reconciled_merged
// row for a dispatch_failed claim, and the merged tail with Resume=true.
func (s *Server) mergeQueueResumeMergedTail(ctx context.Context, cur mergequeue.Entry, claim mergequeue.DispatchClaim, pr *forge.PullRequest,
	record func(mergeQueueResumed) bool, unrecorded func(string) mergeOnPassOutcome,
	held func(string, string) mergeOnPassOutcome) mergeOnPassOutcome {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, cur.RunID)
	if err != nil {
		return held(mergeOnPassReasonTransient, "the run could not be re-read: "+err.Error())
	}
	refs, basis, err := s.mergeQueueResumeTailRefs(ctx, cur, claim.HeadSHA)
	if err != nil {
		return held(mergeOnPassReasonTransient, "the merged tail's references could not be read: "+err.Error())
	}
	declared, _, derr := forgeApprovalGateFor(runRow)
	if derr != nil || (declared && refs.authSeq == 0) {
		r := mergeQueueResumed{Action: mergeQueueResumeActionHold, Reason: mergeOnPassReasonAuthorizationUnrecorded,
			ClaimState: string(claim.State), ClaimPath: claim.Path, ClaimHeadSHA: claim.HeadSHA}
		if !record(r) {
			return unrecorded("merge_queue_resumed")
		}
		return held(mergeOnPassReasonAuthorizationUnrecorded,
			"the pull request merged at the claim's head but no merge_queue_authorized row (path queue) records an approval there; the resume does not settle merged without it")
	}
	if !declared {
		basis = forgeApprovalBasisNotRequired
	}
	if !record(mergeQueueResumed{Action: mergeQueueResumeActionSettleMerged, Reason: mergeQueueResumeReasonMergedAtClaim,
		ClaimState: string(claim.State), ClaimPath: claim.Path, ClaimHeadSHA: claim.HeadSHA,
		Extra: map[string]any{mergeQueueMergeActorVerifiedKey: mergeQueueMergeActorVerified(pr, claim)}}) {
		return unrecorded("merge_queue_resumed")
	}
	if claim.State == mergequeue.DispatchFailed {
		if err := s.appendMergeDispatchSettledStrict(ctx, claim, mergeDispatchReconciledMerged,
			"the forge reports the pull request merged at the claim's head"); err != nil {
			return unrecorded("merge_dispatch_settled (" + err.Error() + ")")
		}
	}
	return s.mergeOnPassMerged(ctx, cur, claim.HeadSHA, basis, refs, mergeOnPassTailOpts{Observed: true, Resume: true})
}

// mergeQueueMergeActorVerified is C6b: the forge client exposes the PR's
// merge commit and merged-by, but neither can be matched to the queue's own
// pinned merge (the pinned call returns no commit and the queue's forge actor
// is not recorded), so the resume never claims the merge was its own: false.
func mergeQueueMergeActorVerified(_ *forge.PullRequest, _ mergequeue.DispatchClaim) bool {
	return false
}

// mergeQueueResumeTailRefs reads the merged tail's references back from the
// chain (ruling R4): the surviving merge_verdict_recorded row, the intent row
// at the entry's anchor, and the newest merge_queue_authorized row whose
// payload path is queue and head_sha is the claim head (authSeq 0 and basis
// "" when there is none).
func (s *Server) mergeQueueResumeTailRefs(ctx context.Context, e mergequeue.Entry, claimHead string) (mergeOnPassMergedRefs, string, error) {
	var refs mergeOnPassMergedRefs
	verdicts, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, CategoryMergeVerdictRecorded)
	if err != nil {
		return refs, "", err
	}
	refs.verdictSeq, refs.verdictHead, _ = survivingMergeVerdict(verdicts)
	intent, found, err := s.mergeQueueIntentRow(ctx, e.RunID, mergeQueueAnchorFor(e))
	if err != nil {
		return refs, "", err
	}
	if found {
		refs.intentSeq = intent.Sequence
	}
	auths, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, CategoryMergeQueueAuthorized)
	if err != nil {
		return refs, "", err
	}
	basis := ""
	for _, row := range auths {
		var p struct {
			Path             string            `json:"path"`
			HeadSHA          string            `json:"head_sha"`
			Basis            string            `json:"basis"`
			CountedApprovals []countedApproval `json:"counted_approvals"`
		}
		if row == nil || json.Unmarshal(row.Payload, &p) != nil || p.Path != mergeDispatchPathQueue || p.HeadSHA != claimHead {
			continue
		}
		if row.Sequence > refs.authSeq {
			refs.authSeq, refs.counted, basis = row.Sequence, p.CountedApprovals, p.Basis
		}
	}
	return refs, basis, nil
}

// resumeMergeCandidateQueues is the boot pass (ADR-093 D8): for every active
// base, a pump whose FIRST admission re-claims a live admitted/advancing
// entry admitted before the boot instant (pumpMergeCandidateQueueBoot). It
// returns how many bases it pumped.
func (s *Server) resumeMergeCandidateQueues(ctx context.Context) int {
	if s.cfg.MergeQueue == nil {
		return 0
	}
	bases, err := s.cfg.MergeQueue.ActiveBases(ctx)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate queue: the boot resume could not list active bases; the next pump tick retries",
			slog.String("error", err.Error()))
		return 0
	}
	for _, b := range bases {
		s.pumpMergeCandidateQueueWith(ctx, b, true)
	}
	return len(bases)
}

// mergeQueueBootAdmit is the boot pump's first admission: AdmitNextReclaiming
// at the boot instant when the store is a Resumer and the instant is set,
// recording each re-claim (merge_queue_resumed reclaim) inside the queue
// transaction, so a failed append re-claims nothing (R8d). Otherwise AdmitNext.
func (s *Server) mergeQueueBootAdmit(ctx context.Context, base mergequeue.Base) (mergequeue.Admission, error) {
	rs, ok := s.cfg.MergeQueue.(mergequeue.Resumer)
	if !ok || s.mergeQueueBootAt.IsZero() {
		return s.cfg.MergeQueue.AdmitNext(ctx, base)
	}
	return rs.AdmitNextReclaiming(ctx, mergequeue.ReclaimRequest{
		Base: base, AdmittedBefore: s.mergeQueueBootAt,
		Record: func(ctx context.Context, tx pgx.Tx, reclaimed mergequeue.Entry) error {
			p, err := mergeQueueResumedParams(reclaimed, mergeQueueResumed{Step: string(reclaimed.Phase),
				Action: mergeQueueResumeActionReclaim, Reason: mergeQueueResumeReasonBootInstant})
			if err != nil {
				return err
			}
			_, err = audit.AppendChainedTx(ctx, tx, p)
			return err
		},
	})
}
