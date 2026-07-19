package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryConcernAddressedByCondition is the audit-log category for the entry
// the confirming-review hook writes when a plan-stage concern's binding
// approval condition is confirmed delivered (E48.9 / #1956). Like the waive
// path, the entry is appended BEFORE the state transition and is the durable
// record of the concern -> condition -> confirming-review lineage: the payload
// carries the concern's stable id, its prior state, the claiming approval's
// audit sequence + approver subject, and the confirming review's sequence +
// reviewer model, so the lineage survives in the chain even if the derived
// concern store is rebuilt. Append failure warn-skips the transition — a
// mutation can never exist without this record.
const CategoryConcernAddressedByCondition = "concern_addressed_by_condition"

// validateClaimsConcernIDs validates an approve request's claims_concern_ids
// (E48.9 / #1956) PRE-Submit, mirroring validateBindingAssertions' posture: a
// malformed claim must insert no approval row so a corrected retry flows
// normally. It writes the response and returns false on the first violation;
// returns true (no response written) when the claim set is valid OR empty.
//
// The rules (each an operator-actionable 400/503):
//
//   - claims on decision != approve -> 400 (a claim only makes sense on an
//     approval that carries the binding condition);
//   - claims on a non-plan stage -> 400 (the concerns a condition answers are
//     plan-stage concerns);
//   - nil ConcernRepo with a non-empty claim -> 503 concern_store_unconfigured
//     (mirrors waive.go — the claim cannot be validated or resolved);
//   - a duplicate id in the list -> 400;
//   - each id must parse as a UUID, resolve via GetByIDs, belong to THIS
//     stage's run, be a plan-stage concern, and be in an OPEN state — the
//     first violation 400s naming the offending id. A GetByIDs infrastructure
//     error (store outage, not a missing row) is a 500 internal_error, not a
//     400: a transient failure is retryable, not an operator-corrected id.
func (s *Server) validateClaimsConcernIDs(w http.ResponseWriter, r *http.Request, stage *run.Stage, decision string, ids []string) bool {
	if len(ids) == 0 {
		return true
	}
	if decision != "approve" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"claims_concern_ids is only valid on an approve decision",
			map[string]any{"field": "claims_concern_ids", "decision": decision})
		return false
	}
	if stage.Type != run.StageTypePlan {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"claims_concern_ids is only valid on a plan-stage approval (the concerns a condition answers are plan-stage concerns)",
			map[string]any{"field": "claims_concern_ids", "stage_type": string(stage.Type)})
		return false
	}
	if s.cfg.ConcernRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "concern_store_unconfigured",
			"claims_concern_ids requires a configured concern repository", nil)
		return false
	}

	seen := make(map[string]struct{}, len(ids))
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, raw := range ids {
		if _, dup := seen[raw]; dup {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"claims_concern_ids contains a duplicate id",
				map[string]any{"field": "claims_concern_ids", "concern_id": raw})
			return false
		}
		seen[raw] = struct{}{}
		cid, perr := uuid.Parse(raw)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"claims_concern_ids entry is not a valid UUID",
				map[string]any{"field": "claims_concern_ids", "concern_id": raw})
			return false
		}
		parsed = append(parsed, cid)
	}

	for i, cid := range parsed {
		rows, gerr := s.cfg.ConcernRepo.GetByIDs(r.Context(), []uuid.UUID{cid})
		if gerr != nil {
			// GetByIDs errors ErrNotFound on a missing row (an operator-actionable
			// bad id -> 400) but returns an infrastructure error on a store outage.
			// Collapsing the latter into 400 misreports a transient failure as a bad
			// concern id, pointing the operator at re-reading gate-view ids instead
			// of retrying. Mirror waive.go: ErrNotFound -> 400 here (a claimed id
			// that resolves to no row is a validation failure, not a 404), any other
			// error -> 500 internal_error.
			if errors.Is(gerr, concern.ErrNotFound) {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					"claims_concern_ids references a concern that does not exist",
					map[string]any{"field": "claims_concern_ids", "concern_id": ids[i]})
				return false
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"claims_concern_ids validation could not read the concern store",
				map[string]any{"field": "claims_concern_ids", "concern_id": ids[i], "error": gerr.Error()})
			return false
		}
		row := rows[0]
		if row.RunID != stage.RunID {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"claims_concern_ids references a concern from a different run",
				map[string]any{"field": "claims_concern_ids", "concern_id": ids[i],
					"concern_run_id": row.RunID.String(), "stage_run_id": stage.RunID.String()})
			return false
		}
		if row.StageKind != concern.StageKindPlan {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"claims_concern_ids references a non-plan-stage concern (only plan-stage concerns can be claimed by a condition)",
				map[string]any{"field": "claims_concern_ids", "concern_id": ids[i], "stage_kind": row.StageKind})
			return false
		}
		if !row.State.IsOpen() {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"claims_concern_ids references a concern that is not in an open state",
				map[string]any{"field": "claims_concern_ids", "concern_id": ids[i], "state": string(row.State)})
			return false
		}
	}
	return true
}

