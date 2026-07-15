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
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/budget"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/cost"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spendalert"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/unpricedmodel"
	"github.com/kuhlman-labs/fishhawk/pricing"
)

// maxTraceBundleBytes mirrors the runner's bundle.MaxBundleBytes so
// the runner and backend agree on the gzipped payload ceiling. v0
// trace volumes are bounded by token budgets; this is the
// belt-and-suspenders cap that protects backend storage from a
// runaway agent.
const maxTraceBundleBytes = 64 * 1024 * 1024

// implementReviewGatingRejectReason is the category-B failure reason
// stamped on an implement stage when a gating agent implement-review
// (human==0) returns a reject verdict during the raw-trace upload
// (advanceStageAfterTrace). It is hoisted to a const so the
// /pull-request handler's dangling-PR-close detection (#877) keys off
// the same source of truth and cannot drift from the failure site.
const implementReviewGatingRejectReason = "implement_review_rejected: agent review verdict reject under gating authority"

// implementReviewGatingRejectPrefix is the stable prefix of
// implementReviewGatingRejectReason. handleShipPullRequest matches a
// failed stage's FailureReason on this prefix to recognize an
// already-failed gating reject and close the just-opened PR (#877).
const implementReviewGatingRejectPrefix = "implement_review_rejected"

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

	// runner_kind reconciliation (#1346 / ADR-045). The runner self-reports
	// its observed execution channel inside the SIGNED manifest; reconcile it
	// against the run's create-time hint and LOCK runner_kind on the first
	// report, closing the #1344 local-loop wedge. Best-effort: the trace is
	// already stored, so any error WARN-logs and never unwinds the upload.
	// Done BEFORE the trace_uploaded audit append so that entry attests the
	// reconciled (locked) kind; the runner_kind_resolved / runner_kind_mismatch
	// entries are chained AFTER trace_uploaded below.
	runnerKindRes := s.reconcileRunnerKind(r.Context(), runID, body)
	if runnerKindRes.Locked != "" {
		auditFields["runner_kind"] = runnerKindRes.Locked
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

	// Emit the reconciliation guardrail audit (#1346 / ADR-045), chained
	// after trace_uploaded. Changed → runner_kind_resolved (the hint was
	// corrected); Mismatch → runner_kind_mismatch (a later report disagreed
	// with the already-locked kind). Best-effort: a failure WARN-logs.
	s.emitRunnerKindReconcileAudit(r.Context(), runID, stageID, runnerKindRes)

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
				// #968: FailStage rejecting the transition means this is a
				// duplicate/stale failure report (e.g. the stage was already
				// recovered by fix-up recovery). A report that transitioned
				// nothing must not advance the run — doing so once routed a
				// run with its review gate still open to completeRun and
				// stamped it succeeded.
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
					"trace upload: transition to failed-A failed",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", stageID.String()),
					slog.String("error", err.Error()))
				s.writeJSON(w, r, http.StatusAccepted, traceUploadResponse{
					RunID:       runID,
					StageID:     stageID,
					Variant:     string(variant),
					ContentHash: contentHash,
				})
				return
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
			s.advanceStageAfterTrace(r, runID, stageID, variant, body)
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
func (s *Server) advanceStageAfterTrace(r *http.Request, runID, stageID uuid.UUID, variant tracestore.Variant, bundleBytes []byte) {
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
	//
	// Gate on the raw variant so the review is dispatched exactly once per
	// stage bundle (#793). The runner POSTs both the raw and the redacted
	// variant of the same bundle, and a push_and_open_pr implement stage
	// stays in `running` across both uploads (the terminal transition is
	// deferred to the /pull-request report), so advanceStageAfterTrace
	// re-enters this block on the redacted upload too — dispatching a SECOND
	// implement review (2x cost, divergent verdicts, and #777's
	// review_action_hint over-firing on the stale first verdict). Gating on
	// raw mirrors the recordCost #678 gate above (trace.go ~line 218): raw is
	// the first/authoritative upload and is always shipped in v0. This
	// structurally cannot suppress a fix-up re-dispatch (#762/#788/#794):
	// FixupStage re-opens the SAME stage_id with a NEW diff/head_sha and the
	// runner re-uploads raw first, so the fix-up's own raw variant fires its
	// own single review on the new diff.
	//
	// This raw-variant hook is the FIRST of two implement re-review dispatch
	// paths. When it is SKIPPED for a fix-up head — the raw trace is routed to
	// failStageCategoryB by a backend policy re-evaluation (a stale-base bundle
	// diff over max_files_changed) so control never reaches this block, then #788
	// recovery restores the stage — succeedFixupPushStage's backstop
	// (maybeBackstopFixupReReview, #1932) is the SECOND path: it re-arms the
	// re-review off the fixup_pushed report so the audit-complete merge gate does
	// not wedge on a 'pending' review that no trace ever dispatched. On the normal
	// path where this hook already dispatched for the head, the backstop detects
	// the existing implement_review_started entry and is a no-op.
	if stage.Type == run.StageTypeImplement && variant == tracestore.VariantRaw {
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
			// Reconcile the PRE-fold scope_drift snapshot against the runner's
			// authoritative per-commit fold record (#1317). When the implement
			// stage folds an operator-approved mid-stage scope amendment, the
			// runner emits scope_drift for the amendment path BEFORE folding it,
			// then later folds it into the pushed HEAD — leaving the drift
			// snapshot stale. The scope_amendments_folded event carries EXACTLY
			// the paths the runner folded for this commit, so subtracting it
			// from the review-presentation surfaces stops the reviewer (and
			// operator agents) reading a landed path as drift-excluded.
			//
			// DELIBERATE Option-A scope (operator decision, #1317): we reconcile
			// ONLY at the REVIEW-presentation surfaces (this Trigger.ScopeDrift
			// list and the gate-evidence ScopeFacts below). The raw scope_drift
			// bundle event / audit entry is intentionally left as the historical
			// PRE-fold record — it is an event log, not a current-state claim.
			//
			// The subtract set is sourced ONLY from the fold record (never from
			// amendment intent), so an approved-but-NOT-folded path is never in
			// the event and stays as real drift — real drift is never hidden.
			// Best-effort, mirroring the ExtractScopeDrift degrade: an extraction
			// error WARN-logs and subtracts nothing, so the review still proceeds.
			folded, fierr := bundle.ExtractScopeAmendmentsFolded(bundleBytes)
			if fierr != nil {
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
					"trace upload: extract scope amendments folded failed — proceeding with no fold subtraction",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", stageID.String()),
					slog.String("error", fierr.Error()),
				)
				folded = nil
			}
			scopeDrift = subtractPaths(scopeDrift, folded)
			// Extract the bundle's verify_run head_sha as the #797 dedup key
			// threaded into runImplementReviews. Best-effort: ErrNoHeadSHA
			// (the no-verify / head_sha-less case) leaves headSHA empty
			// without a WARN (the ordinary case); any other error WARN-logs
			// and also degrades to an empty key. An empty key never blocks —
			// the guard fails open and dedup falls back to the retained
			// raw-variant gate (the redacted double is still prevented).
			headSHA, hserr := bundle.ExtractHeadSHA(bundleBytes)
			if hserr != nil {
				if !errors.Is(hserr, bundle.ErrNoHeadSHA) {
					s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
						"trace upload: extract head_sha failed — proceeding with no dedup key",
						slog.String("run_id", runID.String()),
						slog.String("stage_id", stageID.String()),
						slog.String("error", hserr.Error()),
					)
				}
				headSHA = ""
			}
			// Extract the runner's digested gate results (#963) so the
			// reviewer sees machine-verified build/test/scope truth with
			// outrank guidance instead of assuming gates passed. Best-effort
			// with the same degradation contract as ExtractScopeDrift:
			// ErrNoGateEvidence (older bundles, no gate ran) stays silent —
			// the ordinary case — while any other error WARN-logs; both
			// degrade to nil, which omits the prompt section and never
			// blocks the review.
			var gateEvidence *prompt.GateEvidence
			if ev, geerr := bundle.ExtractGateEvidence(bundleBytes); geerr == nil {
				gateEvidence = gateEvidenceForReview(ev, folded)
			} else if !errors.Is(geerr, bundle.ErrNoGateEvidence) {
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
					"trace upload: extract gate evidence failed — proceeding with no evidence",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", stageID.String()),
					slog.String("error", geerr.Error()),
				)
			}
			if s.runImplementReviews(r.Context(), runID, stageID, diff, scopeDrift, headSHA, gateEvidence) {
				cat := run.FailureB
				reason := implementReviewGatingRejectReason
				if _, ferr := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, cat, reason); ferr != nil {
					// #968: a failure report FailStage rejected (duplicate /
					// already-recovered stage) must not advance the run.
					s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
						"trace upload: transition to failed-B after implement-review gating reject failed",
						slog.String("run_id", runID.String()),
						slog.String("stage_id", stageID.String()),
						slog.String("error", ferr.Error()),
					)
					return
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
	//
	// The decomposed-child push (#771) is gated identically: a child stamps
	// push_to_shared_branch (not push_and_open_pr) and commits + pushes onto
	// the shared parent branch after the trace ships. childPushGated mirrors
	// pushAndOpenPRGated — leave the child stage in `running` and let the
	// /pull-request "pushed"/"failed" report drive the terminal transition,
	// so a child commit/push failure lands the stage `failed` instead of
	// reaching terminal succeeded with no code on the shared branch (the
	// childcompletion sweeper would otherwise consolidate the parent into a
	// PR silently missing that child's work).
	// The fix-up re-dispatch push (#794) is gated identically: a fix-up stamps
	// push_fixup (not push_and_open_pr / push_to_shared_branch) and commits onto
	// the EXISTING PR branch after the trace ships. fixupPushGated mirrors
	// pushAndOpenPRGated — leave the fix-up stage in `running` and let the
	// /pull-request "fixup_pushed"/"failed" report drive the terminal
	// transition, so a fix-up commit/push/compile-gate failure lands the stage
	// `failed` (firing #788 fix-up recovery) instead of reaching terminal
	// succeeded with the implement re-review approving an unlanded diff (the
	// #794 swallow). The advisory implement re-review above still fires at trace
	// time on the bundle diff while the stage stays running — the gate defers
	// only the TERMINAL transition, and on a later push failure #788 recovery
	// un-advances the run.
	if stage.Type == run.StageTypeImplement &&
		(s.pushAndOpenPRGated(bundleBytes) || s.childPushGated(bundleBytes) || s.fixupPushGated(bundleBytes)) {
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

// childPushGated reports whether a decomposed-child implement-stage trace
// upload should DEFER its terminal transition to the /pull-request upload
// (#771). It mirrors pushAndOpenPRGated for the decomposition-child case.
// True only when BOTH hold:
//
//   - the bundle manifest's push_to_shared_branch flag is set (the runner is
//     a decomposed child that will commit + push onto the shared parent
//     branch after the trace ships, without opening its own PR), and
//   - the bundle carries a non-empty git_diff (a commit + push will actually
//     follow, so a /pull-request "pushed"/"failed" report is guaranteed).
//
// A false flag — every non-child stage and every older bundle — returns
// false so the prior trace-driven transition is byte-identical. An empty or
// absent diff is the no-changes path: the child made no edits, so the runner
// pushes nothing and never POSTs to /pull-request; gating it would hang the
// stage in running, so it returns false and the caller advances the stage as
// before (matching the push_and_open_pr empty-diff case).
func (*Server) childPushGated(bundleBytes []byte) bool {
	manifest, err := bundle.ExtractManifest(bundleBytes)
	if err != nil || !manifest.PushToSharedBranch {
		return false
	}
	diff, err := bundle.ExtractDiff(bundleBytes)
	if err != nil {
		return false
	}
	return len(diff.ChangedFiles) > 0
}

// fixupPushGated reports whether a fix-up re-dispatch implement-stage trace
// upload should DEFER its terminal transition to the /pull-request upload
// (#794). It mirrors pushAndOpenPRGated/childPushGated for the fix-up case.
// True only when BOTH hold:
//
//   - the bundle manifest's push_fixup flag is set (the runner is a fix-up
//     re-dispatch that will commit onto the EXISTING PR branch after the trace
//     ships, updating the open PR rather than opening a new one), and
//   - the bundle carries a non-empty git_diff (a commit + push will actually
//     follow, so a /pull-request "fixup_pushed"/"failed" report is guaranteed).
//
// A false flag — every non-fix-up stage and every older bundle — returns false
// so the prior trace-driven transition is byte-identical. An empty or absent
// diff is the no-changes path: the fix-up made no edits, so the runner pushes
// nothing and never POSTs to /pull-request; gating it would hang the stage in
// running, so it returns false and the caller advances the stage as before
// (matching the push_and_open_pr / push_to_shared_branch empty-diff cases).
func (*Server) fixupPushGated(bundleBytes []byte) bool {
	manifest, err := bundle.ExtractManifest(bundleBytes)
	if err != nil || !manifest.PushFixup {
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

// notifyPageClass is the best-effort pings-only immediate hook (#1786),
// invoked right after each currently-batched page-class audit append
// (plan-review reject, implement-review reject, scope-amendment request,
// paged acceptance triage) so the page posts within the event's own
// transaction window instead of riding the next stage transition. The
// notifier dedups on the source audit Sequence, so this cannot double-post
// with the per-transition notifyStatusUpdate; here we just call it and log
// on failure. `source` tags the call site in the log line.
func (s *Server) notifyPageClass(ctx context.Context, runID uuid.UUID, source string) {
	if s.issueNotifier == nil {
		return
	}
	if err := s.issueNotifier.NotifyPageClassForRun(ctx, runID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"page-class ping failed",
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
		// #968: FailStage rejecting the transition means this report is a
		// duplicate against an already-resolved stage (e.g. fix-up recovery
		// restored it to succeeded with the review gate re-parked). A report
		// that transitioned nothing must not advance the run.
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"trace upload: transition to failed-B failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return
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
		// #968: same duplicate-report discipline as the B helper — the
		// 68e13183 incident was a duplicate cat-C report arriving after
		// fix-up recovery restored the stage and re-parked the review gate;
		// the fall-through Advance stamped the run succeeded with the gate
		// open. Never advance on a report that transitioned nothing.
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"trace upload: transition to failed-C failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return
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
	// Fix-up recovery (#788): if this terminal failure is a failed fix-up
	// re-dispatch, restore the run to its pre-fix-up review gate instead
	// of failing it. maybeRecoverFixupFailure un-fails the implement stage
	// + re-parks the review stage and returns true; we then SKIP the
	// run-failing Advance so the run stays `running` at its gate. This is
	// the single chokepoint for the cat-A agent-fail path, the cat-B
	// implement-review gating-reject path, and failStageCategoryB/C — all
	// funnel through here. A non-recovery failure (the common case) returns
	// false and the orchestrator Advance below runs unchanged.
	if s.maybeRecoverFixupFailure(r.Context(), runID, stageID) {
		return
	}
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
		return
	}

	// Board-state sync (#1012): when the Advance drove the RUN itself to a
	// terminal failed state, move the work item to the blocked canonical state.
	// Guarded on the run being failed — a stage failure that leaves sibling
	// stages running must not park the card in Blocked. Best-effort; the run is
	// already failed, so a board miss never changes the outcome.
	if rn, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err == nil && rn.State == run.StateFailed {
		s.boardTransitionForRun(r.Context(), rn, lifecycleRunFailed)
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

	// Stamp the resolved implement model {value, source} (#1013) onto the
	// EXISTING runtime_observed kind so per-model-per-complexity calibration
	// history accumulates without a new audit surface. Empty value/source means
	// today's default spawn — the keys are still emitted (omitempty-free) so the
	// calibration reader can distinguish "default spawn" from a missing field.
	rm := s.resolvedImplementModelForRunID(ctx, runID)

	payload, _ := json.Marshal(map[string]any{
		"stage_type":            string(stage.Type),
		"predicted_minutes":     predictedMinutes,
		"confidence":            string(planArtifact.PredictedRuntimeConfidence),
		"actual_seconds":        actualSeconds,
		"actual_minutes":        actualMinutes,
		"delta_minutes":         deltaMinutes,
		"outcome":               outcome,
		"resolved_model":        rm.Value,
		"resolved_model_source": string(rm.Source),
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

// runnerKindResolver is the optional capability the trace handler uses to
// reconcile a runner self-report against the run's persisted runner_kind
// and LOCK it on the first report (#1346 / ADR-045). The Postgres run
// repository implements it; like runCostRecorder it is deliberately NOT
// part of run.Repository — adding it would break the many hand-rolled test
// fakes that implement the interface without embedding BaseFake — so the
// trace handler asserts for it at runtime and skips reconciliation
// (warn-only) when the wired RunRepo doesn't satisfy it.
type runnerKindResolver interface {
	ResolveRunnerKind(ctx context.Context, runID uuid.UUID, observed string) (run.RunnerKindResolution, error)
}

// reconcileRunnerKind reads the bundle manifest's self-reported execution
// channel and reconciles it against the run's persisted runner_kind
// (#1346 / ADR-045), returning the resolution so the caller can stamp the
// trace_uploaded audit and emit the guardrail audit entry.
//
// Best-effort throughout — the trace is already stored by the time this
// runs, so every degradation returns the zero RunnerKindResolution (a
// no-op the caller treats as "nothing to reconcile") rather than failing
// the upload:
//   - RunRepo nil, or it doesn't implement runnerKindResolver → no-op.
//   - manifest unparsable, or it carries no runner_kind (legacy bundle) →
//     no-op (no WARN for the ordinary no-report case).
//   - ResolveRunnerKind errors → WARN-log, no-op.
func (s *Server) reconcileRunnerKind(ctx context.Context, runID uuid.UUID, body []byte) run.RunnerKindResolution {
	if s.cfg.RunRepo == nil {
		return run.RunnerKindResolution{}
	}
	resolver, ok := s.cfg.RunRepo.(runnerKindResolver)
	if !ok {
		return run.RunnerKindResolution{}
	}
	manifest, err := bundle.ExtractManifest(body)
	if err != nil || manifest.RunnerKind == "" {
		// A legacy / channel-less bundle carries no self-report; that is the
		// ordinary back-compat case, not an error worth a WARN.
		return run.RunnerKindResolution{}
	}
	res, err := resolver.ResolveRunnerKind(ctx, runID, manifest.RunnerKind)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"trace upload: resolve runner_kind failed — leaving runner_kind unreconciled",
			slog.String("run_id", runID.String()),
			slog.String("observed", manifest.RunnerKind),
			slog.String("error", err.Error()))
		return run.RunnerKindResolution{}
	}
	return res
}

// emitRunnerKindReconcileAudit chains the runner_kind reconciliation
// guardrail audit entry after trace_uploaded (#1346 / ADR-045):
//
//   - Changed → category runner_kind_resolved, payload {from, to}: the
//     first signed report corrected the create-time hint (the #1344 fix).
//   - Mismatch → category runner_kind_mismatch, payload {declared,
//     observed}: a later report disagreed with the already-locked kind; the
//     row was left unchanged (warn, never silently flip). This is the
//     post-execution guardrail.
//
// A no-op / re-affirmation resolution emits nothing. Best-effort: a
// nil AuditRepo or an append failure WARN-logs and never unwinds the
// already-stored, already-trace_uploaded-audited upload.
func (s *Server) emitRunnerKindReconcileAudit(ctx context.Context, runID, stageID uuid.UUID, res run.RunnerKindResolution) {
	if s.cfg.AuditRepo == nil {
		return
	}
	var category string
	var fields map[string]any
	switch {
	case res.Mismatch:
		category = "runner_kind_mismatch"
		fields = map[string]any{
			"run_id":   runID.String(),
			"stage_id": stageID.String(),
			"declared": res.Prior,
			"observed": res.Observed,
		}
	case res.Changed:
		category = "runner_kind_resolved"
		fields = map[string]any{
			"run_id":   runID.String(),
			"stage_id": stageID.String(),
			"from":     res.Prior,
			"to":       res.Locked,
		}
	default:
		return
	}
	payload, _ := json.Marshal(fields)
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"trace upload: append runner_kind reconcile audit failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("category", category),
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

	// Price the agent-stage bundle cache-aware (ADR-044 / #1349): the manifest's
	// InputTokens is the FRESH (cache-exclusive) input, with the cache-served
	// read and cache-creation write portions carried separately. FromManifestWithCache
	// prices fresh input + output at the flat rates plus cache read at the family
	// discount and cache write at the premium; an older bundle without the cache
	// fields decodes them to 0 and prices identically to the flat FromManifest.
	rec := cost.FromManifestWithCache(manifest.Model, manifest.InputTokens,
		manifest.CacheReadInputTokens, manifest.CacheWriteInputTokens, manifest.OutputTokens)

	// Infer whether the backend reported usage from the token split. A
	// real invocation always has >0 tokens, so an all-zero manifest means the
	// backend did not report usage — record it at usd=0 rather than a
	// guessed/priced figure (mirroring recordReviewerCost's usage.Known
	// override and the known_model=false contract). #682. Any of the four
	// token buckets being positive counts as usage reported (#1349): a fully
	// cache-served invocation can have zero fresh input/output yet real spend.
	knownUsage := manifest.InputTokens > 0 || manifest.OutputTokens > 0 ||
		manifest.CacheReadInputTokens > 0 || manifest.CacheWriteInputTokens > 0
	if !knownUsage {
		rec.USD = 0
	}

	// Stamp the resolved implement model {value, source} (#1013) alongside the
	// agent-reported `model` (rec.Model) on the EXISTING cost_recorded kind, so
	// per-model spend calibration can attribute cost to the rung that chose the
	// model. Kept under distinct `resolved_model*` keys so it never clobbers the
	// agent-reported `model`. Empty value/source means today's default spawn.
	rm := s.resolvedImplementModelForRunID(ctx, runID)

	// cache_read_input_tokens / cache_write_input_tokens are added ADDITIVELY
	// (#1349) alongside the unchanged input_tokens (FRESH/cache-exclusive) and
	// output_tokens keys, so the read/write split is on the ledger and
	// sumRunTokens can include the cache buckets. Older readers ignore the new
	// keys; older bundles emit them as 0.
	payload, _ := json.Marshal(map[string]any{
		"model":                    rec.Model,
		"input_tokens":             rec.InputTokens,
		"output_tokens":            rec.OutputTokens,
		"cache_read_input_tokens":  rec.CacheReadInputTokens,
		"cache_write_input_tokens": rec.CacheWriteInputTokens,
		"usd":                      rec.USD,
		"known_model":              rec.KnownModel,
		"known_usage":              knownUsage,
		"pricing_as_of":            rec.PricingAsOf,
		"estimated":                true,
		"resolved_model":           rm.Value,
		"resolved_model_source":    string(rm.Source),
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: s.nowFunc().UTC(),
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

	// Ground-truth pricing-coverage check (#1870). The cost_recorded entry
	// carries known_model/known_usage; re-read the recent ledger and warn
	// (once per window) if any dispatched model is unpriced or reported no
	// usage. Warn-only, best-effort, exactly like checkSpendAlert: it never
	// gates the upload.
	s.checkUnpricedModel(ctx, runID, stageID, rec.Model)

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

// reviewerInputTokenWarnCeiling is the per-invocation known FRESH
// (cache-exclusive) input-token level above which recordReviewerCost
// WARN-logs (#995). Usage.InputTokens is normalized to cache-exclusive for
// every adapter (#1010), so this ceiling fires on genuine fresh-token
// blowups and is no longer false-tripped by a heavily-cached codex review
// whose raw cache-inclusive total is large. A review invocation reads one
// server-composed prompt (plan/diff + instructions), so a six-figure fresh
// count signals a context-assembly blowup (e.g. an agentic reviewer
// exploring its cwd across many turns), not a big artifact. Observability
// only — the cost is still priced and recorded.
const reviewerInputTokenWarnCeiling = 100_000

// reviewerTotalInputTokenWarnCeiling is the companion ceiling on the TOTAL
// input-side count (fresh + cached, #1010): it catches a runaway total
// context under heavy caching that the fresh ceiling no longer sees. Seeded
// by the observed 689k-total / ~572k-cached codex review (run 0a0765ff) —
// that run still trips it, while a normal cached review does not. Advisory
// (WARN-only), like the fresh ceiling.
const reviewerTotalInputTokenWarnCeiling = 500_000

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

	// cost.FromManifest stays the source for the Record metadata (Model,
	// KnownModel, PricingAsOf) — it is out of scope this slice (the agent
	// path; slice 2). The reviewer USD is then OVERRIDDEN with the
	// cache-aware total (#1343): fresh input + output priced exactly as
	// FromManifest, plus cache-read at the discount and cache-write at the
	// premium. CostWithCache(model, in, 0, 0, out) reduces exactly to the
	// flat Cost FromManifest uses, so a no-cache reviewer is unaffected.
	rec := cost.FromManifest(model, usage.InputTokens, usage.OutputTokens)
	if usage.Known {
		if usd, ok := pricing.CostWithCache(model, usage.InputTokens, usage.CacheReadInputTokens, usage.CacheWriteInputTokens, usage.OutputTokens); ok {
			rec.USD = usd
		}
	} else {
		// The backend could not report usage; record the cost at 0 rather
		// than pricing zero-value tokens that might be wrong.
		rec.USD = 0
	}

	// Context-assembly blowup tripwire (#995): a known FRESH (cache-exclusive,
	// #1010) input-token count past the ceiling is anomalous for a single
	// composed review prompt. WARN so the next ~400k-fresh-token review is
	// loud, not a silent ledger line.
	if usage.Known && usage.InputTokens > reviewerInputTokenWarnCeiling {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"reviewer cost: input tokens exceed warn ceiling — possible context-assembly blowup",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("source", source),
			slog.String("model", model),
			slog.Int("input_tokens", usage.InputTokens),
			slog.Int("turns", usage.Turns),
			slog.Int("ceiling", reviewerInputTokenWarnCeiling))
	}

	// Companion total-context tripwire (#1010): fresh + cached past the
	// higher ceiling means a runaway total context that heavy caching kept
	// off the fresh ceiling — still worth a loud line.
	cachedInputTokens := usage.CachedInputTokens()
	totalInputTokens := usage.InputTokens + cachedInputTokens
	if usage.Known && totalInputTokens > reviewerTotalInputTokenWarnCeiling {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"reviewer cost: total input tokens (fresh + cached) exceed warn ceiling — runaway total context",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("source", source),
			slog.String("model", model),
			slog.Int("total_input_tokens", totalInputTokens),
			slog.Int("input_tokens", usage.InputTokens),
			slog.Int("cached_input_tokens", cachedInputTokens),
			slog.Int("turns", usage.Turns),
			slog.Int("ceiling", reviewerTotalInputTokenWarnCeiling))
	}

	// turns / cached_input_tokens / cache_read_input_tokens /
	// cache_write_input_tokens / total_input_tokens are observability-only
	// (#995/#1010/#1343): turns exposes a multi-turn agentic blowup;
	// input_tokens is the normalized FRESH (cache-exclusive) count for every
	// adapter; cached_input_tokens is the summed cache-served total (= read +
	// write via the CachedInputTokens() accessor) kept for back-compat, with
	// cache_read_input_tokens / cache_write_input_tokens added ADDITIVELY for
	// the read/write split; and total_input_tokens (= fresh + cached)
	// preserves the raw input-side total. USD is now cache-aware (priced via
	// pricing.CostWithCache above).
	payload, _ := json.Marshal(map[string]any{
		"model":                    rec.Model,
		"input_tokens":             rec.InputTokens,
		"output_tokens":            rec.OutputTokens,
		"cached_input_tokens":      cachedInputTokens,
		"cache_read_input_tokens":  usage.CacheReadInputTokens,
		"cache_write_input_tokens": usage.CacheWriteInputTokens,
		"total_input_tokens":       totalInputTokens,
		"turns":                    usage.Turns,
		"usd":                      rec.USD,
		"known_model":              rec.KnownModel,
		"known_usage":              usage.Known,
		"pricing_as_of":            rec.PricingAsOf,
		"source":                   source,
		"estimated":                true,
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

	d := spendalert.Evaluate(samples, s.nowFunc().UTC(), s.cfg.SpendAlertMultiple)
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

// checkUnpricedModel reads the recent cost_recorded audit history across
// all runs and emits a warn-only unpriced_model_alert when a dispatched
// model recorded a cost row it could not price — either the model id was
// absent from the pricing table (known_model=false) or the backend
// reported no usable usage (known_usage=false) (#1870). Per ADR-044 the
// pricing table stays human-authoritative — this alarms, it never
// auto-prices.
//
// It is warn-only and best-effort throughout, identical in posture to
// checkSpendAlert: the trace upload is already stored, audited, and
// cost-recorded by the time this runs, so a ListAll failure (samples OR
// prior-alert read) or an AppendChained failure all log at WARN and
// return — never propagated, never unwinding the cost_recorded append or
// the upload.
//
// The ListAll -> Evaluate -> AppendChained sequence is deliberately NOT
// serialized or locked: the dedup against prior unpriced_model_alert
// entries is noise-reduction, not a correctness invariant, so a rare
// duplicate warn-only alert under two concurrent recordCost calls is
// acceptable noise (mirrors checkSpendAlert's un-serialized best-effort
// shape). triggeringModel is the model of the bundle that drove this
// check; it's recorded on the alert for operator context.
func (s *Server) checkUnpricedModel(ctx context.Context, runID, stageID uuid.UUID, triggeringModel string) {
	if s.cfg.AuditRepo == nil {
		return
	}

	costCategory := "cost_recorded"
	costEntries, err := s.cfg.AuditRepo.ListAll(ctx, audit.ListAllParams{Category: &costCategory})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"unpriced model: list cost_recorded entries failed — skipping check",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}

	samples := make([]unpricedmodel.Sample, 0, len(costEntries))
	for _, e := range costEntries {
		var p struct {
			Model      string `json:"model"`
			KnownModel bool   `json:"known_model"`
			KnownUsage bool   `json:"known_usage"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		samples = append(samples, unpricedmodel.Sample{
			Time:       e.Timestamp,
			Model:      p.Model,
			KnownModel: p.KnownModel,
			KnownUsage: p.KnownUsage,
		})
	}

	// Prior alerts feed the once-per-window dedup: expand each prior
	// unpriced_model_alert payload's unpriced_models / unknown_usage_models
	// arrays into one Alert per model id at the entry's timestamp.
	alertCategory := "unpriced_model_alert"
	alertEntries, err := s.cfg.AuditRepo.ListAll(ctx, audit.ListAllParams{Category: &alertCategory})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"unpriced model: list unpriced_model_alert entries failed — skipping check",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}

	var priorAlerts []unpricedmodel.Alert
	for _, e := range alertEntries {
		var p struct {
			UnpricedModels     []string `json:"unpriced_models"`
			UnknownUsageModels []string `json:"unknown_usage_models"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		for _, m := range p.UnpricedModels {
			priorAlerts = append(priorAlerts, unpricedmodel.Alert{Time: e.Timestamp, Model: m})
		}
		for _, m := range p.UnknownUsageModels {
			priorAlerts = append(priorAlerts, unpricedmodel.Alert{Time: e.Timestamp, Model: m})
		}
	}

	d := unpricedmodel.Evaluate(samples, priorAlerts, s.nowFunc().UTC(), unpricedmodel.Window)
	if !d.Tripped {
		return
	}

	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
		"unpriced model: dispatched model(s) recorded uncosted rows",
		slog.String("run_id", runID.String()),
		slog.Any("unpriced_models", d.UnpricedModels),
		slog.Any("unknown_usage_models", d.UnknownUsageModels),
		slog.String("triggering_model", triggeringModel))

	modelCount := len(d.UnpricedModels) + len(d.UnknownUsageModels)
	payload, _ := json.Marshal(map[string]any{
		"unpriced_models":      d.UnpricedModels,
		"unknown_usage_models": d.UnknownUsageModels,
		"model_count":          modelCount,
		"triggering_model":     triggeringModel,
		"window_start":         d.WindowStart.Format(time.RFC3339),
		"window_hours":         d.Window.Hours(),
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: s.nowFunc().UTC(),
		Category:  "unpriced_model_alert",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"unpriced model: append unpriced_model_alert audit entry failed",
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
		// Apply the operator's calibration override (#1371) centrally on a
		// copy of the budget so the period sum is evaluated against — and the
		// alert payload reports — the effective limit, not the raw spec one.
		b.LimitUSD = s.effectiveBudgetLimit(b)
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
		// Decide the tier this crossing represents via the single-source
		// escalating ladder (#1371): page > ack_required > over > warn. Each
		// higher tier fires on the bundle that first reached it, deduped
		// per-(workflow,period,tier) by emitBudgetAlert, so the earlier 'warn'
		// / 'over' crossings still emit on their own bundles. budget.Tier's
		// defensive multiple-fallback means a zero-value Config never
		// classifies an ordinary crossing as 'page'.
		tier := budget.Tier(d, s.cfg.BudgetAckMultiple, s.cfg.BudgetPageMultiple)
		if tier == budget.TierOK {
			continue
		}
		s.emitBudgetAlert(ctx, runID, stageID, runRow, b, d, tier)
	}
}

// effectiveBudgetLimit returns the limit the periodic-budget evaluator
// should use for b: the operator's BudgetLimitOverrideUSD when it is
// configured (> 0), else the spec budget's own limit_usd (#1371). It is
// the single application point of the calibration override, reused by both
// the alert path (checkBudgetAlerts) and the display path
// (runBudgetStatus) so the two surfaces cannot drift on which limit they
// report. A zero or negative override (the default) leaves the spec limit
// untouched, byte-identical to pre-#1371 behavior.
func (s *Server) effectiveBudgetLimit(b spec.PeriodicBudget) float64 {
	if s.cfg.BudgetLimitOverrideUSD > 0 {
		return s.cfg.BudgetLimitOverrideUSD
	}
	return b.LimitUSD
}

// budgetAlertSentCategory is the audit category of the cross-run
// comment-delivery dedup marker (#758). It is NOT an issue-comment
// surface: it is an internal, system-actor bookkeeping row written ONLY
// when the advisory budget comment actually landed on the issue, gating
// the COMMENT independently of the budget_alert crossing record. See
// docs/issue-comment-surfaces.md.
const budgetAlertSentCategory = "budget_alert_sent"

// emitBudgetAlert records one crossed (budget, tier) and posts its
// advisory issue comment. The two dedups are deliberately DECOUPLED
// (#758):
//
//   - The budget_alert AUDIT entry is the canonical once-per-period
//     crossing record. It is gated by budgetAlertAlreadyEmitted and
//     written even when the visible comment is suppressed (non-issue
//     run, nil installation), so the SPA's period-spend view and
//     compliance consumers always see the crossing exactly once per
//     (workflow_id, period_start, tier).
//
//   - The COMMENT is gated separately on a budget_alert_sent marker
//     written ONLY when NotifyBudgetAlert actually posts. A crossing
//     recorded on a run that structurally can't comment leaves no
//     marker, so the next capable run still surfaces the comment for the
//     period — fixing the bug where a comment-less first emission
//     poisoned the dedup for the whole period.
//
// Every step is best-effort: a dedup-read failure, an audit-append
// failure, or a notifier failure all WARN-log and never unwind the
// upload.
func (s *Server) emitBudgetAlert(ctx context.Context, runID, stageID uuid.UUID, runRow *run.Run, b spec.PeriodicBudget, d budget.Decision, tier string) {
	periodStart := d.PeriodStart.Format(time.RFC3339)

	// Canonical crossing record: gated ONLY around the budget_alert
	// append so the once-per-period signal is preserved even for
	// non-issue / comment-less runs.
	already, err := s.budgetAlertAlreadyEmitted(ctx, runRow.WorkflowID, periodStart, tier)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"budget alert: dedup read failed — skipping emission",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	if !already {
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
			// Fall through: still try the comment so the surface fires
			// even when the audit row didn't land.
		}
	}

	if s.issueNotifier == nil {
		return
	}

	// Comment-delivery dedup, decoupled from the crossing record above.
	// Keyed on the budget_alert_sent marker — written only when a
	// comment actually posted — so a comment-less first emission does
	// not suppress a later capable run.
	commentSent, err := s.budgetAlertCommentSent(ctx, runRow.WorkflowID, periodStart, tier)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"budget alert: comment-dedup read failed — skipping comment",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	if commentSent {
		return
	}

	posted, err := s.issueNotifier.NotifyBudgetAlert(ctx, runID, issuecomment.BudgetAlertPayload{
		WorkflowID:  runRow.WorkflowID,
		Period:      b.Period,
		PeriodStart: periodStart,
		Spent:       d.Spent,
		Limit:       d.Limit,
		Fraction:    d.Fraction,
		WarnAt:      b.WarnAt,
		Tier:        tier,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"budget alert: post issue comment failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
	if !posted {
		// Comment was suppressed (non-issue run, nil installation,
		// per-run dedup, or a post error). Leave NO budget_alert_sent
		// marker so the next capable run still surfaces the comment.
		return
	}

	// Comment landed — record the cross-run marker so later capable runs
	// dedup the comment for this (workflow_id, period_start, tier).
	sentPayload, _ := json.Marshal(map[string]any{
		"workflow_id":  runRow.WorkflowID,
		"period_start": periodStart,
		"tier":         tier,
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  budgetAlertSentCategory,
		ActorKind: &systemKind,
		Payload:   sentPayload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"budget alert: append budget_alert_sent marker failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
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

// budgetAlertCommentSent reports whether the advisory budget COMMENT has
// already been delivered for this (workflow_id, period_start, tier) — the
// cross-run comment-delivery dedup introduced in #758. It scans
// ListAll(category=budget_alert_sent) in memory, the same cheap
// low-volume pattern as budgetAlertAlreadyEmitted (at most two markers
// per workflow per period). This is DISTINCT from budgetAlertAlreadyEmitted:
// that gates the once-per-period budget_alert crossing record (written
// even for comment-less runs); this gates the visible comment on a marker
// written ONLY when a comment actually landed, so a comment-less first
// emission no longer poisons the period. Like the crossing key it is not
// repo-scoped — mirroring the accepted v0 limitation in #688.
func (s *Server) budgetAlertCommentSent(ctx context.Context, workflowID, periodStart, tier string) (bool, error) {
	category := budgetAlertSentCategory
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

	// Aggregate cost + tokens across the decomposition family (E24.6 /
	// #1146): a wide fan-out can blow the run budget even when no single
	// child is over, so the tripwire sums the parent + every child. A
	// non-decomposed run's family is just itself (familyRuns returns
	// [runRow]), so its figure is byte-identical to the pre-#1146 single-run
	// behavior.
	family := s.familyRuns(ctx, runRow)
	var costUSD float64
	var tokens int64
	for _, m := range family {
		costUSD += m.CostUSDTotal
		tokens += s.sumRunTokens(ctx, m.ID)
	}

	d := budget.EvaluateRun(costUSD, tokens, s.cfg.MaxRunUSD, s.cfg.MaxRunTokens)
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
			InputTokens           int64 `json:"input_tokens"`
			OutputTokens          int64 `json:"output_tokens"`
			CacheReadInputTokens  int64 `json:"cache_read_input_tokens"`
			CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		// Include the cache buckets (#1349): input_tokens is now FRESH
		// (cache-exclusive), so the cache-served read and cache-creation write
		// tokens are real spend that the tripwire must count. Older entries
		// without the keys decode them to 0 — the prior fresh+output figure.
		total += p.InputTokens + p.OutputTokens + p.CacheReadInputTokens + p.CacheWriteInputTokens
	}
	return total
}

// familyAggregationLimit bounds the ListRuns page the family-aggregation
// helper fetches (the Postgres adapter rejects a non-positive limit). A
// decomposition fan-out is small (bounded by the plan's sub_plans), so this
// generous cap never truncates a real family; if a pathological fan-out ever
// exceeded it the aggregate would undercount rather than over-halt — the safe
// direction for a tripwire.
const familyAggregationLimit = 1000

// familyRuns returns the decomposition family the given run belongs to
// (E24.6 / #1146): the family root — runRow itself when it is a parent
// (DecomposedFrom == nil), else the parent named by *DecomposedFrom — plus
// every child minted from that root (ListRuns by DecomposedFrom). The
// returned slice always begins with the root and contains no duplicates
// (the root is a parent, so it never appears in its own children list).
//
// A NON-decomposed run is its own family: DecomposedFrom is nil and the
// root has no children, so the result is exactly [runRow]. This is the
// regression guard that keeps checkRunBudget's figure unchanged for
// ordinary runs.
//
// Best-effort, consistent with the rest of this handler: a root GetRun or
// a children ListRuns failure logs at WARN and degrades to the single run
// ([runRow]) rather than unwinding the upload — the family aggregate just
// falls back to the per-run figure, never false-halting on a read error.
func (s *Server) familyRuns(ctx context.Context, runRow *run.Run) []*run.Run {
	rootID := runRow.ID
	root := runRow
	if runRow.DecomposedFrom != nil {
		rootID = *runRow.DecomposedFrom
		r, err := s.cfg.RunRepo.GetRun(ctx, rootID)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"family aggregation: get parent run failed — using single run",
				slog.String("run_id", runRow.ID.String()),
				slog.String("parent_run_id", rootID.String()),
				slog.String("error", err.Error()))
			return []*run.Run{runRow}
		}
		root = r
	}

	children, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		DecomposedFrom: &rootID,
		Limit:          familyAggregationLimit,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"family aggregation: list children failed — using single run",
			slog.String("run_id", runRow.ID.String()),
			slog.String("root_run_id", rootID.String()),
			slog.String("error", err.Error()))
		return []*run.Run{runRow}
	}

	family := make([]*run.Run, 0, len(children)+1)
	family = append(family, root)
	family = append(family, children...)
	return family
}

// consolidatedReviewTruncatedCategory records that a decomposed parent's
// consolidated diff was truncated by GitHub before the consolidated review
// ran (#1060): the review saw only a partial diff. Internal audit kind
// (system actor), not an issue-comment surface — it backs the loud
// degradation signal the dispatch is required to emit rather than silently
// under-reviewing.
const consolidatedReviewTruncatedCategory = "consolidated_review_diff_truncated"

// operatorScopeUndeliveredCategory is the audit-log category for the advisory
// pre-review signal (#1407) emitted when an implement commit leaves an
// operator-DELIBERATELY-added scope path (an add_scope_files path folded at
// plan approval, or an approved mid-stage scope amendment) UNTOUCHED. It is an
// internal advisory audit kind written by the trace handler before the reviewer
// verdict — NOT an issue-comment surface (nothing in issuecomment emits it).
const operatorScopeUndeliveredCategory = "operator_scope_path_undelivered"

// operatorScopeUndeliveredPayload is the audit payload for an
// operator_scope_path_undelivered entry (#1407): the operator-added scope paths
// the implement commit left untouched, plus the counts that produced the set.
type operatorScopeUndeliveredPayload struct {
	UndeliveredPaths   []string `json:"undelivered_paths"`
	UndeliveredCount   int      `json:"undelivered_count"`
	OperatorAddedCount int      `json:"operator_added_count"`
}

// concernRelitigationSuppressedCategory is the audit-log category for the
// deterministic re-litigation guard (#1913): a reviewer concern whose
// settled_ref resolves to a same-run/same-stage WAIVED or DEFERRED concern and
// whose new_evidence is empty is NOT minted as a fresh open concern row — it is
// recorded here instead, so the suppression is visible on the run surface rather
// than silent. It is an internal advisory audit kind written by persistReviewConcerns;
// it posts no issue comment and adds no Notifier method, so it is NOT an
// issue-comment surface.
const concernRelitigationSuppressedCategory = "concern_relitigation_suppressed"

// concernRelitigationSuppressedPayload is the audit payload for a
// concern_relitigation_suppressed entry (#1913). It records the settled concern
// the re-raise targeted (SettledRef + SettledState) and the reviewer's would-be
// concern (severity/category/note) so an operator can see exactly what was
// suppressed and why, keyed back to the review that emitted it
// (ReviewerModel + OriginReviewSequence).
type concernRelitigationSuppressedPayload struct {
	SettledRef           string `json:"settled_ref"`
	SettledState         string `json:"settled_state"`
	Severity             string `json:"severity"`
	Category             string `json:"category"`
	Note                 string `json:"note"`
	ReviewerModel        string `json:"reviewer_model,omitempty"`
	OriginReviewSequence int64  `json:"origin_review_sequence"`
}

// DispatchConsolidatedReview dispatches the gating agent implement review
// for a decomposed parent run against its consolidated PR diff (#1060),
// satisfying orchestrator.ConsolidatedReviewDispatcher. The orchestrator
// invokes it after Advance dispatches the parent's review stage with the
// consolidated PR present; the dispatch is server-side because the review
// machinery (runImplementReviews) lives here and the server depends on the
// orchestrator, not the reverse.
//
// The diff that actually merges is the consolidated base...head compare —
// the parent has no runner trace bundle of its own — so it is sourced via
// githubclient.ComparePatch and handed to runImplementReviews with the
// stageID of the PARENT'S IMPLEMENT stage (the awaiting_children→succeeded
// stage). That stage id is the load-bearing seam: the implement_reviewed
// concerns attach there, so fishhawk_fixup_stage on that stage resolves
// them and re-invokes the agent over the #1036 shared branch — no new
// fix-up code path. Per-child reviews remain advisory early signal; this
// consolidated review is the gating one (closes #677's parent-merge gap).
//
// Best-effort and idempotent: every guard that doesn't add up — not a
// decomposed parent, no children, no implement stage, GitHub/installation
// not wired, or a compare failure — logs and returns without dispatching,
// and runImplementReviews' own started-key guard dedups a re-fire across
// the sweeper/event-driven double-advance. The review runs on a detached,
// shutdown-tracked goroutine so the orchestrator's Advance (and the request
// that drove it) never blocks on the compare fetch or the reviewer LLMs.
func (s *Server) DispatchConsolidatedReview(ctx context.Context, parentRunID uuid.UUID, base, head string) {
	if s.cfg.RunRepo == nil {
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, parentRunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "consolidated review: get run failed",
			slog.String("run_id", parentRunID.String()), slog.String("error", err.Error()))
		return
	}
	// A child run is never a decomposed parent.
	if runRow.DecomposedFrom != nil {
		return
	}
	// Authoritative parent check: an ordinary feature run (DecomposedFrom
	// nil, a PR present, but no children) must NOT get a consolidated review
	// — its implement review already ran on the trace path. Only a run with
	// decomposed children is a parent.
	children, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{DecomposedFrom: &parentRunID, Limit: 1})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "consolidated review: list children failed",
			slog.String("run_id", parentRunID.String()), slog.String("error", err.Error()))
		return
	}
	if len(children) == 0 {
		return
	}

	var implStage *run.Stage
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, parentRunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "consolidated review: list stages failed",
			slog.String("run_id", parentRunID.String()), slog.String("error", err.Error()))
		return
	}
	for _, st := range stages {
		if st.Type == run.StageTypeImplement {
			implStage = st
			break
		}
	}
	if implStage == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "consolidated review: parent has no implement stage — skipping",
			slog.String("run_id", parentRunID.String()))
		return
	}

	// GitHub-wired diff source. CLI/dev posture (no client / no
	// installation) skips silently — same posture as the consolidated-PR
	// open path; drive parks the review gate until a round dispatches.
	if s.cfg.GitHub == nil || runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "consolidated review: GitHub/installation not wired — skipping dispatch",
			slog.String("run_id", parentRunID.String()))
		return
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "consolidated review: parse repo failed",
			slog.String("run_id", parentRunID.String()), slog.String("error", err.Error()))
		return
	}

	installationID := *runRow.InstallationID
	stageID := implStage.ID
	reviewCtx := context.WithoutCancel(ctx)
	s.bgReviews.Add(1)
	go func() {
		defer s.bgReviews.Done()
		cmp, cerr := s.cfg.GitHub.ComparePatch(reviewCtx, installationID, repo, base, head)
		if cerr != nil {
			s.cfg.Logger.LogAttrs(reviewCtx, slog.LevelWarn, "consolidated review: compare patch failed — review not dispatched",
				slog.String("run_id", parentRunID.String()),
				slog.String("base", base), slog.String("head", head),
				slog.String("error", cerr.Error()))
			return
		}
		if cmp.Truncated {
			// Surface loudly + emit a durable degradation signal: the review
			// is about to run on a partial diff (the consolidated fan-out's
			// large-diff case the #1060 amendment calls out). Still dispatch
			// — a partial review beats none — but the gap is now auditable.
			s.cfg.Logger.LogAttrs(reviewCtx, slog.LevelWarn, "consolidated review: diff truncated by GitHub — review will under-review",
				slog.String("run_id", parentRunID.String()),
				slog.String("reason", cmp.TruncationReason),
				slog.Int("changed_files", len(cmp.Files)))
			s.emitConsolidatedReviewTruncated(reviewCtx, parentRunID, stageID, cmp.TruncationReason, len(cmp.Files))
		}
		diff := consolidatedReviewDiff(cmp)
		// The returned gating-reject signal is intentionally ignored: the
		// parent implement stage is already succeeded, so there is no
		// terminal transition to fail to category-B. Gating is enforced by
		// the drive gate (which parks until this round dispatches) plus the
		// operator reading the verdicts and routing a fix-up. The
		// implement_review_started/_reviewed round and any concerns attach
		// to the parent implement stage regardless of authority.
		s.runImplementReviews(reviewCtx, parentRunID, stageID, diff, nil, cmp.HeadSHA, nil)
	}()
}

