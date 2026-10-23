package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// The operator PROMOTE of a held merge-queue entry (ADR-093 D2/D7, E87.6 /
// #4297): POST /v0/runs/{run_id}/merge-queue/promote moves the run's
// unmarked held entry to the head of its base's line through the store's
// order_key (mergequeue README § "Promote"), and appends merge_queue_promoted
// INSIDE the promotion transaction. This verb never touches a stage
// concurrency group: that waiter's promote is fishhawk_promote_queued_stage
// (stage_concurrency_promote.go, #4191). README.md § "Ejection, re-entry and
// promote (ADR-093 slice 3)" is the contract.

// CategoryMergeQueuePromoted is the ADR-093 D7 promote row, appended inside
// the promotion transaction (INTERNAL, not an issue-comment surface).
const CategoryMergeQueuePromoted = "merge_queue_promoted"

// Merge-queue promote response codes.
const (
	mqPromoteCodeUnconfigured = "merge_queue_unconfigured"
	mqPromoteCodeNotQueued    = "merge_queue_not_queued"
	mqPromoteCodeEntryLive    = "merge_queue_entry_live"
	mqPromoteCodeEntryYielded = "merge_queue_entry_yielded"
	mqPromoteCodeFailed       = "merge_queue_promote_failed"
)

// mergeQueuePromoteResponse is the 200 body. AuditSequence names the
// merge_queue_promoted row and LiveRunID the run holding the base's line;
// both are omitted when absent (a no-op writes no row). The MCP mirror
// (backend/internal/mcpserver/merge_queue_promote.go) decodes it; the json
// tags MUST byte-match.
type mergeQueuePromoteResponse struct {
	RunID         string `json:"run_id"`
	EntryID       string `json:"entry_id"`
	Repo          string `json:"repo"`
	BaseRef       string `json:"base_ref"`
	Promoted      bool   `json:"promoted"`
	OldPosition   int    `json:"old_position"`
	NewPosition   int    `json:"new_position"`
	LiveRunID     string `json:"live_run_id,omitempty"`
	AuditSequence *int64 `json:"audit_sequence,omitempty"`
}

// mergeQueuePromotedPayload is the merge_queue_promoted payload.
type mergeQueuePromotedPayload struct {
	EntryID     string `json:"entry_id"`
	RunID       string `json:"run_id"`
	Repo        string `json:"repo"`
	BaseRef     string `json:"base_ref"`
	OldPosition int    `json:"old_position"`
	NewPosition int    `json:"new_position"`
	Reason      string `json:"reason"`
	Actor       string `json:"actor"`
}

// mergeQueuePromoter returns the configured store's Promote capability, or
// nil when no merge queue is wired or the store does not implement it (a
// test fake embedding mergequeue.Store): the verb then answers 503 instead
// of panicking (approval condition M1).
func (s *Server) mergeQueuePromoter() mergequeue.Promoter {
	if s.cfg.MergeQueue == nil {
		return nil
	}
	p, ok := s.cfg.MergeQueue.(mergequeue.Promoter)
	if !ok {
		return nil
	}
	return p
}

