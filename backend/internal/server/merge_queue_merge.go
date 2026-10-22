package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// guard, re-probes the base tip, runs the ADR-112 forge-approval FOLD at the
// verified head and writes its merge_queue_authorized evidence row (step 6b,
// #4619), records the operator's verdict (record-before-commit, from the
// merge_queue_entered row at the entry's RECORDED intent sequence), claims the
// run's ONE merge dispatch on the D1 ledger, and merges through GateMerger
// with the head PINNED to the verified SHA.
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
// dispatch_failed; the returned claim is that existing one), and the forge
// must then never be called. Any error is a failed claim: nothing is
// dispatched. After a claim the forge outcome is classified ONCE
// (classifyMergeDispatchOutcome): a RELEASE outcome (the forge applied
// nothing) deletes the exact claim via ReleaseMergeDispatch, a compare-and-
// swap; an UNCERTAIN one moves it to dispatch_failed (MarkMergeDispatchFailed),
// never deleted, so only reconciliation or the operator re-arm clears it.
type mergeDispatchLedger interface {
	ClaimMergeDispatch(ctx context.Context, runID uuid.UUID, path, headSHA string) (claim mergequeue.DispatchClaim, claimed bool, err error)
	MarkMergeDispatchFailed(ctx context.Context, runID uuid.UUID, cause string) error
	ReleaseMergeDispatch(ctx context.Context, claim mergequeue.DispatchClaim) error
	// MergeDispatch reads the run's claim (found=false: none); a merged PR
	// is reconciled against it (mergeOnPassReconcileMerged).
	MergeDispatch(ctx context.Context, runID uuid.UUID) (mergequeue.DispatchClaim, bool, error)
}

// mergeOnPassReconcileMerged reconciles a pull request the forge reports
// merged against the run's D1 claim (ruling R1 reconciliation): settle merged
// only when there is no claim, or the claim names the merged head; a
// dispatch_failed claim that reconciles appends merge_dispatch_settled
// reconciled_merged. ok=false holds with reason: the claim could not be read,
// or it names a different head than the one that merged. A nil ledger has no
// claim to read.
func (s *Server) mergeOnPassReconcileMerged(ctx context.Context, ledger mergeDispatchLedger, runID uuid.UUID, mergedHead string) (ok bool, reason, detail string) {
	if ledger == nil {
		return true, "", ""
	}
	claim, found, err := ledger.MergeDispatch(ctx, runID)
	if err != nil {
		return false, mergeOnPassReasonTransient, "the pull request merged but the run's dispatch claim could not be read: " + err.Error()
	}
	if !found {
		return true, "", ""
	}
	if claim.HeadSHA != mergedHead {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: the pull request merged at a head the run's dispatch claim does not name; holding",
			slog.String("run_id", runID.String()), slog.String("claim_head_sha", claim.HeadSHA),
			slog.String("merged_head_sha", mergedHead), slog.String("claim_state", string(claim.State)))
		return false, mergeOnPassReasonMergedClaimMismatch,
			"the pull request merged at " + mergedHead + " but the run's dispatch claim (" + string(claim.State) + ", " + claim.Path + ") names " + claim.HeadSHA
	}
	if claim.State == mergequeue.DispatchFailed {
		s.appendMergeDispatchSettled(ctx, claim, mergeDispatchReconciledMerged, "the forge reports the pull request merged at the claim's head")
	}
	return true, "", ""
}

// mergeDispatchLedgerOutcome classifies a failed forge dispatch made after a D1
// claim (first-officer ruling R1).
type mergeDispatchLedgerOutcome string

const (
	// mergeDispatchRelease: the forge guarantees nothing was applied (our own
	// pre-call checks_pending refusal, a REST merge PUT 405 / 409 / 422, each a
	// typed sentinel), so the claim is released and a later dispatch may claim
	// again.
	mergeDispatchRelease mergeDispatchLedgerOutcome = "released"
	// mergeDispatchUncertain: the merge may have been applied (5xx, timeout,
	// transport error, ctx cancel, 403 that may be a rate limit, a generic
	// GraphQL error envelope, anything else), so the claim moves to
	// dispatch_failed.
	mergeDispatchUncertain mergeDispatchLedgerOutcome = "dispatch_failed"
)

