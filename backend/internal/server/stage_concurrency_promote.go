package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Operator PROMOTE of a queued waiter in a local stage concurrency group
// (#4191): POST /v0/stages/{stage_id}/concurrency/promote moves the stage's
// queued slot row just ahead of every other fresh waiter in its group, so a
// stage that unblocks everyone else is admitted next. The store contract
// (lock order, refusals, the newest promote wins, the episode-restart rule
// that clears a promotion) is backend/internal/concurrency/README.md
// § "Promote". This verb never acts on the ADR-092/093 merge-candidate queue:
// that queue's promote is #4297's own verb (ADR-093 D7 merge_queue_promoted).

// Promote bounds: the request body and the recorded reason.
const (
	promoteBodyMax   = 8 << 10
	promoteReasonMax = 1000
)

// Promote response codes.
const (
	promoteCodeUnconfigured = "concurrency_promote_unconfigured"
	promoteCodeFailed       = "concurrency_promote_failed"
	promoteCodeNotQueued    = "concurrency_slot_not_queued"
	promoteCodeSlotHeld     = "concurrency_slot_held"
	promoteCodeNotLive      = "concurrency_waiter_not_live"
	promoteCodeSlotChanged  = "concurrency_slot_changed"
)

// promoteAuditAppender appends one chained audit row inside the caller's
// transaction (audit.AppendChainedTx's shape). It is the Server's injectable
// seam (promoteAuditAppendOverride), so a test can fail the append INSIDE the
// promotion transaction.
type promoteAuditAppender func(ctx context.Context, tx pgx.Tx, p audit.ChainAppendParams) (*audit.Entry, error)

// promoteAuditAppend returns the test override, or audit.AppendChainedTx.
func (s *Server) promoteAuditAppend() promoteAuditAppender {
	if s.promoteAuditAppendOverride != nil {
		return s.promoteAuditAppendOverride
	}
	return audit.AppendChainedTx
}

// promoteQueuedStageRequest is the POST body: exactly {"reason": string}.
type promoteQueuedStageRequest struct {
	Reason string `json:"reason"`
}

// promoteQueuedStageResponse is the 200 body. AuditSequence names the
// stage_concurrency_promoted row; it is omitted on a no-op (promoted:false,
// the stage was already first and nothing was written).
type promoteQueuedStageResponse struct {
	StageID       uuid.UUID           `json:"stage_id"`
	RunID         uuid.UUID           `json:"run_id"`
	Group         string              `json:"group"`
	Limit         int                 `json:"limit"`
	Promoted      bool                `json:"promoted"`
	OldPosition   int                 `json:"old_position"`
	NewPosition   int                 `json:"new_position"`
	Holders       []concurrencyHolder `json:"holders"`
	AuditSequence *int64              `json:"audit_sequence,omitempty"`
}

// stageConcurrencyPromotedPayload is the stage_concurrency_promoted payload.
type stageConcurrencyPromotedPayload struct {
	StageID     string `json:"stage_id"`
	RunID       string `json:"run_id"`
	Group       string `json:"group"`
	Limit       int    `json:"limit"`
	OldPosition int    `json:"old_position"`
	NewPosition int    `json:"new_position"`
	Reason      string `json:"reason"`
	Actor       string `json:"actor"`
}

// parsePromoteBody strictly decodes the body: ONE JSON object whose only key
// is "reason" (unknown keys, a trailing document and a non-object refused),
// then requires a reason that is non-empty after TrimSpace and at most
// promoteReasonMax bytes. ok=false means a 400 was written. It does not use
// decodeSingleJSONBody, which accepts unknown keys (approval condition 2).
func (s *Server) parsePromoteBody(w http.ResponseWriter, r *http.Request) (reason string, ok bool) {
	var body promoteQueuedStageRequest
	if r.Body != nil {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, promoteBodyMax))
		dec.DisallowUnknownFields()
		decErr := dec.Decode(&body)
		switch {
		case decErr != nil && !errors.Is(decErr, io.EOF):
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				`request body must be a single JSON object {"reason": string}`,
				map[string]any{"field": "body", "error": decErr.Error()})
			return "", false
		case decErr == nil:
			var trailing json.RawMessage
			if tErr := dec.Decode(&trailing); !errors.Is(tErr, io.EOF) {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					`request body must be a single JSON object {"reason": string}; trailing content is refused`,
					map[string]any{"field": "body"})
				return "", false
			}
		}
	}
	reason = strings.TrimSpace(body.Reason)
	if reason == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: say why this waiter should be admitted ahead of the others",
			map[string]any{"field": "reason"})
		return "", false
	}
	if len(reason) > promoteReasonMax {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("reason must be at most %d bytes", promoteReasonMax),
			map[string]any{"field": "reason"})
		return "", false
	}
	return reason, true
}

