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
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/budget"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/cost"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spendalert"
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

	// Cost rollup (#649). Compute the estimated cost of this bundle's
	// model usage from the SIGNED manifest's token counts (not from a
	// runner-emitted span — a tampered/dropped span can't corrupt the
	// ledger), write a cost_recorded audit entry, and accumulate the
	// per-run total. Best-effort: the trace is already stored + audited,
	// so a manifest-parse / audit-write / rollup failure logs at WARN
	// and never unwinds the upload.
	//
	// Gate on the raw variant so cost is recorded exactly once per stage
	// bundle (#678). The runner POSTs both the raw and the redacted
	// variant of the same bundle with identical signed manifest token
	// counts; recording on every variant double-counted the cost (2x).
	// Raw is the first/authoritative upload and is always shipped in v0
	// (tracestore always stores raw; only its exposure is restricted —
	// see the get-stage-trace handler), so gating on raw records the cost
	// on the bundle's canonical upload.
	if variant == tracestore.VariantRaw {
		s.recordCost(r.Context(), runID, stageID, body)

		// Per-run budget tripwire (ADR-030 / #653). After the bundle's cost
		// is rolled into the run total, check it against the operator's
		// per-run ceilings. On breach the run is HALTED (cancelled) and we
		// short-circuit here so the stage-advancement block below never runs
		// — no further stage is dispatched for a run the system has stopped.
		if s.checkRunBudget(r.Context(), runID, stageID) {
			s.writeJSON(w, r, http.StatusAccepted, traceUploadResponse{
				RunID:       runID,
				StageID:     stageID,
				Variant:     string(variant),
				ContentHash: contentHash,
			})
			return
		}
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
			// Best-effort: emit runtime calibration for implement
			// stages that failed agent-side. Errors logged at WARN
			// and do not unwind the upload.
			s.emitRuntimeObserved(r.Context(), runID, stageID, body, "failed")
			s.writeJSON(w, r, http.StatusAccepted, traceUploadResponse{
				RunID:       runID,
				StageID:     stageID,
				Variant:     string(variant),
				ContentHash: contentHash,
			})
			return
		}
	}

	var violations []policy.Violation
	if s.cfg.RunRepo != nil {
		violations = s.reEvaluatePolicy(r, runID, stageID, body)
	}
	if s.cfg.RunRepo != nil {
		switch {
		case len(violations) == 0:
			s.advanceStageAfterTrace(r, runID, stageID, body)
		case s.noDiffCaptured(r.Context(), stageID, body, violations):
			// #691/#692: an implement stage whose bundle carries a
			// present-but-empty diff is a staging/capture miss, not a
			// genuine constraint breach. Route it to retryable
			// category-C so the existing failed → pending retry path
			// gives operators an escape hatch rather than dooming the
			// stage (and any fan-out parent) on an un-redrivable B.
			s.failStageCategoryC(r, runID, stageID,
				"no_diff_captured: implement stage produced an empty diff; nothing was staged or captured (retryable; see #691/#692)",
				body)
		default:
			s.failStageCategoryB(r, runID, stageID, "policy violations on backend re-evaluation", body)
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
//
// bundleBytes is passed through for runtime calibration emit after
// the terminal transition.
func (s *Server) advanceStageAfterTrace(r *http.Request, runID, stageID uuid.UUID, bundleBytes []byte) {
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

	// Implement-review hook (ADR-027 impl 2/2). After the diff has
	// landed (the bundle carries it) and the stage is running, but
	// BEFORE the terminal awaiting_approval/succeeded transition, spawn
	// implement-review agent(s) when this is an implement stage with
	// reviewers.agent>0. A gating reject (human==0) fails the stage as
	// category-B and returns here, so the terminal transition below is
	// blocked by the state machine — mirroring runPlanReviews' contract.
	//
	// Guarded on stage.Type so the default trace path is byte-identical
	// for every non-implement stage; runImplementReviews itself
	// short-circuits when reviewers.agent==0, so reviewer-less implement
	// runs are unaffected too.
	if stage.Type == run.StageTypeImplement {
		// Diff source is the trace bundle, regardless of whether a PR was
		// opened (local --no-pr runs still carry the git_diff event). An
		// extraction error skips review and proceeds to the terminal
		// transition rather than failing the stage.
		if diff, derr := bundle.ExtractDiff(bundleBytes); derr == nil {
			// Surface runner-reported scope_drift to the reviewer so an
			// operator-staged required file in a drifted path is not
			// false-rejected as missing (#695). Best-effort: an extraction
			// error logs at WARN and falls back to nil drift — never blocks
			// the review (the standing anti-false-reject rule in the prompt
			// degrades gracefully when the list is empty).
			scopeDrift, sderr := bundle.ExtractScopeDrift(bundleBytes)
			if sderr != nil {
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
					"trace upload: extract scope drift failed — proceeding with no drift list",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", stageID.String()),
					slog.String("error", sderr.Error()),
				)
				scopeDrift = nil
			}
			if s.runImplementReviews(r.Context(), runID, stageID, diff, scopeDrift) {
				cat := run.FailureB
				reason := "implement_review_rejected: agent review verdict reject under gating authority"
				if _, ferr := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, cat, reason); ferr != nil {
					s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
						"trace upload: transition to failed-B after implement-review gating reject failed",
						slog.String("run_id", runID.String()),
						slog.String("stage_id", stageID.String()),
						slog.String("error", ferr.Error()),
					)
				}
				s.advanceAfterFailure(r, runID, stageID)
				s.emitRuntimeObserved(r.Context(), runID, stageID, bundleBytes, "failed")
				return
			}
		}
	}

	// Plan-stage advancement gate (#603). A plan stage's terminal
	// transition (→ awaiting_approval, or → succeeded for a gateless
	// stage) is driven by a valid standard_v1 plan artifact landing via
	// the plan-upload handler, not by trace upload alone. The runner
	// ships both trace variants before the plan, so at trace-upload time
	// a plan stage has no plan artifact yet — leave it in running and let
	// handleShipPlan drive the terminal transition once the plan
	// validates. Without this a gated plan stage would reach
	// awaiting_approval with nothing to review when the later plan-ship
	// fails validation, and get_plan would return no_plan_yet.
	//
	// Non-plan stages (implement/review) advance unchanged. When
	// ArtifactRepo is nil (minimal config) planArtifactExists returns
	// true so this gate is a no-op and the prior trace-driven behavior is
	// preserved.
	if stage.Type == run.StageTypePlan && !s.planArtifactExists(r.Context(), stageID) {
		return
	}

	// Push-and-open-pr implement gate (#742). When the implement stage will
	// commit + push + open a PR AFTER this trace upload (the runner stamped
	// push_and_open_pr in the manifest) AND the bundle carries a non-empty
	// diff (so a commit + PR-open will actually follow), the terminal
	// transition is driven by the /pull-request upload, NOT by trace upload.
	// The commit/push/PR-open step runs after the trace ships, so advancing
	// to awaiting_approval here would strand the run at the review gate with
	// a null PR if that step fails — the zombie-run bug. Leave the stage in
	// running and let handleShipPullRequest drive the terminal transition:
	// succeeded/awaiting_approval on the success body, failed (category C/B)
	// on the failure body. Mirrors the plan-stage advancement gate (#603).
	//
	// Emit the runtime-calibration row HERE before the early return: the
	// bundle (and its timing) is only available at trace time, and the
	// /pull-request handler never sees it (ADR-030/#649). Outcome is
	// "succeeded" — the agent's work completed; only the PR-open is pending.
	//
	// An empty/absent diff is the no-changes path (agent made no edits → no
	// commit → no PR → the runner's openPRAndShipArtifact short-circuits
	// without a /pull-request POST). pushAndOpenPRGated returns false for it
	// so the terminal transition below advances the stage as before, rather
	// than hanging it in running. Non-push stages are byte-identical: the
	// flag is false → the gate is a no-op.
	if stage.Type == run.StageTypeImplement && s.pushAndOpenPRGated(bundleBytes) {
		s.emitRuntimeObserved(r.Context(), runID, stageID, bundleBytes, "succeeded")
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

	// Best-effort: emit runtime calibration data for implement stages.
	// The stage's work succeeded regardless of whether it needs approval.
	s.emitRuntimeObserved(r.Context(), runID, stageID, bundleBytes, "succeeded")

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

// planArtifactExists reports whether a valid standard_v1 plan artifact
// has been stored for the stage (#603). It mirrors the Kind == plan +
// SchemaVersion == standard_v1 filter in prompt.go::tryLoadPlanForRun so
// the trace handler's plan-stage advancement gate and the prompt builder
// agree on what counts as a usable plan.
//
// When ArtifactRepo is nil (minimal config) it returns true so the gate
// is a no-op and the prior trace-driven advancement is preserved. On a
// list error it returns false — leaving the plan stage in running rather
// than advancing it past a human gate with no plan — because the
// plan-upload handler is the authoritative driver of the terminal
// transition and will advance the stage once a valid plan lands.
func (s *Server) planArtifactExists(ctx context.Context, stageID uuid.UUID) bool {
	if s.cfg.ArtifactRepo == nil {
		return true
	}
	arts, err := s.cfg.ArtifactRepo.ListForStage(ctx, stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"plan-stage gate: list artifacts failed — leaving stage in running",
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	for _, a := range arts {
		if a.Kind != artifact.KindPlan {
			continue
		}
		if a.SchemaVersion == nil || *a.SchemaVersion != "standard_v1" {
			continue
		}
		return true
	}
	return false
}

// pushAndOpenPRGated reports whether an implement-stage trace upload should
// DEFER its terminal transition to the /pull-request upload (#742). True
// only when BOTH hold:
//
//   - the bundle manifest's push_and_open_pr flag is set (the runner will
//     commit + push + open a PR after the trace ships), and
//   - the bundle carries a non-empty git_diff (a commit + PR-open will
//     actually follow, so a /pull-request upload is guaranteed to come).
//
// A false flag — every non-PR-opening stage (plan/review, --no-pr local
// runs, decomposed children) and every older bundle — returns false so the
// prior trace-driven transition is byte-identical. An empty or absent diff
// is the no-changes path: the agent made no edits, so the runner opens no
// PR and never POSTs to /pull-request; gating it would hang the stage in
// running, so it returns false and the caller advances the stage as before.
func (*Server) pushAndOpenPRGated(bundleBytes []byte) bool {
	manifest, err := bundle.ExtractManifest(bundleBytes)
	if err != nil || !manifest.PushAndOpenPR {
		return false
	}
	diff, err := bundle.ExtractDiff(bundleBytes)
	if err != nil {
		return false
	}
	return len(diff.ChangedFiles) > 0
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
// of the closed-set constraints (E3.13). Returns the policy
// violations found; the caller derives pass/fail from the length and
// inspects the violation set to classify the failure (#692: an
// empty-diff implement failure is reclassified to a retryable
// category-C). Returns nil (treated as a pass) when:
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
// Always tries to emit a policy_evaluated audit entry when we
// reach a state that has either constraints or a diff — even if
// one is empty. The SPA reads that entry to render the policy
// section (#233); a missing entry is what makes the section
// stuck on "pending" (#247).
func (s *Server) reEvaluatePolicy(r *http.Request, runID, stageID uuid.UUID, bundleBytes []byte) []policy.Violation {
	ctx := r.Context()
	logger := s.cfg.Logger

	stage, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "policy: get stage failed",
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "policy: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return nil
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
		return nil
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
		return nil
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
		return nil
	}
	return violations
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
//
// bundleBytes is used for runtime calibration emit (best-effort).
func (s *Server) failStageCategoryB(r *http.Request, runID, stageID uuid.UUID, reason string, bundleBytes []byte) {
	if _, err := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureB, reason); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"trace upload: transition to failed-B failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
	s.advanceAfterFailure(r, runID, stageID)
	// Best-effort: emit runtime calibration for implement stages that
	// failed policy re-evaluation.
	s.emitRuntimeObserved(r.Context(), runID, stageID, bundleBytes, "failed")
}

// failStageCategoryC mirrors failStageCategoryB but stamps category C
// (infrastructure/transient per MVP_SPEC §6), which — unlike B — is
// retryable via the existing failed → pending path
// (run.RetryStage + orchestrator re-dispatch). The trace handler uses
// it for the #691 staging-bug signature (an implement stage's
// present-but-empty diff), so operators get an escape hatch instead
// of an un-redrivable B. Same best-effort posture as the B helper:
// failures log at WARN and never unwind the already-stored upload.
func (s *Server) failStageCategoryC(r *http.Request, runID, stageID uuid.UUID, reason string, bundleBytes []byte) {
	if _, err := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureC, reason); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"trace upload: transition to failed-C failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
	s.advanceAfterFailure(r, runID, stageID)
	// Best-effort: emit runtime calibration for implement stages that
	// failed with an empty captured diff.
	s.emitRuntimeObserved(r.Context(), runID, stageID, bundleBytes, "failed")
}

// noDiffCaptured reports whether a failed policy re-evaluation is the
// #691 staging-bug signature: an implement stage whose bundle carries
// a PRESENT-but-empty git_diff event, where that empty diff is the
// SOLE cause of the failure. Such a failure is a capture/staging miss
// rather than a genuine constraint breach, so the caller routes it to
// a retryable category-C (#692) instead of an un-redrivable B.
//
// It returns false — preserving the genuine category-B
// classification — when:
//   - the stage isn't an implement stage,
//   - the bundle has no git_diff event at all (ExtractDiff errors;
//     that case is the no_diff_in_bundle skip→pass path and never
//     reaches a policy failure here),
//   - the diff is non-empty, or
//   - any violation is something other than the empty-diff
//     required_outcomes case. The load-bearing carve-out is the
//     `invalid pattern` config error: a malformed
//     forbidden_paths/allowed_paths glob emits a violation even
//     against an empty diff (policy.matchAny validates the glob
//     before the per-file loop), and reclassifying that would mask a
//     real spec-config category-B.
func (s *Server) noDiffCaptured(ctx context.Context, stageID uuid.UUID, bundleBytes []byte, violations []policy.Violation) bool {
	stage, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil || stage.Type != run.StageTypeImplement {
		return false
	}
	diff, err := bundle.ExtractDiff(bundleBytes)
	if err != nil || len(diff.ChangedFiles) != 0 {
		return false
	}
	// The empty diff must be the sole cause: every violation has to be
	// the tests_added_or_updated empty-diff signature. Any other
	// violation (notably an `invalid pattern` config error) keeps the
	// failure as a genuine category-B.
	for _, v := range violations {
		if v.Constraint != "required_outcomes" || v.Detail != "no test files added or updated" {
			return false
		}
	}
	return len(violations) > 0
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

// emitRuntimeObserved appends a runtime_observed audit entry after an
// implement stage's trace upload. It extracts actual timing from the bundle
// and compares to the plan's predicted_runtime_minutes to build calibration
// data operators and agents can consume via GET /v0/calibration.
//
// All errors are logged at WARN; the helper never unwinds the caller because
// the trace is already stored and audited. Exits early when:
//   - RunRepo or AuditRepo is not wired
//   - stage lookup fails or stage.Type != implement
//   - bundle has fewer than two intermediate events (timing unavailable)
//   - no approved plan exists for the run (plan-less or mid-flight race)
func (s *Server) emitRuntimeObserved(ctx context.Context, runID, stageID uuid.UUID, bundleBytes []byte, outcome string) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	stage, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"runtime calibration: get stage failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return
	}
	if stage.Type != run.StageTypeImplement {
		return
	}
	startedAt, endedAt, ok := bundle.ExtractTiming(bundleBytes)
	if !ok {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"runtime calibration: insufficient timing events in bundle",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()))
		return
	}
	planArtifact, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"runtime calibration: load plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	if planArtifact == nil {
		return
	}

	actualDuration := endedAt.Sub(startedAt)
	actualSeconds := actualDuration.Seconds()
	actualMinutes := actualDuration.Minutes()
	predictedMinutes := float64(planArtifact.PredictedRuntimeMinutes)
	deltaMinutes := actualMinutes - predictedMinutes

	payload, _ := json.Marshal(map[string]any{
		"stage_type":        string(stage.Type),
		"predicted_minutes": predictedMinutes,
		"confidence":        string(planArtifact.PredictedRuntimeConfidence),
		"actual_seconds":    actualSeconds,
		"actual_minutes":    actualMinutes,
		"delta_minutes":     deltaMinutes,
		"outcome":           outcome,
	})

	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "runtime_observed",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"runtime calibration: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
}

