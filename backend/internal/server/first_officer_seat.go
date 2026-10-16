package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/firstofficer"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// First-officer seat lifecycle (ADR-089 D1/D2, E85.6 / #4148): the captain's
// attach, status read and detach verbs. A seat IS its api token — attach
// state is the set of active api_tokens rows for the seat subject, so there
// is no new table. Attach is a captain act (ADR-089 D1): a human write:approvals
// caller who is the anchor repository's sitting captain, or any human
// write:approvals caller while the seat is vacant or unreadable. One seat per
// anchor rests on a pre-check plus a deterministic post-issue convergence
// (keep the oldest active token, revoke the rest). Long-form contract:
// backend/internal/server/README.md "First-officer seat (E85.6 / #4148)".

// auditFirstOfficerSeatAttached is the attach row (registered in
// backend/internal/audit/categories.go). It never carries the plaintext.
const auditFirstOfficerSeatAttached = "first_officer_seat_attached"

// seatRetiredCaptainDetached is the first_officer_seat_retired reason the
// captain's retire verb records.
const seatRetiredCaptainDetached = "captain_detached"

// firstOfficerSeatMaxBodyBytes caps the attach and retire bodies.
const firstOfficerSeatMaxBodyBytes = 8 << 10

// firstOfficerSeatNoteMaxLen bounds the retire reason.
const firstOfficerSeatNoteMaxLen = 1000

