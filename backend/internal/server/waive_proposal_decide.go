package server

// Waive-proposal decisions (E80.28 / #5229, ADR-143 Amendment A1): the
// confirm and reject halves of propose-and-confirm for a server_check
// concern. waive_proposal.go holds the propose half, the chain fold and the
// recorded-head derivation both handlers share.
//
// Both verbs are ADR-143 D2 verbs of the server_check_clear entry
// (clearVerbConfirmWaiveProposal, clearVerbRejectWaiveProposal): the ONE
// refuseNonHumanServerCheckClear call in each handler runs before any
// proposal read or append, so only a human on a Web UI `session` credential
// decides. An agent token or a delegated request gets concern_requires_human;
// every other bearer gets concern_requires_session. There is no MCP or CLI
// confirm or reject.
//
// Every proposal-state write goes through audit.GuardedChainAppender (V1). The
// out-of-lock pre-checks are fast paths; the in-lock guard re-runs the SAME
// fold (liveProposalMatches) and the SAME head derivation (recordedRunHead)
// under the run-row lock, in the append's own transaction. A confirm whose
// recorded head moved after its pre-check appends NOTHING (409
// waive_proposal_stale, dropped_recorded false); a later confirm or propose
// records the drop. A confirm and a reject racing on one proposal: the loser
// appends nothing and gets 409 waive_proposal_not_live.
//
// Y1: a confirmed waive's concern state_reason is NEVER agent-authored text,
// because state_reason is rendered into later re-review prompts as the
// "operator waive reason". It is the human's optional typed reason, or the
// fixed server template confirmWaiveProposalStateReason. The agent's reason
// and evidence stay only on the proposal row and its audit link.
//
// Y2 / V2: a failed disposition after the linked intent is followed by a
// concern_waive_failed naming the intent (intent_sequence), so the fold keeps
// the proposal live and the human can retry. The handler then returns the
// error and never reports success. RESIDUAL: if that corrective write fails
// too, the fold closes the proposal while the row stays open (see
// waive_proposal.go); a concern_waive decision row can exist for a concern
// that stayed open, and the remedy is a direct human waive or a new proposal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/redaction"
)

// confirmWaiveProposalRequest is the body of
// POST /v0/concerns/{concern_id}/waive-proposal/confirm.
type confirmWaiveProposalRequest struct {
	// ProposalSequence names the live proposal being confirmed (> 0).
	ProposalSequence int64 `json:"proposal_sequence"`
	// Reason is the human's OPTIONAL typed reason (Y1). When blank the server
	// records confirmWaiveProposalStateReason; the proposal's agent-authored
	// reason is never used.
	Reason string `json:"reason"`
	// Delegated is parsed only so the guard refuses it (ADR-143 Validation).
	Delegated bool `json:"delegated"`
}

// rejectWaiveProposalRequest is the body of
// POST /v0/concerns/{concern_id}/waive-proposal/reject.
type rejectWaiveProposalRequest struct {
	ProposalSequence int64  `json:"proposal_sequence"`
	Reason           string `json:"reason"`
	Delegated        bool   `json:"delegated"`
}

// confirmWaiveProposalResponse is the confirm's 200 body: the waived concern
// row plus the proposal it confirmed and the acting-identity echo (#5227).
type confirmWaiveProposalResponse struct {
	waiveConcernResponse
	ProposalSequence int64  `json:"proposal_sequence"`
	DecidedBy        string `json:"decided_by"`
	CredentialKind   string `json:"credential_kind"`
}

// rejectWaiveProposalResponse is the reject's 200 body.
type rejectWaiveProposalResponse struct {
	ConcernID        string `json:"concern_id"`
	ProposalSequence int64  `json:"proposal_sequence"`
	State            string `json:"state"`
	DecidedBy        string `json:"decided_by"`
	CredentialKind   string `json:"credential_kind"`
}

// confirmWaiveProposalStateReason is the fixed server template a confirm
// records as the concern's state_reason when the human typed no reason (Y1).
func confirmWaiveProposalStateReason(proposalSequence int64, human string) string {
	return fmt.Sprintf("confirmed agent waive proposal (audit seq %d) by %s; see the proposal for the agent's reasoning",
		proposalSequence, human)
}

