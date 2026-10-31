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
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
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
// A non-approved fold never drops or ejects an entry carrying intent: it
// waits at the head, then yields (ADR-112 R3, merge_queue_yield.go). Nor
// does a retryable merge-gate leg at step 4 (docs/adr/112 A3.2, #4780): a red
// or cancelled required check (A3.1(a)) yields merge_checks_red /
// retrigger_ci at once; pending, unreported or unreadable checks (A3.1(b))
// and a settling implement-review round (A3.1(c)) hold the line on a
// per-admission-epoch merge-gate window (mergeOnPassEnforceGateWait,
// Store.GateWait), then yield merge_checks_pending / merge_review_pending.
// These A3 paths all run BEFORE any dispatch claim; after a claim, ADR-112
// R3 rows 4 and 5 (a forge refusal) are unchanged.
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
// (classifyMergeDispatchOutcome): a RELEASE outcome (the forge guarantees it
// applied nothing: a typed refusal, and for a head mismatch only the precheck
// and rest_sha bases) deletes the exact claim via ReleaseMergeDispatch, a
// compare-and-swap; an UNCERTAIN one (a graphql_reread head mismatch
// included, #4749) moves it to dispatch_failed (MarkMergeDispatchFailed),
// never deleted, so only reconciliation or the operator re-arm clears it.
// ADR-109 Amendment A1 (#4711) adds ONE automatic release: merge-on-pass step
// 8 releases a crash-left QUEUE claim still 'dispatched' (the same CAS
// DELETE) when the PR reads open and unmerged under the lock
// (mergeOnPassReleaseOrphanedClaim); a dispatch_failed claim is never
// released there.
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
// reconciled_merged. ok=false holds with reason: the claim could not be read
// (merged_claim_unread, #4748 M1: never transient, so the hold carries no
// approval-wait ExpiryYield and an already-merged entry is never yielded), or
// it names a different head than the one that merged. A nil ledger has no
// claim to read.
func (s *Server) mergeOnPassReconcileMerged(ctx context.Context, ledger mergeDispatchLedger, runID uuid.UUID, mergedHead string) (ok bool, reason, detail string) {
	ok, reason, detail, err := s.mergeOnPassReconcileMergedStrict(ctx, ledger, runID, mergedHead)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "merge dispatch: the merge_dispatch_settled row did not land",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
	}
	return ok, reason, detail
}

// mergeOnPassReconcileMergedStrict is mergeOnPassReconcileMerged returning the
// reconciled_merged row's append error (ok stays true: the reconcile decision
// stands); the ADR-093 D8 resume holds on it (fail closed, ruling R8d).
func (s *Server) mergeOnPassReconcileMergedStrict(ctx context.Context, ledger mergeDispatchLedger, runID uuid.UUID, mergedHead string) (ok bool, reason, detail string, appendErr error) {
	if ledger == nil {
		return true, "", "", nil
	}
	claim, found, err := ledger.MergeDispatch(ctx, runID)
	if err != nil {
		return false, mergeOnPassReasonMergedClaimUnread, "the pull request merged but the run's dispatch claim could not be read: " + err.Error(), nil
	}
	if !found {
		return true, "", "", nil
	}
	if claim.HeadSHA != mergedHead {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: the pull request merged at a head the run's dispatch claim does not name; holding",
			slog.String("run_id", runID.String()), slog.String("claim_head_sha", claim.HeadSHA),
			slog.String("merged_head_sha", mergedHead), slog.String("claim_state", string(claim.State)))
		return false, mergeOnPassReasonMergedClaimMismatch,
			"the pull request merged at " + mergedHead + " but the run's dispatch claim (" + string(claim.State) + ", " + claim.Path + ") names " + claim.HeadSHA, nil
	}
	if claim.State == mergequeue.DispatchFailed {
		appendErr = s.appendMergeDispatchSettledStrict(ctx, claim, mergeDispatchReconciledMerged, "the forge reports the pull request merged at the claim's head")
	}
	return true, "", "", appendErr
}

// mergeDispatchLedgerOutcome classifies a failed forge dispatch made after a D1
// claim (first-officer ruling R1).
type mergeDispatchLedgerOutcome string

const (
	// mergeDispatchRelease: the forge guarantees nothing was applied (our own
	// pre-call checks_pending refusal, a REST merge PUT 405 / 409 / 422, each a
	// typed sentinel, and a head mismatch only on the precheck or rest_sha
	// basis), so the claim is released and a later dispatch may claim again.
	mergeDispatchRelease mergeDispatchLedgerOutcome = "released"
	// mergeDispatchUncertain: the merge may have been applied (5xx, timeout,
	// transport error, ctx cancel, 403 that may be a rate limit, a generic
	// GraphQL error envelope, a head mismatch on the graphql_reread or an
	// empty or unrecognised basis (#4749), anything else), so the claim moves
	// to dispatch_failed.
	mergeDispatchUncertain mergeDispatchLedgerOutcome = "dispatch_failed"
)

// mergeDispatchHeadMismatchUncertain reports whether err carries a
// *githubclient.HeadMismatchError whose basis is not a forge guarantee that
// nothing applied (graphql_reread, an empty or unrecognised basis): such a
// head mismatch is UNCERTAIN (#4749). A GraphQL execution error can
// accompany an applied mutation, and a moved head proves only that the head
// moved, not that the enable was refused. A bare forge.ErrMergeConflict with
// no HeadMismatchError (the REST merge PUT 409, GitHub's documented sha
// refusal) is not a head-mismatch detail and is decided by the typed-refusal
// case.
func mergeDispatchHeadMismatchUncertain(err error) bool {
	var hm *githubclient.HeadMismatchError
	if !errors.As(err, &hm) {
		return false
	}
	return !mergeHeadMovedNothingApplied(hm.Basis)
}

