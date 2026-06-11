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
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
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

// ReviewerSet resolves the deployment's configured review adapters (#955).
// Default returns the precedence-selected adapter the bare `agent: N`
// count form invokes — nil when no reviewer backend is configured at all.
// For returns the adapter for one spec-declared heterogeneous reviewer
// (reviewers.agents[i]), constructed with the given model override (empty
// model falls back to the provider's deployment-configured default); it
// errors when the provider is not configured in this deployment.
type ReviewerSet interface {
	Default() PlanReviewer
	For(provider, model string) (PlanReviewer, error)
}

// defaultPlanReviewer returns the precedence-selected default adapter, or
// nil when no reviewer set is wired or the set has no configured backend.
// It is the ReviewerSet-era equivalent of the old `cfg.PlanReviewer == nil`
// guard: nil means no reviewer backend exists and the *_review_skipped
// degradation path applies.
func (s *Server) defaultPlanReviewer() PlanReviewer {
	if s.cfg.PlanReviewers == nil {
		return nil
	}
	return s.cfg.PlanReviewers.Default()
}

// reviewerInvocation is one resolved review-agent invocation in a plan- or
// implement-review loop. The count form repeats the default adapter with
// empty provider/specModel; the agents form carries the declared provider
// and model. resolveErr is non-nil when the declared provider could not be
// resolved to a configured adapter (advisory mode, or a config change
// between the dispatch pre-check and execution) — the loop treats that as
// a failed invocation: emit *_review_failed, continue, hasRejection
// untouched.
type reviewerInvocation struct {
	reviewer   PlanReviewer
	provider   string
	specModel  string
	resolveErr error
}

// resolveReviewerInvocations maps a stage's ReviewersConfig to its
// per-invocation reviewer list (#955). The heterogeneous agents form
// resolves each declared {provider, model} via the ReviewerSet; the bare
// count form repeats the precedence-selected default adapter Agent times
// (today's behavior). Callers guard on defaultPlanReviewer() != nil first,
// so cfg.PlanReviewers is non-nil here.
func (s *Server) resolveReviewerInvocations(reviewersCfg *spec.ReviewersConfig) []reviewerInvocation {
	if len(reviewersCfg.Agents) > 0 {
		invocations := make([]reviewerInvocation, 0, len(reviewersCfg.Agents))
		for _, a := range reviewersCfg.Agents {
			reviewer, err := s.cfg.PlanReviewers.For(a.Provider, a.Model)
			invocations = append(invocations, reviewerInvocation{
				reviewer:   reviewer,
				provider:   a.Provider,
				specModel:  a.Model,
				resolveErr: err,
			})
		}
		return invocations
	}
	def := s.cfg.PlanReviewers.Default()
	invocations := make([]reviewerInvocation, reviewersCfg.Agent)
	for i := range invocations {
		invocations[i] = reviewerInvocation{reviewer: def}
	}
	return invocations
}

// maxPlanBundleBytes caps the request body for plan upload. Plans
// are document-shaped JSON (steps, files, scope, etc.); even verbose
// ones rarely top a few KB. 256KB leaves an order-of-magnitude
// headroom and rejects pathological payloads early.
const maxPlanBundleBytes = 256 * 1024

// maxPlanSchemaRetries bounds the in-run schema-retry budget (#646).
// On the first post-coercion validation failure the plan stage is
// re-opened and re-dispatched with the validation error fed back; a
// second failure exhausts the budget and fails-B as before. The budget
// is tracked by counting plan_schema_retry audit entries for the run.
const maxPlanSchemaRetries = 1

// categoryPlanSchemaRetry is the audit-log category for the chained
// entry trySchemaRetry writes when it re-opens a plan stage after a
// transient schema-validation failure (#646). The entry is both the
// budget counter (countSchemaRetries counts them) and the feedback
// source (loadPriorSchemaValidationError reads the newest
// validation_error back into the next plan prompt). The payload-key
// contract (validation_error) is exercised end-to-end by the
// cross-boundary seam test.
const categoryPlanSchemaRetry = "plan_schema_retry"

