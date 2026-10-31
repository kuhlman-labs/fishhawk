package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
)

// CategoryConcernLabelled is the audit-log category the relabel handler writes
// when a HUMAN operator sets or corrects a concern's judgement, or marks it a
// duplicate of an earlier concern of its run (ADR-117 D1 phase 3, E94.8 /
// #4841). The entry is appended BEFORE the write and is the durable record of
// the relabel: the payload carries the PRIOR judgement and duplicate_of beside
// the new label, so a relabel is a new versioned judgement on the chain and
// never a silent rewrite. Append failure fails the request with no write.
// INTERNAL: not issue-comment activity, because a backfill import of many
// labels would flood the run's activity comment.
const CategoryConcernLabelled = "concern_labelled"

// CategoryConcernLabelFailed is the corrective audit-log category the relabel
// handler appends (warn-only) when the write fails AFTER the concern_labelled
// intent entry was durably recorded — a concurrent relabel, a duplicate_of
// target that vanished, or a store failure. It names the intended label and the
// error, so an intent entry without a write is always followed by this entry.
const CategoryConcernLabelFailed = "concern_label_failed"

// Error codes the relabel handler returns.
const (
	// errCodeLabelRequiresHuman refuses ANY agent subject (isAgentSubject: an
	// operator-agent/ token, a run-bound mcp:run: token, or a first-officer
	// seat). ADR-117 D1: a judgement is written ONLY by a human-initiated
	// verb, so there is no delegated path.
	errCodeLabelRequiresHuman = "label_requires_human"
	// errCodeConcernLabelConflict: the concern's judgement or duplicate_of
	// changed between the handler's read and the write (concern.ErrLabelConflict).
	errCodeConcernLabelConflict = "concern_label_conflict"
)

// labelConcernRequest is the JSON body of POST /v0/concerns/{concern_id}/label.
// Exactly one of Judgement and DuplicateOf is set: a judgement (with
// TradeoffCost only for a tradeoff), or the id of an EARLIER concern of the
// same run whose chain root's judgement this one inherits. Backfill records
// judgement_backfill and, on a WAIVED concern only, lets the stored waive
// reason's interim prefix supply the judgement.
type labelConcernRequest struct {
	Judgement    string `json:"judgement"`
	TradeoffCost string `json:"tradeoff_cost"`
	DuplicateOf  string `json:"duplicate_of"`
	Backfill     bool   `json:"backfill"`
}

// labelConcernResponse is the 200 body: the labelled concern's identity and
// state plus its judgement as every read surface renders it, so a duplicate's
// response shows the judgement it INHERITS (judgement_inherited_from).
type labelConcernResponse struct {
	ID        uuid.UUID `json:"id"`
	RunID     uuid.UUID `json:"run_id"`
	StageID   uuid.UUID `json:"stage_id"`
	StageKind string    `json:"stage_kind"`
	State     string    `json:"state"`
	concernJudgementView
}

