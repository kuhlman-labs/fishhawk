package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// maxTraceBundleBytes mirrors the runner's bundle.MaxBundleBytes so
// the runner and backend agree on the gzipped payload ceiling. v0
// trace volumes are bounded by token budgets; this is the
// belt-and-suspenders cap that protects backend storage from a
// runaway agent.
const maxTraceBundleBytes = 64 * 1024 * 1024

// traceUploadResponse is the 202 body in docs/api/v0.openapi.yaml
// for `POST /v0/runs/{run_id}/trace`. Returns the (run, stage,
// variant, content_hash) tuple so the runner can confirm the
// backend stored the same bytes it sent and stash the hash for
// later cross-reference.
type traceUploadResponse struct {
	RunID       uuid.UUID `json:"run_id"`
	StageID     uuid.UUID `json:"stage_id"`
	Variant     string    `json:"variant"`
	ContentHash string    `json:"content_hash"`
}

// handleShipTrace implements POST /v0/runs/{run_id}/trace.
//
// Auth is the Ed25519 signature itself: the runner produced the
// signature with the per-run private key (issued by the
// signing-key endpoint); the backend looks up the stored public
// half and verifies. A forged or expired signature → 401, no
// audit log entry, no bundle stored.
//
// On success the handler writes a kind=trace_uploaded audit
// entry via AppendChained — the chain hash links the upload event
// to the run's prior audit history, so a tampered or replayed
// trace can't slip into an unrelated run's chain.
func (s *Server) handleShipTrace(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.TraceStore == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "trace_upload_unconfigured",
			"trace upload requires signing, tracestore, and audit to be configured", nil)
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

	variant := tracestore.Variant(r.URL.Query().Get("variant"))
	if !variant.Valid() {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"variant must be raw or redacted",
			map[string]any{"field": "variant", "got": string(variant)})
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

	body, err := io.ReadAll(io.LimitReader(r.Body, maxTraceBundleBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxTraceBundleBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"trace bundle exceeds size cap",
			map[string]any{"limit_bytes": maxTraceBundleBytes})
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

	contentHash := sha256Hex(body)
	ref := tracestore.BundleRef{
		RunID:       runID,
		Variant:     variant,
		ContentHash: contentHash,
	}
	if err := s.cfg.TraceStore.Put(r.Context(), ref, bytes.NewReader(body)); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"store trace bundle failed", map[string]any{"error": err.Error()})
		return
	}

	// Look up the run's recorded runner_kind so the audit payload
	// can attest it (ADR-022 / #388). The dispatcher / handleCreateRun
	// stamps this at run-create time; the trace handler reads and
	// records it. The runner never self-declares — its claim would
	// be unverifiable.
	//
	// Best-effort: when RunRepo isn't wired (legacy / minimal
	// config) or the lookup fails, omit the field from the audit
	// payload. Readers treat missing as legacy per ADR-022's
	// back-compat semantics (default `github_actions`). Surfacing
	// an honest "we don't know" beats stamping a default that
	// might be wrong.
	auditFields := map[string]any{
		"run_id":       runID.String(),
		"stage_id":     stageID.String(),
		"variant":      string(variant),
		"content_hash": contentHash,
		"size_bytes":   len(body),
	}
	if s.cfg.RunRepo != nil {
		if runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err == nil && runRow.RunnerKind != "" {
			auditFields["runner_kind"] = runRow.RunnerKind
		}
	}

	// Audit: append a chained entry tying the upload to this run's
	// prior history. AppendChained holds a row-lock on runs so two
	// concurrent uploads can't fork the chain.
	auditPayload, _ := json.Marshal(auditFields)
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "trace_uploaded",
		ActorKind: &systemKind,
		Payload:   auditPayload,
	}); err != nil {
		// Bundle is already stored; failing here would leave us
		// with a stored bundle and no audit record. We surface
		// 500 so the runner retries (idempotent at the storage
		// layer) and the audit row eventually lands.
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// Advance the stage so the approval handler can act on it.
	// The state machine requires dispatched → running →
	// awaiting_approval; we walk both transitions here because
	// the runner's trace upload IS the "started executing"
	// signal in v0 (the runner doesn't separately check in).
	//
	// v0 hardcodes every agent stage as gated (per MVP_SPEC
	// §4.2's example); the gateless case (running → succeeded
	// directly) lands when the workflow spec carries a
	// per-stage `gates: []` signal.
	//
	// We only attempt transitions when the RunRepo is wired —
	// trace upload doesn't strictly require it (signing +
	// tracestore are the load-bearing deps), so a deployment
	// that's not yet on Postgres still accepts uploads.
	//
	// Failures here are logged but don't unwind the upload: the
	// trace is already stored + audited; a stuck stage is
	// surface-able via GET /v0/runs/{id}/stages and recoverable
	// via a follow-up call once the orchestrator wraps this.
	// Re-evaluate policy on the diff carried in the bundle. The
	// runner already evaluated client-side; the backend's verdict
	// is the auditable source of truth (per MVP_SPEC §4.4 +
	// E3.13). On violations the stage transitions to failed-B
	// instead of awaiting_approval.
	// Read the manifest's agent_failed signal (E8.5). When the
	// runner stamped a category-A failure, fail the stage as A and
	// skip both the policy re-evaluation (no plan exists yet, by
	// definition) and the awaiting_approval path (no plan to
	// review). Best-effort: a missing or unparsable manifest falls
	// through to the existing policy + advance flow rather than
	// 500ing the upload — the bundle is already stored, the audit
	// row is already written, and a stuck stage is recoverable.
	if s.cfg.RunRepo != nil {
		if manifest, err := bundle.ExtractManifest(body); err == nil && manifest.AgentFailed {
			reason := manifest.AgentFailureReason
			if reason == "" {
				reason = "agent invocation failed (no reason supplied)"
			}
			if _, err := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureA, reason); err != nil {
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
					"trace upload: transition to failed-A failed",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", stageID.String()),
					slog.String("error", err.Error()))
			}
			// Advance so the orchestrator walks the run to its
			// terminal state — without this the run stays in
			// pending forever once a stage fails. Best-effort:
			// the audit row for the failed stage is the canonical
			// signal; a stuck run is recoverable.
			s.advanceAfterFailure(r, runID, stageID)
			s.writeJSON(w, r, http.StatusAccepted, traceUploadResponse{
				RunID:       runID,
				StageID:     stageID,
				Variant:     string(variant),
				ContentHash: contentHash,
			})
			return
		}
	}

	policyPassed := true
	if s.cfg.RunRepo != nil {
		policyPassed = s.reEvaluatePolicy(r, runID, stageID, body)
	}
	if s.cfg.RunRepo != nil {
		if policyPassed {
			s.advanceStageAfterTrace(r, runID, stageID)
		} else {
			s.failStageCategoryB(r, runID, stageID, "policy violations on backend re-evaluation")
		}
	}

	s.writeJSON(w, r, http.StatusAccepted, traceUploadResponse{
		RunID:       runID,
		StageID:     stageID,
		Variant:     string(variant),
		ContentHash: contentHash,
	})
}

