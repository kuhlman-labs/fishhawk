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
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// CategoryScopeAmendmentRequested is the audit-log category for the
// entry the request handler writes when an implement agent files a
// mid-stage scope amendment request (E22.X / #961). Payload carries
// {amendment_id, paths, reason, remaining_budget}. Internal audit
// kind — NOT an issue-comment surface.
const CategoryScopeAmendmentRequested = "scope_amendment_requested"

// CategoryScopeAmendmentDecided is the audit-log category for the
// entry the decision handler writes when an operator approves or
// denies a scope amendment request. Payload carries {amendment_id,
// decision, reason, decided_by}. Internal audit kind — NOT an
// issue-comment surface.
const CategoryScopeAmendmentDecided = "scope_amendment_decided"

// maxScopeAmendmentsPerStage bounds the number of amendment requests a
// single implement stage may file, counted server-side on rows (not
// audit entries) so a denied request still consumes budget — the cap
// bounds operator interruptions, not approvals. Mirrors the fix-up
// budget posture (defaultMaxFixupPasses).
const maxScopeAmendmentsPerStage = 2

// scopeAmendmentRequest is the JSON body of POST
// /v0/runs/{run_id}/scope-amendments.
type scopeAmendmentRequest struct {
	Paths  []scopeamendment.PathEntry `json:"paths"`
	Reason string                     `json:"reason"`
}

// scopeAmendmentDecisionRequest is the JSON body of POST
// /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision.
type scopeAmendmentDecisionRequest struct {
	Decision string `json:"decision"` // approve | deny
	Reason   string `json:"reason"`
}

// scopeAmendmentResponse is the wire shape of one amendment, shared by
// the request, list, and decision handlers (and consumed verbatim by
// the runner's mid-stage refresh — the cross-boundary fixture).
type scopeAmendmentResponse struct {
	ID             uuid.UUID                  `json:"id"`
	RunID          uuid.UUID                  `json:"run_id"`
	StageID        uuid.UUID                  `json:"stage_id"`
	Paths          []scopeamendment.PathEntry `json:"paths"`
	Reason         string                     `json:"reason"`
	Status         string                     `json:"status"`
	DecisionReason *string                    `json:"decision_reason,omitempty"`
	DecidedBy      *string                    `json:"decided_by,omitempty"`
	RequestedAt    time.Time                  `json:"requested_at"`
	DecidedAt      *time.Time                 `json:"decided_at,omitempty"`
	// Scope-cap headroom (#983), warn-only: what the run's effective
	// scope file count would be with this amendment's paths folded in,
	// against the implement stage's resolved max_files_changed. Absent
	// when the headroom computation failed open or no cap is configured.
	// Populated on request/decision responses and, in the list, on
	// PENDING items only — decided rows carry their decision-time
	// numbers in the audit log, and recomputing them post-hoc would be
	// misleading.
	EffectiveScopeFilesAfterApproval *int `json:"effective_scope_files_after_approval,omitempty"`
	MaxFilesChanged                  *int `json:"max_files_changed,omitempty"`
}

// scopeAmendmentListResponse is the GET list envelope.
type scopeAmendmentListResponse struct {
	Items []scopeAmendmentResponse `json:"items"`
}

func amendmentToResponse(a *scopeamendment.Amendment) scopeAmendmentResponse {
	return scopeAmendmentResponse{
		ID:             a.ID,
		RunID:          a.RunID,
		StageID:        a.StageID,
		Paths:          a.Paths,
		Reason:         a.Reason,
		Status:         string(a.Status),
		DecisionReason: a.DecisionReason,
		DecidedBy:      a.DecidedBy,
		RequestedAt:    a.RequestedAt,
		DecidedAt:      a.DecidedAt,
	}
}

// amendmentHeadroom computes the #983 warn-only headroom pair for one
// amendment: the effective scope file count with the amendment's paths
// folded in (as effectiveScopeHeadroom's extraPaths — already-approved
// amendments dedupe against themselves) and the implement stage's
// resolved max_files_changed. Returns (nil, nil) on fail-open or when
// no cap is configured, so callers attach nothing (omitempty) rather
// than a misleading zero.
func (s *Server) amendmentHeadroom(ctx context.Context, a *scopeamendment.Amendment) (effective, maxFiles *int) {
	paths := make([]string, 0, len(a.Paths))
	for _, p := range a.Paths {
		paths = append(paths, p.Path)
	}
	count, capValue, ok := s.effectiveScopeHeadroom(ctx, a.RunID, paths)
	if !ok || capValue <= 0 {
		return nil, nil
	}
	return &count, &capValue
}