// classifyMergeDispatchOutcome is the ONE classification of a failed forge
// dispatch after a claim, shared by the inline, delegated and queue paths.
func classifyMergeDispatchOutcome(err error) mergeDispatchLedgerOutcome {
	switch {
	case err == nil:
		return mergeDispatchUncertain
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return mergeDispatchUncertain
	case mergeDispatchHeadMismatchUncertain(err):
		// Before the typed-refusal case: the head mismatch rides on
		// ErrMergeConflict, which would otherwise release.
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
// dispatch_failed or reconciled_merged, with the path, head and reason. A
// settle whose cause carries a *githubclient.HeadMismatchError also records
// refusal_basis, expected_head_sha and observed_head_sha (#4749).
const categoryMergeDispatchSettled = "merge_dispatch_settled"

// Reconciliation outcome on merge_dispatch_settled: a dispatch_failed claim
// whose PR the forge reports merged.
const mergeDispatchReconciledMerged = "reconciled_merged"

// mergeDispatchReasonReleasedOrphanedClaim is the merge_dispatch_settled
// reason (outcome released) for ADR-109 Amendment A1's release of an orphaned
// queue claim in merge-on-pass step 8 (#4711).
const mergeDispatchReasonReleasedOrphanedClaim = "released_orphaned_claim"

// mergeDispatchRecordTimeout bounds the post-dispatch ledger write, which
// runs on context.WithoutCancel so a cancelled request or an expired parent
// cannot strand a 'dispatched' claim.
const mergeDispatchRecordTimeout = 10 * time.Second

// settleMergeDispatchClaim classifies a failed forge dispatch after claim and
// records it on the ledger: release (a compare-and-swap DELETE of the exact
// claim) or dispatch_failed. The write runs on context.WithoutCancel(ctx) with
// mergeDispatchRecordTimeout, then an audit row (categoryMergeDispatchSettled)
// is appended best-effort; an uncertain outcome logs at WARN. A release that
// did not land is reported as uncertain: the claim was not deleted. The
// returned outcome is the ledger state the claim was LEFT in, so a caller
// reporting the claim reads it, never a re-derived classification.
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
	s.appendMergeDispatchSettledRow(wctx, claim, string(outcome), cause.Error(), mergeDispatchHeadMismatchDetail(cause))
	return outcome
}

// mergeDispatchHeadMismatchDetail is the head-movement detail a settle
// records as separate merge_dispatch_settled keys (#4749) when cause carries
// a *githubclient.HeadMismatchError: refusal_basis, expected_head_sha and
// observed_head_sha (empty when the forge did not report it). nil otherwise.
func mergeDispatchHeadMismatchDetail(cause error) map[string]any {
	var hm *githubclient.HeadMismatchError
	if !errors.As(cause, &hm) {
		return nil
	}
	return map[string]any{
		"refusal_basis":     hm.Basis,
		"expected_head_sha": hm.ExpectedHeadSHA,
		"observed_head_sha": hm.ObservedHeadSHA,
	}
}

// appendMergeDispatchSettled appends the categoryMergeDispatchSettled row,
// best-effort (the ledger write is the authority).
func (s *Server) appendMergeDispatchSettled(ctx context.Context, claim mergequeue.DispatchClaim, outcome, reason string) {
	s.appendMergeDispatchSettledRow(ctx, claim, outcome, reason, nil)
}

// appendMergeDispatchSettledRow is appendMergeDispatchSettled with optional
// extra payload keys; the base keys (outcome, path, head_sha, reason) win.
func (s *Server) appendMergeDispatchSettledRow(ctx context.Context, claim mergequeue.DispatchClaim, outcome, reason string, extra map[string]any) {
	if s.cfg.AuditRepo == nil {
		return
	}
	if err := s.appendMergeDispatchSettledPayload(ctx, claim, outcome, reason, extra); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "merge dispatch: the merge_dispatch_settled row did not land",
			slog.String("run_id", claim.RunID.String()), slog.String("error", err.Error()))
	}
}

// appendMergeDispatchSettledStrict appends the categoryMergeDispatchSettled
// row and returns the append error: the ADR-093 D8 resume writes it BEFORE
// its ledger transition and holds on a failure (fail closed, ruling R8d).
func (s *Server) appendMergeDispatchSettledStrict(ctx context.Context, claim mergequeue.DispatchClaim, outcome, reason string) error {
	return s.appendMergeDispatchSettledPayload(ctx, claim, outcome, reason, nil)
}

