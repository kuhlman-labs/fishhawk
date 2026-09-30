package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The crew-message REST surface (E77.3 / #3737, ADR-081 #3727 D4 and rules
// 3-4). Five handlers over the E77.2 chain-authoritative mailbox:
//
//	POST /v0/crew-messages                                  send
//	GET  /v0/crew-messages                                  list (recipient OR anchor)
//	GET  /v0/crew-messages/{sequence}                       get one (+ opt-in ?wait=)
//	POST /v0/crew-messages/{sequence}/respond               refusal ladder
//	POST /v0/crew-messages/{sequence}/escalation-decision   captain decision
//
// Two invariants carry the design; the long-form contract is in this
// package's README ("Crew messages"):
//
//   - The SENDER ROLE is derived server-side from the caller's identity — a
//     run-bound token's executing stage (plan -> planner, review -> reviewer;
//     nothing else) or `captain` for an operator identity — and a body naming
//     a DIFFERENT role is refused (crewmessage.WithSenderRole).
//   - A crew message's TEXT leaves this server only in prompt.RenderCrewMessages'
//     quarantine envelope. No response carries the raw payload, and every read
//     route applies the SAME run-bound gate as send (write:messages AND the
//     token's own run), so an implement-stage token — which is never minted
//     write:messages — cannot read one (ARCHITECTURE.md §6 invariant #8).

// scopeWriteMessages is the stage-typed run-token scope handleIssueMCPToken
// mints for plan and review stages ONLY (mcptoken.go).
const scopeWriteMessages = "write:messages"

// crewMessageMaxBodyBytes caps a send / decision body. A crew message is a
// bounded, contract-closed document; 64 KiB is far above any legal one.
const crewMessageMaxBodyBytes = 64 << 10

// maxCrewMessageWaitSeconds caps the opt-in ?wait= long-poll on GET one,
// mirroring maxScopeAmendmentWaitSeconds' forward-safe posture (fishhawkd
// sets no WriteTimeout). A held wait returns the unchanged single-read
// envelope on EXPIRY — the message stays open; an expiry is never a denial.
const maxCrewMessageWaitSeconds = 30

// crewMessageWaitPollInterval is how often the ?wait long-poll re-reads the
// row and its thread. A package var so tests can shorten it.
var crewMessageWaitPollInterval = 500 * time.Millisecond

// crewMessageListLimitMax caps ?limit on the recipient list.
const crewMessageListLimitMax = 500

// crewAnchorResponse is the wire anchor: exactly one member is set.
type crewAnchorResponse struct {
	RunID            *uuid.UUID `json:"run_id,omitempty"`
	IssueRef         string     `json:"issue_ref,omitempty"`
	DecisionRecordID string     `json:"decision_record_id,omitempty"`
}

// crewMessageResponse is one crew_messages row's CONTRACT-CLOSED metadata.
// It deliberately carries no payload field: the message text reaches a caller
// only through Rendered (GET one) — never raw.
type crewMessageResponse struct {
	SentSequence        int64              `json:"sent_sequence"`
	ThreadRootSequence  int64              `json:"thread_root_sequence"`
	MessageType         string             `json:"message_type"`
	SenderRole          string             `json:"sender_role"`
	RecipientRole       string             `json:"recipient_role"`
	Anchor              crewAnchorResponse `json:"anchor"`
	ResponseRequired    bool               `json:"response_required"`
	Deadline            *time.Time         `json:"deadline,omitempty"`
	State               string             `json:"state"`
	DispositionSequence *int64             `json:"disposition_sequence,omitempty"`
	ReasonSequence      *int64             `json:"reason_sequence,omitempty"`
	Round               int                `json:"round"`
	SentAt              time.Time          `json:"sent_at"`
	DisposedAt          *time.Time         `json:"disposed_at,omitempty"`
	// ProjectionDegraded is set on a send/decision whose chain entry
	// COMMITTED but whose derived row did not project: the operation
	// HAPPENED and must not be retried (crewmessage.ProjectionError).
	ProjectionDegraded bool `json:"projection_degraded,omitempty"`
	// ConsultDeadline and ConsultBudgetRemaining are set ONLY on the send
	// response of a run-bound response_required consult (E77.5 / #3739): the
	// effective deadline the responder is held to, and how many more consults
	// the stage may send. Both omitempty, so every other response is
	// byte-identical to E77.3's.
	ConsultDeadline        *time.Time `json:"consult_deadline,omitempty"`
	ConsultBudgetRemaining *int       `json:"consult_budget_remaining,omitempty"`
}

// crewMessageAnswer is the first reply in a consulted message's thread, in
// rendered form only.
type crewMessageAnswer struct {
	SentSequence int64  `json:"sent_sequence"`
	SenderRole   string `json:"sender_role"`
	Rendered     string `json:"rendered"`
}

// crewMessageGetResponse is GET one: metadata plus the prompt-rendered form.
type crewMessageGetResponse struct {
	crewMessageResponse
	// Rendered is prompt.RenderCrewMessages over this one message — the SAME
	// bytes the three reviewed prompts embed.
	Rendered string `json:"rendered"`
	// Answered reports whether a reply exists in the thread after this
	// message; Answer carries it, rendered.
	Answered bool               `json:"answered"`
	Answer   *crewMessageAnswer `json:"answer,omitempty"`
}

type crewMessageListResponse struct {
	Items []crewMessageResponse `json:"items"`
}

// crewEscalationDecisionRequest is the escalation-decision body.
type crewEscalationDecisionRequest struct {
	Decision string `json:"decision"` // accepted | rejected
	Reason   string `json:"reason"`
}

func crewRowToResponse(row crewmessage.Row) crewMessageResponse {
	return crewMessageResponse{
		SentSequence:       row.SentSequence,
		ThreadRootSequence: row.ThreadRootSequence,
		MessageType:        string(row.MessageType),
		SenderRole:         string(row.SenderRole),
		RecipientRole:      string(row.RecipientRole),
		Anchor: crewAnchorResponse{
			RunID: row.RunID, IssueRef: row.IssueRef, DecisionRecordID: row.DecisionRecordID,
		},
		ResponseRequired:    row.ResponseRequired,
		Deadline:            row.Deadline,
		State:               string(row.State),
		DispositionSequence: row.DispositionSequence,
		ReasonSequence:      row.ReasonSequence,
		Round:               row.Round,
		SentAt:              row.SentAt,
		DisposedAt:          row.DisposedAt,
	}
}

// crewSenderRoleForStage maps a run-bound token's executing stage type onto
// the crew role it sends as. A POSITIVE allow-list over the two stage types
// write:messages is minted for: a future stage type — and implement, deploy
// and acceptance today — derives no role and is refused, so a token that
// outlived its stage cannot send under the next stage's identity.
func crewSenderRoleForStage(st run.StageType) (crewmessage.Role, bool) {
	switch st {
	case run.StageTypePlan:
		return crewmessage.RolePlanner, true
	case run.StageTypeReview:
		return crewmessage.RoleReviewer, true
	}
	return "", false
}

// parseCrewMessageWaitSeconds parses the opt-in ?wait= value. Absent, empty,
// non-integer, zero or negative is 0 (single read); anything above the cap is
// clamped to maxCrewMessageWaitSeconds.
func parseCrewMessageWaitSeconds(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	if n > maxCrewMessageWaitSeconds {
		return maxCrewMessageWaitSeconds
	}
	return n
}

// crewAccountVisible is the run-less account predicate, mirroring the audit
// list's NULL-allow window: an identity without an account is unconstrained,
// and an untenanted row stays visible.
func crewAccountVisible(identityAccount, rowAccount *uuid.UUID) bool {
	if identityAccount == nil || rowAccount == nil {
		return true
	}
	return *identityAccount == *rowAccount
}

// crewConfigured answers 503 crew_message_unconfigured unless the mailbox and
// the run and audit repositories it reads through are wired. Dropping
// cfg.CrewMailbox in serve.go is the documented partial rollback.
func (s *Server) crewConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.CrewMailbox == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "crew_message_unconfigured",
			"crew-message endpoints require the crew mailbox, run, and audit repositories", nil)
		return false
	}
	return true
}

