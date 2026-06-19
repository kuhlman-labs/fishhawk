package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The three scope-completeness audit kinds (#1231) are internal audit
// kinds, NOT issue-comment surfaces. They have no Notifier methods and
// nothing in `issuecomment` posts them — see docs/issue-comment-surfaces.md.
const (
	// CategoryScopeCompletenessParked is written by the pull-request park
	// handler (server/pullrequest.go::parkScopeCompletenessStage) when the
	// runner reports {outcome:"scope_park"}: the implement stage's ONLY
	// committed-tree gate failure was the missing-declared-scope-file
	// check, the verified commit is held on the run branch, and the stage
	// parked in awaiting_scope_decision. Payload carries {run_id, stage_id,
	// branch, head_sha, base_sha, verified_tree_sha, missing_paths,
	// auth_method}.
	CategoryScopeCompletenessParked = "scope_completeness_parked"

	// CategoryScopeCompletenessExempted is written by the decision handler
	// on an `exempt` decision: the operator accepted the already-committed
	// tree, so the held commit's PR is opened with NO agent re-run. Payload
	// carries {run_id, stage_id, decision, reason, decided_by,
	// held_commit_sha, run_branch, verified_tree_sha, missing_paths,
	// gate_evidence}. The gate_evidence field reuses the #1153 channel so a
	// downstream implement-review gate sees the missing-file shortfall was
	// operator-exempted rather than re-failing on it.
	CategoryScopeCompletenessExempted = "scope_completeness_exempted"

	// CategoryScopeCompletenessFailed is written by the decision handler on
	// a `fail` decision: the operator rejected the exemption, so the stage
	// fails category-B (today's restore path). Payload carries {run_id,
	// stage_id, decision, reason, decided_by, missing_paths}.
	CategoryScopeCompletenessFailed = "scope_completeness_failed"
)

// scopeCompletenessDecisionRequest is the JSON body of POST
// /v0/runs/{run_id}/scope-completeness/decision.
type scopeCompletenessDecisionRequest struct {
	Decision string `json:"decision"` // exempt | fail
	Reason   string `json:"reason"`
}

// scopeCompletenessDecisionResponse echoes the resolved decision back to
// the operator. On `exempt`, HeldCommitSHA / RunBranch carry the commit the
// follow-on push_and_open_pr dispatch opens the PR from (no agent re-run).
type scopeCompletenessDecisionResponse struct {
	StageID       uuid.UUID `json:"stage_id"`
	Decision      string    `json:"decision"`
	State         string    `json:"state"`
	HeldCommitSHA string    `json:"held_commit_sha,omitempty"`
	RunBranch     string    `json:"run_branch,omitempty"`
}

// handleDecideScopeCompleteness implements POST
// /v0/runs/{run_id}/scope-completeness/decision (#1231).
//
// Auth: operator bearer/session ONLY, requiring write:stages for token
// callers — the same posture as the scope-amendment decision endpoint.
// Run-bound fhm_ tokens are rejected outright (403 self_decision): the
// implement agent that produced the parked commit must never decide its
// own exemption; the decision is an operator action.
//
// The implement stage MUST currently be parked in awaiting_scope_decision
// (409 otherwise). On `exempt` the stage resumes running so the held
// commit's PR can be opened with no agent re-run (the push_and_open_pr
// dispatch + the runner's PR-open from the held commit are the sibling
// slices); on `fail` the stage drops to today's category-B restore path.
func (s *Server) handleDecideScopeCompleteness(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "scope_completeness_unconfigured",
			"scope-completeness decision endpoint requires run and audit repositories", nil)
		return
	}

	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "self_decision",
			"a run-bound agent token may not decide a scope-completeness park; the decision is an operator action",
			nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages",
			map[string]any{"required_scope": "write:stages"})
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var reqBody scopeCompletenessDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body must be valid JSON {decision, reason}",
			map[string]any{"error": err.Error()})
		return
	}
	switch reqBody.Decision {
	case "exempt", "fail":
	default:
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"decision must be \"exempt\" or \"fail\"",
			map[string]any{"field": "decision", "got": reqBody.Decision})
		return
	}
	if strings.TrimSpace(reqBody.Reason) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: state why the parked scope-completeness shortfall is exempted or failed",
			map[string]any{"field": "reason"})
		return
	}

	stage, err := s.resolveParkedScopeStage(r, runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"resolve parked stage failed", map[string]any{"error": err.Error()})
		return
	}
	if stage == nil {
		s.writeError(w, r, http.StatusConflict, "stage_not_parked",
			"the implement stage is not parked awaiting a scope-completeness decision",
			nil)
		return
	}

	if reqBody.Decision == "exempt" {
		s.exemptScopeCompleteness(w, r, runID, stage, reqBody.Reason, id.Subject)
		return
	}
	s.failScopeCompleteness(w, r, runID, stage, reqBody.Reason, id.Subject)
}

