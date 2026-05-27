package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// promptResponse is the 200 body for GET /v0/stages/{stage_id}/prompt.
// Wrapped in a JSON object so future fields (template version,
// hash, redaction notes) can be added without breaking the runner.
type promptResponse struct {
	StageID             string `json:"stage_id"`
	StageType           string `json:"stage_type"`
	Prompt              string `json:"prompt"`
	PromptHash          string `json:"prompt_hash"`
	AgentTimeoutSeconds int    `json:"agent_timeout_seconds"`
	// DecomposedFromRunID is the parent run's ID when this run is a
	// decomposed child. Absent for standalone runs. Runners use this to
	// route decomposed children onto a shared parent branch.
	DecomposedFromRunID  string `json:"decomposed_from_run_id,omitempty"`
	VerifyCommand        string `json:"verify_command,omitempty"`
	VerifyTimeoutSeconds int    `json:"verify_timeout_seconds,omitempty"`
	// MinRunnerVersion is the minimum runner version the backend requires.
	// Runners that are older than this should exit with a version-skew error
	// rather than proceeding to invoke the agent.
	MinRunnerVersion string `json:"min_runner_version,omitempty"`
	// AgentSelfRetry is true when the workflow spec opts the stage into
	// ADR-023 runner-side self-retry on category-A/C failures.
	AgentSelfRetry bool `json:"agent_self_retry,omitempty"`
	// MaxRetriesSnapshot is the run's max_retries_snapshot at prompt-fetch
	// time. Together with RetryAttempt it lets the runner compute the
	// remaining self-retry budget without a separate API call.
	MaxRetriesSnapshot int `json:"max_retries_snapshot,omitempty"`
	// RetryAttempt is the run's current retry_attempt counter. 0 for
	// original runs; incremented by the backend on each auto-retry.
	RetryAttempt int `json:"retry_attempt,omitempty"`
}

// issueGetter is the slice of githubclient.Client the prompt
// handler consumes. Defining the interface in the server package
// lets tests substitute a stub without spinning up an httptest
// fake of api.github.com — *githubclient.Client satisfies it
// in production.
type issueGetter interface {
	GetIssue(ctx context.Context, installationID int64, repo githubclient.RepoRef, number int) (*githubclient.Issue, error)
}