// crewAuthenticated answers 401 for an anonymous caller.
func (s *Server) crewAuthenticated(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return id, false
	}
	return id, true
}

// requireCrewMessagesScope is the run-bound half of every send AND read: the
// token must hold write:messages, which is minted for plan and review stages
// only — so an implement-stage token is refused here on every route.
func (s *Server) requireCrewMessagesScope(w http.ResponseWriter, r *http.Request, id Identity) bool {
	if !hasScope(id, scopeWriteMessages) {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: "+scopeWriteMessages,
			map[string]any{"required_scope": scopeWriteMessages})
		return false
	}
	return true
}

// writeCrossRunCrewMessage is the run-anchor binding refusal: a run-bound
// token's authority is its run, so another run's anchor AND every run-less
// anchor are refused.
func (s *Server) writeCrossRunCrewMessage(w http.ResponseWriter, r *http.Request, tokenRunID uuid.UUID, anchorRunID string) {
	details := map[string]any{"token_run_id": tokenRunID.String()}
	if anchorRunID == "" {
		details["anchor"] = "run_less"
	} else {
		details["anchor_run_id"] = anchorRunID
	}
	s.writeError(w, r, http.StatusForbidden, "cross_run_crew_message",
		"a run-bound token may only send or read crew messages anchored on its own run", details)
}