// runCostRecorder is the optional capability the trace handler uses to
// accumulate the per-run cost rollup + pin the resolved model id
// (#649). The Postgres run repository implements it; it is deliberately
// NOT part of run.Repository so the many test fakes that don't roll
// cost need no stub. The trace handler asserts for it at runtime and
// skips the rollup (warn-only) when the wired RunRepo doesn't satisfy
// it — consistent with the rest of this handler's best-effort posture.
type runCostRecorder interface {
	AddRunCost(ctx context.Context, runID uuid.UUID, deltaUSD float64, resolvedModel string) (*run.Run, error)
}

// recordCost reads the trace bundle's manifest, computes the estimated
// cost of its model usage via the shared pricing table, writes a
// cost_recorded audit entry tying the figure to the run, and
// accumulates the per-run total (pinning the resolved model id) when
// the RunRepo supports it (#649).
//
// This MUST be invoked once per stage bundle, not once per variant
// upload (#678). The caller gates it on the raw variant; the runner
// ships both the raw and redacted variant of the same bundle with
// identical manifest token counts, so calling this on every variant
// double-counts the cost. Do not move this call out from under the
// raw-variant guard without re-introducing the 2x double-count.
//
// Every step is best-effort: the bundle is already stored + audited by
// the time this runs, so a missing/unparsable manifest, a nil
// AuditRepo, an audit-write failure, or a RunRepo that doesn't
// implement runCostRecorder all log at WARN (or silently no-op) and
// never unwind the upload. An unknown model id is recorded at usd=0
// with known_model=false rather than guessed.
//
// Likewise, a manifest that carries no usable token split (a backend
// that did not report usage, leaving both token counts zero) is
// recorded honestly at usd=0 with known_usage=false rather than a
// silent $0 indistinguishable from a real tiny run — symmetric with
// recordReviewerCost's known_usage contract (#682).
func (s *Server) recordCost(ctx context.Context, runID, stageID uuid.UUID, bundleBytes []byte) {
	if s.cfg.AuditRepo == nil {
		return
	}
	manifest, err := bundle.ExtractManifest(bundleBytes)
	if err != nil {
		// Malformed / non-manifest bundle (e.g. legacy fixtures). The
		// agent_failed path logs its own parse miss; here we stay quiet
		// at debug since a bundle with no manifest carries no token
		// counts to price.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
			"cost rollup: manifest unavailable — skipping cost record",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return
	}

	rec := cost.FromManifest(manifest.Model, manifest.InputTokens, manifest.OutputTokens)

	// Infer whether the backend reported usage from the token split. A
	// real invocation always has >0 tokens, so a 0/0 manifest means the
	// backend did not report usage — record it at usd=0 rather than a
	// guessed/priced figure (mirroring recordReviewerCost's usage.Known
	// override and the known_model=false contract). #682.
	knownUsage := manifest.InputTokens > 0 || manifest.OutputTokens > 0
	if !knownUsage {
		rec.USD = 0
	}

	payload, _ := json.Marshal(map[string]any{
		"model":         rec.Model,
		"input_tokens":  rec.InputTokens,
		"output_tokens": rec.OutputTokens,
		"usd":           rec.USD,
		"known_model":   rec.KnownModel,
		"known_usage":   knownUsage,
		"pricing_as_of": rec.PricingAsOf,
		"estimated":     true,
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "cost_recorded",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"cost rollup: append cost_recorded audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		// Fall through: still try to accumulate the run-record total so
		// the rollup doesn't silently drift from the (failed) audit row.
	}

	// Spend-anomaly check (#649). The cost_recorded entry is now part of
	// the ledger, so re-read the recent cost history (across runs) and
	// warn if the current hour is a multiple above the rolling average.
	// Warn-only: it never gates the upload.
	s.checkSpendAlert(ctx, runID, stageID, rec.Model)

	recorder, ok := s.cfg.RunRepo.(runCostRecorder)
	if !ok {
		return
	}
	if _, err := recorder.AddRunCost(ctx, runID, rec.USD, rec.Model); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"cost rollup: accumulate per-run cost total failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}

	// Advisory periodic-budget check (#688). Placed AFTER AddRunCost so
	// the period sum (SumWorkflowCostInRange over runs.cost_usd_total)
	// already includes this bundle's cost — a check before the increment
	// would miss the very crossing this bundle triggers and fire one
	// bundle late. Warn-only and best-effort, like checkSpendAlert.
	s.checkBudgetAlerts(ctx, runID, stageID)
}

// recordReviewerCost prices and records the cost of one advisory reviewer
// (plan-review / implement-review) agent invocation (#681). Unlike runner
// stage agents, reviewer agents run server-side inside fishhawkd and never
// ship a trace bundle, so their tokens never reach recordCost — this is the
// single seam that captures them, at the reviewer CONTRACT boundary.
//
// It is called from the per-reviewer review loop at the plan_reviewed /
// implement_reviewed call site with the usage the adapter reported through
// planreview.ReviewVerdict.Usage — never branching on which backend ran.
// source is "plan_review" or "implement_review" so reviewer cost is
// distinguishable from runner stage-agent cost (which carries no source).
//
// Graceful degradation: a backend that could not report usage leaves
// usage.Known false (with zero-value tokens), so the record lands at usd=0
// with known_usage=false rather than a guessed figure — mirroring the
// unknown-model known_model=false contract in recordCost.
//
// Best-effort throughout, like recordCost: the advisory review verdict is
// already recorded by the time this runs, so a nil AuditRepo, an audit-write
// failure, or a RunRepo that doesn't implement runCostRecorder all log at
// WARN (or silently no-op) and never unwind the review. It deliberately does
// NOT call checkSpendAlert — reviewer cost_recorded entries are swept by the
// next runner-triggered checkSpendAlert, which re-reads all cost_recorded
// entries across runs.
//
// It deliberately passes "" as resolvedModel to AddRunCost (#684): the
// reviewer is an auxiliary agent that runs AFTER the stage trace ships, so a
// non-empty model here would make the reviewer the last writer of the G6
// reproducibility pin (runs.resolved_model) and clobber the stage-agent pin.
// The empty string hits the CASE-WHEN-empty ELSE branch in AddRunCost
// (run/queries.sql), folding reviewer cost into cost_usd_total while leaving
// resolved_model untouched. Only recordCost (the stage agent) pins it.
func (s *Server) recordReviewerCost(ctx context.Context, runID, stageID uuid.UUID, model string, usage planreview.Usage, source string) {
	if s.cfg.AuditRepo == nil {
		return
	}

	rec := cost.FromManifest(model, usage.InputTokens, usage.OutputTokens)
	if !usage.Known {
		// The backend could not report usage; record the cost at 0 rather
		// than pricing zero-value tokens that might be wrong.
		rec.USD = 0
	}

	payload, _ := json.Marshal(map[string]any{
		"model":         rec.Model,
		"input_tokens":  rec.InputTokens,
		"output_tokens": rec.OutputTokens,
		"usd":           rec.USD,
		"known_model":   rec.KnownModel,
		"known_usage":   usage.Known,
		"pricing_as_of": rec.PricingAsOf,
		"source":        source,
		"estimated":     true,
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "cost_recorded",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"reviewer cost: append cost_recorded audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("source", source),
			slog.String("error", err.Error()))
		// Fall through: still try to accumulate the run-record total so the
		// rollup doesn't silently drift from the (failed) audit row.
	}

	recorder, ok := s.cfg.RunRepo.(runCostRecorder)
	if !ok {
		return
	}
	if _, err := recorder.AddRunCost(ctx, runID, rec.USD, ""); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"reviewer cost: accumulate per-run cost total failed",
			slog.String("run_id", runID.String()),
			slog.String("source", source),
			slog.String("error", err.Error()))
	}
}