// advanceStageAfterTrace walks the stage out of `dispatched`. The
// terminal target depends on the stage's spec:
//
//   - RequiresApproval = true  → dispatched → running → awaiting_approval
//   - RequiresApproval = false → dispatched → running → succeeded; orchestrator picks it up
//
// Each step is idempotent (the state machine allows same-state
// re-application), so a redelivered trace upload or a parallel
// transition path doesn't fault. Errors at any step log but don't
// unwind: the trace is already stored + audited, and the stage's
// state is recoverable via GET /v0/runs/{id}/stages.
func (s *Server) advanceStageAfterTrace(r *http.Request, runID, stageID uuid.UUID) {
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"trace upload: get stage for transition failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	// Local-runner runs skip the GHA dispatcher's
	// pending → dispatched step (there's no workflow_dispatch fire
	// to gate on), so the stage arrives here in `pending`. Walk it
	// through dispatched first so the rest of the chain — which
	// the state machine forbids from pending — stays uniform.
	if stage.State == run.StageStatePending {
		if _, err := s.cfg.RunRepo.TransitionStage(r.Context(), stageID,
			run.StageStateDispatched, nil); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"trace upload: transition to dispatched failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()),
			)
			return
		}
	}

	if _, err := s.cfg.RunRepo.TransitionStage(r.Context(), stageID,
		run.StageStateRunning, nil); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"trace upload: transition to running failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	terminal := run.StageStateAwaitingApproval
	if !stage.RequiresApproval {
		terminal = run.StageStateSucceeded
	}
	if _, err := s.cfg.RunRepo.TransitionStage(r.Context(), stageID,
		terminal, nil); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"trace upload: transition to terminal failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("target", string(terminal)),
			slog.String("error", err.Error()),
		)
		return
	}

	// Gateless stages don't get an approval submission to drive the
	// next dispatch — fire the orchestrator ourselves so the next
	// stage (typically a human-led review) gets picked up. Best-
	// effort: a failure here logs but doesn't unwind the upload.
	if !stage.RequiresApproval && s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"trace upload: orchestrator advance after gateless stage failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	// Plan-ready comment-back (#234). Fires only after a plan stage
	// transitions terminally and only for issue-triggered runs (the
	// notifier itself short-circuits on non-issue triggers + on
	// repeat calls via its audit-log dedup). Best-effort: errors
	// log but don't unwind the upload — the run's plan is already
	// attached, the stage is already advanced.
	if stage.Type == run.StageTypePlan {
		s.notifyPlanReady(r.Context(), runID, stage)
	}

	// Sticky status comment (E20.4 / #330). Every terminal stage
	// transition is a state change worth surfacing — plan terminal,
	// implement terminal, review terminal, etc. The notifier itself
	// short-circuits for non-issue triggers; for issue triggers it
	// edits the seeded comment in place.
	s.notifyStatusUpdate(r.Context(), runID, "trace_handler")
}