// classifyMergeDispatchOutcome is the ONE classification of a failed forge
// dispatch after a claim, shared by the inline, delegated and queue paths.
func classifyMergeDispatchOutcome(err error) mergeDispatchLedgerOutcome {
	switch {
	case err == nil:
		return mergeDispatchUncertain
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return mergeDispatchUncertain
	case errors.Is(err, forge.ErrPullRequestUnstableStatus),
		errors.Is(err, forge.ErrPullRequestNotMergeable),
		errors.Is(err, forge.ErrMergeConflict),
		errors.Is(err, forge.ErrMergeUnprocessable):
		// Typed refusals only: a bare forge.ErrValidation also carries a
		// generic GraphQL error envelope (HTTP 200), which is uncertain.
		return mergeDispatchRelease
	}
	return mergeDispatchUncertain
}

// categoryMergeDispatchSettled records what became of a D1 dispatch claim
// after a failed forge dispatch, or on reconciliation: outcome released,
// dispatch_failed or reconciled_merged, with the path, head and reason.
const categoryMergeDispatchSettled = "merge_dispatch_settled"

// Reconciliation outcome on merge_dispatch_settled: a dispatch_failed claim
// whose PR the forge reports merged.
const mergeDispatchReconciledMerged = "reconciled_merged"

// mergeDispatchRecordTimeout bounds the post-dispatch ledger write, which
// runs on context.WithoutCancel so a cancelled request or an expired parent
// cannot strand a 'dispatched' claim.
const mergeDispatchRecordTimeout = 10 * time.Second

// settleMergeDispatchClaim classifies a failed forge dispatch after claim and
// records it on the ledger: release (a compare-and-swap DELETE of the exact
// claim) or dispatch_failed. The write runs on context.WithoutCancel(ctx) with
// mergeDispatchRecordTimeout, then an audit row (categoryMergeDispatchSettled)
// is appended best-effort; an uncertain outcome logs at WARN. A release that
// did not land is reported as uncertain: the claim was not deleted.
func (s *Server) settleMergeDispatchClaim(ctx context.Context, ledger mergeDispatchLedger, claim mergequeue.DispatchClaim, cause error) mergeDispatchLedgerOutcome {
	outcome := classifyMergeDispatchOutcome(cause)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mergeDispatchRecordTimeout)
	defer cancel()
	if outcome == mergeDispatchRelease {
		if err := ledger.ReleaseMergeDispatch(wctx, claim); err != nil {
			s.cfg.Logger.LogAttrs(wctx, slog.LevelWarn,
				"merge dispatch: releasing the claim did not land; marking it dispatch_failed instead",
				slog.String("run_id", claim.RunID.String()), slog.String("error", err.Error()))
			outcome = mergeDispatchUncertain
		}
	}
	if outcome == mergeDispatchUncertain {
		if err := ledger.MarkMergeDispatchFailed(wctx, claim.RunID, cause.Error()); err != nil {
			s.cfg.Logger.LogAttrs(wctx, slog.LevelError,
				"merge dispatch: marking the claim dispatch_failed did not land; the claim stands as dispatched",
				slog.String("run_id", claim.RunID.String()), slog.String("error", err.Error()))
		}
		s.cfg.Logger.LogAttrs(wctx, slog.LevelWarn,
			"merge dispatch: the forge outcome is uncertain; the claim is dispatch_failed and a re-dispatch needs reconciliation or the operator re-arm",
			slog.String("run_id", claim.RunID.String()), slog.String("path", claim.Path),
			slog.String("head_sha", claim.HeadSHA), slog.String("error", cause.Error()))
	}
	s.appendMergeDispatchSettled(wctx, claim, string(outcome), cause.Error())
	return outcome
}

