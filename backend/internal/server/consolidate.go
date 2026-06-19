package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// consolidateResponse is the JSON body POST /v0/runs/{run_id}/consolidate
// returns on a 200. Outcome is "integrated" (the fan-in merged every slice
// cleanly, the parent implement stage resolved succeeded, and the
// consolidated PR opened) or "slice_conflict" (a slice branch failed to
// merge, so the parent implement stage failed recoverable category-B,
// preserving the E24.2 contract). The conflict fields are populated only on
// the slice_conflict outcome and mirror the slice_integration_conflict audit
// payload the sweeper emits.
type consolidateResponse struct {
	RunID                 string `json:"run_id"`
	Outcome               string `json:"outcome"`
	ResolvedToState       string `json:"resolved_to_state"`
	ConsolidatedBranch    string `json:"consolidated_branch,omitempty"`
	PullRequestURL        string `json:"pull_request_url,omitempty"`
	ConflictingSliceIndex *int   `json:"conflicting_slice_index,omitempty"`
	ConflictingChildRunID string `json:"conflicting_child_run_id,omitempty"`
	Detail                string `json:"detail,omitempty"`
}

// handleConsolidateRun implements POST /v0/runs/{run_id}/consolidate.
//
// It is the operator-drivable, error-SURFACING fan-in trigger (E24.2 /
// ADR-041 / #1238) for a decomposed parent run parked in awaiting_children.
// The 60s child-completion sweeper is the automatic backstop that normally
// runs the fan-in, but it is off by default in the local dev fishhawkd
// ("dev-loop posture"), so on the local runner a parent whose children have
// all settled stays parked with no consolidated branch/PR. This endpoint lets
// an operator run that same fan-in on demand AND see why it failed: where the
// event-driven path WARN-swallows a non-conflict IntegrateSlices error
// (leaving a silent stuck parent), this returns the error (502
// slice_integration_error) so the operator can diagnose it.
//
// It composes the existing EXPORTED orchestrator primitives
// (IntegrateSlices -> TransitionStage -> Advance), mirroring
// childcompletion.resolveParent, WITHOUT touching the hot event-driven /
// sweeper paths. The children_settled and slice_integration_conflict audit
// payloads are byte-identical to the sweeper's so the children_status
// integration-phase classifier reports correctly after the verb runs.
//
// Auth: operator/operator-agent write:runs token. A run-bound fhm_ agent
// token is rejected (403) — consolidation is an operator action, not a
// self-service one the implement agent drives.
func (s *Server) handleConsolidateRun(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// A run-bound agent token may not consolidate — the decision to fan a
	// decomposition in is an operator action (mirrors the scope-amendment
	// decision endpoint's run-bound rejection).
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "agent_token_forbidden",
			"a run-bound agent token may not consolidate a decomposed parent; the fan-in is an operator action",
			nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:runs") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:runs",
			map[string]any{"required_scope": "write:runs"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.Orchestrator == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "consolidate_unconfigured",
			"consolidate endpoint requires run + audit repositories and an orchestrator", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	// Require a decomposed PARENT: not itself a child (DecomposedFrom == nil)
	// AND it has at least one decomposed child. A non-parent has no fan-in to
	// run, so consolidating it is a 400 rather than a silent no-op.
	if runRow.DecomposedFrom != nil {
		s.writeError(w, r, http.StatusBadRequest, "not_a_decomposed_parent",
			"run is itself a decomposed child, not a parent; only a decomposed parent can be consolidated",
			map[string]any{"decomposed_from": runRow.DecomposedFrom.String()})
		return
	}
	children, err := s.cfg.RunRepo.ListRuns(r.Context(), run.ListRunsFilter{
		DecomposedFrom: &runID,
		Limit:          100,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list decomposed children failed", map[string]any{"error": err.Error()})
		return
	}
	if len(children) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "not_a_decomposed_parent",
			"run has no decomposed children; there is nothing to consolidate", nil)
		return
	}

	// Locate the implement stage parked in awaiting_children. Its absence
	// means the parent was already resolved (or never decomposed), so the
	// fan-in is a no-op — 409 keeps the verb idempotent-friendly (a second
	// call after a successful consolidate, or after the sweeper won the race,
	// reports not_awaiting_children rather than re-resolving).
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stages failed", map[string]any{"error": err.Error()})
		return
	}
	var parentStage *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeImplement && st.State == run.StageStateAwaitingChildren {
			parentStage = st
			break
		}
	}
	if parentStage == nil {
		s.writeError(w, r, http.StatusConflict, "not_awaiting_children",
			"the parent has no implement stage parked in awaiting_children; it is already resolved or not a decomposition",
			map[string]any{"run_id": runID.String()})
		return
	}

	// Every child must be terminal, and every terminal child must have
	// succeeded. A still-running child means the fan-in is premature (409);
	// a failed child means consolidating a partial set would silently drop
	// that slice — route the operator to the failed-child recovery path
	// instead (409) rather than producing a half-consolidated PR.
	inFlight := make([]string, 0)
	failed := make([]string, 0)
	for _, c := range children {
		if !c.State.IsTerminal() {
			inFlight = append(inFlight, c.ID.String())
			continue
		}
		if c.State != run.StateSucceeded {
			failed = append(failed, c.ID.String())
		}
	}
	if len(inFlight) > 0 {
		sort.Strings(inFlight)
		s.writeError(w, r, http.StatusConflict, "children_in_flight",
			"not every decomposed child has reached a terminal state; wait for them to settle before consolidating",
			map[string]any{"non_terminal_child_run_ids": inFlight})
		return
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		s.writeError(w, r, http.StatusConflict, "children_failed",
			"one or more decomposed children failed; resolve or re-drive the failed child rather than consolidating a partial set",
			map[string]any{"failed_child_run_ids": failed})
		return
	}

	s.runConsolidation(w, r, runID, parentStage)
}