// authorizeCrewRead is the read gate every read route applies to ONE row:
//
//   - run-bound token: write:messages (insufficient_scope) AND the row's
//     anchor is the token's own run — another run's message and every
//     run-less message are refused (cross_run_crew_message);
//   - any other identity: read:audit, and the row's account must be the
//     caller's (404 crew_message_not_found, so existence does not leak).
func (s *Server) authorizeCrewRead(w http.ResponseWriter, r *http.Request, id Identity, row crewmessage.Row) bool {
	if tokenRunID, runBound := runBoundTokenRunID(id); runBound {
		if !s.requireCrewMessagesScope(w, r, id) {
			return false
		}
		if row.RunID == nil || *row.RunID != tokenRunID {
			anchor := ""
			if row.RunID != nil {
				anchor = row.RunID.String()
			}
			s.writeCrossRunCrewMessage(w, r, tokenRunID, anchor)
			return false
		}
		return true
	}
	if !s.requireWriteScope(w, r, "read:audit") {
		return false
	}
	if !crewAccountVisible(identityAccountID(r.Context()), row.AccountID) {
		s.writeError(w, r, http.StatusNotFound, "crew_message_not_found",
			"no crew message at that sequence", nil)
		return false
	}
	return true
}

// readCrewBody reads a bounded request body.
func (s *Server) readCrewBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, crewMessageMaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
				"request body exceeds size cap", map[string]any{"limit_bytes": crewMessageMaxBodyBytes})
			return nil, false
		}
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return nil, false
	}
	return body, true
}

// parseCrewSequence reads the {sequence} path value.
func (s *Server) parseCrewSequence(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("sequence")
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq <= 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"sequence must be a positive integer", map[string]any{"field": "sequence", "got": raw})
		return 0, false
	}
	return seq, true
}

// writeCrewMessageError maps the crewmessage sentinels onto named HTTP
// refusals. Every refusal it maps is returned BEFORE the mailbox appends.
func (s *Server) writeCrewMessageError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		schemaErr *crewmessage.SchemaError
		parseErr  *crewmessage.ParseError
	)
	switch {
	case errors.Is(err, crewmessage.ErrSenderRoleNotSettable):
		s.writeError(w, r, http.StatusBadRequest, "sender_role_not_settable", err.Error(),
			map[string]any{"field": "sender_role"})
	case errors.Is(err, crewmessage.ErrRecipientNotAddressable):
		s.writeError(w, r, http.StatusUnprocessableEntity, "recipient_not_addressable", err.Error(),
			map[string]any{"field": "recipient_role"})
	case errors.Is(err, crewmessage.ErrDuplicateMember),
		errors.Is(err, crewmessage.ErrResponseNotAnswerable):
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(), nil)
	case errors.As(err, &schemaErr):
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", schemaErr.Message,
			map[string]any{"instance_location": schemaErr.Path})
	case errors.As(err, &parseErr):
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", parseErr.Error(), nil)
	case errors.Is(err, crewmessage.ErrMessageNotFound):
		s.writeError(w, r, http.StatusNotFound, "crew_message_not_found",
			"no crew message at that sequence", nil)
	case errors.Is(err, crewmessage.ErrAlreadyDisposed):
		s.writeError(w, r, http.StatusConflict, "crew_message_already_disposed",
			"the crew message is no longer open", nil)
	case errors.Is(err, crewmessage.ErrInvalidDisposition):
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(),
			map[string]any{"field": "decision"})
	case errors.Is(err, crewmessage.ErrRoundBoundExhausted):
		s.writeError(w, r, http.StatusUnprocessableEntity, "crew_round_bound_exhausted",
			"the thread's reject-and-reply round bound is exhausted; it has been escalated", nil)
	case errors.Is(err, crewmessage.ErrThreadRootNotFound),
		errors.Is(err, crewmessage.ErrThreadRootNotRoot),
		errors.Is(err, crewmessage.ErrThreadCrossAccount),
		errors.Is(err, crewmessage.ErrThreadAnchorMismatch):
		s.writeError(w, r, http.StatusUnprocessableEntity, "crew_thread_invalid", err.Error(), nil)
	default:
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"crew message operation failed", map[string]any{"error": err.Error()})
	}
}

