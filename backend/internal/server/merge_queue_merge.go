package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-093 slice 2 (D3/D6, E87.5 / #4296): MERGE-ON-PASS, the decision. The
// per-base merge queue merges its own head of line: for a LIVE entry in phase
// passed that carries the operator's merge intent, mergeQueueMergeOnPass
// re-checks eligibility at the verified head, then — under the run's
// merge-intent lock, the SAME lock the inline merge_run tail and the delegated
// may_merge arm take — re-reads the entry and the PR, refuses a run whose
// operator merged inline after the entry's intent, runs the merge overlap
// guard, re-probes the base tip, records the operator's verdict
// (record-before-commit, from the merge_queue_entered row at the entry's
// RECORDED intent sequence), re-checks changes_requested and a dismissed
// approval, claims the run's ONE merge dispatch on the D1 ledger, and merges
// through GateMerger with the head PINNED to the verified SHA.
//
// It returns an OUTCOME the pump applies with the admission-epoch CAS; the one
// queue write it makes itself is the merged settle after the forge accepted
// THIS decision's pinned merge (approval condition D3): that settle lands
// before the lock is released, so a later inline caller never reads a live,
// passed, intent-bearing entry for a merged PR. Contract:
// backend/internal/server/README.md § "Merge-on-pass (ADR-093 D3/D6)".

// Merge-on-pass audit categories (registered in audit/categories.go).
// INTERNAL: not issue-comment surfaces. merge_queue_advanced and
// merge_queue_verified are written by the pump (merge_candidate_queue.go).
const (
	CategoryMergeQueueAdvanced       = "merge_queue_advanced"
	CategoryMergeQueueVerified       = "merge_queue_verified"
	CategoryMergeQueueMerged         = "merge_queue_merged"
	CategoryMergeQueueOverlapRefused = "merge_queue_overlap_refused"
)

// mergeVerdictViaMergeQueue is the merge_verdict_recorded `via` value a
// merge-on-pass verdict carries; an inline merge_run row carries no `via`.
const mergeVerdictViaMergeQueue = "merge_queue"

// mergeDispatchPathQueue is the merge-dispatch ledger path merge-on-pass
// claims under (approval condition D1). The other two paths are "inline"
// (merge_run) and "delegated" (the may_merge arm).
const mergeDispatchPathQueue = "queue"

// mergeDispatchLedger is the D1 per-run merge-dispatch ledger seam: at most
// ONE dispatch claim per run, database-enforced by a UNIQUE run_id. The
// decision receives it injected (mergeOnPassRequest.Ledger) so it compiles
// before the store method exists; the queue-core slice wires the real
// mergequeue.Store method behind it.
//
// ClaimMergeDispatch claims the run's dispatch under the caller's merge-intent
// lock and BEFORE any forge merge call: claimed=false with a nil error is the
// store's ErrAlreadyDispatched (a claim already exists, dispatched or
// dispatch_failed), and the forge must then never be called. Any error is a
// failed claim: nothing is dispatched. MarkMergeDispatchFailed records a forge
// dispatch that failed AFTER a claim; the claim is updated, never deleted, so
// a retry needs the operator's explicit re-arm.
type mergeDispatchLedger interface {
	ClaimMergeDispatch(ctx context.Context, runID uuid.UUID, path, headSHA string) (claimed bool, err error)
	MarkMergeDispatchFailed(ctx context.Context, runID uuid.UUID, cause string) error
}

// mergeOnPassRequest is one merge-on-pass decision: the entry as the pump
// read it, the sequence of the merge_queue_entered row that recorded its
// intent (0 for a legacy entry: the newest row naming the base), and the D1
// dispatch ledger. A nil Ledger holds: no merge without a claim.
type mergeOnPassRequest struct {
	Entry          mergequeue.Entry
	IntentSequence int64
	Ledger         mergeDispatchLedger
}

// mergeOnPassKind is what the pump does with a decision.
type mergeOnPassKind string

// Merge-on-pass outcome kinds.
const (
	// mergeOnPassHeld: leave the entry as it is; the next pump re-decides.
	mergeOnPassHeld mergeOnPassKind = "held"
	// mergeOnPassMerged: the PR merged through this decision (or was
	// observed merged after its dispatch). Settled reports the decision
	// settled the entry merged inside the lock (D3).
	mergeOnPassMerged mergeOnPassKind = "merged"
	// mergeOnPassSettle: settle the entry State/Reason (SettleAt).
	mergeOnPassSettle mergeOnPassKind = "settle"
	// mergeOnPassReadvance: return the entry to held at its place (Unadmit)
	// so its next admission re-advances it onto the moved base or head.
	mergeOnPassReadvance mergeOnPassKind = "readvance"
)

// mergeOnPassOutcome is mergeQueueMergeOnPass's answer. State is set only on
// mergeOnPassSettle (and mergeOnPassMerged: StateMerged).
type mergeOnPassOutcome struct {
	Kind          mergeOnPassKind
	State         mergequeue.State
	Reason        string
	Detail        string
	ApprovalBasis string
	HeadSHA       string
	Settled       bool
}

// Merge-on-pass reasons. Hold reasons leave the entry; eject/drop reasons are
// the queue's (mergequeue README § "Reasons").
const (
	mergeOnPassReasonNoIntent              = "no_merge_intent"
	mergeOnPassReasonNotLive               = "not_live"
	mergeOnPassReasonNotPassed             = "not_passed"
	mergeOnPassReasonNoPinnedMerge         = "no_pinned_merge"
	mergeOnPassReasonTransient             = "transient"
	mergeOnPassReasonVerifyNotPassed       = "verify_not_passed"
	mergeOnPassReasonNotMergeable          = "not_mergeable_yet"
	mergeOnPassReasonApprovalUndetermined  = "transferred_approval_undetermined"
	mergeOnPassReasonEntryChanged          = "entry_changed"
	mergeOnPassReasonInlineMergeDispatched = "inline_merge_dispatched"
	mergeOnPassReasonOverlapRefused        = "overlap_refused"
	mergeOnPassReasonIntentRowMissing      = "intent_row_missing"
	mergeOnPassReasonVerdictUnrecorded     = "verdict_unrecorded"
	mergeOnPassReasonChangesRequested      = "changes_requested"
	mergeOnPassReasonApprovalDismissed     = "approval_dismissed"
	mergeOnPassReasonLedgerUnavailable     = "dispatch_ledger_unavailable"
	mergeOnPassReasonAlreadyDispatched     = "merge_already_dispatched"
	mergeOnPassReasonLockUnavailable       = "merge_lock_unavailable"
	mergeOnPassReasonBaseMoved             = "base_moved"
	mergeOnPassReasonHeadMoved             = "head_moved"
	mergeOnPassReasonForgeHeadMismatch     = "forge_head_mismatch"

	mergeCandidateQueueReasonPRMerged               = "pr_merged"
	mergeCandidateQueueReasonMergedByQueue          = "merged_by_queue"
	mergeCandidateQueueReasonForgeApprovalDismissed = "forge_approval_dismissed"
	mergeCandidateQueueReasonForgeMergeRefused      = "forge_merge_refused"
)

// mergeOnPassForgeCallTimeout bounds ONE forge read inside merge-on-pass, and
// mergeOnPassMergeCallTimeout the pinned merge call: both well under the
// merge-intent lock fn's 90s deadline, so a hung forge cannot consume the
// lock's whole budget (approval condition (f)).
const (
	mergeOnPassForgeCallTimeout = 10 * time.Second
	mergeOnPassMergeCallTimeout = 30 * time.Second
)

// headPinnedMerger is GateMerger asserted to HeadPinnedMerger, or nil when no
// GateMerger is bound or it has no pinned merge (merge-on-pass then holds and
// the queue never owns a merge: the operational stop).
func (s *Server) headPinnedMerger() HeadPinnedMerger {
	if s.cfg.GateMerger == nil {
		return nil
	}
	m, ok := s.cfg.GateMerger.(HeadPinnedMerger)
	if !ok {
		return nil
	}
	return m
}

// withRunMergeLock runs fn under the run's merge-intent lock when the queue is
// wired (mergequeue.Store.WithMergeIntentLock: fn's ctx carries the intent fn
// deadline, and fn must not take another pglock lock), else calls fn(ctx).
// Every merge dispatch path takes it: merge-on-pass, the inline merge_run
// tail, and the delegated may_merge arm.
func (s *Server) withRunMergeLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error {
	if s.cfg.MergeQueue == nil {
		return fn(ctx)
	}
	return s.cfg.MergeQueue.WithMergeIntentLock(ctx, runID, fn)
}

// mergeQueueOwnsMerge reports whether the merge queue owns the run's merge
// (approval condition D2): the run has an ACTIVE entry, live OR held, in any
// phase, carrying the operator's merge intent, and a head-pinned merge seam
// is bound (without one the queue can never merge, so it owns nothing). While
// it owns the merge the inline merge_run refuses and the delegated arm only
// observes. A queue read error is returned: the caller fails closed.
func (s *Server) mergeQueueOwnsMerge(ctx context.Context, runID uuid.UUID) (bool, error) {
	if s.cfg.MergeQueue == nil || s.headPinnedMerger() == nil {
		return false, nil
	}
	st, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, runID)
	if err != nil {
		return false, err
	}
	owned := found && st.Entry.State.Active() && st.Entry.MergeIntentAt != nil // ADR-093 D2: live or held, any phase
	return owned, nil
}