// checkSpendAlert reads the recent cost_recorded audit history across
// all runs, evaluates the current hour's spend against the rolling
// average of prior hours, and emits a spend_alert audit entry when the
// current hour exceeds the configured multiple (#649).
//
// It is warn-only and best-effort throughout: the trace upload is
// already stored, audited, and cost-recorded by the time this runs, so
// a ListAll failure, an empty/insufficient history, or an audit-write
// failure all log at WARN (or silently no-op) and never unwind the
// upload. The detector itself (spendalert.Evaluate) suppresses alerts
// until a baseline exists, so a fresh deployment stays quiet.
//
// triggeringModel is the resolved model id of the bundle that pushed
// the current hour over the line; it's recorded on the alert so an
// operator can see which agent's usage drove the spike.
func (s *Server) checkSpendAlert(ctx context.Context, runID, stageID uuid.UUID, triggeringModel string) {
	if s.cfg.AuditRepo == nil {
		return
	}

	category := "cost_recorded"
	entries, err := s.cfg.AuditRepo.ListAll(ctx, audit.ListAllParams{Category: &category})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"spend alert: list cost_recorded entries failed — skipping check",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}

	samples := make([]spendalert.Sample, 0, len(entries))
	for _, e := range entries {
		var p struct {
			USD float64 `json:"usd"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		samples = append(samples, spendalert.Sample{Time: e.Timestamp, USD: p.USD})
	}

	d := spendalert.Evaluate(samples, time.Now().UTC(), s.cfg.SpendAlertMultiple)
	if !d.Tripped {
		return
	}

	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
		"spend alert: current hour spend exceeds rolling average",
		slog.String("run_id", runID.String()),
		slog.Float64("latest_hour_usd", d.LatestHourUSD),
		slog.Float64("rolling_avg_usd", d.RollingAvgUSD),
		slog.Float64("ratio", d.Ratio),
		slog.Float64("multiple", d.Multiple))

	payload, _ := json.Marshal(map[string]any{
		"latest_hour_usd":   d.LatestHourUSD,
		"rolling_avg_usd":   d.RollingAvgUSD,
		"ratio":             d.Ratio,
		"multiple":          d.Multiple,
		"prior_hours":       d.PriorHours,
		"latest_hour_start": d.LatestHourStart.Format(time.RFC3339),
		"triggering_model":  triggeringModel,
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "spend_alert",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"spend alert: append spend_alert audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
}

// runCostSummer is the optional capability checkBudgetAlerts uses to
// total a workflow's spend over a calendar period (#688). The Postgres
// run repository implements it; like runCostRecorder it is deliberately
// NOT part of run.Repository so the many test fakes that don't sum cost
// need no stub. The trace handler asserts for it at runtime and skips
// the advisory budget check (warn-only) when the wired RunRepo doesn't
// satisfy it — consistent with the rest of this handler's best-effort
// posture.
type runCostSummer interface {
	SumWorkflowCostInRange(ctx context.Context, repo, workflowID string, from, to time.Time) (float64, error)
}

// evaluateWorkflowBudget runs the shared period-range -> period-sum ->
// evaluate chain for one periodic budget, against the workflow's spend
// in the calendar period containing now (in loc). It is the single
// source of that sequence for both the alert path (checkBudgetAlerts)
// and the display path (runBudgetStatus, GET /v0/runs/{id}/budget).
//
// The bool reports whether the decision is usable: false (with a nil
// error) means the budget's period was unrecognized and the caller must
// skip it rather than bucket into the wrong window. A SumWorkflowCostInRange
// failure is returned as the error.
func evaluateWorkflowBudget(ctx context.Context, summer runCostSummer, repo, workflowID string, b spec.PeriodicBudget, now time.Time, loc *time.Location) (budget.Decision, bool, error) {
	start, end := budget.PeriodRange(b.Period, now, loc)
	if start.IsZero() {
		return budget.Decision{}, false, nil
	}
	spent, err := summer.SumWorkflowCostInRange(ctx, repo, workflowID, start, end)
	if err != nil {
		return budget.Decision{}, false, err
	}
	return budget.Evaluate(spent, b.LimitUSD, b.WarnAt, b.Period, now, loc), true, nil
}

// checkBudgetAlerts evaluates the run's workflow advisory periodic
// budgets (ADR-030 / #688) after the cost_recorded entry has been
// accumulated into runs.cost_usd_total. For each advisory budget on the
// run's cached workflow spec it sums the workflow's spend over the
// current calendar period (timezone-aware in cfg.BudgetLocation),
// evaluates it via budget.Evaluate, and on a warn_at or 100% crossing
// emits a deduped budget_alert audit entry plus a (non-sticky) issue
// comment.
//
// MUST run after AddRunCost: the period sum includes this bundle's cost
// only once the rollup has been incremented, so a check before the
// increment would fire the crossing one bundle late.
//
// Warn-only and best-effort throughout, mirroring checkSpendAlert: it
// never gates, fails, or blocks a run. It no-ops when RunRepo/AuditRepo
// is nil, the RunRepo doesn't implement runCostSummer, the run lookup or
// spec parse fails, the workflow has no budgets, or every budget is
// blocking (blocking enforcement is an admission-time gate — scope item
// 4, a separate follow-up — never this warn path). Every failure
// WARN-logs and returns without unwinding the already-stored cost.
func (s *Server) checkBudgetAlerts(ctx context.Context, runID, stageID uuid.UUID) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	summer, ok := s.cfg.RunRepo.(runCostSummer)
	if !ok {
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"budget alert: get run failed — skipping check",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	if len(runRow.WorkflowSpec) == 0 {
		return
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"budget alert: parse cached workflow spec failed — skipping check",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok || len(wf.Budgets) == 0 {
		return
	}

	loc := s.cfg.BudgetLocation
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now()

	for _, b := range wf.Budgets {
		// Only advisory budgets surface here. An empty enforcement value
		// defaults to advisory (the spec's documented zero-value), so the
		// blocking admission gate is the single excluded case.
		if b.Enforcement == spec.EnforcementBlocking {
			continue
		}
		d, ok, err := evaluateWorkflowBudget(ctx, summer, runRow.Repo, runRow.WorkflowID, b, now, loc)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"budget alert: sum period spend failed — skipping budget",
				slog.String("run_id", runID.String()),
				slog.String("workflow_id", runRow.WorkflowID),
				slog.String("period", b.Period),
				slog.String("error", err.Error()))
			continue
		}
		if !ok {
			// Unrecognized period — the schema enum makes this
			// unreachable, but don't bucket into the wrong window.
			continue
		}
		// Decide the tier this crossing represents. Over implies
		// WarnCrossed (warn_at <= 1), so check Over first and emit only
		// the higher tier per bundle; the 'warn' tier still fires on the
		// earlier bundle that first crossed warn_at but not 100%.
		tier := ""
		switch {
		case d.Over:
			tier = budgetTierOver
		case d.WarnCrossed:
			tier = budgetTierWarn
		default:
			continue
		}
		s.emitBudgetAlert(ctx, runID, stageID, runRow, b, d, tier)
	}
}

// Budget alert tiers, recorded in the budget_alert payload and used as
// the per-period de-dup discriminator.
const (
	budgetTierWarn = "warn"
	budgetTierOver = "over"
)

// emitBudgetAlert appends a budget_alert audit entry and posts the
// advisory issue comment for one crossed (budget, tier), deduped so each
// tier fires at most once per (workflow_id, period_start, tier). Both
// steps are best-effort: a dedup-read failure, an audit-append failure,
// or a notifier failure all WARN-log and never unwind the upload.
func (s *Server) emitBudgetAlert(ctx context.Context, runID, stageID uuid.UUID, runRow *run.Run, b spec.PeriodicBudget, d budget.Decision, tier string) {
	periodStart := d.PeriodStart.Format(time.RFC3339)

	already, err := s.budgetAlertAlreadyEmitted(ctx, runRow.WorkflowID, periodStart, tier)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"budget alert: dedup read failed — skipping emission",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	if already {
		return
	}

	payloadMap := map[string]any{
		"workflow_id":  runRow.WorkflowID,
		"repo":         runRow.Repo,
		"period":       b.Period,
		"period_start": periodStart,
		"spent":        d.Spent,
		"limit":        d.Limit,
		"fraction":     d.Fraction,
		"tier":         tier,
		"enforcement":  "advisory",
	}
	if b.WarnAt != nil {
		payloadMap["warn_at"] = *b.WarnAt
	}
	payload, _ := json.Marshal(payloadMap)
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "budget_alert",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"budget alert: append budget_alert audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		// Fall through: still try the comment so the surface fires even
		// when the audit row didn't land.
	}

	if s.issueNotifier == nil {
		return
	}
	if err := s.issueNotifier.NotifyBudgetAlert(ctx, runID, issuecomment.BudgetAlertPayload{
		WorkflowID:  runRow.WorkflowID,
		Period:      b.Period,
		PeriodStart: periodStart,
		Spent:       d.Spent,
		Limit:       d.Limit,
		Fraction:    d.Fraction,
		WarnAt:      b.WarnAt,
		Tier:        tier,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"budget alert: post issue comment failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}

// budgetAlertAlreadyEmitted reports whether a budget_alert audit entry
// already carries the same (workflow_id, period_start, tier) — the
// per-period/per-tier de-dup key. It scans ListAll(category=budget_alert)
// in memory rather than adding a workflow filter to the audit query
// (accepted v0 approach, #688): the budget_alert volume is tiny (at most
// two rows per workflow per period). The key is intentionally not
// repo-scoped, so two repos sharing a workflow_id could suppress each
// other's alert in the same period — accepted for an advisory,
// best-effort surface in v0.
func (s *Server) budgetAlertAlreadyEmitted(ctx context.Context, workflowID, periodStart, tier string) (bool, error) {
	category := "budget_alert"
	entries, err := s.cfg.AuditRepo.ListAll(ctx, audit.ListAllParams{Category: &category})
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		var p struct {
			WorkflowID  string `json:"workflow_id"`
			PeriodStart string `json:"period_start"`
			Tier        string `json:"tier"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.WorkflowID == workflowID && p.PeriodStart == periodStart && p.Tier == tier {
			return true, nil
		}
	}
	return false, nil
}

