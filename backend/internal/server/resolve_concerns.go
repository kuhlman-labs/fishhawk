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
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
)

// CategoryConcernResolvedWithEvidence is the audit-log category the resolve
// handler writes when a HUMAN operator resolves a routed (addressed_pending)
// concern as `addressed` on the strength of their own evidence (E83.53 /
// #4086). It is the counterpart to the permanent operator_evidence_routed veto:
// a fix-up pass carrying operator_evidence makes every concern it routes immune
// to reviewer-confirmation auto-resolve, so before this verb the only way to
// clear such a concern was a waiver, which misrecords a resolved concern as a
// non-blocking one. The entry is appended BEFORE the state transition and is
// the durable record of the resolution: the payload carries the concern's
// stable ID, its prior state and the REQUIRED evidence note. Append failure
// fails the item — a resolution can never exist without this record.
const CategoryConcernResolvedWithEvidence = "concern_resolved_with_evidence"

// CategoryConcernResolveFailed is the corrective audit-log category the
// resolve handler appends (warn-only) when the state transition fails AFTER
// the concern_resolved_with_evidence intent entry was durably recorded — e.g.
// a concurrent transition raced the resolve. It names the actual state, so an
// intent entry without a mutation is always followed by this entry.
const CategoryConcernResolveFailed = "concern_resolve_failed"

// Error codes the resolve handler returns.
const (
	// errCodeResolveRequiresHuman refuses ANY agent subject (isAgentSubject:
	// an operator-agent/ token or a run-bound mcp:run: token, even on its own
	// run). Operator evidence is a human authority claim — the whole point of
	// operator_evidence is that the operator, not a model, executed the
	// reproduction — so no agent can resolve on it, delegated or not.
	errCodeResolveRequiresHuman = "resolve_requires_human"
	// errCodeConcernResolveConflict refuses a concern that is not in
	// addressed_pending (pre-validation, 409) and names a per-item transition
	// failure in the apply loop.
	errCodeConcernResolveConflict = "concern_resolve_conflict"
)

// resolveConcernsMaxConcerns bounds one batch. It is the bulk waive's cap
// (bulkWaiveMaxConcerns) on purpose: the two verbs settle the same ledger at
// the same gate, so one per-run batch size covers both.
const resolveConcernsMaxConcerns = bulkWaiveMaxConcerns

// resolveConcernsReasonPrefix prefixes the evidence on the concern's
// state_reason, so a reader of the settled ledger can tell an operator-evidence
// resolution from a reviewer-confirmed `addressed` without joining the chain.
const resolveConcernsReasonPrefix = "operator evidence: "

// resolveConcernsRequest is the JSON body of
// POST /v0/runs/{run_id}/concerns/resolve. ONE evidence note covers the batch,
// like the bulk waive's single reason: the case this verb exists for is "the
// fix-up I routed with operator_evidence landed and I re-ran the reproduction".
type resolveConcernsRequest struct {
	ConcernIDs []string `json:"concern_ids"`
	Evidence   string   `json:"evidence"`
}