// handleGetStagePrompt implements GET /v0/stages/{stage_id}/prompt.
//
// Auth is the same per-run signing-key signature used by the trace
// upload endpoint: the canonical message is sha256("prompt:" +
// stage_id), signed by the runner with the private half issued at
// signing-key time. Bound-to-stage scope keeps a leaked signature
// from being replayed against a different stage's prompt.
//
// Construction is server-side and pull-style (E3.12 design): the
// runner sees the constructed prompt rather than building it
// itself, so two replays of the same stage produce byte-identical
// prompts. Auditability of "what the agent was asked to do" is
// the load-bearing reason for that choice.
func (s *Server) handleGetStagePrompt(w http.ResponseWriter, r *http.Request) {
	github := s.issueGetter()
	if s.cfg.SigningRepo == nil || s.cfg.RunRepo == nil || github == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "prompt_unconfigured",
			"prompt construction requires signing, run, and GitHub repos to be configured", nil)
		return
	}

	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", map[string]any{"stage_id": stageID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get stage failed", map[string]any{"error": err.Error()})
		return
	}

	switch stage.State {
	case run.StageStateAwaitingApproval, run.StageStateAwaitingChildren,
		run.StageStateSucceeded, run.StageStateFailed, run.StageStateCancelled:
		s.writeError(w, r, http.StatusConflict, "stage_not_runnable",
			"stage is not in a runnable state",
			map[string]any{"current_state": string(stage.State), "stage_id": stageID.String()})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run for stage failed", map[string]any{"error": err.Error()})
		return
	}

	if !s.verifyPromptSignature(w, r, runRow.ID, stageID) {
		return
	}

	// Build the trigger context. For issue-style triggers we fetch
	// the issue from GitHub at request time so the prompt reflects
	// the latest title/body — the cost is one API call per stage
	// dispatch, which is acceptable. If the issue can't be fetched
	// (e.g., deleted, App lacks access) we still return a prompt
	// rather than failing — the agent will work without it, just
	// with less context.
	trigger := prompt.Trigger{
		Source: string(runRow.TriggerSource),
		Repo:   runRow.Repo,
	}
	if runRow.TriggerRef != nil {
		if number, ok := parseIssueRef(*runRow.TriggerRef); ok {
			trigger.IssueNumber = number
			s.fillIssueContext(r.Context(), github, runRow, number, &trigger)
		}
	}
	// Plan-as-contract (#223): for implement stages, the approved
	// plan is the binding instruction. Look up the run's
	// plan-stage's most-recent standard_v1 artifact and feed it
	// into the prompt builder. Missing plan → fall back to the
	// issue-only template and emit `plan_missing_for_implement` so
	// the audit log captures the gap.
	if stage.Type == run.StageTypeImplement {
		approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), runRow.ID)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"load approved plan failed", map[string]any{"error": err.Error()})
			return
		}
		if approvedPlan == nil {
			s.emitPlanMissingForImplement(r.Context(), runRow.ID, stage.ID)
		}
		trigger.ApprovedPlan = approvedPlan
		if approvedPlan != nil {
			budgetSecs := s.resolveAgentTimeout(r.Context(), runRow, run.StageTypeImplement)
			trigger.PredictionContext = &prompt.PredictionContext{
				PredictedMinutes:    approvedPlan.PredictedRuntimeMinutes,
				PredictedConfidence: string(approvedPlan.PredictedRuntimeConfidence),
				StageBudgetMinutes:  budgetSecs / 60,
			}
		}
		trigger.ScopeConstraint = s.resolveDecomposedScopeConstraint(r.Context(), runRow)
	}

	// Decompose-required hint: when the run's last plan approval was
	// rejected with --decompose, tell the agent it must populate
	// decomposition.sub_plans in the next plan attempt.
	if stage.Type == run.StageTypePlan {
		if s.loadLastDecomposeRejectionReason(r.Context(), runRow.ID) {
			trigger.DecomposeRequired = true
		}
		if hint, err := s.resolveCalibrationHint(r.Context(), runRow.WorkflowID); err != nil {
			slog.WarnContext(r.Context(), "calibration hint resolution failed", "error", err)
		} else {
			trigger.CalibrationHint = hint
		}
		if runRow.TriggerRef != nil {
			trigger.PriorRejectionFeedback = s.loadPriorRejectionFeedback(r.Context(), runRow.Repo, *runRow.TriggerRef, runRow.ID)
		}
	}

	trigger.PlanStageTimeout = time.Duration(s.resolveAgentTimeout(r.Context(), runRow, run.StageTypePlan)) * time.Second
	trigger.ImplementStageTimeout = time.Duration(s.resolveAgentTimeout(r.Context(), runRow, run.StageTypeImplement)) * time.Second

	text, err := prompt.Build(string(stage.Type), trigger)
	if err != nil {
		if errors.Is(err, prompt.ErrUnsupportedStage) {
			s.writeError(w, r, http.StatusNotImplemented, "unsupported_stage_type",
				"prompt construction not yet implemented for this stage type",
				map[string]any{"stage_type": string(stage.Type)})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"build prompt failed", map[string]any{"error": err.Error()})
		return
	}

	hash := signing.ComputeMessage([]byte(text))
	verifyCmd, verifyTimeoutSecs := s.resolveVerifyConfig(r.Context(), runRow, stage.Type)
	resp := promptResponse{
		StageID:              stageID.String(),
		StageType:            string(stage.Type),
		Prompt:               text,
		PromptHash:           hex.EncodeToString(hash),
		AgentTimeoutSeconds:  s.resolveAgentTimeout(r.Context(), runRow, stage.Type),
		VerifyCommand:        verifyCmd,
		VerifyTimeoutSeconds: verifyTimeoutSecs,
		MinRunnerVersion:     version.MinRunnerVersion,
		AgentSelfRetry:       s.resolveAgentSelfRetryForStage(r.Context(), runRow, stage.Type),
		MaxRetriesSnapshot:   runRow.MaxRetriesSnapshot,
		RetryAttempt:         runRow.RetryAttempt,
	}
	if runRow.DecomposedFrom != nil {
		resp.DecomposedFromRunID = runRow.DecomposedFrom.String()
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// handleGetStagePromptRender implements GET /v0/stages/{stage_id}/prompt-render.
//
// SPA-readable counterpart of handleGetStagePrompt: same response
// shape, same construction, but no X-Fishhawk-Signature requirement.
// The runner contract on the signature-authed path stays untouched.
//
// Read access tracks the existing stage/audit read endpoints — no
// auth gate at the handler level today; the surrounding middleware
// handles cookie/bearer resolution. Used by the implement-stage
// session view (#215) to show the user the deterministic prompt
// the agent received.
func (s *Server) handleGetStagePromptRender(w http.ResponseWriter, r *http.Request) {
	github := s.issueGetter()
	if s.cfg.RunRepo == nil || github == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "prompt_unconfigured",
			"prompt construction requires run repo and GitHub access to be configured", nil)
		return
	}

	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", map[string]any{"stage_id": stageID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get stage failed", map[string]any{"error": err.Error()})
		return
	}

	switch stage.State {
	case run.StageStateAwaitingApproval, run.StageStateAwaitingChildren,
		run.StageStateSucceeded, run.StageStateFailed, run.StageStateCancelled:
		s.writeError(w, r, http.StatusConflict, "stage_not_runnable",
			"stage is not in a runnable state",
			map[string]any{"current_state": string(stage.State), "stage_id": stageID.String()})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run for stage failed", map[string]any{"error": err.Error()})
		return
	}

	trigger := prompt.Trigger{
		Source: string(runRow.TriggerSource),
		Repo:   runRow.Repo,
	}
	if runRow.TriggerRef != nil {
		if number, ok := parseIssueRef(*runRow.TriggerRef); ok {
			trigger.IssueNumber = number
			s.fillIssueContext(r.Context(), github, runRow, number, &trigger)
		}
	}
	// Plan-as-contract (#223): for implement stages, the approved
	// plan is the binding instruction. Look up the run's
	// plan-stage's most-recent standard_v1 artifact and feed it
	// into the prompt builder. Missing plan → fall back to the
	// issue-only template and emit `plan_missing_for_implement` so
	// the audit log captures the gap.
	if stage.Type == run.StageTypeImplement {
		approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), runRow.ID)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"load approved plan failed", map[string]any{"error": err.Error()})
			return
		}
		if approvedPlan == nil {
			s.emitPlanMissingForImplement(r.Context(), runRow.ID, stage.ID)
		}
		trigger.ApprovedPlan = approvedPlan
		if approvedPlan != nil {
			budgetSecs := s.resolveAgentTimeout(r.Context(), runRow, run.StageTypeImplement)
			trigger.PredictionContext = &prompt.PredictionContext{
				PredictedMinutes:    approvedPlan.PredictedRuntimeMinutes,
				PredictedConfidence: string(approvedPlan.PredictedRuntimeConfidence),
				StageBudgetMinutes:  budgetSecs / 60,
			}
		}
		trigger.ScopeConstraint = s.resolveDecomposedScopeConstraint(r.Context(), runRow)
	}

	// Decompose-required hint: when the run's last plan approval was
	// rejected with --decompose, tell the agent it must populate
	// decomposition.sub_plans in the next plan attempt.
	if stage.Type == run.StageTypePlan {
		if s.loadLastDecomposeRejectionReason(r.Context(), runRow.ID) {
			trigger.DecomposeRequired = true
		}
		if hint, err := s.resolveCalibrationHint(r.Context(), runRow.WorkflowID); err != nil {
			slog.WarnContext(r.Context(), "calibration hint resolution failed", "error", err)
		} else {
			trigger.CalibrationHint = hint
		}
		if runRow.TriggerRef != nil {
			trigger.PriorRejectionFeedback = s.loadPriorRejectionFeedback(r.Context(), runRow.Repo, *runRow.TriggerRef, runRow.ID)
		}
	}

	trigger.PlanStageTimeout = time.Duration(s.resolveAgentTimeout(r.Context(), runRow, run.StageTypePlan)) * time.Second
	trigger.ImplementStageTimeout = time.Duration(s.resolveAgentTimeout(r.Context(), runRow, run.StageTypeImplement)) * time.Second

	text, err := prompt.Build(string(stage.Type), trigger)
	if err != nil {
		if errors.Is(err, prompt.ErrUnsupportedStage) {
			s.writeError(w, r, http.StatusNotImplemented, "unsupported_stage_type",
				"prompt construction not yet implemented for this stage type",
				map[string]any{"stage_type": string(stage.Type)})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"build prompt failed", map[string]any{"error": err.Error()})
		return
	}

	hash := signing.ComputeMessage([]byte(text))
	verifyCmd, verifyTimeoutSecs := s.resolveVerifyConfig(r.Context(), runRow, stage.Type)
	resp := promptResponse{
		StageID:              stageID.String(),
		StageType:            string(stage.Type),
		Prompt:               text,
		PromptHash:           hex.EncodeToString(hash),
		AgentTimeoutSeconds:  s.resolveAgentTimeout(r.Context(), runRow, stage.Type),
		VerifyCommand:        verifyCmd,
		VerifyTimeoutSeconds: verifyTimeoutSecs,
		MinRunnerVersion:     version.MinRunnerVersion,
		AgentSelfRetry:       s.resolveAgentSelfRetryForStage(r.Context(), runRow, stage.Type),
		MaxRetriesSnapshot:   runRow.MaxRetriesSnapshot,
		RetryAttempt:         runRow.RetryAttempt,
	}
	if runRow.DecomposedFrom != nil {
		resp.DecomposedFromRunID = runRow.DecomposedFrom.String()
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// verifyPromptSignature reads the X-Fishhawk-Signature header and
// validates it against sha256("prompt:" + stage_id) using the
// run's stored public key. Returns true on success; on failure
// writes the response and returns false so the caller short-circuits.
func (s *Server) verifyPromptSignature(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID) bool {
	sigHeader := r.Header.Get("X-Fishhawk-Signature")
	if sigHeader == "" {
		s.writeError(w, r, http.StatusUnauthorized, "signature_missing",
			"X-Fishhawk-Signature header is required", nil)
		return false
	}
	signature, err := hex.DecodeString(sigHeader)
	if err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
			"X-Fishhawk-Signature is not valid hex",
			map[string]any{"error": err.Error()})
		return false
	}

	message := promptCanonicalMessage(stageID)
	if err := s.cfg.SigningRepo.Verify(r.Context(), runID, message, signature); err != nil {
		switch {
		case errors.Is(err, signing.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "signing_key_not_found",
				"no signing key issued for this run", map[string]any{"run_id": runID.String()})
		case errors.Is(err, signing.ErrExpired):
			s.writeError(w, r, http.StatusUnauthorized, "signing_key_expired",
				"signing key TTL has passed", map[string]any{"run_id": runID.String()})
		case errors.Is(err, signing.ErrSignatureInvalid):
			s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
				"signature does not match the run's stored public key", nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"signature verification failed", map[string]any{"error": err.Error()})
		}
		return false
	}
	return true
}