// appendMergeDispatchSettledPayload is the one merge_dispatch_settled row
// builder: the base keys plus any extra keys, returning the append error.
func (s *Server) appendMergeDispatchSettledPayload(ctx context.Context, claim mergequeue.DispatchClaim, outcome, reason string, extra map[string]any) error {
	if s.cfg.AuditRepo == nil {
		return errors.New("merge_dispatch_settled: no audit repository is wired")
	}
	fields := make(map[string]any, len(extra)+4)
	for k, v := range extra {
		fields[k] = v
	}
	fields["outcome"], fields["path"], fields["head_sha"], fields["reason"] = outcome, claim.Path, claim.HeadSHA, reason
	payload, _ := json.Marshal(fields)
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	_, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        claim.RunID,
		Timestamp:    time.Now().UTC(),
		Category:     categoryMergeDispatchSettled,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	})
	return err
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
	// mergeOnPassAwaitApproval (ADR-112 R3 row 3): the fold is not approved
	// and the reason waits at the head. The pump starts or continues the
	// persisted approval wait (Store.ApprovalWait) outside the lock: before
	// expiry the entry holds the line; at expiry it yields Yield.
	mergeOnPassAwaitApproval mergeOnPassKind = "await_approval"
	// mergeOnPassYield (ADR-112 R3 rows 2-4): yield Yield now (Store.Yield
	// outside the lock); the pump continues to the next held entry.
	mergeOnPassYield mergeOnPassKind = "yield"
	// mergeOnPassRedecide (ADR-093 D8): the restart resume returned a merging
	// entry with NO dispatch claim, or with an orphaned queue claim whose PR
	// reads open (ADR-109 A1, released in step 8), to phase passed; the pump
	// continues, and ordinary merge-on-pass decides (fold and veto in-lock).
	mergeOnPassRedecide mergeOnPassKind = "redecide"
	// mergeOnPassExpiredYielded (ADR-112 R3, #4748): the expired-wait
	// enforcement (mergeOnPassEnforceExpiredWait) or the merge-gate wait
	// enforcement (mergeOnPassEnforceGateWait, docs/adr/112 A3.2) yielded the
	// entry under the run's merge-intent lock instead of applying a
	// readvance, a hold or a merge-gate wait; the pump continues to the next
	// held entry.
	mergeOnPassExpiredYielded mergeOnPassKind = "expired_yielded"
	// mergeOnPassAwaitGate (docs/adr/112 A3.2, #4780): step 4 read a
	// retryable merge-gate leg (A3.1) and nothing non-retryable. An AWAIT
	// kind, never mergeOnPassHeld: Yield is the step-4 yield
	// (mergeGateYieldFor). Immediate (A3.1(a) red) yields at once;
	// otherwise ((b) pending, (c) review) the entry holds the line on the
	// epoch's merge-gate window (Store.GateWait) and yields at its expiry
	// or with no hold token left. mergeOnPassEnforceGateWait applies it.
	mergeOnPassAwaitGate mergeOnPassKind = "await_gate"
)

// mergeOnPassOutcome is mergeQueueMergeOnPass's answer. State is set only on
// mergeOnPassSettle (and mergeOnPassMerged: StateMerged); Yield only on
// mergeOnPassAwaitApproval, mergeOnPassAwaitGate and mergeOnPassYield (Reason
// is then Yield.Reason).
// ForgeReason and ForgeDetail are set only on a forge_merge_refused ejection
// (ADR-093 D4): the typed refusal code and the forge's untrusted refusal text,
// which the merge_queue_ejected writer sanitises. InFlightMarked reports that
// THIS decision marked the run in s.mergeInFlight (ADR-093 D8), so the pump
// clears only its own mark. ExpiryYield (ADR-112 R3, #4748) is the yield the
// expired-wait enforcement applies INSTEAD of this outcome when the entry's
// ESTABLISHED approval wait has expired: it is set only on readvance and
// transient-hold outcomes (every head_moved and base_moved readvance, and
// every mergeOnPassReasonTransient hold), never by the ADR-093 D8 resume
// (merge_queue_resume.go keeps its own closures). A post-claim outcome may
// carry it too; InFlightMarked makes the enforcement skip it.
type mergeOnPassOutcome struct {
	Kind          mergeOnPassKind
	State         mergequeue.State
	Reason        string
	Detail        string
	ApprovalBasis string
	HeadSHA       string
	Settled       bool
	Yield         mergeQueueYield
	ForgeReason   string
	ForgeDetail   string
	// InFlightMarked: this decision marked s.mergeInFlight for the run.
	InFlightMarked bool
	// ExpiryYield: the yield an expired approval wait applies (nil: none).
	ExpiryYield *mergeQueueYield
	// GateRemaining (mergeOnPassAwaitGate only): the open merge-gate
	// window's remaining time, for the pump's wait log.
	GateRemaining time.Duration
	// Immediate (mergeOnPassAwaitGate only, docs/adr/112 A3.2): the
	// merge-gate yield is (a) red and yields at once, with no window.
	Immediate bool
}