// runBoundTokenRunID extracts the run UUID from a run-bound MCP token
// identity ("mcp:run:<uuid>" subject, set by bearerAuth). Returns
// (uuid.Nil, false) for any other identity.
func runBoundTokenRunID(id Identity) (uuid.UUID, bool) {
	if !strings.HasPrefix(id.Subject, "mcp:run:") {
		return uuid.Nil, false
	}
	runID, err := uuid.Parse(strings.TrimPrefix(id.Subject, "mcp:run:"))
	if err != nil {
		return uuid.Nil, false
	}
	return runID, true
}

// handleRequestScopeAmendment implements POST
// /v0/runs/{run_id}/scope-amendments.
//
// Auth: a run-bound fhm_ token carrying write:scope-amendments, ONLY.
// The path run_id must equal the token's run (cross-run → 403), and
// the run's currently-executing stage must be an implement stage —
// amendments exist so the implement agent can widen its effective
// scope.files mid-stage, nothing else.
func (s *Server) handleRequestScopeAmendment(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ScopeAmendmentRepo == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "scope_amendment_unconfigured",
			"scope-amendment endpoint requires scope-amendment, run, and audit repositories", nil)
		return
	}

	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	tokenRunID, runBound := runBoundTokenRunID(id)
	if !runBound {
		s.writeError(w, r, http.StatusForbidden, "agent_token_required",
			"scope amendment requests are filed by the implement agent's run-bound token; operators decide, not request",
			nil)
		return
	}
	if !hasScope(id, "write:scope-amendments") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:scope-amendments",
			map[string]any{"required_scope": "write:scope-amendments"})
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	if tokenRunID != runID {
		s.writeError(w, r, http.StatusForbidden, "cross_run_scope_amendment",
			"mcp token may only request scope amendments for its own run",
			map[string]any{
				"token_run_id": tokenRunID.String(),
				"path_run_id":  runID.String(),
			})
		return
	}

	var reqBody scopeAmendmentRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body must be valid JSON {paths, reason}",
			map[string]any{"error": err.Error()})
		return
	}
	paths, err := scopeamendment.ValidatePaths(reqBody.Paths)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "paths"})
		return
	}
	if strings.TrimSpace(reqBody.Reason) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: state why each path must be folded into the scope",
			map[string]any{"field": "reason"})
		return
	}

	// Resolve the run's currently-executing implement stage; the
	// amendment hangs off it (budget count + audit entries).
	stage, err := s.resolveExecutingImplementStage(r, runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"resolve executing stage failed", map[string]any{"error": err.Error()})
		return
	}
	if stage == nil {
		s.writeError(w, r, http.StatusConflict, "stage_not_implement",
			"scope amendments may only be requested while an implement stage is executing",
			nil)
		return
	}

	// Server-side per-stage cap, counted on rows so denied requests
	// still consume budget.
	used, err := s.cfg.ScopeAmendmentRepo.CountByStage(r.Context(), stage.ID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"count scope amendments failed", map[string]any{"error": err.Error()})
		return
	}
	if used >= maxScopeAmendmentsPerStage {
		s.writeError(w, r, http.StatusUnprocessableEntity, "amendment_budget_exhausted",
			"this stage has exhausted its scope-amendment budget; adapt within the approved scope or fail loud",
			map[string]any{"max": maxScopeAmendmentsPerStage, "used": used})
		return
	}

	amendment, err := s.cfg.ScopeAmendmentRepo.Create(r.Context(), scopeamendment.CreateParams{
		RunID:   runID,
		StageID: stage.ID,
		Paths:   paths,
		Reason:  strings.TrimSpace(reqBody.Reason),
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create scope amendment failed", map[string]any{"error": err.Error()})
		return
	}

	effective, maxFiles := s.amendmentHeadroom(r.Context(), amendment)
	s.writeScopeAmendmentRequestedAudit(r, amendment, id.Subject,
		maxScopeAmendmentsPerStage-used-1, effective, maxFiles)

	resp := amendmentToResponse(amendment)
	resp.EffectiveScopeFilesAfterApproval = effective
	resp.MaxFilesChanged = maxFiles
	s.writeJSON(w, r, http.StatusCreated, resp)
}