// PromptCanonicalMessage exposes the canonical message the prompt
// endpoint signs over so the runner can derive the same bytes
// without re-implementing the format.
func PromptCanonicalMessage(stageID uuid.UUID) []byte {
	return promptCanonicalMessage(stageID)
}

func promptCanonicalMessage(stageID uuid.UUID) []byte {
	return signing.ComputeMessage([]byte("prompt:" + stageID.String()))
}

// loadApprovedPlanForRun returns the plan stage's most-recent
// kind=plan, schema_version=standard_v1 artifact for the run, decoded
// into a *plan.Plan. Returns (nil, nil) when no such artifact exists
// (race between plan upload and implement dispatch, or a manual run
// with no plan stage). The implement-stage prompt builder treats nil
// as "no plan available" and falls back to the issue-only template.
//
// CI-failure retry runs (#279 / E16) intentionally skip the plan
// stage — their implement stage is meant to re-run against the
// parent's already-approved plan. When the current run has no plan
// stage of its own, we walk ParentRunID upward until we find a run
// that does (or until the chain ends). The walk is capped at
// retryPlanChainDepth so a corrupt parent_run_id cycle can't loop
// forever.
//
// Errors are returned to the caller only when the underlying repo
// IO fails — a missing or malformed plan logs and yields nil so the
// prompt fetch stays robust against the kinds of mid-flight states
// the runner sees during re-tries.
func (s *Server) loadApprovedPlanForRun(ctx context.Context, runID uuid.UUID) (*plan.Plan, error) {
	if s.cfg.ArtifactRepo == nil || s.cfg.RunRepo == nil {
		return nil, nil
	}
	current := runID
	for depth := 0; depth < retryPlanChainDepth; depth++ {
		p, found, err := s.tryLoadPlanForRun(ctx, current)
		if err != nil {
			return nil, err
		}
		if found {
			return p, nil
		}
		// No plan stage on this run; walk to the parent.
		runRow, err := s.cfg.RunRepo.GetRun(ctx, current)
		if err != nil {
			return nil, fmt.Errorf("get run for parent walk: %w", err)
		}
		if runRow.ParentRunID == nil {
			return nil, nil
		}
		current = *runRow.ParentRunID
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parent-walk hit depth cap",
		slog.String("run_id", runID.String()),
		slog.Int("max_depth", retryPlanChainDepth))
	return nil, nil
}