// handleMergeQueuePromote implements POST /v0/runs/{run_id}/merge-queue/promote.
// The rungs, every one decided before any write:
//
//	401 authentication_required  — anonymous.
//	403 run_token_forbidden      — a run-bound agent token, even its own run.
//	403 insufficient_scope       — a token without write:runs.
//	400 validation_failed        — a non-UUID run_id, or a body that is not
//	                               exactly {"reason": string} with a
//	                               1..1000-byte trimmed reason.
//	503 merge_queue_unconfigured — no merge queue, or a store without the
//	                               Promote capability (after the auth rungs,
//	                               so an anonymous caller still reads 401).
//
// Then the store's single transaction (mergequeue README § "Promote"):
// 404 run_not_found, 409 merge_queue_not_queued / merge_queue_entry_live (the
// live entry is never preempted) / merge_queue_entry_yielded (naming the
// yield's next_step), or 500 merge_queue_promote_failed (rolled back, order
// unchanged). An entry already first answers 200 promoted:false and appends
// nothing; a change answers 200 promoted:true with the row's audit_sequence
// and pumps the base detached.
//
// Auth: memberWrite via requireRunAccount, the run-scoped operator controls'
// tier. A delegated operator-agent token IS admitted, like the concurrency
// promote: an ordering control, not an approval; the row records the actor.
func (s *Server) handleMergeQueuePromote(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not promote a merge-queue entry; reordering the merge queue is an operator action", nil)
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
	reason, ok := s.parsePromoteBody(w, r)
	if !ok {
		return
	}
	promoter := s.mergeQueuePromoter()
	if promoter == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, mqPromoteCodeUnconfigured,
			"the merge-queue promote requires a configured merge queue", nil)
		return
	}

	ctx := r.Context()
	subject := id.Subject
	actorKind := actorKindForSubject(subject)
	var auditSeq int64
	record := func(ctx context.Context, tx pgx.Tx, p mergequeue.Promotion) error {
		raw, err := json.Marshal(mergeQueuePromotedPayload{
			EntryID:     p.Entry.ID.String(),
			RunID:       p.Entry.RunID.String(),
			Repo:        p.Entry.Repo,
			BaseRef:     p.Entry.BaseRef,
			OldPosition: p.OldPosition,
			NewPosition: p.NewPosition,
			Reason:      reason,
			Actor:       subject,
		})
		if err != nil {
			return fmt.Errorf("marshal %s payload: %w", CategoryMergeQueuePromoted, err)
		}
		entry, err := s.promoteAuditAppend()(ctx, tx, audit.ChainAppendParams{
			RunID:        p.Entry.RunID,
			Timestamp:    time.Now().UTC(),
			Category:     CategoryMergeQueuePromoted,
			ActorKind:    &actorKind,
			ActorSubject: &subject,
			Payload:      raw,
		})
		if err != nil {
			return fmt.Errorf("append %s: %w", CategoryMergeQueuePromoted, err)
		}
		auditSeq = entry.Sequence
		return nil
	}
	p, err := promoter.Promote(ctx, mergequeue.PromoteRequest{RunID: runID, Record: record})
	if err != nil {
		s.writeMergeQueuePromoteError(w, r, runID, err)
		return
	}
	resp := mergeQueuePromoteResponse{
		RunID:       runID.String(),
		EntryID:     p.Entry.ID.String(),
		Repo:        p.Entry.Repo,
		BaseRef:     p.Entry.BaseRef,
		Promoted:    p.Changed,
		OldPosition: p.OldPosition,
		NewPosition: p.NewPosition,
	}
	if p.LiveRunID != nil {
		resp.LiveRunID = p.LiveRunID.String()
	}
	if p.Changed {
		resp.AuditSequence = &auditSeq
		// The promoted entry may now be the next to admit: hand the base
		// to the pump without waiting on an admission.
		s.pumpMergeCandidateQueueDetached(ctx, p.Entry.Base())
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// writeMergeQueuePromoteError maps a Promote error to its response. Every
// refusal and failure leaves the queue order unchanged (the store's
// transaction rolled back).
func (s *Server) writeMergeQueuePromoteError(w http.ResponseWriter, r *http.Request, runID uuid.UUID, err error) {
	details := map[string]any{"run_id": runID.String()}
	switch {
	case errors.Is(err, mergequeue.ErrNotFound):
		s.writeError(w, r, http.StatusNotFound, "run_not_found", "no run with that id", details)
	case errors.Is(err, mergequeue.ErrEntryLive):
		s.writeError(w, r, http.StatusConflict, mqPromoteCodeEntryLive,
			"the run's merge-queue entry is live (it holds its base's line); the live entry is never preempted, and only a held entry can be promoted", details)
	case errors.Is(err, mergequeue.ErrEntryYielded):
		next := mergeQueueYieldNextNone
		if st, found, rerr := s.cfg.MergeQueue.ActiveForRun(r.Context(), runID); rerr == nil && found && st.Entry.Yielded() {
			details["yield_reason"] = st.Entry.YieldReason
			next = mergeQueueYieldNextStep(st.Entry.YieldReason)
		}
		details["next_step"] = next
		s.writeError(w, r, http.StatusConflict, mqPromoteCodeEntryYielded,
			fmt.Sprintf("the run's merge-queue entry has yielded its place and admission skips it until the yield clears; take its next_step (%s) first, then promote", next), details)
	case errors.Is(err, mergequeue.ErrNotQueued):
		s.writeError(w, r, http.StatusConflict, mqPromoteCodeNotQueued,
			"the run has no held merge-queue entry (it was never queued, or its entry settled); queue it with fishhawk_merge_run, then promote", details)
	default:
		details[internalCauseKey] = err.Error()
		s.writeError(w, r, http.StatusInternalServerError, mqPromoteCodeFailed,
			"the promotion failed and was rolled back; the merge-queue order is unchanged", details)
	}
}
