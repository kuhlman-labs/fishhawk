package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// registerRoutes wires every endpoint onto mux. Method-aware patterns
// require Go 1.22+ ServeMux. Add new routes here as handlers land
// per docs/api/v0.openapi.yaml.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	// Run/stage/concern routes are wrapped with the tiered account-ownership
	// middleware (ADR-057 / E44.5, #1829): readAccess (GET run/stage/gate
	// views — ownership only), memberWrite (operator-decision writes), and
	// adminWrite (destructive/admin sub-actions). The wrapper resolves the
	// route's run WITH its account and enforces caller.AccountID ==
	// run.AccountID (untenanted run = allowed), plus cookie role-bounding on
	// the write tiers. It falls through to the handler unchanged when the run
	// can't be resolved, so 503/400/404 surfaces are untouched. Non-run routes
	// below are left as-is. The tier of each write route encodes the
	// operator's admin-vs-member founder decision, visible here for review.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v0/runs", s.handleListRuns)
	mux.HandleFunc("POST /v0/runs", s.handleCreateRun)
	mux.HandleFunc("GET /v0/runs/{run_id}", s.requireRunAccount(readAccess, s.handleGetRun))
	mux.HandleFunc("POST /v0/runs/{run_id}/cancel", s.requireRunAccount(adminWrite, s.handleCancelRun))
	mux.HandleFunc("POST /v0/runs/{run_id}/recover", s.requireRunAccount(adminWrite, s.handleRecoverRun))
	mux.HandleFunc("POST /v0/runs/{run_id}/redrive", s.requireRunAccount(adminWrite, s.handleRedriveChild))
	mux.HandleFunc("POST /v0/runs/{run_id}/revive", s.requireRunAccount(adminWrite, s.handleReviveRun))
	mux.HandleFunc("POST /v0/runs/{run_id}/reset-branch", s.requireRunAccount(adminWrite, s.handleResetRunBranch))
	mux.HandleFunc("POST /v0/runs/{run_id}/consolidate", s.requireRunAccount(memberWrite, s.handleConsolidateRun))
	mux.HandleFunc("POST /v0/runs/{run_id}/integrate-wave", s.requireRunAccount(memberWrite, s.handleIntegrateWave))
	mux.HandleFunc("POST /v0/runs/{run_id}/vouch-commit", s.requireRunAccount(memberWrite, s.handleVouchCommit))
	mux.HandleFunc("POST /v0/runs/{run_id}/merge", s.requireRunAccount(memberWrite, s.handleMergeRun))
	mux.HandleFunc("POST /v0/runs/{run_id}/auto-drive", s.requireRunAccount(memberWrite, s.handleAutoDrive))
	mux.HandleFunc("POST /v0/runs/{run_id}/auto-drive/acts", s.requireRunAccount(memberWrite, s.handleAutoDriveRecordAct))
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", s.requireRunAccount(readAccess, s.handleListRunStages))
	mux.HandleFunc("GET /v0/runs/{run_id}/stages/{stage_id}", s.requireRunAccount(readAccess, s.handleGetRunStage))
	mux.HandleFunc("POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure", s.requireRunAccount(adminWrite, s.handleReapStageFailure))
	mux.HandleFunc("POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch", s.requireRunAccount(memberWrite, s.handleHostDispatchStage))
	mux.HandleFunc("GET /v0/runs/{run_id}/audit", s.requireRunAccount(readAccess, s.handleListRunAudit))
	mux.HandleFunc("GET /v0/runs/{run_id}/gate-view", s.requireRunAccount(readAccess, s.handleGetRunGateView))
	mux.HandleFunc("GET /v0/runs/{run_id}/budget", s.requireRunAccount(readAccess, s.handleGetRunBudget))
	mux.HandleFunc("GET /v0/runs/{run_id}/cache-efficiency", s.requireRunAccount(readAccess, s.handleGetRunCacheEfficiency))
	mux.HandleFunc("GET /v0/runs/{run_id}/cost", s.requireRunAccount(readAccess, s.handleGetRunCost))
	mux.HandleFunc("GET /v0/runs/{run_id}/latency", s.requireRunAccount(readAccess, s.handleGetRunLatency))
	mux.HandleFunc("GET /v0/runs/{run_id}/diagnostics", s.requireRunAccount(readAccess, s.handleGetRunDiagnostics))
	mux.HandleFunc("POST /v0/runs/{run_id}/product-reports", s.requireRunAccount(memberWrite, s.handleFileProductReport))
	mux.HandleFunc("GET /v0/runs/{run_id}/status-comment", s.requireRunAccount(readAccess, s.handleGetStatusComment))
	mux.HandleFunc("POST /v0/runs/{run_id}/status-comment", s.requireRunAccount(memberWrite, s.handlePostStatusComment))
	mux.HandleFunc("GET /v0/audit", s.handleListGlobalAudit)
	mux.HandleFunc("GET /v0/audit/export", s.handleAuditExport)
	mux.HandleFunc("GET /v0/audit/export.csv", s.handleAuditExportCSV)
	mux.HandleFunc("GET /v0/reports/agent-changes", s.handleAgentChangesReport)
	mux.HandleFunc("GET /v0/reports/agent-changes.md", s.handleAgentChangesReportMarkdown)
	mux.HandleFunc("GET /v0/releases/notes/preview", s.handleReleaseNotesPreview)
	mux.HandleFunc("POST /v0/releases/notes", s.handleReleaseNotesPersist)
	mux.HandleFunc("POST /v0/releases/cut", s.handleReleaseCut)
	mux.HandleFunc("POST /v0/releases/publish", s.handleReleasePublish)
	mux.HandleFunc("GET /v0/campaigns", s.handleListCampaigns)
	mux.HandleFunc("POST /v0/campaigns", s.handleCreateCampaign)
	mux.HandleFunc("GET /v0/campaigns/{campaign_id}", s.handleGetCampaign)
	mux.HandleFunc("GET /v0/campaigns/{campaign_id}/items", s.handleListCampaignItems)
	mux.HandleFunc("GET /v0/campaigns/{campaign_id}/status", s.handleGetCampaignStatus)
	mux.HandleFunc("POST /v0/campaigns/{campaign_id}/runs", s.handleStartCampaignItemRun)
	mux.HandleFunc("POST /v0/campaigns/{campaign_id}/resume", s.handleResumeCampaign)
	mux.HandleFunc("POST /v0/refinement/sessions", s.handleCreateRefinementSession)
	mux.HandleFunc("GET /v0/refinement/sessions/{session_id}", s.handleGetRefinementSession)
	mux.HandleFunc("PATCH /v0/refinement/sessions/{session_id}/draft", s.handlePatchRefinementDraft)
	mux.HandleFunc("POST /v0/refinement/sessions/{session_id}/decision", s.handleDecideRefinementSession)
	mux.HandleFunc("POST /v0/refinement/sessions/{session_id}/file", s.handleFileRefinementSession)
	mux.HandleFunc("POST /v0/work-items", s.handleFileWorkItem)
	mux.HandleFunc("GET /v0/calibration", s.handleGetCalibration)
	mux.HandleFunc("GET /v0/acceptance-triage/stats", s.handleGetAcceptanceTriageStats)
	mux.HandleFunc("POST /v0/runs/{run_id}/signing-key", s.requireRunAccount(adminWrite, s.handleIssueSigningKey))
	mux.HandleFunc("POST /v0/runs/{run_id}/trace", s.requireRunAccount(memberWrite, s.handleShipTrace))
	mux.HandleFunc("POST /v0/runs/{run_id}/plan", s.requireRunAccount(memberWrite, s.handleShipPlan))
	mux.HandleFunc("POST /v0/runs/{run_id}/pull-request", s.requireRunAccount(memberWrite, s.handleShipPullRequest))
	mux.HandleFunc("POST /v0/runs/{run_id}/deployment", s.requireRunAccount(memberWrite, s.handleShipDeployment))
	mux.HandleFunc("POST /v0/runs/{run_id}/deployment/rollback", s.requireRunAccount(adminWrite, s.handleRollbackDeployment))
	mux.HandleFunc("POST /v0/runs/{run_id}/acceptance", s.requireRunAccount(memberWrite, s.handleShipAcceptance))
	mux.HandleFunc("POST /v0/runs/{run_id}/installation-token", s.requireRunAccount(adminWrite, s.handleIssueInstallationToken))
	mux.HandleFunc("POST /v0/runs/{run_id}/mcp-token", s.requireRunAccount(adminWrite, s.handleIssueMCPToken))
	mux.HandleFunc("POST /v0/runs/{run_id}/scope-amendments", s.requireRunAccount(memberWrite, s.handleRequestScopeAmendment))
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", s.requireRunAccount(readAccess, s.handleListScopeAmendments))
	mux.HandleFunc("POST /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision", s.requireRunAccount(memberWrite, s.handleDecideScopeAmendment))
	mux.HandleFunc("POST /v0/runs/{run_id}/scope-completeness/decision", s.requireRunAccount(memberWrite, s.handleDecideScopeCompleteness))
	mux.HandleFunc("GET /v0/stages/{stage_id}", s.requireStageAccount(readAccess, s.handleGetStage))
	mux.HandleFunc("GET /v0/stages/{stage_id}/artifacts", s.requireStageAccount(readAccess, s.handleListStageArtifacts))
	mux.HandleFunc("GET /v0/stages/{stage_id}/prompt", s.requireStageAccount(readAccess, s.handleGetStagePrompt))
	mux.HandleFunc("GET /v0/stages/{stage_id}/prompt-render", s.requireStageAccount(readAccess, s.handleGetStagePromptRender))
	mux.HandleFunc("GET /v0/stages/{stage_id}/trace", s.requireStageAccount(readAccess, s.handleGetStageTrace))
	mux.HandleFunc("GET /v0/stages/{stage_id}/checks", s.requireStageAccount(readAccess, s.handleListStageChecks))
	mux.HandleFunc("POST /v0/stages/{stage_id}/approvals", s.requireStageAccount(memberWrite, s.handleSubmitApproval))
	mux.HandleFunc("POST /v0/stages/{stage_id}/clarification", s.requireStageAccount(memberWrite, s.handleAnswerClarification))
	mux.HandleFunc("POST /v0/stages/{stage_id}/revise", s.requireStageAccount(memberWrite, s.handleRevisePlan))
	mux.HandleFunc("POST /v0/stages/{stage_id}/retry", s.requireStageAccount(memberWrite, s.handleRetryStage))
	mux.HandleFunc("POST /v0/stages/{stage_id}/acceptance-admission", s.requireStageAccount(memberWrite, s.handleAcceptanceAdmission))
	mux.HandleFunc("POST /v0/stages/{stage_id}/fixup", s.requireStageAccount(memberWrite, s.handleFixupStage))
	mux.HandleFunc("POST /v0/concerns/{concern_id}/waive", s.requireConcernAccount(memberWrite, s.handleWaiveConcern))
	mux.HandleFunc("POST /v0/concerns/{concern_id}/defer", s.requireConcernAccount(memberWrite, s.handleDeferConcern))
	mux.HandleFunc("GET /v0/artifacts/{artifact_id}", s.handleGetArtifact)
	mux.HandleFunc("GET /v0/tokens", s.handleListTokens)
	mux.HandleFunc("POST /v0/tokens", s.handleCreateToken)
	mux.HandleFunc("GET /v0/tokens/login", s.handleTokenLoginDiscovery)
	mux.HandleFunc("POST /v0/tokens/login", s.handleTokenLoginMint)
	mux.HandleFunc("DELETE /v0/tokens/{token_id}", s.handleRevokeToken)
	mux.HandleFunc("GET /v0/auth/github/login", s.handleGitHubLogin)
	mux.HandleFunc("GET /v0/auth/github/callback", s.handleGitHubCallback)
	mux.HandleFunc("GET /v0/auth/github/manifest-flow-start", s.handleGitHubManifestFlowStart)
	mux.HandleFunc("GET /v0/auth/github/manifest-callback", s.handleGitHubManifestCallback)
	mux.HandleFunc("GET /v0/auth/me", s.handleGetMe)
	mux.HandleFunc("GET /v0/onboarding/readiness", s.handleGetOnboardingReadiness)
	mux.HandleFunc("POST /v0/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /webhooks/github", s.handleWebhook)
	mux.HandleFunc("POST /webhooks/gitlab", s.handleWebhookGitLab)
}

type healthResponse struct {
	Status           string            `json:"status"`
	Version          string            `json:"version"`
	GitSHA           string            `json:"git_sha"`
	MinRunnerVersion string            `json:"min_runner_version"`
	Schemas          map[string]string `json:"schemas"`
	StartNonce       string            `json:"start_nonce,omitempty"`
}

// handleHealth answers liveness probes with a small JSON payload that
// also exposes the running version. Operators rely on the version
// field to confirm a deploy reached this instance. start_nonce echoes
// Config.StartNonce verbatim (omitted when unset) so scripts/dev can
// prove the listener on the port is the daemon it spawned (#1018).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		Status:           "ok",
		Version:          version.Version,
		GitSHA:           version.GitSHA,
		MinRunnerVersion: version.MinRunnerVersion,
		Schemas: map[string]string{
			"plan-standard-v1": plan.EmbeddedSchemaHash(),
			"workflow-v0":      spec.EmbeddedSchemaHash(),
			"workflow-v1":      spec.EmbeddedSchemaHashV1(),
		},
		StartNonce: s.cfg.StartNonce,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError, "encode health response",
			slog.String("error", err.Error()),
		)
	}
}