// Merge-on-pass reasons. Hold reasons leave the entry; eject/drop reasons are
// the queue's (mergequeue README § "Reasons").
const (
	mergeOnPassReasonNoIntent            = "no_merge_intent"
	mergeOnPassReasonNotLive             = "not_live"
	mergeOnPassReasonNotPassed           = "not_passed"
	mergeOnPassReasonNoPinnedMerge       = "no_pinned_merge"
	mergeOnPassReasonTransient           = "transient"
	mergeOnPassReasonVerifyNotPassed     = "verify_not_passed"
	mergeOnPassReasonNotMergeable        = "not_mergeable_yet"
	mergeOnPassReasonEntryChanged        = "entry_changed"
	mergeOnPassReasonMergedClaimMismatch = "merged_claim_head_mismatch"
	// mergeOnPassReasonMergedClaimUnread (#4748 M1): the pull request merged
	// and the run's dispatch claim could not be read. Not transient: the
	// hold carries no ExpiryYield, so an expired wait never yields a merged PR.
	mergeOnPassReasonMergedClaimUnread     = "merged_claim_unread"
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
	// mergeOnPassReasonOrphanReleaseFailed (ADR-109 A1, #4711): step 8 judged
	// the run's queue claim orphaned but its merge_dispatch_settled row or
	// its release did not land. Not transient: no approval-wait ExpiryYield.
	mergeOnPassReasonOrphanReleaseFailed = "orphaned_claim_release_failed"
	// mergeOnPassReasonAuthorizationUnrecorded: the fold approved but the
	// merge_queue_authorized append failed (ADR-112 R4): no verdict row, no
	// claim; the next pump re-decides.
	mergeOnPassReasonAuthorizationUnrecorded = "authorization_unrecorded"

	mergeCandidateQueueReasonPRMerged          = "pr_merged"
	mergeCandidateQueueReasonMergedByQueue     = "merged_by_queue"
	mergeCandidateQueueReasonForgeMergeRefused = "forge_merge_refused"
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
		out := mergeOnPassOutcome{Kind: mergeOnPassHeld, Reason: reason, Detail: detail, HeadSHA: e.AnchoredHeadSHA}
		if reason == mergeOnPassReasonTransient { // ADR-112 R3 (#4748): an approval-uncertain hold carries its expiry yield
			out.ExpiryYield = mergeOnPassUndeterminedExpiryYield(reason, detail)
		}
		return out
	}
	settle := func(state mergequeue.State, reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassSettle, State: state, Reason: reason, Detail: detail, HeadSHA: e.AnchoredHeadSHA}
	}
	readvance := func(reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassReadvance, Reason: reason, Detail: detail, HeadSHA: e.AnchoredHeadSHA,
			ExpiryYield: mergeOnPassUndeterminedExpiryYield(reason, detail)} // ADR-112 R3 (#4748)
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
	// E93.2 / #4658: the pre-lock merge-method refusal, right after the run
	// read and BEFORE the installation guard, the PR read, eligibility, the
	// republish and the merge-intent lock (so before the fold, the verdict,
	// SetPhase(merging) and the claim). An EJECT, not a hold: the run's
	// admission declaration never changes, so a hold would block the head of
	// the line forever (the verify_failed eject below is the pre-lock
	// precedent). Detail and ForgeDetail are field-built, never forge text.
	if ref := staticMergeMethodRefusal(runRow); ref != nil {
		out := settle(mergequeue.StateEjected, mergeCandidateQueueReasonForgeMergeRefused, ref.FieldDetail())
		out.ForgeReason, out.ForgeDetail = mergeQueueForgeReasonMergeMethodRefused, ref.FieldDetail()
		return out
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

	// 4. Eligibility, merge variant, with the intent anchor (D6 transfer),
	// classified onto docs/adr/112 A3.1's legs (A3.2): an undeterminable
	// non-check leg holds (the merge variant answers every checks-read
	// failure as checks_not_green, so this is never a check read; A3
	// residual); any non-retryable determined reason drops, even beside a
	// retryable one; otherwise the retryable legs await the merge gate:
	// (a) red yields at once, (b) pending and (c) review hold on the epoch's
	// window (mergeOnPassEnforceGateWait). Never a drop for a retryable leg.
	anchor := mergeQueueIntentAnchor{Sequence: req.IntentSequence, BaseRef: e.BaseRef}
	elig := s.mergeQueueMergeEligibility(ctx, runRow, head, anchor)
	if !elig.Eligible {
		legs := mergeGateLegsOf(elig)
		if legs.Undeterminable {
			return held(mergeOnPassReasonNotMergeable, mergeQueueReasonsText(elig.Reasons))
		}
		if y, ok := mergeGateYieldFor(legs, elig); ok && !legs.Other {
			return mergeOnPassOutcome{Kind: mergeOnPassAwaitGate, Reason: y.Reason, Detail: y.Detail, HeadSHA: head, Yield: y, Immediate: legs.Red}
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

// mergeQueueGateWait is the configured merge-gate window (docs/adr/112
// A3.2): Config.MergeQueueGateWait when positive, else the approval wait.
func (s *Server) mergeQueueGateWait() time.Duration {
	if s.cfg.MergeQueueGateWait > 0 {
		return s.cfg.MergeQueueGateWait
	}
	return s.mergeQueueApprovalWait()
}

// mergeOnPassEnforceGateWait applies a step-4 await_gate outcome (docs/adr/112
// A3.2, #4780); any other kind is returned unchanged. Immediate ((a) red) goes
// straight to the protected yield. Otherwise ((b) pending, (c) review)
// Store.GateWait opens or continues the epoch's merge-gate window OUTSIDE the
// merge-intent lock: a write error holds (the pump stops, the next pump
// re-decides); an open, unexpired window returns o (the entry holds the line,
// the base frozen); Expired or NoToken (the hold budget is empty) yields.
//
// The protected yield takes the run's merge-intent lock, runs the pre-yield
// refusal (mergeOnPassPreYieldRefusal: entry re-read, in-flight registry, D1
// claim; a refusal holds with no yield), re-judges GateWait for a window
// (idempotent within the epoch: it continues, never spends) and yields o.Yield
// through yieldMergeQueueEntry, answering mergeOnPassExpiredYielded. It never
// calls ApprovalWait: a merge-gate hold never touches approval_wait_started_at.
func (s *Server) mergeOnPassEnforceGateWait(ctx context.Context, req mergeOnPassRequest, o mergeOnPassOutcome) mergeOnPassOutcome {
	if o.Kind != mergeOnPassAwaitGate {
		return o
	}
	e := req.Entry
	if !o.Immediate {
		res, err := s.cfg.MergeQueue.GateWait(ctx, e.ID, e.AdmissionEpoch, s.mergeQueueGateWait())
		if err != nil {
			return mergeOnPassExpiredWaitHeld(e, mergeOnPassReasonEntryChanged, "the merge-gate wait could not be recorded: "+err.Error())
		}
		if !res.Expired && !res.NoToken {
			o.GateRemaining = res.Remaining
			return o
		}
	}
	out, ran := o, false
	lockErr := s.withRunMergeLock(ctx, e.RunID, func(lctx context.Context) error {
		ran = true
		if h := s.mergeOnPassPreYieldRefusal(lctx, req); h != nil {
			out = *h
			return nil
		}
		cur := e
		if !o.Immediate {
			res, err := s.cfg.MergeQueue.GateWait(lctx, e.ID, e.AdmissionEpoch, s.mergeQueueGateWait())
			if err != nil {
				out = mergeOnPassExpiredWaitHeld(e, mergeOnPassReasonEntryChanged, "the merge-gate wait could not be judged: "+err.Error())
				return nil
			}
			if !res.Expired && !res.NoToken {
				out.GateRemaining = res.Remaining
				return nil
			}
			cur = res.Entry
		}
		if !s.yieldMergeQueueEntry(lctx, cur, o.Yield, mergeQueueYieldSiteMergeOnPass) {
			out = mergeOnPassExpiredWaitHeld(cur, mergeOnPassReasonEntryChanged, "the merge-gate yield did not land")
			return nil
		}
		out = mergeOnPassOutcome{Kind: mergeOnPassExpiredYielded, Reason: o.Yield.Reason, Detail: o.Yield.Detail, HeadSHA: cur.AnchoredHeadSHA, Yield: o.Yield}
		return nil
	})
	if !ran {
		detail := "the run's merge-intent lock could not be taken to yield the merge-gate wait"
		if lockErr != nil {
			detail += ": " + lockErr.Error()
		}
		return mergeOnPassExpiredWaitHeld(e, mergeOnPassReasonLockUnavailable, detail)
	}
	if lockErr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge queue: the merge-gate wait enforcement's lock lapsed after its judgement ran; its writes stand",
			slog.String("run_id", e.RunID.String()), slog.String("outcome", string(out.Kind)),
			slog.String("error", lockErr.Error()))
	}
	return out
}

// mergeOnPassLocked is the locked half of mergeQueueMergeOnPass. ctx carries
// the merge-intent lock fn's deadline; every forge read takes its own
// sub-deadline.
func (s *Server) mergeOnPassLocked(ctx context.Context, req mergeOnPassRequest, runRow *run.Run,
	target mergeOnPassTarget, head string, merger HeadPinnedMerger) (res mergeOnPassOutcome) {
	e := req.Entry
	// marked: this decision marked s.mergeInFlight; every outcome past the
	// mark carries it so the pump clears only its own mark (#4298 48a65a85).
	marked := false
	defer func() { res.InFlightMarked = marked }()
	// basis is the fold's (step 6b); "" until the fold ran.
	basis := ""
	held := func(reason, detail string) mergeOnPassOutcome {
		out := mergeOnPassOutcome{Kind: mergeOnPassHeld, Reason: reason, Detail: detail, HeadSHA: head, ApprovalBasis: basis}
		if reason == mergeOnPassReasonTransient { // ADR-112 R3 (#4748): an approval-uncertain hold carries its expiry yield
			out.ExpiryYield = mergeOnPassUndeterminedExpiryYield(reason, detail)
		}
		return out
	}
	readvance := func(reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassReadvance, Reason: reason, Detail: detail, HeadSHA: head, ApprovalBasis: basis,
			ExpiryYield: mergeOnPassUndeterminedExpiryYield(reason, detail)} // ADR-112 R3 (#4748)
	}
	settle := func(state mergequeue.State, reason, detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassSettle, State: state, Reason: reason, Detail: detail, HeadSHA: head, ApprovalBasis: basis}
	}
	yield := func(kind mergeOnPassKind, y mergeQueueYield) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: kind, Reason: y.Reason, Detail: y.Detail, HeadSHA: head, ApprovalBasis: basis, Yield: y}
	}

	// 1. The entry, re-read under the lock.
	cur, found, err := s.cfg.MergeQueue.ActiveForRun(ctx, e.RunID)
	if err != nil {
		return held(mergeOnPassReasonTransient, "the queue entry could not be re-read: "+err.Error())
	}
	// ADR-093 D8 ruling R3: passed, or merging (an earlier decision of THIS
	// cycle stamped it and its dispatch is on the ledger: the claim below
	// decides). The pre-lock check stays passed-only; the pump routes a
	// merging entry to the resume.
	inLockPhaseOK := cur.Entry.Phase == mergequeue.PhasePassed || cur.Entry.Phase == mergequeue.PhaseMerging
	if !found || cur.Entry.ID != e.ID || cur.Entry.State != mergequeue.StateLive || !inLockPhaseOK ||
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
		// ADR-112 R3 (#4748): an entry with NO established approval wait
		// re-advances with the generic ExpiryYield and no fold read. An
		// anchored entry first runs the fold at head, writing nothing, so an
		// expiry yield names the real reason. R4: this path yields ONLY on
		// expiry, a qualified veto included; a veto's immediate yield stays
		// at step 6b on the entry's next pass.
		out := readvance(mergeOnPassReasonBaseMoved, e.BaseRef+" moved past the verified head "+head)
		if cur.Entry.ApprovalWaitStartedAt == nil {
			return out
		}
		// ADR-112 R3: an established wait judges the fold before re-advancing.
		fold, ferr := s.forgeApprovalFold(ctx, runRow, head)
		switch {
		case ferr != nil:
			y := mergeOnPassSnapshotUndeterminedYield(ferr)
			out.ExpiryYield = &y
		case fold.Merged:
			if ok, reason, detail := s.mergeOnPassReconcileMerged(ctx, req.Ledger, e.RunID, fold.ObservedHead); !ok {
				return held(reason, detail)
			}
			return settle(mergequeue.StateMerged, mergeCandidateQueueReasonPRMerged, "the pull request is already merged")
		case fold.Reason == forgeApprovalReasonHeadMoved:
			return readvance(mergeOnPassReasonHeadMoved, fold.Detail)
		case fold.approved():
			out.ExpiryYield = nil // the fold approved: no wait to enforce
		default:
			y, _ := mergeOnPassFoldYield(fold) // #4748 R4: the base-move path yields only on expiry; a veto's immediate yield stays at step 6b
			out.ExpiryYield = &y
		}
		return out
	}

	// 6b. ADR-112 (#4619): the ONE forge-approval fold at the verified head,
	// AFTER the base re-probe and BEFORE the verdict, the single source of
	// approvals and vetoes on this path. A snapshot transport failure is an
	// UNDETERMINED approval result (#4748): it WAITS at the head through the
	// ordinary await_approval path (the pump stamps or continues the anchor
	// with Store.ApprovalWait), then yields forge_approval_undetermined at
	// expiry; a head the snapshot reports moved re-advances (H1); every other
	// non-approved result WAITS at the head or YIELDS (ADR-112 R3,
	// mergeOnPassFoldYield; the pump applies either outside this lock). None
	// of these writes a verdict row, an authorization row or a claim. An
	// approved fold on a declared gate writes merge_queue_authorized BEFORE
	// the verdict; a failed append holds.
	fold, ferr := s.forgeApprovalFold(ctx, runRow, head)
	if ferr != nil {
		return yield(mergeOnPassAwaitApproval, mergeOnPassSnapshotUndeterminedYield(ferr))
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
		y, immediate := mergeOnPassFoldYield(fold)
		if immediate {
			return yield(mergeOnPassYield, y)
		}
		return yield(mergeOnPassAwaitApproval, y)
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

	// 8. Approval condition D1: claim the run's ONE merge dispatch (an
	// orphaned queue claim is released first: ADR-109 A1, below).
	if req.Ledger == nil {
		return held(mergeOnPassReasonLedgerUnavailable, "no merge-dispatch ledger is wired, so no dispatch can be claimed")
	}
	// ADR-093 D8: record the step BEFORE the claim, so a restart finds a
	// merging entry wherever a queue dispatch may be in flight. A CAS miss
	// holds with no claim and no forge call; an unchanged phase writes nothing.
	if cur.Entry.Phase != mergequeue.PhaseMerging {
		if _, err := s.cfg.MergeQueue.SetPhase(ctx, e.ID, e.AdmissionEpoch, mergequeue.PhaseMerging); err != nil {
			return held(mergeOnPassReasonEntryChanged, "the merging step could not be recorded before the dispatch claim: "+err.Error())
		}
	}
	s.mergeInFlight.mark(e.RunID)
	marked = true
	merged := func(observed bool) mergeOnPassOutcome {
		// The forge accepted the merge: record settling, best-effort (the
		// merge is a fact), then the merged tail.
		if _, err := s.cfg.MergeQueue.SetPhase(ctx, e.ID, e.AdmissionEpoch, mergequeue.PhaseSettling); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "merge queue: the settling step could not be recorded; the merged tail still runs",
				slog.String("run_id", e.RunID.String()), slog.String("error", err.Error()))
		}
		return s.mergeOnPassMerged(ctx, e, head, basis, mergeOnPassMergedRefs{verdictSeq: verdictSeq, verdictHead: verdictHead,
			intentSeq: intentSeq, authSeq: authSeq, counted: fold.Counted}, mergeOnPassTailOpts{Observed: observed})
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
		// ADR-109 Amendment A1 (#4711): steps 1-7 of THIS decision re-read the
		// entry and an open, unmerged PR, and re-ran the head, base, fold, R4
		// authorization and verdict under the lock, so a crash-left queue
		// claim still 'dispatched' is released here and claimed afresh; the
		// merge below is the ordinary one (no bespoke retry).
		released, reason, detail := s.mergeOnPassReleaseOrphanedClaim(ctx, req.Ledger, claim)
		if reason != "" {
			return held(reason, detail)
		}
		if released {
			claim, claimed, err = req.Ledger.ClaimMergeDispatch(ctx, e.RunID, mergeDispatchPathQueue, head)
			if err != nil {
				return held(mergeOnPassReasonTransient, "the merge dispatch could not be claimed after the orphaned claim's release: "+err.Error())
			}
		}
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
	case errors.Is(mergeErr, forge.ErrPullRequestNotMergeable), errors.Is(mergeErr, forge.ErrMergeUnprocessable),
		errors.Is(mergeErr, forge.ErrPullRequestUnstableStatus):
		// ADR-112 R3 row 4 is decided FIRST for every typed refusal, the
		// unstable-status one included (first-officer ruling C-R4).
		if decision, status, ok := s.mergeOnPassRefusalNeedsReview(ctx, runRow); ok {
			// ADR-112 R3 row 4: the claim was released (a typed RELEASE
			// outcome above), so the entry yields approve_pr, never ejects.
			return yield(mergeOnPassYield, newMergeQueueYield(mergeQueueYieldForgeReviewRequired,
				fmt.Sprintf(mergeQueueYieldForgeRefusalDetail, nonEmpty(decision, "null"), nonEmpty(status, "null")), mergeQueueYieldRowForgeRefusal))
		}
		// ADR-112 R3 row 5 ("Any other forge refusal | Eject (D4)") and
		// ADR-093 D4 (a forge merge refusal ejects naming the forge's reason
		// and never retries in a loop): the unstable-status refusal ejects
		// checks_not_settled like the 405/422 arms instead of holding as
		// transient, which let the next pump claim and dispatch again. Before
		// dispatch, step 4 never drops or ejects on a retryable merge-gate leg
		// (docs/adr/112 A3.2): pending checks hold on the merge-gate window and
		// then yield merge_checks_pending, red checks yield merge_checks_red,
		// so slow CI never ejects; only the forge's own refusal of the merge
		// PUT does.
		// E93.2 / #4658: a refusal ForgeMerger attributed to the run's merge
		// method renders from its closed-set fields only (FieldDetail), never
		// mergeErr.Error() or its Cause.
		refusalText := mergeErr.Error()
		var mref *MergeMethodRefusalError
		if errors.As(mergeErr, &mref) {
			refusalText = mref.FieldDetail()
		}
		out := settle(mergequeue.StateEjected, mergeCandidateQueueReasonForgeMergeRefused,
			"the forge refused the pinned merge ("+refusalText+"); the dispatch claim was released; resolve it, then re-invoke fishhawk_merge_run")
		out.ForgeReason, out.ForgeDetail = mergeQueueForgeReasonFor(mergeErr), refusalText
		if out.ForgeReason == mergeQueueForgeReasonChecksNotSettled {
			out.ForgeDetail = mergeQueueChecksNotSettledDetail + mergeQueueYieldDetail(mergeErr.Error())
		}
		return out
	}
	return held(mergeOnPassReasonTransient, "the pinned merge failed: "+mergeErr.Error())
}