// resolveConcernsItemResult is one concern's outcome. State/StateReason carry
// the updated row on success; ErrorCode/Error name the failure otherwise
// (concern_resolve_conflict / audit_append_failed / internal_error).
type resolveConcernsItemResult struct {
	ConcernID   string `json:"concern_id"`
	Applied     bool   `json:"applied"`
	State       string `json:"state,omitempty"`
	StateReason string `json:"state_reason,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
	Error       string `json:"error,omitempty"`
}

// resolveConcernsResponse is the 200 body. Resolved counts Results with
// applied==true and Failed counts applied==false; Results is in REQUEST order.
type resolveConcernsResponse struct {
	RunID    string                      `json:"run_id"`
	Evidence string                      `json:"evidence"`
	Resolved int                         `json:"resolved"`
	Failed   int                         `json:"failed"`
	Results  []resolveConcernsItemResult `json:"results"`
}

// handleResolveConcerns implements POST /v0/runs/{run_id}/concerns/resolve
// (E83.53 / #4086): a HUMAN operator resolves a list of this run's routed
// (addressed_pending) concerns as `addressed`, with an audited evidence note.
//
// It is deliberately distinct from the waive: a waived concern says "this does
// not block", while a concern resolved here says "this was fixed, and I — the
// operator who executed the reproduction — confirm it". The concern lands in
// the terminal-for-now `addressed` state, the same state a reviewer
// confirmation produces, so a later review can still reopen it.
//
// Auth: authenticated required; write:stages OR write:fixups (the waive
// predicate); and ANY agent subject is refused with 403 resolve_requires_human
// (isAgentSubject — an operator-agent/ token or a run-bound mcp:run: token,
// even on its own run). There is no delegated path.
//
// Like the bulk waive the batch has two halves:
//
//   - PRE-VALIDATION is ALL-OR-NOTHING and mutates NOTHING: blank or
//     over-cap evidence, empty or over-cap list, non-UUID id, duplicate id
//     (compared on the PARSED uuid), unwired store (503), unknown id (404), an
//     id from another run (400 concern_run_mismatch), or an id not in
//     addressed_pending (409 concern_resolve_conflict naming its state and the
//     recovery).
//   - the APPLY loop is PER-ITEM: the concern_resolved_with_evidence intent
//     entry is appended FIRST (failure → audit_append_failed, no mutation),
//     then ApplyResolution(addressed). A transition failure appends the
//     corrective concern_resolve_failed entry and fails that ONE item.
//
// It deliberately does NOT refresh the issue-thread status comment: the
// activity-line rendering for concern_resolved_with_evidence lands with the
// gate-view slice (issuecomment activityCategories), so a refresh tagged with
// this category here would render nothing.
func (s *Server) handleResolveConcerns(w http.ResponseWriter, r *http.Request) {
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
	// Human-only: operator evidence is a human authority claim, so ANY agent
	// subject is refused before the body is read or any concern is touched.
	if isAgentSubject(id.Subject) {
		s.writeError(w, r, http.StatusForbidden, errCodeResolveRequiresHuman,
			"resolving a concern with operator evidence is a human operator's authority claim; "+
				"an agent token (operator-agent/ or a run-bound mcp:run: token) cannot resolve it. "+
				"A human operator must call this verb, or waive or route the concern instead",
			map[string]any{"subject": id.Subject})
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var reqBody resolveConcernsRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {concern_ids, evidence}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	if strings.TrimSpace(reqBody.Evidence) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"evidence is required: the operator's evidence note is recorded on every concern_resolved_with_evidence audit entry and stored as each concern's state_reason",
			map[string]any{"field": "evidence"})
		return
	}
	if len(reqBody.Evidence) > maxOperatorConcernBytes {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("evidence is %d bytes; the maximum is %d (it is not truncated)", len(reqBody.Evidence), maxOperatorConcernBytes),
			map[string]any{"field": "evidence", "bytes": len(reqBody.Evidence), "max": maxOperatorConcernBytes})
		return
	}
	if len(reqBody.ConcernIDs) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concern_ids must name at least one concern",
			map[string]any{"field": "concern_ids"})
		return
	}
	if len(reqBody.ConcernIDs) > resolveConcernsMaxConcerns {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concern_ids exceeds the per-batch cap; split the batch",
			map[string]any{"field": "concern_ids", "count": len(reqBody.ConcernIDs), "max": resolveConcernsMaxConcerns})
		return
	}

	// Parse FIRST, then dedupe on the PARSED uuid (the bulk waive's rule):
	// two spellings of one id would otherwise append two intent entries.
	seen := make(map[uuid.UUID]string, len(reqBody.ConcernIDs))
	parsed := make([]uuid.UUID, 0, len(reqBody.ConcernIDs))
	for _, raw := range reqBody.ConcernIDs {
		cid, perr := uuid.Parse(raw)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"concern_ids entry is not a valid UUID",
				map[string]any{"field": "concern_ids", "concern_id": raw})
			return
		}
		if first, dup := seen[cid]; dup {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"concern_ids contains a duplicate id",
				map[string]any{
					"field":                "concern_ids",
					"concern_id":           raw,
					"canonical_concern_id": cid.String(),
					"first_spelling":       first,
				})
			return
		}
		seen[cid] = raw
		parsed = append(parsed, cid)
	}

	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "concern_store_unconfigured",
			"resolve endpoint requires concern + audit repositories", nil)
		return
	}

	rows := make([]*concern.Concern, 0, len(parsed))
	for i, cid := range parsed {
		got, gerr := s.cfg.ConcernRepo.GetByIDs(r.Context(), []uuid.UUID{cid})
		if gerr != nil {
			if errors.Is(gerr, concern.ErrNotFound) {
				s.writeError(w, r, http.StatusNotFound, "concern_not_found",
					"no concern with that id",
					map[string]any{"concern_id": reqBody.ConcernIDs[i]})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"get concern failed",
				map[string]any{"concern_id": reqBody.ConcernIDs[i], "error": gerr.Error()})
			return
		}
		row := got[0]
		if row.RunID != runID {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"concern_ids references a concern from a different run",
				map[string]any{
					"field":          "concern_ids",
					"rule":           "concern_run_mismatch",
					"concern_id":     reqBody.ConcernIDs[i],
					"concern_run_id": row.RunID.String(),
					"path_run_id":    runID.String(),
				})
			return
		}
		if row.State != concern.StateAddressedPending {
			s.writeError(w, r, http.StatusConflict, errCodeConcernResolveConflict,
				fmt.Sprintf("concern %s is %s, not addressed_pending; %s. The whole batch was refused and nothing was resolved",
					reqBody.ConcernIDs[i], row.State, resolveConflictRecovery(row.State)),
				map[string]any{
					"concern_id": reqBody.ConcernIDs[i],
					"from":       string(row.State),
					"to":         string(concern.StateAddressed),
				})
			return
		}
		rows = append(rows, row)
	}

	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)

	resp := resolveConcernsResponse{
		RunID:    runID.String(),
		Evidence: reqBody.Evidence,
		Results:  make([]resolveConcernsItemResult, 0, len(rows)),
	}
	for _, row := range rows {
		item := resolveConcernsItemResult{ConcernID: row.ID.String()}
		updated, aerr := s.applyConcernResolve(r.Context(), row, reqBody.Evidence, subject, actorKind)
		if aerr != nil {
			item.Error = aerr.Error()
			var appendErr concernResolveAuditAppendError
			var bad concern.InvalidTransitionError
			switch {
			case errors.As(aerr, &appendErr):
				item.ErrorCode = "audit_append_failed"
			case errors.As(aerr, &bad):
				item.ErrorCode = errCodeConcernResolveConflict
			default:
				item.ErrorCode = "internal_error"
			}
			resp.Failed++
			resp.Results = append(resp.Results, item)
			continue
		}
		item.Applied = true
		item.State = string(updated.State)
		item.StateReason = updated.StateReason
		resp.Resolved++
		resp.Results = append(resp.Results, item)
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// resolveConflictRecovery names the recovery for a concern the resolve verb
// refused because it is not addressed_pending.
func resolveConflictRecovery(state concern.State) string {
	switch state {
	case concern.StateRaised, concern.StateReopened:
		return "route it with a fix-up (fishhawk_fixup_stage) first, or waive it"
	default:
		return "it is already closed, so there is nothing to resolve"
	}
}

// concernResolveAuditAppendError distinguishes the audit-APPEND failure from a
// transition failure in applyConcernResolve's single error return, exactly
// like concernWaiveAuditAppendError: the former leaves no intent on the chain
// (no mutation, no corrective entry), the latter does.
type concernResolveAuditAppendError struct{ err error }

func (e concernResolveAuditAppendError) Error() string { return e.err.Error() }
func (e concernResolveAuditAppendError) Unwrap() error { return e.err }

// applyConcernResolve is the durable-record-first resolve body:
//
//  1. append the concern_resolved_with_evidence intent entry FIRST. On failure
//     return concernResolveAuditAppendError with NO mutation and NO corrective
//     entry;
//  2. ApplyResolution to `addressed`. On failure append the corrective
//     concern_resolve_failed entry (warn-only) naming the actual state, then
//     return the transition error unwrapped.
//
// A non-empty row.Provenance is stamped as an additive provenance key, like
// the waive.
func (s *Server) applyConcernResolve(ctx context.Context, row *concern.Concern, evidence, subject string, actorKind audit.ActorKind) (*concern.Concern, error) {
	fields := map[string]any{
		"concern_id":  row.ID.String(),
		"prior_state": string(row.State),
		"evidence":    evidence,
		"stage_kind":  row.StageKind,
		"severity":    row.Severity,
		"category":    row.Category,
	}
	if row.Provenance != "" {
		fields["provenance"] = row.Provenance
	}
	payload, _ := json.Marshal(fields)
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        row.RunID,
		StageID:      &row.StageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryConcernResolvedWithEvidence,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); aerr != nil {
		return nil, concernResolveAuditAppendError{err: aerr}
	}

	updated, err := s.cfg.ConcernRepo.ApplyResolution(ctx, row.ID, concern.StateAddressed, resolveConcernsReasonPrefix+evidence)
	if err != nil {
		s.writeConcernResolveFailedAudit(ctx, row, err)
		return nil, err
	}
	return updated, nil
}

// writeConcernResolveFailedAudit appends the corrective concern_resolve_failed
// entry after a transition failure that followed a durably-recorded intent
// entry. Best-effort/warn-only: the per-item result already reports the
// failure; this entry keeps the chain from showing an intent without its
// outcome.
func (s *Server) writeConcernResolveFailedAudit(ctx context.Context, row *concern.Concern, cause error) {
	actual := string(row.State)
	var bad concern.InvalidTransitionError
	if errors.As(cause, &bad) {
		actual = string(bad.From)
	}
	payload, _ := json.Marshal(map[string]any{
		"concern_id":     row.ID.String(),
		"intended_state": string(concern.StateAddressed),
		"actual_state":   actual,
		"error":          cause.Error(),
	})
	systemKind := audit.ActorSystem
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     row.RunID,
		StageID:   &row.StageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryConcernResolveFailed,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"resolve: append corrective concern_resolve_failed entry failed",
			slog.String("run_id", row.RunID.String()),
			slog.String("concern_id", row.ID.String()),
			slog.String("error", aerr.Error()))
	}
}