// calibrationHintMinSamples is the minimum number of historical implement-
// stage samples required before the calibration hint is appended to the
// plan-stage prompt. Below this threshold the section is silently omitted.
const calibrationHintMinSamples = 5

// resolveCalibrationHint loads runtime_observed audit entries for the
// workflow, filters to implement-stage samples, and computes calibration
// statistics. Returns nil when the AuditRepo is unconfigured, when RunRepo
// is nil (can't resolve workflow_id per entry), or when the sample count
// is below calibrationHintMinSamples. Errors degrade gracefully — the
// caller logs at WARN and proceeds with a hint-free prompt.
func (s *Server) resolveCalibrationHint(ctx context.Context, workflowID string) (*prompt.CalibrationHint, error) {
	if s.cfg.AuditRepo == nil {
		return nil, nil
	}
	const runtimeObservedCategory = "runtime_observed"
	cat := runtimeObservedCategory
	entries, err := s.cfg.AuditRepo.ListAll(ctx, audit.ListAllParams{Category: &cat})
	if err != nil {
		return nil, fmt.Errorf("list runtime_observed entries: %w", err)
	}
	var samples []runtimeObservedPayload
	for _, e := range entries {
		var p runtimeObservedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.StageType != "implement" {
			continue
		}
		if e.RunID == nil || s.cfg.RunRepo == nil {
			continue
		}
		runRow, err := s.cfg.RunRepo.GetRun(ctx, *e.RunID)
		if err != nil || runRow.WorkflowID != workflowID {
			continue
		}
		samples = append(samples, p)
	}
	result := computeCalibration(workflowID, "implement", samples)
	if result.Samples < calibrationHintMinSamples {
		return nil, nil
	}
	bands := make(map[string]prompt.CalibrationBand, len(result.ConfidenceBandAccuracy))
	for level, b := range result.ConfidenceBandAccuracy {
		bands[level] = prompt.CalibrationBand{Samples: b.Samples, WithinScale: b.Within1p5x}
	}
	return &prompt.CalibrationHint{
		Samples:          result.Samples,
		CalibrationRatio: result.CalibrationRatio,
		ConfidenceBands:  bands,
	}, nil
}