// maybeBackstopFixupReReview dispatches a post-fix-up implement re-review when
// the fix-up's trace-time review hook (#793) never fired for the pushed head
// (#1932). The trace-time hook lives in advanceStageAfterTrace and dispatches
// the re-review from the RAW trace variant; when that raw trace is routed to
// failStageCategoryB by a backend policy re-evaluation (a stale-base bundle diff
// that exceeds max_files_changed is the observed case, run 98020210) the handler
// never reaches the review hook, #788 fix-up recovery then restores the implement
// stage to succeeded, and the subsequent fixup_pushed report records the new head
// with nothing re-arming the re-review — implement_review_status stays 'pending'
// forever and the audit-complete merge gate wedges. Called from
// succeedFixupPushStage after the fixup_pushed audit entry lands, this backstop
// re-arms the re-review for ANY trace-time miss.
//
// Guards, in order — each fails closed to no-second-review, because a double
// dispatch is the worse failure (2x review cost, divergent verdicts, and #777's
// review_action_hint over-firing on the stale first verdict):
//
//	(a) AuditRepo nil → skip (the started ledger is unreadable).
//	(b) an implement_review_started entry already exists for (stage, new head)
//	    → the trace-time hook already dispatched for this head, so the backstop
//	    is a no-op and review cost is unchanged (the normal path). A list error
//	    also skips — the backstop must not double-dispatch under an unknown state.
//	(c) the NEWEST implement_review_started entry for THIS stage carries an empty
//	    head_sha → an unkeyed prior round is indistinguishable from a missed one,
//	    so skip (fail closed). Fix-up passes run the committed-tree verify gate,
//	    so bundle heads are present in practice; a residual verify-less miss is
//	    the accepted trade against double-dispatch. WARN-logged so it stays
//	    diagnosable. When NO started entry exists for the stage the trace-time
//	    hook never fired at all, so the backstop proceeds (a genuine miss).
//	(d) GitHub client / run installation not wired → the same CLI/dev posture
//	    DispatchConsolidatedReview carries (INFO-log and skip).
//
// When the guards pass it dispatches on a detached, shutdown-tracked goroutine
// (context.WithoutCancel + s.bgReviews) exactly like DispatchConsolidatedReview:
// ComparePatch(baseSHA, headSHA) IS the fix-up delta — baseSHA is the branch head
// the fix-up committed onto (the fixup_pushed report's base_sha), coherent with
// the #1725 delta re-review framing runImplementReviews applies — mapped through
// consolidatedReviewDiff and handed to runImplementReviews for the new head. That
// call reuses runImplementReviews' own (stage_id, head_sha) idempotency guard
// (#797) as the second line against a double dispatch, and its gating-reject
// return is intentionally ignored: the stage is already terminal/restored at
// push-report time, so there is no in-flight transition to fail (the
// consolidated-review call-site rationale, trace.go ~2687). gateEvidence is nil
// (the PR-report path has no bundle in hand), the documented byte-identical omit
// case in prompt.Build.
func (s *Server) maybeBackstopFixupReReview(ctx context.Context, runID uuid.UUID, stage *run.Stage, headSHA, baseSHA string) {
	if s.cfg.AuditRepo == nil {
		return
	}
	started, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "implement_review_started")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup re-review backstop: list implement_review_started failed — skipping backstop",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	// (b) Normal path: the trace-time hook already dispatched for this head.
	if implementReviewAlreadyStarted(started, stage.ID, headSHA) {
		return
	}
	// (c) Conservative empty-head skip: an unkeyed prior round cannot be told
	// apart from a missed one, so fail closed rather than risk a double review.
	if newestSHA, found := newestImplementReviewStartedHead(started, stage.ID); found && newestSHA == "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup re-review backstop: newest implement_review_started for stage has empty head_sha — skipping to avoid double-dispatch",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("head_sha", headSHA))
		return
	}
	// (d) GitHub-wired diff source. CLI/dev posture (no client / no installation)
	// skips silently — the same posture as DispatchConsolidatedReview.
	if s.cfg.GitHub == nil || s.cfg.RunRepo == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"fixup re-review backstop: GitHub/run repo not wired — skipping dispatch",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()))
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup re-review backstop: get run failed — skipping dispatch",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"fixup re-review backstop: GitHub/installation not wired — skipping dispatch",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()))
		return
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup re-review backstop: parse repo failed — skipping dispatch",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}

	installationID := *runRow.InstallationID
	stageID := stage.ID
	reviewCtx := context.WithoutCancel(ctx)
	s.bgReviews.Add(1)
	go func() {
		defer s.bgReviews.Done()
		cmp, cerr := s.cfg.GitHub.ComparePatch(reviewCtx, installationID, repo, baseSHA, headSHA)
		if cerr != nil {
			s.cfg.Logger.LogAttrs(reviewCtx, slog.LevelWarn,
				"fixup re-review backstop: compare patch failed — review not dispatched",
				slog.String("run_id", runID.String()),
				slog.String("base", baseSHA), slog.String("head", headSHA),
				slog.String("error", cerr.Error()))
			return
		}
		diff := consolidatedReviewDiff(cmp)
		// The gating-reject return is intentionally ignored: the fix-up stage is
		// already terminal/restored at push-report time, so there is no in-flight
		// transition to fail to category-B (the consolidated-review rationale
		// above). The started/reviewed round and any concerns attach to the
		// implement stage regardless of authority, re-arming the merge gate.
		s.runImplementReviews(reviewCtx, runID, stageID, diff, nil, headSHA, nil)
	}()
}

