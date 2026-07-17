package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// categoryDeploymentDispatchFailed records that the delegating deploy stage
// could NOT fire its external pipeline (DispatchWorkflow / webhook POST
// errored, or the delegate config was unusable). Paired with a category-C
// FailStage so a deploy that cannot trigger fails loudly rather than silently
// parking (#1386 / E23.6). Defined here (slice 1) rather than in deployment.go
// — it is an audit category, not an issue-comment surface, so it needs no
// docs/issue-comment-surfaces.md entry.
const categoryDeploymentDispatchFailed = "deployment_dispatch_failed"

// deployHTTPClient is the outbound client for the webhook delegate target. A
// dedicated client (not http.DefaultClient) bounds the POST and keeps the
// trigger's outbound surface explicit.
var deployHTTPClient = &http.Client{Timeout: 30 * time.Second}

// triggerDeploy fires the external delegating pipeline for an approved+dispatched
// deploy stage and parks it at awaiting_deployment (#1386 / E23.6, ADR-038).
//
// Called from advanceForDecision once an approved deploy stage has transitioned
// awaiting_deploy_approval → dispatched. It reads the stage's executor.delegate
// config from the run's cached workflow spec and, by target:
//
//   - github_actions — DispatchWorkflow (workflow_dispatch) carrying the
//     Fishhawk correlation token (fishhawk_run_id / fishhawk_stage_id) as
//     inputs, then best-effort resolves the resulting run id (the dispatch
//     endpoint returns 204 with no body) via ResolveDispatchedRun.
//   - webhook — POST a trigger payload to delegate.url.
//
// On a successful trigger it records the external run handle into the
// deployment_dispatched audit payload (so the slice-2 reconciler can read it
// back) and transitions dispatched → running → awaiting_deployment. On a trigger
// ERROR it writes a deployment_dispatch_failed audit and fails the stage
// category C — never a silent park.
//
// Returns the resulting stage (awaiting_deployment on success, failed on a
// trigger error) and an error ONLY for an internal repository failure the HTTP
// layer should surface as 500. The dispatch itself happens on the approval
// request path; the deploy gate already performs network I/O there, so this adds
// no new blocking posture.
//
// NOT-WIRED POSTURE: a github_actions target with no GitHub client configured
// (cfg.GitHub == nil) is the demo/un-wired backend, mirroring
// orchestrator.dispatchViaWorkflow — it WARN-logs and leaves the stage at
// dispatched rather than failing it. A genuine dispatch error (GitHub returned
// non-204) is distinct and DOES fail the stage.
func (s *Server) triggerDeploy(ctx context.Context, stage *run.Stage) (*run.Stage, error) {
	if s.cfg.RunRepo == nil {
		return stage, errors.New("deploy trigger requires a run repository")
	}

	delegate, runRow, failed, err := s.resolveDeployDelegate(ctx, stage)
	if delegate == nil {
		// resolveDeployDelegate already failed the stage + audited; propagate
		// its (failed-stage, err) verbatim.
		return failed, err
	}

	switch delegate.Target {
	case spec.DelegateTargetGitHubActions:
		return s.triggerDeployGitHubActions(ctx, stage, runRow, delegate)
	case spec.DelegateTargetWebhook:
		return s.triggerDeployWebhook(ctx, stage, runRow, delegate)
	default:
		return s.failDeployTrigger(ctx, stage,
			fmt.Sprintf("deploy delegate target %q is not supported", delegate.Target),
			map[string]any{"target": delegate.Target})
	}
}

// resolveDeployDelegate reads the deploy stage's executor.delegate config from
// the run's cached workflow spec. On any can't-resolve branch it fails the stage
// category C (the spec was already parsed at the pre-flight gate, so a failure
// here is an infrastructure-class surprise) and returns a nil delegate alongside
// the failed stage + error for the caller to propagate. On success it returns
// the delegate + run with a nil stage/error.
func (s *Server) resolveDeployDelegate(ctx context.Context, stage *run.Stage) (*spec.DelegateConfig, *run.Run, *run.Stage, error) {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		failed, ferr := s.failDeployTrigger(ctx, stage, "deploy trigger: run lookup failed",
			map[string]any{"error": err.Error()})
		return nil, nil, failed, ferr
	}
	if len(runRow.WorkflowSpec) == 0 {
		failed, ferr := s.failDeployTrigger(ctx, stage, "deploy trigger: run carries no cached workflow spec", nil)
		return nil, nil, failed, ferr
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		failed, ferr := s.failDeployTrigger(ctx, stage, "deploy trigger: cached workflow spec does not parse",
			map[string]any{"error": err.Error()})
		return nil, nil, failed, ferr
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		failed, ferr := s.failDeployTrigger(ctx, stage, "deploy trigger: run's workflow not in cached spec",
			map[string]any{"workflow_id": runRow.WorkflowID})
		return nil, nil, failed, ferr
	}
	for _, st := range wf.Stages {
		if st.Type == spec.StageTypeDeploy && st.Executor.Delegate != nil {
			return st.Executor.Delegate, runRow, nil, nil
		}
	}
	failed, ferr := s.failDeployTrigger(ctx, stage, "deploy trigger: no delegating deploy stage in cached spec", nil)
	return nil, nil, failed, ferr
}