// checkRunBudget enforces the per-run budget tripwire (ADR-030 / #653): the
// whole-run safety rail that lets "the system stop itself" before a runaway
// run overruns silently. After the bundle's cost has been rolled into
// runs.cost_usd_total (#649), it compares the run's cumulative spend — US$
// (the rolled total) and tokens (summed from the cost_recorded ledger) —
// against the operator-configured per-run ceilings. On breach it HALTS the
// run via the cancel transition (SYSTEM actor) and appends a
// run_budget_exceeded audit entry naming the breached dimension and the
// figures, then returns true so the caller short-circuits stage advancement.
//
// Terminal state is `cancelled` by operator decision: a budget tripwire is a
// protective system HALT, not a work failure, and cancelled is non-retryable
// — a runaway run is deliberately NOT auto-redriven (unlike a failed-A/C).
// The audit kind carries the honest reason. There is no Notifier surface;
// this is an internal audit-only signal (see docs/issue-comment-surfaces.md).
//
// Returns false (the run proceeds) when the tripwire is disabled (both
// ceilings <= 0, the default), the deps aren't wired, the run lookup fails,
// or neither dimension is over. Best-effort throughout, consistent with the
// rest of this handler: the bundle is already stored + audited, so a
// transition or audit-write failure WARN-logs and never unwinds the upload.
// On a transition failure (e.g. the run is already terminal) it still returns
// true — a detected breach must never dispatch further work.
func (s *Server) checkRunBudget(ctx context.Context, runID, stageID uuid.UUID) bool {
	// Fast path: tripwire disabled (operator opt-in; default 0 = off). Skip
	// every read so the default deployment pays nothing for the rail.
	if s.cfg.MaxRunUSD <= 0 && s.cfg.MaxRunTokens <= 0 {
		return false
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return false
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"run budget: get run failed — skipping tripwire",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return false
	}

	tokens := s.sumRunTokens(ctx, runID)

	d := budget.EvaluateRun(runRow.CostUSDTotal, tokens, s.cfg.MaxRunUSD, s.cfg.MaxRunTokens)
	if !d.Over {
		return false
	}

	// Halt: cancel transition (SYSTEM actor) + run_budget_exceeded audit.
	if _, err := s.cfg.RunRepo.TransitionRun(ctx, runID, run.StateCancelled); err != nil {
		// Already terminal or a repo error. The breach is real, so we still
		// short-circuit (return true) — but skip the audit append to avoid a
		// spurious run_budget_exceeded entry on a run we couldn't halt.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"run budget: cancel transition failed — run already terminal or repo error",
			slog.String("run_id", runID.String()),
			slog.String("dimension", d.Dimension),
			slog.String("error", err.Error()))
		return true
	}

	payload, _ := json.Marshal(map[string]any{
		"dimension":      d.Dimension,
		"cost_usd_total": d.CostUSD,
		"max_run_usd":    d.MaxUSD,
		"tokens_total":   d.Tokens,
		"max_run_tokens": d.MaxTokens,
		"terminal_state": string(run.StateCancelled),
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  "run_budget_exceeded",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"run budget: append run_budget_exceeded audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}

	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
		"run budget: per-run ceiling breached — run halted (cancelled)",
		slog.String("run_id", runID.String()),
		slog.String("dimension", d.Dimension),
		slog.Float64("cost_usd_total", d.CostUSD),
		slog.Float64("max_run_usd", d.MaxUSD),
		slog.Int64("tokens_total", d.Tokens),
		slog.Int64("max_run_tokens", d.MaxTokens))
	return true
}

