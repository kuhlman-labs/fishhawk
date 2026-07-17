package server

import (
	"bytes"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// rollbackDispatchInput is the workflow_dispatch / webhook trigger input that
// marks a deploy trigger as the ROLLBACK path rather than the forward deploy
// (#1386 / E23.6). The same delegate workflow_ref serves both directions — the
// external deploy workflow keys its rollback branch off this input being
// "true". Re-dispatching the same workflow_ref with a rollback input (rather
// than a separately configured rollback_workflow_ref) is the operator-ratified
// tradeoff that avoids a workflow-v* schema bump; a distinct rollback_workflow_ref
// stays a deferred additive spec field.
const rollbackDispatchInput = "fishhawk_rollback"

// rollbackResponse is the 202 body of POST /v0/runs/{run_id}/deployment/rollback.
// It echoes the external rollback run handle so an operator (or the issue
// timeline) can follow the rollback pipeline to its terminal outcome.
type rollbackResponse struct {
	RunID          string `json:"run_id"`
	StageID        string `json:"stage_id"`
	Target         string `json:"target"`
	GHARunID       int64  `json:"gha_run_id,omitempty"`
	ExternalRunURL string `json:"external_run_url,omitempty"`
	Message        string `json:"message"`
}

// handleRollbackDeployment implements POST /v0/runs/{run_id}/deployment/rollback.
//
// It is the operator-triggered rollback sub-action for a delegating deploy
// (#1386 / E23.6, ADR-038's rolled_back disposition). Fishhawk holds no prod
// credentials, so a rollback is just another delegating trigger: it
// re-dispatches the SAME external pipeline down its rollback path and records
// the rollback run handle. The endpoint OWNS only the INITIATE side — it writes
// the deployment_rollback_initiated audit carrying the rollback run handle,
// DISTINCT from the initial deploy's deployment_dispatched handle (#1386
// binding condition 2).
//
// Terminal completion (set DeployOutcome=rolled_back + write
// deployment_rollback_completed) is recorded ONLY when the rollback run reaches
// terminal — NOT at initiation. The external rollback pipeline reports terminal
// by calling back into POST /v0/runs/{run_id}/deployment with
// {outcome:"rolled_back", rollback_action:"completed"}, which the existing
// deployment-upload handler persists (the rolled_back deployment artifact is the
// durable carrier of DeployOutcome) and chains as deployment_rollback_completed.
// This is the rollback terminal callback path and is uniform across both
// targets. (Reconciler-side polling of a github_actions rollback handle for a
// pipeline that does NOT call back is a deferred follow-up: the deploy
// reconciler scans only awaiting_deployment stages, and a rolled-back deploy
// stage is already terminal.)
//
// Auth: operator-only (an authenticated bearer with write:runs scope). A
// rollback is never runner-initiated, so there is no Ed25519 signature path. A
// run-bound MCP token (subject "mcp:run:<uuid>") may roll back only its own run,
// mirroring the reset-branch / fixup handlers.
func (s *Server) handleRollbackDeployment(w http.ResponseWriter, r *http.Request) {
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
	// write:deploy (ADR-038 / #1390) gates the OPERATOR bearer rollback path
	// on top of write:runs. The mcp:run self-rollback path is exempt: a
	// run-bound token (subject "mcp:run:<uuid>") is constrained instead by the
	// subject-binding guard below (it may roll back only its own run), so it
	// keeps working without the deploy scope. Cookie sessions (TokenID == "")
	// are exempt like every other scope check.
	if id.TokenID != "" && !strings.HasPrefix(id.Subject, "mcp:run:") && !hasScope(id, "write:deploy") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:deploy",
			map[string]any{"required_scope": "write:deploy"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "rollback_unconfigured",
			"deployment rollback requires run and audit repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	// Subject-binding guard: a run-bound MCP token may roll back ONLY its own
	// run's deploy. Mirrors the reset-branch + fixup handlers.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		subjectRunID, parseErr := uuid.Parse(strings.TrimPrefix(id.Subject, "mcp:run:"))
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_rollback",
				"mcp token may only roll back its own run's deploy",
				map[string]any{
					"token_run_id":  subjectRunID.String(),
					"target_run_id": runID.String(),
				})
			return
		}
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

	deployStage, err := s.deployStageForRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list run stages failed", map[string]any{"error": err.Error()})
		return
	}
	if deployStage == nil {
		s.writeError(w, r, http.StatusNotFound, "deploy_stage_not_found",
			"run has no deploy stage to roll back",
			map[string]any{"run_id": runID.String()})
		return
	}
	// Precondition: only a SETTLED deploy can be rolled back. A succeeded or
	// failed deploy stage has a recorded outcome (failed carries partial via
	// the artifact's outcome field); an in-flight stage (awaiting_deployment /
	// awaiting_deploy_approval / pending) has nothing to revert yet. Rolling
	// back what never shipped is rejected rather than firing a meaningless
	// rollback dispatch.
	if deployStage.State != run.StageStateSucceeded && deployStage.State != run.StageStateFailed {
		s.writeError(w, r, http.StatusConflict, "deploy_not_settled",
			"deploy stage has not reached a terminal outcome; nothing to roll back",
			map[string]any{"stage_id": deployStage.ID.String(), "state": string(deployStage.State)})
		return
	}

	delegate, err := deployDelegateForRun(runRow)
	if err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "rollback_unconfigured",
			"run's cached workflow spec carries no delegating deploy stage to re-dispatch",
			map[string]any{"error": err.Error()})
		return
	}

	target, ghaRunID, externalURL, derr := s.dispatchRollback(r.Context(), runRow, deployStage, delegate)
	if derr != nil {
		s.writeError(w, r, derr.status, derr.code, derr.message, derr.details)
		return
	}

	// deployment_rollback_initiated — the DISTINCT rollback handle (#1386
	// binding condition 2). Separate category from the initial deploy's
	// deployment_dispatched so a reader (and a future reconciler) can tell a
	// rollback run apart from the forward deploy run.
	dispatchedAt := time.Now().UTC()
	subj := id.Subject
	actorKind := actorKindForSubject(id.Subject)
	payload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         deployStage.ID.String(),
		"target":           target,
		"workflow_ref":     delegate.WorkflowRef,
		"url":              delegate.URL,
		"git_ref":          rollbackGitRef(delegate),
		"gha_run_id":       ghaRunID,
		"external_run_url": externalURL,
		"rollback":         true,
		"dispatched_at":    dispatchedAt.Format(time.RFC3339),
		"auth_method":      "bearer",
		"actor_subject":    subj,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &deployStage.ID,
		Timestamp:    dispatchedAt,
		Category:     CategoryDeploymentRollbackInitiated,
		ActorKind:    &actorKind,
		ActorSubject: &subj,
		Payload:      payload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append deployment_rollback_initiated audit failed",
			map[string]any{"error": err.Error()})
		return
	}

	// Surface the rollback on the issue timeline (the deploy audit categories
	// render data-drivenly through the issuecomment activity set).
	s.notifyStatusUpdate(r.Context(), runID, "deployment_rollback_initiated")

	s.writeJSON(w, r, http.StatusAccepted, rollbackResponse{
		RunID:          runID.String(),
		StageID:        deployStage.ID.String(),
		Target:         target,
		GHARunID:       ghaRunID,
		ExternalRunURL: externalURL,
		Message: "rollback re-dispatched; deployment_rollback_completed and the " +
			"rolled_back outcome are recorded when the rollback run reaches terminal " +
			"(the external pipeline calls back into POST /v0/runs/{run_id}/deployment).",
	})
}