// notifyPlanReady fires the plan-ready comment-back hook after a
// plan stage transitions terminally (#234). Pulls the most-recent
// standard_v1 plan artifact for the run and hands it to the
// notifier. Best-effort: errors log but never unwind the
// transition. Skips silently when:
//
//   - The notifier isn't wired (no GitHub client / no ExternalURL).
//   - No standard_v1 plan artifact is attached to the run yet — the
//     runner may have transitioned the stage before posting the
//     plan body. (Today the trace upload happens after the plan
//     POST, but defensive against that ordering changing.)
//   - The notifier itself decides to skip (non-issue-triggered run,
//     dedup hit).
func (s *Server) notifyPlanReady(ctx context.Context, runID uuid.UUID, stage *run.Stage) {
	if s.issueNotifier == nil {
		return
	}
	planArtifact, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"plan-ready notify: load plan failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	if planArtifact == nil {
		// Most-likely cause: the runner ships trace before plan
		// (current order), so the trace-handler hook runs before
		// the plan artifact lands. The plan-upload handler will
		// re-fire this hook when its turn comes (#245). The
		// notifier's audit-log dedup ensures only one comment
		// posts. Logged at debug level so the next time this
		// branch fires for an unexpected reason (e.g. the runner's
		// upload order changed and the plan never lands) the
		// absence is recoverable from logs instead of silent.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
			"plan-ready notify: no plan artifact yet — will retry on plan-upload hook",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
		)
		return
	}
	if err := s.issueNotifier.NotifyPlanReady(ctx, runID, stage, planArtifact); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"plan-ready notify: comment-back failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// notifyStatusUpdate is the best-effort sticky-comment hook called
