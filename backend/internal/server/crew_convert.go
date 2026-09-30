package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Finding -> concern conversion (E77.7 / #3741, ADR-081 #3727 D1 option 3).
//
//	POST /v0/crew-messages/{sequence}/convert-to-concern
//
// A crew `finding` is ADVISORY: it lives in crew_messages, never in
// review_concerns, so it is structurally absent from ConcernRepo.ListOpenByRun
// — the merge gate's open-concern source. This route is the captain's
// explicit promotion: after it, the EXISTING concern_ids fix-up routing makes
// the concern binding with no change. Long-form contract: this package's
// README ("Crew messages" -> "Finding conversion").

// CategoryCrewFindingConverted records one landed conversion: the finding's
// sent sequence, its disposition entry, and the concern it became.
const CategoryCrewFindingConverted = "crew_finding_converted"

// CategoryCrewFindingConvertFailed is the corrective entry appended when the
// finding was DISPOSED but the concern insert then failed (the
// concern_defer_failed shape): the chain records that the finding is closed
// yet no concern exists, so the operator can reconcile by hand.
const CategoryCrewFindingConvertFailed = "crew_finding_convert_failed"

// crewConvertedConcernCategory is the review_concerns.category every
// converted concern carries, so a read surface can tell a captain-promoted
// crew finding from a reviewer-raised concern.
const crewConvertedConcernCategory = "crew_finding"

// crewConvertDefaultSeverity is the concern severity when the finding named
// none (the schema's severity is optional).
const crewConvertDefaultSeverity = "medium"

// crewConvertRequest is the OPTIONAL body: a reason recorded on the
// disposition entry (never copied into the row).
type crewConvertRequest struct {
	Reason string `json:"reason"`
}

// crewConvertResponse is the 201 body: the disposed finding's metadata plus
// the concern it became.
type crewConvertResponse struct {
	CrewMessage crewMessageResponse  `json:"crew_message"`
	Concern     crewConvertedConcern `json:"concern"`
}

// crewConvertedConcern is the minted concern's identity and routing fields.
type crewConvertedConcern struct {
	ID                   uuid.UUID `json:"id"`
	RunID                uuid.UUID `json:"run_id"`
	StageID              uuid.UUID `json:"stage_id"`
	StageKind            string    `json:"stage_kind"`
	OriginReviewSequence int64     `json:"origin_review_sequence"`
	Severity             string    `json:"severity"`
	Category             string    `json:"category"`
	State                string    `json:"state"`
}

// crewFindingConvertedPayload is the crew_finding_converted payload.
type crewFindingConvertedPayload struct {
	SentSequence        int64  `json:"sent_sequence"`
	DispositionSequence *int64 `json:"disposition_sequence,omitempty"`
	ConcernID           string `json:"concern_id"`
	StageKind           string `json:"stage_kind"`
	Severity            string `json:"severity"`
}

// crewFindingConvertFailedPayload is the crew_finding_convert_failed payload.
type crewFindingConvertFailedPayload struct {
	SentSequence        int64  `json:"sent_sequence"`
	DispositionSequence *int64 `json:"disposition_sequence,omitempty"`
	ActualState         string `json:"actual_state"`
	StageKind           string `json:"stage_kind"`
	Error               string `json:"error"`
}

// crewConcernStage picks the stage a converted concern attaches to: the run's
// NEWEST (highest sequence) plan or implement stage — review_concerns'
// stage_kind CHECK admits exactly those two. ok is false when the run has
// neither.
func crewConcernStage(stages []*run.Stage) (*run.Stage, string, bool) {
	var best *run.Stage
	for _, st := range stages {
		if st == nil || (st.Type != run.StageTypePlan && st.Type != run.StageTypeImplement) {
			continue
		}
		if best == nil || st.Sequence > best.Sequence {
			best = st
		}
	}
	if best == nil {
		return nil, "", false
	}
	if best.Type == run.StageTypePlan {
		return best, concern.StageKindPlan, true
	}
	return best, concern.StageKindImplement, true
}

// crewConvertedNote is the converted concern's note: the finding's summary,
// quoted, and a citation of the crew message it came from. The captain's
// conversion is what adopts this text as a concern.
func crewConvertedNote(msg *crewmessage.Message, seq int64) string {
	summary := strings.TrimSpace(msg.Payload.Summary)
	if summary == "" {
		summary = "(the finding carried no summary)"
	}
	return "Converted from crew finding (" + string(msg.SenderRole) + "): \"" + summary +
		"\" [crew_message:" + strconv.FormatInt(seq, 10) + "]"
}

// handleConvertCrewFindingToConcern implements POST
// /v0/crew-messages/{sequence}/convert-to-concern. Order is load-bearing:
//
//  1. OPERATOR-ONLY: a run-bound token is refused self_decision BEFORE any
//     read (a crew role must not promote its own advice into a merge-gate
//     blocker); every other identity needs write:stages.
//  2. The row must be a run-anchored OPEN `finding` thread root in the
//     caller's account, the run must have a plan or implement stage, and the
//     finding's document must resolve — all checked before anything appends.
//  3. Dispose(accepted) FIRST: the mailbox's refuse-before-append
//     ErrAlreadyDisposed makes the conversion exactly-once under a race.
//  4. THEN InsertRaised. A failure here appends the corrective
//     crew_finding_convert_failed entry and answers 500 — never a success
//     entry for a conversion that did not land.
func (s *Server) handleConvertCrewFindingToConcern(w http.ResponseWriter, r *http.Request) {
	if !s.crewConfigured(w, r) {
		return
	}
	if s.cfg.ConcernRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "crew_message_unconfigured",
			"finding conversion requires the concern repository", nil)
		return
	}
	id, ok := s.crewAuthenticated(w, r)
	if !ok {
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "self_decision",
			"a run-bound agent token cannot convert a crew finding into a concern; the captain decides", nil)
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
	var req crewConvertRequest
	if len(strings.TrimSpace(string(body))) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be empty or valid JSON {reason}", map[string]any{"error": err.Error()})
			return
		}
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
	if reason := crewNotConvertibleReason(row); reason != "" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "crew_message_not_finding",
			"only a run-anchored finding thread root converts into a concern",
			map[string]any{"message_type": string(row.MessageType), "reason": reason})
		return
	}
	if row.State != crewmessage.StateOpen {
		s.writeError(w, r, http.StatusConflict, "crew_message_already_disposed",
			"the finding is already disposed", map[string]any{"state": string(row.State)})
		return
	}
	runID := *row.RunID
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not read the finding's run stages", nil)
		return
	}
	stage, stageKind, ok := crewConcernStage(stages)
	if !ok {
		s.writeError(w, r, http.StatusUnprocessableEntity, "crew_finding_no_concern_stage",
			"the finding's run has no plan or implement stage for a concern to attach to",
			map[string]any{"run_id": runID.String()})
		return
	}
	msg, err := s.crewSentDocument(ctx, row)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not resolve the finding's recorded document", nil)
		return
	}
	severity := msg.Payload.Severity
	if severity == "" {
		severity = crewConvertDefaultSeverity
	}

	// (3) Dispose FIRST — exactly-once.
	disposed, err := s.cfg.CrewMailbox.Dispose(ctx, crewmessage.DisposeParams{
		SentSequence: seq,
		Disposition:  crewmessage.DispositionAccepted,
		Reason:       req.Reason,
		Actor:        crewmessage.Actor{Kind: audit.ActorUser, Subject: id.Subject},
	})
	var projErr *crewmessage.ProjectionError
	projectionDegraded := errors.As(err, &projErr) && disposed != nil
	if err != nil && !projectionDegraded {
		s.writeCrewMessageError(w, r, err)
		return
	}

	// (4) THEN the concern.
	minted, err := s.cfg.ConcernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stage.ID,
		StageKind:            stageKind,
		OriginReviewSequence: seq,
		Concerns: []concern.RaisedConcern{{
			Severity: severity,
			Category: crewConvertedConcernCategory,
			Note:     crewConvertedNote(msg, seq),
		}},
	})
	if err == nil && len(minted) != 1 {
		err = errors.New("concern insert returned no row")
	}
	if err != nil {
		s.appendCrewConvertAudit(ctx, runID, stage.ID, CategoryCrewFindingConvertFailed, crewFindingConvertFailedPayload{
			SentSequence:        seq,
			DispositionSequence: disposed.DispositionSequence,
			ActualState:         string(disposed.State),
			StageKind:           stageKind,
			Error:               err.Error(),
		})
		s.writeError(w, r, http.StatusInternalServerError, "crew_finding_convert_failed",
			"the finding was disposed but the concern could not be recorded; see the crew_finding_convert_failed audit entry",
			map[string]any{"sent_sequence": seq})
		return
	}
	c := minted[0]
	s.appendCrewConvertAudit(ctx, runID, stage.ID, CategoryCrewFindingConverted, crewFindingConvertedPayload{
		SentSequence:        seq,
		DispositionSequence: disposed.DispositionSequence,
		ConcernID:           c.ID.String(),
		StageKind:           stageKind,
		Severity:            severity,
	})

	resp := crewConvertResponse{
		CrewMessage: crewRowToResponse(*disposed),
		Concern: crewConvertedConcern{
			ID: c.ID, RunID: c.RunID, StageID: c.StageID, StageKind: c.StageKind,
			OriginReviewSequence: c.OriginReviewSequence, Severity: c.Severity,
			Category: c.Category, State: string(c.State),
		},
	}
	resp.CrewMessage.ProjectionDegraded = projectionDegraded
	s.writeJSON(w, r, http.StatusCreated, resp)
}

// crewNotConvertibleReason names why row cannot convert, or "" when it can:
// it must be a `finding`, anchored on a run (a concern is a run row), and a
// thread ROOT (a reply is part of another message's exchange).
func crewNotConvertibleReason(row crewmessage.Row) string {
	switch {
	case row.MessageType != crewmessage.TypeFinding:
		return "not_a_finding"
	case row.RunID == nil:
		return "run_less_anchor"
	case row.ThreadRootSequence != row.SentSequence:
		return "not_a_thread_root"
	}
	return ""
}

// appendCrewConvertAudit appends one conversion fact on the run's chain.
// Best-effort/warn-only: the HTTP response already carries the outcome.
func (s *Server) appendCrewConvertAudit(ctx context.Context, runID, stageID uuid.UUID, category string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	kind := audit.ActorSystem
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &kind,
		Payload:   raw,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "crew: append conversion entry failed",
			slog.String("run_id", runID.String()),
			slog.String("category", category),
			slog.String("error", aerr.Error()))
	}
}