// handleListScopeAmendments implements GET
// /v0/runs/{run_id}/scope-amendments.
//
// Auth: EITHER the run-bound fhm_ token with mcp:read — the implement
// agent's poll loop and the runner's pre-commit refresh both reuse the
// token the runner fetched at stage start (single agent-side auth
// path) — with the same path-run_id == token-run binding as the POST
// (cross-run → 403); OR an operator bearer/session (not run-bound).
func (s *Server) handleListScopeAmendments(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ScopeAmendmentRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "scope_amendment_unconfigured",
			"scope-amendment endpoint requires a scope-amendment repository", nil)
		return
	}

	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	if tokenRunID, runBound := runBoundTokenRunID(id); runBound {
		if !hasScope(id, "mcp:read") {
			s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
				"token is missing required scope: mcp:read",
				map[string]any{"required_scope": "mcp:read"})
			return
		}
		if tokenRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_scope_amendment",
				"mcp token may only read scope amendments for its own run",
				map[string]any{
					"token_run_id": tokenRunID.String(),
					"path_run_id":  runID.String(),
				})
			return
		}
	}

	items, err := s.cfg.ScopeAmendmentRepo.ListByRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list scope amendments failed", map[string]any{"error": err.Error()})
		return
	}
	resp := scopeAmendmentListResponse{Items: make([]scopeAmendmentResponse, 0, len(items))}
	for _, a := range items {
		item := amendmentToResponse(a)
		// Headroom (#983) for PENDING items only: the operator deciding
		// from the list sees what approving would do; decided rows carry
		// their decision-time numbers in the audit log.
		if a.Status == scopeamendment.StatusPending {
			item.EffectiveScopeFilesAfterApproval, item.MaxFilesChanged =
				s.amendmentHeadroom(r.Context(), a)
		}
		resp.Items = append(resp.Items, item)
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// handleDecideScopeAmendment implements POST
// /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision.
//
// Auth: operator bearer/session ONLY, requiring write:stages for token
// callers. Run-bound tokens are rejected outright (403 self_decision):
// the requesting agent's token holds write:scope-amendments and must
// never decide its own request. Implement-stage tokens are never
// issued write:stages (server/mcptoken.go), so the run-bound rejection
// is defense in depth.
func (s *Server) handleDecideScopeAmendment(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ScopeAmendmentRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "scope_amendment_unconfigured",
			"scope-amendment decision endpoint requires scope-amendment and audit repositories", nil)
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
			"a run-bound agent token may not decide a scope amendment; the decision is an operator action",
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
	amendmentID, err := uuid.Parse(r.PathValue("amendment_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"amendment_id must be a valid UUID",
			map[string]any{"field": "amendment_id", "got": r.PathValue("amendment_id")})
		return
	}

	var reqBody scopeAmendmentDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body must be valid JSON {decision, reason}",
			map[string]any{"error": err.Error()})
		return
	}
	var status scopeamendment.Status
	switch reqBody.Decision {
	case "approve":
		status = scopeamendment.StatusApproved
	case "deny":
		status = scopeamendment.StatusDenied
	default:
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"decision must be \"approve\" or \"deny\"",
			map[string]any{"field": "decision", "got": reqBody.Decision})
		return
	}

	existing, err := s.cfg.ScopeAmendmentRepo.GetByID(r.Context(), amendmentID)
	if err != nil {
		if errors.Is(err, scopeamendment.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "amendment_not_found",
				"no scope amendment with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get scope amendment failed", map[string]any{"error": err.Error()})
		return
	}
	if existing.RunID != runID {
		// Path/row mismatch reads as not-found: the amendment does
		// not exist under this run.
		s.writeError(w, r, http.StatusNotFound, "amendment_not_found",
			"no scope amendment with that id for this run", nil)
		return
	}

	decided, err := s.cfg.ScopeAmendmentRepo.Decide(r.Context(), scopeamendment.DecideParams{
		ID:        amendmentID,
		Status:    status,
		Reason:    reqBody.Reason,
		DecidedBy: id.Subject,
	})
	if err != nil {
		switch {
		case errors.Is(err, scopeamendment.ErrAlreadyDecided):
			s.writeError(w, r, http.StatusConflict, "amendment_already_decided",
				"this scope amendment has already been decided",
				map[string]any{"status": string(existing.Status)})
			return
		case errors.Is(err, scopeamendment.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "amendment_not_found",
				"no scope amendment with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"decide scope amendment failed", map[string]any{"error": err.Error()})
		return
	}

	effective, maxFiles := s.amendmentHeadroom(r.Context(), decided)
	s.writeScopeAmendmentDecidedAudit(r, decided, reqBody.Decision, effective, maxFiles)

	resp := amendmentToResponse(decided)
	resp.EffectiveScopeFilesAfterApproval = effective
	resp.MaxFilesChanged = maxFiles
	s.writeJSON(w, r, http.StatusOK, resp)
}

// resolveExecutingImplementStage returns the run's currently-executing
// (dispatched/running) implement stage, nil when no such stage exists.
// run.ErrNotFound when the run itself doesn't exist.
func (s *Server) resolveExecutingImplementStage(r *http.Request, runID uuid.UUID) (*run.Stage, error) {
	if _, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err != nil {
		return nil, err
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		return nil, err
	}
	for _, st := range stages {
		if st.Type != run.StageTypeImplement {
			continue
		}
		if st.State == run.StageStateDispatched || st.State == run.StageStateRunning {
			return st, nil
		}
	}
	return nil, nil
}

// writeScopeAmendmentRequestedAudit appends the scope_amendment_requested
// chain entry. Best-effort: a failure logs but doesn't unwind the
// request — the row is the durable record; the audit entry is the
// operator's await anchor (fishhawk_await_audit, #977).
func (s *Server) writeScopeAmendmentRequestedAudit(r *http.Request, a *scopeamendment.Amendment, subject string, remainingBudget int, effectiveAfterApproval, maxFiles *int) {
	actorKind := audit.ActorAgent
	payloadMap := map[string]any{
		"amendment_id":     a.ID.String(),
		"paths":            a.Paths,
		"reason":           a.Reason,
		"remaining_budget": remainingBudget,
	}
	if effectiveAfterApproval != nil && maxFiles != nil {
		payloadMap["effective_scope_files_after_approval"] = *effectiveAfterApproval
		payloadMap["max_files_changed"] = *maxFiles
	}
	payload, _ := json.Marshal(payloadMap)
	stageID := a.StageID
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        a.RunID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryScopeAmendmentRequested,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"scope_amendment_requested audit append failed",
			slog.String("run_id", a.RunID.String()),
			slog.String("amendment_id", a.ID.String()),
			slog.String("error", err.Error()))
	}
}

// writeScopeAmendmentDecidedAudit appends the scope_amendment_decided
// chain entry. Best-effort, same posture as the requested entry.
func (s *Server) writeScopeAmendmentDecidedAudit(r *http.Request, a *scopeamendment.Amendment, decision string, effectiveAfterApproval, maxFiles *int) {
	actorKind := audit.ActorUser
	subject := ""
	if a.DecidedBy != nil {
		subject = *a.DecidedBy
	}
	payloadMap := map[string]any{
		"amendment_id": a.ID.String(),
		"decision":     decision,
		"reason":       a.DecisionReason,
		"decided_by":   subject,
	}
	if effectiveAfterApproval != nil && maxFiles != nil {
		payloadMap["effective_scope_files_after_approval"] = *effectiveAfterApproval
		payloadMap["max_files_changed"] = *maxFiles
	}
	payload, _ := json.Marshal(payloadMap)
	stageID := a.StageID
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        a.RunID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryScopeAmendmentDecided,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"scope_amendment_decided audit append failed",
			slog.String("run_id", a.RunID.String()),
			slog.String("amendment_id", a.ID.String()),
			slog.String("error", err.Error()))
	}
}