// maxSchemaValidationErrorBytes caps the validation_error stored in a
// plan_schema_retry audit entry, mirroring buildPlan's maxFeedbackBytes
// so the recorded error never outgrows the prompt-injection cap.
const maxSchemaValidationErrorBytes = 4000

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
			// Transient-retry backstop (#646): before failing-B, attempt a
			// bounded in-run re-dispatch of the plan stage with the
			// validation error fed back. A transient generation slip
			// recovers on the re-attempt; a deterministic/structural
			// violation fails the same validation twice and exhausts the
			// budget, falling through to the unchanged fail-B path below.
			if s.trySchemaRetry(r, runID, stageID, reportErr) {
				// The 400 lets the now-finished runner exit cleanly (exactly
				// as the fail-B path does). retry_scheduled signals to the
				// local operator/driver that a re-attempt was set up rather
				// than a terminal failure — the re-opened plan stage is
				// re-driven automatically on the github_actions path and by a
				// fresh fishhawk_run_stage --stage plan on the local path.
				s.writeError(w, r, http.StatusBadRequest, "plan_invalid",
					"plan does not validate against standard_v1; a bounded in-run retry was scheduled",
					map[string]any{"error": reportErr.Error(), "retry_scheduled": true})
				return
			}

			// Coercion could not help (non-string type or re-validation
			// still fails) and the schema-retry budget is exhausted or
			// unavailable. Fail the stage as category-B and walk the run
			// to terminal failed (#527 / #603). The trace handler now
			// leaves a plan stage in running (it no longer advances plan
			// stages without a plan artifact), so FailStage walks
			// running → failed; under a future plan-first reordering it
			// walks from whatever non-terminal state the stage is in.
			// advanceAfterFailure then drives the orchestrator so the run
			// doesn't strand with a failed stage but a pending/running run.
			// Best-effort: a transition / advance error logs but doesn't
			// change the upload response.
			cat := run.FailureB
			reason := "plan_invalid: " + reportErr.Error()
			if _, ferr := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, cat, reason); ferr != nil {
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
					"plan upload: transition to failed-B after validation error failed",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", stageID.String()),
					slog.String("error", ferr.Error()))
			}
			s.advanceAfterFailure(r, runID, stageID)
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

	// Plan-gate scope pre-check (#658): evaluate the plan's scope.files
	// against the implement stage's path constraints (forbidden_paths /
	// allowed_paths / max_files_changed) and record an advisory
	// plan_scope_precheck audit entry. Run it before runPlanReviews so the
	// precheck entry precedes any plan_reviewed entries in the audit
	// sequence, and on the request context (cheap, synchronous, no external
	// calls). Advisory + fail-open: never blocks or unwinds the upload.
	// The returned result (nil on fail-open) is threaded into the
	// plan-review prompt's gate-evidence section below (#963).
	precheck := s.runScopePrecheck(r.Context(), runID, stageID, body)

	// Plan-gate surface sweep (#763): evaluate the plan's scope.files
	// against the static surface registry and record an advisory
	// plan_surface_sweep audit entry flagging sibling surfaces a plan
	// must move in lockstep with (an @-mention render peer, or the
	// mandated issue-comment-surfaces doc). Run it alongside the scope
	// pre-check, before runPlanReviews. Advisory + fail-open: never
	// blocks or unwinds the upload. Like the pre-check, the returned
	// result feeds the plan-review prompt's gate-evidence section (#963).
	sweep := s.runSurfaceSweep(r.Context(), runID, stageID, body)

	// Plan review: invoke configured review agents after the artifact
	// is stored and audited, before advancing the stage. The gate
	// results computed above ride along so the review prompt carries
	// the machine-verified evidence (#963). Returns true when authority
	// is gating and at least one verdict is reject; in that case the
	// stage has been transitioned to failed-B and stage advancement is
	// blocked.
	gatingRejected := s.runPlanReviews(r.Context(), runID, stageID, body, precheck, sweep)

	// Plan-stage terminal advancement (#603). With a valid plan artifact
	// now stored, this handler is the authoritative driver of the plan
	// stage's terminal transition: the trace handler leaves plan stages in
	// running until a plan exists. Advance running → awaiting_approval (or
	// → succeeded + orchestrator Advance for a gateless plan stage).
	// Suppressed on gating-reject: runPlanReviews has already failed the
	// stage, so advancing it would be a no-op-or-fault either way.
	//
	// notifyPlanReadyIfReady must run AFTER the advance — its guard
	// requires the stage to be terminal/awaiting_approval. The
	// just-landed artifact is now visible to the notifier even when the
	// runner's trace-then-plan order beat the trace-handler hook to
	// running. Best-effort + dedup'd via the audit log; safe alongside
	// the trace-handler hook.
	if !gatingRejected {
		stage = s.advancePlanStageTerminal(r, runID, stage)
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

// countSchemaRetries counts the run's plan_schema_retry audit entries
// — the in-run schema-retry budget counter (#646). Returns 0 on any
// error or when the AuditRepo is unconfigured (best-effort, same read
// shape as loadLastDecomposeRejectionReason). A nil count degrades to
// "no retries recorded", which is the conservative choice: at worst the
// budget gate lets one extra retry through rather than wedging the run.
func (s *Server) countSchemaRetries(ctx context.Context, runID uuid.UUID) int {
	if s.cfg.AuditRepo == nil {
		return 0
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, categoryPlanSchemaRetry)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan upload: count schema retries failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return 0
	}
	return len(entries)
}

