package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
)

// bulkWaiveMaxConcerns bounds one batch. It is an arbitrary but STATED cap
// chosen to bound the audit-append fan-out and the response body; the campaign
// that motivated E64.77 / #3318 waived roughly 90 concerns across many runs, so
// a per-RUN batch of 50 sits comfortably above the observed per-run need. It is
// a named 400 refusal, never a silent truncation: an operator who hits it
// splits the batch rather than losing concerns.
const bulkWaiveMaxConcerns = 50

// bulkWaiveRequest is the JSON body of POST /v0/runs/{run_id}/concerns/waive.
// ONE reason covers the whole batch — the merge-gate case this verb exists for
// is "these N concerns were all answered by the same approval condition", and
// forcing a per-concern rationale there produces N copies of one sentence.
type bulkWaiveRequest struct {
	ConcernIDs []string `json:"concern_ids"`
	Reason     string   `json:"reason"`
	// Delegated opts the batch into the ADR-040 delegated-action path, exactly
	// like the single waive's field. It is evaluated ONCE for the run before any
	// intent entry is appended — every concern in the batch shares the run, so a
	// per-item re-evaluation would read the same state N times.
	Delegated bool `json:"delegated"`
}

// bulkWaiveItemResult is one concern's outcome. State/StateReason carry the
// updated row on success; ErrorCode/Error name the failure otherwise, reusing
// the SINGLE path's vocabulary (concern_waive_conflict / audit_append_failed /
// internal_error) so an operator reads one code set across both verbs.
type bulkWaiveItemResult struct {
	ConcernID   string `json:"concern_id"`
	Applied     bool   `json:"applied"`
	State       string `json:"state,omitempty"`
	StateReason string `json:"state_reason,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
	Error       string `json:"error,omitempty"`
}

// bulkWaiveResponse is the 200 body. Waived counts Results with applied==true
// and Failed counts applied==false, so a three-item batch whose SECOND item
// fails reports waived=2 / failed=1 with Results carrying
// [applied=true, applied=false, applied=true] in REQUEST order.
type bulkWaiveResponse struct {
	RunID   string                `json:"run_id"`
	Reason  string                `json:"reason"`
	Waived  int                   `json:"waived"`
	Failed  int                   `json:"failed"`
	Results []bulkWaiveItemResult `json:"results"`
}

// handleBulkWaiveConcerns implements POST /v0/runs/{run_id}/concerns/waive
// (E64.77 / #3318): waive a LIST of this run's open concerns with ONE audited
// reason, recording one concern_waived audit row per concern. It is the
// merge-gate toil closer for the case a campaign surfaced — a run reaching its
// gate with a dozen open concerns the operator has already judged non-blocking,
// each today costing a separate fishhawk_waive_concern round trip.
//
// Auth mirrors handleWaiveConcern exactly: authenticated required; write:stages
// OR write:fixups; and the mcp:run: subject-binding guard, compared here against
// the PATH run id so a run-bound token can only bulk-waive its OWN run.
//
// The batch splits into two halves with deliberately different atomicity:
//
//   - PRE-VALIDATION is ALL-OR-NOTHING and mutates NOTHING. The first violation
//     wins and names the offending id: blank reason, empty list, over-cap,
//     duplicate id, non-UUID id, unwired store, unknown id, an id belonging to
//     ANOTHER run, or an id not in an OPEN state.
//   - the APPLY loop is PER-ITEM. The batch is deliberately NOT a database
//     transaction: each concern carries its own audit row, and wrapping N chained
//     audit appends plus N transitions in one transaction would either serialize
//     the chain append or leave the chain and the concern store inconsistent on
//     rollback. So a concurrent transition that raced the pre-validation fails
//     ONE item and the rest still apply — reported honestly as per-item results
//     plus waived/failed counts rather than a single misleading status.
func (s *Server) handleBulkWaiveConcerns(w http.ResponseWriter, r *http.Request) {
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

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	// Subject-binding guard: an MCP run-bound token may only bulk-waive within
	// its own run. Compared against the PATH run id — the run-scoped route is
	// what makes the same-run invariant structural here, so this check needs no
	// concern read to be decided.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		subjectRunID, parseErr := uuid.Parse(strings.TrimPrefix(id.Subject, "mcp:run:"))
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_waive",
				"mcp token may only waive concerns within its own run",
				map[string]any{
					"token_run_id": subjectRunID.String(),
					"path_run_id":  runID.String(),
				})
			return
		}
	}

	var reqBody bulkWaiveRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {concern_ids, reason}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	if strings.TrimSpace(reqBody.Reason) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: the waive rationale is recorded on every concern_waived audit entry in the batch and shown to later re-reviews",
			map[string]any{"field": "reason"})
		return
	}
	if len(reqBody.ConcernIDs) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concern_ids must name at least one concern",
			map[string]any{"field": "concern_ids"})
		return
	}
	if len(reqBody.ConcernIDs) > bulkWaiveMaxConcerns {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concern_ids exceeds the per-batch cap; split the batch",
			map[string]any{"field": "concern_ids", "count": len(reqBody.ConcernIDs), "max": bulkWaiveMaxConcerns})
		return
	}

	seen := make(map[string]struct{}, len(reqBody.ConcernIDs))
	parsed := make([]uuid.UUID, 0, len(reqBody.ConcernIDs))
	for _, raw := range reqBody.ConcernIDs {
		if _, dup := seen[raw]; dup {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"concern_ids contains a duplicate id",
				map[string]any{"field": "concern_ids", "concern_id": raw})
			return
		}
		seen[raw] = struct{}{}
		cid, perr := uuid.Parse(raw)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"concern_ids entry is not a valid UUID",
				map[string]any{"field": "concern_ids", "concern_id": raw})
			return
		}
		parsed = append(parsed, cid)
	}

	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "concern_store_unconfigured",
			"bulk waive endpoint requires concern + audit repositories", nil)
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
		if !row.State.IsOpen() {
			s.writeError(w, r, http.StatusUnprocessableEntity, "concern_waive_conflict",
				"concern_ids references a concern that is not in an open state; the whole batch was refused and nothing was waived",
				map[string]any{
					"concern_id": reqBody.ConcernIDs[i],
					"from":       string(row.State),
					"to":         string(concern.StateWaived),
				})
			return
		}
		rows = append(rows, row)
	}

	// Delegated-action enforcement (ADR-040 / #1026), evaluated ONCE for the run
	// BEFORE any intent entry is appended: every batched concern shares the run,
	// so the may_waive condition resolves identically for all of them.
	var delegatedRule string
	if reqBody.Delegated {
		rule, ok := s.checkDelegation(w, r, runID, delegation.ActionWaive)
		if !ok {
			return
		}
		delegatedRule = rule
	}

	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)

	resp := bulkWaiveResponse{
		RunID:   runID.String(),
		Reason:  reqBody.Reason,
		Results: make([]bulkWaiveItemResult, 0, len(rows)),
	}
	for _, row := range rows {
		item := bulkWaiveItemResult{ConcernID: row.ID.String()}
		updated, aerr := s.applyConcernWaive(r.Context(), row, reqBody.Reason, subject, actorKind, delegatedRule, true)
		if aerr != nil {
			item.Error = aerr.Error()
			var appendErr concernWaiveAuditAppendError
			var bad concern.InvalidTransitionError
			switch {
			case errors.As(aerr, &appendErr):
				item.ErrorCode = "audit_append_failed"
			case errors.As(aerr, &bad):
				item.ErrorCode = "concern_waive_conflict"
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
		resp.Waived++
		resp.Results = append(resp.Results, item)
	}

	s.writeJSON(w, r, http.StatusOK, resp)
}