// handlePromoteQueuedStage implements POST
// /v0/stages/{stage_id}/concurrency/promote. Every rung is decided before any
// write:
//
//	503 concurrency_promote_unconfigured — no concurrency store or run repo.
//	401 authentication_required          — anonymous.
//	403 run_token_forbidden              — a run-bound agent token, even for
//	                                       its own run.
//	403 insufficient_scope               — a token without write:runs.
//	400 validation_failed                — a non-UUID stage_id, or a body that
//	                                       is not exactly {"reason": string}
//	                                       with a 1..1000-byte trimmed reason.
//
// Then the store's single transaction (concurrency README § "Promote"):
// 404 stage_not_found, 409 concurrency_slot_not_queued / concurrency_slot_held
// (holders are never preempted) / concurrency_waiter_not_live /
// concurrency_slot_changed, or 500 concurrency_promote_failed (rolled back,
// nothing changed). The stage_concurrency_promoted audit row is appended
// INSIDE that transaction through the Record callback, so a failed append
// rolls the promotion back. A stage already first answers 200 promoted:false
// and appends nothing.
//
// Auth: memberWrite via requireStageAccount (the sibling /v0/stages operator
// controls' tier). A delegated operator-agent token IS admitted, unlike
// review_rerun.go: this is an operational ordering control like dispatch and
// retry, not an approval, and the audit row records the actor and actor_kind.
func (s *Server) handlePromoteQueuedStage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Concurrency == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, promoteCodeUnconfigured,
			"concurrency promote requires the concurrency store and the run repository", nil)
		return
	}
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not promote a queued stage; reordering a concurrency queue is an operator action", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:runs") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:runs",
			map[string]any{"required_scope": "write:runs"})
		return
	}
	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}
	reason, ok := s.parsePromoteBody(w, r)
	if !ok {
		return
	}

	p, auditSeq, err := s.promoteQueuedStageRecorded(r.Context(), stageID, reason, actorKindForSubject(id.Subject), id.Subject)
	if err != nil {
		s.writePromoteError(w, r, stageID, err)
		return
	}
	resp := promoteQueuedStageResponse{
		StageID:     p.StageID,
		RunID:       p.RunID,
		Group:       p.GroupKey,
		Limit:       p.Limit,
		Promoted:    p.Changed,
		OldPosition: p.OldPosition,
		NewPosition: p.NewPosition,
		Holders:     toConcurrencyHolders(p.Holders),
	}
	if p.Changed {
		resp.AuditSequence = &auditSeq
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// promoteQueuedStageRecorded is the promote core shared by the operator verb
// and the campaign hotfix auto-promote (#4234, campaign_dispatch_hold.go): it
// runs the store's single promote transaction with a Record callback that
// appends the stage_concurrency_promoted row INSIDE it, so a failed append
// rolls the promotion back. reason is recorded verbatim; the caller owns its
// rules. auditSeq names the row and is meaningful only when p.Changed (a
// stage already first is a no-op: nothing appended).
func (s *Server) promoteQueuedStageRecorded(ctx context.Context, stageID uuid.UUID, reason string, actorKind audit.ActorKind, subject string) (p concurrency.Promotion, auditSeq int64, err error) {
	record := func(ctx context.Context, tx pgx.Tx, p concurrency.Promotion) error {
		raw, err := json.Marshal(stageConcurrencyPromotedPayload{
			StageID:     p.StageID.String(),
			RunID:       p.RunID.String(),
			Group:       p.GroupKey,
			Limit:       p.Limit,
			OldPosition: p.OldPosition,
			NewPosition: p.NewPosition,
			Reason:      reason,
			Actor:       subject,
		})
		if err != nil {
			return fmt.Errorf("marshal %s payload: %w", CategoryStageConcurrencyPromoted, err)
		}
		sid := p.StageID
		entry, err := s.promoteAuditAppend()(ctx, tx, audit.ChainAppendParams{
			RunID:        p.RunID,
			StageID:      &sid,
			Timestamp:    time.Now().UTC(),
			Category:     CategoryStageConcurrencyPromoted,
			ActorKind:    &actorKind,
			ActorSubject: &subject,
			Payload:      raw,
		})
		if err != nil {
			return fmt.Errorf("append %s: %w", CategoryStageConcurrencyPromoted, err)
		}
		auditSeq = entry.Sequence
		return nil
	}
	p, err = s.cfg.Concurrency.Promote(ctx, concurrency.PromoteRequest{StageID: stageID, Record: record})
	return p, auditSeq, err
}

// writePromoteError maps a Promote error to its response. Every refusal and
// failure leaves the queue unchanged (the store's transaction rolled back).
func (s *Server) writePromoteError(w http.ResponseWriter, r *http.Request, stageID uuid.UUID, err error) {
	details := map[string]any{"stage_id": stageID.String()}
	switch {
	case errors.Is(err, run.ErrNotFound):
		s.writeError(w, r, http.StatusNotFound, "stage_not_found", "no stage with that id", details)
	case errors.Is(err, concurrency.ErrSlotHeld):
		s.writeError(w, r, http.StatusConflict, promoteCodeSlotHeld,
			"the stage holds its concurrency slot; holders are never preempted, and only a queued waiter can be promoted", details)
	case errors.Is(err, concurrency.ErrNotQueued):
		s.writeError(w, r, http.StatusConflict, promoteCodeNotQueued,
			"the stage is not queued in a concurrency group (it never asked for a slot, or it is no longer pending/awaiting_host_dispatch); only a queued waiter can be promoted", details)
	case errors.Is(err, concurrency.ErrWaiterNotLive):
		s.writeError(w, r, http.StatusConflict, promoteCodeNotLive,
			"the stage's queued waiter is not live (no refresh within the queue TTL), so a promotion would be lost when it restarts at the tail; re-dispatch the stage to start a live waiter, then promote it", details)
	case errors.Is(err, concurrency.ErrSlotChanged):
		s.writeError(w, r, http.StatusConflict, promoteCodeSlotChanged,
			"the stage's slot row moved to another concurrency group during the promote; read the stage's concurrency block and retry", details)
	default:
		details[internalCauseKey] = err.Error()
		s.writeError(w, r, http.StatusInternalServerError, promoteCodeFailed,
			"the promotion failed and was rolled back; the queue order is unchanged", details)
	}
}