// retryPlanChainDepth caps the parent-walk in loadApprovedPlanForRun.
// In practice an auto-retry chain is at most a handful of links
// (max_retries defaults to 1); 8 is generous and bounds a corrupt
// cycle without imposing on legitimate workflows.
const retryPlanChainDepth = 8

// tryLoadPlanForRun looks for a standard_v1 plan artifact on the
// single run identified by runID. Returns (plan, true, nil) on a
// hit; (nil, false, nil) when the run has no plan stage or its plan
// stage has no usable plan artifact (caller should walk to parent);
// (nil, false, err) on repo IO failure.
func (s *Server) tryLoadPlanForRun(ctx context.Context, runID uuid.UUID) (*plan.Plan, bool, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, false, fmt.Errorf("list stages for run: %w", err)
	}
	var planStageID uuid.UUID
	for _, st := range stages {
		if st.Type == run.StageTypePlan {
			planStageID = st.ID
			break
		}
	}
	if planStageID == uuid.Nil {
		return nil, false, nil
	}
	arts, err := s.cfg.ArtifactRepo.ListForStage(ctx, planStageID)
	if err != nil {
		return nil, false, fmt.Errorf("list plan stage artifacts: %w", err)
	}
	var picked *artifact.Artifact
	for _, a := range arts {
		if a.Kind != artifact.KindPlan {
			continue
		}
		if a.SchemaVersion == nil || *a.SchemaVersion != "standard_v1" {
			continue
		}
		if picked == nil || a.CreatedAt.After(picked.CreatedAt) {
			picked = a
		}
	}
	if picked == nil {
		return nil, false, nil
	}
	var p plan.Plan
	if err := json.Unmarshal(picked.Content, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: plan unmarshal failed",
			slog.String("run_id", runID.String()),
			slog.String("artifact_id", picked.ID.String()),
			slog.String("error", err.Error()),
		)
		return nil, false, nil
	}
	return &p, true, nil
}