// errWaiveProposalNotLive is the in-lock refusal when the requested proposal
// is no longer the concern's live proposal (another human confirmed, rejected
// or a drop closed it after the pre-check). Nothing is appended.
var errWaiveProposalNotLive = errors.New("the waive proposal is no longer live")

// waiveProposalStaleError is the confirm's in-lock refusal when the recorded
// run head is no longer the proposal's head (V1). Nothing is appended.
type waiveProposalStaleError struct{ ProposalHead, CurrentHead string }

func (e waiveProposalStaleError) Error() string {
	return fmt.Sprintf("the recorded run head moved from %s to %s since the proposal", e.ProposalHead, e.CurrentHead)
}

// decideWaiveProposalPrelude runs the shared refusal order of confirm and
// reject up to the concern read: 401, 403 insufficient_scope, 503
// concern_store_unconfigured, 400 concern_id, decode (which writes its own
// 400), 404, then 422 concern_not_server_check. Each handler then makes its
// ONE refuseNonHumanServerCheckClear call (before any proposal read or
// append) and calls refuseConcernNotOpen.
func (s *Server) decideWaiveProposalPrelude(w http.ResponseWriter, r *http.Request, decode func() bool) (Identity, *concern.Concern, bool) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return id, nil, false
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") && !hasScope(id, "write:fixups") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages or write:fixups",
			map[string]any{"required_scope": "write:stages or write:fixups"})
		return id, nil, false
	}
	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "concern_store_unconfigured",
			"waive-proposal decisions require concern + audit repositories", nil)
		return id, nil, false
	}
	concernID, err := uuid.Parse(r.PathValue("concern_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concern_id must be a valid UUID",
			map[string]any{"field": "concern_id", "got": r.PathValue("concern_id")})
		return id, nil, false
	}
	if !decode() {
		return id, nil, false
	}
	rows, err := s.cfg.ConcernRepo.GetByIDs(r.Context(), []uuid.UUID{concernID})
	if err != nil {
		if errors.Is(err, concern.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "concern_not_found", "no concern with that id", nil)
			return id, nil, false
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get concern failed", map[string]any{"error": err.Error()})
		return id, nil, false
	}
	row := rows[0]
	if !row.IsServerCheck() {
		s.writeError(w, r, http.StatusUnprocessableEntity, "concern_not_server_check",
			"only a server_check concern takes a waive proposal; a reviewer-raised concern is waived directly",
			map[string]any{"concern_id": row.ID.String(), "provenance": row.Provenance})
		return id, nil, false
	}
	return id, row, true
}

// refuseConcernNotOpen writes 409 concern_not_open for a row a human already
// waived, deferred or resolved, so a decision never leaves an orphan intent.
func (s *Server) refuseConcernNotOpen(w http.ResponseWriter, r *http.Request, row *concern.Concern) bool {
	if row.State.IsOpen() {
		return false
	}
	s.writeError(w, r, http.StatusConflict, "concern_not_open",
		fmt.Sprintf("concern %s is %s, not open; there is no waive proposal to decide", row.ID, row.State),
		map[string]any{"concern_id": row.ID.String(), "state": string(row.State)})
	return true
}

// decodeDecisionBody decodes a decision body and validates proposal_sequence
// and the optional (or, when required, mandatory) human reason: at most
// maxOperatorConcernBytes (refused, not truncated). A required reason is also
// credential-scanned like a proposal's text.
func (s *Server) decodeDecisionBody(w http.ResponseWriter, r *http.Request, dst any, seq *int64, reason *string, reasonRequired bool) bool {
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {proposal_sequence, reason?}",
				map[string]any{"error": err.Error()})
			return false
		}
	}
	if *seq <= 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"proposal_sequence is required and must be > 0: it names the proposal being decided",
			map[string]any{"field": "proposal_sequence"})
		return false
	}
	if reasonRequired && strings.TrimSpace(*reason) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: a rejection is recorded with the human's reason",
			map[string]any{"field": "reason"})
		return false
	}
	if len(*reason) > maxOperatorConcernBytes {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("reason is %d bytes; the maximum is %d (it is not truncated)", len(*reason), maxOperatorConcernBytes),
			map[string]any{"field": "reason", "bytes": len(*reason), "max": maxOperatorConcernBytes})
		return false
	}
	if reasonRequired {
		if _, hits := redaction.RedactDefault([]byte(*reason)); len(hits) > 0 {
			patterns := make([]string, 0, len(hits))
			for _, h := range hits {
				patterns = append(patterns, h.Pattern)
			}
			s.writeError(w, r, http.StatusBadRequest, "waive_proposal_contains_credential",
				"reason contains credential-shaped text; describe the finding without pasting the matched bytes",
				map[string]any{"field": "reason", "patterns": patterns})
			return false
		}
	}
	return true
}

