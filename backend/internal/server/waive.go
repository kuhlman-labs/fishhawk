package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
)

// CategoryConcernWaived is the audit-log category for the entry the
// waive handler writes when an operator waives a review concern (E22.X /
// #984). The entry is appended BEFORE the state transition and is the
// durable record of the waive intent: the payload carries the concern's
// stable ID, its prior state, and the REQUIRED operator reason, so a
// waived concern's "why" survives in the chain even if the derived
// concern store is rebuilt. Append failure fails the request — a waive
// mutation can never exist without this record.
const CategoryConcernWaived = "concern_waived"

// CategoryConcernWaiveFailed is the corrective audit-log category the
// waive handler appends (warn-only) when the state transition fails
// AFTER the concern_waived intent entry was durably recorded — e.g. a
// concurrent transition raced the waive. It names the actual state the
// concern was found in, keeping the chain truthful in every
// interleaving: an intent entry without a mutation is always followed by
// this corrective entry.
const CategoryConcernWaiveFailed = "concern_waive_failed"

// waiveConcernRequest is the JSON body of
// POST /v0/concerns/{concern_id}/waive. Reason is REQUIRED: the waive is
// an operator judgment ("this concern does not block") and the rationale
// is recorded on the concern_waived audit entry and as the concern's
// state_reason — a reviewer seeing the waived concern in a later
// re-review prompt reads exactly this text.
type waiveConcernRequest struct {
	Reason string `json:"reason"`
	// Delegated opts the waive into the ADR-040 delegated-action path
	// (#1026): checkDelegation re-evaluates the operator_agent may_waive
	// condition (solo_low) server-side at action time — 403
	// delegation_not_configured / delegation_condition_unmet on refusal,
	// `delegated: "<rule>"` on the concern_waived payload when met.
	// Absent → behavior byte-identical to today.
	Delegated bool `json:"delegated"`
}

// waiveConcernResponse is the 200 body: the updated concern row.
type waiveConcernResponse struct {
	ID          uuid.UUID `json:"id"`
	RunID       uuid.UUID `json:"run_id"`
	StageID     uuid.UUID `json:"stage_id"`
	StageKind   string    `json:"stage_kind"`
	Severity    string    `json:"severity"`
	Category    string    `json:"category"`
	Note        string    `json:"note"`
	State       string    `json:"state"`
	StateReason string    `json:"state_reason"`
}

// handleWaiveConcern implements POST /v0/concerns/{concern_id}/waive
// (E22.X / #984): the operator verb that transitions any OPEN concern
// (raised, addressed_pending, reopened) to the terminal waived state
// with a required, audited reason. A waived concern stops appearing in
// the run-status open-concerns block and is rendered in later re-review
// prompts as context that must NOT be re-litigated.
//
// Auth mirrors the fix-up handler exactly: the same write:stages /
// write:fixups scope pair, and the same mcp:run:<uuid> subject-binding
// guard (a run-bound token may waive only its own run's concerns). The
// concern is resolved AFTER the scope check so an unscoped token cannot
// probe concern IDs.
//
// Ordering invariant (the audited-waiver contract): the concern_waived
// audit entry is appended FIRST and append failure fails the request
// (500 audit_append_failed, no mutation) — only then does the state
// transition run. If the transition fails after the append (a concurrent
// transition raced it), a corrective concern_waive_failed entry is
// appended (warn-only) naming the actual state, and the request returns
// 422. A mutation can therefore never exist without a durable audit
// record, in every interleaving.
func (s *Server) handleWaiveConcern(w http.ResponseWriter, r *http.Request) {
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

	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "concern_store_unconfigured",
			"waive endpoint requires concern + audit repositories", nil)
		return
	}

	concernID, err := uuid.Parse(r.PathValue("concern_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concern_id must be a valid UUID",
			map[string]any{"field": "concern_id", "got": r.PathValue("concern_id")})
		return
	}

	var reqBody waiveConcernRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {reason}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	if strings.TrimSpace(reqBody.Reason) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: the waive rationale is recorded on the concern_waived audit entry and shown to later re-reviews",
			map[string]any{"field": "reason"})
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

	// Subject-binding guard: an MCP run-bound token may only waive
	// concerns within its own run. Mirrors the fix-up handler.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		runIDStr := strings.TrimPrefix(id.Subject, "mcp:run:")
		subjectRunID, parseErr := uuid.Parse(runIDStr)
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != row.RunID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_waive",
				"mcp token may only waive concerns within its own run",
				map[string]any{
					"token_run_id":   subjectRunID.String(),
					"concern_run_id": row.RunID.String(),
				})
			return
		}
	}

	// Delegated-action enforcement (ADR-040 / #1026): a delegated:true
	// waive must hold the may_waive condition (solo_low) against CURRENT
	// run state, re-evaluated server-side before the intent entry is
	// appended.
	var delegatedRule string
	if reqBody.Delegated {
		rule, ok := s.checkDelegation(w, r, row.RunID, delegation.ActionWaive)
		if !ok {
			return
		}
		delegatedRule = rule
	}

	// Durable-record-first: append the concern_waived intent entry BEFORE
	// the state mutation. Append failure is request failure — no
	// mutation may occur without the audit record. The ordering itself lives
	// in applyConcernWaive, SHARED with the bulk verb so the two paths cannot
	// drift (E64.77 / #3318).
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	updated, err := s.applyConcernWaive(r.Context(), row, reqBody.Reason, subject,
		actorKindForSubject(subject), delegatedRule, false)
	if err != nil {
		var appendErr concernWaiveAuditAppendError
		if errors.As(err, &appendErr) {
			s.writeError(w, r, http.StatusInternalServerError, "audit_append_failed",
				"appending the concern_waived audit entry failed; the waive was NOT applied",
				map[string]any{"error": appendErr.Unwrap().Error()})
			return
		}
		var bad concern.InvalidTransitionError
		if errors.As(err, &bad) {
			s.writeError(w, r, http.StatusUnprocessableEntity, "concern_waive_conflict",
				err.Error(),
				map[string]any{"from": string(bad.From), "to": string(bad.To)})
			return
		}
		if errors.Is(err, concern.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "concern_not_found",
				"no concern with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"waive concern failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusOK, waiveConcernResponse{
		ID:          updated.ID,
		RunID:       updated.RunID,
		StageID:     updated.StageID,
		StageKind:   updated.StageKind,
		Severity:    updated.Severity,
		Category:    updated.Category,
		Note:        updated.Note,
		State:       string(updated.State),
		StateReason: updated.StateReason,
	})
}