// handleSendCrewMessage implements POST /v0/crew-messages. The body is a
// crew-message-v1 document whose sender_role may be omitted (it is derived)
// or equal to the derived role; any other value is refused
// sender_role_not_settable with NO chain entry.
func (s *Server) handleSendCrewMessage(w http.ResponseWriter, r *http.Request) {
	if !s.crewConfigured(w, r) {
		return
	}
	id, ok := s.crewAuthenticated(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var (
		role      crewmessage.Role
		actor     crewmessage.Actor
		execStage *run.Stage
	)
	tokenRunID, runBound := runBoundTokenRunID(id)
	if runBound {
		if !s.requireCrewMessagesScope(w, r, id) {
			return
		}
		runRow, err := s.cfg.RunRepo.GetRun(ctx, tokenRunID)
		if err != nil {
			if errors.Is(err, run.ErrNotFound) {
				s.writeError(w, r, http.StatusNotFound, "run_not_found", "the token's run does not exist", nil)
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"get run failed", map[string]any{"error": err.Error()})
			return
		}
		var st run.StageType
		if execStage = s.resolveExecutingStage(r, runRow); execStage != nil {
			st = execStage.Type
		}
		derived, ok := crewSenderRoleForStage(st)
		if !ok {
			s.writeError(w, r, http.StatusForbidden, "crew_sender_not_derivable",
				"the token's run has no executing plan or review stage to derive a crew sender role from",
				map[string]any{"stage_type": string(st)})
			return
		}
		role = derived
		actor = crewmessage.Actor{Kind: audit.ActorAgent, Subject: id.Subject}
	} else {
		if !s.requireWriteScope(w, r, "write:stages") {
			return
		}
		role = crewmessage.RoleCaptain
		actor = crewmessage.Actor{Kind: audit.ActorUser, Subject: id.Subject}
	}

	body, ok := s.readCrewBody(w, r)
	if !ok {
		return
	}
	// WithSenderRole walks the caller's RAW bytes for duplicate members
	// before anything can collapse them, then refuses a different role.
	doc, err := crewmessage.WithSenderRole(body, role)
	if err != nil {
		s.writeCrewMessageError(w, r, err)
		return
	}
	// Parse once here to read the anchor from the SAME single interpretation
	// Send will record (Send re-parses; both see identical bytes).
	msg, err := crewmessage.Parse(doc)
	if err != nil {
		s.writeCrewMessageError(w, r, err)
		return
	}
	if runBound {
		anchorRun, perr := uuid.Parse(msg.Anchor.RunID)
		if msg.Anchor.RunID == "" || perr != nil || anchorRun != tokenRunID {
			s.writeCrossRunCrewMessage(w, r, tokenRunID, msg.Anchor.RunID)
			return
		}
	} else if msg.Anchor.RunID != "" {
		anchorRun, perr := uuid.Parse(msg.Anchor.RunID)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"anchor.run_id must be a valid UUID", map[string]any{"field": "anchor.run_id"})
			return
		}
		runRow, gerr := s.cfg.RunRepo.GetRun(ctx, anchorRun)
		if gerr != nil {
			if errors.Is(gerr, run.ErrNotFound) {
				s.writeError(w, r, http.StatusNotFound, "run_not_found", "no run with that id",
					map[string]any{"field": "anchor.run_id"})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"get run failed", map[string]any{"error": gerr.Error()})
			return
		}
		if !s.enforceAccount(w, r, memberWrite, runRow) {
			return
		}
	}

	// The consult branch (E77.5 / #3739): a run-bound response_required
	// consult is bounded and routed BEFORE Send, so both refusals append
	// NOTHING. It reads the SAME parsed msg the chain will record.
	params := crewmessage.SendParams{RawMessage: doc, Actor: actor, AccountID: identityAccountID(ctx)}
	var (
		consult       *CrewConsultRequest
		responder     CrewResponder
		budgetRemains int
	)
	if runBound && msg.Type == crewmessage.TypeConsult && msg.ResponseRequired {
		used, err := s.crewConsultBudgetUsed(ctx, tokenRunID, execStage.ID)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"count crew consults failed", map[string]any{"error": err.Error()})
			return
		}
		if used >= maxCrewConsultsPerStage {
			s.writeError(w, r, http.StatusUnprocessableEntity, "crew_consult_budget_exhausted",
				"this stage has exhausted its consult budget; proceed on what you can infer",
				map[string]any{"max": maxCrewConsultsPerStage, "used": used})
			return
		}
		var found bool
		if responder, found = s.cfg.CrewResponders.Lookup(msg.RecipientRole); !found {
			s.writeError(w, r, http.StatusUnprocessableEntity, "crew_responder_unavailable",
				"no responder is registered for the consulted role; proceed on what you can infer",
				map[string]any{"recipient_role": string(msg.RecipientRole)})
			return
		}
		stageID := execStage.ID
		params.StageID = &stageID
		budgetRemains = maxCrewConsultsPerStage - used - 1
		consult = &CrewConsultRequest{
			RunID: tokenRunID, StageID: stageID,
			SenderRole: msg.SenderRole, RecipientRole: msg.RecipientRole, Anchor: msg.Anchor,
			Question: msg.Payload.Question, WhatICanInfer: msg.Payload.WhatICanInfer, Context: msg.Payload.Context,
			Evidence: msg.Evidence, Deadline: crewConsultDeadline(msg, time.Now().UTC()),
		}
	}

	row, err := s.cfg.CrewMailbox.Send(ctx, params)
	var projErr *crewmessage.ProjectionError
	degraded := errors.As(err, &projErr) && row != nil
	if err != nil && !degraded {
		s.writeCrewMessageError(w, r, err)
		return
	}
	resp := crewRowToResponse(*row)
	resp.ProjectionDegraded = degraded
	// E77.6 (#3740): a newly sent ROOT escalation pages the captain. Fired after
	// the response-shaping decision above, so a notifier outage can never turn a
	// recorded send into an HTTP error and never suppresses the
	// ProjectionError-degraded 200.
	s.notifyRootEscalationPage(ctx, msg, params.ThreadRootSequence, row)
	if consult != nil {
		// The consult is on the chain (a projection gap does not undo it), so
		// the responder runs either way; Dispose falls back to upserting the
		// terminal row when the send projection never landed.
		consult.SentSequence = row.SentSequence
		deadline := consult.Deadline
		resp.ConsultDeadline = &deadline
		resp.ConsultBudgetRemaining = &budgetRemains
		s.dispatchCrewConsult(ctx, responder, *consult)
	}
	s.writeJSON(w, r, http.StatusCreated, resp)
}

