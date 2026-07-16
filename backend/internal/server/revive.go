package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// RunRevivedCategory is the audit-log category for the single chained
// entry the revive handler writes when a terminal-failed run is
// re-admitted. The payload lists each re-parked stage's id / type /
// prior failure category+reason / restored pre-dispatch state so the
// audit trail records the whole batch re-park in one entry.
//
// The category is registered in audit.KnownCategories (#1941), so operators
// await it without allow_unknown. The identifier deliberately carries the
// "Category" substring so this emit site falls under the
// categories_completeness_test.go value-spec sweep shape, structurally
// enforcing that the emitted literal and the registry entry never diverge.
const RunRevivedCategory = "run_revived"

// reviveResponse is the POST /v0/runs/{run_id}/revive success body: the
// re-opened run plus the per-stage re-park summary.
type reviveResponse struct {
	Run            runResponse           `json:"run"`
	RestoredStages []reviveRestoredStage `json:"restored_stages"`
	// Resumed is true when this call completed an INTERRUPTED prior revive
	// (every failed stage was already re-parked; only the run reopen had not
	// landed) rather than performing fresh re-parks. On a resumed revive
	// restored_stages is empty. Additive field (#1942).
	Resumed bool `json:"resumed"`
	// AuditWarning is set ONLY when the run_revived chained audit append failed
	// AFTER the re-park/reopen transitions were already committed. The revive
	// itself succeeded — the run is reopened and the stages re-parked — and this
	// warning is the caller-visible signal that the chained provenance record is
	// missing. Fail-closed is impossible here: run.ReviveRun commits every
	// re-park and the run reopen before the audit append, and a committed revive
	// cannot be re-run to re-attempt the append (a second revive on a now-running
	// run refuses 422 revive_not_applicable), so a 500 would falsely tell the
	// operator the revive did not happen. Omitted on success (#1943).
	AuditWarning string `json:"audit_warning,omitempty"`
}

// reviveRestoredStage is one re-parked stage on the wire (and in the
// run_revived audit payload). It mirrors run.ReviveStageRestore.
type reviveRestoredStage struct {
	StageID       uuid.UUID `json:"stage_id"`
	Type          string    `json:"type"`
	PriorCategory string    `json:"prior_category"`
	PriorReason   string    `json:"prior_reason"`
	RestoredState string    `json:"restored_state"`
}

// handleReviveRun implements POST /v0/runs/{run_id}/revive.
//
// Revive is the operator recovery action that re-admits a terminal-FAILED
// run for another turn: it pre-validates that EVERY failed stage is
// retryable, then re-parks each failed stage in its correct gate-ordered
// pre-dispatch state (via run.ReviveRun, reusing the run.RetryStage
// per-category targets) and flips the run failed → running. It replaces
// the retry-without-dispatch dance (#1915).
//
// CRUCIALLY revive performs NO orchestrator handoff and never dispatches
// — it re-parks only. That is the deliberate semantic difference from
// /retry and /redrive (both of which Advance): a revived stage sits in
// its pre-dispatch state until the operator dispatches it at its proper
// gate turn via the existing verbs, so the #1700 wrong-order re-dispatch
// corruption is structurally impossible here.
//
// Authorization mirrors /redrive: revive is an OPERATOR action. It
// requires the operator retry scope (write:stages or write:retries) AND
// rejects any MCP subject-bound (agent) token outright — re-opening a
// terminal run is never an agent-permitted action.
func (s *Server) handleReviveRun(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// Reject agent (MCP subject-bound) tokens outright. Re-opening a
	// terminal run is an operator-only recovery action; an agent token
	// must never revive any run.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		s.writeError(w, r, http.StatusForbidden, "agent_token_forbidden",
			"revive is an operator action; agent (mcp) tokens may not revive any run", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") && !hasScope(id, "write:retries") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages or write:retries",
			map[string]any{"required_scope": "write:stages or write:retries"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "revive_unconfigured",
			"revive endpoint requires run + audit repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	dec, err := run.ReviveRun(r.Context(), s.cfg.RunRepo, runID)
	if err != nil {
		switch {
		case errors.Is(err, run.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		case errors.Is(err, run.ErrReviveNotApplicable):
			s.writeError(w, r, http.StatusUnprocessableEntity, "revive_not_applicable",
				err.Error(), nil)
			return
		}
		var inv run.InvalidTransitionError
		if errors.As(err, &inv) {
			s.writeError(w, r, http.StatusConflict, "invalid_state_transition",
				err.Error(),
				map[string]any{"run_id": runID.String(), "from": inv.From, "to": inv.To})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"revive failed", map[string]any{"error": err.Error()})
		return
	}

	restored := make([]reviveRestoredStage, 0, len(dec.Stages))
	for _, st := range dec.Stages {
		restored = append(restored, reviveRestoredStage{
			StageID:       st.StageID,
			Type:          string(st.StageType),
			PriorCategory: string(st.PriorCategory),
			PriorReason:   st.PriorReason,
			RestoredState: string(st.RestoredState),
		})
	}

	// Audit the whole batch re-park in one chained entry. There is
	// DELIBERATELY no orchestrator.Advance and no drive retry_reopen stamp
	// after this — revive re-parks, never dispatches (the semantic
	// difference from /retry and /redrive).
	//
	// The re-park/reopen transitions are already committed, so an append
	// failure does NOT unwind the revive: surface it to the caller as
	// audit_warning on the 200 rather than 500ing on a committed state change
	// (fail-closed is impossible — a committed revive cannot be re-run to
	// re-attempt the append; see #1943).
	auditWarning := ""
	if err := s.writeReviveAudit(r, runID, restored, dec.Resumed); err != nil {
		auditWarning = "run_revived audit append failed: " + err.Error() +
			" — the revive is committed but no chained provenance record was written; see server logs"
	}

	// Sticky status comment (E20.4 / #330): the run flipped failed → running
	// and stages re-parked, so the status comment should re-render.
	s.notifyStatusUpdate(r.Context(), runID, "run_revive")

	s.writeJSON(w, r, http.StatusOK, reviveResponse{
		Run:            toRunResponse(dec.Run),
		RestoredStages: restored,
		Resumed:        dec.Resumed,
		AuditWarning:   auditWarning,
	})
}

// writeReviveAudit appends the single run_revived entry capturing every
// re-parked stage's prior failure detail and restored state, plus the
// actor that triggered the revive. The transitions are already committed,
// so a failure here does NOT unwind: it logs the structured error AND
// returns it so the caller can surface an audit_warning on the 200 (#1943).
// Returns nil on a successful append.
func (s *Server) writeReviveAudit(r *http.Request, runID uuid.UUID, restored []reviveRestoredStage, resumed bool) error {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser

	payload, _ := json.Marshal(map[string]any{
		"run_id":          runID.String(),
		"restored_stages": restored,
		"stage_count":     len(restored),
		"resumed":         resumed,
		"via":             scopeUsed(id),
	})

	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     RunRevivedCategory,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for revive",
			"run_id", runID,
			"error", err.Error(),
		)
		return err
	}
	return nil
}