// trySchemaRetry attempts a bounded in-run re-dispatch of a plan stage
// whose uploaded plan failed standard_v1 validation after coercion
// (#646). It returns true when the re-dispatch was set up (caller
// responds 400 with retry_scheduled) and false when the caller should
// fall through to the terminal fail-B path.
//
// Preconditions (any false → return false → fail-B):
//   - Orchestrator and AuditRepo are wired (needed to re-dispatch and to
//     record/count the budget).
//   - countSchemaRetries < maxPlanSchemaRetries (budget remaining).
//
// On a granted retry, in order (audit-first, mirroring handleRetryStage
// so the retry intent is durable even if a later step fails):
//  1. Append a chained plan_schema_retry audit entry carrying the
//     validation_error (capped), attempt, stage_id, and run_id. This
//     entry is BOTH the budget counter and the feedback source the next
//     plan prompt reads back via loadPriorSchemaValidationError.
//  2. Re-open the stage: FailStage(FailureA) walks it
//     running/dispatched → failed (transient category A, never B), then
//     RunRepo.RetryStage(StageStatePending) walks failed → pending and
//     clears the transient failure metadata — so the FailureA never
//     leaks into the run's terminal state or the upload response.
//  3. Orchestrator.Advance re-dispatches. The plan stage is sequence 0,
//     so Advance picks it up (github_actions fires workflow_dispatch;
//     local skips dispatch but still walks pending → dispatched, leaving
//     the stage re-drivable by a fresh fishhawk_run_stage --stage plan,
//     whose prompt then carries Trigger.PriorSchemaValidationError).
func (s *Server) trySchemaRetry(r *http.Request, runID, stageID uuid.UUID, reportErr error) bool {
	if s.cfg.Orchestrator == nil || s.cfg.AuditRepo == nil {
		return false
	}
	attempt := s.countSchemaRetries(r.Context(), runID)
	if attempt >= maxPlanSchemaRetries {
		return false
	}

	validationErr := reportErr.Error()
	if len(validationErr) > maxSchemaValidationErrorBytes {
		validationErr = validationErr[:maxSchemaValidationErrorBytes] + "...[truncated]"
	}
	payload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"attempt":          attempt + 1,
		"validation_error": validationErr,
	})
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPlanSchemaRetry,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		// Without the budget/feedback entry the retry is neither bounded
		// nor steerable — fall back to fail-B rather than re-dispatching
		// blind.
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"plan upload: append plan_schema_retry audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()))
		return false
	}

	// Re-open: running/dispatched → failed (transient A) → pending.
	if _, ferr := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureA,
		"plan_invalid_transient_retry: "+reportErr.Error()); ferr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"plan upload: transition to failed-A for schema retry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", ferr.Error()))
		return false
	}
	if _, rerr := s.cfg.RunRepo.RetryStage(r.Context(), stageID, run.StageStatePending); rerr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"plan upload: re-open stage to pending for schema retry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", rerr.Error()))
		return false
	}

	// Drive the orchestrator to re-dispatch the now-pending plan stage.
	// Best-effort: a failure here logs but still returns true — the stage
	// is re-opened and the audit entry recorded, so an operator (or a
	// fresh fishhawk_run_stage) can re-drive it.
	if _, aerr := s.cfg.Orchestrator.Advance(r.Context(), runID); aerr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"plan upload: orchestrator advance after schema retry re-open failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()))
	}

	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo,
		"plan upload: scheduled in-run schema retry",
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stageID.String()),
		slog.Int("attempt", attempt+1))
	return true
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