// sumRunTokens totals the input+output tokens across the run's cost_recorded
// audit entries — the per-invocation cost ledger (#649) — to give the per-run
// budget tripwire a cumulative token figure without a dedicated runs column.
// Best-effort: a list failure or an unparsable payload contributes 0 rather
// than unwinding the upload, so the token tripwire degrades to "low" rather
// than false-halting on a read error.
func (s *Server) sumRunTokens(ctx context.Context, runID uuid.UUID) int64 {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "cost_recorded")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"run budget: list cost_recorded entries failed — token total may be low",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return 0
	}
	var total int64
	for _, e := range entries {
		var p struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		total += p.InputTokens + p.OutputTokens
	}
	return total
}

// runImplementReviews resolves the implement stage's review config and
// dispatches the review agents after the diff has landed in the trace
// bundle (ADR-027 impl 2/2). It mirrors runPlanReviews' decoupling (#584):
// the cheap request-scoped reads (GetRun, resolveStageReviewers,
// loadApprovedPlanForRun, prompt.Build) run on the caller's context, then
// it branches on authority:
//
//   - gating (reviewers.agent>0 && human==0): runs the review loop
//     SYNCHRONOUSLY and returns true when any verdict is reject — the
//     caller (advanceStageAfterTrace) then fails the stage as category-B
//     so the terminal awaiting_approval advance is blocked. Gating review
//     can't be detached: the failed-B transition has to land before the
//     terminal transition. scope.files drift is flag-only — a
//     {category:"scope"} concern never forces a reject (ADR-027 Decision
//     Q6); only an overall verdict of reject blocks.
//
//   - advisory (reviewers.agent>0 && human>0): dispatches the review loop
//     on a DETACHED context (context.WithoutCancel) in a goroutine tracked
//     by s.bgReviews, and returns false immediately. The terminal
//     awaiting_approval/succeeded transition then proceeds while the
//     review runs to its own FISHHAWKD_PLAN_REVIEW_TIMEOUT budget,
//     decoupled from the runner's upload client timeout — the human gate
//     stays authoritative, so the advisory verdict landing after
//     advancement is fine.
//
// Returns false (no gating block) when:
//   - RunRepo is nil
//   - the workflow spec carries no implement stage with reviewers.agent>0
//   - PlanReviewer is nil (emits implement_review_skipped, then proceeds)
//   - no approved plan is available
//   - authority is advisory (review runs detached, never blocks)
//   - all review agents approve (or approve_with_concerns)
//
// Per-invocation errors are WARN-logged and skipped so a transient
// reviewer failure doesn't block the stage — the diff is already stored.
func (s *Server) runImplementReviews(ctx context.Context, runID, stageID uuid.UUID, diff policy.Diff, scopeDrift []string) bool {
	if s.cfg.RunRepo == nil {
		return false
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}

	reviewersCfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement)
	if reviewersCfg == nil || reviewersCfg.Agent == 0 {
		return false
	}

	authority := planreview.ResolveAuthority(*reviewersCfg)

	// PlanReviewer not wired but the spec requested agent review.
	// Emit implement_review_skipped so the degradation is auditable,
	// then proceed (in advisory mode the human gate remains
	// authoritative). Mirrors runPlanReviews.
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
				Category:  "implement_review_skipped",
				ActorKind: &systemKind,
				Payload:   payload,
			}); aerr != nil {
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: append implement_review_skipped audit entry failed",
					slog.String("run_id", runID.String()),
					slog.String("error", aerr.Error()),
				)
			}
		}
		return false
	}

	// Load the approved plan for the self-review guard (GeneratedBy.Model)
	// and the implement_review prompt's scope/approach/verification. No
	// plan → skip review (the diff has nothing to be measured against).
	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: load plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	if approvedPlan == nil {
		return false
	}

	trig := prompt.Trigger{
		Repo:         runRow.Repo,
		ApprovedPlan: approvedPlan,
		Diff:         renderDiffForReview(diff),
		// Full unified-diff hunks for content-level review (#585). Empty
		// when the bundle predates the patch field or the runner couldn't
		// compute it — buildImplementReview falls back to the file list.
		DiffPatch: diff.Patch,
		// Runner-reported paths excluded from the scope-bounded diff that
		// the operator may stage into the final commit (#695). Lets the
		// reviewer distinguish an operator-stageable drift path from a
		// genuinely missing file. Empty/nil when there was no drift.
		ScopeDrift: scopeDrift,
	}
	if runRow.IssueContext != nil {
		trig.IssueTitle = runRow.IssueContext.Title
		trig.IssueBody = runRow.IssueContext.Body
		// Map the cached comments so the implement-review prompt renders the
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
	promptText, err := prompt.Build("implement_review", trig)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: build prompt failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}

	// Pending-signal (#600): emit an implement_review_started audit entry
	// now that a reviewer will actually run. Emitted synchronously before
	// the dispatch loop so started precedes every implement_reviewed entry
	// under both authorities — the MCP review_status proxy reads it to tell
	// 'configured + running' (pending) from 'none configured'. Mirrors the
	// plan path (runPlanReviews). Best-effort: never blocks dispatch.
	s.emitReviewStarted(ctx, runID, stageID, "implement_review_started", authority, reviewersCfg.Agent)

	// Detach the reviewer context from the request lifecycle (#584); see
	// runPlanReviews for the rationale. The goroutine / loop closes over
	// only already-resolved values (built prompt, IDs, authority, author
	// model) — never r, the bundle, or request-scoped state.
	authorModel := approvedPlan.GeneratedBy.Model
	reviewCtx := context.WithoutCancel(ctx)

	// Advisory: dispatch detached so the terminal transition can proceed
	// without waiting on the reviewer.
	if authority != planreview.AuthorityGating {
		s.bgReviews.Add(1)
		go func() {
			defer s.bgReviews.Done()
			s.runImplementReviewLoop(reviewCtx, runID, stageID, reviewersCfg.Agent, authority, promptText, authorModel)
		}()
		return false
	}

	// Gating: run synchronously so the caller can fail the stage as
	// category-B before the terminal transition.
	return s.runImplementReviewLoop(reviewCtx, runID, stageID, reviewersCfg.Agent, authority, promptText, authorModel)
}