// from every meaningful transition (E20.4 / #330). The notifier
// short-circuits for non-issue triggers + handles its own dedup +
// 404-recovery; here we just call it and log on failure. The state
// machine is authoritative; the comment is a UI mirror.
//
// `source` is a short tag identifying the call site (e.g.
// "trace_handler", "approval_submit", "pr_merged"). It lands in the
// log line as a slog attribute so operators tailing logs can pinpoint
// which transition tripped a notify failure.
func (s *Server) notifyStatusUpdate(ctx context.Context, runID uuid.UUID, source string) {
	if s.issueNotifier == nil {
		return
	}
	if err := s.issueNotifier.NotifyStatusUpdateForRun(ctx, runID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"status comment update failed",
			slog.String("source", source),
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// reEvaluatePolicy is the backend's source-of-truth re-evaluation
// of the closed-set constraints (E3.13). Returns true (policy
// passed) when:
//
//   - the spec lookup fails (best-effort: we don't black-hole a
//     stage because GitHub flapped, just log and proceed)
//   - the stage type isn't in the parsed spec
//   - EmitEvaluation returns no violations (including the cases
//     where the diff is empty and / or no constraints apply —
//     those still emit a passed=true audit entry per #247 so the
//     SPA's policy section can render the pass state instead of
//     "pending")
//
// Returns false only when EmitEvaluation produces one or more
// violations. The caller transitions the stage accordingly.
//
// Always tries to emit a policy_evaluated audit entry when we
// reach a state that has either constraints or a diff — even if
// one is empty. The SPA reads that entry to render the policy
// section (#233); a missing entry is what makes the section
// stuck on "pending" (#247).
func (s *Server) reEvaluatePolicy(r *http.Request, runID, stageID uuid.UUID, bundleBytes []byte) bool {
	ctx := r.Context()
	logger := s.cfg.Logger

	stage, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "policy: get stage failed",
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return true
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "policy: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return true
	}

	stageType := string(stage.Type)

	// Diff extraction: ErrNoDiffEvent is a known-empty case, emit
	// skipped with no_diff_in_bundle so the SPA can render the
	// reason rather than "pending" (#283). Other ExtractDiff errors
	// also flow through the skip path — the diff is unparseable, but
	// the audit story is "we tried; here's why we couldn't."
	diff, diffErr := bundle.ExtractDiff(bundleBytes)
	if diffErr != nil {
		reason := policy.SkipNoDiffInBundle
		s.emitPolicySkipped(ctx, runID, stageID, stageType, reason, diffErr.Error())
		return true
	}

	// Constraints: load from the cached spec on the run row (#283).
	// Failures are categorized so the audit signals the right cause.
	constraints, skipReason, skipDetail, ok := s.loadStageConstraintsFromCache(runRow, stageType)
	if !ok {
		logger.LogAttrs(ctx, slog.LevelWarn, "policy: load constraints skipped",
			slog.String("run_id", runID.String()),
			slog.String("stage_type", stageType),
			slog.String("skip_reason", string(skipReason)),
			slog.String("detail", skipDetail))
		s.emitPolicySkipped(ctx, runID, stageID, stageType, skipReason, skipDetail)
		return true
	}

	// Happy path: real evaluation. EmitEvaluation handles the empty-
	// constraints case cleanly — Evaluate returns no violations, the
	// row carries Applied={} and Passed=true. SPA renders "Policy
	// passed · No constraints configured."
	violations, err := policy.EmitEvaluation(ctx, s.cfg.AuditRepo,
		runID, stageID, stageType, diff, constraints, nil)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "policy: emit evaluation failed",
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return true
	}
	return len(violations) == 0
}

// emitPolicySkipped is a thin wrapper around policy.EmitEvaluationSkipped
// that logs the audit-write failure as WARN rather than returning it.
// The trace upload is already stored at this point; failing the
// handler just for a skipped-emit doesn't help anyone.
func (s *Server) emitPolicySkipped(ctx context.Context, runID, stageID uuid.UUID,
	stageType string, reason policy.SkipReason, detail string) {
	if err := policy.EmitEvaluationSkipped(ctx, s.cfg.AuditRepo,
		runID, stageID, stageType, reason, detail); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "policy: emit skipped audit failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("skip_reason", string(reason)),
			slog.String("error", err.Error()))
	}
}

