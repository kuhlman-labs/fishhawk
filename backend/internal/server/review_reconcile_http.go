package server

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/dberr"
)

// reconcileRunReviewsResponse is the 200 body of
// POST /v0/runs/{run_id}/reviews/reconcile.
type reconcileRunReviewsResponse struct {
	RunID string `json:"run_id"`
	// Terminated mirrors the boot sweep's run-level predicate: true when any
	// stage synthesized a terminal entry on THIS call.
	Terminated bool              `json:"terminated"`
	Stages     []reconciledStage `json:"stages"`
}

// handleReconcileRunReviews is the ON-DEMAND orphaned-review recovery (#2712),
// the operator-reachable twin of the boot sweep ReconcileOrphanedReviews runs
// once at startup. It exists because the boot sweep can only run at boot: an
// operator whose review was orphaned by a restart that has ALREADY happened
// (the daemon is up, the strand is durable) would otherwise have to restart
// fishhawkd a second time to clear it.
//
// It runs the SAME per-run helper over the same gates, with two deliberate
// differences from the sweep:
//
//   - NO run-state filter. That is the entire point relative to the sweep,
//     which pages a fixed state set; here the operator names the run.
//   - the boot-marker gate is KEPT. It is the control that stops this verb
//     terminating a review THIS process legitimately still has in flight: a
//     round whose latest *_review_started entry is not before s.processStart
//     was dispatched by the running daemon, so its reviewer is alive and the
//     call reports skip_reason=review_dispatched_by_this_process rather than
//     synthesizing a failure over a live reviewer.
//
// Idempotent: it synthesizes only the MISSING count for the current round, so
// already-landed verdicts are never re-paid for and a second call against a
// healed run is a no-op reporting skip_reason=round_already_settled.
//
// Auth: adminWrite via the route wrapper, mirroring the sibling recovery verb
// reap-failure; the handler additionally requires an authenticated identity
// carrying write:runs, matching handleReapStageFailure verbatim.
func (s *Server) handleReconcileRunReviews(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "review_reconcile_unconfigured",
			"review reconcile endpoint requires run and audit repositories", nil)
		return
	}

	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:runs") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:runs",
			map[string]any{"required_scope": "write:runs"})
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	// The run must exist: reconciling a run id that names nothing would report
	// a vacuous "nothing to heal" instead of the operator's typo.
	if _, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err != nil {
		if dberr.IsUnavailable(err) {
			s.writeDBUnavailable(w, r)
			return
		}
		s.writeError(w, r, http.StatusNotFound, "run_not_found",
			"no run with that id", map[string]any{"run_id": runID.String()})
		return
	}

	stages, err := s.reconcileRunOrphanedReviews(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "review_reconcile_failed",
			"could not reconcile this run's orphaned reviews",
			map[string]any{internalCauseKey: err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusOK, reconcileRunReviewsResponse{
		RunID:      runID.String(),
		Terminated: reconcileSynthesizedAny(stages),
		Stages:     stages,
	})
}