// advancePlanStageTerminal drives the plan stage's terminal transition
// once a valid plan artifact has landed (#603). The trace handler leaves
// plan stages in running until a plan exists, so this handler owns the
// running → awaiting_approval (gated) or running → succeeded (gateless)
// transition.
//
// Idempotent: the state machine treats same-state re-application as a
// no-op, so a future runner reordering where the trace handler already
// advanced the stage (plan-first ordering) does not double-fault. On the
// gateless path it fires the orchestrator's Advance so the next stage is
// picked up, mirroring advanceStageAfterTrace.
//
// Best-effort: transition / advance / notify errors are WARN-logged and
// never unwind the upload response. Returns the post-transition stage
// (or the pre-transition stage on error) so the caller can pass the
// settled state to notifyPlanReadyIfReady.
func (s *Server) advancePlanStageTerminal(r *http.Request, runID uuid.UUID, stage *run.Stage) *run.Stage {
	terminal := run.StageStateAwaitingApproval
	if !stage.RequiresApproval {
		terminal = run.StageStateSucceeded
	}
	updated, err := s.cfg.RunRepo.TransitionStage(r.Context(), stage.ID, terminal, nil)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"plan upload: transition to terminal failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("target", string(terminal)),
			slog.String("error", err.Error()))
		return stage
	}

	// Sticky status comment (E20.4 / #330): the plan stage just settled.
	s.notifyStatusUpdate(r.Context(), runID, "plan_handler")

	// Gateless plan stages get no approval submission to drive the next
	// dispatch — fire the orchestrator ourselves so the next stage gets
	// picked up. Best-effort: a failure here logs but doesn't unwind.
	if terminal == run.StageStateSucceeded && s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"plan upload: orchestrator advance after gateless plan stage failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
	}
	return updated
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

