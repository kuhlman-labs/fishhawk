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
	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// The WITHDRAW-INTENT verb for a YIELDED merge-queue entry (ADR-112 R3,
// E87.17 / #4685): POST /v0/runs/{run_id}/merge-queue/withdraw settles the
// run's yielded held entry dropped intent_withdrawn through the store
// (mergequeue README § "Withdraw intent") and appends
// merge_queue_intent_withdrawn INSIDE that transaction. A settled entry is
// terminal, so every intent lookup then treats the intent as absent:
// mergeQueueOwnsMerge and the delegated arm read ActiveForRun, the held arm of
// the merge-candidate gate reads only a held entry, and the re-entry sweep
// requires an ejected newest row. A later inline merge_run is therefore no
// longer refused by the queue, and still runs every one of its own gates.
// docs/merge-queue.md § "Withdraw intent (ADR-112 R3, #4685)" is the contract.

// CategoryMergeQueueIntentWithdrawn is the withdraw-intent row, appended
// inside the withdrawal transaction (INTERNAL, not an issue-comment surface;
// run-bound-readable: its payload carries only this run's ids and no
// yield_detail).
const CategoryMergeQueueIntentWithdrawn = "merge_queue_intent_withdrawn"

// Withdraw-intent response codes. merge_queue_unconfigured,
// merge_queue_not_queued and merge_queue_entry_live are the promote verb's
// codes (mqPromoteCode*), reused with the same meaning.
const (
	mqWithdrawCodeNoIntent   = "merge_queue_no_intent"
	mqWithdrawCodeNotYielded = "merge_queue_entry_not_yielded"
	mqWithdrawCodeClaimed    = "merge_dispatch_claimed"
	mqWithdrawCodeFailed     = "merge_queue_withdraw_failed"
)

// mergeQueueWithdrawResponse is the 200 body. The MCP mirror
// (backend/internal/mcpserver/merge_queue_withdraw.go) decodes it; the json
// tags MUST byte-match.
type mergeQueueWithdrawResponse struct {
	RunID          string `json:"run_id"`
	EntryID        string `json:"entry_id"`
	Repo           string `json:"repo"`
	BaseRef        string `json:"base_ref"`
	Withdrawn      bool   `json:"withdrawn"`
	SettledState   string `json:"settled_state"`
	SettleReason   string `json:"settle_reason"`
	IntentSequence int64  `json:"intent_sequence,omitempty"`
	YieldReason    string `json:"yield_reason"`
	LiveRunID      string `json:"live_run_id,omitempty"`
	AuditSequence  int64  `json:"audit_sequence"`
}

// mergeQueueIntentWithdrawnPayload is the merge_queue_intent_withdrawn
// payload. It carries only THIS run's own ids. yield_detail is deliberately
// omitted: it is untrusted forge-derived text (mergequeue.Entry.YieldDetail),
// and the row stays run-bound-readable (ADR-106) without it.
type mergeQueueIntentWithdrawnPayload struct {
	EntryID         string     `json:"entry_id"`
	RunID           string     `json:"run_id"`
	Repo            string     `json:"repo"`
	BaseRef         string     `json:"base_ref"`
	AnchoredHeadSHA string     `json:"anchored_head_sha"`
	MergeIntentAt   *time.Time `json:"merge_intent_at"`
	IntentSequence  int64      `json:"intent_sequence"`
	YieldReason     string     `json:"yield_reason"`
	YieldedAt       *time.Time `json:"yielded_at"`
	Reason          string     `json:"reason"`
	Actor           string     `json:"actor"`
}

// mergeQueueIntentWithdrawer returns the configured store's WithdrawIntent
// capability, or nil when no merge queue is wired or the store does not
// implement it (a test fake embedding mergequeue.Store): the verb then
// answers 503 instead of panicking.
func (s *Server) mergeQueueIntentWithdrawer() mergequeue.IntentWithdrawer {
	if s.cfg.MergeQueue == nil {
		return nil
	}
	wd, ok := s.cfg.MergeQueue.(mergequeue.IntentWithdrawer)
	if !ok {
		return nil
	}
	return wd
}