// loadStageConstraintsFromCache reads constraints from the cached
// workflow spec on the run row (#283). Returns (constraints, "", "",
// true) on success and (zero, reason, detail, false) when the cache
// can't yield constraints — the caller emits a skip audit with the
// reason and treats the stage as policy-pass.
//
// Pre-#283 this called GitHub directly using `runRow.WorkflowSHA` as
// the contents-API `ref`, but that's a blob SHA, not a commit / branch
// ref — GitHub returned 404, the function errored, and the trace
// handler skipped the audit emission, leaving the SPA's <PolicySection>
// stuck on "pending."
func (*Server) loadStageConstraintsFromCache(runRow *run.Run, stageType string) (
	policy.Constraints, policy.SkipReason, string, bool,
) {
	if len(runRow.WorkflowSpec) == 0 {
		// Legacy row created before #283's migration, or a CLI / UI
		// flow that didn't fetch a spec. No constraints to evaluate
		// against; surface the reason in the audit instead of going
		// silent.
		return policy.Constraints{}, policy.SkipSpecUnavailable,
			"run row has no cached workflow spec (legacy or non-dispatcher flow)", false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return policy.Constraints{}, policy.SkipSpecUnparseable, err.Error(), false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return policy.Constraints{}, policy.SkipWorkflowNotInSpec,
			fmt.Sprintf("workflow %q not in cached spec", runRow.WorkflowID), false
	}
	for _, stg := range wf.Stages {
		if string(stg.Type) == stageType {
			return mergeConstraints(stg.Constraints), "", "", true
		}
	}
	return policy.Constraints{}, policy.SkipStageNotInSpec,
		fmt.Sprintf("stage_type %q not in workflow %q", stageType, runRow.WorkflowID), false
}

// mergeConstraints folds the spec's []spec.Constraint (each entry a
// single-rule object per the schema's maxProperties:1) into one
// policy.Constraints. Repeated rules are unioned at the slice level
// (forbidden_paths from multiple entries concatenate); scalar rules
// (max_files_changed) take the most restrictive when more than one
// is set.
func mergeConstraints(in []spec.Constraint) policy.Constraints {
	var out policy.Constraints
	for _, c := range in {
		if len(c.ForbiddenPaths) > 0 {
			out.ForbiddenPaths = append(out.ForbiddenPaths, c.ForbiddenPaths...)
		}
		if len(c.AllowedPaths) > 0 {
			out.AllowedPaths = append(out.AllowedPaths, c.AllowedPaths...)
		}
		if c.MaxFilesChanged > 0 {
			if out.MaxFilesChanged == 0 || c.MaxFilesChanged < out.MaxFilesChanged {
				out.MaxFilesChanged = c.MaxFilesChanged
			}
		}
		if len(c.RequiredOutcomes) > 0 {
			out.RequiredOutcomes = append(out.RequiredOutcomes, c.RequiredOutcomes...)
		}
	}
	return out
}

func isEmptyConstraints(c policy.Constraints) bool {
	return len(c.ForbiddenPaths) == 0 &&
		len(c.AllowedPaths) == 0 &&
		c.MaxFilesChanged == 0 &&
		len(c.RequiredOutcomes) == 0
}

// failStageCategoryB transitions the stage to failed with category
// B (constraint/policy violation per MVP_SPEC §6). Delegates to the
// shared run.FailStage helper, which walks dispatched → running →
// failed if needed. Failures are logged but don't unwind — the
// policy_evaluated audit entry is the primary signal; a stuck stage
// is recoverable.
//
// After the stage transitions to failed, advance the run so the
// orchestrator walks pending → running → failed. Without this the
// run stays in pending forever once any stage fails.
func (s *Server) failStageCategoryB(r *http.Request, runID, stageID uuid.UUID, reason string) {
	if _, err := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureB, reason); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"trace upload: transition to failed-B failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
	s.advanceAfterFailure(r, runID, stageID)
}