// notifyRootEscalationPage fires the pings-only page-class hook for a newly
// SENT crew escalation (E77.6 / #3740). Before this, neither crew-message
// handler called a notifier at all, so the escalation chain entries were silent
// on the issue thread.
//
// The send-time condition MATCHES the projection (binding approval condition 2):
// issuecomment's crew_message_sent case yields a page ONLY for a thread ROOT of
// type `escalation`, so notifying on a THREADED reply would page for an event
// the projection never produces — the ping would find nothing new and the log
// line would be the only trace. threadRoot non-nil therefore notifies NOTHING.
//
// A run-less anchor (issue_ref / decision_record_id) has no run chain and no
// anchor comment, so row.RunID == nil pages nothing; that residual is stated in
// docs/issue-comment-surfaces.md rather than left implicit.
func (s *Server) notifyRootEscalationPage(ctx context.Context, msg *crewmessage.Message, threadRoot *int64, row *crewmessage.Row) {
	if msg == nil || row == nil || row.RunID == nil {
		return
	}
	if msg.Type != crewmessage.TypeEscalation || threadRoot != nil {
		return
	}
	s.notifyPageClass(ctx, *row.RunID, crewmessage.CategorySent)
}

// handleGetCrewMessage implements GET /v0/crew-messages/{sequence}: the row's
// metadata plus ONLY the prompt-rendered form. ?wait=<seconds> (clamped)
// holds the request until the message leaves open or a reply appears in its
// thread; on expiry the unchanged single-read envelope is returned.
func (s *Server) handleGetCrewMessage(w http.ResponseWriter, r *http.Request) {
	if !s.crewConfigured(w, r) {
		return
	}
	id, ok := s.crewAuthenticated(w, r)
	if !ok {
		return
	}
	// A run-bound token without write:messages is refused BEFORE any read, so
	// an implement-stage token learns nothing — not even existence.
	if _, runBound := runBoundTokenRunID(id); runBound && !s.requireCrewMessagesScope(w, r, id) {
		return
	}
	seq, ok := s.parseCrewSequence(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	store := s.cfg.CrewMailbox.Store()
	row, err := store.Get(ctx, seq)
	if err != nil {
		s.writeCrewMessageError(w, r, err)
		return
	}
	if !s.authorizeCrewRead(w, r, id, row) {
		return
	}

	wait := parseCrewMessageWaitSeconds(r.URL.Query().Get("wait"))
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	reply, err := s.crewThreadReply(ctx, row)
	if err != nil {
		s.writeCrewMessageError(w, r, err)
		return
	}
	for wait > 0 && row.State == crewmessage.StateOpen && reply == nil && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(crewMessageWaitPollInterval):
		}
		if row, err = store.Get(ctx, seq); err != nil {
			s.writeCrewMessageError(w, r, err)
			return
		}
		if reply, err = s.crewThreadReply(ctx, row); err != nil {
			s.writeCrewMessageError(w, r, err)
			return
		}
	}

	rendered, err := s.renderCrewRow(ctx, row)
	if err != nil {
		s.writeCrewMessageError(w, r, err)
		return
	}
	resp := crewMessageGetResponse{crewMessageResponse: crewRowToResponse(row), Rendered: rendered}
	if reply != nil {
		answer, err := s.renderCrewRow(ctx, *reply)
		if err != nil {
			s.writeCrewMessageError(w, r, err)
			return
		}
		resp.Answered = true
		resp.Answer = &crewMessageAnswer{
			SentSequence: reply.SentSequence, SenderRole: string(reply.SenderRole), Rendered: answer,
		}
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// crewThreadReply returns the first message in row's thread sent AFTER row,
// or nil. Replies share the thread root's anchor, so the anchor list holds
// every one.
func (s *Server) crewThreadReply(ctx context.Context, row crewmessage.Row) (*crewmessage.Row, error) {
	rows, err := s.cfg.CrewMailbox.Store().ListByAnchor(ctx, crewmessage.AnchorFilter{
		RunID: row.RunID, IssueRef: row.IssueRef, DecisionRecordID: row.DecisionRecordID,
	})
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ThreadRootSequence == row.ThreadRootSequence && rows[i].SentSequence > row.SentSequence {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// crewSentDocument resolves row's crew_message_sent chain entry — the
// authority; the row holds no text — and parses its document. The entry hash
// must match the row's citation, so a row can never render another entry.
func (s *Server) crewSentDocument(ctx context.Context, row crewmessage.Row) (*crewmessage.Message, error) {
	var (
		entries []*audit.Entry
		err     error
	)
	if row.RunID != nil {
		entries, err = s.cfg.AuditRepo.ListForRunByCategory(ctx, *row.RunID, crewmessage.CategorySent)
	} else {
		category := crewmessage.CategorySent
		params := audit.ListAllParams{Category: &category}
		if row.AccountID != nil {
			params.AccountID = row.AccountID.String()
		}
		entries, err = s.cfg.AuditRepo.ListAll(ctx, params)
	}
	if err != nil {
		return nil, fmt.Errorf("crew message %d: read chain: %w", row.SentSequence, err)
	}
	for _, e := range entries {
		if e.Sequence != row.SentSequence {
			continue
		}
		if e.Category != crewmessage.CategorySent || e.EntryHash != row.SentEntryHash {
			return nil, fmt.Errorf("crew message %d: chain entry does not match the row's citation", row.SentSequence)
		}
		var payload struct {
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return nil, fmt.Errorf("crew message %d: decode chain payload: %w", row.SentSequence, err)
		}
		msg, err := crewmessage.Parse(payload.Message)
		if err != nil {
			return nil, fmt.Errorf("crew message %d: recorded document no longer parses: %v", row.SentSequence, err)
		}
		return msg, nil
	}
	return nil, fmt.Errorf("crew message %d: chain entry not found", row.SentSequence)
}

// renderCrewRow renders one row through prompt.RenderCrewMessages — the ONE
// exported form a crew message's text may leave the prompt package in. This
// handler never assembles an envelope of its own.
func (s *Server) renderCrewRow(ctx context.Context, row crewmessage.Row) (string, error) {
	msg, err := s.crewSentDocument(ctx, row)
	if err != nil {
		return "", err
	}
	return prompt.RenderCrewMessages([]prompt.CrewMessage{crewMessageForPrompt(msg)}), nil
}

// crewMessageForPrompt flattens a parsed message onto the prompt's input
// shape. Every payload field is untrusted prose and goes into MessageText,
// which the renderer quarantines; the rest is contract-closed metadata.
func crewMessageForPrompt(msg *crewmessage.Message) prompt.CrewMessage {
	anchor := ""
	switch {
	case msg.Anchor.RunID != "":
		anchor = "run_id " + msg.Anchor.RunID
	case msg.Anchor.IssueRef != "":
		anchor = "issue_ref " + msg.Anchor.IssueRef
	case msg.Anchor.DecisionRecordID != "":
		anchor = "decision_record_id " + msg.Anchor.DecisionRecordID
	}
	p := msg.Payload
	var b strings.Builder
	for _, f := range []struct{ name, value string }{
		{"question", p.Question}, {"what_i_can_infer", p.WhatICanInfer}, {"context", p.Context},
		{"title", p.Title}, {"summary", p.Summary}, {"severity", p.Severity}, {"detail", p.Detail},
		{"rationale", p.Rationale}, {"recommended_default", p.RecommendedDefault}, {"tradeoffs", p.Tradeoffs},
	} {
		if f.value == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(f.name + ": " + f.value)
	}
	refs := make([]string, 0, len(msg.Evidence))
	for _, e := range msg.Evidence {
		refs = append(refs, string(e.Kind)+":"+e.Ref)
	}
	return prompt.CrewMessage{
		Type:         string(msg.Type),
		SenderRole:   string(msg.SenderRole),
		AnchorRef:    anchor,
		MessageText:  b.String(),
		EvidenceRefs: refs,
	}
}

// handleListCrewMessages implements GET /v0/crew-messages. Exactly one filter
// family: recipient (?recipient_role, ?state, ?limit) or anchor (?run_id |
// ?issue_ref | ?decision_record_id). A run-bound token may list only its own
// run's anchor; every returned row still passes authorizeCrewRead's account
// predicate for an operator.
func (s *Server) handleListCrewMessages(w http.ResponseWriter, r *http.Request) {
	if !s.crewConfigured(w, r) {
		return
	}
	id, ok := s.crewAuthenticated(w, r)
	if !ok {
		return
	}
	tokenRunID, runBound := runBoundTokenRunID(id)
	if runBound {
		if !s.requireCrewMessagesScope(w, r, id) {
			return
		}
	} else if !s.requireWriteScope(w, r, "read:audit") {
		return
	}

	q := r.URL.Query()
	recipientFamily := q.Has("recipient_role") || q.Has("state") || q.Has("limit")
	anchorFamily := q.Has("run_id") || q.Has("issue_ref") || q.Has("decision_record_id")
	if recipientFamily && anchorFamily {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"name EITHER a recipient filter (recipient_role, state, limit) OR an anchor filter (run_id, issue_ref, decision_record_id), not both", nil)
		return
	}

	ctx := r.Context()
	store := s.cfg.CrewMailbox.Store()
	var (
		rows []crewmessage.Row
		err  error
	)
	if anchorFamily {
		f, ok := s.parseCrewAnchorFilter(w, r)
		if !ok {
			return
		}
		if runBound && (f.RunID == nil || *f.RunID != tokenRunID) {
			anchor := ""
			if f.RunID != nil {
				anchor = f.RunID.String()
			}
			s.writeCrossRunCrewMessage(w, r, tokenRunID, anchor)
			return
		}
		rows, err = store.ListByAnchor(ctx, f)
	} else {
		if runBound {
			// A recipient listing spans every anchor, so it is operator-only.
			s.writeCrossRunCrewMessage(w, r, tokenRunID, "")
			return
		}
		f := crewmessage.ListFilter{
			RecipientRole: crewmessage.Role(q.Get("recipient_role")),
			State:         crewmessage.State(q.Get("state")),
		}
		if raw := q.Get("limit"); raw != "" {
			n, perr := strconv.Atoi(raw)
			if perr != nil || n <= 0 || n > crewMessageListLimitMax {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					fmt.Sprintf("limit must be an integer in 1..%d", crewMessageListLimitMax),
					map[string]any{"field": "limit", "got": raw})
				return
			}
			f.Limit = n
		}
		rows, err = store.ListByRecipient(ctx, f)
	}
	if err != nil {
		s.writeCrewMessageError(w, r, err)
		return
	}
	acct := identityAccountID(ctx)
	out := crewMessageListResponse{Items: make([]crewMessageResponse, 0, len(rows))}
	for _, row := range rows {
		if !runBound && !crewAccountVisible(acct, row.AccountID) {
			continue
		}
		out.Items = append(out.Items, crewRowToResponse(row))
	}
	s.writeJSON(w, r, http.StatusOK, out)
}

// parseCrewAnchorFilter reads exactly one anchor query parameter.
func (s *Server) parseCrewAnchorFilter(w http.ResponseWriter, r *http.Request) (crewmessage.AnchorFilter, bool) {
	q := r.URL.Query()
	named := 0
	for _, k := range []string{"run_id", "issue_ref", "decision_record_id"} {
		if q.Get(k) != "" {
			named++
		}
	}
	if named != 1 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"name exactly one non-empty anchor filter: run_id, issue_ref or decision_record_id", nil)
		return crewmessage.AnchorFilter{}, false
	}
	var f crewmessage.AnchorFilter
	if raw := q.Get("run_id"); raw != "" {
		u, err := uuid.Parse(raw)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"run_id must be a valid UUID", map[string]any{"field": "run_id", "got": raw})
			return f, false
		}
		f.RunID = &u
	}
	f.IssueRef = q.Get("issue_ref")
	f.DecisionRecordID = q.Get("decision_record_id")
	return f, true
}