// handleMergeQueueWithdraw implements POST
// /v0/runs/{run_id}/merge-queue/withdraw. The rungs, every one decided before
// any write (the promote verb's ladder):
//
//	401 authentication_required  — anonymous.
//	403 run_token_forbidden      — a run-bound agent token, even its own run.
//	403 insufficient_scope       — a token without write:runs.
//	400 validation_failed        — a non-UUID run_id, or a body that is not
//	                               exactly {"reason": string} with a
//	                               1..1000-byte trimmed reason.
//	503 merge_queue_unconfigured — no merge queue, a store without the
//	                               IntentWithdrawer capability, or no run
//	                               repository (requireRunAccount cannot check
//	                               the run's account without it); after the
//	                               auth rungs, so an anonymous caller reads 401.
//	503 merge_lock_unavailable   — the run's merge-intent lock was never taken
//	                               (bounded by mergeRunLockAcquireTimeout);
//	                               nothing written.
//
// Then the store's single transaction (mergequeue README § "Withdraw
// intent"), run under the run's merge-intent lock: 404 run_not_found, 409
// merge_queue_not_queued / merge_queue_entry_live (never withdrawn mid-pass)
// / merge_queue_no_intent / merge_queue_entry_not_yielded (an unmarked entry
// is progressing to the queue's own merge) / merge_dispatch_claimed (any
// ADR-109 dispatch claim, dispatched or dispatch_failed; the ledger is never
// written, so a dispatch_failed claim still needs reconciliation or the
// operator re-arm), or 500 merge_queue_withdraw_failed (rolled back, nothing
// changed). 200 withdrawn:true carries the merge_queue_intent_withdrawn row's
// audit_sequence. No pump is triggered: a yielded entry never holds the line
// and AdmitNext already skips it.
//
// Lock (approval condition 9): the store call runs inside
// s.cfg.MergeQueue.WithMergeIntentLock, so the claim it reads cannot be
// inserted concurrently. ClaimMergeDispatch's three production call sites
// each run under that lock through withRunMergeLock (merge_queue_merge.go):
// claimInlineMergeDispatch (merge_run.go, inside the inline fn), the delegated
// arm's dispatchAcceptanceGatedMergeSettled (autodrive.go) and merge-on-pass's
// mergeOnPassLocked (merge_queue_merge.go). No single site proves the
// invariant, so TestMergeQueueWithdraw_TakesMergeIntentLockBeforeClaimRead
// pins that a withdraw waits for a holder that claims a dispatch, then
// refuses on it.
//
// Auth: memberWrite via requireRunAccount, the promote's tier. A delegated
// operator-agent token IS admitted INTENTIONALLY, mirroring the promote: a
// withdrawal only takes the run's merge out of the queue and merges nothing,
// and the inline merge_run it unblocks still applies every gate (the
// forge-approval fold, the merge-candidate gate, verify, the ledger claim).
// The row records the actor and actor_kind.
func (s *Server) handleMergeQueueWithdraw(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not withdraw a merge-queue intent; taking a merge out of the queue is an operator action", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:runs") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:runs",
			map[string]any{"required_scope": "write:runs"})
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	reason, ok := s.parseReasonBody(w, r, "reason is required: say why this run's merge should leave the merge queue")
	if !ok {
		return
	}
	withdrawer := s.mergeQueueIntentWithdrawer()
	if withdrawer == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, mqPromoteCodeUnconfigured,
			"the merge-queue withdraw-intent verb requires a configured merge queue and the run repository", nil)
		return
	}

	subject := id.Subject
	actorKind := actorKindForSubject(subject)
	var auditSeq int64
	record := func(ctx context.Context, tx pgx.Tx, wd mergequeue.Withdrawal) error {
		e := wd.Entry
		raw, err := json.Marshal(mergeQueueIntentWithdrawnPayload{
			EntryID:         e.ID.String(),
			RunID:           e.RunID.String(),
			Repo:            e.Repo,
			BaseRef:         e.BaseRef,
			AnchoredHeadSHA: e.AnchoredHeadSHA,
			MergeIntentAt:   e.MergeIntentAt,
			IntentSequence:  e.MergeIntentSequence,
			YieldReason:     e.YieldReason,
			YieldedAt:       e.YieldedAt,
			Reason:          reason,
			Actor:           subject,
		})
		if err != nil {
			return fmt.Errorf("marshal %s payload: %w", CategoryMergeQueueIntentWithdrawn, err)
		}
		entry, err := s.promoteAuditAppend()(ctx, tx, audit.ChainAppendParams{
			RunID:        e.RunID,
			Timestamp:    time.Now().UTC(),
			Category:     CategoryMergeQueueIntentWithdrawn,
			ActorKind:    &actorKind,
			ActorSubject: &subject,
			Payload:      raw,
		})
		if err != nil {
			return fmt.Errorf("append %s: %w", CategoryMergeQueueIntentWithdrawn, err)
		}
		auditSeq = entry.Sequence
		return nil
	}

	// merge_run's bounded acquire: a lock that was never taken answers 503
	// merge_lock_unavailable with nothing written; a lapse after fn ran is a
	// WARN only, because the store transaction already committed or refused.
	acquireCtx, started, stopAcquire := boundMergeLockAcquire(r.Context(), mergeRunLockAcquireTimeout)
	defer stopAcquire()
	var (
		ran  bool
		wd   mergequeue.Withdrawal
		werr error
	)
	lockErr := s.cfg.MergeQueue.WithMergeIntentLock(acquireCtx, runID, func(fctx context.Context) error {
		if !started() {
			return errMergeLockAcquireTimedOut
		}
		ran = true
		wd, werr = withdrawer.WithdrawIntent(fctx, mergequeue.WithdrawIntentRequest{RunID: runID, Record: record})
		return nil
	})
	if lockErr != nil {
		if !ran {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge-queue withdraw: merge-intent lock unavailable; nothing written (fail closed)",
				slog.String("run_id", runID.String()), slog.String("error", lockErr.Error()))
			s.writeError(w, r, http.StatusServiceUnavailable, "merge_lock_unavailable",
				"the run's merge-intent lock could not be taken (a merge of this run holds it, or the wait was bounded out); nothing was written. Re-invoke the withdrawal to retry.",
				map[string]any{"run_id": runID.String(), "retryable": true})
			return
		}
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge-queue withdraw: merge-intent lock lapsed after the withdrawal ran; its answer stands",
			slog.String("run_id", runID.String()), slog.String("error", lockErr.Error()))
	}
	if werr != nil {
		s.writeMergeQueueWithdrawError(w, r, runID, werr)
		return
	}
	resp := mergeQueueWithdrawResponse{
		RunID:          runID.String(),
		EntryID:        wd.Entry.ID.String(),
		Repo:           wd.Entry.Repo,
		BaseRef:        wd.Entry.BaseRef,
		Withdrawn:      true,
		SettledState:   string(wd.Entry.State),
		SettleReason:   wd.Entry.EjectReason,
		IntentSequence: wd.Entry.MergeIntentSequence,
		YieldReason:    wd.Entry.YieldReason,
		AuditSequence:  auditSeq,
	}
	if wd.LiveRunID != nil {
		resp.LiveRunID = wd.LiveRunID.String()
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// writeMergeQueueWithdrawError maps a WithdrawIntent error to its response.
// Every refusal and failure leaves the queue and the ledger unchanged (the
// store's transaction rolled back).
func (s *Server) writeMergeQueueWithdrawError(w http.ResponseWriter, r *http.Request, runID uuid.UUID, err error) {
	details := map[string]any{"run_id": runID.String()}
	switch {
	case errors.Is(err, mergequeue.ErrNotFound):
		s.writeError(w, r, http.StatusNotFound, "run_not_found", "no run with that id", details)
	case errors.Is(err, mergequeue.ErrEntryLive):
		s.writeError(w, r, http.StatusConflict, mqPromoteCodeEntryLive,
			"the run's merge-queue entry is live (it holds its base's line and its pass is in flight); a live entry's merge intent is never withdrawn mid-pass. Watch it with fishhawk_get_merge_queue.", details)
	case errors.Is(err, mergequeue.ErrNotQueued):
		s.writeError(w, r, http.StatusConflict, mqPromoteCodeNotQueued,
			"the run has no active merge-queue entry (it was never queued, its entry settled, or an earlier withdrawal already settled it); there is no merge intent to withdraw", details)
	case errors.Is(err, mergequeue.ErrNoIntent):
		s.writeError(w, r, http.StatusConflict, mqWithdrawCodeNoIntent,
			"the run's merge-queue entry carries no merge intent, so there is nothing to withdraw and the queue does not own its merge", details)
	case errors.Is(err, mergequeue.ErrEntryNotYielded):
		s.writeError(w, r, http.StatusConflict, mqWithdrawCodeNotYielded,
			"the run's merge-queue entry has not yielded: it is progressing to the queue's own head-pinned merge (ADR-109: the queue owns the merge), and only a YIELDED entry's merge intent can be withdrawn. To stop it, cancel the run.", details)
	case errors.Is(err, mergequeue.ErrDispatchClaimed):
		next := "fishhawk_get_merge_queue"
		if c, found, rerr := s.cfg.MergeQueue.MergeDispatch(r.Context(), runID); rerr == nil && found {
			details["claim_state"] = string(c.State)
			details["claim_path"] = c.Path
			details["claim_head_sha"] = c.HeadSHA
			if c.State == mergequeue.DispatchFailed {
				next = mergeQueueResumeNextOperatorRearm
			}
		}
		details["next_step"] = next
		s.writeError(w, r, http.StatusConflict, mqWithdrawCodeClaimed,
			fmt.Sprintf("the run holds a merge-dispatch claim (ADR-109), so its merge intent is not withdrawn and the claim is untouched: a dispatched claim means a merge was handed to the forge, and a dispatch_failed claim needs reconciliation or the operator re-arm (backend/internal/mergequeue/README.md § \"Merge-dispatch ledger\") first; an inline merge would be refused merge_already_dispatched either way. Next: %s.", next), details)
	default:
		details[internalCauseKey] = err.Error()
		s.writeError(w, r, http.StatusInternalServerError, mqWithdrawCodeFailed,
			"the withdrawal failed and was rolled back; the merge-queue entry and its merge intent are unchanged", details)
	}
}
