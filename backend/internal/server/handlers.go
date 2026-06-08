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
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v0/runs", s.handleListRuns)
	mux.HandleFunc("POST /v0/runs", s.handleCreateRun)
	mux.HandleFunc("GET /v0/runs/{run_id}", s.handleGetRun)
	mux.HandleFunc("POST /v0/runs/{run_id}/cancel", s.handleCancelRun)
	mux.HandleFunc("POST /v0/runs/{run_id}/redrive", s.handleRedriveChild)
	mux.HandleFunc("POST /v0/runs/{run_id}/reset-branch", s.handleResetRunBranch)
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", s.handleListRunStages)
	mux.HandleFunc("GET /v0/runs/{run_id}/audit", s.handleListRunAudit)
	mux.HandleFunc("GET /v0/runs/{run_id}/budget", s.handleGetRunBudget)
	mux.HandleFunc("GET /v0/runs/{run_id}/status-comment", s.handleGetStatusComment)
	mux.HandleFunc("POST /v0/runs/{run_id}/status-comment", s.handlePostStatusComment)
	mux.HandleFunc("GET /v0/audit", s.handleListGlobalAudit)
	mux.HandleFunc("GET /v0/calibration", s.handleGetCalibration)
	mux.HandleFunc("POST /v0/runs/{run_id}/signing-key", s.handleIssueSigningKey)
	mux.HandleFunc("POST /v0/runs/{run_id}/trace", s.handleShipTrace)
	mux.HandleFunc("POST /v0/runs/{run_id}/plan", s.handleShipPlan)
	mux.HandleFunc("POST /v0/runs/{run_id}/pull-request", s.handleShipPullRequest)
	mux.HandleFunc("POST /v0/runs/{run_id}/installation-token", s.handleIssueInstallationToken)
	mux.HandleFunc("POST /v0/runs/{run_id}/mcp-token", s.handleIssueMCPToken)
	mux.HandleFunc("GET /v0/stages/{stage_id}", s.handleGetStage)
	mux.HandleFunc("GET /v0/stages/{stage_id}/artifacts", s.handleListStageArtifacts)
	mux.HandleFunc("GET /v0/stages/{stage_id}/prompt", s.handleGetStagePrompt)
	mux.HandleFunc("GET /v0/stages/{stage_id}/prompt-render", s.handleGetStagePromptRender)
	mux.HandleFunc("GET /v0/stages/{stage_id}/trace", s.handleGetStageTrace)
	mux.HandleFunc("GET /v0/stages/{stage_id}/checks", s.handleListStageChecks)
	mux.HandleFunc("POST /v0/stages/{stage_id}/approvals", s.handleSubmitApproval)
	mux.HandleFunc("POST /v0/stages/{stage_id}/retry", s.handleRetryStage)
	mux.HandleFunc("POST /v0/stages/{stage_id}/fixup", s.handleFixupStage)
	mux.HandleFunc("GET /v0/artifacts/{artifact_id}", s.handleGetArtifact)
	mux.HandleFunc("GET /v0/tokens", s.handleListTokens)
	mux.HandleFunc("POST /v0/tokens", s.handleCreateToken)
	mux.HandleFunc("DELETE /v0/tokens/{token_id}", s.handleRevokeToken)
	mux.HandleFunc("GET /v0/auth/github/login", s.handleGitHubLogin)
	mux.HandleFunc("GET /v0/auth/github/callback", s.handleGitHubCallback)
	mux.HandleFunc("GET /v0/auth/github/manifest-flow-start", s.handleGitHubManifestFlowStart)
	mux.HandleFunc("GET /v0/auth/github/manifest-callback", s.handleGitHubManifestCallback)
	mux.HandleFunc("GET /v0/auth/me", s.handleGetMe)
	mux.HandleFunc("POST /v0/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /webhooks/github", s.handleWebhook)
}

type healthResponse struct {
	Status           string            `json:"status"`
	Version          string            `json:"version"`
	GitSHA           string            `json:"git_sha"`
	MinRunnerVersion string            `json:"min_runner_version"`
	Schemas          map[string]string `json:"schemas"`
}

// handleHealth answers liveness probes with a small JSON payload that
// also exposes the running version. Operators rely on the version
// field to confirm a deploy reached this instance.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		Status:           "ok",
		Version:          version.Version,
		GitSHA:           version.GitSHA,
		MinRunnerVersion: version.MinRunnerVersion,
		Schemas: map[string]string{
			"plan-standard-v1": plan.EmbeddedSchemaHash(),
			"workflow-v0":      spec.EmbeddedSchemaHash(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError, "encode health response",
			slog.String("error", err.Error()),
		)
	}
}