// handleRespondCrewMessage implements POST /v0/crew-messages/{sequence}/respond
// as a REFUSAL LADDER (ADR-081 rule 4): only a fishhawkd-invoked responder
// answers a consult, and no HTTP caller is one. A run-bound token is refused
// first (agent_token_required), then every other identity
// (responder_required). The behaviour lives in RespondToCrewMessage, the
// in-process entry point E77.5's consult dispatcher (crew_consult.go) calls; if a later change admits an
// HTTP responder identity, the responder_required rung is the single place to
// widen.
func (s *Server) handleRespondCrewMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.crewAuthenticated(w, r)
	if !ok {
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "agent_token_required",
			"a stage agent cannot answer a crew message; only a fishhawkd-invoked responder can", nil)
		return
	}
	s.writeError(w, r, http.StatusForbidden, "responder_required",
		"crew messages are answered in-process by a fishhawkd-invoked responder, never over HTTP", nil)
}

// RespondParams is one in-process response to an open crew message.
type RespondParams struct {
	// SentSequence is the message being answered.
	SentSequence int64
	// Reply is the reply's crew-message-v1 document; its sender_role is
	// derived from ResponderRole exactly as the HTTP send derives it.
	Reply         []byte
	ResponderRole crewmessage.Role
	Actor         crewmessage.Actor
}