// runPlanReviews resolves the plan stage's review config and dispatches
// the review agents. It does the cheap request-scoped reads (GetRun,
// resolveStageReviewers, plan.Parse, prompt.Build) on the caller's
// context, then branches on authority:
//
//   - gating (reviewers.agent>0 && human==0): runs the review loop
//     SYNCHRONOUSLY. When any verdict is reject it transitions the stage
//     to failed-B and returns true, so the trace handler's
//     dispatched→awaiting_approval advancement is blocked by the
//     terminal state machine. The failed-B edge must land before stage
//     advancement, which is why gating review can't be detached.
//
//   - advisory (reviewers.agent>0 && human>0): dispatches the review
//     loop on a DETACHED context (context.WithoutCancel) in a goroutine
//     tracked by s.bgReviews, and returns false immediately so the
//     upload handler writes its 201 without waiting on the reviewer.
//     The detachment is the #584 fix: the review runs to its own
//     FISHHAWKD_PLAN_REVIEW_TIMEOUT budget instead of dying when the
//     runner's upload client disconnects and cancels r.Context().
//
// Returns false (no gating rejection) when:
//   - no reviewer backend is configured (nil ReviewerSet or Default() nil)
//   - RunRepo is nil
//   - the run's workflow spec carries no plan stage with reviewers.agent>0
//   - authority is advisory (review runs detached, never blocks)
//   - all review agents approve (or approve_with_concerns)
//
// All per-invocation errors are WARN-logged and skipped so a transient
// reviewer failure doesn't fail the upload response — the plan artifact
// is already durably stored.
//
// precheck and sweep are the plan-gate results handleShipPlan's
// synchronous checks computed (nil when a gate failed open); they are
// threaded into the plan-review prompt's gate-evidence section (#963)
// and never alter dispatch, authority, or verdict handling.
func (s *Server) runPlanReviews(ctx context.Context, runID, stageID uuid.UUID, planBody []byte, precheck *ScopePrecheckPayload, sweep *SurfaceSweepPayload) bool {
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

	reviewersCfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypePlan)
	if reviewersCfg == nil || reviewersCfg.AgentCount() == 0 {
		return false
	}

	authority := planreview.ResolveAuthority(*reviewersCfg)

	// No reviewer backend wired but the spec requested agent review
	// (#574 / ADR-027). Emit a plan_review_skipped audit entry so the
	// degradation is auditable rather than silent, then continue: the
	// stage advances to awaiting_approval via the trace path (in
	// advisory mode the human gate remains authoritative). The
	// gating-mode hard block lives at the run-create endpoint
	// (handleCreateRun); the dispatcher-driven path is not guarded
	// there, so the entry is emitted for both authorities here.
	if s.defaultPlanReviewer() == nil {
		if s.cfg.AuditRepo != nil {
			payload, _ := json.Marshal(planreview.ReviewSkippedPayload{
				Reason:           "reviewer_not_configured",
				ConfiguredAgents: reviewersCfg.AgentCount(),
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
		Repo:             runRow.Repo,
		ApprovedPlan:     parsedPlan,
		PlanGateEvidence: planGateEvidence(precheck, sweep),
	}
	if runRow.IssueContext != nil {
		trig.IssueTitle = runRow.IssueContext.Title
		trig.IssueBody = runRow.IssueContext.Body
		// Map the cached comments so the plan-review prompt renders the
		// same comment-borne refinements the planner saw (#622) — identical
		// shape to fillIssueContext branch 1.
		for _, c := range runRow.IssueContext.Comments {
			trig.IssueComments = append(trig.IssueComments, prompt.IssueComment{
				Author:    c.Author,
				Body:      c.Body,
				CreatedAt: c.CreatedAt,
			})
		}
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

	// Pending-signal (#600): now that a reviewer will actually run
	// (agent>0 AND PlanReviewer wired), emit a plan_review_started audit
	// entry. This is the only MCP-readable proxy that distinguishes a
	// configured-and-running review ('pending') from no review configured
	// ('none'). It is emitted synchronously here, before the dispatch loop
	// below appends the terminal plan_reviewed entries, so started always
	// has a lower audit sequence than reviewed under both gating
	// (synchronous) and advisory (detached) authority. Best-effort:
	// WARN-log and continue on append failure so dispatch is never blocked.
	s.emitReviewStarted(ctx, runID, stageID, "plan_review_started", authority, reviewersCfg.AgentCount(), "")

	// Resolve the per-invocation reviewer list (#955) up front so the
	// detached goroutine closes over fully-resolved adapters, never the
	// spec config or request-scoped state.
	invocations := s.resolveReviewerInvocations(reviewersCfg)

	// Detach the reviewer context from the request lifecycle (#584).
	// context.WithoutCancel keeps the parent's values but is NOT
	// cancelled when the parent is — so the review survives the runner's
	// upload client disconnecting (which cancels r.Context()) and the
	// handler returning. The reviewer adapter's own
	// context.WithTimeout(reviewCtx, cfg.Timeout) then becomes the
	// effective per-invocation bound (FISHHAWKD_PLAN_REVIEW_TIMEOUT).
	authorModel := parsedPlan.GeneratedBy.Model
	reviewCtx := context.WithoutCancel(ctx)

	// Advisory: dispatch detached so the upload returns promptly. The
	// goroutine closes over only already-resolved values (built prompt,
	// IDs, authority, author model) — never r or request-scoped state.
	if authority != planreview.AuthorityGating {
		s.bgReviews.Add(1)
		go func() {
			defer s.bgReviews.Done()
			s.runPlanReviewLoop(reviewCtx, runID, stageID, invocations, authority, promptText, authorModel)
		}()
		return false
	}

	// Gating: run synchronously so the failed-B transition lands before
	// the trace handler advances the stage.
	hasRejection := s.runPlanReviewLoop(reviewCtx, runID, stageID, invocations, authority, promptText, authorModel)
	if hasRejection {
		cat := run.FailureB
		reason := "plan_review_rejected: agent review verdict reject under gating authority"
		if _, terr := s.cfg.RunRepo.TransitionStage(reviewCtx, stageID,
			run.StageStateFailed, &run.StageCompletion{
				FailureCategory: &cat,
				FailureReason:   &reason,
			}); terr != nil {
			s.cfg.Logger.LogAttrs(reviewCtx, slog.LevelWarn,
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

// planGateEvidence maps the plan-gate result payloads handleShipPlan's
// synchronous checks return into the prompt-side evidence struct (#963).
// The server owns the mapping so the prompt package stays free of a
// policy/server import — the same pattern the trace handler uses to map
// bundle structs into prompt fields. Returns nil when neither gate
// produced a result (both failed open) so the plan-review prompt stays
// byte-identical to the pre-#963 output.
func planGateEvidence(precheck *ScopePrecheckPayload, sweep *SurfaceSweepPayload) *prompt.PlanGateEvidence {
	if precheck == nil && sweep == nil {
		return nil
	}
	ev := &prompt.PlanGateEvidence{}
	if precheck != nil {
		pc := &prompt.ScopePrecheckEvidence{
			ImplementStageID: precheck.ImplementStageID,
			ScannedFiles:     precheck.ScannedFiles,
			MaxFilesChanged:  precheck.MaxFilesChanged,
		}
		for _, v := range precheck.Violations {
			pc.Violations = append(pc.Violations, prompt.GateViolation{
				Constraint: v.Constraint,
				Detail:     v.Detail,
				Files:      v.Files,
			})
		}
		ev.ScopePrecheck = pc
	}
	if sweep != nil {
		sw := &prompt.SurfaceSweepEvidence{ScannedFiles: sweep.ScannedFiles}
		for _, f := range sweep.Findings {
			sw.Findings = append(sw.Findings, prompt.SurfaceSweepFindingEvidence{
				Pattern:         f.Pattern,
				TriggerPath:     f.TriggerPath,
				MissingSiblings: f.MissingSiblings,
			})
		}
		ev.SurfaceSweep = sw
	}
	return ev
}

// emitReviewStarted appends a best-effort *_review_started audit entry at
// review dispatch (#600). Shared by the plan-review and implement-review
// paths. It is called only on the branch where a reviewer will actually
// run (agent>0 AND PlanReviewer wired) — never for the agent==0 ('none')
// or nil-reviewer ('skipped') branches — so a consumer can read the entry
// as proof that a review is pending rather than absent. Mirrors the
// skipped-entry error handling: WARN-log and continue on append failure
// so the dispatch path is never blocked.
func (s *Server) emitReviewStarted(ctx context.Context, runID, stageID uuid.UUID, category string, authority planreview.AuthorityMode, configuredAgents int, headSHA string) {
	if s.cfg.AuditRepo == nil {
		return
	}
	payload, _ := json.Marshal(planreview.ReviewStartedPayload{
		ConfiguredAgents: configuredAgents,
		Authority:        authority,
		// Empty for the plan path (no diff/head_sha; omitempty keeps the
		// payload byte-identical); the implement path passes the bundle's
		// verify_run head_sha as the #797 dedup key.
		HeadSHA: headSHA,
	})
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review: append "+category+" audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("error", aerr.Error()),
		)
	}
}

// emitReviewFailed appends a best-effort terminal *_review_failed audit
// entry when a wired reviewer invocation errors or times out (#664). Shared
// by the plan-review (plan.go) and implement-review (trace.go) error
// branches via the passed category ("plan_review_failed" /
// "implement_review_failed"). It mirrors emitReviewStarted's contract:
// ActorKind=system, best-effort, WARN-log on append failure so the review
// loop is never blocked. Observability-only — it records that a reviewer
// failed without altering gating advance/degrade semantics (#574). timeout
// sets the #747 discriminator: true when the reviewer was killed by the
// size-aware per-invocation budget deadline, false for other failures.
func (s *Server) emitReviewFailed(ctx context.Context, runID, stageID uuid.UUID, category string, authority planreview.AuthorityMode, model, reason string, timeout bool) {
	if s.cfg.AuditRepo == nil {
		return
	}
	payload, _ := json.Marshal(planreview.ReviewFailedPayload{
		Reason:        reason,
		ReviewerModel: model,
		Authority:     authority,
		Timeout:       timeout,
	})
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review: append "+category+" audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("error", aerr.Error()),
		)
	}
}

// runPlanReviewLoop runs the per-reviewer plan-review loop shared by the
// synchronous (gating) and detached (advisory) dispatch paths. For each
// resolved invocation it calls its reviewer's Review, logs WARN on
// self-review (reviewer model == authorModel), and appends one
// plan_reviewed audit entry. Returns true when at least one verdict is
// reject. It performs no stage transition — the gating caller owns the
// failed-B transition so the advance-blocking edge stays on the
// synchronous path only.
//
// ctx is the detached review context: per-invocation errors (including a
// reviewer whose provider failed to resolve) are WARN-logged and skipped
// so a transient reviewer failure doesn't strand the loop.
func (s *Server) runPlanReviewLoop(ctx context.Context, runID, stageID uuid.UUID, invocations []reviewerInvocation, authority planreview.AuthorityMode, promptText, authorModel string) bool {
	systemKind := audit.ActorKind("system")
	hasRejection := false
	budget := s.cfg.ReviewBudget.Budget(len(promptText))
	for i, inv := range invocations {
		// An unresolvable provider (#955) is handled like a failed
		// invocation: terminal *_review_failed entry, loop continues,
		// hasRejection untouched.
		if inv.resolveErr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: reviewer provider unresolved",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.Int("reviewer_index", i),
				slog.String("provider", inv.provider),
				slog.String("error", inv.resolveErr.Error()),
			)
			s.emitReviewFailed(ctx, runID, stageID, "plan_review_failed", authority, inv.specModel, inv.resolveErr.Error(), false)
			continue
		}
		// Apply the size-aware per-invocation budget (#747) as a context
		// deadline so a large diff gets proportionally more wall-clock. The
		// claudecode adapter honours this incoming deadline instead of capping
		// at its own cfg.Timeout. cancel() is called directly each turn (not a
		// deferred stack) so deadlines don't accumulate across reviewers.
		invocationCtx, cancel := context.WithTimeout(ctx, budget)
		verdict, model, err := inv.reviewer.Review(invocationCtx, promptText)
		timedOut := errors.Is(invocationCtx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: reviewer invocation failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.Int("reviewer_index", i),
				slog.Bool("timed_out", timedOut),
				slog.Duration("budget", budget),
				slog.String("error", err.Error()),
			)
			// Emit a terminal plan_review_failed audit entry (#664) so a
			// timed-out / errored reviewer is observable as a definite
			// 'failed' state rather than an ambiguous 'pending'. The timeout
			// discriminator (#747) tells a budget-kill apart from a transport
			// failure. hasRejection is deliberately untouched — gating
			// advance/degrade semantics are unchanged (#574); observability-only.
			s.emitReviewFailed(ctx, runID, stageID, "plan_review_failed", authority, model, err.Error(), timedOut)
			continue
		}

		// Self-review guard (ADR-027): warn when the review agent's
		// model matches the plan author's model. Warn-only per ADR;
		// the verdict is still recorded.
		if model != "" && model == authorModel {
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
		entry, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     runID,
			StageID:   &stageID,
			Timestamp: time.Now().UTC(),
			Category:  "plan_reviewed",
			ActorKind: &systemKind,
			Payload:   payloadBytes,
		})
		if aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: append audit entry failed",
				slog.String("run_id", runID.String()),
				slog.String("error", aerr.Error()),
			)
		} else if entry != nil {
			// Persist the verdict's concerns with stable IDs (#964) using
			// the sequence the append returned; a failed append (no
			// sequence) skips persistence for this verdict.
			s.persistReviewConcerns(ctx, runID, stageID, concern.StageKindPlan, model, entry.Sequence, verdict.Concerns)
		}

		// Capture this reviewer invocation's agent token cost (#681). The
		// usage rode in on the planreview.ReviewVerdict contract; we price
		// and record it here, backend-agnostically.
		s.recordReviewerCost(ctx, runID, stageID, model, verdict.Usage, "plan_review")

		if verdict.Verdict == planreview.VerdictReject {
			hasRejection = true
		}
	}
	return hasRejection
}

// resolveStageReviewers reads the run's workflow spec and returns the
// ReviewersConfig for the first stage of the given type in the active
// workflow. Returns nil when the spec is absent, unparseable, or the
// workflow has no stage of that type. Shared by the plan-review and
// implement-review paths (ADR-027 impl 1/2 + 2/2).
func (s *Server) resolveStageReviewers(ctx context.Context, runRow *run.Run, stageType spec.StageType) *spec.ReviewersConfig {
	if runRow.WorkflowSpec == nil {
		return nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "stage review: parse workflow spec failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("stage_type", string(stageType)),
			slog.String("error", err.Error()),
		)
		return nil
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return nil
	}
	for _, st := range wf.Stages {
		if st.Type == stageType {
			return st.Reviewers
		}
	}
	return nil
}