// mergeOnPassReleaseOrphanedClaim is ADR-109 Amendment A1's release arm
// (#4711), called from merge-on-pass step 8 when the run's claim already
// exists. It acts ONLY on a QUEUE-path claim still 'dispatched' and
// crash-left on the database clock (mergeQueueClaimCrashLeft: older than
// mergeQueueCrashLeftClaimAge; a zero claimed_at fails closed); an inline,
// delegated or dispatch_failed claim, or a young one, returns
// (false, "", ""): the caller's merge_already_dispatched hold. For an orphan
// the strict merge_dispatch_settled {released, released_orphaned_claim} row
// is appended FIRST (record before action), then ReleaseMergeDispatch, the
// exact-claim CAS DELETE on state 'dispatched'. Either failure returns
// orphaned_claim_release_failed with the claim standing (an append failure
// changes nothing; a release failure leaves the row before a claim the next
// decision re-judges). released=true: the claim is gone and the caller
// claims afresh.
func (s *Server) mergeOnPassReleaseOrphanedClaim(ctx context.Context, ledger mergeDispatchLedger, claim mergequeue.DispatchClaim) (released bool, holdReason, detail string) {
	if claim.Path != mergeDispatchPathQueue || claim.State != mergequeue.DispatchDispatched {
		return false, "", ""
	}
	if aged, _ := s.mergeQueueClaimCrashLeft(ctx, claim); !aged {
		return false, "", ""
	}
	if err := s.appendMergeDispatchSettledStrict(ctx, claim, string(mergeDispatchRelease), mergeDispatchReasonReleasedOrphanedClaim); err != nil {
		return false, mergeOnPassReasonOrphanReleaseFailed,
			"the orphaned queue claim's merge_dispatch_settled row could not be recorded, so the claim was not released: " + err.Error()
	}
	if err := ledger.ReleaseMergeDispatch(ctx, claim); err != nil {
		return false, mergeOnPassReasonOrphanReleaseFailed,
			"the orphaned queue claim could not be released and still stands: " + err.Error()
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
		"merge queue: released an orphaned queue dispatch claim whose pull request reads open and unmerged (ADR-109 A1); claiming afresh",
		slog.String("run_id", claim.RunID.String()), slog.String("path", claim.Path),
		slog.String("head_sha", claim.HeadSHA), slog.Time("claimed_at", claim.ClaimedAt))
	return true, "", ""
}