type firstOfficerSeatRequest struct {
	CampaignID string `json:"campaign_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	// Reason is the retire verb's required note; attach refuses it.
	Reason string `json:"reason,omitempty"`
}

type firstOfficerSeatAnchorView struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type firstOfficerSeatAttachResponse struct {
	SeatSubject string                     `json:"seat_subject"`
	Token       string                     `json:"token"`
	TokenID     string                     `json:"token_id"`
	Scopes      []string                   `json:"scopes"`
	Anchor      firstOfficerSeatAnchorView `json:"anchor"`
}

type firstOfficerSeatStatusResponse struct {
	SeatSubject    string                     `json:"seat_subject"`
	Attached       bool                       `json:"attached"`
	TokenIDs       []string                   `json:"token_ids"`
	Anchor         firstOfficerSeatAnchorView `json:"anchor"`
	AnchorState    string                     `json:"anchor_state"`
	AnchorTerminal bool                       `json:"anchor_terminal"`
}

type firstOfficerSeatRetireResponse struct {
	SeatSubject     string                     `json:"seat_subject"`
	RevokedTokenIDs []string                   `json:"revoked_token_ids"`
	Anchor          firstOfficerSeatAnchorView `json:"anchor"`
}

// seatAnchorRecord is what the lifecycle verbs read about an anchor.
type seatAnchorRecord struct {
	anchor    firstofficer.Anchor
	repo      string
	accountID string
	state     string
	terminal  bool
}

func anchorView(a firstofficer.Anchor) firstOfficerSeatAnchorView {
	return firstOfficerSeatAnchorView{Kind: string(a.Kind), ID: a.ID.String()}
}

// firstOfficerSeatsConfigured requires the token and audit repositories plus
// the anchor repositories; anything unwired is 503 with nothing written.
func (s *Server) firstOfficerSeatsConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.APITokenRepo == nil || s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil || s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "first_officer_seats_unconfigured",
			"the first-officer seat surface needs the token, audit, run and campaign repositories configured", nil)
		return false
	}
	return true
}

// parseSeatAnchor resolves exactly one of campaign_id / run_id.
func (s *Server) parseSeatAnchor(w http.ResponseWriter, r *http.Request, campaignID, runID string) (firstofficer.Anchor, bool) {
	campaignID, runID = strings.TrimSpace(campaignID), strings.TrimSpace(runID)
	if (campaignID == "") == (runID == "") {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"exactly one of campaign_id or run_id is required", nil)
		return firstofficer.Anchor{}, false
	}
	kind, raw, field := firstofficer.AnchorRun, runID, "run_id"
	if campaignID != "" {
		kind, raw, field = firstofficer.AnchorCampaign, campaignID, "campaign_id"
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			field+" must be a UUID", map[string]any{"field": field})
		return firstofficer.Anchor{}, false
	}
	return firstofficer.Anchor{Kind: kind, ID: id}, true
}

// loadSeatAnchor reads the anchor and refuses one outside the caller's
// account as not found (a foreign anchor's existence is not disclosed). A
// tenanted anchor is visible only to its own account; an untenanted anchor
// to any caller, the same ownership rule enforceAccount applies to a run.
func (s *Server) loadSeatAnchor(w http.ResponseWriter, r *http.Request, a firstofficer.Anchor) (*seatAnchorRecord, bool) {
	ctx := r.Context()
	rec := &seatAnchorRecord{anchor: a}
	notFound := func() (*seatAnchorRecord, bool) {
		if a.Kind == firstofficer.AnchorRun {
			s.writeError(w, r, http.StatusNotFound, "run_not_found", "no run with that id in your account",
				map[string]any{"run_id": a.ID.String()})
		} else {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found", "no campaign with that id in your account",
				map[string]any{"campaign_id": a.ID.String()})
		}
		return nil, false
	}
	switch a.Kind {
	case firstofficer.AnchorRun:
		rn, err := s.cfg.RunRepo.GetRun(ctx, a.ID)
		if errors.Is(err, run.ErrNotFound) {
			return notFound()
		}
		if err != nil {
			s.writeDBUnavailable(w, r)
			return nil, false
		}
		rec.repo, rec.accountID, rec.state, rec.terminal = rn.Repo, rn.AccountID, string(rn.State), rn.State.IsTerminal()
	default:
		c, err := s.cfg.CampaignRepo.GetCampaign(ctx, a.ID)
		if errors.Is(err, campaign.ErrNotFound) {
			return notFound()
		}
		if err != nil {
			s.writeDBUnavailable(w, r)
			return nil, false
		}
		acct, err := s.cfg.CampaignRepo.GetCampaignAccountID(ctx, a.ID)
		if errors.Is(err, campaign.ErrNotFound) {
			return notFound()
		}
		if err != nil {
			s.writeDBUnavailable(w, r)
			return nil, false
		}
		rec.repo, rec.accountID, rec.state, rec.terminal = c.Repo, acct, string(c.State), c.State.IsTerminal()
	}
	if rec.accountID != "" && rec.accountID != IdentityFrom(ctx).AccountID {
		return notFound()
	}
	return rec, true
}

// authorizeSeatCaptainAct is the captain gate shared by attach and retire
// (ADR-089 D1). An agent caller (an operator-agent, a run-bound mcp identity
// or a seat) is refused 403 first_officer_attach_requires_human; a delegated
// operator-agent attach waits for an operator_agent grant. While the anchor
// repository has a SITTING captain only that subject may act (403
// first_officer_attach_not_captain); a vacant or unreadable seat admits any
// human write:approvals holder, and the basis is recorded on the audit row.
func (s *Server) authorizeSeatCaptainAct(w http.ResponseWriter, r *http.Request, rec *seatAnchorRecord) (string, bool) {
	caller := IdentityFrom(r.Context()).Subject
	if isAgentSubject(caller) {
		s.writeError(w, r, http.StatusForbidden, "first_officer_attach_requires_human",
			"attaching or retiring a first-officer seat is a captain act; an agent identity cannot perform it",
			map[string]any{"subject": caller})
		return "", false
	}
	captainSubject, _, basis := s.currentCaptain(r.Context(), parseAccountUUID(rec.accountID), rec.repo)
	if basis == captainBasisCaptain && captainSubject != caller {
		s.writeError(w, r, http.StatusForbidden, "first_officer_attach_not_captain",
			"a captain is seated on this repository; only the captain may attach or retire its first-officer seat",
			map[string]any{"repo": rec.repo, "captain": captainSubject})
		return "", false
	}
	return basis, true
}

// decodeSeatRequest decodes a bounded, closed JSON body.
func (s *Server) decodeSeatRequest(w http.ResponseWriter, r *http.Request) (firstOfficerSeatRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, firstOfficerSeatMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req firstOfficerSeatRequest
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields", map[string]any{"error": err.Error()})
		return req, false
	}
	return req, true
}

// handleAttachFirstOfficerSeat implements POST /v0/first-officer-seats. Order:
// write:approvals; configured; body; human caller; anchor visible (404);
// captain gate (403); live anchor (409 first_officer_anchor_terminal); no
// active seat token (409 first_officer_seat_exists); Issue; post-issue
// convergence; first_officer_seat_attached; 201 with the token ONCE.
func (s *Server) handleAttachFirstOfficerSeat(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "write:approvals") {
		return
	}
	if !s.firstOfficerSeatsConfigured(w, r) {
		return
	}
	req, ok := s.decodeSeatRequest(w, r)
	if !ok {
		return
	}
	if req.Reason != "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason applies only to POST /v0/first-officer-seats/retire", map[string]any{"field": "reason"})
		return
	}
	anchor, ok := s.parseSeatAnchor(w, r, req.CampaignID, req.RunID)
	if !ok {
		return
	}
	if isAgentSubject(IdentityFrom(r.Context()).Subject) {
		// Refused before any anchor read, so an agent learns nothing about
		// the anchor's existence.
		s.authorizeSeatCaptainAct(w, r, &seatAnchorRecord{anchor: anchor})
		return
	}
	rec, ok := s.loadSeatAnchor(w, r, anchor)
	if !ok {
		return
	}
	basis, ok := s.authorizeSeatCaptainAct(w, r, rec)
	if !ok {
		return
	}
	if rec.terminal {
		s.writeError(w, r, http.StatusConflict, "first_officer_anchor_terminal",
			"a first-officer seat cannot be attached to a terminal "+string(anchor.Kind),
			map[string]any{"anchor_kind": string(anchor.Kind), "anchor_id": anchor.ID.String(), "anchor_state": rec.state})
		return
	}
	ctx := r.Context()
	subject := firstofficer.SubjectFor(anchor)
	existing, err := s.cfg.APITokenRepo.ListForSubject(ctx, subject)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error", "list seat tokens failed",
			map[string]any{"error": err.Error()})
		return
	}
	if len(existing) > 0 {
		s.writeSeatExists(w, r, subject, existing)
		return
	}
	tok, err := s.cfg.APITokenRepo.Issue(ctx, subject, firstofficer.SeatScopes())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error", "issue seat token failed",
			map[string]any{"error": err.Error()})
		return
	}
	won, unrevoked, err := s.convergeSeatTokens(ctx, subject, tok.ID)
	if err != nil {
		// Cannot prove ONE seat: withdraw ours (fail closed).
		s.withdrawSeatAttach(w, r, subject, tok.ID, "could not verify the seat is unique", err)
		return
	}
	if len(unrevoked) > 0 {
		// A losing token's revocation did not persist, so more than one seat
		// token may be active: never answer 201 or 409 over that. Token ids
		// are not secrets, and 5xx details are redacted, so the message names
		// them.
		s.withdrawSeatAttach(w, r, subject, tok.ID,
			"could not revoke losing seat token(s) "+strings.Join(unrevoked, ", ")+", so more than one may be active",
			errors.New("seat convergence revoke failed"))
		return
	}
	if !won {
		// No unrevoked loser means every losing token, ours included, was
		// revoked, so the message below is persisted fact.
		s.writeError(w, r, http.StatusConflict, "first_officer_seat_exists",
			"a concurrent attach won this anchor's seat; this request's token was revoked",
			map[string]any{"seat_subject": subject})
		return
	}
	caller := IdentityFrom(ctx).Subject
	payload, _ := json.Marshal(map[string]any{
		"seat_subject":  subject,
		"token_id":      tok.ID.String(),
		"anchor":        anchorView(anchor),
		"scopes":        tok.Scopes,
		"attached_by":   caller,
		"captain_basis": basis,
	})
	if err := s.appendSeatLifecycleRow(ctx, rec, auditFirstOfficerSeatAttached, audit.ActorUser, caller, payload); err != nil {
		// An unrecorded seat must not exist: withdraw the token.
		s.withdrawSeatAttach(w, r, subject, tok.ID, "record the seat attach failed", err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, firstOfficerSeatAttachResponse{
		SeatSubject: subject, Token: tok.PlainText, TokenID: tok.ID.String(), Scopes: tok.Scopes, Anchor: anchorView(anchor),
	})
}

func (s *Server) writeSeatExists(w http.ResponseWriter, r *http.Request, subject string, toks []*apitoken.Token) {
	ids := make([]string, 0, len(toks))
	for _, t := range toks {
		ids = append(ids, t.ID.String())
	}
	s.writeError(w, r, http.StatusConflict, "first_officer_seat_exists",
		"this anchor already has a first-officer seat; retire it first (POST /v0/first-officer-seats/retire)",
		map[string]any{"seat_subject": subject, "token_ids": ids})
}

// withdrawSeatAttach is an attach's fail-closed exit after Issue: it revokes
// this request's token and answers 500. The message states what was
// PERSISTED: when the revocation itself fails the token may still be active,
// so the envelope says so and names the token instead of claiming it was
// revoked.
func (s *Server) withdrawSeatAttach(w http.ResponseWriter, r *http.Request, subject string, id uuid.UUID, what string, cause error) {
	details := map[string]any{"error": cause.Error(), "seat_subject": subject, "token_id": id.String()}
	msg := what + "; the issued token was revoked"
	if rerr := s.revokeSeatToken(r.Context(), subject, id); rerr != nil {
		msg = what + "; revoking the issued token " + id.String() + " also failed, so it may still be active: " +
			"retire the seat (POST /v0/first-officer-seats/retire) to withdraw it"
		details["revoke_error"] = rerr.Error()
	}
	s.writeError(w, r, http.StatusInternalServerError, "internal_error", msg, details)
}

// convergeSeatTokens is the post-issue convergence: with more than one active
// token for the subject, the OLDEST (created_at, then id) is kept and every
// other one is revoked. It reports whether mine is the survivor and the ids
// of losing tokens whose Revoke FAILED (still possibly active). The outcome
// reflects persisted revocation: a non-empty unrevoked list means no winner,
// so the handler fails closed instead of answering 201 beside a live
// competitor or 409 claiming a revocation that did not happen.
func (s *Server) convergeSeatTokens(ctx context.Context, subject string, mine uuid.UUID) (won bool, unrevoked []string, err error) {
	toks, err := s.cfg.APITokenRepo.ListForSubject(ctx, subject)
	if err != nil {
		return false, nil, fmt.Errorf("list seat tokens: %w", err)
	}
	if len(toks) == 0 {
		return false, nil, errors.New("the issued seat token is not active")
	}
	sort.SliceStable(toks, func(i, j int) bool {
		if !toks[i].CreatedAt.Equal(toks[j].CreatedAt) {
			return toks[i].CreatedAt.Before(toks[j].CreatedAt)
		}
		return toks[i].ID.String() < toks[j].ID.String()
	})
	for _, t := range toks[1:] {
		if rerr := s.revokeSeatToken(ctx, subject, t.ID); rerr != nil {
			unrevoked = append(unrevoked, t.ID.String())
		}
	}
	if len(unrevoked) > 0 {
		return false, unrevoked, nil
	}
	return toks[0].ID == mine, nil, nil
}

// revokeSeatToken revokes one seat token, logging and returning any failure.
func (s *Server) revokeSeatToken(ctx context.Context, subject string, id uuid.UUID) error {
	if _, err := s.cfg.APITokenRepo.Revoke(ctx, id, subject); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "first-officer seat: revoke failed",
			slog.String("seat_subject", subject), slog.String("token_id", id.String()), slog.String("error", err.Error()))
		return err
	}
	return nil
}

// appendSeatLifecycleRow writes an attach/retire row: a run seat on the run's
// chain, a campaign seat on the global chain under the campaign's account.
func (s *Server) appendSeatLifecycleRow(ctx context.Context, rec *seatAnchorRecord, category string, actorKind audit.ActorKind, actorSubject string, payload []byte) error {
	now := time.Now().UTC()
	if rec.anchor.Kind == firstofficer.AnchorRun {
		_, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: rec.anchor.ID, Timestamp: now, Category: category,
			ActorKind: &actorKind, ActorSubject: &actorSubject, Payload: payload,
		})
		return err
	}
	_, err := s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp: now, Category: category, ActorKind: &actorKind, ActorSubject: &actorSubject,
		Payload: payload, AccountID: parseAccountUUID(rec.accountID),
	})
	return err
}

// handleGetFirstOfficerSeat implements GET /v0/first-officer-seats
// ?campaign_id=|run_id= (read:runs): the seat's attach state, derived from the
// active tokens for its subject, plus the anchor's state.
func (s *Server) handleGetFirstOfficerSeat(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "read:runs") {
		return
	}
	if !s.firstOfficerSeatsConfigured(w, r) {
		return
	}
	q := r.URL.Query()
	anchor, ok := s.parseSeatAnchor(w, r, q.Get("campaign_id"), q.Get("run_id"))
	if !ok {
		return
	}
	rec, ok := s.loadSeatAnchor(w, r, anchor)
	if !ok {
		return
	}
	subject := firstofficer.SubjectFor(anchor)
	toks, err := s.cfg.APITokenRepo.ListForSubject(r.Context(), subject)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error", "list seat tokens failed",
			map[string]any{"error": err.Error()})
		return
	}
	ids := make([]string, 0, len(toks))
	for _, t := range toks {
		ids = append(ids, t.ID.String())
	}
	s.writeJSON(w, r, http.StatusOK, firstOfficerSeatStatusResponse{
		SeatSubject: subject, Attached: len(ids) > 0, TokenIDs: ids, Anchor: anchorView(anchor),
		AnchorState: rec.state, AnchorTerminal: rec.terminal,
	})
}

// handleRetireFirstOfficerSeat implements POST /v0/first-officer-seats/retire
// {campaign_id|run_id, reason}: the captain detaches the seat. Same human and
// captain gate as attach; a terminal anchor is allowed (a captain may clean
// up). Every active token is revoked and first_officer_seat_retired
// {reason: captain_detached, note} is appended. No active token is 404
// first_officer_seat_not_attached.
func (s *Server) handleRetireFirstOfficerSeat(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "write:approvals") {
		return
	}
	if !s.firstOfficerSeatsConfigured(w, r) {
		return
	}
	req, ok := s.decodeSeatRequest(w, r)
	if !ok {
		return
	}
	note := strings.TrimSpace(req.Reason)
	if note == "" || len(note) > firstOfficerSeatNoteMaxLen {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("reason is required (1-%d characters)", firstOfficerSeatNoteMaxLen), map[string]any{"field": "reason"})
		return
	}
	anchor, ok := s.parseSeatAnchor(w, r, req.CampaignID, req.RunID)
	if !ok {
		return
	}
	if isAgentSubject(IdentityFrom(r.Context()).Subject) {
		s.authorizeSeatCaptainAct(w, r, &seatAnchorRecord{anchor: anchor})
		return
	}
	rec, ok := s.loadSeatAnchor(w, r, anchor)
	if !ok {
		return
	}
	if _, ok := s.authorizeSeatCaptainAct(w, r, rec); !ok {
		return
	}
	ctx := r.Context()
	subject := firstofficer.SubjectFor(anchor)
	toks, err := s.cfg.APITokenRepo.ListForSubject(ctx, subject)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error", "list seat tokens failed",
			map[string]any{"error": err.Error()})
		return
	}
	if len(toks) == 0 {
		s.writeError(w, r, http.StatusNotFound, "first_officer_seat_not_attached",
			"this anchor has no attached first-officer seat", map[string]any{"seat_subject": subject})
		return
	}
	revoked := make([]string, 0, len(toks))
	for _, t := range toks {
		if _, rerr := s.cfg.APITokenRepo.Revoke(ctx, t.ID, subject); rerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error", "revoke seat token failed",
				map[string]any{"error": rerr.Error(), "revoked_token_ids": revoked})
			return
		}
		revoked = append(revoked, t.ID.String())
	}
	caller := IdentityFrom(ctx).Subject
	payload, _ := json.Marshal(map[string]any{
		"seat_subject":      subject,
		"reason":            seatRetiredCaptainDetached,
		"note":              note,
		"anchor":            anchorView(anchor),
		"anchor_state":      rec.state,
		"revoked_token_ids": revoked,
		"retired_by":        caller,
	})
	if err := s.appendSeatLifecycleRow(ctx, rec, auditFirstOfficerSeatRetired, audit.ActorUser, caller, payload); err != nil {
		// The revocation stands (fail toward fewer live seats); the missing
		// row is logged and surfaced.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "first-officer seat retire: audit append failed",
			slog.String("seat_subject", subject), slog.String("error", err.Error()))
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"the seat was retired but its audit row could not be recorded", map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, firstOfficerSeatRetireResponse{
		SeatSubject: subject, RevokedTokenIDs: revoked, Anchor: anchorView(anchor),
	})
}