// appendMergeDispatchSettled appends the categoryMergeDispatchSettled row,
// best-effort (the ledger write is the authority).
func (s *Server) appendMergeDispatchSettled(ctx context.Context, claim mergequeue.DispatchClaim, outcome, reason string) {
	if s.cfg.AuditRepo == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"outcome": outcome, "path": claim.Path, "head_sha": claim.HeadSHA, "reason": reason,
	})
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        claim.RunID,
		Timestamp:    time.Now().UTC(),
		Category:     categoryMergeDispatchSettled,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "merge dispatch: the merge_dispatch_settled row did not land",
			slog.String("run_id", claim.RunID.String()), slog.String("error", err.Error()))
	}
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
	mergeOnPassReasonEntryChanged          = "entry_changed"
	mergeOnPassReasonMergedClaimMismatch   = "merged_claim_head_mismatch"
	mergeOnPassReasonInlineMergeDispatched = "inline_merge_dispatched"
	mergeOnPassReasonOverlapRefused        = "overlap_refused"
	mergeOnPassReasonIntentRowMissing      = "intent_row_missing"
	mergeOnPassReasonVerdictUnrecorded     = "verdict_unrecorded"
	mergeOnPassReasonLedgerUnavailable     = "dispatch_ledger_unavailable"
	mergeOnPassReasonAlreadyDispatched     = "merge_already_dispatched"
	mergeOnPassReasonLockUnavailable       = "merge_lock_unavailable"
	mergeOnPassReasonBaseMoved             = "base_moved"
	mergeOnPassReasonHeadMoved             = "head_moved"
	mergeOnPassReasonForgeHeadMismatch     = "forge_head_mismatch"
	// mergeOnPassReasonAuthorizationUnrecorded: the fold approved but the
	// merge_queue_authorized append failed (ADR-112 R4): no verdict row, no
	// claim; the next pump re-decides.
	mergeOnPassReasonAuthorizationUnrecorded = "authorization_unrecorded"

	mergeCandidateQueueReasonPRMerged               = "pr_merged"
	mergeCandidateQueueReasonMergedByQueue          = "merged_by_queue"
	mergeCandidateQueueReasonForgeApprovalDismissed = "forge_approval_dismissed"
	mergeCandidateQueueReasonForgeMergeRefused      = "forge_merge_refused"

	// ADR-112 interim posture (#4619, until #4632's bounded wait then yield):
	// a non-approved fold at the head of the line DROPS or EJECTS with one
	// of these named reasons and the line continues.
	mergeCandidateQueueReasonForgeApprovalVetoed              = "forge_approval_vetoed"
	mergeCandidateQueueReasonForgeApprovalReviewRequired      = "forge_approval_review_required"
	mergeCandidateQueueReasonForgeApprovalAbsent              = "forge_approval_absent"
	mergeCandidateQueueReasonForgeApprovalUndetermined        = "forge_approval_undetermined"
	mergeCandidateQueueReasonForgeApprovalReviewsTruncated    = "forge_approval_reviews_truncated"
	mergeCandidateQueueReasonForgeApprovalProviderUnsupported = "forge_approval_provider_unsupported"
	// mergeCandidateQueueReasonForgeReviewRequired is the post-refusal eject
	// (ADR-112 R3 row 4): a TEMPORARY stand-in for #4632's yield.
	mergeCandidateQueueReasonForgeReviewRequired = "forge_review_required"
)

// mergeOnPassFoldSettle is the ADR-112 INTERIM table (#4619): the queue
// state and reason a non-approved fold result settles with. vetoed and
// review_required DROP; every other result EJECTS. #4632 converts both into a
// bounded wait followed by a yield.
func mergeOnPassFoldSettle(result string) (mergequeue.State, string) {
	switch result {
	case forgeApprovalVetoed:
		return mergequeue.StateDropped, mergeCandidateQueueReasonForgeApprovalVetoed
	case forgeApprovalReviewRequired:
		return mergequeue.StateDropped, mergeCandidateQueueReasonForgeApprovalReviewRequired
	case forgeApprovalDismissed:
		return mergequeue.StateEjected, mergeCandidateQueueReasonForgeApprovalDismissed
	case forgeApprovalAbsent:
		return mergequeue.StateEjected, mergeCandidateQueueReasonForgeApprovalAbsent
	case forgeApprovalReviewsTruncated:
		return mergequeue.StateEjected, mergeCandidateQueueReasonForgeApprovalReviewsTruncated
	case forgeApprovalProviderUnsupported:
		return mergequeue.StateEjected, mergeCandidateQueueReasonForgeApprovalProviderUnsupported
	}
	return mergequeue.StateEjected, mergeCandidateQueueReasonForgeApprovalUndetermined
}