// rollbackDispatchError carries an HTTP-mappable dispatch failure out of
// dispatchRollback so the handler can render the right status/code without the
// dispatch logic knowing about http.ResponseWriter.
type rollbackDispatchError struct {
	status  int
	code    string
	message string
	details map[string]any
}

func (e *rollbackDispatchError) Error() string { return e.code + ": " + e.message }

// dispatchRollback re-dispatches the delegate's rollback path and returns the
// resolved external run handle. github_actions fires a workflow_dispatch of the
// same workflow_ref carrying the fishhawk correlation + rollbackDispatchInput,
// then best-effort resolves the rollback run id (the dispatch endpoint returns
// 204 with no body, mirroring slice-1's trigger). webhook POSTs a rollback
// payload to the delegate URL. A dispatch failure returns a *rollbackDispatchError
// the handler maps to the response.
func (s *Server) dispatchRollback(ctx context.Context, runRow *run.Run, stage *run.Stage, delegate *spec.DelegateConfig) (target string, ghaRunID int64, externalURL string, derr *rollbackDispatchError) {
	switch delegate.Target {
	case spec.DelegateTargetGitHubActions:
		return s.dispatchRollbackGitHubActions(ctx, runRow, stage, delegate)
	case spec.DelegateTargetWebhook:
		return s.dispatchRollbackWebhook(ctx, runRow, stage, delegate)
	default:
		return "", 0, "", &rollbackDispatchError{
			status:  http.StatusUnprocessableEntity,
			code:    "rollback_unconfigured",
			message: fmt.Sprintf("deploy delegate target %q is not supported", delegate.Target),
			details: map[string]any{"target": delegate.Target},
		}
	}
}