// triggerDeployGitHubActions dispatches the customer's deploy workflow via
// workflow_dispatch and parks the stage at awaiting_deployment. The correlation
// token rides the dispatch INPUTS (the deploy workflow must declare
// fishhawk_run_id / fishhawk_stage_id inputs) so the reconciler can match the
// resulting run unambiguously (#1386 binding condition 1).
func (s *Server) triggerDeployGitHubActions(ctx context.Context, stage *run.Stage, runRow *run.Run, delegate *spec.DelegateConfig) (*run.Stage, error) {
	if delegate.WorkflowRef == "" {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: github_actions delegate is missing workflow_ref", nil)
	}
	if s.cfg.GitHub == nil {
		// Un-wired/demo backend — mirror orchestrator.dispatchViaWorkflow: WARN
		// and leave the stage at dispatched rather than failing it. A wired
		// production backend always has a GitHub client.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"deploy trigger: GitHub not configured; leaving deploy stage at dispatched",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()))
		return stage, nil
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: run has no installation_id; cannot dispatch the deploy workflow", nil)
	}
	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		return s.failDeployTrigger(ctx, stage,
			fmt.Sprintf("deploy trigger: %v", err), map[string]any{"repo": runRow.Repo})
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)

	branch := delegate.GitRef
	if branch == "" {
		// The deploy targets the merged change; default to the repo's default
		// branch when the spec pins no explicit git_ref.
		branch = "main"
	}
	correlation := map[string]string{
		"fishhawk_run_id":   stage.RunID.String(),
		"fishhawk_stage_id": stage.ID.String(),
	}
	dispatchedAt := time.Now().UTC()

	if err := s.cfg.GitHub.DispatchWorkflow(ctx, scope, repo,
		delegate.WorkflowRef, branch, githubclient.DispatchInputs(correlation)); err != nil {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: workflow_dispatch failed",
			map[string]any{"error": err.Error(), "workflow_ref": delegate.WorkflowRef, "git_ref": branch})
	}

	// Best-effort run-id resolution. A failure or an indeterminate result is NOT
	// a trigger failure — the pipeline IS running; the reconciler re-resolves by
	// the correlation token + dispatched_at window stored in the audit payload.
	var ghaRunID int64
	var externalURL string
	resolved, rerr := s.cfg.GitHub.ResolveDispatchedRun(ctx, scope, repo, branch, correlation, dispatchedAt.Add(-1*time.Minute))
	switch {
	case rerr != nil:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"deploy trigger: dispatched-run resolution errored; reconciler will re-resolve",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", rerr.Error()))
	case resolved == nil:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"deploy trigger: dispatched run not yet resolvable; reconciler will re-resolve",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()))
	default:
		ghaRunID = resolved.ID
		externalURL = resolved.HTMLURL
	}

	payload := map[string]any{
		"run_id":           stage.RunID.String(),
		"stage_id":         stage.ID.String(),
		"target":           spec.DelegateTargetGitHubActions,
		"workflow_ref":     delegate.WorkflowRef,
		"git_ref":          branch,
		"dispatched_at":    dispatchedAt.Format(time.RFC3339),
		"gha_run_id":       ghaRunID,
		"external_run_url": externalURL,
	}
	return s.recordDispatchAndPark(ctx, stage, payload)
}