// mergeOnPassFoldDetail is the settle detail for a non-approved fold: the
// sanitised fold detail, the next step and the interim re-entry route.
func mergeOnPassFoldDetail(d forgeApprovalDecision) string {
	return d.Detail + ". " + forgeApprovalNextStepProse(d) +
		" Fishhawk cannot approve on the operator's behalf (self-approval is prohibited); after it clears, re-invoke fishhawk_merge_run and the run rejoins the queue at the tail (ADR-112 interim posture until #4632)."
}

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
		if ok, reason, detail := s.mergeOnPassReconcileMerged(ctx, req.Ledger, e.RunID, pr.HeadSHA); !ok {
			return held(reason, detail)
		}
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

	// 5. Best-effort audit-check republish at the head the merge lands. The
	// forge-side approval is judged ONLY in-lock (step 6b, ADR-112): no
	// pre-lock approval read exists.
	s.republishAuditCheckBeforeMergeAtHead(ctx, e.RunID, head)

	var out mergeOnPassOutcome
	ran := false
	lockErr := s.withRunMergeLock(ctx, e.RunID, func(lctx context.Context) error {
		ran = true
		out = s.mergeOnPassLocked(lctx, req, runRow, target, head, merger)
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
	return out
}

// mergeOnPassReviewRequiredRemedy is the post-refusal forge_review_required
// eject's operator remedy (ADR-112 R3 row 4, temporary until #4632).
const mergeOnPassReviewRequiredRemedy = "the forge refused the pinned merge and reports the pull request still needs review or is blocked (reviewDecision %s, mergeStateStatus %s). Fishhawk cannot approve on your behalf (self-approval is prohibited): approve under your own GitHub identity (gh pr review --approve), or resolve what else GitHub reports blocking, then re-invoke fishhawk_merge_run; the run rejoins the queue at the tail (temporary until #4632)"

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
	target mergeOnPassTarget, head string, merger HeadPinnedMerger) mergeOnPassOutcome {
	e := req.Entry
	// basis is the fold's (step 6b); "" until the fold ran.
	basis := ""
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
		if ok, reason, detail := s.mergeOnPassReconcileMerged(ctx, req.Ledger, e.RunID, pr.HeadSHA); !ok {
			return held(reason, detail)
		}
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

	// 6b. ADR-112 (#4619): the ONE forge-approval fold at the verified head,
	// AFTER the base re-probe and BEFORE the verdict, the single source of
	// approvals and vetoes on this path. A snapshot transport failure holds
	// (a read failure, not a decision: nothing is written); a head the
	// snapshot reports moved re-advances (H1); every other non-approved
	// result settles per the interim table with no verdict row, no
	// authorization row and no claim. An approved fold on a declared gate
	// writes merge_queue_authorized BEFORE the verdict; a failed append holds.
	fold, ferr := s.forgeApprovalFold(ctx, runRow, head)
	if ferr != nil {
		return held(mergeOnPassReasonTransient, sanitizeForgeReadDetail("review_snapshot", ferr.Error()))
	}
	basis = fold.Basis
	switch {
	case fold.Merged:
		if ok, reason, detail := s.mergeOnPassReconcileMerged(ctx, req.Ledger, e.RunID, fold.ObservedHead); !ok {
			return held(reason, detail)
		}
		return settle(mergequeue.StateMerged, mergeCandidateQueueReasonPRMerged, "the pull request is already merged")
	case fold.Reason == forgeApprovalReasonHeadMoved:
		return readvance(mergeOnPassReasonHeadMoved, fold.Detail)
	case !fold.approved():
		state, reason := mergeOnPassFoldSettle(fold.Result)
		return settle(state, reason, mergeOnPassFoldDetail(fold))
	}
	var authSeq int64
	if fold.Declared {
		authSeq, err = s.writeMergeQueueAuthorized(ctx, runRow, fold, forgeApprovalAuthorization{Path: mergeDispatchPathQueue, IntentSequence: intentSeq})
		if err != nil {
			return held(mergeOnPassReasonAuthorizationUnrecorded,
				"the forge approval qualified but merge_queue_authorized could not be recorded (evidence before verdict): "+err.Error())
		}
	}

	// 7. Record before commit: the operator's verdict.
	verdictSeq, verdictHead, err := s.recordMergeOnPassVerdict(ctx, runRow, e, intent, head, verdicts)
	if err != nil {
		return held(mergeOnPassReasonVerdictUnrecorded, "the merge verdict could not be recorded (record before commit): "+err.Error())
	}

	// 8. Approval condition D1: claim the run's ONE merge dispatch.
	if req.Ledger == nil {
		return held(mergeOnPassReasonLedgerUnavailable, "no merge-dispatch ledger is wired, so no dispatch can be claimed")
	}
	merged := func(observed bool) mergeOnPassOutcome {
		return s.mergeOnPassMerged(ctx, e, head, basis, mergeOnPassMergedRefs{verdictSeq: verdictSeq, verdictHead: verdictHead,
			intentSeq: intentSeq, authSeq: authSeq, counted: fold.Counted}, observed)
	}
	observedMerged := func() bool {
		p, rerr := s.mergeOnPassReadPR(ctx, target)
		return rerr == nil && p.Merged
	}
	claim, claimed, err := req.Ledger.ClaimMergeDispatch(ctx, e.RunID, mergeDispatchPathQueue, head)
	if err != nil {
		return held(mergeOnPassReasonTransient, "the merge dispatch could not be claimed: "+err.Error())
	}
	if !claimed {
		// Reconciliation (ruling R1) is the pre-lock and in-lock PR reads
		// above: a PR the forge reports merged settles pr_merged before the
		// claim. Reaching here, the PR is unmerged: hold and surface it.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: a merge dispatch is already claimed for this run; the forge was not called",
			slog.String("run_id", e.RunID.String()), slog.String("claim_state", string(claim.State)),
			slog.String("claim_path", claim.Path), slog.String("claim_head_sha", claim.HeadSHA),
			slog.String("failure_cause", claim.FailureCause))
		return held(mergeOnPassReasonAlreadyDispatched,
			"a merge dispatch is already claimed for this run (state "+string(claim.State)+", path "+claim.Path+
				"; dispatch_failed awaits reconciliation or the operator's re-arm); the forge was not called")
	}

	// 9. The pinned merge.
	mctx, mcancel := context.WithTimeout(ctx, mergeOnPassMergeCallTimeout)
	mergeErr := merger.MergePullRequestAtHead(mctx, runRow, head)
	mcancel()
	if mergeErr == nil {
		return merged(false)
	}
	if observedMerged() {
		return merged(true)
	}
	if s.settleMergeDispatchClaim(ctx, req.Ledger, claim, mergeErr) == mergeDispatchUncertain {
		return held(mergeOnPassReasonTransient, "the pinned merge failed with an uncertain outcome; the dispatch claim is dispatch_failed: "+mergeErr.Error())
	}
	switch {
	case errors.Is(mergeErr, forge.ErrMergeConflict):
		return readvance(mergeOnPassReasonForgeHeadMismatch,
			"the forge refused the pinned merge because the head no longer matched "+head+": "+mergeErr.Error())
	case errors.Is(mergeErr, forge.ErrPullRequestNotMergeable), errors.Is(mergeErr, forge.ErrMergeUnprocessable):
		if decision, status, ok := s.mergeOnPassRefusalNeedsReview(ctx, runRow); ok {
			return settle(mergequeue.StateEjected, mergeCandidateQueueReasonForgeReviewRequired,
				fmt.Sprintf(mergeOnPassReviewRequiredRemedy, nonEmpty(decision, "null"), nonEmpty(status, "null")))
		}
		return settle(mergequeue.StateEjected, mergeCandidateQueueReasonForgeMergeRefused,
			"the forge refused the pinned merge ("+mergeErr.Error()+"); the dispatch claim was released; resolve it, then re-invoke fishhawk_merge_run")
	}
	return held(mergeOnPassReasonTransient, "the pinned merge failed: "+mergeErr.Error())
}