func (s *Server) dispatchRollbackGitHubActions(ctx context.Context, runRow *run.Run, stage *run.Stage, delegate *spec.DelegateConfig) (string, int64, string, *rollbackDispatchError) {
	if delegate.WorkflowRef == "" {
		return "", 0, "", &rollbackDispatchError{
			status: http.StatusUnprocessableEntity, code: "rollback_unconfigured",
			message: "github_actions delegate is missing workflow_ref",
		}
	}
	if s.cfg.GitHub == nil {
		// Un-wired/demo backend — a rollback cannot dispatch without a GitHub
		// client. Unlike slice-1's trigger (which parks at dispatched in the
		// un-wired posture), an operator-triggered rollback fails loud so the
		// caller knows nothing fired.
		return "", 0, "", &rollbackDispatchError{
			status: http.StatusServiceUnavailable, code: "rollback_unconfigured",
			message: "github_actions rollback requires a configured GitHub client",
		}
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return "", 0, "", &rollbackDispatchError{
			status: http.StatusUnprocessableEntity, code: "rollback_unconfigured",
			message: "run has no installation_id; cannot dispatch the rollback workflow",
		}
	}
	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		return "", 0, "", &rollbackDispatchError{
			status: http.StatusUnprocessableEntity, code: "rollback_unconfigured",
			message: err.Error(), details: map[string]any{"repo": runRow.Repo},
		}
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)

	branch := rollbackGitRef(delegate)
	correlation := map[string]string{
		"fishhawk_run_id":     stage.RunID.String(),
		"fishhawk_stage_id":   stage.ID.String(),
		rollbackDispatchInput: "true",
	}
	dispatchedAt := time.Now().UTC()
	if err := s.cfg.GitHub.DispatchWorkflow(ctx, scope, repo,
		delegate.WorkflowRef, branch, githubclient.DispatchInputs(correlation)); err != nil {
		return "", 0, "", &rollbackDispatchError{
			status: http.StatusBadGateway, code: "rollback_dispatch_failed",
			message: "workflow_dispatch of the rollback path failed",
			details: map[string]any{"error": err.Error(), "workflow_ref": delegate.WorkflowRef, "git_ref": branch},
		}
	}

	// Best-effort run-id resolution. An indeterminate / not-yet-visible result
	// is NOT a dispatch failure — the rollback pipeline IS running; the handle
	// is recorded with a zero run id and the rollback can still be followed via
	// the external pipeline's terminal callback.
	var ghaRunID int64
	var externalURL string
	resolved, rerr := s.cfg.GitHub.ResolveDispatchedRun(ctx, scope, repo, branch, correlation, dispatchedAt.Add(-1*time.Minute))
	switch {
	case rerr != nil:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"deploy rollback: dispatched-run resolution errored; handle recorded without a run id",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", rerr.Error()))
	case resolved == nil:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"deploy rollback: dispatched run not yet resolvable; handle recorded without a run id",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()))
	default:
		ghaRunID = resolved.ID
		externalURL = resolved.HTMLURL
	}
	return spec.DelegateTargetGitHubActions, ghaRunID, externalURL, nil
}