// consolidatedReviewDiff maps a githubclient compare result onto a
// policy.Diff for the consolidated review: each changed file's GitHub
// word-form status becomes a policy.Status letter, and the reconstructed
// unified diff rides on Patch for the reviewer's content-level lens.
func consolidatedReviewDiff(cmp *githubclient.ComparePatchResult) policy.Diff {
	diff := policy.Diff{Patch: cmp.Patch}
	diff.ChangedFiles = make([]policy.ChangedFile, 0, len(cmp.Files))
	for _, f := range cmp.Files {
		diff.ChangedFiles = append(diff.ChangedFiles, policy.ChangedFile{
			Path:   f.Path,
			Status: githubStatusToPolicy(f.Status),
		})
	}
	return diff
}

// githubStatusToPolicy maps GitHub's compare word-form file status onto the
// single-letter policy.Status. Unknown / "modified" both fall through to M.
func githubStatusToPolicy(status string) policy.Status {
	switch status {
	case "added":
		return policy.StatusAdded
	case "removed":
		return policy.StatusDeleted
	case "renamed":
		return policy.StatusRenamed
	case "copied":
		return policy.StatusCopied
	case "changed":
		return policy.StatusTypeChg
	default:
		return policy.StatusModified
	}
}

// emitConsolidatedReviewTruncated writes the durable degradation signal
// (#1060) when GitHub truncated the consolidated diff before review.
// Best-effort, system actor; attached to the parent implement stage so it
// sits beside the consolidated review's other entries.
func (s *Server) emitConsolidatedReviewTruncated(ctx context.Context, runID, stageID uuid.UUID, reason string, changedFiles int) {
	if s.cfg.AuditRepo == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{"reason": reason, "changed_files": changedFiles})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  consolidatedReviewTruncatedCategory,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "consolidated review: append truncation audit failed",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
	}
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
//   - no reviewer backend is configured (emits implement_review_skipped,
//     then proceeds)
//   - no approved plan is available
//   - authority is advisory (review runs detached, never blocks)
//   - all review agents approve (or approve_with_concerns)
//
// Per-invocation errors are WARN-logged and skipped so a transient
// reviewer failure doesn't block the stage — the diff is already stored.
func (s *Server) runImplementReviews(ctx context.Context, runID, stageID uuid.UUID, diff policy.Diff, scopeDrift []string, headSHA string, gateEvidence *prompt.GateEvidence) bool {
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
	if reviewersCfg == nil || reviewersCfg.AgentCount() == 0 {
		return false
	}

	authority := planreview.ResolveAuthority(*reviewersCfg)

	// No reviewer backend wired but the spec requested agent review.
	// Emit implement_review_skipped so the degradation is auditable,
	// then proceed (in advisory mode the human gate remains
	// authoritative). Mirrors runPlanReviews.
	if s.defaultPlanReviewer() == nil {
		if s.cfg.AuditRepo != nil {
			payload, _ := json.Marshal(planreview.ReviewSkippedPayload{
				Reason:           planreview.ReasonReviewerNotConfigured,
				ConfiguredAgents: reviewersCfg.AgentCount(),
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
		// Paths the operator authorized at approval time via the #730
		// approval-condition prose fold or the #824 add_scope_files structured
		// fold that are NOT already in the raw plan scope.files (#829). The
		// implement-stage prompt folds these into its effective scope, but this
		// review prompt is built directly from approvedPlan, so we recompute the
		// folds here (reusing the same resolvers handleGetStagePrompt uses) and
		// thread the remainder through so the reviewer treats them as in-scope
		// rather than flagging them as scope drift.
		AmendedScopeFiles: s.amendedScopeFilesForReview(ctx, runRow, approvedPlan),
		// The operator's binding approve-with-conditions text (#1021), via
		// the same resolver the implement-stage prompt uses (decomposed-
		// child/retry-parent fallback included). The conditions AMEND the
		// plan (#558), so the reviewer must see them or a diff correctly
		// implementing an amendment reads as a plan deviation. Fix-up
		// re-reviews inherit this for free: the post-fixup re-dispatch
		// routes through this same function.
		ApprovalConditions: s.resolveApprovalConditions(ctx, runRow),
		// The stage's previously recorded OPEN implement-review concerns for the
		// delta-verification section (#984): open states the reviewer must
		// resolve via concern_resolutions. A first review finds no rows → empty
		// set → the section is omitted and the prompt stays byte-identical; the
		// post-fixup re-review (same stage_id, new head_sha per #797) finds the
		// addressed_pending rows the fix-up trigger wrote.
		PriorConcerns: s.priorConcernsForReview(ctx, runID, stageID),
		// The stage's SETTLED concerns for the "Settled concerns" ledger
		// (#1913): operator arbitrations (waived, deferred) + prior rounds'
		// resolved rows (addressed, superseded), carried forward so a round-N
		// reviewer has the settled history and does not re-raise a settled
		// finding reworded. Empty on a first review → the ledger is omitted and
		// the prompt stays byte-identical to the pre-#1913 output.
		SettledConcerns: s.settledConcernsForReview(ctx, runID, stageID),
		// Machine-verified gate results from the bundle's gate_evidence
		// event (#963), pre-redacted runner-side. Nil (older bundles,
		// no gate ran, extraction error) keeps the prompt byte-identical.
		GateEvidence: gateEvidence,
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

	// Delta re-review (#1725). On a post-fix-up re-review — detected by the
	// presence of an addressed_pending prior concern (the rows the fix-up trigger
	// wrote when it routed concerns back to the agent) — replace the full
	// base..head PR diff with ONLY the fix-up delta since the head the previous
	// review ran against, so the reviewer focuses on whether the routed concerns
	// are resolved and the stable prefix caches across rounds. resolveFixupDeltaDiff
	// fails closed to the full diff on ANY miss (not wired, no PR,
	// unresolvable/degenerate prior head, compare error), so first-review coverage
	// is unchanged. A first review has no prior concerns and never enters this
	// branch.
	//
	// The trigger is specifically a ROUTED concern (addressed_pending), NOT any
	// prior concern. Since #1913 waived concerns no longer ride trig.PriorConcerns
	// (they moved to the settled ledger), but the guard still keys on
	// addressed_pending directly, so a run whose only settled context is a waived
	// concern — with no routed round — keeps the full review diff rather than
	// collapsing to a delta with delta framing. The routed concerns and the
	// reviewer's concern_resolutions delta-verification mechanism are preserved
	// either way — they ride on trig.PriorConcerns, which is already set above.
	if hasFixupRoutedConcern(trig.PriorConcerns) {
		if delta, ok := s.resolveFixupDeltaDiff(ctx, runRow, runID, stageID, headSHA); ok {
			trig.Diff = renderDiffForReview(delta)
			trig.DiffPatch = delta.Patch
			trig.DeltaReReview = true
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "implement review: re-review diff mode",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("mode", "delta"),
			)
		} else {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "implement review: re-review diff mode",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("mode", "full"),
			)
		}
	}

	// Operator-scope-undelivered pre-review signal (#1407): the operator may
	// have DELIBERATELY added a scope path — either an add_scope_files path
	// folded at plan approval (already computed as trig.AmendedScopeFiles) or
	// an approved mid-stage scope amendment — that the implement commit left
	// UNTOUCHED. Today that is conflated with benign plan-scope under-staging
	// and surfaces only at the implement-review reject → fixup round-trip
	// (E23.9/E23.10). Union both operator-add provenance channels and compute
	// the subset absent from the committed diff. Both channel lookups are
	// best-effort and never block the review (approvedAmendmentScopePaths
	// WARN-logs a nil repo / list error and contributes nothing).
	operatorAdded := append([]string(nil), trig.AmendedScopeFiles...)
	operatorAdded = append(operatorAdded, s.approvedAmendmentScopePaths(ctx, runID)...)
	if undelivered := operatorScopeUndelivered(operatorAdded, diff); len(undelivered) > 0 {
		// Populate the prompt signal so the reviewer sees the miss as a
		// high-priority gate-evidence warning. Allocate gateEvidence if the
		// bundle carried none (mirrors the existing allocate-if-needed
		// pattern), then thread it onto the trigger.
		if gateEvidence == nil {
			gateEvidence = &prompt.GateEvidence{}
			trig.GateEvidence = gateEvidence
		}
		gateEvidence.OperatorScopeUndelivered = undelivered

		// Append the deterministic advisory audit entry so the miss is
		// visible on the run surface BEFORE any reviewer verdict. Best-effort
		// with a WARN on failure, exactly like the implement_review_skipped
		// emission; a nil AuditRepo skips the entry (mirrors that guard).
		if s.cfg.AuditRepo != nil {
			payload, _ := json.Marshal(operatorScopeUndeliveredPayload{
				UndeliveredPaths:   undelivered,
				UndeliveredCount:   len(undelivered),
				OperatorAddedCount: len(operatorAdded),
			})
			systemKind := audit.ActorKind("system")
			if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
				RunID:     runID,
				StageID:   &stageID,
				Timestamp: time.Now().UTC(),
				Category:  operatorScopeUndeliveredCategory,
				ActorKind: &systemKind,
				Payload:   payload,
			}); aerr != nil {
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: append operator_scope_path_undelivered audit entry failed",
					slog.String("run_id", runID.String()),
					slog.String("stage_id", stageID.String()),
					slog.String("error", aerr.Error()),
				)
			}
		}
	}

	// Declared-scope provenance decomposition (#1914): reconstruct the effective
	// scope and decompose the declared-vs-staged count so the reviewer can
	// machine-classify a fold-only divergence as NON-drift. Adjacent to the
	// #1407 block and using the same allocate-if-nil gateEvidence pattern; the
	// #1407 operator_scope_path_undelivered signal is deliberately unchanged.
	if prov := s.scopeProvenanceForReview(ctx, runID, stageID, approvedPlan, trig, diff, gateEvidence); prov != nil {
		if gateEvidence == nil {
			gateEvidence = &prompt.GateEvidence{}
			trig.GateEvidence = gateEvidence
		}
		gateEvidence.ScopeProvenance = prov
	}

	promptText, err := prompt.Build("implement_review", trig)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: build prompt failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}

	// Idempotency guard (#797): the outer raw-variant gate (#793) already
	// dedups the raw+redacted pair of one pack, but a retried raw upload (a
	// transient 5xx after the review already dispatched → runner re-POSTs
	// raw) would otherwise dispatch a SECOND review with divergent verdicts
	// and #777 hint over-fire. Skip dispatch when an implement_review_started
	// entry already exists for this stage with the SAME head_sha. Keying on
	// (stage_id, head_sha) preserves the legitimate FixupStage re-review
	// (same stage_id, NEW head_sha). Fails open: implementReviewAlreadyStarted
	// returns false for an empty headSHA (the no-verify / head_sha-less path),
	// and a read error WARN-logs and falls through to dispatch — an absent or
	// unreadable key never suppresses a review.
	if headSHA != "" && s.cfg.AuditRepo != nil {
		started, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "implement_review_started")
		if lerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: list implement_review_started failed — proceeding with dispatch",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", lerr.Error()),
			)
		} else if implementReviewAlreadyStarted(started, stageID, headSHA) {
			return false
		}
	}

	// Pending-signal (#600): emit an implement_review_started audit entry
	// now that a reviewer will actually run. Emitted synchronously before
	// the dispatch loop so started precedes every implement_reviewed entry
	// under both authorities — the MCP review_status proxy reads it to tell
	// 'configured + running' (pending) from 'none configured'. Mirrors the
	// plan path (runPlanReviews). Best-effort: never blocks dispatch.
	s.emitReviewStarted(ctx, runID, stageID, "implement_review_started", authority, reviewersCfg.AgentCount(), headSHA)

	// Resolve the per-invocation reviewer list (#955) up front so the
	// detached goroutine closes over fully-resolved adapters, never the
	// spec config or request-scoped state. The gate-resolved review_model
	// override (#1416/#1426) is threaded in here so an operator-supplied
	// review_model actually reaches the reviewer adapter; an empty override
	// (no review model_resolved entry) leaves the spawn byte-identical to today.
	reviewModelOverride := s.gateResolvedReviewModel(ctx, runID)
	invocations := s.resolveReviewerInvocationsWithReviewModel(reviewersCfg, reviewModelOverride)

	// Detach the reviewer context from the request lifecycle (#584); see
	// runPlanReviews for the rationale. The goroutine / loop closes over
	// only already-resolved values (built prompt, IDs, authority, author
	// model) — never r, the bundle, or request-scoped state.
	authorModel := approvedPlan.GeneratedBy.Model
	reviewCtx := context.WithoutCancel(ctx)

	// Per-stage review-budget floor (#1494): the implement stage's
	// reviewers.review_timeout OVERRIDES the FISHHAWKD_PLAN_REVIEW_TIMEOUT
	// deployment default (s.cfg.ReviewBudget.Floor). Only the Floor rung is
	// overridden; PerKB and Cap stay deployment-level.
	stageBudget := s.cfg.ReviewBudget
	stageBudget.Floor = spec.ResolveReviewTimeout(reviewersCfg, s.cfg.ReviewBudget.Floor)

	// Advisory: dispatch detached so the terminal transition can proceed
	// without waiting on the reviewer.
	if authority != planreview.AuthorityGating {
		s.bgReviews.Add(1)
		go func() {
			defer s.bgReviews.Done()
			s.runImplementReviewInvocations(reviewCtx, runID, stageID, invocations, authority, promptText, authorModel, "", "", stageBudget)
		}()
		return false
	}

	// Gating: run synchronously so the caller can fail the stage as
	// category-B before the terminal transition.
	return s.runImplementReviewInvocations(reviewCtx, runID, stageID, invocations, authority, promptText, authorModel, "", "", stageBudget)
}