// runConsolidation runs the fan-in by composing the exported orchestrator
// primitives, mirroring childcompletion.resolveParent's all-succeeded arm.
// Split out so the precondition checks above stay readable.
func (s *Server) runConsolidation(w http.ResponseWriter, r *http.Request, runID uuid.UUID, parentStage *run.Stage) {
	conflict, err := s.cfg.Orchestrator.IntegrateSlices(r.Context(), runID)
	switch {
	case err != nil:
		// The diagnosability fix: the event-driven path WARN-swallows this;
		// here the operator SEES why the local fan-in failed. The stage is
		// left UNCHANGED (still awaiting_children) — IntegrateSlices is
		// idempotent, so a retry after the operator fixes the cause re-enters
		// cleanly.
		s.writeError(w, r, http.StatusBadGateway, "slice_integration_error",
			"slice integration failed; the parent implement stage is left awaiting_children for retry",
			map[string]any{"error": err.Error()})
		return
	case conflict != nil:
		// A slice branch failed to merge: fail the parent implement stage
		// recoverable (category-B), preserving the E24.2 contract. The audit
		// payload is byte-identical to childcompletion.emitSliceIntegrationConflict
		// so the children_status classifier reports integration_conflict.
		cat := run.FailureB
		reason := conflict.Detail
		if _, terr := s.cfg.RunRepo.TransitionStage(r.Context(), parentStage.ID, run.StageStateFailed, &run.StageCompletion{
			FailureCategory: &cat,
			FailureReason:   &reason,
		}); terr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"transition parent stage to failed-B on slice conflict failed",
				map[string]any{"error": terr.Error()})
			return
		}
		s.emitSliceIntegrationConflict(r.Context(), runID, parentStage.ID, conflict)
		idx := conflict.SliceIndex
		s.writeJSON(w, r, http.StatusOK, consolidateResponse{
			RunID:                 runID.String(),
			Outcome:               "slice_conflict",
			ResolvedToState:       string(run.StageStateFailed),
			ConflictingSliceIndex: &idx,
			ConflictingChildRunID: conflict.ChildRunID.String(),
			Detail:                conflict.Detail,
		})
		return
	}

	// Clean integration: resolve the parent implement stage succeeded, emit a
	// children_settled audit (byte-identical to the sweeper's), then advance
	// the parent so maybeOpenConsolidatedPR opens the consolidated PR and the
	// parent review gate dispatches.
	if _, err := s.cfg.RunRepo.TransitionStage(r.Context(), parentStage.ID, run.StageStateSucceeded, nil); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"transition parent stage to succeeded failed", map[string]any{"error": err.Error()})
		return
	}
	s.emitChildrenSettled(r.Context(), runID, parentStage.ID)

	if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"advance parent after children settled failed", map[string]any{"error": err.Error()})
		return
	}

	// Re-read the run so the response carries the consolidated PR URL Advance
	// stamped, and source the consolidated branch from the slices_integrated
	// audit IntegrateSlices emitted (authoritative; no formula duplication).
	resp := consolidateResponse{
		RunID:              runID.String(),
		Outcome:            "integrated",
		ResolvedToState:    string(run.StageStateSucceeded),
		ConsolidatedBranch: s.consolidatedBranchFromAudit(r.Context(), runID),
	}
	if updated, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err == nil &&
		updated.PullRequestURL != nil {
		resp.PullRequestURL = *updated.PullRequestURL
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// emitChildrenSettled writes a children_settled audit entry whose payload is
// byte-identical to childcompletion.emitChildrenSettled's, so the
// children_status integration-phase classifier reports correctly whether the
// fan-in settled via the sweeper or this operator verb. Best-effort: the
// stage already resolved, so an append failure WARNs but does not unwind the
// response. The all-succeeded arm of this verb only ever resolves the stage
// to succeeded, so resolved_to_state is always "succeeded" here.
func (s *Server) emitChildrenSettled(ctx context.Context, parentRunID, parentStageID uuid.UUID) {
	children, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		DecomposedFrom: &parentRunID,
		Limit:          100,
	})
	if err != nil {
		s.logConsolidateWarn(ctx, "list children for children_settled audit failed", parentRunID, err)
		return
	}
	ids := make([]string, 0, len(children))
	for _, c := range children {
		ids = append(ids, c.ID.String())
	}
	payload, err := json.Marshal(map[string]any{
		"child_run_ids":     ids,
		"parent_stage_id":   parentStageID.String(),
		"resolved_to_state": string(run.StageStateSucceeded),
	})
	if err != nil {
		s.logConsolidateWarn(ctx, "marshal children_settled payload failed", parentRunID, err)
		return
	}
	s.appendConsolidateAudit(ctx, parentRunID, parentStageID, "children_settled", payload)
}