// mergeOnPassRefusalNeedsReview is ADR-112 R3 rows 4/5 after a TYPED forge
// refusal whose claim was released: ONE fresh review snapshot (no permission
// reads). ok=true ONLY when reviewDecision is REVIEW_REQUIRED or
// CHANGES_REQUESTED, or mergeStateStatus is BLOCKED (row 4, forge_review_required).
// A null reviewDecision (a base without required-review protection) is NOT
// "not approved". A non-GitHub forge, an unaddressable run or a snapshot read
// error is ok=false (row 5, forge_merge_refused). It applies with or without a
// declared gate.
func (s *Server) mergeOnPassRefusalNeedsReview(ctx context.Context, runRow *run.Run) (reviewDecision, mergeStateStatus string, ok bool) {
	reader, unsupported, _ := s.forgeApprovalReaderFor(runRow)
	if unsupported || reader == nil {
		return "", "", false
	}
	rctx, cancel := context.WithTimeout(ctx, mergeOnPassForgeCallTimeout)
	defer cancel()
	snap, err := reader.ReviewSnapshot(rctx)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "merge queue: post-refusal review snapshot unreadable; ejecting forge_merge_refused",
			slog.String("run_id", runRow.ID.String()), slog.String("detail", sanitizeForgeReadDetail("review_snapshot", err.Error())))
		return "", "", false
	}
	switch {
	case snap.ReviewDecision == "REVIEW_REQUIRED", snap.ReviewDecision == "CHANGES_REQUESTED", snap.MergeStateStatus == "BLOCKED":
		return snap.ReviewDecision, snap.MergeStateStatus, true
	}
	return snap.ReviewDecision, snap.MergeStateStatus, false
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