// triggerDeployWebhook POSTs the deploy trigger to the delegate's URL and parks
// the stage at awaiting_deployment. The external webhook-driven pipeline reports
// its terminal outcome by calling back into POST /v0/runs/{run_id}/deployment
// (#1395) — the reconciler does NOT poll webhook targets (slice 2).
func (s *Server) triggerDeployWebhook(ctx context.Context, stage *run.Stage, runRow *run.Run, delegate *spec.DelegateConfig) (*run.Stage, error) {
	if delegate.URL == "" {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: webhook delegate is missing url", nil)
	}
	dispatchedAt := time.Now().UTC()
	triggerBody, _ := json.Marshal(map[string]any{
		"fishhawk_run_id":   stage.RunID.String(),
		"fishhawk_stage_id": stage.ID.String(),
		"repo":              runRow.Repo,
		"workflow_id":       runRow.WorkflowID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delegate.URL, bytes.NewReader(triggerBody))
	if err != nil {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: building webhook request failed",
			map[string]any{"error": err.Error(), "url": delegate.URL})
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := deployHTTPClient.Do(req)
	if err != nil {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: webhook POST failed",
			map[string]any{"error": err.Error(), "url": delegate.URL})
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: webhook POST returned a non-2xx status",
			map[string]any{"status": resp.StatusCode, "url": delegate.URL})
	}

	payload := map[string]any{
		"run_id":        stage.RunID.String(),
		"stage_id":      stage.ID.String(),
		"target":        spec.DelegateTargetWebhook,
		"url":           delegate.URL,
		"dispatched_at": dispatchedAt.Format(time.RFC3339),
	}
	return s.recordDispatchAndPark(ctx, stage, payload)
}

// recordDispatchAndPark writes the deployment_dispatched audit entry carrying
// the external run handle, then transitions the stage dispatched → running →
// awaiting_deployment. A failure to write the audit is fatal to the trigger (the
// reconciler reads the handle from that entry — a dispatched-but-unrecorded run
// would be unresolvable), so it fails the stage rather than parking blind.
func (s *Server) recordDispatchAndPark(ctx context.Context, stage *run.Stage, payload map[string]any) (*run.Stage, error) {
	raw, _ := json.Marshal(payload)
	if s.cfg.AuditRepo != nil {
		systemKind := audit.ActorKind("system")
		if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     stage.RunID,
			StageID:   &stage.ID,
			Timestamp: time.Now().UTC(),
			Category:  CategoryDeploymentDispatched,
			ActorKind: &systemKind,
			Payload:   raw,
		}); err != nil {
			return s.failDeployTrigger(ctx, stage,
				"deploy trigger: recording the deployment_dispatched handle failed",
				map[string]any{"error": err.Error()})
		}
	}

	if _, err := s.cfg.RunRepo.TransitionStage(ctx, stage.ID, run.StageStateRunning, nil); err != nil {
		return stage, fmt.Errorf("deploy trigger: dispatched → running: %w", err)
	}
	parked, err := s.cfg.RunRepo.TransitionStage(ctx, stage.ID, run.StageStateAwaitingDeployment, nil)
	if err != nil {
		return stage, fmt.Errorf("deploy trigger: running → awaiting_deployment: %w", err)
	}
	return parked, nil
}

// failDeployTrigger writes a deployment_dispatch_failed audit (system actor) and
// fails the stage category C. Best-effort audit: a logged append failure never
// suppresses the FailStage. Returns the failed stage (or, if FailStage itself
// errors, the original stage + a wrapped error for the 500 path).
func (s *Server) failDeployTrigger(ctx context.Context, stage *run.Stage, reason string, details map[string]any) (*run.Stage, error) {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "deploy trigger failed",
		slog.String("run_id", stage.RunID.String()),
		slog.String("stage_id", stage.ID.String()),
		slog.String("reason", reason))

	if s.cfg.AuditRepo != nil {
		if details == nil {
			details = map[string]any{}
		}
		details["stage_id"] = stage.ID.String()
		details["reason"] = reason
		payload, _ := json.Marshal(details)
		systemKind := audit.ActorKind("system")
		if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     stage.RunID,
			StageID:   &stage.ID,
			Timestamp: time.Now().UTC(),
			Category:  categoryDeploymentDispatchFailed,
			ActorKind: &systemKind,
			Payload:   payload,
		}); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"deploy trigger: append deployment_dispatch_failed audit failed",
				slog.String("run_id", stage.RunID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	failed, err := run.FailStage(ctx, s.cfg.RunRepo, stage.ID, run.FailureC, reason)
	if err != nil {
		return stage, fmt.Errorf("deploy trigger: failing stage: %w", err)
	}
	return failed, nil
}

// parseRepoRef splits "owner/name" into a forge.RepoRef. Local to the
// server package (orchestrator.parseRepo is unexported); a shared helper is a
// v0.x cleanup.
func parseRepoRef(s string) (forge.RepoRef, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			if i == 0 || i == len(s)-1 {
				return forge.RepoRef{}, fmt.Errorf("malformed repo %q", s)
			}
			return forge.RepoRef{Owner: s[:i], Name: s[i+1:]}, nil
		}
	}
	return forge.RepoRef{}, fmt.Errorf("malformed repo %q", s)
}