// amendedScopeFilesForReview computes the approval-time scope folds that the
// implement-review prompt must treat as in-scope rather than as scope drift
// (#829). Despite the name it now also feeds the implement-STAGE prompt: both
// handleGetStagePrompt and handleGetStagePromptRender call it to populate
// Trigger.AmendedScopeFiles so the implement agent SEES the operator-added paths
// as in-scope and does not file a redundant mid-stage amendment for paths
// already folded into the enforced scope (#1406). It reuses the SAME single fold
// source handleGetStagePrompt applies to the implement-stage prompt —
// resolveApprovalAddScopeFiles (#824 structured add_scope_files) — so the
// review-side, stage-side, and enforced-scope folds all stay in lockstep (single
// source of truth). The name is retained to avoid churn (the #1225 regression
// test pins it). The #730 approve-reason prose fold
// (extractScopePathsFromConditions over resolveApprovalConditions) was removed
// from BOTH sides in #1225: a repo-relative token scraped out of the operator's
// free-text reason no longer mutates scope, so the review side must no longer
// surface one either, or it would flag a prose-named committed path as scope
// drift while the stage no longer scopes it. It returns the structured fold
// paths, deduped and EXCLUDING any path already present in the raw plan
// scope.files (those are already rendered by writePlanForReview, so naming them
// again would only restate the existing scope).
//
// It mirrors the merge helpers' empty-scope philosophy: when the plan declares
// no scope.files the reviewer has no baseline to drift against, so there is
// nothing to amend and it returns nil. Returns nil (not an empty slice) when
// there is no amendment, keeping the review prompt byte-identical to today.
func (s *Server) amendedScopeFilesForReview(ctx context.Context, runRow *run.Run, approvedPlan *plan.Plan) []string {
	if approvedPlan == nil || len(approvedPlan.Scope.Files) == 0 {
		return nil
	}
	inRawScope := make(map[string]struct{}, len(approvedPlan.Scope.Files))
	for _, f := range approvedPlan.Scope.Files {
		inRawScope[f.Path] = struct{}{}
	}

	var amended []string
	seen := make(map[string]struct{})
	add := func(paths []string) {
		for _, p := range paths {
			if _, ok := inRawScope[p]; ok {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			amended = append(amended, p)
		}
	}

	add(s.resolveApprovalAddScopeFiles(ctx, runRow))
	return amended
}

// approvedAmendmentScopePaths returns the paths of every APPROVED mid-stage
// scope amendment on the run (#1407). It is the SECOND operator-add provenance
// channel the operator_scope_path_undelivered signal must union with the
// approval-time add_scope_files folds — amendedScopeFilesForReview folds ONLY
// resolveApprovalAddScopeFiles, never approved mid-stage amendments, so relying
// on it alone would miss the amendment channel that recurred in E23.9/E23.10.
// Best-effort and fail-closed: a nil ScopeAmendmentRepo or a ListByRun error
// WARN-logs and contributes nothing, never blocking the review. Filters to
// StatusApproved (pending/denied confer nothing). Returns order-preserving paths
// (de-dup is performed by the downstream operatorScopeUndelivered set walk).
func (s *Server) approvedAmendmentScopePaths(ctx context.Context, runID uuid.UUID) []string {
	if s.cfg.ScopeAmendmentRepo == nil {
		return nil
	}
	items, err := s.cfg.ScopeAmendmentRepo.ListByRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"implement review: list scope amendments failed — operator-scope-undelivered signal contributes nothing for this run",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	var out []string
	for _, a := range items {
		if a.Status != scopeamendment.StatusApproved {
			continue
		}
		for _, p := range a.Paths {
			out = append(out, p.Path)
		}
	}
	return out
}

// scopeProvenanceForReview reconstructs the implement stage's effective
// scope.files and decomposes the declared-vs-staged count into its provenance
// (#1914), so the implement reviewer can machine-classify a fold-only count
// divergence as NON-drift instead of waiving it as a false positive (the class
// of six near-identical waivers across the 2026-07-12/13 drives). It runs
// adjacent to the #1407 operator-scope-undelivered block, reusing the same
// resolvers the prompt-serve folds use — so the partition matches the runner's
// served DeclaredFiles by construction, and any residual disagreement surfaces
// honestly as UnexplainedCount rather than being hidden.
//
// The effective set is rebuilt in the SAME fold order handleGetStagePrompt
// applies (server/prompt.go): plan scope.files, then trig.AmendedScopeFiles
// (approval-add-scope-files), approvedAmendmentScopePaths (scope-amendment),
// and — on a fix-up pass only — resolveFixupAllowCreate (fixup-allow-create)
// and the coupled *_test.go stem siblings over the accumulated set
// (fixup-coupled-test-sibling), deduped by path with first-source-wins
// (plan wins over any fold), matching foldScopePaths semantics.
//
// Each plan/fold path is marked touched by membership in the committed diff
// using the same set walk + trailing-slash / non-repo-relative skips as
// operatorScopeUndelivered (a directory or absolute/traversal token can never
// name a committed path, so it is treated as touched rather than a false
// untouched-permission signal). UnexplainedCount = max(0, DeclaredFiles - the
// reconstructed size) when ScopeFacts is present (nil ScopeFacts → 0).
//
// Best-effort throughout: any channel lookup WARN-logs inside its resolver and
// contributes nothing; the construction never blocks review dispatch. Returns
// nil when there is nothing to report (no folds, no untouched plan path, no
// unexplained residual, and not a fix-up pass), keeping the prompt
// byte-identical for the common plain-plan-scope case.
func (s *Server) scopeProvenanceForReview(ctx context.Context, runID, stageID uuid.UUID, approvedPlan *plan.Plan, trig prompt.Trigger, diff policy.Diff, ev *prompt.GateEvidence) *prompt.GateScopeProvenance {
	if approvedPlan == nil || len(approvedPlan.Scope.Files) == 0 {
		return nil
	}

	committed := make(map[string]struct{}, len(diff.ChangedFiles))
	for _, f := range diff.ChangedFiles {
		committed[f.Path] = struct{}{}
	}
	// touched mirrors operatorScopeUndelivered's skips: a trailing-slash
	// directory prefix or a non-repo-relative token can never match a committed
	// diff path, so it is treated as touched (not a false untouched signal).
	touched := func(p string) bool {
		if strings.HasSuffix(p, "/") || !isRepoRelativePath(p) {
			return true
		}
		_, ok := committed[p]
		return ok
	}

	inScope := make(map[string]struct{}, len(approvedPlan.Scope.Files))
	accum := make([]scopeFile, 0, len(approvedPlan.Scope.Files))
	var planPaths []string
	for _, f := range approvedPlan.Scope.Files {
		if _, ok := inScope[f.Path]; ok {
			continue
		}
		inScope[f.Path] = struct{}{}
		planPaths = append(planPaths, f.Path)
		accum = append(accum, scopeFile{Path: f.Path, Operation: string(f.Operation)})
	}

	var folds []prompt.GateScopeFold
	addFold := func(paths []string, source string) {
		for _, p := range paths {
			// first-source-wins dedup (plan wins over folds; earlier fold wins
			// over later), matching foldScopePaths' compare-by-Path semantics.
			if _, ok := inScope[p]; ok {
				continue
			}
			inScope[p] = struct{}{}
			accum = append(accum, scopeFile{Path: p, Operation: "modify"})
			folds = append(folds, prompt.GateScopeFold{Path: p, Source: source, Touched: touched(p)})
		}
	}
	addFold(trig.AmendedScopeFiles, "approval-add-scope-files")
	addFold(s.approvedAmendmentScopePaths(ctx, runID), "scope-amendment")
	fixupPass := hasFixupRoutedConcern(trig.PriorConcerns)
	if fixupPass {
		addFold(s.resolveFixupAllowCreate(ctx, runID, stageID), "fixup-allow-create")
		// Coupled stem-sibling tests fold over the accumulated set (plan + prior
		// folds), mirroring effectiveFixupScope's final fold so the reconstruction
		// reproduces the served scope.
		addFold(coupledTestSiblings(accum), "fixup-coupled-test-sibling")
	}

	var planUntouched []string
	for _, p := range planPaths {
		if !touched(p) {
			planUntouched = append(planUntouched, p)
		}
	}

	unexplained := 0
	if ev != nil && ev.ScopeFacts != nil {
		if d := ev.ScopeFacts.DeclaredFiles - len(inScope); d > 0 {
			unexplained = d
		}
	}

	// Nothing to report → nil keeps the prompt byte-identical for the common
	// plain-plan-scope case (no folds, every plan file touched, no residual).
	if len(folds) == 0 && len(planUntouched) == 0 && unexplained == 0 && !fixupPass {
		return nil
	}

	return &prompt.GateScopeProvenance{
		PlanFiles:        len(planPaths),
		PlanUntouched:    planUntouched,
		Folds:            folds,
		FixupPass:        fixupPass,
		UnexplainedCount: unexplained,
	}
}

// runImplementReviewInvocations runs the per-reviewer implement-review loop
// shared by the synchronous (gating) and detached (advisory) dispatch
// paths. For each resolved invocation it calls its reviewer's Review, logs
// WARN on self-review (reviewer model == authorModel), and appends one
// implement_reviewed audit entry. Returns true when at least one verdict
// is reject. It performs no stage transition — the gating caller owns the
// failed-B transition so the advance-blocking edge stays on the
// synchronous path only.
//
// origin and headSHA stamp the verdict's provenance (#1250): the first-review
// and parent-decomposition callers pass "" for both, keeping their
// implement_reviewed payloads byte-identical; the base-rebase re-invoke
// supplemental caller passes Origin="base_rebase_reinvoke" + the re-landed
// head SHA so the additive verdict is labelable and the dispatch idempotent
// on (stage_id, Origin, HeadSHA).
func (s *Server) runImplementReviewInvocations(ctx context.Context, runID, stageID uuid.UUID, invocations []reviewerInvocation, authority planreview.AuthorityMode, promptText, authorModel, origin, headSHA string, reviewBudget planreview.ReviewBudget) bool {
	systemKind := audit.ActorKind("system")
	hasRejection := false
	// pagedRejectAppended tracks whether THIS loop appended a page-class audit
	// entry — an implement_reviewed reject verdict (#1786). Gating the
	// immediate hook on this (not on an unconditional call) keeps an
	// all-approve loop from calling NotifyPageClassForRun, which evaluates the
	// full audit history and would otherwise flush an OLDER unpinged
	// page-class event at this unrelated moment.
	pagedRejectAppended := false
	budget := reviewBudget.Budget(len(promptText))
	for i, inv := range invocations {
		// An unresolvable provider is a deployment CAPABILITY gap, not a
		// reviewer error (#1495, reframes #955): the spec-declared provider is
		// unavailable on this deployment. Emit a capability-framed terminal
		// implement_review_skipped entry honoring the per-reviewer optional
		// flag (loud for optional:false, quiet for optional:true), continue,
		// hasRejection untouched. implement_review_skipped counts as terminal
		// (planreview.Settled), so the review-settled gate still resolves.
		if inv.resolveErr != nil {
			s.emitReviewerUnavailable(ctx, runID, stageID, "implement_review_skipped", authority, inv.provider, inv.optional, len(invocations), inv.resolveErr)
			continue
		}
		// Resolve the reviewer's CLI version + binary-path provenance once per
		// invocation (#1768) and stamp both onto the implement_reviewed payload
		// below. The implement loop has no agent_version guard, so this is a
		// straight provenance add; empty for the non-codex adapters (omitempty).
		reviewerVersion, reviewerBinary := s.resolveReviewerProvenance(ctx, inv.reviewer)
		// Apply the size-aware per-invocation budget (#747) as a context
		// deadline. Implement-review inputs (the diff) are larger than plan
		// review, so the size-aware formula naturally grants a larger budget
		// here with no separate per-stage default. cancel() per turn so
		// deadlines don't accumulate across reviewers.
		invocationCtx, cancel := context.WithTimeout(ctx, budget)
		verdict, model, err := inv.reviewer.Review(invocationCtx, promptText)
		timedOut := errors.Is(invocationCtx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: reviewer invocation failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.Int("reviewer_index", i),
				slog.Bool("timed_out", timedOut),
				slog.Duration("budget", budget),
				slog.String("error", err.Error()),
			)
			// Terminal implement_review_failed audit entry (#664), mirroring
			// the plan path: surfaces a timed-out / errored reviewer as a
			// definite 'failed' state, with the #747 timeout discriminator
			// distinguishing a budget-kill from a transport failure.
			// hasRejection untouched (#574).
			s.emitReviewFailed(ctx, runID, stageID, "implement_review_failed", authority, model, err.Error(), timedOut)
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
			// The reviewer's delta-verification verdicts on prior concerns
			// (#984) ride on the authoritative audit payload; the concern
			// store applies them below as a derived index.
			ConcernResolutions: verdict.ConcernResolutions,
			// Per-invocation token usage on the review surface (#995).
			InputTokens:  verdict.Usage.InputTokens,
			OutputTokens: verdict.Usage.OutputTokens,
			// Provenance markers (#1250): empty for the first review and the
			// parent-decomposition consolidated review (byte-identical via
			// omitempty); set for the base-rebase re-invoke supplemental pass.
			Origin:  origin,
			HeadSHA: headSHA,
			// Resolved reviewer CLI version + binary-path provenance (#1768),
			// probed once above. Empty for non-codex reviewers (omitempty).
			ReviewerVersion: reviewerVersion,
			ReviewerBinary:  reviewerBinary,
		}
		payloadBytes, _ := json.Marshal(payload)
		entry, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     runID,
			StageID:   &stageID,
			Timestamp: time.Now().UTC(),
			Category:  "implement_reviewed",
			ActorKind: &systemKind,
			Payload:   payloadBytes,
		})
		if aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: append audit entry failed",
				slog.String("run_id", runID.String()),
				slog.String("error", aerr.Error()),
			)
		} else if entry != nil {
			// Persist the verdict's concerns with stable IDs (#964),
			// stamped with the sequence the append returned — the audit
			// chain stays the sole sequence authority, so a failed append
			// (no sequence) skips persistence for this verdict.
			s.persistReviewConcerns(ctx, runID, stageID, concern.StageKindImplement, model, entry.Sequence, verdict.Concerns)
			// Apply the delta-verification resolutions to the concern
			// store (#984) — same append-gated, best-effort posture.
			s.applyConcernResolutions(ctx, runID, stageID, verdict.ConcernResolutions)
		}

		// Capture this reviewer invocation's agent token cost (#681). The
		// usage rode in on the planreview.ReviewVerdict contract; we price
		// and record it here, backend-agnostically.
		s.recordReviewerCost(ctx, runID, stageID, model, verdict.Usage, "implement_review")

		if verdict.Verdict == planreview.VerdictReject {
			hasRejection = true
			pagedRejectAppended = true
		}
	}

	// The implement review has now written its terminal entries
	// (implement_reviewed / implement_review_failed). Re-derive and
	// republish fishhawk_audit_complete so the #947 review-pending presence
	// gate flips green automatically once the advisory review lands — GitHub
	// re-evaluates branch protection on the Check Run conclusion update, so
	// the merge clears with no operator action. Best-effort: never affects
	// the review loop's return (a publish failure recomputes on the next SPA
	// visit or PR webhook).
	s.recomputeAndPublishAuditComplete(ctx, runID)

	// Fire the page-class ping immediately (#1786) so a reviewer reject pages
	// the operator within the review append flow rather than riding the next
	// transition (the operator's own fixup/approve) minutes later — but ONLY
	// when this loop actually appended a reject verdict (the page-class event).
	// An all-approve loop appends no page-class entry, so an unconditional call
	// would flush an OLDER unpinged page-class event at this unrelated moment.
	// Deduped on the source Sequence, so it never double-posts with
	// notifyStatusUpdate.
	if pagedRejectAppended {
		s.notifyPageClass(ctx, runID, "implement_review")
	}

	return hasRejection
}