// forgeReportsMergeUnblocked reports a GitHub mergeable_state that positively
// says no review requirement holds the merge (MergeStateStatus CLEAN, UNSTABLE
// or HAS_HOOKS). Every other value (blocked, behind, dirty, unknown, empty)
// is not a positive answer.
func forgeReportsMergeUnblocked(state string) bool {
	switch state {
	case "clean", "unstable", "has_hooks":
		return true
	}
	return false
}

// mergeOnPassTarget is the run's resolved forge target.
type mergeOnPassTarget struct {
	scope  forge.CredentialScope
	repo   forge.RepoRef
	number int
}

// readPR reads the run's PR under mergeOnPassForgeCallTimeout.
func (s *Server) mergeOnPassReadPR(ctx context.Context, t mergeOnPassTarget) (*forge.PullRequest, error) {
	cctx, cancel := context.WithTimeout(ctx, mergeOnPassForgeCallTimeout)
	defer cancel()
	return s.cfg.GitHub.GetPullRequest(cctx, t.scope, t.repo, t.number)
}

// mergeQueueMergeOnPass decides one merge-on-pass for req.Entry (see the file
// comment). It never retries a forge refusal in a loop: a refusal ejects, a
// moved base or head re-advances, and every uncertainty holds.
func (s *Server) mergeQueueMergeOnPass(ctx context.Context, req mergeOnPassRequest) mergeOnPassOutcome {
	e := req.Entry
	held := func(reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassHeld, Reason: reason, Detail: detail, HeadSHA: e.AnchoredHeadSHA}
	}
	settle := func(state mergequeue.State, reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassSettle, State: state, Reason: reason, Detail: detail, HeadSHA: e.AnchoredHeadSHA}
	}
	readvance := func(reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassReadvance, Reason: reason, Detail: detail, HeadSHA: e.AnchoredHeadSHA}
	}

	if e.MergeIntentAt == nil { // ADR-093: merge-on-pass needs the operator's merge intent
		return held(mergeOnPassReasonNoIntent, "the entry carries no operator merge intent; fishhawk_merge_run records it")
	}
	if e.State != mergequeue.StateLive {
		return held(mergeOnPassReasonNotLive, "the entry is "+string(e.State)+", not live")
	}
	if e.Phase != mergequeue.PhasePassed {
		return held(mergeOnPassReasonNotPassed, "the entry is in phase "+string(e.Phase)+", not passed")
	}
	merger := s.headPinnedMerger()
	if merger == nil {
		return held(mergeOnPassReasonNoPinnedMerge, "no head-pinned merge seam is bound, so the queue cannot merge")
	}
	if s.cfg.GitHub == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return held(mergeOnPassReasonTransient, "the GitHub client, run or audit repository is not wired")
	}

	// 1. The run.
	runRow, err := s.cfg.RunRepo.GetRun(ctx, e.RunID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			return settle(mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal, "the run no longer exists")
		}
		return held(mergeOnPassReasonTransient, "the run could not be read: "+err.Error())
	}
	if runRow.State.IsTerminal() {
		return settle(mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal, "the run is "+string(runRow.State))
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return held(mergeOnPassReasonTransient, "the run has no installation to read its pull request")
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return held(mergeOnPassReasonTransient, "the run repo is unparseable: "+err.Error())
	}
	number := parsePRNumberFromURL(runRow.PullRequestURL)
	if number <= 0 {
		return held(mergeOnPassReasonTransient, "the run has no tracked pull request")
	}
	target := mergeOnPassTarget{scope: forge.FromGitHubInstallationID(*runRow.InstallationID), repo: repo, number: number}

	// 2. The PR.
	pr, err := s.mergeOnPassReadPR(ctx, target)
	if err != nil {
		return held(mergeOnPassReasonTransient, "reading the pull request failed: "+err.Error())
	}
	if pr.Merged {
		return settle(mergequeue.StateMerged, mergeCandidateQueueReasonPRMerged, "the pull request is already merged")
	}
	if pr.State == "closed" {
		return settle(mergequeue.StateDropped, mergeCandidateQueueReasonPRClosed, "the pull request is closed")
	}
	head := e.AnchoredHeadSHA
	if pr.HeadSHA != head {
		return readvance(mergeOnPassReasonHeadMoved, "the pull request head moved from "+head+" to "+pr.HeadSHA)
	}

	// 3. The verify state at the head.
	st, err := s.mergeCandidateVerifyState(ctx, runRow, head)
	if err != nil {
		return held(mergeOnPassReasonTransient, "the merge-candidate verify state could not be read: "+err.Error())
	}
	if st.State == mergeCandidateStateFailed {
		return settle(mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed, "the head has a failed merge-candidate verdict")
	}
	verified := st.State == mergeCandidateStatePassed || st.State == mergeCandidateStateNotRequired // ADR-093 D3: not_required counts as passed
	if !verified {
		return held(mergeOnPassReasonVerifyNotPassed, "the head's merge-candidate verify state is "+st.State)
	}

	// 4. Eligibility, merge variant, with the intent anchor (D6 transfer).
	anchor := mergeQueueIntentAnchor{Sequence: req.IntentSequence, BaseRef: e.BaseRef}
	elig := s.mergeQueueMergeEligibility(ctx, runRow, head, anchor)
	if !elig.Eligible {
		if elig.Undeterminable || onlyChecksNotGreen(elig) {
			return held(mergeOnPassReasonNotMergeable, mergeQueueReasonsText(elig.Reasons))
		}
		return settle(mergequeue.StateDropped, mergeCandidateQueueReasonNotMergeEligible,
			"the run is no longer merge-queue eligible ("+mergeQueueReasonsText(elig.Reasons)+"); fishhawk_merge_run re-enqueues it at the tail once it is")
	}

	// 5. The forge-side approval.
	actx, acancel := context.WithTimeout(ctx, mergeOnPassForgeCallTimeout)
	approval := s.approvalDismissedCheck(actx, runRow)
	acancel()
	if approval.Refusal != nil {
		return settle(mergequeue.StateEjected, mergeCandidateQueueReasonForgeApprovalDismissed, mergeOnPassApprovalDismissedRemedy)
	}
	holdTransferred := elig.ApprovalBasis == mergeQueueApprovalTransferred && !approval.Determined && !forgeReportsMergeUnblocked(pr.MergeableState) // ADR-093 D6 fail-closed
	if holdTransferred {
		return held(mergeOnPassReasonApprovalUndetermined,
			"the approval is transferred and the forge approval could not be determined ("+approval.Undetermined+")")
	}

	// 6. Best-effort audit-check republish at the head the merge lands.
	s.republishAuditCheckBeforeMergeAtHead(ctx, e.RunID, head)

	var out mergeOnPassOutcome
	ran := false
	lockErr := s.withRunMergeLock(ctx, e.RunID, func(lctx context.Context) error {
		ran = true
		out = s.mergeOnPassLocked(lctx, req, runRow, target, head, elig.ApprovalBasis, merger)
		return nil
	})
	if !ran {
		reason := "the run's merge-intent lock could not be taken"
		if lockErr != nil {
			reason += ": " + lockErr.Error()
		}
		return held(mergeOnPassReasonLockUnavailable, reason)
	}
	if lockErr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: merge-on-pass lock lapsed after its decision ran; the decision's writes stand",
			slog.String("run_id", e.RunID.String()), slog.String("outcome", string(out.Kind)),
			slog.String("error", lockErr.Error()))
	}
	if out.ApprovalBasis == "" {
		out.ApprovalBasis = elig.ApprovalBasis
	}
	return out
}