// RespondToCrewMessage is the in-process responder entry point
// (runCrewConsult in crew_consult.go calls it, E77.5 #3739): it sends the reply threaded under the answered message's thread
// root, then disposes the answered message as accepted. The two chain
// entries are sequential, not one transaction: a failure between them leaves
// a recorded reply on a still-open message, which a retried Dispose closes.
//
// An ESCALATION is refused (ErrInvalidDisposition): only the captain's
// escalation-decision endpoint may terminally dispose one, because that
// terminal state is what makes a ruling binding prompt text (E77.6 / #3740).
func (s *Server) RespondToCrewMessage(ctx context.Context, p RespondParams) (reply, answered *crewmessage.Row, err error) {
	if s.cfg.CrewMailbox == nil {
		return nil, nil, errors.New("crew mailbox not configured")
	}
	row, err := s.cfg.CrewMailbox.Store().Get(ctx, p.SentSequence)
	if err != nil {
		return nil, nil, err
	}
	// E77.6 (#3740): an ESCALATION is never answered here. This entry point
	// disposes the answered message `accepted`, and
	// resolveDecidedCrewEscalations (prompt.go) promotes an accepted or
	// rejected escalation ROOT into TRUSTED binding plan text — so admitting an
	// escalation would let an in-process responder manufacture a binding
	// "ruling" (with a non-captain reason, or none) the captain never made.
	// handleDecideCrewEscalation is the ONLY path that may terminally dispose
	// an escalation. Today the sole caller is the consult dispatcher, which
	// already gates on type `consult` (crew_consult.go); this refusal keeps
	// that true for the next caller rather than leaving it a property of one
	// call site.
	if row.MessageType == crewmessage.TypeEscalation {
		return nil, nil, fmt.Errorf(
			"crew message %d: an escalation is ruled on by the captain's escalation decision, never answered in-process: %w",
			p.SentSequence, crewmessage.ErrInvalidDisposition)
	}
	if row.State != crewmessage.StateOpen {
		return nil, nil, fmt.Errorf("crew message %d: %w", p.SentSequence, crewmessage.ErrAlreadyDisposed)
	}
	doc, err := crewmessage.WithSenderRole(p.Reply, p.ResponderRole)
	if err != nil {
		return nil, nil, err
	}
	root := row.ThreadRootSequence
	reply, err = s.cfg.CrewMailbox.Send(ctx, crewmessage.SendParams{
		RawMessage: doc, Actor: p.Actor, AccountID: row.AccountID, ThreadRootSequence: &root,
	})
	var projErr *crewmessage.ProjectionError
	if err != nil && !errors.As(err, &projErr) {
		return nil, nil, err
	}
	// E77.6 (#3740): the SAME send-time seam the HTTP handler uses. A responder's
	// reply is always THREADED (root is non-nil just above), so this notifies
	// nothing — which is the point: the discrimination lives in one function
	// rather than in two call sites hoping to agree.
	if replyMsg, perr := crewmessage.Parse(doc); perr == nil {
		s.notifyRootEscalationPage(ctx, replyMsg, &root, reply)
	}
	answered, err = s.cfg.CrewMailbox.Dispose(ctx, crewmessage.DisposeParams{
		SentSequence: p.SentSequence, Disposition: crewmessage.DispositionAccepted, Actor: p.Actor,
	})
	if err != nil && !errors.As(err, &projErr) {
		return reply, nil, err
	}
	return reply, answered, nil
}