// runSupplementalReinvokeReview dispatches the bounded, ADDITIVE supplemental
// implement-review pass for a base-rebase re-invoke ship (#1250). When a
// re-invoke ship carries a non-empty supplemental scope-exemption delta — the
// extra declared-scope-file exemptions the final scope-completeness gate
// honored AFTER the first review's sealed bundle shipped under #742 forward
// gating — this dispatches a second, exemption-scoped review against the
// PUSHED re-landed tree so the delta reaches the implement-review surface, not
// only the #1218 audit row.
//
// It reuses the lower-level implement-review machinery: resolveStageReviewers
// + ResolveAuthority (skip when no agent reviewers), the wired-reviewer check
// (skip when none), loadApprovedPlanForRun (skip on nil — nothing to judge
// soundness against), resolveReviewerInvocations, and
// runImplementReviewInvocations, stamping Origin=base_rebase_reinvoke +
// headSHA so the verdict is labelable and the dispatch idempotent.
//
// CRITICAL — it does NOT call emitReviewStarted. The anchor floors
// verdict-counting at the latest implement_review_started Sequence
// (anchor_template.go), so emitting a fresh started would advance the floor
// and BURY the first review's verdict. Skipping it keeps the floor at the
// first review and lets this implement_reviewed entry count ADDITIVELY above
// it.
//
// Returns true ONLY on a gating-authority reject (the caller fails the stage
// category-B). Advisory dispatch is detached and returns false. A skipped
// dispatch (no reviewers, no backend, no plan, or an idempotent duplicate)
// returns false.
func (s *Server) runSupplementalReinvokeReview(ctx context.Context, runID, stageID uuid.UUID, headSHA string, exemptions []prompt.GateScopeExemption) bool {
	if s.cfg.RunRepo == nil || len(exemptions) == 0 {
		return false
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "supplemental reinvoke review: get run failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}

	reviewersCfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement)
	if reviewersCfg == nil || reviewersCfg.AgentCount() == 0 {
		return false
	}
	authority := planreview.ResolveAuthority(*reviewersCfg)

	// No reviewer backend wired: nothing can run. The first-review path
	// already emitted implement_review_skipped for this stage, so re-emitting
	// here would only double-record the same degradation; skip quietly.
	if s.defaultPlanReviewer() == nil {
		return false
	}

	// Load the approved plan: the supplemental prompt judges exemption
	// soundness against the plan's scope/approach (and the self-review guard
	// needs GeneratedBy.Model). No plan → nothing to measure against → skip.
	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "supplemental reinvoke review: load plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	if approvedPlan == nil {
		return false
	}

	// Idempotency (#1250): a retried PR-upload with the SAME re-landed head SHA
	// must not dispatch a second supplemental review. Dedup on
	// (stage_id, Origin=base_rebase_reinvoke, head_sha). Best-effort and
	// non-atomic — acceptable because the runner drives a stage's PR-uploads
	// serially, so there is no concurrent second dispatch to race; a list
	// error WARN-logs and falls through to dispatch (fail-open, never suppress
	// a review on a read failure). An empty headSHA cannot be deduped, but the
	// caller only invokes this on a success ship that carries pr.HeadSHA.
	if headSHA != "" && s.cfg.AuditRepo != nil {
		reviewed, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "implement_reviewed")
		if lerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "supplemental reinvoke review: list implement_reviewed failed — proceeding with dispatch",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", lerr.Error()),
			)
		} else if supplementalReinvokeReviewAlreadyRecorded(reviewed, stageID, headSHA) {
			return false
		}
	}

	trig := prompt.Trigger{
		Repo:                 runRow.Repo,
		ApprovedPlan:         approvedPlan,
		SupplementalReinvoke: true,
		// The additional exemption delta is the entire subject of this pass;
		// it rides in GateEvidence so buildImplementReview's supplemental
		// branch renders it via the shared ScopeExemptions section.
		GateEvidence: &prompt.GateEvidence{ScopeExemptions: exemptions},
		// The operator's binding approve-with-conditions text (#1021): an
		// exemption may be sound only in light of a condition, so the
		// supplemental reviewer must see the same conditions the first review
		// saw. Same resolver runImplementReviews uses.
		ApprovalConditions: s.resolveApprovalConditions(ctx, runRow),
	}
	if runRow.IssueContext != nil {
		trig.IssueTitle = runRow.IssueContext.Title
		trig.IssueBody = runRow.IssueContext.Body
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
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "supplemental reinvoke review: build prompt failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}

	// Thread the gate-resolved review_model override (#1416/#1426) so the
	// supplemental reinvoke verdict runs under the same operator-resolved model
	// as the first review; an empty override leaves the spawn byte-identical to today.
	reviewModelOverride := s.gateResolvedReviewModel(ctx, runID)
	invocations := s.resolveReviewerInvocationsWithReviewModel(reviewersCfg, reviewModelOverride)
	authorModel := approvedPlan.GeneratedBy.Model
	reviewCtx := context.WithoutCancel(ctx)

	// Per-stage review-budget floor (#1494): same resolution as the first
	// implement review — the stage's reviewers.review_timeout overrides the
	// FISHHAWKD_PLAN_REVIEW_TIMEOUT deployment default; PerKB and Cap stay
	// deployment-level.
	stageBudget := s.cfg.ReviewBudget
	stageBudget.Floor = spec.ResolveReviewTimeout(reviewersCfg, s.cfg.ReviewBudget.Floor)

	// Advisory: dispatch detached so the PR-upload response is not blocked on
	// the reviewer. Stamp the provenance markers so the additive verdict is
	// labelable and idempotent. Deliberately no emitReviewStarted (see the
	// function doc): the first review's started entry remains the anchor floor.
	if authority != planreview.AuthorityGating {
		s.bgReviews.Add(1)
		go func() {
			defer s.bgReviews.Done()
			s.runImplementReviewInvocations(reviewCtx, runID, stageID, invocations, authority, promptText, authorModel, planreview.OriginBaseRebaseReinvoke, headSHA, stageBudget)
		}()
		return false
	}

	// Gating: run synchronously so the caller can fail the stage category-B
	// before responding. Same no-started-emission discipline.
	return s.runImplementReviewInvocations(reviewCtx, runID, stageID, invocations, authority, promptText, authorModel, planreview.OriginBaseRebaseReinvoke, headSHA, stageBudget)
}