// preFoldLiveProposal is the out-of-lock pre-fold both decisions share: 500 on
// a read error, 404 waive_proposal_not_found when the concern has no live
// proposal, 409 waive_proposal_not_live when the live one is not seq.
func (s *Server) preFoldLiveProposal(w http.ResponseWriter, r *http.Request, row *concern.Concern, seq int64) (*waiveProposal, bool) {
	st, err := loadWaiveProposalState(r.Context(), s.cfg.AuditRepo, row.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"reading waive-proposal state failed; nothing was recorded", map[string]any{"error": err.Error()})
		return nil, false
	}
	live, match := liveProposalMatches(st, row.ID, seq)
	if live == nil {
		s.writeError(w, r, http.StatusNotFound, "waive_proposal_not_found",
			fmt.Sprintf("concern %s has no live waive proposal", row.ID),
			map[string]any{"concern_id": row.ID.String(), "proposal_sequence": seq})
		return nil, false
	}
	if !match {
		s.writeWaiveProposalNotLive(w, r, row, seq, live.Sequence)
		return nil, false
	}
	return live, true
}

// writeWaiveProposalNotLive writes 409 waive_proposal_not_live. liveSeq is 0
// when the in-lock guard found no live proposal at all.
func (s *Server) writeWaiveProposalNotLive(w http.ResponseWriter, r *http.Request, row *concern.Concern, seq, liveSeq int64) {
	details := map[string]any{"concern_id": row.ID.String(), "proposal_sequence": seq}
	if liveSeq > 0 {
		details["live_proposal_sequence"] = liveSeq
	}
	s.writeError(w, r, http.StatusConflict, "waive_proposal_not_live",
		fmt.Sprintf("waive proposal %d is not the live proposal of concern %s; nothing was recorded", seq, row.ID), details)
}

// writeWaiveProposalStale writes 409 waive_proposal_stale.
func (s *Server) writeWaiveProposalStale(w http.ResponseWriter, r *http.Request, row *concern.Concern, seq int64, proposalHead, currentHead string, dropped bool) {
	s.writeError(w, r, http.StatusConflict, "waive_proposal_stale",
		"the PR head moved since the proposal was recorded, so it is no longer confirmable; nothing was waived. "+
			"The agent may propose again at the new head, or a human may waive or defer the concern directly",
		map[string]any{
			"concern_id":        row.ID.String(),
			"proposal_sequence": seq,
			"proposal_head_sha": proposalHead,
			"current_head_sha":  currentHead,
			"dropped_recorded":  dropped,
		})
}