// mergeOnPassApprovalDismissedRemedy is the forge_approval_dismissed eject's
// operator remedy (ADR-093 Q2).
const mergeOnPassApprovalDismissedRemedy = "the forge reports the pull request's approval dismissed and not re-given, so GitHub would refuse the merge. Fishhawk cannot re-approve (self-approval is prohibited): re-approve under your own GitHub identity (gh pr review --approve), then re-invoke fishhawk_merge_run; the run rejoins the queue at the tail (ADR-093 Q2)"

// onlyChecksNotGreen reports a result whose every reason is checks_not_green
// (a pending check: hold, never drop).
func onlyChecksNotGreen(r mergeQueueEligibilityResult) bool {
	if len(r.Reasons) == 0 {
		return false
	}
	for _, x := range r.Reasons {
		if x.Code != mergeQueueReasonChecksNotGreen {
			return false
		}
	}
	return true
}

// mergeOnPassLocked is the locked half of mergeQueueMergeOnPass. ctx carries
// the merge-intent lock fn's deadline; every forge read takes its own
// sub-deadline.
func (s *Server) mergeOnPassLocked(ctx context.Context, req mergeOnPassRequest, runRow *run.Run,
	target mergeOnPassTarget, head, basis string, merger HeadPinnedMerger) mergeOnPassOutcome {
	e := req.Entry
	held := func(reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassHeld, Reason: reason, Detail: detail, HeadSHA: head, ApprovalBasis: basis}
	}
	readvance := func(reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassReadvance, Reason: reason, Detail: detail, HeadSHA: head, ApprovalBasis: basis}
	}
	settle := func(state mergequeue.State, reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassSettle, State: state, Reason: reason, Detail: detail, HeadSHA: head, ApprovalBasis: basis}
	}

	// 1. The entry, re-read under the lock.
	cur, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, e.RunID)
	if err != nil {
		return held(mergeOnPassReasonTransient, "the queue entry could not be re-read: "+err.Error())
	}
	if !found || cur.Entry.ID != e.ID || cur.Entry.State != mergequeue.StateLive || cur.Entry.Phase != mergequeue.PhasePassed ||
		cur.Entry.MergeIntentAt == nil || cur.Entry.AnchoredHeadSHA != head {
		return held(mergeOnPassReasonEntryChanged, "the queue entry changed before the merge could be decided")
	}

	// 2. The PR, re-read under the lock.
	pr, err := s.mergeOnPassReadPR(ctx, target)
	if err != nil {
		return held(mergeOnPassReasonTransient, "re-reading the pull request failed: "+err.Error())
	}
	if pr.Merged {
		return settle(mergequeue.StateMerged, mergeCandidateQueueReasonPRMerged, "the pull request is already merged")
	}
	if pr.State == "closed" {
		return settle(mergequeue.StateDropped, mergeCandidateQueueReasonPRClosed, "the pull request is closed")
	}
	if pr.HeadSHA != head {
		return readvance(mergeOnPassReasonHeadMoved, "the pull request head moved from "+head+" to "+pr.HeadSHA)
	}

	// 3. The intent row at the recorded sequence.
	intent, found, err := s.mergeQueueIntentRow(ctx, e.RunID, mergeQueueIntentAnchor{Sequence: req.IntentSequence, BaseRef: e.BaseRef})
	if err != nil {
		return held(mergeOnPassReasonTransient, "the merge_queue_entered row could not be read: "+err.Error())
	}
	if !found {
		return held(mergeOnPassReasonIntentRowMissing, "no merge_queue_entered row records this entry's merge intent")
	}
	intentSeq := intent.Sequence

	// 4. The inline-merge rule.
	verdicts, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, CategoryMergeVerdictRecorded)
	if err != nil {
		return held(mergeOnPassReasonTransient, "the merge verdicts could not be read: "+err.Error())
	}
	for _, row := range verdicts {
		v := decodeMergeOnPassVerdict(row)
		inlineAfterIntent := !v.ViaQueue && intentSeq > 0 && v.Sequence > intentSeq // ADR-093: the operator merged inline after this entry's intent
		if inlineAfterIntent {
			return held(mergeOnPassReasonInlineMergeDispatched,
				"the operator merged this run inline after the entry's merge intent; the forge's merge or the PR-resolution webhook settles the entry")
		}
	}

	// 5. The merge overlap guard (no override on this path).
	octx, ocancel := context.WithTimeout(ctx, mergeOnPassForgeCallTimeout)
	overlap := s.mergeOverlapGuard(octx, runRow) // ADR-093 merge-on-pass overlap guard
	ocancel()
	if overlap.refused() {
		if overlap.Status == http.StatusConflict {
			if rerr := s.recordMergeQueueOverlapRefusal(ctx, runRow, overlap); rerr != nil {
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
					"merge queue: merge_queue_overlap_refused could not be recorded; the merge is still held",
					slog.String("run_id", e.RunID.String()), slog.String("error", rerr.Error()))
			}
		}
		return held(mergeOnPassReasonOverlapRefused, overlap.Code+": "+overlap.Reason)
	}

	// 6. The base re-probe.
	cctx, ccancel := context.WithTimeout(ctx, mergeOnPassForgeCallTimeout)
	behind, err := s.cfg.GitHub.CompareCommits(cctx, target.scope, target.repo, head, e.BaseRef)
	ccancel()
	if err != nil {
		return held(mergeOnPassReasonTransient, "the base re-probe failed: "+err.Error())
	}
	baseMoved := len(behind) > 0 // ADR-093 D3 base re-probe
	if baseMoved {
		return readvance(mergeOnPassReasonBaseMoved, e.BaseRef+" moved past the verified head "+head)
	}

	// 7. Record before commit: the operator's verdict.
	verdictSeq, verdictHead, err := s.recordMergeOnPassVerdict(ctx, runRow, e, intent, head, verdicts)
	if err != nil {
		return held(mergeOnPassReasonVerdictUnrecorded, "the merge verdict could not be recorded (record before commit): "+err.Error())
	}

	// 8. Approval condition (d): changes_requested and a dismissed approval,
	// re-checked immediately before the pinned merge.
	if declared, _, derr := reviewApprovalGateForRun(runRow); derr != nil {
		return held(mergeOnPassReasonTransient, "the review approval gate could not be read: "+derr.Error())
	} else if declared {
		rctx, rcancel := context.WithTimeout(ctx, mergeOnPassForgeCallTimeout)
		reviews, rerr := s.listMergeSettledReviews(rctx, runRow)
		rcancel()
		if rerr != nil {
			return held(mergeOnPassReasonTransient, "re-reading the pull request reviews failed: "+rerr.Error())
		}
		if requesters := changesRequestingReviewers(reviews); len(requesters) > 0 {
			return held(mergeOnPassReasonChangesRequested, "a reviewer requested changes since the pre-lock check")
		}
	}
	actx, acancel := context.WithTimeout(ctx, mergeOnPassForgeCallTimeout)
	approval := s.approvalDismissedCheck(actx, runRow)
	acancel()
	if approval.Refusal != nil {
		return held(mergeOnPassReasonApprovalDismissed, "the forge approval was dismissed since the pre-lock check")
	}
	if basis == mergeQueueApprovalTransferred && !approval.Determined && !forgeReportsMergeUnblocked(pr.MergeableState) {
		return held(mergeOnPassReasonApprovalUndetermined,
			"the approval is transferred and the forge approval could not be determined ("+approval.Undetermined+")")
	}

	// 9. Approval condition D1: claim the run's ONE merge dispatch.
	if req.Ledger == nil {
		return held(mergeOnPassReasonLedgerUnavailable, "no merge-dispatch ledger is wired, so no dispatch can be claimed")
	}
	claimed, err := req.Ledger.ClaimMergeDispatch(ctx, e.RunID, mergeDispatchPathQueue, head)
	if err != nil {
		return held(mergeOnPassReasonTransient, "the merge dispatch could not be claimed: "+err.Error())
	}
	if !claimed {
		return held(mergeOnPassReasonAlreadyDispatched,
			"a merge dispatch is already claimed for this run (dispatched, or dispatch_failed awaiting the operator's re-arm); the forge was not called")
	}

	// 10. The pinned merge.
	mctx, mcancel := context.WithTimeout(ctx, mergeOnPassMergeCallTimeout)
	mergeErr := merger.MergePullRequestAtHead(mctx, runRow, head)
	mcancel()
	merged := func(observed bool) mergeOnPassOutcome {
		return s.mergeOnPassMerged(ctx, e, head, basis, verdictSeq, verdictHead, intentSeq, observed)
	}
	if mergeErr == nil {
		return merged(false)
	}
	observedMerged := func() bool {
		p, rerr := s.mergeOnPassReadPR(ctx, target)
		return rerr == nil && p.Merged
	}
	if observedMerged() {
		return merged(true)
	}
	s.markMergeOnPassDispatchFailed(ctx, req.Ledger, e.RunID, mergeErr)
	switch {
	case errors.Is(mergeErr, forge.ErrMergeConflict):
		return readvance(mergeOnPassReasonForgeHeadMismatch,
			"the forge refused the pinned merge because the head no longer matched "+head+": "+mergeErr.Error())
	case errors.Is(mergeErr, forge.ErrPullRequestNotMergeable), errors.Is(mergeErr, forge.ErrValidation),
		errors.Is(mergeErr, forge.ErrForbidden), errors.Is(mergeErr, forge.ErrNotFound):
		dctx, dcancel := context.WithTimeout(ctx, mergeOnPassForgeCallTimeout)
		again := s.approvalDismissedCheck(dctx, runRow)
		dcancel()
		if again.Refusal != nil {
			return settle(mergequeue.StateEjected, mergeCandidateQueueReasonForgeApprovalDismissed, mergeOnPassApprovalDismissedRemedy)
		}
		return settle(mergequeue.StateEjected, mergeCandidateQueueReasonForgeMergeRefused,
			"the forge refused the pinned merge ("+mergeErr.Error()+"); resolve it, then re-arm the dispatch and re-invoke fishhawk_merge_run")
	}
	return held(mergeOnPassReasonTransient, "the pinned merge failed: "+mergeErr.Error())
}