// handleLabelConcern implements POST /v0/concerns/{concern_id}/label (ADR-117
// D1 phase 3, E94.8 / #4841): a HUMAN operator sets or corrects the judgement
// on ANY concern, in any state, or marks it duplicate_of an earlier concern of
// the SAME run. It never touches state, state_reason, outcome or updated_at.
//
// Every refusal below writes NOTHING (no audit row, no column):
//
//  1. 401 anonymous; 403 insufficient_scope without write:stages or
//     write:fixups (the waive pair); 403 label_requires_human for any agent
//     subject.
//  2. 503 concern_store_unconfigured.
//  3. 400 validation_failed for a concern_id, body or duplicate_of that does
//     not decode.
//  4. 400 duplicate_of_excludes_judgement when duplicate_of arrives with a
//     judgement or a tradeoff_cost.
//  5. 404 concern_not_found.
//  6. a duplicate label: the target is read and concern.CheckDuplicateTarget
//     applied — a MISSING target and a target of ANOTHER run are refused with
//     the IDENTICAL 400 duplicate_of_not_found whose message and details name
//     only the labelled concern's own run (approval condition M1), and a
//     target not strictly earlier in the run ledger is duplicate_of_not_earlier.
//     A judgement label: concernJudgementFromRequest, with the stored waive
//     reason as the prefix source ONLY when backfill is set AND the concern is
//     waived.
//
// Then the concern_labelled intent entry is appended FIRST (failure: 500
// audit_append_failed, no write), and only then Repository.ApplyLabel, guarded
// on the prior judgement and duplicate_of this handler read and audited. A
// write failure appends the corrective concern_label_failed entry and maps to
// 409 concern_label_conflict (a concurrent relabel), 400 with the store's
// duplicate_of rule (a target that vanished after the pre-check: the store's
// distinct sentinel, never the labelled concern's 404), 404 concern_not_found,
// or 500 internal_error.
//
// A human verb's judgement on a duplicate-linked concern (waive, resolve,
// defer) is recorded on that verb's audit row, but the EFFECTIVE judgement
// stays inherited from the chain root until this verb relabels the concern
// with an explicit judgement (approval condition M2, ADR-117 D1).
func (s *Server) handleLabelConcern(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") && !hasScope(id, "write:fixups") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages or write:fixups",
			map[string]any{"required_scope": "write:stages or write:fixups"})
		return
	}
	// Human-only (ADR-117 D1): a judgement is written only by a
	// human-initiated verb, so ANY agent subject is refused before the body is
	// read or any concern is touched.
	if isAgentSubject(id.Subject) {
		s.writeError(w, r, http.StatusForbidden, errCodeLabelRequiresHuman,
			"labelling a concern records a human judgement (ADR-117 D1); an agent token "+
				"(operator-agent/, a run-bound mcp:run: token or a first-officer seat) cannot label it. "+
				"A human operator must call this verb",
			map[string]any{"subject": id.Subject})
		return
	}

	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "concern_store_unconfigured",
			"label endpoint requires concern + audit repositories", nil)
		return
	}

	concernID, err := uuid.Parse(r.PathValue("concern_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concern_id must be a valid UUID",
			map[string]any{"field": "concern_id", "got": r.PathValue("concern_id")})
		return
	}
	var reqBody labelConcernRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {judgement, tradeoff_cost, duplicate_of, backfill}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	var duplicateOf *uuid.UUID
	if reqBody.DuplicateOf != "" {
		d, perr := uuid.Parse(reqBody.DuplicateOf)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"duplicate_of must be a valid UUID",
				map[string]any{"field": "duplicate_of", "got": reqBody.DuplicateOf})
			return
		}
		duplicateOf = &d
	}
	// Exclusivity, before any read or audit append: a duplicate inherits its
	// chain root's judgement, so it carries neither a judgement nor a cost.
	// Without this refusal the duplicate path below would record the link and
	// silently drop the judgement or cost the caller sent.
	if duplicateOf != nil && (reqBody.Judgement != "" || reqBody.TradeoffCost != "") {
		field, msg := "duplicate_of", "duplicate_of and judgement are exclusive: a duplicate inherits its chain root's judgement; send one or the other"
		if reqBody.Judgement == "" {
			field, msg = "tradeoff_cost", "duplicate_of and tradeoff_cost are exclusive: a duplicate inherits its chain root's judgement and records no cost of its own"
		}
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", msg,
			map[string]any{"field": field, "rule": concern.JudgementRuleDuplicateOfExcludesJudgement})
		return
	}

	rows, err := s.cfg.ConcernRepo.GetByIDs(r.Context(), []uuid.UUID{concernID})
	if err != nil {
		if errors.Is(err, concern.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "concern_not_found",
				"no concern with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get concern failed", map[string]any{"error": err.Error()})
		return
	}
	row := rows[0]
	// Snapshot what this handler read: the intent row audits it as the prior
	// label and ApplyLabel's optimistic guard compares against it, so a
	// concurrent relabel landing after this read is a conflict, never absorbed.
	priorJudgement := row.Judgement
	var priorDuplicateOf *uuid.UUID
	if row.DuplicateOf != nil {
		d := *row.DuplicateOf
		priorDuplicateOf = &d
	}

	var judgement concern.Judgement
	var tradeoffCost string
	if duplicateOf != nil {
		var target *concern.Concern
		targets, terr := s.cfg.ConcernRepo.GetByIDs(r.Context(), []uuid.UUID{*duplicateOf})
		switch {
		case errors.Is(terr, concern.ErrNotFound):
			// A missing target is refused EXACTLY like a foreign one, by
			// CheckDuplicateTarget (M1).
		case terr != nil:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"get duplicate_of concern failed", map[string]any{"error": terr.Error()})
			return
		default:
			target = targets[0]
		}
		if cerr := concern.CheckDuplicateTarget(row, target); cerr != nil {
			s.writeLabelJudgementError(w, r, row, cerr)
			return
		}
	} else {
		// Backfill prefix derivation reads the stored reason ONLY for a waived
		// row: the interim prefixes are a waive-reason convention, and a
		// resolved or deferred row's state_reason is server-composed.
		prefixSource := ""
		if reqBody.Backfill && row.State == concern.StateWaived {
			prefixSource = row.StateReason
		}
		var ok bool
		judgement, tradeoffCost, ok = s.concernJudgementFromRequest(w, r, reqBody.Judgement, reqBody.TradeoffCost, prefixSource)
		if !ok {
			return
		}
	}

	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)
	intended := map[string]any{
		"concern_id":      row.ID.String(),
		"stage_kind":      row.StageKind,
		"severity":        row.Severity,
		"category":        row.Category,
		"state":           string(row.State),
		"prior_judgement": string(concern.NormalizeJudgement(string(priorJudgement))),
		"backfill":        reqBody.Backfill,
	}
	if priorDuplicateOf != nil {
		intended["prior_duplicate_of"] = priorDuplicateOf.String()
	}
	if duplicateOf != nil {
		intended["duplicate_of"] = duplicateOf.String()
	} else {
		intended["judgement"] = string(judgement)
	}
	if tradeoffCost != "" {
		intended["tradeoff_cost"] = tradeoffCost
	}
	if row.Provenance != "" {
		intended["provenance"] = row.Provenance
	}
	payload, _ := json.Marshal(intended)
	if _, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        row.RunID,
		StageID:      &row.StageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryConcernLabelled,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); aerr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "audit_append_failed",
			"appending the concern_labelled audit entry failed; the label was NOT applied",
			map[string]any{"error": aerr.Error()})
		return
	}

	updated, err := s.cfg.ConcernRepo.ApplyLabel(r.Context(), row.ID, concern.Label{
		Judgement:        judgement,
		TradeoffCost:     tradeoffCost,
		DuplicateOf:      duplicateOf,
		ActorKind:        string(actorKind),
		Backfill:         reqBody.Backfill,
		PriorJudgement:   priorJudgement,
		PriorDuplicateOf: priorDuplicateOf,
	})
	if err != nil {
		// The intent entry is durable; keep the chain truthful (warn-only).
		s.writeConcernLabelFailedAudit(r.Context(), row, intended, err)
		var je concern.JudgementError
		switch {
		case errors.As(err, &je):
			s.writeLabelJudgementError(w, r, row, err)
		case errors.Is(err, concern.ErrLabelConflict):
			s.writeError(w, r, http.StatusConflict, errCodeConcernLabelConflict,
				"the concern's judgement or duplicate_of changed since it was read; the label was NOT applied. Re-read the concern and label it again",
				map[string]any{"concern_id": row.ID.String()})
		case errors.Is(err, concern.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "concern_not_found",
				"no concern with that id", nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"label concern failed", map[string]any{"error": err.Error()})
		}
		return
	}

	// Render the judgement as the read surfaces do, resolved over the run's
	// full ledger, so a duplicate shows what it inherits. A list failure
	// degrades to the row's own judgement: the label is already durable.
	var resolved map[uuid.UUID]concern.EffectiveJudgement
	if ledger, lerr := s.cfg.ConcernRepo.ListByRun(r.Context(), updated.RunID); lerr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"label: list run concerns for the judgement render failed; rendering the row's own judgement",
			slog.String("run_id", updated.RunID.String()),
			slog.String("concern_id", updated.ID.String()),
			slog.String("error", lerr.Error()))
	} else {
		resolved = concern.ResolveJudgements(ledger)
	}
	s.writeJSON(w, r, http.StatusOK, labelConcernResponse{
		ID:                   updated.ID,
		RunID:                updated.RunID,
		StageID:              updated.StageID,
		StageKind:            updated.StageKind,
		State:                string(updated.State),
		concernJudgementView: newConcernJudgementView(updated, resolved),
	})
}