// handleDecideCrewEscalation implements POST
// /v0/crew-messages/{sequence}/escalation-decision: the captain's answer to an
// escalation. A run-bound token is refused self_decision and an identity
// without write:stages insufficient_scope, exactly as the other decision
// endpoints do; the message must be an escalation in the caller's account.
func (s *Server) handleDecideCrewEscalation(w http.ResponseWriter, r *http.Request) {
	if !s.crewConfigured(w, r) {
		return
	}
	id, ok := s.crewAuthenticated(w, r)
	if !ok {
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "self_decision",
			"a run-bound agent token cannot decide a crew escalation; the captain decides", nil)
		return
	}
	if !s.requireWriteScope(w, r, "write:stages") {
		return
	}
	seq, ok := s.parseCrewSequence(w, r)
	if !ok {
		return
	}
	body, ok := s.readCrewBody(w, r)
	if !ok {
		return
	}
	var req crewEscalationDecisionRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body must be valid JSON {decision, reason}", map[string]any{"error": err.Error()})
		return
	}
	disposition := crewmessage.Disposition(req.Decision)
	if disposition != crewmessage.DispositionAccepted && disposition != crewmessage.DispositionRejected {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"decision must be accepted or rejected", map[string]any{"field": "decision", "got": req.Decision})
		return
	}

	ctx := r.Context()
	row, err := s.cfg.CrewMailbox.Store().Get(ctx, seq)
	if err != nil {
		s.writeCrewMessageError(w, r, err)
		return
	}
	if !crewAccountVisible(identityAccountID(ctx), row.AccountID) {
		s.writeError(w, r, http.StatusNotFound, "crew_message_not_found",
			"no crew message at that sequence", nil)
		return
	}
	if row.MessageType != crewmessage.TypeEscalation {
		s.writeError(w, r, http.StatusUnprocessableEntity, "crew_message_not_escalation",
			"only an escalation takes an escalation decision",
			map[string]any{"message_type": string(row.MessageType)})
		return
	}

	disposed, err := s.cfg.CrewMailbox.Dispose(ctx, crewmessage.DisposeParams{
		SentSequence: seq,
		Disposition:  disposition,
		Reason:       req.Reason,
		Actor:        crewmessage.Actor{Kind: audit.ActorUser, Subject: id.Subject},
	})
	var projErr *crewmessage.ProjectionError
	if errors.As(err, &projErr) && disposed != nil {
		resp := crewRowToResponse(*disposed)
		resp.ProjectionDegraded = true
		s.writeJSON(w, r, http.StatusOK, resp)
		return
	}
	if err != nil {
		s.writeCrewMessageError(w, r, err)
		// E77.6 (#3740): the captain's rejection exhausted the thread's
		// reject-and-reply round bound. Mailbox.escalate appends the
		// crew_message_escalated entry INSIDE the transaction that then reports
		// exhaustion, so the notify follows a DURABLE row, and the refusal
		// written just above is unchanged — best-effort, after the
		// response-shaping decision, exactly like the other notify sites.
		if errors.Is(err, crewmessage.ErrRoundBoundExhausted) && row.RunID != nil {
			s.notifyOperatorVisible(ctx, *row.RunID, crewmessage.CategoryEscalated)
		}
		return
	}
	s.writeJSON(w, r, http.StatusOK, crewRowToResponse(*disposed))
}