// markMergeOnPassDispatchFailed records a failed dispatch on the ledger,
// best-effort: the claim stands either way, so nothing re-dispatches.
func (s *Server) markMergeOnPassDispatchFailed(ctx context.Context, ledger mergeDispatchLedger, runID uuid.UUID, cause error) {
	if err := ledger.MarkMergeDispatchFailed(ctx, runID, cause.Error()); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: marking the merge dispatch failed did not land; the claim stands as dispatched",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
	}
}

// mergeOnPassVerdict is a decoded merge_verdict_recorded row.
type mergeOnPassVerdict struct {
	Sequence int64
	ViaQueue bool
	HeadSHA  string
}

func decodeMergeOnPassVerdict(row *audit.Entry) mergeOnPassVerdict {
	if row == nil {
		return mergeOnPassVerdict{}
	}
	var p struct {
		Via     string `json:"via"`
		HeadSHA string `json:"head_sha"`
	}
	_ = json.Unmarshal(row.Payload, &p)
	return mergeOnPassVerdict{Sequence: row.Sequence, ViaQueue: p.Via == mergeVerdictViaMergeQueue, HeadSHA: p.HeadSHA}
}

// recordMergeOnPassVerdict appends the operator's merge_verdict_recorded row
// for this merge, record-before-commit: the intent row's verdict prose and
// actor, via merge_queue, intent_sequence, head_sha and entry_id. An existing
// verdict row (the 0062 once-index admits one per run) is a RESUME: the
// earliest row stands and no row is appended; a once-index duplicate on the
// append is the same resume. It returns the surviving row's sequence and head
// (verdict_head_sha, "" when that row names none). Any other append error is
// returned: nothing merges.
func (s *Server) recordMergeOnPassVerdict(ctx context.Context, runRow *run.Run, e mergequeue.Entry,
	intent mergeQueueIntentRecord, head string, existing []*audit.Entry) (int64, string, error) {
	surviving := func(rows []*audit.Entry) (int64, string, bool) {
		var best *audit.Entry
		for _, r := range rows {
			if r != nil && (best == nil || r.Sequence < best.Sequence) {
				best = r
			}
		}
		if best == nil {
			return 0, "", false
		}
		return best.Sequence, decodeMergeOnPassVerdict(best).HeadSHA, true
	}
	if seq, vh, ok := surviving(existing); ok {
		return seq, vh, nil
	}
	payload, err := json.Marshal(map[string]any{
		"run_id":          runRow.ID.String(),
		"verdict":         intent.Verdict,
		"pr_url":          prURLOf(runRow),
		"delegated":       false,
		"via":             mergeVerdictViaMergeQueue,
		"intent_sequence": intent.Sequence,
		"head_sha":        head,
		"entry_id":        e.ID.String(),
	})
	if err != nil {
		return 0, "", err
	}
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	if intent.Entry != nil {
		if intent.Entry.ActorKind != nil {
			kind = *intent.Entry.ActorKind
		}
		if intent.Entry.ActorSubject != nil {
			subject = *intent.Entry.ActorSubject
		}
	}
	row, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runRow.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeVerdictRecorded,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	})
	if err == nil {
		return row.Sequence, head, nil
	}
	if !audit.IsMergeVerdictDuplicate(err) {
		return 0, "", err
	}
	winner, rerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, CategoryMergeVerdictRecorded)
	if rerr != nil {
		return 0, "", rerr
	}
	if seq, vh, ok := surviving(winner); ok {
		return seq, vh, nil
	}
	return 0, "", errors.New("merge verdict duplicate conflict but no recorded verdict found on re-read")
}