// approvalConcernClaims is the loaded record of an approve's claims_concern_ids
// plus the lineage provenance of the claiming approval entry.
type approvalConcernClaims struct {
	ConcernIDs      []string
	ApprovalSeq     int64
	ApproverSubject string
}

// loadApprovalConcernClaims scans the run's approval_submitted audit entries
// (newest-first) for the first entry where decision=="approve" carrying a
// non-empty claims_concern_ids, returning those ids plus that entry's audit
// sequence and approver subject for lineage (E48.9 / #1956). Mirrors
// loadApprovalBindingAssertions. Returns nil when none is found; best-effort:
// WARN-logs and returns nil on any error.
func (s *Server) loadApprovalConcernClaims(ctx context.Context, runID uuid.UUID) *approvalConcernClaims {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "condition-claims: list approval_submitted failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			Decision         string   `json:"decision"`
			Approver         string   `json:"approver"`
			ClaimsConcernIDs []string `json:"claims_concern_ids"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Decision == "approve" && len(payload.ClaimsConcernIDs) > 0 {
			return &approvalConcernClaims{
				ConcernIDs:      payload.ClaimsConcernIDs,
				ApprovalSeq:     entries[i].Sequence,
				ApproverSubject: payload.Approver,
			}
		}
	}
	return nil
}

// resolveConditionClaimedPlanConcerns resolves the run's condition-claimed
// plan-stage concerns to the terminal addressed_by_condition state after ONE
// implement review returns a confirming (non-reject) verdict (E48.9 / #1956).
// The operator's binding approval condition is the authority; the reviewer is
// the witness — so a single confirming verdict resolves the claims even if a
// heterogeneous co-reviewer rejects in the same round.
//
// For each claimed id: GetByIDs; a defense-in-depth WARN-skip unless the row is
// THIS run's plan-stage concern; a silent skip when the row is already terminal
// (idempotency across review rounds, and an operator waive/defer that landed
// first wins); otherwise append the concern_addressed_by_condition audit entry
// FIRST — append failure WARN-skips the transition so a mutation never exists
// without its durable record — then ApplyResolution to addressed_by_condition
// with a lineage state_reason. An InvalidTransitionError WARN-skips (never
// wedges the review loop). The whole function is best-effort/warn-only,
// matching applyConcernResolutions.
//
// confirmingReviewFreshConcernIDs (#2066) are the fresh implement-stage
// concern ids the SAME confirming review minted in this loop iteration. When
// non-empty, the resolution is QUALIFIED: the state_reason drops the
// unqualified "confirmed delivered" phrasing (that review re-raised its own
// implement concerns, so a bare "delivered" ledger label would be misleading)
// and the concern_addressed_by_condition audit payload cross-links those fresh
// ids + a qualified flag. When empty, the historical unqualified wording and
// byte-identical clean-approve behavior are preserved. Qualification keys on
// the MINTED-fresh count (not verdict==approve_with_concerns) so a review whose
// concerns were all relitigation-suppressed correctly stays unqualified and a
// bare approve that still carried a fresh concern is correctly qualified.
func (s *Server) resolveConditionClaimedPlanConcerns(ctx context.Context, runID uuid.UUID, reviewSequence int64, reviewerModel, verdict string, confirmingReviewFreshConcernIDs []uuid.UUID) {
	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	claims := s.loadApprovalConcernClaims(ctx, runID)
	if claims == nil || len(claims.ConcernIDs) == 0 {
		return
	}
	warn := func(concernID, reason string) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "condition-claims: skipping claimed concern",
			slog.String("run_id", runID.String()),
			slog.String("concern_id", concernID),
			slog.String("reason", reason),
		)
	}
	for _, raw := range claims.ConcernIDs {
		cid, perr := uuid.Parse(raw)
		if perr != nil {
			warn(raw, "id is not a valid UUID: "+perr.Error())
			continue
		}
		rows, gerr := s.cfg.ConcernRepo.GetByIDs(ctx, []uuid.UUID{cid})
		if gerr != nil {
			warn(raw, "get concern failed: "+gerr.Error())
			continue
		}
		row := rows[0]
		// Defense-in-depth: the approve-time gate already enforced this, but a
		// reviewed run's own approval entries are the sole claim source, so a
		// cross-run or implement-stage row here would be a bug — never resolve it.
		if row.RunID != runID || row.StageKind != concern.StageKindPlan {
			warn(raw, "claimed concern belongs to a different run or is not a plan-stage concern")
			continue
		}
		// Already-terminal is the idempotent case: a second confirming review
		// round, or an operator waive/defer that resolved the concern first.
		if !row.State.IsOpen() {
			continue
		}

		priorState := string(row.State)
		// QUALIFIED (#2066): when the confirming review minted its own fresh
		// implement concerns, drop the unqualified "confirmed delivered"
		// phrasing — a bare "delivered" ledger label would mislead when the
		// same review re-raised concerns. The concern still resolves (the
		// operator's binding condition is the authority); only the LABEL is
		// honest about the fresh concerns to review before treating as
		// delivered. UNQUALIFIED keeps the historical wording verbatim so the
		// clean-approve path and its gateview settled-ledger rendering are
		// byte-identical.
		qualified := len(confirmingReviewFreshConcernIDs) > 0
		var reason string
		if qualified {
			reason = fmt.Sprintf(
				"binding approval condition (approval sequence %d) claimed by implement review sequence %d, which itself raised %d fresh implement concern(s) (verdict %s) — review the cross-linked concerns before treating as delivered",
				claims.ApprovalSeq, reviewSequence, len(confirmingReviewFreshConcernIDs), verdict)
		} else {
			reason = fmt.Sprintf(
				"binding approval condition (approval sequence %d) confirmed delivered by implement review sequence %d",
				claims.ApprovalSeq, reviewSequence)
		}

		auditPayload := map[string]any{
			"concern_id":                  row.ID.String(),
			"prior_state":                 priorState,
			"approval_sequence":           claims.ApprovalSeq,
			"approver_subject":            claims.ApproverSubject,
			"confirming_review_sequence":  reviewSequence,
			"reviewer_model":              reviewerModel,
			"verdict":                     verdict,
			"confirming_review_qualified": qualified,
		}
		if qualified {
			freshIDStrings := make([]string, 0, len(confirmingReviewFreshConcernIDs))
			for _, id := range confirmingReviewFreshConcernIDs {
				freshIDStrings = append(freshIDStrings, id.String())
			}
			auditPayload["confirming_review_fresh_concern_ids"] = freshIDStrings
		}
		payload, _ := json.Marshal(auditPayload)
		systemKind := audit.ActorSystem
		if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     row.RunID,
			StageID:   &row.StageID,
			Timestamp: time.Now().UTC(),
			Category:  CategoryConcernAddressedByCondition,
			ActorKind: &systemKind,
			Payload:   payload,
		}); aerr != nil {
			// Durable-record-first: without the audit entry, do NOT transition.
			warn(raw, "append concern_addressed_by_condition entry failed: "+aerr.Error())
			continue
		}
		if _, aerr := s.cfg.ConcernRepo.ApplyResolution(ctx, cid, concern.StateAddressedByCondition, reason); aerr != nil {
			// InvalidTransitionError (e.g. a reopen that raced this hook) lands
			// here — warn-skip, never wedge the review loop.
			warn(raw, "apply resolution failed: "+aerr.Error())
			continue
		}
	}
}