// supplementalReinvokeReviewAlreadyRecorded reports whether a base-rebase
// re-invoke supplemental implement_reviewed entry for the given stage with the
// same re-landed head SHA already exists (#1250), so a retried PR-upload is
// deduped before re-dispatching. It mirrors implementReviewAlreadyStarted: the
// entries are sequence-ascending, nil/mismatched StageIDs are skipped, and the
// key is (stage_id, Origin=base_rebase_reinvoke, head_sha) — only the
// supplemental verdict carries Origin, so the first review's origin-less
// entries never match. Fail-open on an empty headSHA (returns false), though
// the caller always supplies pr.HeadSHA.
func supplementalReinvokeReviewAlreadyRecorded(entries []*audit.Entry, stageID uuid.UUID, headSHA string) bool {
	if headSHA == "" {
		return false
	}
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			Origin  string `json:"origin"`
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.Origin == planreview.OriginBaseRebaseReinvoke && payload.HeadSHA == headSHA {
			return true
		}
	}
	return false
}

// priorConcernsForReview gathers the stage's OPEN implement-review concerns
// for the implement-review prompt's delta-verification section (#984): every
// open-state concern (raised, addressed_pending, reopened) the reviewer must
// explicitly confirm/reopen/supersede via concern_resolutions. It feeds
// Trigger.PriorConcerns and hasFixupRoutedConcern.
//
// The SETTLED rows (waived/deferred + addressed/superseded) are gathered
// separately by settledConcernsForReview (#1913): waived concerns MOVED out of
// this open set into the settled ledger, so a run whose only prior concerns are
// waived no longer carries them here (hasFixupRoutedConcern still keys on
// addressed_pending, so the delta-collapse gate is unaffected).
//
// Best-effort: a nil repo or a list error returns nil (warn-logged) — a store
// outage must never block review dispatch, and an empty set keeps the prompt
// byte-identical to the pre-#984 output.
func (s *Server) priorConcernsForReview(ctx context.Context, runID, stageID uuid.UUID) []prompt.PriorConcern {
	if s.cfg.ConcernRepo == nil {
		return nil
	}
	rows, err := s.cfg.ConcernRepo.ListByRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: list concerns failed — dispatching without the prior-concerns section",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	var out []prompt.PriorConcern
	for _, c := range rows {
		if c.StageID != stageID || c.StageKind != concern.StageKindImplement {
			continue
		}
		if !c.State.IsOpen() {
			continue
		}
		out = append(out, prompt.PriorConcern{
			ID:          c.ID.String(),
			State:       string(c.State),
			Severity:    c.Severity,
			Category:    c.Category,
			Note:        c.Note,
			StateReason: c.StateReason,
		})
	}
	return out
}

// settledConcernsForReview gathers the stage's SETTLED implement-review concerns
// for the implement-review prompt's "Settled concerns" ledger (#1913): the
// operator-arbitrated rows (waived, deferred) plus the prior rounds' resolved
// rows (addressed, superseded), each carrying its audited StateReason. Threading
// these into every post-fixup round closes the two convergence gaps the issue
// measured: DEFERRED arbitrations reach the reviewer at all (they were dropped
// entirely before), and delta rounds N>=2 carry the full settled history instead
// of only the currently-open rows — so a round-N reviewer stops re-raising a
// settled finding reworded.
//
// Same best-effort posture as priorConcernsForReview: a nil repo or a list error
// returns nil (warn-logged), and an empty set keeps the prompt byte-identical to
// the pre-#1913 output.
func (s *Server) settledConcernsForReview(ctx context.Context, runID, stageID uuid.UUID) []prompt.PriorConcern {
	if s.cfg.ConcernRepo == nil {
		return nil
	}
	rows, err := s.cfg.ConcernRepo.ListByRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: list concerns failed — dispatching without the settled-concerns section",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	var out []prompt.PriorConcern
	for _, c := range rows {
		if c.StageID != stageID || c.StageKind != concern.StageKindImplement {
			continue
		}
		switch c.State {
		case concern.StateWaived, concern.StateDeferred, concern.StateAddressed, concern.StateSuperseded:
		default:
			continue
		}
		out = append(out, prompt.PriorConcern{
			ID:          c.ID.String(),
			State:       string(c.State),
			Severity:    c.Severity,
			Category:    c.Category,
			Note:        c.Note,
			StateReason: c.StateReason,
		})
	}
	return out
}

// hasFixupRoutedConcern reports whether any prior concern was actually routed
// back to the agent for fix-up — i.e. transitioned to addressed_pending by the
// fix-up trigger. This is the discriminator for a post-fix-up delta re-review
// (#1725): keying the delta collapse on len(PriorConcerns) alone would be wrong
// — a reopened concern is open but not necessarily fix-up-routed — so requiring
// an addressed_pending concern keeps the full diff when no fix-up round happened.
// (Since #1913 waived concerns no longer appear in PriorConcerns at all; they
// moved to the settled ledger, so they can never spuriously trip this gate.)
func hasFixupRoutedConcern(prior []prompt.PriorConcern) bool {
	for _, c := range prior {
		if c.State == string(concern.StateAddressedPending) {
			return true
		}
	}
	return false
}

// resolveFixupDeltaDiff computes the fix-up delta for a post-fix-up re-review of
// the implement stage (#1725): the base..head compare between the head the
// PREVIOUS review ran against and the current head, via githubclient.ComparePatch
// — the same seam DispatchConsolidatedReview reuses, mapped through
// consolidatedReviewDiff. It returns the delta as a policy.Diff and ok=true ONLY
// when every precondition holds: a GitHub client is wired, the run has an
// installation and an open PR, the current head is present, the prior-reviewed
// head resolves AND differs from the current head, the repo parses, and
// ComparePatch succeeds. On ANY miss it returns ok=false (best-effort
// WARN/degrade, mirroring the other resolvers) so the caller keeps the full
// bundle diff — fail-closed to the pre-#1725 behavior, preserving first-review
// and no-GitHub coverage unchanged.
//
// The "missing PR number" degrade the ticket names maps to runRow.PullRequestURL
// being nil: ComparePatch itself operates on commit SHAs, not a PR number, but a
// fix-up re-review only happens on a run that reached the PR stage, so a nil
// PullRequestURL (no PR opened) is treated as "compare unavailable" alongside the
// no-client / no-installation cases.
func (s *Server) resolveFixupDeltaDiff(ctx context.Context, runRow *run.Run, runID, stageID uuid.UUID, currentHead string) (policy.Diff, bool) {
	// GitHub compare unavailable: no client, no installation, or no PR opened.
	// ComparePatch runs against the App installation over the run's PR, so all
	// three are hard preconditions (and the nil InstallationID guard prevents a
	// nil-pointer dereference below).
	if s.cfg.GitHub == nil {
		return policy.Diff{}, false
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return policy.Diff{}, false
	}
	if runRow.PullRequestURL == nil {
		return policy.Diff{}, false
	}
	// No current head to diff against (the no-verify / head_sha-less bundle).
	if currentHead == "" {
		return policy.Diff{}, false
	}
	// The head the previous review ran against. Unresolvable ("") — or equal to
	// the current head, a degenerate no-op compare that would starve the reviewer
	// of any diff — keeps the full diff.
	priorHead := s.resolvePriorReviewedHeadSHA(ctx, runID, stageID)
	if priorHead == "" || priorHead == currentHead {
		return policy.Diff{}, false
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: parse repo for fixup delta failed — keeping full diff",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return policy.Diff{}, false
	}
	cmp, err := s.cfg.GitHub.ComparePatch(ctx, *runRow.InstallationID, repo, priorHead, currentHead)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: compare patch for fixup delta failed — keeping full diff",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("prior_head", priorHead),
			slog.String("current_head", currentHead),
			slog.String("error", err.Error()),
		)
		return policy.Diff{}, false
	}
	return consolidatedReviewDiff(cmp), true
}