// mergeOnPassRefusalNeedsReview is ADR-112 R3 rows 4/5 after a TYPED forge
// refusal whose claim was released: ONE fresh review snapshot
// (forgeReviewRequiredSnapshot). ok=true ONLY when it shows the row-4
// condition (yield forge_review_required, next_step approve_pr). A non-GitHub
// forge, an unaddressable run or a snapshot read error is ok=false (row 5,
// forge_merge_refused). It applies with or without a declared gate.
func (s *Server) mergeOnPassRefusalNeedsReview(ctx context.Context, runRow *run.Run) (reviewDecision, mergeStateStatus string, ok bool) {
	decision, status, present, err := s.forgeReviewRequiredSnapshot(ctx, runRow)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "merge queue: post-refusal review snapshot unreadable; ejecting forge_merge_refused",
			slog.String("run_id", runRow.ID.String()), slog.String("detail", sanitizeForgeReadDetail("review_snapshot", err.Error())))
		return "", "", false
	}
	return decision, status, present
}

// errForgeReviewSnapshotUnavailable is a run whose forge or pull request has
// no review snapshot to read (a non-GitHub forge, an unaddressable run).
var errForgeReviewSnapshotUnavailable = errors.New("no review snapshot is readable for this run (non-GitHub forge or unaddressable pull request)")