// writeLabelJudgementError renders a refused label (a concern.JudgementError)
// as 400 validation_failed with details {field, rule, run_id}. run_id is the
// LABELLED concern's own run, never a duplicate_of target's (approval condition
// M1): the message is the error's own, which CheckDuplicateTarget computes from
// the labelled concern alone for duplicate_of_not_found.
func (s *Server) writeLabelJudgementError(w http.ResponseWriter, r *http.Request, row *concern.Concern, err error) {
	je := concern.JudgementError{Field: "duplicate_of", Message: err.Error()}
	errors.As(err, &je)
	s.writeError(w, r, http.StatusBadRequest, "validation_failed", je.Message,
		map[string]any{"field": je.Field, "rule": je.Rule, "run_id": row.RunID.String()})
}

// writeConcernLabelFailedAudit appends the corrective concern_label_failed
// entry after a write failure that followed a durably-recorded concern_labelled
// intent entry. Best-effort/warn-only: the response already tells the operator
// the label did not land; this entry keeps the chain from showing an intent
// without its outcome.
func (s *Server) writeConcernLabelFailedAudit(ctx context.Context, row *concern.Concern, intended map[string]any, cause error) {
	fields := map[string]any{
		"concern_id": row.ID.String(),
		"error":      cause.Error(),
	}
	for from, to := range map[string]string{
		"judgement": "intended_judgement", "duplicate_of": "intended_duplicate_of",
		"prior_judgement": "prior_judgement", "prior_duplicate_of": "prior_duplicate_of", "backfill": "backfill",
	} {
		if v, ok := intended[from]; ok {
			fields[to] = v
		}
	}
	payload, _ := json.Marshal(fields)
	systemKind := audit.ActorSystem
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     row.RunID,
		StageID:   &row.StageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryConcernLabelFailed,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"label: append corrective concern_label_failed entry failed",
			slog.String("run_id", row.RunID.String()),
			slog.String("concern_id", row.ID.String()),
			slog.String("error", aerr.Error()))
	}
}