// resolvePriorReviewedHeadSHA returns the head_sha the PREVIOUS implement review
// of this stage ran against, for the delta re-review (#1725). It looks, in order:
//
//  1. the newest prior implement_reviewed entry for the stage carrying a head_sha;
//  2. else the newest prior implement_review_started entry for the stage carrying
//     a head_sha (both are stamped with the review round's head_sha per #797/#1250);
//  3. else the second-newest DISTINCT head across the reported-head ledger
//     (pull_request_opened / child_pushed / fixup_pushed) — the head before the
//     current fix-up head — for the head_sha-less prior-review case.
//
// Returns "" on any miss (nil AuditRepo, no entry, read error) — best-effort
// WARN-and-proceed, mirroring resolveNewestReportedHeadSHA. The caller then keeps
// the full diff. This runs BEFORE the current round emits its own
// implement_review_started entry, so every review entry it sees is from a prior
// round.
func (s *Server) resolvePriorReviewedHeadSHA(ctx context.Context, runID, stageID uuid.UUID) string {
	if s.cfg.AuditRepo == nil {
		return ""
	}
	// (1) and (2): the newest review entry for THIS stage that carries a head_sha.
	for _, cat := range []string{"implement_reviewed", "implement_review_started"} {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: list review entries for prior head failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("category", cat),
				slog.String("error", err.Error()),
			)
			continue
		}
		var newest *audit.Entry
		var newestSHA string
		for _, e := range entries {
			if e.StageID == nil || *e.StageID != stageID {
				continue
			}
			var payload struct {
				HeadSHA string `json:"head_sha"`
			}
			if uerr := json.Unmarshal(e.Payload, &payload); uerr != nil || payload.HeadSHA == "" {
				continue
			}
			if newest == nil || e.Timestamp.After(newest.Timestamp) ||
				(e.Timestamp.Equal(newest.Timestamp) && e.Sequence > newest.Sequence) {
				newest = e
				newestSHA = payload.HeadSHA
			}
		}
		if newestSHA != "" {
			return newestSHA
		}
	}
	// (3) second-newest distinct reported-head ledger entry.
	return s.resolveSecondNewestReportedHeadSHA(ctx, runID, stageID)
}

// resolveSecondNewestReportedHeadSHA returns the second-newest DISTINCT head_sha
// across the reported-head ledger (lineageLedgerCategories) — the head before the
// current one. It is the #1725 fallback behind resolvePriorReviewedHeadSHA for
// the case where the prior review recorded no head_sha (a head_sha-less bundle):
// the newest ledger head is the current fix-up head, so the prior head is the
// second-newest. Returns "" (best-effort WARN-and-omit) when the AuditRepo is
// unconfigured, fewer than two distinct heads exist, or on any read error.
func (s *Server) resolveSecondNewestReportedHeadSHA(ctx context.Context, runID, stageID uuid.UUID) string {
	if s.cfg.AuditRepo == nil {
		return ""
	}
	type headEntry struct {
		ts  time.Time
		seq int64
		sha string
	}
	var heads []headEntry
	for _, cat := range lineageLedgerCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "implement review: list reported-head ledger for prior head failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("category", cat),
				slog.String("error", err.Error()),
			)
			return ""
		}
		for _, e := range entries {
			var payload struct {
				HeadSHA string `json:"head_sha"`
			}
			if uerr := json.Unmarshal(e.Payload, &payload); uerr != nil || payload.HeadSHA == "" {
				continue
			}
			heads = append(heads, headEntry{ts: e.Timestamp, seq: e.Sequence, sha: payload.HeadSHA})
		}
	}
	// Newest-first by (timestamp, sequence).
	sort.Slice(heads, func(i, j int) bool {
		if heads[i].ts.Equal(heads[j].ts) {
			return heads[i].seq > heads[j].seq
		}
		return heads[i].ts.After(heads[j].ts)
	})
	// Return the second DISTINCT head (dedupe consecutive equal heads so an
	// unchanged head reported twice does not masquerade as the prior head).
	var newestSHA string
	for _, h := range heads {
		if newestSHA == "" {
			newestSHA = h.sha
			continue
		}
		if h.sha != newestSHA {
			return h.sha
		}
	}
	return ""
}

// applyConcernResolutions applies one reviewer's delta-verification
// resolutions (#984) to the durable concern store: confirmed →
// addressed, reopened → reopened, superseded → superseded, via
// ApplyResolution's state-machine validation. Every malformed or
// inapplicable entry — unparseable UUID, unknown ID, a concern from a
// different run/stage or a plan stage, an unknown resolution string, or
// an InvalidTransitionError — is WARN-logged and skipped, with valid
// sibling entries still applied: a sloppy reviewer can never wedge the
// gate, and the store stays the best-effort derived index #982
// established (the audit payload already carries the resolutions
// authoritatively). REOPEN WINS over confirm needs no reconciliation
// pass here: the state machine encodes it order-independently
// (addressed → reopened is a valid edge; reopened → addressed is
// absent and surfaces as the warn-logged InvalidTransitionError).
func (s *Server) applyConcernResolutions(ctx context.Context, runID, stageID uuid.UUID, resolutions []planreview.ConcernResolution) {
	if s.cfg.ConcernRepo == nil || len(resolutions) == 0 {
		return
	}
	warn := func(res planreview.ConcernResolution, reason string) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "concern resolutions: skipping entry",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("concern_id", res.ID),
			slog.String("resolution", res.Resolution),
			slog.String("reason", reason),
		)
	}
	for _, res := range resolutions {
		cid, perr := uuid.Parse(res.ID)
		if perr != nil {
			warn(res, "id is not a valid UUID: "+perr.Error())
			continue
		}
		var to concern.State
		switch res.Resolution {
		case "confirmed":
			to = concern.StateAddressed
		case "reopened":
			to = concern.StateReopened
		case "superseded":
			to = concern.StateSuperseded
		default:
			warn(res, "unknown resolution (want confirmed|reopened|superseded)")
			continue
		}
		rows, gerr := s.cfg.ConcernRepo.GetByIDs(ctx, []uuid.UUID{cid})
		if gerr != nil {
			warn(res, "get concern failed: "+gerr.Error())
			continue
		}
		row := rows[0]
		if row.RunID != runID || row.StageID != stageID || row.StageKind != concern.StageKindImplement {
			// A reviewer can never touch another run's, another stage's,
			// or a plan-stage concern through this path.
			warn(res, "concern belongs to a different run/stage or is not an implement-stage concern")
			continue
		}
		if _, aerr := s.cfg.ConcernRepo.ApplyResolution(ctx, cid, to, res.Note); aerr != nil {
			// InvalidTransitionError lands here — including the
			// confirm-after-reopen case across heterogeneous reviewers,
			// satisfying concern.go's never-silently-swallow contract.
			warn(res, "apply resolution failed: "+aerr.Error())
			continue
		}
	}
}

// persistReviewConcerns records one review verdict's concerns into the
// durable concern store with stable server-minted IDs (#964), stamped
// with the audit sequence AppendChained returned for the *_reviewed
// entry. Shared by the implement-review and plan-review loops.
// Best-effort like the append itself: a nil repo or an insert failure
// warn-logs and never fails the review loop — the audit payload remains
// the authoritative record and this store is a derived index over it.
func (s *Server) persistReviewConcerns(ctx context.Context, runID, stageID uuid.UUID, stageKind, reviewerModel string, originSequence int64, concerns []planreview.Concern) {
	if s.cfg.ConcernRepo == nil || len(concerns) == 0 {
		return
	}
	raised := make([]concern.RaisedConcern, 0, len(concerns))
	for _, c := range concerns {
		// Re-litigation guard (#1913): a concern re-raising an operator-arbitrated
		// (waived/deferred) settled concern with no new evidence is recorded as a
		// suppression audit entry instead of minted as a fresh open row — that
		// durable open row is what wedges the gate and drives repeat rounds. The
		// reviewer's verdict and its audit payload are recorded unchanged upstream;
		// the guard only stops the OPEN concern row from minting. Every other case
		// (unparsable/unknown ref, a ref to another run/stage or a non-waived/
		// deferred state, non-empty new_evidence, any lookup/append error) falls
		// open to the normal insert below, so a sloppy or absent tag can never
		// suppress a genuine finding and a store outage never wedges the loop.
		if s.suppressRelitigation(ctx, runID, stageID, stageKind, reviewerModel, originSequence, c) {
			continue
		}
		raised = append(raised, concern.RaisedConcern{
			Severity:       string(c.Severity),
			Category:       c.Category,
			Note:           c.Note,
			SuggestedPatch: c.SuggestedPatch,
		})
	}
	if len(raised) == 0 {
		return
	}
	if _, err := s.cfg.ConcernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stageID,
		StageKind:            stageKind,
		ReviewerModel:        reviewerModel,
		OriginReviewSequence: originSequence,
		Concerns:             raised,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review concerns: persist failed — audit payload remains authoritative",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("stage_kind", stageKind),
			slog.String("error", err.Error()),
		)
	}
}

// suppressRelitigation reports whether the concern c is a no-evidence re-raise of
// an operator-arbitrated (waived/deferred) settled concern that must be
// SUPPRESSED — excluded from the InsertRaised batch and recorded as a
// concern_relitigation_suppressed audit entry instead (#1913). It returns true
// ONLY when every condition holds AND the audit entry lands durably; on ANY
// other outcome it returns false so the caller inserts the concern normally:
//
//   - empty SettledRef, or non-empty NewEvidence → not a suppression candidate;
//   - unparsable SettledRef → a sloppy tag never suppresses a finding;
//   - GetByIDs error / no row (unknown ref, store outage) → fail open (WARN);
//   - a ref to another run/stage/stageKind → fail open;
//   - a ref to a non-waived/deferred state (addressed/superseded/open) → fail
//     open: a genuine regression of an addressed fix must reach the operator,
//     tagged with settled_ref+new_evidence for lineage but RECORDED, not discarded;
//   - the audit append fails → fail open (WARN), so a suppression is never silent.
func (s *Server) suppressRelitigation(ctx context.Context, runID, stageID uuid.UUID, stageKind, reviewerModel string, originSequence int64, c planreview.Concern) bool {
	if c.SettledRef == "" || c.NewEvidence != "" {
		return false
	}
	refID, perr := uuid.Parse(c.SettledRef)
	if perr != nil {
		// A malformed tag can never suppress a genuine finding — fall open.
		return false
	}
	rows, gerr := s.cfg.ConcernRepo.GetByIDs(ctx, []uuid.UUID{refID})
	if gerr != nil || len(rows) == 0 || rows[0] == nil {
		if gerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review concerns: settled_ref lookup failed — inserting concern (fail-open)",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("settled_ref", c.SettledRef),
				slog.String("error", gerr.Error()),
			)
		}
		return false
	}
	row := rows[0]
	if row.RunID != runID || row.StageID != stageID || row.StageKind != stageKind {
		// A reviewer can never suppress via a ref to another run's/stage's concern.
		return false
	}
	if row.State != concern.StateWaived && row.State != concern.StateDeferred {
		// addressed/superseded/open: the insertable-regression path — record it.
		return false
	}
	// Suppression candidate: record the audit entry FIRST. Only when it lands
	// durably do we exclude the concern; an append failure (or a nil AuditRepo)
	// falls open to the normal insert so the suppression is never silent.
	if err := s.appendRelitigationSuppressed(ctx, runID, stageID, reviewerModel, originSequence, c, string(row.State)); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "review concerns: append concern_relitigation_suppressed failed — inserting concern (fail-open)",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("settled_ref", c.SettledRef),
			slog.String("error", err.Error()),
		)
		return false
	}
	return true
}

// appendRelitigationSuppressed writes the concern_relitigation_suppressed audit
// entry for a suppressed re-litigation (#1913). Returns an error (so the caller
// falls open) when the AuditRepo is unconfigured or the chained append fails.
func (s *Server) appendRelitigationSuppressed(ctx context.Context, runID, stageID uuid.UUID, reviewerModel string, originSequence int64, c planreview.Concern, settledState string) error {
	if s.cfg.AuditRepo == nil {
		return errors.New("audit repo not configured")
	}
	payload, _ := json.Marshal(concernRelitigationSuppressedPayload{
		SettledRef:           c.SettledRef,
		SettledState:         settledState,
		Severity:             string(c.Severity),
		Category:             c.Category,
		Note:                 c.Note,
		ReviewerModel:        reviewerModel,
		OriginReviewSequence: originSequence,
	})
	systemKind := audit.ActorKind("system")
	_, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  concernRelitigationSuppressedCategory,
		ActorKind: &systemKind,
		Payload:   payload,
	})
	return err
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

// subtractPaths returns a new slice of `paths` with every element present in
// `remove` dropped, preserving the order of the survivors. It is the single
// set-difference primitive both #1317 review-surface subtractions use
// (Trigger.ScopeDrift and the gate-evidence ScopeFacts). A nil/empty `remove`
// returns `paths` unchanged; a nil `paths` yields nil.
func subtractPaths(paths, remove []string) []string {
	if len(remove) == 0 || len(paths) == 0 {
		return paths
	}
	removeSet := make(map[string]struct{}, len(remove))
	for _, p := range remove {
		removeSet[p] = struct{}{}
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, ok := removeSet[p]; ok {
			continue
		}
		out = append(out, p)
	}
	return out
}

