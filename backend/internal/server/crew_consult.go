package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The synchronous consult round trip (E77.5 / #3739, ADR-081 #3727 D1 option
// 1). A plan stage POSTs a `consult` with response_required; fishhawkd
// resolves the recipient role through a CLOSED server-side responder
// registry, invokes the responder IN-PROCESS on a detached, deadline-bounded
// goroutine, and records the answer through RespondToCrewMessage. The sender
// long-polls GET /v0/crew-messages/{sequence}?wait= until the answer renders.
//
// Two bounds, both copied from the scope-amendment posture:
//
//   - maxCrewConsultsPerStage, checked BEFORE the chain append;
//   - crewConsultWindow, after which the consult is disposed `expired` —
//     "no answer", never a stage failure. The stage row is never touched.
//
// Long-form contract: this package's README ("Crew messages" -> "Consult").

// maxCrewConsultsPerStage bounds the response_required consults one stage may
// send. It copies maxScopeAmendmentsPerStage's posture: counted on what the
// stage SENT (crew_message_sent chain entries stamped with the stage id, never
// the derived table), so an expired or unanswered consult still consumes
// budget — the cap bounds wall-clock interruptions, not answers. The check and
// the append are NOT serialized per run (neither are the scope-amendment
// cap's CountByStage and Create), so concurrent consults from one stage can
// each pass the check before either appends and exceed the cap by one per
// racer; the cap bounds interruptions, not safety.
const maxCrewConsultsPerStage = 2

// crewConsultWindowSeconds is the longest a consult is held open waiting for
// its responder, mirroring AmendmentPollWindowSeconds (900s, ~15 minutes). A
// sender may ask for LESS via the document's deadline, never more.
const crewConsultWindowSeconds = 900

// crewConsultWindow is crewConsultWindowSeconds as a duration. A package var
// only so tests can shorten it (the crewMessageWaitPollInterval idiom).
var crewConsultWindow = crewConsultWindowSeconds * time.Second

// crewConsultWriteTimeout bounds the chain writes that close a consult (the
// answer + accepted disposition, or the expired disposition). They run on a
// fresh detached context because the consult's own context is already done
// when the deadline branch fires. A package var so tests can shorten it.
var crewConsultWriteTimeout = 30 * time.Second

// crewConsultActor is the chain actor for everything the dispatcher records
// on a responder's behalf.
func crewConsultActor(role crewmessage.Role) crewmessage.Actor {
	return crewmessage.Actor{Kind: audit.ActorSystem, Subject: "fishhawkd:responder:" + string(role)}
}

// CrewConsultRequest is the PARSED consult a responder answers. Every text
// field is untrusted agent-authored prose; a model-backed responder must
// quarantine it exactly as the prompt package does.
type CrewConsultRequest struct {
	SentSequence  int64
	RunID         uuid.UUID
	StageID       uuid.UUID
	SenderRole    crewmessage.Role
	RecipientRole crewmessage.Role
	Anchor        crewmessage.Anchor
	Question      string
	WhatICanInfer string
	Context       string
	Evidence      []crewmessage.EvidenceReference
	Deadline      time.Time
}

// CrewConsultAnswer is a responder's answer. Summary is required (a blank one
// is treated as no answer and the consult expires); Detail and Evidence are
// optional. Plain data, so a DETERMINISTIC responder (E77.8's historian
// precedent query) needs no model call and no prompt.
type CrewConsultAnswer struct {
	Summary  string
	Detail   string
	Evidence []crewmessage.EvidenceReference
}

// CrewResponder answers one consult. Respond must honour ctx: the dispatcher
// stops waiting at the consult's deadline and disposes it expired whether or
// not Respond has returned.
type CrewResponder interface {
	Respond(ctx context.Context, req CrewConsultRequest) (CrewConsultAnswer, error)
}

// CrewResponderRegistry is the CLOSED role -> responder map. The zero value is
// the empty registry (every Lookup misses), which is the E77.5 production
// value: no responder is registered until E77.8 wires the historian.
type CrewResponderRegistry struct {
	responders map[crewmessage.Role]CrewResponder
}

// NewCrewResponderRegistry builds the registry, REFUSING construction when the
// map names crewmessage.RoleImplementer (ARCHITECTURE.md §6 invariant #8 — the
// second gate behind crewmessage.CanReceive), a role outside
// crewmessage.AllRoles, or a nil responder. A nil or empty map is legal and
// yields the empty registry. The map is copied, so a later caller mutation
// cannot widen it.
func NewCrewResponderRegistry(m map[crewmessage.Role]CrewResponder) (CrewResponderRegistry, error) {
	out := make(map[crewmessage.Role]CrewResponder, len(m))
	for role, r := range m {
		if role == crewmessage.RoleImplementer {
			return CrewResponderRegistry{}, fmt.Errorf("crew responder registry: role %q is executed by an implement stage and can never be consulted (ARCHITECTURE.md §6 invariant #8)", role)
		}
		if !isCrewRole(role) {
			return CrewResponderRegistry{}, fmt.Errorf("crew responder registry: %q is not a crew role", role)
		}
		if r == nil {
			return CrewResponderRegistry{}, fmt.Errorf("crew responder registry: role %q has a nil responder", role)
		}
		out[role] = r
	}
	return CrewResponderRegistry{responders: out}, nil
}

func isCrewRole(role crewmessage.Role) bool {
	for _, r := range crewmessage.AllRoles {
		if r == role {
			return true
		}
	}
	return false
}

// Lookup returns the responder registered for role, or false.
func (g CrewResponderRegistry) Lookup(role crewmessage.Role) (CrewResponder, bool) {
	r, ok := g.responders[role]
	return r, ok && r != nil
}

// crewConsultDeadline resolves a consult's effective deadline:
// min(document deadline when present and parseable, now + crewConsultWindow).
func crewConsultDeadline(msg *crewmessage.Message, now time.Time) time.Time {
	limit := now.Add(crewConsultWindow)
	if msg.Deadline == "" {
		return limit
	}
	// Parse already validated the format; RFC 3339 permits a lowercase t/z.
	d, err := time.Parse(time.RFC3339, strings.ToUpper(msg.Deadline))
	if err != nil || !d.Before(limit) {
		return limit
	}
	return d.UTC()
}

// resolveExecutingStage returns the run's executing stage row — the same
// active-or-next fallback resolveExecutingStageType applies (#1030) — or nil.
// The consult branch needs the row, not just its type, for the per-stage cap.
func (s *Server) resolveExecutingStage(r *http.Request, runRow *run.Run) *run.Stage {
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runRow.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"list stages for crew sender role failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	return activeOrNextStage(stages)
}

// crewConsultBudgetUsed counts the response_required consult THREAD ROOTS the
// stage has already sent, read from the CHAIN (crew_message_sent entries
// stamped with stageID) — never from the derived table, so truncating
// crew_messages cannot reset the budget. A threaded reply is not a consult and
// never consumes the asker's budget.
func (s *Server) crewConsultBudgetUsed(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, crewmessage.CategorySent)
	if err != nil {
		return 0, fmt.Errorf("count crew consults: %w", err)
	}
	used := 0
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var p struct {
			Message struct {
				Type             crewmessage.MessageType `json:"type"`
				ResponseRequired bool                    `json:"response_required"`
			} `json:"message"`
			ThreadRootSequence *int64 `json:"thread_root_sequence"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.ThreadRootSequence == nil && p.Message.Type == crewmessage.TypeConsult && p.Message.ResponseRequired {
			used++
		}
	}
	return used, nil
}

// dispatchCrewConsult runs the responder on a DETACHED context bounded by
// req.Deadline (context.WithoutCancel keeps the request's values — identity,
// account — and drops its cancellation, so the consult outlives the sender's
// HTTP connection), tracked by s.bgReviews so Shutdown drains it (the #584
// detachment runPlanReviews' advisory path uses).
func (s *Server) dispatchCrewConsult(ctx context.Context, responder CrewResponder, req CrewConsultRequest) {
	dctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), req.Deadline)
	s.bgReviews.Add(1)
	go func() {
		defer s.bgReviews.Done()
		defer cancel()
		s.runCrewConsult(dctx, responder, req)
	}()
}

type crewConsultResult struct {
	answer CrewConsultAnswer
	err    error
}

// runCrewConsult invokes the responder and closes the consult exactly once:
// an answer is recorded through RespondToCrewMessage (threaded notice +
// accepted); a responder error, a blank answer, an answer that fails
// validation, or the deadline disposes it `expired` with a reason naming the
// cause. The STAGE is never touched: an expired consult is "no answer" and
// nothing else. Every non-answer path WARN-logs run id, sequence and role.
func (s *Server) runCrewConsult(ctx context.Context, responder CrewResponder, req CrewConsultRequest) {
	// The responder runs on its own goroutine so one that ignores ctx cannot
	// hold the consult open past its deadline.
	done := make(chan crewConsultResult, 1)
	go func() {
		a, err := responder.Respond(ctx, req)
		done <- crewConsultResult{answer: a, err: err}
	}()

	var res crewConsultResult
	select {
	case res = <-done:
	case <-ctx.Done():
		s.expireCrewConsult(ctx, req, "the "+string(req.RecipientRole)+" responder did not answer before the consult deadline")
		return
	}
	if res.err != nil {
		s.expireCrewConsult(ctx, req, "the "+string(req.RecipientRole)+" responder failed: "+res.err.Error())
		return
	}
	if strings.TrimSpace(res.answer.Summary) == "" {
		s.expireCrewConsult(ctx, req, "the "+string(req.RecipientRole)+" responder returned a blank answer")
		return
	}

	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), crewConsultWriteTimeout)
	defer wcancel()
	doc, err := json.Marshal(crewmessage.Message{
		SchemaVersion: crewmessage.SchemaVersion,
		Type:          crewmessage.TypeNotice,
		SenderRole:    req.RecipientRole,
		RecipientRole: req.SenderRole,
		Anchor:        req.Anchor,
		Payload:       crewmessage.Payload{Summary: res.answer.Summary, Detail: res.answer.Detail},
		Evidence:      res.answer.Evidence,
	})
	if err != nil {
		s.expireCrewConsult(ctx, req, "the "+string(req.RecipientRole)+" responder's answer could not be encoded: "+err.Error())
		return
	}
	actor := crewConsultActor(req.RecipientRole)
	reply, _, err := s.RespondToCrewMessage(wctx, RespondParams{
		SentSequence: req.SentSequence, Reply: doc, ResponderRole: req.RecipientRole, Actor: actor,
	})
	if err == nil {
		return
	}
	if reply != nil {
		// The answer is recorded; only the accepted disposition failed. The
		// sender's wait already sees the threaded reply, so the consult is
		// answered — log it rather than contradict it with an expiry.
		s.cfg.Logger.LogAttrs(wctx, slog.LevelWarn, "crew consult answered but not disposed",
			slog.String("run_id", req.RunID.String()), slog.Int64("sent_sequence", req.SentSequence),
			slog.String("recipient_role", string(req.RecipientRole)), slog.String("error", err.Error()))
		return
	}
	s.expireCrewConsult(ctx, req, "the "+string(req.RecipientRole)+" responder's answer could not be recorded: "+err.Error())
}

// expireCrewConsult disposes the consult `expired` with reason, on a fresh
// detached context. A consult already closed (ErrAlreadyDisposed) is left as
// it is.
func (s *Server) expireCrewConsult(ctx context.Context, req CrewConsultRequest, reason string) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), crewConsultWriteTimeout)
	defer cancel()
	attrs := []slog.Attr{
		slog.String("run_id", req.RunID.String()), slog.Int64("sent_sequence", req.SentSequence),
		slog.String("recipient_role", string(req.RecipientRole)), slog.String("reason", reason),
	}
	s.cfg.Logger.LogAttrs(wctx, slog.LevelWarn, "crew consult expired without an answer", attrs...)
	_, err := s.cfg.CrewMailbox.Dispose(wctx, crewmessage.DisposeParams{
		SentSequence: req.SentSequence, Disposition: crewmessage.DispositionExpired,
		Reason: reason, Actor: crewConsultActor(req.RecipientRole),
	})
	var projErr *crewmessage.ProjectionError
	if err != nil && !errors.As(err, &projErr) && !errors.Is(err, crewmessage.ErrAlreadyDisposed) {
		s.cfg.Logger.LogAttrs(wctx, slog.LevelWarn, "crew consult expiry could not be recorded",
			append(attrs, slog.String("error", err.Error()))...)
	}
}