// forgeReviewRequiredSnapshot reads ONE fresh review snapshot (no permission
// reads), with or without a declared gate, and reports whether it shows the
// ADR-112 R3 row-4 condition: reviewDecision REVIEW_REQUIRED or
// CHANGES_REQUESTED, or mergeStateStatus BLOCKED. A null reviewDecision (a
// base without required-review protection) is NOT the condition. The error is
// a snapshot that could not be read (errForgeReviewSnapshotUnavailable, or a
// transport failure): the condition is then undetermined. The row-4 yield
// (mergeOnPassRefusalNeedsReview) and its clear (mergeQueueYieldClears) both
// judge it here.
func (s *Server) forgeReviewRequiredSnapshot(ctx context.Context, runRow *run.Run) (reviewDecision, mergeStateStatus string, present bool, err error) {
	reader, unsupported, _ := s.forgeApprovalReaderFor(runRow)
	if unsupported || reader == nil {
		return "", "", false, errForgeReviewSnapshotUnavailable
	}
	rctx, cancel := context.WithTimeout(ctx, mergeOnPassForgeCallTimeout)
	defer cancel()
	snap, err := reader.ReviewSnapshot(rctx)
	if err != nil {
		return "", "", false, err
	}
	switch {
	case snap.ReviewDecision == "REVIEW_REQUIRED", snap.ReviewDecision == "CHANGES_REQUESTED", snap.MergeStateStatus == "BLOCKED":
		return snap.ReviewDecision, snap.MergeStateStatus, true, nil
	}
	return snap.ReviewDecision, snap.MergeStateStatus, false, nil
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

// survivingMergeVerdict is the run's surviving merge_verdict_recorded row
// (the EARLIEST: the 0062 once-index admits one per run): its sequence and
// head ("" when it names none); ok=false when rows holds none.
func survivingMergeVerdict(rows []*audit.Entry) (int64, string, bool) {
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
	surviving := survivingMergeVerdict
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

// mergeOnPassTailOpts selects the merged tail's mode, threaded EXPLICITLY
// (ADR-093 D8 ruling R4). Observed: the merge was observed on the forge, not
// returned by this decision's call. Resume: the restart resume's tail, under
// the merge-intent lock: merge_queue_merged is appended only when no existing
// row names the entry, a failed dedup read or append HOLDS
// (mergeQueueResumeReasonUnrecorded, nothing settled), and the payload
// carries resumed:true. Resume=false is the first attempt, unchanged.
type mergeOnPassTailOpts struct {
	Observed bool
	Resume   bool
}

// mergeOnPassMerged is the merged tail: the merge_queue_merged row, then the
// merged settle INSIDE the lock (approval condition D3). On the first attempt
// a failed row append after the forge accepted the merge is logged at ERROR
// and the outcome is still merged (approval condition (c)); a failed settle
// leaves Settled false so the pump settles it. opts.Resume changes the row
// handling only (mergeOnPassTailOpts).
func (s *Server) mergeOnPassMerged(ctx context.Context, e mergequeue.Entry, head, basis string,
	refs mergeOnPassMergedRefs, opts mergeOnPassTailOpts) mergeOnPassOutcome {
	out := mergeOnPassOutcome{Kind: mergeOnPassMerged, State: mergequeue.StateMerged,
		Reason: mergeCandidateQueueReasonMergedByQueue, HeadSHA: head, ApprovalBasis: basis}
	resumeHeld := func(detail string) mergeOnPassOutcome {
		return mergeOnPassOutcome{Kind: mergeOnPassHeld, Reason: mergeQueueResumeReasonUnrecorded, Detail: detail, HeadSHA: head, ApprovalBasis: basis}
	}
	observed := opts.Observed
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
	appendRow := true
	if opts.Resume {
		fields["resumed"] = true
		named, err := s.mergeQueueMergedRowNames(ctx, e)
		if err != nil {
			return resumeHeld("the merge_queue_merged rows could not be read for the resume's dedup: " + err.Error())
		}
		appendRow = !named
	}
	payload, _ := json.Marshal(fields)
	kind, subject := audit.ActorSystem, mergeCandidateSystemSubject
	// !appendRow: a row already names this entry (a crash after the append,
	// before the settle), so the resume settles without a second row.
	if !appendRow {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "merge queue resume: merge_queue_merged already names the entry; settling without a second row",
			slog.String("run_id", e.RunID.String()), slog.String("entry_id", e.ID.String()))
	} else if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        e.RunID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeQueueMerged,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil && opts.Resume {
		return resumeHeld("merge_queue_merged could not be recorded on the resume: " + err.Error())
	} else if err != nil {
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