// resolveParkedScopeStage returns the run's implement stage when it is
// currently parked in awaiting_scope_decision; nil when the run exists but
// no implement stage is parked (→ 409). run.ErrNotFound when the run
// itself doesn't exist.
func (s *Server) resolveParkedScopeStage(r *http.Request, runID uuid.UUID) (*run.Stage, error) {
	if _, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err != nil {
		return nil, err
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		return nil, err
	}
	for _, st := range stages {
		if st.Type == run.StageTypeImplement && st.State == run.StageStateAwaitingScopeDecision {
			return st, nil
		}
	}
	return nil, nil
}

// exemptScopeCompleteness resolves an `exempt` decision: it resumes the
// parked implement stage to running so the held commit's PR can be opened
// with NO agent re-run (the push_and_open_pr dispatch + the runner's
// held-commit PR-open are the sibling slices), and appends a
// scope_completeness_exempted audit entry pinning the held commit and the
// gate_evidence marker. Best-effort audit, mirroring the pull-request
// handlers: the transition is the durable state change.
func (s *Server) exemptScopeCompleteness(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	stage *run.Stage, reason, decidedBy string) {
	if _, err := s.cfg.RunRepo.TransitionStage(r.Context(), stage.ID, run.StageStateRunning, nil); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"resume parked stage failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeScopeCompletenessDecisionAudit(r, runID, stage, CategoryScopeCompletenessExempted, reason, decidedBy)
	s.notifyStatusUpdate(r.Context(), runID, "scope_exempted")

	resp := scopeCompletenessDecisionResponse{
		StageID:  stage.ID,
		Decision: "exempt",
		State:    string(run.StageStateRunning),
	}
	if park := stage.ScopeCompletenessPark; park != nil {
		resp.HeldCommitSHA = park.HeldCommitSHA
		resp.RunBranch = park.RunBranch
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// failScopeCompleteness resolves a `fail` decision: it fails the parked
// implement stage category-B (today's restore path) and appends a
// scope_completeness_failed audit entry, then advances the run so the
// orchestrator walks it forward — mirroring failPullRequestStage.
func (s *Server) failScopeCompleteness(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	stage *run.Stage, reason, decidedBy string) {
	if _, err := run.FailStage(r.Context(), s.cfg.RunRepo, stage.ID, run.FailureB, reason); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"fail parked stage failed", map[string]any{"error": err.Error()})
		return
	}

	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"scope-completeness fail decision: orchestrator advance failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	s.writeScopeCompletenessDecisionAudit(r, runID, stage, CategoryScopeCompletenessFailed, reason, decidedBy)
	s.notifyStatusUpdate(r.Context(), runID, "scope_failed")

	s.writeJSON(w, r, http.StatusOK, scopeCompletenessDecisionResponse{
		StageID:  stage.ID,
		Decision: "fail",
		State:    string(run.StageStateFailed),
	})
}

// writeScopeCompletenessDecisionAudit appends the exempted/failed decision
// audit entry, pinning the operator's reason and the parked held-commit
// coordinates into the chain. Best-effort: a failure logs but doesn't
// unwind the decision — the stage transition is the durable record.
func (s *Server) writeScopeCompletenessDecisionAudit(r *http.Request, runID uuid.UUID,
	stage *run.Stage, category, reason, decidedBy string) {
	actorKind := actorKindForSubject(decidedBy)
	stageID := stage.ID
	payloadMap := map[string]any{
		"run_id":     runID.String(),
		"stage_id":   stageID.String(),
		"decision":   strings.TrimPrefix(category, "scope_completeness_"),
		"reason":     reason,
		"decided_by": decidedBy,
	}
	if park := stage.ScopeCompletenessPark; park != nil {
		payloadMap["missing_paths"] = park.MissingPaths
		if category == CategoryScopeCompletenessExempted {
			payloadMap["held_commit_sha"] = park.HeldCommitSHA
			payloadMap["run_branch"] = park.RunBranch
			payloadMap["verified_tree_sha"] = park.VerifiedTreeSHA
			// Reuse the #1153 gate_evidence channel so a downstream
			// implement-review gate reads the missing-file shortfall as
			// operator-exempted rather than re-failing on it.
			payloadMap["gate_evidence"] = CategoryScopeCompletenessExempted
		}
	}
	payload, _ := json.Marshal(payloadMap)
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &actorKind,
		ActorSubject: &decidedBy,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			category+" audit append failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
}