// emitSliceIntegrationConflict writes a slice_integration_conflict audit entry
// whose payload is byte-identical to childcompletion.emitSliceIntegrationConflict's
// — the structured conflicting_slice_index + conflicting_child_run_id the
// next_actions arm reads back as the resume target. Best-effort.
func (s *Server) emitSliceIntegrationConflict(ctx context.Context, parentRunID, parentStageID uuid.UUID, conflict *orchestrator.SliceConflict) {
	payload, err := json.Marshal(map[string]any{
		"parent_stage_id":          parentStageID.String(),
		"conflicting_slice_index":  conflict.SliceIndex,
		"conflicting_child_run_id": conflict.ChildRunID.String(),
	})
	if err != nil {
		s.logConsolidateWarn(ctx, "marshal slice_integration_conflict payload failed", parentRunID, err)
		return
	}
	s.appendConsolidateAudit(ctx, parentRunID, parentStageID, "slice_integration_conflict", payload)
}

// appendConsolidateAudit chain-appends a system-actor audit entry for the
// consolidate verb. Best-effort, mirroring the sweeper's append helpers.
func (s *Server) appendConsolidateAudit(ctx context.Context, parentRunID, parentStageID uuid.UUID, category string, payload json.RawMessage) {
	systemKind := audit.ActorSystem
	stageID := parentStageID
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.logConsolidateWarn(ctx, "append "+category+" audit failed", parentRunID, err)
	}
}

// consolidatedBranchFromAudit reads the consolidated branch name back from the
// slices_integrated audit entry IntegrateSlices emits, so the response need
// not duplicate the orchestrator's branch-naming formula. Returns "" when no
// such entry exists (e.g. the graceful-skip path where GitHub isn't wired).
func (s *Server) consolidatedBranchFromAudit(ctx context.Context, runID uuid.UUID) string {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "slices_integrated")
	if err != nil || len(entries) == 0 {
		return ""
	}
	var payload struct {
		ConsolidatedBranch string `json:"consolidated_branch"`
	}
	// The newest entry reflects the latest integration.
	last := entries[len(entries)-1]
	if json.Unmarshal(last.Payload, &payload) != nil {
		return ""
	}
	return payload.ConsolidatedBranch
}

func (s *Server) logConsolidateWarn(ctx context.Context, msg string, runID uuid.UUID, err error) {
	if s.cfg.Logger == nil {
		return
	}
	s.cfg.Logger.WarnContext(ctx, "consolidate: "+msg,
		"run_id", runID.String(), "error", err.Error())
}