// emitPlanMissingForImplement records the case where an implement-
// stage prompt was served without an approved plan. It's not an
// error in the HTTP sense — the runner gets a usable issue-only
// prompt — but the audit log should capture the gap so reviewers can
// tell whether the agent was working off the plan they approved.
//
// Best-effort: a failure to append the audit entry doesn't unwind
// the prompt response. Logged at warn level for operator visibility.
func (s *Server) emitPlanMissingForImplement(ctx context.Context, runID, stageID uuid.UUID) {
	if s.cfg.AuditRepo == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"run_id":   runID.String(),
		"stage_id": stageID.String(),
		"reason":   "no standard_v1 plan artifact found for the run's plan stage at implement-prompt fetch time",
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "plan_missing_for_implement",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: append plan_missing_for_implement failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// fillIssueContext populates the trigger's IssueTitle, IssueBody,
// and IssueURL.
//
// Resolution order (#415):
//  1. The run row's cached IssueContext — present when the CLI
//     ran `gh issue view` at run-create time and shipped the
//     payload inline. Used as-is; no GitHub call.
//  2. The webhook-dispatched path: when the run carries an
//     installation_id but no cached payload, fetch via GitHub
//     App token (unchanged behavior).
//  3. Otherwise leave the title + body empty; the prompt
//     template falls back to a "URL only" shape the agent can
//     navigate via its own tools.
//
// IssueURL is derived from `repo + IssueNumber` rather than the
// API response's html_url — the canonical github.com URL is fully
// determined by those two fields, and avoiding the response
// dependency means the field is set even on a partial fetch.
func (s *Server) fillIssueContext(ctx context.Context, github issueGetter, runRow *run.Run, issueNumber int, trigger *prompt.Trigger) {
	// Set the URL up front so any of the three branches below
	// leave the link-only renderer with a working fallback.
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse repo failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()),
		)
		return
	}
	trigger.IssueURL = fmt.Sprintf("https://github.com/%s/%s/issues/%d",
		repo.Owner, repo.Name, issueNumber)

	// Branch 1: operator's `gh` fetch at run-create time
	// pre-populated the title + body on the row. Prefer this
	// over a fresh GitHub call so local-runner runs (which lack
	// an installation_id) get the full prompt context.
	if runRow.IssueContext != nil {
		trigger.IssueTitle = runRow.IssueContext.Title
		trigger.IssueBody = runRow.IssueContext.Body
		return
	}

	// Branch 2: webhook-dispatched runs — fetch via the App's
	// installation token. Unchanged from the pre-#415 behavior.
	if runRow.InstallationID == nil {
		return
	}
	issue, err := github.GetIssue(ctx, *runRow.InstallationID, repo, issueNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: get issue failed",
			slog.String("run_id", runRow.ID.String()),
			slog.Int("issue", issueNumber),
			slog.String("error", err.Error()),
		)
		return
	}
	trigger.IssueTitle = issue.Title
	trigger.IssueBody = issue.Body
}

// issueGetter returns the configured client cast to the small
// interface the handler needs. Returns nil when GitHub is unset.
// The promptIssueGetterOverride test seam takes precedence so
// handler tests don't need a real *githubclient.Client.
func (s *Server) issueGetter() issueGetter {
	if s.promptIssueGetterOverride != nil {
		return s.promptIssueGetterOverride
	}
	if s.cfg.GitHub == nil {
		return nil
	}
	return s.cfg.GitHub
}