// operatorScopeUndelivered returns the order-preserving, deduped subset of
// operatorAdded paths the implement commit left UNTOUCHED — i.e. NOT present in
// the committed diff's file set (#1407). It is the deterministic detection
// behind the operator_scope_path_undelivered signal: an operator-deliberately-
// added scope path (add_scope_files fold or approved amendment) absent from the
// committed tree is a likely dropped operator-required edit. Detection is
// untouched-only; a path touched with the wrong content cannot be detected
// deterministically and stays a review concern. Trailing-slash directory-prefix
// entries and non-repo-relative tokens are skipped (mirroring MissingScopeFiles)
// — a directory or absolute/traversal token can never name a committed diff
// path, so it would only produce false positives.
func operatorScopeUndelivered(operatorAdded []string, diff policy.Diff) []string {
	if len(operatorAdded) == 0 {
		return nil
	}
	committed := make(map[string]struct{}, len(diff.ChangedFiles))
	for _, f := range diff.ChangedFiles {
		committed[f.Path] = struct{}{}
	}
	var out []string
	seen := make(map[string]struct{}, len(operatorAdded))
	for _, p := range operatorAdded {
		if strings.HasSuffix(p, "/") || !isRepoRelativePath(p) {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		if _, ok := committed[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// gateEvidenceForReview maps the bundle's gate_evidence wire struct into
// the prompt package's mirror (#963), keeping prompt free of a bundle
// import — the same boundary pattern renderDiffForReview applies to
// policy.Diff. Pure field copies; the runner already bounded and
// redacted every free-text field before packing.
// folded carries the runner's authoritative per-commit scope_amendments_folded
// set (#1317); it is subtracted from the ScopeFacts drift surfaces below
// (UndeclaredPaths + UndeclaredCategorized) for the same review-presentation
// reconciliation the trace handler applies to Trigger.ScopeDrift. nil/empty
// folded leaves ScopeFacts byte-identical to the pre-#1317 behavior.
func gateEvidenceForReview(ev bundle.GateEvidence, folded []string) *prompt.GateEvidence {
	out := &prompt.GateEvidence{
		FlakeRetries: ev.FlakeRetries,
	}
	for _, vr := range ev.VerifyRuns {
		out.VerifyRuns = append(out.VerifyRuns, prompt.GateVerifyRun{
			Command:       vr.Command,
			ExitCode:      vr.ExitCode,
			Outcome:       vr.Outcome,
			OutputTail:    vr.OutputTail,
			TailTruncated: vr.TailTruncated,
			Superseded:    vr.Superseded,
		})
	}
	if ev.VerifySummary != nil {
		out.VerifySummary = &prompt.GateVerifySummary{
			Outcome:       ev.VerifySummary.Outcome,
			Iterations:    ev.VerifySummary.Iterations,
			MaxIterations: ev.VerifySummary.MaxIterations,
			Detail:        ev.VerifySummary.Detail,
		}
	}
	if ev.ScopeFacts != nil {
		out.ScopeFacts = &prompt.GateScopeFacts{
			DeclaredFiles:   ev.ScopeFacts.DeclaredFiles,
			StagedFiles:     ev.ScopeFacts.StagedFiles,
			UndeclaredPaths: subtractPaths(ev.ScopeFacts.UndeclaredPaths, folded),
		}
		foldedSet := make(map[string]struct{}, len(folded))
		for _, p := range folded {
			foldedSet[p] = struct{}{}
		}
		for _, dp := range ev.ScopeFacts.UndeclaredCategorized {
			if _, ok := foldedSet[dp.Path]; ok {
				continue
			}
			out.ScopeFacts.UndeclaredCategorized = append(out.ScopeFacts.UndeclaredCategorized, prompt.GateDriftPath{
				Path:        dp.Path,
				Category:    dp.Category,
				Disposition: dp.Disposition,
			})
		}
	}
	for _, pv := range ev.PolicyViolations {
		out.PolicyViolations = append(out.PolicyViolations, prompt.GatePolicyViolation{
			Check:      pv.Check,
			Constraint: pv.Constraint,
			Detail:     pv.Detail,
			Files:      pv.Files,
		})
	}
	for _, ex := range ev.ScopeExemptions {
		out.ScopeExemptions = append(out.ScopeExemptions, prompt.GateScopeExemption{
			Path:   ex.Path,
			Reason: ex.Reason,
		})
	}
	if ev.FixupSelfReportDivergence != nil {
		out.FixupSelfReportDivergence = &prompt.GateFixupSelfReportDivergence{
			ClaimedVerifyStatus: ev.FixupSelfReportDivergence.ClaimedVerifyStatus,
			ActualVerifyStatus:  ev.FixupSelfReportDivergence.ActualVerifyStatus,
		}
	}
	return out
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

// implementReviewAlreadyStarted reports whether an implement_review_started
// audit entry for the given stage with the same head_sha already exists, so a
// retried raw trace upload can be deduped before re-dispatching the review
// (#797). It mirrors childPushAlreadyRecorded (#776): the entries are
// sequence-ascending per ListForRunByCategory's contract, and we skip any
// whose StageID is nil or mismatched. The keying is (stage_id, head_sha) — a
// FixupStage re-review reopens the SAME stage_id with a NEW head_sha and is
// not suppressed.
//
// CRUCIALLY it returns false immediately for an empty headSHA so an absent
// head_sha NEVER suppresses dispatch (fail-open): the no-verify path, the
// gate-skipped/infra-failure paths, and older entries without the field all
// degrade to the retained #793 raw-variant gate rather than blocking a review.
func implementReviewAlreadyStarted(entries []*audit.Entry, stageID uuid.UUID, headSHA string) bool {
	if headSHA == "" {
		return false
	}
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.HeadSHA == headSHA {
			return true
		}
	}
	return false
}

// newestImplementReviewStartedHead returns the head_sha of the NEWEST
// implement_review_started entry for the given stage, and whether ANY such entry
// exists. It powers the fixup re-review backstop's conservative empty-head skip
// (#1932): a found entry whose head_sha is "" means the newest prior review round
// was unkeyed, so the backstop cannot tell a missed re-review from an already-run
// one and fails closed. found==false means NO review ever started for the stage
// (the trace-time hook never fired at all), so the backstop proceeds. Newest is
// resolved by (Timestamp, Sequence) exactly like resolvePriorReviewedHeadSHA; an
// undecodable payload is skipped, and its empty-string head_sha is returned as-is
// (the found result still reflects the newest entry).
func newestImplementReviewStartedHead(entries []*audit.Entry, stageID uuid.UUID) (string, bool) {
	var newest *audit.Entry
	var newestSHA string
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if newest == nil || e.Timestamp.After(newest.Timestamp) ||
			(e.Timestamp.Equal(newest.Timestamp) && e.Sequence > newest.Sequence) {
			newest = e
			newestSHA = payload.HeadSHA
		}
	}
	if newest == nil {
		return "", false
	}
	return newestSHA, true
}

// ResolveDeploymentFromPollState records a delegating deploy stage's
// terminal outcome once the deploy reconciler has polled the external
// GitHub Actions run to completion (#1386 / E23.6, ADR-038). It is the
// deploy-side analogue of ResolveReviewFromPollState: the deployreconciler
// owns the GitHub polling + conclusion→outcome mapping; this method owns the
// server-internal persistence — artifact, audit, trace event, stage
// transition, and run advance — so all of it stays in the server package.
//
// Steps:
//  1. Re-read the stage; no-op when it is no longer parked at
//     awaiting_deployment (a webhook callback or an earlier tick already
//     resolved it — the resolve is idempotent).
//  2. Persist the deployment artifact (artifact.KindDeployment), deduped on
//     (stage_id, content_hash) exactly like handleShipDeployment so a repeat
//     tick before the transition lands does not double-write.
//  3. Append the deployment_outcome_recorded audit entry (same payload shape
//     the webhook callback writes, for consistent issue-comment rendering)
//     and the deploy_run trace event carrying the polled run's identity.
//  4. Transition awaiting_deployment → succeeded (outcome=succeeded) or →
//     failed (outcome=failed/partial; partial rides State=failed +
//     the artifact's outcome=partial per run.Stage.DeployOutcome's contract).
//  5. Advance the run so a terminal deploy stage completes the run.
//
// Best-effort audit: an audit-append failure WARN-logs and does NOT unwind
// the already-persisted artifact. A transition failure IS returned so the
// reconciler retries next tick.
func (s *Server) ResolveDeploymentFromPollState(ctx context.Context, runID, stageID uuid.UUID, outcome run.DeployOutcome, gitRef string, wr *githubclient.WorkflowRun) error {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		return errors.New("server: ResolveDeploymentFromPollState requires RunRepo, ArtifactRepo, and AuditRepo")
	}
	if !outcome.Valid() {
		return fmt.Errorf("server: ResolveDeploymentFromPollState invalid outcome %q", outcome)
	}

	stage, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil {
		return fmt.Errorf("resolve deployment from poll: get stage %s: %w", stageID, err)
	}
	if stage.State != run.StageStateAwaitingDeployment {
		// Already resolved by a prior tick or the webhook callback. Idempotent
		// no-op — the deploy executor never re-records a settled stage.
		return nil
	}

	externalURL := ""
	if wr != nil {
		externalURL = wr.HTMLURL
	}
	environment := s.deployEnvironmentForRun(ctx, runID)

	// Persist the deployment artifact (the durable carrier of the outcome —
	// partial/rolled_back are representable here even though the stage STATE
	// is only succeeded/failed). Mirrors handleShipDeployment's body shape.
	depBody := deploymentBody{
		Environment:    environment,
		Ref:            gitRef,
		ExternalRunURL: externalURL,
		Outcome:        string(outcome),
	}
	content, _ := json.Marshal(depBody)
	contentHash := sha256Hex(content)

	var artifactID uuid.UUID
	if existing, gerr := s.cfg.ArtifactRepo.GetByHash(ctx, stageID, contentHash); gerr == nil {
		artifactID = existing.ID
	} else if !errors.Is(gerr, artifact.ErrNotFound) {
		return fmt.Errorf("resolve deployment from poll: check existing artifact: %w", gerr)
	} else {
		created, cerr := s.cfg.ArtifactRepo.Create(ctx, artifact.CreateParams{
			StageID:     stageID,
			Kind:        artifact.KindDeployment,
			Content:     json.RawMessage(content),
			ContentHash: contentHash,
		})
		if cerr != nil {
			return fmt.Errorf("resolve deployment from poll: create artifact: %w", cerr)
		}
		artifactID = created.ID
	}

	systemKind := audit.ActorSystem
	outcomePayload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"artifact_id":      artifactID.String(),
		"content_hash":     contentHash,
		"environment":      environment,
		"ref":              gitRef,
		"external_run_url": externalURL,
		"outcome":          string(outcome),
		"auth_method":      "reconciler",
	})
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryDeploymentOutcomeRecorded,
		ActorKind: &systemKind,
		Payload:   outcomePayload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"resolve deployment from poll: append outcome audit failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()))
	}

	// deploy_run trace event: the governance record of the external run the
	// reconciler polled to terminal. Best-effort, like the outcome audit.
	var ghaRunID int64
	var conclusion, ghStatus string
	if wr != nil {
		ghaRunID, conclusion, ghStatus = wr.ID, wr.Conclusion, wr.Status
	}
	deployRunPayload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"gha_run_id":       ghaRunID,
		"external_run_url": externalURL,
		"conclusion":       conclusion,
		"status":           ghStatus,
		"outcome":          string(outcome),
	})
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryDeployRun,
		ActorKind: &systemKind,
		Payload:   deployRunPayload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"resolve deployment from poll: append deploy_run trace event failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()))
	}

	if err := s.advanceDeployStageTerminal(ctx, stageID, outcome, conclusion); err != nil {
		return err
	}

	s.notifyStatusUpdate(ctx, runID, "deployment_recorded")
	s.advanceRunAfterReviewResolve(ctx, runID)
	return nil
}

// ResolveDeploymentRollbackFromPollState records a delegating deploy stage's
// ROLLBACK as terminal once the deploy reconciler has polled the rollback's
// external GitHub Actions run to completion (#1398 / E23.6, #1386 binding
// condition 2). It is the rollback-side analogue of ResolveDeploymentFromPollState:
// the deployreconciler owns the GitHub polling; this method owns the
// server-internal persistence — a rolled_back deployment artifact, the
// deployment_outcome_recorded + deploy_run trace + deployment_rollback_completed
// audit entries.
//
// Unlike the forward resolve, the deploy stage is ALREADY terminal
// (succeeded/failed) when a rollback is initiated, so this method does NOT
// transition the stage or advance the run. "Set DeployOutcome=rolled_back" is
// realized through the rolled_back deployment artifact + audit (DeployOutcome is
// in-memory only — no column per migration 0038), mirroring the webhook callback
// (deployment.go::handleShipDeployment with rollback_action=completed).
//
// Idempotency: a no-op when a deployment_rollback_completed entry already exists
// for the stage (a prior tick or the webhook callback already finalized the
// rollback). Best-effort audit: an audit-append failure WARN-logs and does NOT
// unwind the already-persisted artifact.
func (s *Server) ResolveDeploymentRollbackFromPollState(ctx context.Context, runID, stageID uuid.UUID, gitRef string, wr *githubclient.WorkflowRun) error {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		return errors.New("server: ResolveDeploymentRollbackFromPollState requires RunRepo, ArtifactRepo, and AuditRepo")
	}

	// Idempotency guard: if a rollback_completed entry already exists for this
	// stage, the rollback was already finalized (an earlier tick or the webhook
	// callback). Re-recording would double-write the artifact + audit.
	if entries, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryDeploymentRollbackCompleted); lerr == nil {
		for _, e := range entries {
			if e.StageID != nil && *e.StageID == stageID {
				return nil
			}
		}
	} else {
		// A read error is fail-open (proceed to record): a missed idempotency
		// check at worst double-writes a content-identical artifact, which the
		// hash dedup below collapses anyway.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"resolve deployment rollback: read rollback_completed history failed; proceeding",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", lerr.Error()))
	}

	externalURL := ""
	if wr != nil {
		externalURL = wr.HTMLURL
	}
	environment := s.deployEnvironmentForRun(ctx, runID)

	// Persist the rolled_back deployment artifact — the durable carrier of the
	// rolled_back disposition. Deduped on (stage_id, content_hash) so a repeat
	// tick before the audit lands does not double-write.
	depBody := deploymentBody{
		Environment:    environment,
		Ref:            gitRef,
		ExternalRunURL: externalURL,
		Outcome:        string(run.DeployOutcomeRolledBack),
	}
	content, _ := json.Marshal(depBody)
	contentHash := sha256Hex(content)

	var artifactID uuid.UUID
	if existing, gerr := s.cfg.ArtifactRepo.GetByHash(ctx, stageID, contentHash); gerr == nil {
		artifactID = existing.ID
	} else if !errors.Is(gerr, artifact.ErrNotFound) {
		return fmt.Errorf("resolve deployment rollback: check existing artifact: %w", gerr)
	} else {
		created, cerr := s.cfg.ArtifactRepo.Create(ctx, artifact.CreateParams{
			StageID:     stageID,
			Kind:        artifact.KindDeployment,
			Content:     json.RawMessage(content),
			ContentHash: contentHash,
		})
		if cerr != nil {
			return fmt.Errorf("resolve deployment rollback: create artifact: %w", cerr)
		}
		artifactID = created.ID
	}

	systemKind := audit.ActorSystem
	outcomePayload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"artifact_id":      artifactID.String(),
		"content_hash":     contentHash,
		"environment":      environment,
		"ref":              gitRef,
		"external_run_url": externalURL,
		"outcome":          string(run.DeployOutcomeRolledBack),
		"auth_method":      "reconciler",
	})
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryDeploymentOutcomeRecorded,
		ActorKind: &systemKind,
		Payload:   outcomePayload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"resolve deployment rollback: append outcome audit failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()))
	}

	// deploy_run trace event: the governance record of the rollback run the
	// reconciler polled to terminal. The actual conclusion is preserved here
	// even though the recorded outcome is always rolled_back.
	var ghaRunID int64
	var conclusion, ghStatus string
	if wr != nil {
		ghaRunID, conclusion, ghStatus = wr.ID, wr.Conclusion, wr.Status
	}
	deployRunPayload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"gha_run_id":       ghaRunID,
		"external_run_url": externalURL,
		"conclusion":       conclusion,
		"status":           ghStatus,
		"outcome":          string(run.DeployOutcomeRolledBack),
		"rollback":         true,
	})
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryDeployRun,
		ActorKind: &systemKind,
		Payload:   deployRunPayload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"resolve deployment rollback: append deploy_run trace event failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()))
	}

	// deployment_rollback_completed: the terminal rollback marker, chaining the
	// deployment_rollback_initiated handle. This is what removes the stage from
	// the reconciler's rollback-pending scan on the next tick.
	rollbackPayload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"artifact_id":      artifactID.String(),
		"environment":      environment,
		"external_run_url": externalURL,
		"rollback_action":  "completed",
		"auth_method":      "reconciler",
	})
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryDeploymentRollbackCompleted,
		ActorKind: &systemKind,
		Payload:   rollbackPayload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"resolve deployment rollback: append rollback_completed audit failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()))
	}

	// Refresh the sticky living-anchor comment so the rolled_back disposition
	// surfaces on the issue timeline. The stage is already terminal — no stage
	// transition, no run advance.
	s.notifyStatusUpdate(ctx, runID, "deployment_recorded")
	return nil
}

// advanceDeployStageTerminal transitions a parked deploy stage to its
// terminal state from the mapped outcome (#1386 / E23.6): succeeded →
// StageStateSucceeded; failed/partial → StageStateFailed (category C — the
// external pipeline, not a Fishhawk agent, produced the failure). A
// partial deploy is a failed STATE whose partial disposition rides the
// artifact's outcome field (run.Stage.DeployOutcome's contract). Shared by
// the reconciler resolve and the webhook callback so both surfaces map an
// outcome to a state identically.
func (s *Server) advanceDeployStageTerminal(ctx context.Context, stageID uuid.UUID, outcome run.DeployOutcome, conclusion string) error {
	switch outcome {
	case run.DeployOutcomeSucceeded:
		if _, err := s.cfg.RunRepo.TransitionStage(ctx, stageID, run.StageStateSucceeded, nil); err != nil {
			return fmt.Errorf("resolve deployment: awaiting_deployment → succeeded: %w", err)
		}
		return nil
	default:
		reason := fmt.Sprintf("external deploy pipeline reported %s (conclusion=%q)", outcome, conclusion)
		if _, err := run.FailStage(ctx, s.cfg.RunRepo, stageID, run.FailureC, reason); err != nil {
			return fmt.Errorf("resolve deployment: awaiting_deployment → failed: %w", err)
		}
		return nil
	}
}

// deployEnvironmentForRun derives the deployed environment label from the
// run's cached workflow spec — the deploy stage's first
// allowed_environments entry (the pre-execution gate already constrained
// the deploy to that set). Best-effort: an unparseable/absent spec yields
// "" rather than failing the resolve, since the authoritative outcome
// fields are the external_run_url + outcome, not the environment label.
func (s *Server) deployEnvironmentForRun(ctx context.Context, runID uuid.UUID) string {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil || len(runRow.WorkflowSpec) == 0 {
		return ""
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return ""
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return ""
	}
	for _, st := range wf.Stages {
		if st.Type != spec.StageTypeDeploy {
			continue
		}
		for _, c := range st.Constraints {
			if len(c.AllowedEnvironments) > 0 {
				return c.AllowedEnvironments[0]
			}
		}
	}
	return ""
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