// mergeOnPassMerged is the merged tail: the merge_queue_merged row, then the
// merged settle INSIDE the lock (approval condition D3). A failed row append
// after the forge accepted the merge is logged at ERROR and the outcome is
// still merged (approval condition (c)); a failed settle leaves Settled false
// so the pump settles it.
func (s *Server) mergeOnPassMerged(ctx context.Context, e mergequeue.Entry, head, basis string,
	verdictSeq int64, verdictHead string, intentSeq int64, observed bool) mergeOnPassOutcome {
	out := mergeOnPassOutcome{Kind: mergeOnPassMerged, State: mergequeue.StateMerged,
		Reason: mergeCandidateQueueReasonMergedByQueue, HeadSHA: head, ApprovalBasis: basis}
	payload, _ := json.Marshal(map[string]any{
		"entry_id":         e.ID.String(),
		"base_ref":         e.BaseRef,
		"head_sha":         head,
		"verdict_sequence": verdictSeq,
		"verdict_head_sha": verdictHead,
		"intent_sequence":  intentSeq,
		"approval_basis":   basis,
		"observed_merged":  observed,
	})
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        e.RunID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeQueueMerged,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"merge queue: the forge accepted the pinned merge but merge_queue_merged could not be recorded; the entry is settled merged",
			slog.String("run_id", e.RunID.String()), slog.String("head_sha", head), slog.String("error", err.Error()))
	}
	res, err := s.cfg.MergeQueue.Settle(ctx, e.ID, mergequeue.StateMerged, mergeCandidateQueueReasonMergedByQueue)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: settling the merged entry inside the lock failed; the pump settles it",
			slog.String("run_id", e.RunID.String()), slog.String("error", err.Error()))
		return out
	}
	out.Settled = true
	if res.Settled {
		s.writeMergeCandidateQueueAudit(ctx, CategoryMergeCandidateQueueSettled, res.Entry,
			map[string]any{"from_state": string(e.State)})
	}
	return out
}