// resolveAgentTimeout returns the spec-governed timeout in seconds for the
// given run stage. Returns 0 when the workflow spec is absent or unparseable
// — the runner falls back to its own 15-minute constant in that case.
func (s *Server) resolveAgentTimeout(ctx context.Context, runRow *run.Run, stageType run.StageType) int {
	if runRow.WorkflowSpec == nil {
		return 0
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse workflow spec for timeout",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return 0
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return 0
	}
	// Primary match: spec stage ID == string(stageType) (canonical workflow).
	// Fallback: spec stage Type == stageType string.
	var specStage spec.Stage
	for _, st := range wf.Stages {
		if st.ID == string(stageType) {
			specStage = st
			break
		}
	}
	if specStage.ID == "" {
		for _, st := range wf.Stages {
			if string(st.Type) == string(stageType) {
				specStage = st
				break
			}
		}
	}
	resolved := spec.ResolveStageTimeout(wf, specStage, spec.DefaultStageTimeout)
	return int(resolved.Seconds())
}

// resolveVerifyConfig returns the verify command and timeout (in seconds)
// for the given stage from the run's workflow spec. Returns ("", 0) when
// the spec is absent, the stage declares no executor.verify block, or the
// timeout is zero. Mirrors resolveAgentTimeout's parse + lookup pattern.
func (s *Server) resolveVerifyConfig(ctx context.Context, runRow *run.Run, stageType run.StageType) (command string, timeoutSecs int) {
	if runRow.WorkflowSpec == nil {
		return "", 0
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse workflow spec for verify config",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return "", 0
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return "", 0
	}
	var specStage spec.Stage
	for _, st := range wf.Stages {
		if st.ID == string(stageType) {
			specStage = st
			break
		}
	}
	if specStage.ID == "" {
		for _, st := range wf.Stages {
			if string(st.Type) == string(stageType) {
				specStage = st
				break
			}
		}
	}
	if specStage.Executor.Verify == nil || specStage.Executor.Verify.Command == "" {
		return "", 0
	}
	secs := int(specStage.Executor.Verify.Timeout.Seconds())
	return specStage.Executor.Verify.Command, secs
}

// parseIssueRef extracts the issue number from a TriggerRef of the
// form "issue:<n>". Returns (n, true) on match; (0, false) otherwise.
func parseIssueRef(ref string) (int, bool) {
	const prefix = "issue:"
	if !strings.HasPrefix(ref, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(ref[len(prefix):])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// parseRepoOwnerName splits "owner/name" into a RepoRef. Returns
// an error if the input doesn't contain exactly one slash with
// non-empty segments.
func parseRepoOwnerName(s string) (githubclient.RepoRef, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return githubclient.RepoRef{}, fmt.Errorf("repo %q is not owner/name", s)
	}
	return githubclient.RepoRef{Owner: parts[0], Name: parts[1]}, nil
}

// resolveAgentSelfRetryForStage returns whether the workflow spec opts the
// given stage type into runner-side self-retry on category-A/C failures
// (ADR-023). Mirrors resolveVerifyConfig's parse + lookup pattern. Returns
// false on any error (nil spec, missing workflow, parse failure) so the
// runner degrades gracefully to the pre-ADR-023 behavior.
func (s *Server) resolveAgentSelfRetryForStage(ctx context.Context, runRow *run.Run, stageType run.StageType) bool {
	if runRow.WorkflowSpec == nil {
		return false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse workflow spec for agent_self_retry",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return false
	}
	var specStage spec.Stage
	for _, st := range wf.Stages {
		if st.ID == string(stageType) {
			specStage = st
			break
		}
	}
	if specStage.ID == "" {
		for _, st := range wf.Stages {
			if string(st.Type) == string(stageType) {
				specStage = st
				break
			}
		}
	}
	return specStage.Executor.AgentSelfRetry
}

// loadPriorRejectionFeedback searches the most-recent prior runs for
// the same trigger_ref and returns the rejection_comment from the newest
// approval_submitted audit entry where decision=reject and
// rejection_comment is non-empty. Returns nil when there is no matching
// prior rejection, when RunRepo or AuditRepo is unconfigured, or on any
// error (best-effort, same posture as CalibrationHint). At most 3 prior
// runs are inspected to bound audit fan-out; at most 10 runs are fetched
// from the repo in total (Limit=10).
func (s *Server) loadPriorRejectionFeedback(ctx context.Context, repo, triggerRef string, currentRunID uuid.UUID) *string {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil
	}
	ref := triggerRef
	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		Repo:       repo,
		TriggerRef: &ref,
		Limit:      10,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list runs for prior rejection failed",
			slog.String("trigger_ref", triggerRef),
			slog.String("error", err.Error()),
		)
		return nil
	}

	checked := 0
	for _, r := range runs {
		if r.ID == currentRunID {
			continue
		}
		if checked >= 3 {
			break
		}
		checked++

		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, r.ID, "approval_submitted")
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list approval_submitted for prior run failed",
				slog.String("run_id", r.ID.String()),
				slog.String("error", err.Error()),
			)
			continue
		}

		// Scan newest-first (ListForRunByCategory returns entries ordered ASC by ts).
		for i := len(entries) - 1; i >= 0; i-- {
			var payload struct {
				Decision         string `json:"decision"`
				RejectionComment string `json:"rejection_comment"`
			}
			if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
				continue
			}
			if payload.Decision == "reject" && payload.RejectionComment != "" {
				c := payload.RejectionComment
				s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
					"prompt: loaded prior rejection feedback into plan prompt",
					slog.String("prior_run_id", r.ID.String()),
					slog.Int("comment_bytes", len(c)),
				)
				return &c
			}
		}
	}
	return nil
}