// mergeOnPassMergedRefs are the chain references merge_queue_merged records:
// the verdict row, the intent row and (ADR-112 R4) THIS attempt's
// merge_queue_authorized row with the fold's counted approvals.
type mergeOnPassMergedRefs struct {
	verdictSeq  int64
	verdictHead string
	intentSeq   int64
	authSeq     int64
	counted     []countedApproval
}

// mergeOnPassMerged is the merged tail: the merge_queue_merged row, then the
// merged settle INSIDE the lock (approval condition D3). A failed row append
// after the forge accepted the merge is logged at ERROR and the outcome is
// still merged (approval condition (c)); a failed settle leaves Settled false
// so the pump settles it.
func (s *Server) mergeOnPassMerged(ctx context.Context, e mergequeue.Entry, head, basis string,
	refs mergeOnPassMergedRefs, observed bool) mergeOnPassOutcome {
	out := mergeOnPassOutcome{Kind: mergeOnPassMerged, State: mergequeue.StateMerged,
		Reason: mergeCandidateQueueReasonMergedByQueue, HeadSHA: head, ApprovalBasis: basis}
	fields := map[string]any{
		"entry_id":         e.ID.String(),
		"base_ref":         e.BaseRef,
		"head_sha":         head,
		"verdict_sequence": refs.verdictSeq,
		"verdict_head_sha": refs.verdictHead,
		"intent_sequence":  refs.intentSeq,
		"approval_basis":   basis,
		"observed_merged":  observed,
	}
	if refs.authSeq > 0 {
		fields["authorization_sequence"] = refs.authSeq
	}
	if len(refs.counted) > 0 {
		q := refs.counted[0]
		fields["qualifying_reviewer"] = map[string]any{"login": q.Login, "actor_id": q.ActorID, "permission": q.Permission}
	}
	payload, _ := json.Marshal(fields)
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