// handleConfirmWaiveProposal implements
// POST /v0/concerns/{concern_id}/waive-proposal/confirm (ADR-143 A1): a human
// on a Web UI session confirms an agent's live waive proposal, waiving the
// concern in one step with the human as actor.
//
// Order: the decideWaiveProposalPrelude refusals (the human-channel guard
// before any proposal read); the pre-fold (404 waive_proposal_not_found, 409
// waive_proposal_not_live); the head pre-check (500 on a read error; a moved
// head records concern_waive_proposal_dropped through the guarded path and
// answers 409 waive_proposal_stale, dropped_recorded true). Then the
// concern_waived intent is appended through applyConcernWaiveLinked with an
// in-lock guard that re-folds and re-reads the recorded head: a moved head
// answers 409 waive_proposal_stale with NOTHING appended (dropped_recorded
// false, V1), a closed proposal 409 waive_proposal_not_live. A repository
// without audit.GuardedChainAppender answers 503
// waive_proposal_atomic_append_unavailable (no non-atomic fallback).
//
// The E82.1 delegation shadow is captured on the pre-decision state and
// recorded after the waive landed, exactly as handleWaiveConcern does.
func (s *Server) handleConfirmWaiveProposal(w http.ResponseWriter, r *http.Request) {
	var req confirmWaiveProposalRequest
	id, row, ok := s.decideWaiveProposalPrelude(w, r, func() bool {
		return s.decodeDecisionBody(w, r, &req, &req.ProposalSequence, &req.Reason, false)
	})
	if !ok {
		return
	}
	// ADR-143 D1/D2 as amended by A1: only a human on a Web UI session
	// confirms. Before any proposal read or append.
	if s.refuseNonHumanServerCheckClear(w, r, row, id.Subject, req.Delegated, clearVerbConfirmWaiveProposal) {
		return
	}
	if s.refuseConcernNotOpen(w, r, row) {
		return
	}
	ctx := r.Context()
	live, ok := s.preFoldLiveProposal(w, r, row, req.ProposalSequence)
	if !ok {
		return
	}
	head, err := recordedRunHead(ctx, s.cfg.AuditRepo, row.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"reading the recorded run head failed; nothing was recorded", map[string]any{"error": err.Error()})
		return
	}
	if head != live.HeadSHA {
		ga, gok := s.guardedAppender()
		if !gok {
			s.writeGuardedAppendUnavailable(w, r)
			return
		}
		dropped, derr := s.dropStaleWaiveProposal(ctx, ga, row, live, head)
		if derr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "audit_append_failed",
				"recording the stale proposal's drop failed; nothing was waived", map[string]any{"error": derr.Error()})
			return
		}
		s.writeWaiveProposalStale(w, r, row, live.Sequence, live.HeadSHA, head, dropped)
		return
	}
	judgement, err := concern.ResolveHumanJudgement(live.Judgement, live.TradeoffCost, "")
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"the proposal's recorded judgement does not validate; nothing was waived",
			map[string]any{"error": err.Error(), "proposal_sequence": live.Sequence})
		return
	}

	subject := id.Subject
	// Y1: human text or the fixed template, never the agent's reason.
	stateReason := strings.TrimSpace(req.Reason)
	if stateReason == "" {
		stateReason = confirmWaiveProposalStateReason(live.Sequence, subject)
	}
	link := &waiveProposalLink{
		ProposalSequence:    live.Sequence,
		ProposedBy:          live.ProposedBy,
		ProposedByActorKind: live.ProposedByActorKind,
		ProposalHeadSHA:     live.HeadSHA,
	}
	guard := func(ctx context.Context, read audit.ChainReader) error {
		gst, ferr := loadWaiveProposalState(ctx, read, row.RunID)
		if ferr != nil {
			return ferr
		}
		if _, match := liveProposalMatches(gst, row.ID, live.Sequence); !match {
			return errWaiveProposalNotLive
		}
		cur, herr := recordedRunHead(ctx, read, row.RunID)
		if herr != nil {
			return herr
		}
		if cur != live.HeadSHA {
			return waiveProposalStaleError{ProposalHead: live.HeadSHA, CurrentHead: cur}
		}
		return nil
	}
	// Delegation shadow stamp (E82.1 / #3778): captured on the PRE-decision
	// state, recorded only after the waive landed (handleWaiveConcern's shape).
	shadow := s.captureDelegationShadow(ctx, row.RunID, delegation.ActionWaive, subject, false)
	updated, err := s.applyConcernWaiveLinked(ctx, row, stateReason, subject, actorKindForSubject(subject),
		"", false, false, judgement, live.TradeoffCost, link, guard)
	if err != nil {
		s.writeConfirmError(w, r, row, live, err)
		return
	}
	stageID := row.StageID
	s.recordDelegationShadow(ctx, shadow, &stageID, "waive", CategoryConcernWaived, []uuid.UUID{updated.ID})
	s.writeJSON(w, r, http.StatusOK, confirmWaiveProposalResponse{
		waiveConcernResponse: waiveConcernResponse{
			ID:          updated.ID,
			RunID:       updated.RunID,
			StageID:     updated.StageID,
			StageKind:   updated.StageKind,
			Severity:    updated.Severity,
			Category:    updated.Category,
			Note:        updated.Note,
			State:       string(updated.State),
			StateReason: updated.StateReason,
		},
		ProposalSequence: live.Sequence,
		DecidedBy:        subject,
		CredentialKind:   approvalCredentialKind(id),
	})
}