// resolveDecomposedScopeConstraint builds a *prompt.ScopeConstraint for
// child runs of a decomposed plan. Returns nil when:
//   - the run is not decomposed (DecomposedFrom == nil)
//   - the run has no cached IssueContext (can't match a sub-plan without it)
//   - ArtifactRepo is unconfigured (nil-guard: avoids a panic in tryLoadPlanForRun)
//   - the parent plan can't be loaded (degrade gracefully — agent gets an unconstrained prompt)
//   - no sub-plan title matches the child's IssueContext.Body prefix (defensive — wrong constraint is worse than none)
//
// Matching uses strings.HasPrefix(body, "## "+title+"\n\n"), which is the
// invariant enforced by childIssueContextFromSubPlan in orchestrator.go.
func (s *Server) resolveDecomposedScopeConstraint(ctx context.Context, runRow *run.Run) *prompt.ScopeConstraint {
	if runRow.DecomposedFrom == nil || runRow.IssueContext == nil {
		return nil
	}
	if s.cfg.ArtifactRepo == nil {
		return nil
	}
	parentPlan, found, err := s.tryLoadPlanForRun(ctx, *runRow.DecomposedFrom)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: resolve scope constraint: load parent plan failed",
			slog.String("parent_run_id", runRow.DecomposedFrom.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if !found || parentPlan == nil || parentPlan.Decomposition == nil {
		return nil
	}

	body := runRow.IssueContext.Body
	matchIdx := -1
	for i, sub := range parentPlan.Decomposition.SubPlans {
		if strings.HasPrefix(body, "## "+sub.Title+"\n\n") {
			matchIdx = i
			break
		}
	}
	if matchIdx < 0 {
		return nil
	}

	matched := parentPlan.Decomposition.SubPlans[matchIdx]
	var siblingHints []string
	for i, sub := range parentPlan.Decomposition.SubPlans {
		if i != matchIdx {
			siblingHints = append(siblingHints, sub.ScopeHint)
		}
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"prompt: injected scope constraint for decomposed child",
		slog.String("child_run_id", runRow.ID.String()),
		slog.String("parent_run_id", runRow.DecomposedFrom.String()),
		slog.Int("sibling_count", len(siblingHints)),
	)
	return &prompt.ScopeConstraint{
		ScopeHint:    matched.ScopeHint,
		ParentRunID:  runRow.DecomposedFrom.String(),
		SiblingHints: siblingHints,
	}
}

// loadLastDecomposeRejectionReason scans the run's approval_submitted
// audit entries (newest-first) and returns true when it finds one with
// decision=reject and reject_reason=decompose_required. Used by the
// plan-stage prompt builder to inject a binding decompose hint on
// re-plan attempts after the approver requested decomposition.
func (s *Server) loadLastDecomposeRejectionReason(ctx context.Context, runID uuid.UUID) bool {
	if s.cfg.AuditRepo == nil {
		return false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "approval_submitted")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list approval_submitted audit failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		var payload struct {
			Decision     string `json:"decision"`
			RejectReason string `json:"reject_reason"`
		}
		if err := json.Unmarshal(entries[i].Payload, &payload); err != nil {
			continue
		}
		if payload.Decision == "reject" && payload.RejectReason == "decompose_required" {
			return true
		}
	}
	return false
}