// runImplementReviewLoop runs the per-reviewer implement-review loop
// shared by the synchronous (gating) and detached (advisory) dispatch
// paths. For each configured agent it calls PlanReviewer.Review, logs
// WARN on self-review (reviewer model == authorModel), and appends one
// implement_reviewed audit entry. Returns true when at least one verdict
// is reject. It performs no stage transition — the gating caller owns the
// failed-B transition so the advance-blocking edge stays on the
// synchronous path only.
func (s *Server) runImplementReviewLoop(ctx context.Context, runID, stageID uuid.UUID, agents int, authority planreview.AuthorityMode, promptText, authorModel string) bool {
	systemKind := audit.ActorKind("system")
	hasRejection := false
	for i := 0; i < agents; i++ {
		verdict, model, err := s.cfg.PlanReviewer.Review(ctx, promptText)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: reviewer invocation failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.Int("reviewer_index", i),
				slog.String("error", err.Error()),
			)
			// Terminal implement_review_failed audit entry (#664), mirroring
			// the plan path: surfaces a timed-out / errored reviewer as a
			// definite 'failed' state. hasRejection untouched (#574).
			s.emitReviewFailed(ctx, runID, stageID, "implement_review_failed", authority, model, err.Error())
			continue
		}

		// Self-review guard (ADR-027): warn when the reviewer's model
		// matches the plan author's model. Warn-only; verdict still
		// recorded. The approved plan's GeneratedBy.Model is an
		// approximation of the implement-stage author's model — v0 does
		// not record the implement agent's model separately.
		if model != "" && model == authorModel {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"implement review: self-review detected — reviewer model matches plan author model",
				slog.String("model", model),
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
			)
		}

		payload := planreview.ImplementReviewedPayload{
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
			Category:  "implement_reviewed",
			ActorKind: &systemKind,
			Payload:   payloadBytes,
		}); aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: append audit entry failed",
				slog.String("run_id", runID.String()),
				slog.String("error", aerr.Error()),
			)
		}

		// Capture this reviewer invocation's agent token cost (#681). The
		// usage rode in on the planreview.ReviewVerdict contract; we price
		// and record it here, backend-agnostically.
		s.recordReviewerCost(ctx, runID, stageID, model, verdict.Usage, "implement_review")

		if verdict.Verdict == planreview.VerdictReject {
			hasRejection = true
		}
	}
	return hasRejection
}

// renderDiffForReview formats a policy.Diff (per-file path + git status)
// into the changed-files summary the implement_review prompt embeds. The
// reviewer compares this list against the plan's scope.files to flag
// drift. Status letters mirror `git diff --name-status` (A/M/D/R/C).
func renderDiffForReview(diff policy.Diff) string {
	var b strings.Builder
	for _, f := range diff.ChangedFiles {
		fmt.Fprintf(&b, "- %s %s\n", f.Status, f.Path)
	}
	return b.String()
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