// recordMergeQueueOverlapRefusal appends merge_queue_overlap_refused for a
// merge-on-pass the overlap guard held, at the delegated_merge_overlap_refused
// detail level (counts, code, reason and head; never another run's id or
// path), unless the run's LATEST such row records the same decision: the pump
// re-decides a held entry every tick, so one row per distinct decision.
func (s *Server) recordMergeQueueOverlapRefusal(ctx context.Context, runRow *run.Run, o mergeOverlapResult) error {
	decision := delegatedOverlapDecision{
		HeadSHA:           o.HeadSHA,
		RefusedCode:       o.Code,
		RefusedReason:     o.Reason,
		BlockingCount:     len(o.Blockers),
		UndeterminedCount: len(o.Undetermined),
	}
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, CategoryMergeQueueOverlapRefused)
	if err != nil {
		return err
	}
	var latest *audit.Entry
	for _, row := range rows {
		if row != nil && (latest == nil || row.Sequence >= latest.Sequence) {
			latest = row
		}
	}
	if latest != nil {
		var prior delegatedOverlapDecision
		if json.Unmarshal(latest.Payload, &prior) == nil && prior == decision {
			return nil
		}
	}
	payload, err := json.Marshal(map[string]any{
		"run_id":             runRow.ID.String(),
		"pr_url":             prURLOf(runRow),
		"head_sha":           decision.HeadSHA,
		"refused_code":       decision.RefusedCode,
		"refused_reason":     decision.RefusedReason,
		"blocking_count":     decision.BlockingCount,
		"undetermined_count": decision.UndeterminedCount,
	})
	if err != nil {
		return err
	}
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	_, err = s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runRow.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeQueueOverlapRefused,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	})
	return err
}