// writeConcernWaiveFailedAudit appends the corrective concern_waive_failed
// entry after a transition failure that followed a durably-recorded
// concern_waived intent entry. Best-effort/warn-only: the 4xx/5xx response
// already tells the operator the waive did not land; this entry exists so
// the audit chain never shows an intent without its outcome.
func (s *Server) writeConcernWaiveFailedAudit(ctx context.Context, row *concern.Concern, cause error) {
	actual := string(row.State)
	var bad concern.InvalidTransitionError
	if errors.As(cause, &bad) {
		actual = string(bad.From)
	}
	payload, _ := json.Marshal(map[string]any{
		"concern_id":     row.ID.String(),
		"intended_state": string(concern.StateWaived),
		"actual_state":   actual,
		"error":          cause.Error(),
	})
	systemKind := audit.ActorSystem
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     row.RunID,
		StageID:   &row.StageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryConcernWaiveFailed,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"waive: append corrective concern_waive_failed entry failed",
			slog.String("run_id", row.RunID.String()),
			slog.String("concern_id", row.ID.String()),
			slog.String("error", aerr.Error()))
	}
}

// concernWaiveAuditAppendError distinguishes the audit-APPEND failure from a
// transition failure in applyConcernWaive's single error return. The two map to
// different operator-facing outcomes — 500 audit_append_failed with NO mutation
// and no corrective entry, versus a 422/404/500 that DID durably record intent
// and therefore also appended the corrective concern_waive_failed entry — so
// collapsing them into one opaque error would make the bulk path unable to
// report the same per-item error_code vocabulary the single path uses.
type concernWaiveAuditAppendError struct{ err error }

func (e concernWaiveAuditAppendError) Error() string { return e.err.Error() }
func (e concernWaiveAuditAppendError) Unwrap() error { return e.err }

// applyConcernWaive is the SHARED durable-record-first waive body behind both
// POST /v0/concerns/{concern_id}/waive and the bulk
// POST /v0/runs/{run_id}/concerns/waive (E64.77 / #3318). Extracting it is what
// makes the ordering invariant impossible to drift between the two verbs:
//
//  1. append the concern_waived intent entry FIRST. On failure return
//     concernWaiveAuditAppendError with NO mutation and NO corrective entry —
//     there is no intent on the chain to correct;
//  2. ApplyResolution to the terminal waived state. On failure append the
//     corrective concern_waive_failed entry (warn-only) naming the actual state,
//     then return the transition error unwrapped so the caller can map
//     InvalidTransitionError -> 422 and ErrNotFound -> 404.
//
// bulk stamps an ADDITIVE bulk_waive:true marker on the payload so an operator
// reading the chain can tell a batched waive from a single one. The single path
// passes bulk=false and its payload stays byte-identical to pre-#3318.
func (s *Server) applyConcernWaive(ctx context.Context, row *concern.Concern, reason, subject string, actorKind audit.ActorKind, delegatedRule string, bulk bool) (*concern.Concern, error) {
	waivedFields := map[string]any{
		"concern_id":  row.ID.String(),
		"prior_state": string(row.State),
		"reason":      reason,
		"stage_kind":  row.StageKind,
		"severity":    row.Severity,
		"category":    row.Category,
	}
	if delegatedRule != "" {
		waivedFields["delegated"] = delegatedRule
	}
	if bulk {
		waivedFields["bulk_waive"] = true
	}
	payload, _ := json.Marshal(waivedFields)
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        row.RunID,
		StageID:      &row.StageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryConcernWaived,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); aerr != nil {
		return nil, concernWaiveAuditAppendError{err: aerr}
	}

	updated, err := s.cfg.ConcernRepo.ApplyResolution(ctx, row.ID, concern.StateWaived, reason)
	if err != nil {
		// The intent entry is already durable; keep the chain truthful with a
		// corrective entry naming the actual outcome (warn-only).
		s.writeConcernWaiveFailedAudit(ctx, row, err)
		return nil, err
	}
	return updated, nil
}