func (*Server) dispatchRollbackWebhook(ctx context.Context, runRow *run.Run, stage *run.Stage, delegate *spec.DelegateConfig) (string, int64, string, *rollbackDispatchError) {
	if delegate.URL == "" {
		return "", 0, "", &rollbackDispatchError{
			status: http.StatusUnprocessableEntity, code: "rollback_unconfigured",
			message: "webhook delegate is missing url",
		}
	}
	triggerBody, _ := json.Marshal(map[string]any{
		"fishhawk_run_id":     stage.RunID.String(),
		"fishhawk_stage_id":   stage.ID.String(),
		rollbackDispatchInput: true,
		"repo":                runRow.Repo,
		"workflow_id":         runRow.WorkflowID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delegate.URL, bytes.NewReader(triggerBody))
	if err != nil {
		return "", 0, "", &rollbackDispatchError{
			status: http.StatusBadGateway, code: "rollback_dispatch_failed",
			message: "building the webhook rollback request failed",
			details: map[string]any{"error": err.Error(), "url": delegate.URL},
		}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := deployHTTPClient.Do(req)
	if err != nil {
		return "", 0, "", &rollbackDispatchError{
			status: http.StatusBadGateway, code: "rollback_dispatch_failed",
			message: "webhook rollback POST failed",
			details: map[string]any{"error": err.Error(), "url": delegate.URL},
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, "", &rollbackDispatchError{
			status: http.StatusBadGateway, code: "rollback_dispatch_failed",
			message: "webhook rollback POST returned a non-2xx status",
			details: map[string]any{"status": resp.StatusCode, "url": delegate.URL},
		}
	}
	// The webhook delegate URL is the external handle; there is no GHA run id.
	return spec.DelegateTargetWebhook, 0, delegate.URL, nil
}

// deployStageForRun returns the run's deploy stage, or nil when the run has
// none. The deploy stage type is unique within a workflow in v0, so the first
// match is authoritative.
func (s *Server) deployStageForRun(ctx context.Context, runID uuid.UUID) (*run.Stage, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, st := range stages {
		if st.Type == run.StageTypeDeploy {
			return st, nil
		}
	}
	return nil, nil
}

// deployDelegateForRun reads the run's cached workflow spec and returns the
// delegating deploy stage's executor.delegate config. Unlike slice-1's
// resolveDeployDelegate it never mutates stage state — the rollback handler maps
// the error to an HTTP response, not a stage failure.
func deployDelegateForRun(runRow *run.Run) (*spec.DelegateConfig, error) {
	if len(runRow.WorkflowSpec) == 0 {
		return nil, errors.New("run carries no cached workflow spec")
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return nil, fmt.Errorf("cached workflow spec does not parse: %w", err)
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return nil, fmt.Errorf("run's workflow %q not in cached spec", runRow.WorkflowID)
	}
	for _, st := range wf.Stages {
		if st.Type == spec.StageTypeDeploy && st.Executor.Delegate != nil {
			return st.Executor.Delegate, nil
		}
	}
	return nil, errors.New("no delegating deploy stage in cached spec")
}

// rollbackGitRef resolves the git ref the rollback workflow_dispatch targets:
// the delegate's explicit git_ref, or "main" when the spec pins none (matching
// slice-1's trigger default).
func rollbackGitRef(delegate *spec.DelegateConfig) string {
	if delegate.GitRef != "" {
		return delegate.GitRef
	}
	return "main"
}