// writeConfirmError maps applyConcernWaiveLinked's error for a confirm. The
// in-lock refusals and the missing capability appended nothing; the rest
// mirror handleWaiveConcern.
func (s *Server) writeConfirmError(w http.ResponseWriter, r *http.Request, row *concern.Concern, live *waiveProposal, err error) {
	var stale waiveProposalStaleError
	var appendErr concernWaiveAuditAppendError
	var bad concern.InvalidTransitionError
	switch {
	case errors.Is(err, errGuardedAppendUnavailable):
		s.writeGuardedAppendUnavailable(w, r)
	case errors.As(err, &stale):
		s.writeWaiveProposalStale(w, r, row, live.Sequence, stale.ProposalHead, stale.CurrentHead, false)
	case errors.Is(err, errWaiveProposalNotLive):
		s.writeWaiveProposalNotLive(w, r, row, live.Sequence, 0)
	case errors.As(err, &appendErr):
		s.writeError(w, r, http.StatusInternalServerError, "audit_append_failed",
			"appending the concern_waived audit entry failed; the waive was NOT applied",
			map[string]any{"error": appendErr.Unwrap().Error()})
	case errors.As(err, &bad):
		s.writeError(w, r, http.StatusUnprocessableEntity, "concern_waive_conflict",
			err.Error(), map[string]any{"from": string(bad.From), "to": string(bad.To)})
	case errors.Is(err, concern.ErrNotFound):
		s.writeError(w, r, http.StatusNotFound, "concern_not_found", "no concern with that id", nil)
	default:
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"confirm waive proposal failed; the proposal stays live", map[string]any{"error": err.Error()})
	}
}

// handleRejectWaiveProposal implements
// POST /v0/concerns/{concern_id}/waive-proposal/reject (ADR-143 A1): a human
// on a Web UI session rejects an agent's live waive proposal with a reason.
// The concern row is untouched and stays open; a new proposal may follow.
//
// Same order as confirm (guard verb clearVerbRejectWaiveProposal); a live
// proposal may be rejected whether or not its head moved.
// concern_waive_proposal_rejected is appended through the guarded path, whose
// in-lock guard requires the proposal still live, so a reject racing a
// confirm appends nothing and answers 409 waive_proposal_not_live.
func (s *Server) handleRejectWaiveProposal(w http.ResponseWriter, r *http.Request) {
	var req rejectWaiveProposalRequest
	id, row, ok := s.decideWaiveProposalPrelude(w, r, func() bool {
		return s.decodeDecisionBody(w, r, &req, &req.ProposalSequence, &req.Reason, true)
	})
	if !ok {
		return
	}
	// ADR-143 D1/D2 as amended by A1: only a human on a Web UI session
	// rejects. Before any proposal read or append.
	if s.refuseNonHumanServerCheckClear(w, r, row, id.Subject, req.Delegated, clearVerbRejectWaiveProposal) {
		return
	}
	if s.refuseConcernNotOpen(w, r, row) {
		return
	}
	live, ok := s.preFoldLiveProposal(w, r, row, req.ProposalSequence)
	if !ok {
		return
	}
	ga, ok := s.guardedAppender()
	if !ok {
		s.writeGuardedAppendUnavailable(w, r)
		return
	}
	subject := id.Subject
	actorKind := actorKindForSubject(subject)
	payload, _ := json.Marshal(map[string]any{
		"concern_id":             row.ID.String(),
		waiveProposalKeySequence: live.Sequence,
		"reason":                 req.Reason,
		"provenance":             row.Provenance,
	})
	guard := func(ctx context.Context, read audit.ChainReader) error {
		gst, ferr := loadWaiveProposalState(ctx, read, row.RunID)
		if ferr != nil {
			return ferr
		}
		if _, match := liveProposalMatches(gst, row.ID, live.Sequence); !match {
			return errWaiveProposalNotLive
		}
		return nil
	}
	_, err := ga.AppendChainedGuarded(r.Context(), audit.ChainAppendParams{
		RunID:        row.RunID,
		StageID:      &row.StageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryConcernWaiveProposalRejected,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}, guard)
	if errors.Is(err, errWaiveProposalNotLive) {
		s.writeWaiveProposalNotLive(w, r, row, live.Sequence, 0)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "audit_append_failed",
			"appending the concern_waive_proposal_rejected entry failed; nothing was rejected", map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, rejectWaiveProposalResponse{
		ConcernID:        row.ID.String(),
		ProposalSequence: live.Sequence,
		State:            "rejected",
		DecidedBy:        subject,
		CredentialKind:   approvalCredentialKind(id),
	})
}
