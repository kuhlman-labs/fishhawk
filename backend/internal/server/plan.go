package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// PlanReviewer invokes a review agent synchronously with the given
// prompt text and returns the structured verdict and the model identifier
// used. The model is compared to plan.GeneratedBy.Model for the
// self-review guard (ADR-027). Nil PlanReviewer disables agent-driven
// plan review regardless of the stage's reviewers spec config.
type PlanReviewer interface {
	Review(ctx context.Context, promptText string) (verdict *planreview.ReviewVerdict, model string, err error)
}

// maxPlanBundleBytes caps the request body for plan upload. Plans
// are document-shaped JSON (steps, files, scope, etc.); even verbose
// ones rarely top a few KB. 256KB leaves an order-of-magnitude
// headroom and rejects pathological payloads early.
const maxPlanBundleBytes = 256 * 1024

// handleShipPlan implements POST /v0/runs/{run_id}/plan.
//
// Auth: same Ed25519 signing-key flow as /v0/runs/{run_id}/trace.
// The runner produced the signature over sha256(body) using the
// per-run private key issued by /v0/runs/{run_id}/signing-key.
//
// On success: validates the body against the standard_v1 schema,
// dedups against any existing plan with the same content_hash for
// the same stage (idempotent re-upload), creates an artifacts row,
// and appends a `plan_generated` audit entry. Returns 201 with the
// artifact's id + content_hash.
//
// Failure modes are explicit so the runner can map them to the
// right category:
//   - schema invalid     → 400 (category B / constraint)
//   - signature failures → 401 (matches trace upload)
//   - oversized          → 413
//   - storage failures   → 500 (category C — runner retries)
func (s *Server) handleShipPlan(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.ArtifactRepo == nil ||
		s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "plan_upload_unconfigured",
			"plan upload requires signing, artifact, audit, and run repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	stageID, err := uuid.Parse(r.URL.Query().Get("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id query parameter must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.URL.Query().Get("stage_id")})
		return
	}

	// Confirm the stage exists and belongs to the run before doing
	// signature math. A mismatched stage_id is a 404, not a forged
	// signature.
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not exist",
			map[string]any{"stage_id": stageID.String()})
		return
	}
	if stage.RunID != runID {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage does not belong to the supplied run",
			map[string]any{"stage_id": stageID.String(), "run_id": runID.String()})
		return
	}

	sigHeader := r.Header.Get("X-Fishhawk-Signature")
	if sigHeader == "" {
		s.writeError(w, r, http.StatusUnauthorized, "signature_missing",
			"X-Fishhawk-Signature header is required", nil)
		return
	}
	signature, err := hex.DecodeString(sigHeader)
	if err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
			"X-Fishhawk-Signature is not valid hex",
			map[string]any{"error": err.Error()})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPlanBundleBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxPlanBundleBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"plan body exceeds size cap",
			map[string]any{"limit_bytes": maxPlanBundleBytes})
		return
	}

	message := signing.ComputeMessage(body)
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
		return
	}

	// Validate payload against standard_v1. For the known string-elision
	// class of schema violations, attempt coercion before returning 400.
	// For all other errors, category-B maps the failure so the runner
	// doesn't retry — the agent's output is bad and re-shipping won't help.
	if err := plan.Validate(body); err != nil {
		coercionOK := false
		reportErr := err // updated to post-coercion error on partial coercion
		var schemaErr *plan.SchemaError
		if errors.As(err, &schemaErr) {
			coercedBytes, coercions, coerceErr := plan.TryCoerce(body, time.Now().UTC())
			if coerceErr == nil && len(coercions) > 0 {
				// Coercion produced a valid plan. Record it before
				// artifact storage so a spike in plan_coerced entries
				// is visible as a prompt-quality signal.
				coercionPayload, _ := json.Marshal(map[string]any{
					"run_id":    runID.String(),
					"stage_id":  stageID.String(),
					"coercions": coercions,
				})
				systemKind := audit.ActorKind("system")
				if _, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
					RunID:     runID,
					StageID:   &stageID,
					Timestamp: time.Now().UTC(),
					Category:  "plan_coerced",
					ActorKind: &systemKind,
					Payload:   coercionPayload,
				}); aerr != nil {
					s.writeError(w, r, http.StatusInternalServerError, "internal_error",
						"append coercion audit entry failed", map[string]any{"error": aerr.Error()})
					return
				}
				body = coercedBytes
				coercionOK = true
			} else if coerceErr != nil && len(coercions) > 0 {
				// Partial coercion: some fields were fixed but the plan is
				// still invalid. Report the post-coercion error so the 400
				// names the remaining violation rather than a field that
				// coercion already fixed.
				reportErr = coerceErr
			}
		}
		if !coercionOK {
			// Coercion could not help (non-string type or re-validation
			// still fails). Transition the stage to failed-B so the run
			// reflects the bad output rather than getting stuck in
			// `running` (#527). Best-effort: a TransitionStage error
			// logs but doesn't change the upload response.
			cat := run.FailureB
			reason := "plan_invalid: " + reportErr.Error()
			if _, terr := s.cfg.RunRepo.TransitionStage(r.Context(), stageID,
				run.StageStateFailed, &run.StageCompletion{
					FailureCategory: &cat,
					FailureReason:   &reason,
				}); terr != nil {
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
					"plan upload: transition to failed-B after validation error failed",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", stageID.String()),
					slog.String("error", terr.Error()))
			}
			s.writeError(w, r, http.StatusBadRequest, "plan_invalid",
				"plan does not validate against standard_v1",
				map[string]any{"error": reportErr.Error()})
			return
		}
	}

	contentHash := sha256Hex(body)

	// Idempotency: if a plan with this content hash already exists
	// for this stage, return it without inserting a duplicate row.
	// Re-uploads from a retried runner job are common enough that the
	// expected-success case shouldn't fail with a unique-constraint
	// 500.
	if existing, err := s.cfg.ArtifactRepo.GetByHash(r.Context(), stageID, contentHash); err == nil {
		// Plan-ready comment-back retry hook (#245). The notify
		// path may have run from the trace handler before this
		// plan artifact existed, in which case it skipped silently;
		// fire here too so the comment lands. Audit-log dedup keeps
		// the second call a no-op when the first succeeded.
		s.notifyPlanReadyIfReady(r, runID, stage)
		s.writeJSON(w, r, http.StatusOK, planResponse{
			ID:            existing.ID,
			StageID:       existing.StageID,
			ContentHash:   existing.ContentHash,
			SchemaVersion: deref(existing.SchemaVersion),
			Idempotent:    true,
		})
		return
	} else if !errors.Is(err, artifact.ErrNotFound) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing plan failed", map[string]any{"error": err.Error()})
		return
	}

	schemaVersion := "standard_v1"
	created, err := s.cfg.ArtifactRepo.Create(r.Context(), artifact.CreateParams{
		StageID:       stageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &schemaVersion,
		Content:       json.RawMessage(body),
		ContentHash:   contentHash,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create plan artifact failed", map[string]any{"error": err.Error()})
		return
	}

	// Audit. The chained-append holds a row-lock on runs so two
	// concurrent uploads can't fork the chain. Failure here leaves
	// us with an artifact row and no audit entry, which is the
	// auditability inverse of what we want — surface 500 so the
	// runner retries (idempotent at the artifact layer via GetByHash).
	auditPayload, _ := json.Marshal(map[string]any{
		"run_id":         runID.String(),
		"stage_id":       stageID.String(),
		"artifact_id":    created.ID.String(),
		"content_hash":   contentHash,
		"schema_version": schemaVersion,
		"size_bytes":     len(body),
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "plan_generated",
		ActorKind: &systemKind,
		Payload:   auditPayload,
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// Plan review: invoke configured review agents after the artifact
	// is stored and audited, before advancing the stage. Returns true
	// when authority is gating and at least one verdict is reject;
	// in that case the stage has been transitioned to failed-B and
	// stage advancement is blocked.
	gatingRejected := s.runPlanReviews(r.Context(), runID, stageID, body)

	// Plan-ready comment-back hook (#245). Fires here so the
	// notifier sees the just-landed artifact even when the runner's
	// trace-then-plan upload order beats the trace-handler's
	// notifyPlanReady to running. Best-effort + dedup'd via the
	// audit log; safe to call alongside the trace-handler hook.
	// Suppressed on gating-reject: the stage has been failed, not
	// awaiting approval, so a plan-ready comment would be misleading.
	if !gatingRejected {
		s.notifyPlanReadyIfReady(r, runID, stage)
	}

	s.writeJSON(w, r, http.StatusCreated, planResponse{
		ID:            created.ID,
		StageID:       created.StageID,
		ContentHash:   created.ContentHash,
		SchemaVersion: deref(created.SchemaVersion),
		Idempotent:    false,
	})
}

// notifyPlanReadyIfReady wraps the trace handler's notifyPlanReady
// with a "stage is terminal" guard for the plan-upload path (#245).
// The trace handler is reached only after a stage transitions
// terminally, so the trace-side hook can fire unconditionally; the
// plan-upload handler can be reached BEFORE the trace upload (a
// future runner reordering) and shouldn't comment until the stage
// is actually settled.
func (s *Server) notifyPlanReadyIfReady(r *http.Request, runID uuid.UUID, stage *run.Stage) {
	if stage.Type != run.StageTypePlan {
		return
	}
	if !stage.State.IsTerminal() && stage.State != run.StageStateAwaitingApproval {
		return
	}
	s.notifyPlanReady(r.Context(), runID, stage)
}

// planResponse is the JSON returned to the runner on plan upload.
// The runner doesn't need the full content back (it just sent it),
// so the response is the minimal acknowledgement.
type planResponse struct {
	ID            uuid.UUID `json:"id"`
	StageID       uuid.UUID `json:"stage_id"`
	ContentHash   string    `json:"content_hash"`
	SchemaVersion string    `json:"schema_version"`
	// Idempotent is true when the upload matched an existing plan
	// for this stage and no new row was inserted.
	Idempotent bool `json:"idempotent"`
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// runPlanReviews invokes configured review agents after a plan artifact
// is stored and audited. For each invocation it:
//  1. Builds the plan_review prompt from the plan artifact + run context.
//  2. Calls PlanReviewer.Review and logs WARN on self-review (model match).
//  3. Appends a plan_reviewed audit entry with the verdict payload.
//
// Returns true when authority is gating (reviewers.agent>0 && human==0)
// and at least one verdict is reject. In that case the stage has been
// transitioned to failed-B so trace-driven awaiting_approval advancement
// is blocked by the terminal state machine.
//
// Returns false (no gating rejection) when:
//   - PlanReviewer is nil (production default — not yet wired)
//   - RunRepo is nil
//   - the run's workflow spec carries no plan stage with reviewers.agent>0
//   - all review agents approve (or approve_with_concerns)
//
// All per-invocation errors are WARN-logged and skipped so a transient
// reviewer failure doesn't fail the upload response — the plan artifact
// is already durably stored.
func (s *Server) runPlanReviews(ctx context.Context, runID, stageID uuid.UUID, planBody []byte) bool {
	// RunRepo is required to resolve the workflow spec; without it we
	// can't tell whether agent review was even requested.
	if s.cfg.RunRepo == nil {
		return false
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}

	reviewersCfg := s.resolvePlanStageReviewers(ctx, runRow)
	if reviewersCfg == nil || reviewersCfg.Agent == 0 {
		return false
	}

	authority := planreview.ResolveAuthority(*reviewersCfg)

	// PlanReviewer not wired but the spec requested agent review
	// (#574 / ADR-027). Emit a plan_review_skipped audit entry so the
	// degradation is auditable rather than silent, then continue: the
	// stage advances to awaiting_approval via the trace path (in
	// advisory mode the human gate remains authoritative). The
	// gating-mode hard block lives at the run-create endpoint
	// (handleCreateRun); the dispatcher-driven path is not guarded
	// there, so the entry is emitted for both authorities here.
	if s.cfg.PlanReviewer == nil {
		if s.cfg.AuditRepo != nil {
			payload, _ := json.Marshal(planreview.ReviewSkippedPayload{
				Reason:           "reviewer_not_configured",
				ConfiguredAgents: reviewersCfg.Agent,
				Authority:        authority,
			})
			systemKind := audit.ActorKind("system")
			if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
				RunID:     runID,
				StageID:   &stageID,
				Timestamp: time.Now().UTC(),
				Category:  "plan_review_skipped",
				ActorKind: &systemKind,
				Payload:   payload,
			}); aerr != nil {
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: append plan_review_skipped audit entry failed",
					slog.String("run_id", runID.String()),
					slog.String("error", aerr.Error()),
				)
			}
		}
		return false
	}

	// Parse plan for the self-review guard (GeneratedBy.Model) and
	// for the plan_review prompt builder. Validation already passed
	// earlier in handleShipPlan; parse failure here is an internal
	// inconsistency — log and skip reviews.
	parsedPlan, err := plan.Parse(planBody)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: parse plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}

	// Build the plan_review prompt using the same trigger-context
	// machinery as the agent prompt handler.
	trig := prompt.Trigger{
		Repo:         runRow.Repo,
		ApprovedPlan: parsedPlan,
	}
	if runRow.IssueContext != nil {
		trig.IssueTitle = runRow.IssueContext.Title
		trig.IssueBody = runRow.IssueContext.Body
	}
	if runRow.TriggerRef != nil {
		if n, ok := parseIssueRef(*runRow.TriggerRef); ok {
			trig.IssueNumber = n
		}
	}
	promptText, err := prompt.Build("plan_review", trig)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: build prompt failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}

	systemKind := audit.ActorKind("system")
	hasRejection := false
	for i := 0; i < reviewersCfg.Agent; i++ {
		verdict, model, err := s.cfg.PlanReviewer.Review(ctx, promptText)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: reviewer invocation failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.Int("reviewer_index", i),
				slog.String("error", err.Error()),
			)
			continue
		}

		// Self-review guard (ADR-027): warn when the review agent's
		// model matches the plan author's model. Warn-only per ADR;
		// the verdict is still recorded.
		if model != "" && model == parsedPlan.GeneratedBy.Model {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"plan review: self-review detected — reviewer model matches plan author model",
				slog.String("model", model),
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
			)
		}

		// Append a plan_reviewed audit entry for each invocation.
		payload := planreview.PlanReviewedPayload{
			ReviewerKind:  "agent",
			ReviewerModel: model,
			Authority:     authority,
			Verdict:       verdict.Verdict,
			Concerns:      verdict.Concerns,
			FreeForm:      verdict.FreeForm,
		}
		payloadBytes, _ := json.Marshal(payload)
		if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     runID,
			StageID:   &stageID,
			Timestamp: time.Now().UTC(),
			Category:  "plan_reviewed",
			ActorKind: &systemKind,
			Payload:   payloadBytes,
		}); aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: append audit entry failed",
				slog.String("run_id", runID.String()),
				slog.String("error", aerr.Error()),
			)
		}

		if verdict.Verdict == planreview.VerdictReject {
			hasRejection = true
		}
	}

	// Authority gating: when gating mode and any verdict is reject,
	// transition the stage to failed-B so the trace handler's
	// dispatched→awaiting_approval path is blocked by the terminal state.
	if authority == planreview.AuthorityGating && hasRejection {
		cat := run.FailureB
		reason := "plan_review_rejected: agent review verdict reject under gating authority"
		if _, terr := s.cfg.RunRepo.TransitionStage(ctx, stageID,
			run.StageStateFailed, &run.StageCompletion{
				FailureCategory: &cat,
				FailureReason:   &reason,
			}); terr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"plan review: transition to failed-B after gating reject failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", terr.Error()),
			)
		}
		return true
	}
	return false
}

// resolvePlanStageReviewers reads the run's workflow spec and returns
// the ReviewersConfig for the plan-type stage in the active workflow.
// Returns nil when the spec is absent, unparseable, or the workflow
// has no plan stage.
func (s *Server) resolvePlanStageReviewers(ctx context.Context, runRow *run.Run) *spec.ReviewersConfig {
	if runRow.WorkflowSpec == nil {
		return nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: parse workflow spec failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return nil
	}
	for _, st := range wf.Stages {
		if st.Type == spec.StageTypePlan {
			return st.Reviewers
		}
	}
	return nil
}