// advanceAfterFailure invokes the orchestrator to walk the run's
// state machine after a stage has transitioned terminally. Errors
// are logged but never unwind the caller — the audit log is the
// canonical signal that the stage failed; a stuck run is the kind
// of bug we want surfaced via /v0/runs (state != pending) rather
// than via a 500 on the upload that already wrote the audit row.
func (s *Server) advanceAfterFailure(r *http.Request, runID, stageID uuid.UUID) {
	if s.cfg.Orchestrator == nil {
		return
	}
	if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"trace upload: orchestrator advance after stage failure failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// handleGetStageTrace implements GET /v0/stages/{stage_id}/trace (#218).
//
// Streams the redacted trace bundle for the stage as gzipped JSONL
// bytes. The SPA gunzips via the browser's auto-decompression and
// parses line-by-line to render the agent transcript.
//
// "Most-recent" is the right shape because retried stages can ship
// multiple bundles; we want the latest. Audit log is the source of
// truth for the (stage_id, content_hash) mapping — the bundle key
// itself doesn't carry stage_id, so we read the trace_uploaded
// audit entries for the run, filter to this stage + redacted
// variant, take the highest-sequence one, and resolve to a
// BundleRef.
//
// Raw variant is intentionally not exposed here: it can carry
// secrets in agent output and lives behind S3 Object Lock for
// compliance reasons. Surfacing raw is a separate decision (see
// #218 "Out of scope").
func (s *Server) handleGetStageTrace(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.TraceStore == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "trace_unconfigured",
			"trace endpoint requires run, audit, and tracestore repos to be configured", nil)
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

	entries, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), stage.RunID, "trace_uploaded")
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list trace audit entries failed", map[string]any{"error": err.Error()})
		return
	}

	contentHash, ok := pickRedactedTraceHash(entries, stageID)
	if !ok {
		s.writeError(w, r, http.StatusNotFound, "trace_not_found",
			"no redacted trace bundle uploaded for this stage yet",
			map[string]any{"stage_id": stageID.String()})
		return
	}

	body, err := s.cfg.TraceStore.Get(r.Context(), tracestore.BundleRef{
		RunID:       stage.RunID,
		Variant:     tracestore.VariantRedacted,
		ContentHash: contentHash,
	})
	if err != nil {
		if errors.Is(err, tracestore.ErrNotFound) {
			// Audit row says we should have a bundle but storage
			// disagrees — surface 410 so callers don't keep hammering
			// the same stage hoping the storage catches up. The
			// audit chain is the canonical record; the storage gap
			// needs a human (or the verifier).
			s.writeError(w, r, http.StatusGone, "trace_storage_missing",
				"audit log references a trace bundle that is no longer in storage",
				map[string]any{"stage_id": stageID.String(), "content_hash": contentHash})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get trace bundle failed", map[string]any{"error": err.Error()})
		return
	}
	defer func() {
		_ = body.Close()
	}()

	// Set Content-Encoding: gzip so the browser auto-decompresses on
	// fetch — the bytes on disk are gzipped JSONL, the SPA wants
	// JSONL. Content-Type advertises the inner format. Disposition
	// is inline (the SPA reads the body via fetch; download isn't a
	// supported flow yet).
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("X-Fishhawk-Content-Hash", contentHash)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		// Headers already written; we can't return an error envelope.
		// Log and let the connection drop.
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "trace stream copy failed",
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// pickRedactedTraceHash walks the run's trace_uploaded audit entries
// and returns the most recent redacted variant's content hash for
// the given stage. Returns false when none match.
//
// The audit entries are sequence-ascending (per ListForRunByCategory's
// contract), so we iterate in reverse to find the latest match.
func pickRedactedTraceHash(entries []*audit.Entry, stageID uuid.UUID) (string, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			Variant     string `json:"variant"`
			ContentHash string `json:"content_hash"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.Variant != string(tracestore.VariantRedacted) {
			continue
		}
		if len(payload.ContentHash) != 64 {
			continue
		}
		return payload.ContentHash, true
	}
	return "", false
}
